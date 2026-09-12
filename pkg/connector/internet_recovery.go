// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package connector

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/lrhodin/corten-matrix/pkg/internetprobe"
	"github.com/lrhodin/corten-matrix/pkg/rustpushgo"
)

const internetReconnectRetryMargin = 60 * time.Second

// Timing knobs. These are vars, not consts, solely so tests can scale them down
// and drive runPublicOnlyInternetRecovery itself, not only the pure transition
// function it applies: the table test proves the policy, the loop tests prove
// the wiring (that every decision's effect is performed, and that a send
// failure or cancellation exits). Never mutated in production.
var (
	internetRecoveryPollInterval = 10 * time.Second
	internetRecoveryStablePeriod = 60 * time.Second

	// When the probe itself cannot run on this host — no socket permission, no
	// descriptors — there is no connectivity signal at all. Public-only mode must
	// not hold the bridge hostage on no evidence, so after this grace period hand
	// control back to bridgev2 and let a real Apple attempt decide.
	internetRecoveryBlockedGrace = 2 * time.Minute

	// There is deliberately NO ceiling on a confirmed outage. Earlier rounds
	// handed control back to bridgev2 after 30 minutes of Unreachable verdicts
	// (and after 60 minutes of any episode) on the argument that a permanently
	// wrong probe must not hold the bridge down forever. That defense is
	// withdrawn: the requirement is to touch Apple only after public
	// connectivity has returned and stayed stable, and a hand-back on a
	// confirmed-down verdict contacts Apple while the probe still says down.
	// Two things changed the calculus. The probe is no longer one ping: a
	// provider can vote Reachable only through certificate-validated TLS, and a
	// down verdict requires at least one real network failure with neither
	// provider authenticated as reachable. More importantly, the requirement
	// forbids touching Apple, not being down. So when the probe
	// says down for a long time the correct behavior is a LOUD dead bridge,
	// not an automatic Apple reconnect: after this much continuous confirmed
	// outage the loop raises a throttled Error and posts a management-room
	// notice, and keeps holding Apple-free. The episode still ends cleanly on
	// cancellation or shutdown; it never ends by contacting Apple. The one
	// hand-back that remains is the unusable-probe hatch above, which fires on
	// NO verdict rather than on a down one.
	internetRecoveryHoldAlarmAfter    = 30 * time.Minute
	internetRecoveryHoldAlarmInterval = 30 * time.Minute

	// Rounds logged at Info before steady-state repetition drops to Debug. At the
	// 10-second cadence this covers the first minute; a change of verdict is
	// always Info regardless. Without this an overnight outage writes tens of
	// thousands of identical Info lines into bridge.log.
)

const (
	internetProbeVerboseRounds = 6

	// A reconnect storm publishes NO Failed state at all, so neither the Rust
	// escalation counter nor the RetryFailed path can see the highest-rate Apple
	// contact there is. rustpush builds its backoff INSIDE the failure loop
	// (util.rs), so when generate() succeeds and the connection then dies at once,
	// the manager re-selects with no sleep: Generated -> Generating -> Generated,
	// forever, no Failed. Both of those states reset retrying_failures. That shape
	// is the duplicate-device-token "early eof" storm which has been observed
	// escalating to a temporary Apple account disable, and the Interrupted handler
	// would otherwise just allow every reconnect with no rate limit at all.
	internetFlapStormThreshold = 5
	internetFlapStormWindow    = 60 * time.Second

	// A burst window alone leaves a gap: 4 interruptions per minute never reaches
	// 5-in-60s, yet sustains 240/hour — twice the ~120/hour this file's own
	// accounting treats as abusive. The long window closes that band without
	// firing on a brief burst. Both are evaluated; either one trips.
	internetFlapSustainedThreshold = 12
	internetFlapSustainedWindow    = 10 * time.Minute

	// A single failed probe is not an outage. rustpush reconnects a dropped
	// transport in about a second, so tearing the whole client epoch down on one
	// 3-second sample turns a 10-second router reboot into 6-8 minutes of
	// downtime (teardown + 60s stability + bridgev2's own 4-6 min rebuild wait).
	// Confirm first: the cost of waiting is a handful of courier attempts, which
	// is far below the cost of being wrong.
	internetOutageConfirmRounds = 3
)

// A var only so the event loop's confirmation window can be driven in a test;
// never reassigned in production.
var internetOutageConfirmDelay = 10 * time.Second

// Hand-back backoff bounds Apple contact ACROSS episodes. A hand-back asks
// bridgev2 to rebuild, and that rebuild closes recoveryDone, killing the loop
// that owned this episode's re-ask gate — so without connector-level state every
// hand-back starts a fresh episode with a fresh clock and the cadence never
// widens. With Apple genuinely unreachable that is a rebuild every ceiling
// period, forever. The counter lives on IMConnector, which outlives client
// rebuilds, and is cleared only when the receive-wedge watchdog sees an inbound
// APS frame on the rebuilt client (see clearHandBacks) or on logout.
const (
	recoveryHandBackBaseDelay = 7 * time.Minute
	recoveryHandBackMaxDelay  = 45 * time.Minute
)

// handBackDelay returns the minimum spacing before the nth hand-back of a
// consecutive run: 7m, 14m, 28m, then capped. Doubling rather than a fixed gate
// means a genuinely broken probe costs progressively less Apple traffic while a
// one-off still recovers promptly.
func handBackDelay(consecutive int) time.Duration {
	if consecutive <= 1 {
		return recoveryHandBackBaseDelay
	}
	delay := recoveryHandBackBaseDelay << (consecutive - 1)
	if delay > recoveryHandBackMaxDelay || delay <= 0 {
		return recoveryHandBackMaxDelay
	}
	return delay
}

// Seams for tests. internetProbeFunc scripts probe verdicts; sendRecoveryState
// removes the need for a real *bridgev2.BridgeStateQueue. Never reassigned in
// production.
var (
	internetProbeFunc = func(ctx context.Context, _ string) internetprobe.Result {
		return internetprobe.Probe(ctx)
	}
	retryDelayFunc    = internetRecoveryStateRetryDelay
	sendRecoveryState = sendInternetRecoveryState
)

// internetProbeLogger keeps recovery logging informative without flooding
// bridge.log during a multi-hour outage. The opening rounds and every change of
// verdict are Info; unchanged steady-state rounds drop to Debug.
type internetProbeLogger struct {
	round    int
	last     internetprobe.Result
	haveLast bool
}

// beginRound advances the round counter and reports whether this is one of the
// opening rounds, which are always logged at Info.
func (p *internetProbeLogger) beginRound() (round int, opening bool) {
	p.round++
	return p.round, p.round <= internetProbeVerboseRounds
}

// recordResult stores this round's verdict and reports whether it warrants Info.
// Opening rounds always do, and so does any change of verdict — so entering and
// leaving an outage is never buried at Debug no matter how long the outage ran.
func (p *internetProbeLogger) recordResult(result internetprobe.Result, opening bool) bool {
	changed := !p.haveLast || p.last.Cloudflare != result.Cloudflare || p.last.Google != result.Google
	p.last, p.haveLast = result, true
	return opening || changed
}

// run performs one probe round and logs it before and after execution.
func (p *internetProbeLogger) run(ctx context.Context, log zerolog.Logger, phase string) internetprobe.Result {
	round, opening := p.beginRound()

	pre := log.Debug()
	if opening {
		pre = log.Info()
	}
	pre.
		Str("platform", runtime.GOOS).
		Str("probe_phase", phase).
		Int("probe_round", round).
		Strs("public_probe_targets", []string{internetprobe.CloudflareTarget, internetprobe.GoogleTarget}).
		Msg("Testing public Internet connectivity without contacting Apple")

	result := internetProbeFunc(ctx, phase)
	logInternetProbeResult(log, phase, result, round, p.recordResult(result, opening))
	return result
}

// runAPSConnectionEventLoop performs no periodic network activity. rustpushgo
// emits an event only when the previously usable APS resource leaves Generated
// or when a regeneration fails.
func (c *IMClient) runAPSConnectionEventLoop(stop <-chan struct{}, log zerolog.Logger) {
	ctx := c.bridgeRecoveryContext()
	var probeLog internetProbeLogger
	// Timestamps of recent interruptions, for flap-storm detection.
	var interruptions []time.Time
	for {
		select {
		case <-stop:
			return
		// Read without connectionEventMu. Connect writes the channel and then
		// launches this goroutine, which is the happens-before edge, and
		// nothing rewrites it for the life of the epoch (a client is never
		// Connect-ed twice). The mutex orders the other readers —
		// OnConnectionEvent and dropPendingConnectionEvent — not this one.
		case <-c.connectionEventWake:
			event, ok := c.takeConnectionEvent()
			if !ok {
				continue
			}
			// The FFI callback already ended and banked the healthy stretch at
			// event time, before this controller could coalesce or discard it.
			log.Info().
				Str("platform", runtime.GOOS).
				Strs("public_probe_targets", []string{internetprobe.CloudflareTarget, internetprobe.GoogleTarget}).
				Uint("aps_event", uint(event)).
				Bool("statuskit_deferred", c.statusKitDeferred.Load()).
				Msg("APS connection event received; checking public Internet before deciding how to reconnect")
			switch event {
			case rustpushgo.ApsConnectionEventInterrupted:
				result := probeLog.run(ctx, log, "aps_interruption_classification")
				if channelClosed(stop) {
					return
				}
				// A locally blocked public probe has no standing to call the
				// Internet down, but it also cannot grant an unbounded exception
				// to the flap-storm limit. Count every interruption for which the
				// probe did not establish an outage; otherwise EACCES on the public
				// sockets leaves Generated -> Generating -> Generated storms
				// reconnecting to Apple without backoff forever.
				if result.Reachable() || result.Blocked() {
					interruptions = recordInterruption(interruptions, time.Now())
					if burst, sustained, window := classifyFlapStorm(interruptions, time.Now()); burst || sustained {
						log.Warn().
							Int("interruptions", len(interruptions)).
							Bool("burst", burst).
							Bool("sustained", sustained).
							Bool("probe_blocked", result.Blocked()).
							Dur("window", window).
							Msg("APS transport is flapping faster than a healthy link can explain; entering conservative recovery to stop unbounded Apple retries")
						c.runPublicOnlyInternetRecovery(log, classifyRecoveryVerdict(result))
						return
					}
				}
				if result.Reachable() {
					log.Info().
						Int("interruptions_in_window", len(interruptions)).
						Msg("APS transport interrupted while public Internet remains reachable; allowing immediate transport reconnect")
					continue
				}
				if result.Blocked() {
					// No signal, so no grounds for the drastic branch. Falling
					// through to the ordinary upstream reconnect is strictly
					// better than tearing the client down on no evidence.
					logBlockedProbe(log, result).
						Msg("Public Internet probe could not run on this host; allowing the ordinary transport reconnect instead of entering Apple-free recovery")
					continue
				}
				log.Warn().
					Int("confirm_rounds", internetOutageConfirmRounds).
					Dur("confirm_delay", internetOutageConfirmDelay).
					Msg("APS transport interrupted and the public Internet probe failed; confirming before tearing the client down")
				if !c.confirmPublicOutage(ctx, stop, log, &probeLog) {
					// The APS observer escalates a sustained retrying failure to
					// RetryFailed after ~15s, which lands INSIDE this ~30-40s
					// window. Left latched, it would drive an unconditional
					// teardown on the next loop pass and undo the confirmation
					// entirely — 6-8 minutes of downtime for a blip we just
					// watched recover.
					c.dropPendingConnectionEvent()
					continue
				}
				log.Warn().Msg("Public Internet outage confirmed across the confirmation window; stopping Apple retries")
				c.runPublicOnlyInternetRecovery(log, verdictUnreachable)
				return

			case rustpushgo.ApsConnectionEventRetryFailed:
				// Requirement: a failed regeneration always enters conservative
				// recovery, so this branch does not consult the probe verdict to
				// decide *whether* to recover — only to describe why.
				result := probeLog.run(ctx, log, "aps_retry_failure_classification")
				if channelClosed(stop) {
					return
				}
				switch {
				case result.Reachable():
					log.Warn().Msg("APS reconnect failed while public Internet is reachable; stopping retries before conservative recovery")
				case result.Blocked():
					logBlockedProbe(log, result).
						Msg("APS reconnect failed and the public Internet probe could not run on this host; entering conservative recovery with no connectivity signal")
				default:
					log.Warn().Msg("APS reconnect failed and both public Internet probes failed; stopping Apple retries")
				}
				c.runPublicOnlyInternetRecovery(log, classifyRecoveryVerdict(result))
				return
			}
		}
	}
}

// confirmPublicOutage re-probes before authorizing a teardown. It returns true
// only if every confirmation round also failed; a single recovered round, or a
// round the host would not let us run, aborts the escalation and lets the
// ordinary upstream reconnect proceed.
func (c *IMClient) confirmPublicOutage(ctx context.Context, stop <-chan struct{}, log zerolog.Logger, probeLog *internetProbeLogger) bool {
	for round := 1; round <= internetOutageConfirmRounds; round++ {
		if !waitForInternetRecoveryContext(ctx, stop, internetOutageConfirmDelay) {
			return false
		}
		result := probeLog.run(ctx, log, "outage_confirmation")
		if channelClosed(stop) {
			return false
		}
		if result.Reachable() {
			log.Info().Int("confirm_round", round).
				Msg("Public Internet recovered during confirmation; no teardown needed")
			return false
		}
		if result.Blocked() {
			logBlockedProbe(log, result).Int("confirm_round", round).
				Msg("Public Internet probe could not run during confirmation; declining to tear the client down on no evidence")
			return false
		}
	}
	return true
}

// classifyFlapStorm reports whether the interruption history constitutes a
// reconnect storm, by either the short burst window or the longer sustained one.
func classifyFlapStorm(history []time.Time, now time.Time) (burst, sustained bool, window time.Duration) {
	inBurst := 0
	burstCutoff := now.Add(-internetFlapStormWindow)
	for _, at := range history {
		if at.After(burstCutoff) {
			inBurst++
		}
	}
	if inBurst >= internetFlapStormThreshold {
		return true, false, internetFlapStormWindow
	}
	// Windowed here, not by relying on the caller having pruned the history:
	// recordInterruption does prune to this window, but a classifier that counts
	// bare len(history) is only correct by that coincidence.
	inSustained := 0
	sustainedCutoff := now.Add(-internetFlapSustainedWindow)
	for _, at := range history {
		if at.After(sustainedCutoff) {
			inSustained++
		}
	}
	if inSustained >= internetFlapSustainedThreshold {
		return false, true, internetFlapSustainedWindow
	}
	return false, false, internetFlapSustainedWindow
}

// recordInterruption appends now and drops anything older than the longest flap
// window, so the history covers both the burst and sustained checks.
func recordInterruption(history []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-internetFlapSustainedWindow)
	kept := history[:0]
	for _, at := range history {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	return append(kept, now)
}

// runPublicOnlyInternetRecovery deliberately outlives this IMClient's stopChan.
// It tears the client down completely, stopping APS and every Apple-facing
// recurring worker. The bridge context then owns the public-only recovery loop.
//
// The loop body is deliberately thin: every decision is made by
// recoveryEpisode.step / finishPreflight, which are pure functions of the
// episode state and the round's inputs. This loop only performs the effects
// they name (probe, send, log) and exits when a send fails or the episode is
// canceled. Seven audit rounds found a new critical defect in the previous
// arm-structured body on six of seven fixes, every one an interaction between
// mutable variables that were updated in one arm and read in another. The
// transition function makes those interactions enumerable: see
// TestRecoveryStepDecisionTable.
// entry is the last thing the caller actually knew about the network when it
// decided to enter recovery: verdictUnreachable for a confirmed outage,
// verdictReachable for an Apple-specific failure on a healthy link, and
// verdictBlocked when the probe could not run. The episode is seeded with it,
// so a hand-back cannot be sent on the strength of "no verdict yet" when the
// last verdict — the one that started the episode — was a confirmed outage.
func (c *IMClient) runPublicOnlyInternetRecovery(log zerolog.Logger, entry recoveryVerdict) {
	appleSpecificFailure := entry == verdictReachable
	// courierFailure: the courier failed on a link the probe did NOT call down
	// — a flap storm or a sustained regeneration failure, entered on a
	// Reachable or Blocked verdict. These are the episodes whose recovered
	// rebuilds the flap run counts and holds (flap_recovery.go). A confirmed
	// outage (entry Unreachable, from the confirmation window or the wedge
	// watchdog) is NOT one: its rebuild after the stability window gets normal
	// timing and neither reads nor writes the run. This is the only place the
	// distinction is drawn, so the two cannot drift.
	courierFailure := entry != verdictUnreachable
	main := c.Main
	if main == nil || main.Bridge == nil {
		return
	}
	// Read without disconnectMu. Every caller is a goroutine Connect launched
	// after writing these (the APS event loop and the receive-wedge watchdog),
	// so the reads are ordered by goroutine creation, and nothing rewrites them
	// afterward: a client is never Connect-ed twice. disconnectMu is what
	// orders the lifecycle predicates against teardown, and is not needed here.
	bridgeState := c.recoveryBridgeState
	recoveryDone := c.recoveryDone
	// TryLock, not Lock: an episode already owns this login's recovery, and a
	// second entrant (the APS event loop and the receive-wedge watchdog can both
	// reach here) must not park a goroutine on a mutex held for the length of an
	// outage.
	if !c.internetRecoveryMu.TryLock() {
		log.Info().Msg("Internet recovery is already running for this login; not starting a second episode")
		return
	}
	defer c.internetRecoveryMu.Unlock()

	ctx := c.bridgeRecoveryContext()
	if main.Bridge.IsStopping() || bridgeState == nil || recoveryDone == nil || internetRecoveryCanceled(ctx, recoveryDone) {
		return
	}

	// Do not consume bridgev2's UserLogin.disconnectOnce here. bridgev2 needs
	// that guard when StateUnknownError performs the eventual safe recreation.
	c.disconnectForInternetRecovery()
	if main.Bridge.IsStopping() || internetRecoveryCanceled(ctx, recoveryDone) {
		return
	}
	message := "Internet connection lost; waiting to reconnect to iMessage"
	if appleSpecificFailure {
		message = "Apple connection failed; waiting before reconnecting to iMessage"
	}
	if !sendRecoveryState(main.Bridge, bridgeState, status.BridgeState{
		StateEvent: status.StateTransientDisconnect,
		Error:      "im-internet-offline",
		Message:    message,
	}) {
		return
	}
	log.Warn().
		Str("platform", runtime.GOOS).
		Strs("public_probe_targets", []string{internetprobe.CloudflareTarget, internetprobe.GoogleTarget}).
		Bool("apple_specific_failure", appleSpecificFailure).
		Bool("courier_failure", courierFailure).
		Int("flap_rebuilds_so_far", main.flapRebuildCount(c.UserLogin.ID)).
		Dur("required_stability", internetRecoveryStablePeriod).
		Dur("unusable_probe_grace", internetRecoveryBlockedGrace).
		Dur("hold_alarm_after", internetRecoveryHoldAlarmAfter).
		Msg("APNs recovery entered public-only mode: iMessage client teardown completed; the recovery watcher will use only public probes until Internet stability is established")

	var probeLog internetProbeLogger
	episode := newRecoveryEpisode(time.Now(), entry)
	canceled := func() bool {
		return main.Bridge.IsStopping() || internetRecoveryCanceled(ctx, recoveryDone)
	}
	for {
		if canceled() {
			return
		}
		result := probeLog.run(ctx, log, "public_only_recovery")
		if canceled() {
			return
		}
		now := time.Now()
		round := recoveryRound{
			now:        now,
			verdict:    classifyRecoveryVerdict(result),
			retryDelay: retryDelayFunc(main.Bridge.Config.UnknownErrorAutoReconnect),
			timing:     currentRecoveryTiming(),
		}
		// Spacing that survives the rebuild which kills this loop.
		if wait, ok := main.handBackDue(c.UserLogin.ID, now); !ok {
			round.handBackHold = wait
		}
		// The flap run's spacing, read ONLY for a courier-failure episode: a
		// confirmed-outage episode leaves flapHold zero and so reaches the
		// preflight on the stability window alone.
		if courierFailure {
			if wait, ok := main.flapRebuildDue(c.UserLogin.ID, now); !ok {
				round.flapHold = wait
			}
		}

		decision := episode.step(round)
		c.logRecoveryDecision(log, recoveryPhaseMain, result, round, decision, episode)
		if decision.holdAlarm {
			sendRecoveryHoldNotice(c, ctx, log, recoveryHoldNoticeText(now.Sub(episode.outageStartedAt)))
		}
		switch decision.action {
		case actionWithdraw:
			if !c.sendRecoveryWithdrawal(main, bridgeState) {
				return
			}
		case actionHandBack:
			if !sendRecoveryState(main.Bridge, bridgeState, status.BridgeState{
				StateEvent: status.StateUnknownError,
				Error:      decision.handBackCode,
				Message:    "Cannot confirm Internet connectivity; retrying the iMessage connection",
				Info:       map[string]interface{}{"recovery_request": episode.attempt},
			}) {
				return
			}
			// The queue does not report whether it accepted the state, so retain
			// the hand-back run's existing conservative request pacing. A dropped
			// state cannot contact Apple and therefore does not itself advance the
			// separate flap-rebuild schedule.
			main.noteHandBack(c.UserLogin.ID, now)
			// Remember the request so LoadUserLogin can count it if and only if
			// bridgev2 actually constructs a replacement. This is the path used
			// when public probes are locally blocked; the first actual replacement
			// remains normal, while later actual replacements widen and defer
			// optional StatusKit startup.
			if courierFailure {
				main.markFlapRebuildRequested(c.UserLogin.ID)
				log.Info().
					Int("completed_flap_rebuilds", main.flapRebuildCount(c.UserLogin.ID)).
					Msg("Requested a replacement after a courier failure on an unjudgeable link; the flap run advances only if LoadUserLogin constructs that replacement")
			}
		case actionPreflight:
			// Final public preflight immediately before authorizing a single
			// bridgev2-owned reconstruction.
			preflight := probeLog.run(ctx, log, recoveryPhasePreflight)
			if canceled() {
				return
			}
			// The preflight is a second reading of the same three-valued
			// verdict, so it is classified by the same function and logged
			// through the same path as the main round.
			preflightRound := round
			preflightRound.verdict = classifyRecoveryVerdict(preflight)
			after := episode.finishPreflight(now, preflightRound.verdict)
			c.logRecoveryDecision(log, recoveryPhasePreflight, preflight, preflightRound, after, episode)
			switch after.action {
			case actionWithdraw:
				if !c.sendRecoveryWithdrawal(main, bridgeState) {
					return
				}
			case actionRecovered:
				// bridgev2 owns the rebuild and applies its own jittered
				// UnknownErrorAutoReconnect delay first, so the client does NOT
				// come back the moment this is logged. Log the delay so an
				// operator watching `corten-matrix logs` during an outage does
				// not read the wait as a hang.
				log.Info().Dur("stable_for", round.timing.stablePeriod).
					Int("request_attempt", episode.attempt).
					Dur("bridgev2_reconnect_delay", main.Bridge.Config.UnknownErrorAutoReconnect).
					Msg("Public Internet remained stable and final preflight passed; authorizing one bridgev2 iMessage client rebuild (bridgev2 applies its own jittered reconnect delay before rebuilding)")
				if !sendRecoveryState(main.Bridge, bridgeState, status.BridgeState{
					StateEvent: status.StateUnknownError,
					Error:      "im-internet-recovered",
					Message:    "Internet recovered; reconnecting to iMessage",
					Info:       map[string]interface{}{"recovery_request": episode.attempt},
				}) {
					return
				}
				// Only courier-failure episodes mark a pending flap replacement.
				// The widening schedule advances later, at actual construction; a
				// confirmed-outage entry never enters this run.
				if courierFailure {
					main.markFlapRebuildRequested(c.UserLogin.ID)
					log.Info().
						Int("completed_flap_rebuilds", main.flapRebuildCount(c.UserLogin.ID)).
						Dur("healthy_lease", flapRecoveryHealthyLease).
						Msg("Requested a replacement after an APNs courier failure; the flap run advances only if LoadUserLogin constructs that replacement")
				}
			}
		}

		if !waitForInternetRecoveryContext(ctx, recoveryDone, internetRecoveryPollInterval) {
			return
		}
	}
}

// sendRecoveryWithdrawal retracts a pending "recovered" rebuild request. Note
// this changes prev.Timestamp, which makes bridgev2's pending
// unknownErrorReconnect decline at Debug level — the cost is one more of its
// wait cycles once the link settles, and the benefit is not rebuilding into an
// outage. The connector's uncounted flap lineage deliberately survives: the
// waiter may already be past its final state check, and only actual construction
// consumes and counts that lineage.
func (c *IMClient) sendRecoveryWithdrawal(main *IMConnector, bridgeState *bridgev2.BridgeStateQueue) bool {
	return sendRecoveryState(main.Bridge, bridgeState, status.BridgeState{
		StateEvent: status.StateTransientDisconnect,
		Error:      "im-internet-offline",
		Message:    "Internet became unstable; reconnect delay reset",
	})
}

// Probe phases of a recovery episode, as logged with each probe result.
const (
	recoveryPhaseMain      = "public_only_recovery"
	recoveryPhasePreflight = "final_reconnect_preflight"
)

// logRecoveryDecision emits the log lines a decision calls for, for the main
// round and the final preflight alike. Kept out of the transition function so
// that function stays pure and the loop stays thin.
func (c *IMClient) logRecoveryDecision(log zerolog.Logger, phase string, result internetprobe.Result, round recoveryRound, d recoveryDecision, e *recoveryEpisode) {
	log = log.With().Str("probe_phase", phase).Logger()
	if d.stabilityStarted {
		log.Info().Dur("required_stability", round.timing.stablePeriod).
			Msg("Public Internet probe succeeded; starting the continuous recovery-stability window")
	}
	if d.stabilityReset {
		switch round.verdict {
		case verdictBlocked:
			logBlockedProbe(log, result).
				Msg("Public Internet probe could not run; resetting the recovery-stability window but holding any pending rebuild request, because a probe that did not run is not evidence of an outage")
		default:
			log.Warn().Msg("Public Internet probe failed; resetting the recovery-stability window")
		}
	}
	if d.action == actionWithdraw {
		log.Warn().Msg("Public Internet probe failed with a rebuild request pending; withdrawing it without contacting Apple so bridgev2 does not rebuild into an outage")
	}
	if d.declineAlarm {
		log.Error().
			Int("request_attempt", e.attempt).
			Dur("since_first_request", round.now.Sub(e.firstRequestedAt)).
			Msg("Public Internet is reachable and a client rebuild was requested, but the client has not been rebuilt — the request may have been dropped from the bridge-state queue (lossy while the homeserver is unreachable) or declined; the iMessage client is torn down and this login may need a bridge restart")
	}
	if d.holdLog {
		log.Info().
			Dur("next_hand_back_in", round.handBackHold).
			Dur("hold_log_interval", round.retryDelay).
			Msg("Holding in Apple-free recovery: the previous hand-back was recent and consecutive hand-backs back off, so retrying Apple now would only repeat it")
	}
	if d.flapHoldLog {
		log.Info().
			Str("verdict", round.verdict.String()).
			Dur("next_flap_rebuild_in", round.flapHold).
			Dur("hold_log_interval", round.retryDelay).
			Int("flap_rebuilds", c.Main.flapRebuildCount(c.UserLogin.ID)).
			Msg("A rebuild is due, but the previous courier-failure rebuild was recent and consecutive ones back off (7m, 14m, 28m, then 45m); holding the rebuild request rather than rebuilding into another flap")
	}
	if d.holdAlarm {
		log.Error().
			Dur("offline_for", round.now.Sub(e.outageStartedAt)).
			Dur("episode_age", round.now.Sub(e.startedAt)).
			Dur("alarm_interval", round.timing.holdAlarmInterval).
			Msg("Public Internet has been confirmed unreachable for a long time; the iMessage client stays torn down and Apple will NOT be contacted until connectivity returns and remains stable — if this network is expected to be up, the bridge needs attention")
	}
	if d.action == actionHandBack {
		logBlockedProbe(log.With().
			Int("request_attempt", e.attempt).
			Dur("no_verdict_for", round.now.Sub(e.lastVerdictAt)).
			Dur("bridgev2_reconnect_delay", c.Main.Bridge.Config.UnknownErrorAutoReconnect).
			Logger(), result).Msg("Public Internet probe cannot run on this host, so recovery has no connectivity signal at all; handing control back to bridgev2 rather than staying in Apple-free mode on no evidence")
	}
}

// recoveryHoldNoticeText is the management-room notice for a long confirmed
// outage; the operator-facing twin of the holdAlarm log line.
func recoveryHoldNoticeText(offlineFor time.Duration) string {
	return fmt.Sprintf("iMessage bridge: the public Internet has been unreachable for %s. The iMessage client is held offline and Apple will not be contacted until connectivity returns and stays stable for %s. If this network is expected to be up, the bridge needs attention.",
		offlineFor.Round(time.Minute), internetRecoveryStablePeriod)
}

// sendRecoveryHoldNotice is the seam through which the loop posts that notice.
// Never reassigned in production.
var sendRecoveryHoldNotice = (*IMClient).postManagementNotice

// bridgeRecoveryContext returns the bridge-owned context that outlives one
// client epoch, so recovery keeps a cancellation source after teardown.
func (c *IMClient) bridgeRecoveryContext() context.Context {
	if c.Main == nil || c.Main.Bridge == nil || c.Main.Bridge.BackgroundCtx == nil {
		return context.Background()
	}
	return c.Main.Bridge.BackgroundCtx
}

// ---------------------------------------------------------------------------
// The per-round transition function
// ---------------------------------------------------------------------------
//
// Everything below is pure: no clock reads, no sends, no logs. A round is
// (episode state, recoveryRound) -> (episode state', recoveryDecision), and
// TestRecoveryStepDecisionTable enumerates it. Two structural properties do the
// work the old arms did by convention:
//
//  1. A pending request has a PREMISE, and a verdict that destroys the premise
//     withdraws the request. A "recovered" request rests on "the link is
//     stable"; an unusable-probe hand-back rests on "nothing is known to be
//     down". A confirmed Unreachable verdict destroys both, so it withdraws
//     either kind; a Blocked verdict (no evidence) destroys neither. The kind
//     is not what protects a hand-back from withdrawal — the hand-back
//     precondition (clause 7's invariant) is what keeps one from being sent
//     while the most recent verdict is Unreachable, so a hand-back that IS
//     sent is withdrawn only by genuinely new negative evidence.
//
//  2. The re-ask gate is evaluated BEFORE the hand-back clause. While bridgev2
//     has a request it has not had time to act on, no clock can preempt the
//     normal path: a reachable link keeps accumulating stability and re-asks
//     with im-internet-recovered the moment the gate opens. The round-7 latch
//     — a stale clock routing every later round into the hand-back arm —
//     cannot be expressed here either, and the relation between retryDelay
//     and the connector backoff is not load-bearing, which is what makes the
//     loop safe under operator-supplied unknown_error_auto_reconnect values.
//
//  3. A confirmed-Unreachable verdict NEVER produces a request, and once the
//     episode has concluded the network is down (outageConfirmed) nothing
//     produces a request until the network is positively re-established to
//     the rebuild standard. There is no outage ceiling and no episode ceiling
//     (both were removed in round 10: each contacted Apple while the probe
//     still said down, which the requirement forbids). The only hand-back is
//     the unusable-probe hatch, which fires on the ABSENCE of any conclusion
//     in an episode that holds no outage belief. A link that never stabilizes —
//     down for good, or flapping faster than the stability window forever —
//     therefore holds Apple-free indefinitely, and the loop makes that loud
//     rather than automatic: after holdAlarmAfter of continuous confirmed
//     outage it raises a throttled Error and a management-room notice
//     (clause 7). The episode ends on cancellation or shutdown, never by
//     contacting Apple.

// recoveryVerdict reduces a probe result to the three cases the decision table
// distinguishes. Blocked is checked before "not reachable" because Blocked
// implies !Reachable() and must never be read as an outage (probe.go's
// contract).
type recoveryVerdict int

const (
	verdictReachable recoveryVerdict = iota
	verdictUnreachable
	verdictBlocked
)

func (v recoveryVerdict) String() string {
	switch v {
	case verdictReachable:
		return "reachable"
	case verdictUnreachable:
		return "unreachable"
	default:
		return "blocked"
	}
}

func classifyRecoveryVerdict(result internetprobe.Result) recoveryVerdict {
	switch {
	case result.Blocked():
		return verdictBlocked
	case !result.Reachable():
		return verdictUnreachable
	default:
		return verdictReachable
	}
}

// recoveryPending names the rebuild request bridgev2 has been asked for and has
// not yet acted on (if it had, this loop would be dead: the rebuild closes
// recoveryDone).
type recoveryPending int

const (
	pendingNone recoveryPending = iota
	// im-internet-recovered: the probe said the link is stable. Withdrawn by an
	// unreachable round.
	pendingRecovered
	// im-internet-probe-unusable: the probe produced no verdict at all for the
	// whole grace period. Never withdrawn — bridgev2 acting on it despite the
	// probe is the intended outcome.
	pendingHandBack
)

func (p recoveryPending) String() string {
	switch p {
	case pendingRecovered:
		return "recovered"
	case pendingHandBack:
		return "hand_back"
	default:
		return "none"
	}
}

// recoveryAction is the one effect a step asks the loop to perform.
type recoveryAction int

const (
	actionNone recoveryAction = iota
	// Send StateTransientDisconnect: the pending recovered request is void.
	actionWithdraw
	// Run the final preflight probe, then call finishPreflight with its verdict.
	actionPreflight
	// Send StateUnknownError with recoveryDecision.handBackCode.
	actionHandBack
	// Only from finishPreflight: send StateUnknownError im-internet-recovered.
	actionRecovered
)

func (a recoveryAction) String() string {
	switch a {
	case actionWithdraw:
		return "withdraw"
	case actionPreflight:
		return "preflight"
	case actionHandBack:
		return "hand_back"
	case actionRecovered:
		return "recovered"
	default:
		return "none"
	}
}

// The one hand-back code left. "im-internet-offline-ceiling" no longer exists:
// a confirmed outage never hands back (see the section comment).
const handBackCodeProbeUnusable status.BridgeStateErrorCode = "im-internet-probe-unusable"

// recoveryTiming carries the package-level timing knobs into a step as plain
// values, so a table test can pin a decision to explicit durations instead of
// to whatever the package vars hold.
type recoveryTiming struct {
	stablePeriod      time.Duration
	blockedGrace      time.Duration
	holdAlarmAfter    time.Duration
	holdAlarmInterval time.Duration
}

func currentRecoveryTiming() recoveryTiming {
	return recoveryTiming{
		stablePeriod:      internetRecoveryStablePeriod,
		blockedGrace:      internetRecoveryBlockedGrace,
		holdAlarmAfter:    internetRecoveryHoldAlarmAfter,
		holdAlarmInterval: internetRecoveryHoldAlarmInterval,
	}
}

// recoveryRound is everything one round reads from outside the episode.
type recoveryRound struct {
	now        time.Time
	verdict    recoveryVerdict
	retryDelay time.Duration
	// Non-zero while the connector-level hand-back backoff is holding; the
	// value is how much longer it holds.
	handBackHold time.Duration
	// Non-zero while the flap run is holding a courier-failure rebuild; the
	// value is how much longer it holds. The loop sets it only for a
	// courier-failure episode, so for a confirmed outage it is always zero and
	// clauses 6 and 7 are unchanged. Consulted by both rebuild sources: the
	// stability-window preflight (clause 6) and the unusable-probe hand-back
	// (clause 7).
	flapHold time.Duration
	timing   recoveryTiming
}

// recoveryDecision is a step's output: exactly one action, plus independent
// log events that may accompany any action.
type recoveryDecision struct {
	action       recoveryAction
	handBackCode status.BridgeStateErrorCode // actionHandBack only

	stabilityStarted bool // first reachable round of a stability window
	stabilityReset   bool // a window was in progress and this round ended it
	holdLog          bool // the backoff hold is logged this round (throttled)
	flapHoldLog      bool // the flap-run hold on a due rebuild (clause 6 or 7) is logged this round (throttled)
	declineAlarm     bool // a request is old and unanswered on a reachable link
	holdAlarm        bool // a long confirmed outage is being held Apple-free (throttled)
}

// recoveryEpisode is the complete mutable state of one recovery episode. Every
// field is written only by step, finishPreflight and their helpers.
type recoveryEpisode struct {
	// startedAt is the episode clock, re-armed by a hand-back send. It drives
	// no decision any more (the episode ceiling is gone); it is logged so an
	// operator can see how long the loop has held.
	startedAt time.Time
	// outageStartedAt is the per-outage clock: reset by every reachable round.
	// It is what the hold alarm measures from, so a flapping link that keeps
	// resetting it is a different, quieter kind of hold than a dead link.
	outageStartedAt time.Time
	// lastVerdictAt is when the probe last produced a conclusion (Reachable or
	// Unreachable, from a main round or the final preflight), as opposed to
	// being refused a socket by this host — including the verdict the episode
	// was entered on. It is a timestamp, not a belief: it says how long the
	// probe has been silent, nothing about what it last said.
	lastVerdictAt time.Time
	// outageConfirmed is the episode's BELIEF that the network is down. SET by
	// every Unreachable conclusion — the entry verdict, a main round, or a
	// failed final preflight — and CLEARED by exactly one thing: the evidence
	// that authorizes a rebuild (a full stability window plus a passing
	// preflight, i.e. actionRecovered). A single Reachable sample does not
	// clear it; a most-recent sample is not a conclusion, and an earlier
	// version that used one as the hand-back guard let a confirmed outage
	// followed by one Reachable round and then Blocked rounds hand back.
	outageConfirmed bool

	stabilizing bool
	stable      internetprobe.Stability

	pending     recoveryPending
	requestedAt time.Time
	// firstRequestedAt is the FIRST request of the current streak; re-asks do
	// not re-stamp it, which is what makes the decline alarm reachable.
	firstRequestedAt time.Time
	attempt          int

	lastDeclineAlarmAt time.Time
	lastHoldLogAt      time.Time
	lastHoldAlarmAt    time.Time
}

// newRecoveryEpisode starts an episode whose most recent network knowledge is
// entry (see runPublicOnlyInternetRecovery).
func newRecoveryEpisode(now time.Time, entry recoveryVerdict) *recoveryEpisode {
	return &recoveryEpisode{startedAt: now, outageStartedAt: now, lastVerdictAt: now, outageConfirmed: entry == verdictUnreachable}
}

// step applies one probe round to the episode and names the loop's next effect.
// The order of the clauses IS the policy; each is numbered so a table-test case
// can cite the clause it pins.
func (e *recoveryEpisode) step(in recoveryRound) recoveryDecision {
	var d recoveryDecision
	now := in.now

	// 1. Conclusions. Shared with finishPreflight (noteConclusion), so a
	// preflight's verdict counts exactly as a main round's does.
	e.noteConclusion(now, in.verdict)

	// 2. Stability. Blocked cannot assert stability on absent evidence, so it
	// resets the window like an outage does — but see clause 3 for the
	// difference that matters.
	ready := false
	if in.verdict == verdictReachable {
		ready = e.stable.Observe(now, true, in.timing.stablePeriod)
		if !e.stabilizing {
			e.stabilizing = true
			d.stabilityStarted = true
		}
	} else {
		e.resetStability(&d)
	}

	// 3. Withdrawal. A verdict that destroys a pending request's premise voids
	// it (see recoveryPending.voidedBy); a Blocked round destroys nothing (one
	// EACCES round must not cost a bridgev2 wait cycle).
	if e.withdrawIfVoided(in.verdict, &d) {
		return d
	}

	// 4. Decline alarm. The Internet is back and a rebuild was requested, but
	// nobody tore this loop down — so the request was dropped or declined. The
	// client is already down, so silence here is a dead bridge.
	if in.verdict == verdictReachable && e.pending != pendingNone &&
		internetRecoveryDeclineAlarmDue(now, e.firstRequestedAt, e.lastDeclineAlarmAt, in.retryDelay) {
		e.lastDeclineAlarmAt = now
		d.declineAlarm = true
	}

	// 5. The re-ask gate. bridgev2 waits UnknownErrorAutoReconnect (jittered
	// +/-20%) before acting; until that window plus a margin has passed, a new
	// request would only replace one it may be about to honor. Evaluated
	// before the ceiling on purpose (see the section comment).
	if e.pending != pendingNone && now.Sub(e.requestedAt) < in.retryDelay {
		return d
	}

	// 6. Normal exit: a reachable link that has stayed up for the whole window
	// earns a final preflight and, if that passes, a rebuild request — unless
	// the flap run is holding it. The hold is a courier-failure episode's
	// widening schedule (the loop leaves flapHold zero for a confirmed
	// outage); the window keeps accumulating underneath it, so the round
	// after the hold expires reaches the preflight at once. Logged on the
	// re-ask cadence, like the hand-back hold, not every poll.
	if in.verdict == verdictReachable && ready {
		if in.flapHold > 0 {
			if internetRecoveryHoldLogDue(now, e.lastHoldLogAt, in.retryDelay) {
				e.lastHoldLogAt = now
				d.flapHoldLog = true
			}
			return d
		}
		d.action = actionPreflight
		return d
	}

	// 7. Holds, and the one hand-back.
	//
	// INVARIANT — the hand-back precondition: the unusable-probe hatch may
	// fire only when this episode has NEVER concluded the network is down, or
	// has since positively re-established it to the standard that authorizes
	// a rebuild (a full stability window plus a passing preflight, the only
	// thing that clears outageConfirmed) — AND the probe has produced no
	// conclusion of any kind for blockedGrace. It is a statement about the
	// episode's belief, not about the most recent sample and not about clause
	// order: a failed preflight sets the belief like any other Unreachable
	// conclusion, a single Reachable round does not clear it, and clause 3
	// enforces the same premise after the fact by withdrawing a pending
	// hand-back the moment an Unreachable conclusion arrives. A confirmed
	// outage therefore never reaches Apple through this clause, in either
	// direction: not before the hand-back (the precondition) and not after
	// (the withdrawal).
	//
	// Everything else here is a hold. A confirmed outage — Unreachable rounds,
	// or Blocked rounds after one — is held Apple-free for as long as it lasts
	// and made loud rather than automatic: after holdAlarmAfter of the current
	// outage the loop alarms (throttled to holdAlarmInterval) and keeps
	// holding.
	probeUnusable := in.verdict == verdictBlocked && !e.outageConfirmed && now.Sub(e.lastVerdictAt) >= in.timing.blockedGrace
	if !probeUnusable {
		held := in.verdict == verdictUnreachable || (in.verdict == verdictBlocked && e.outageConfirmed)
		if held && now.Sub(e.outageStartedAt) >= in.timing.holdAlarmAfter &&
			internetRecoveryHoldLogDue(now, e.lastHoldAlarmAt, in.timing.holdAlarmInterval) {
			e.lastHoldAlarmAt = now
			d.holdAlarm = true
		}
		return d
	}
	// Both connector-level backoffs hold the hatch: the flap run (a
	// courier-failure episode's widening schedule — zero for a confirmed
	// outage, which cannot reach here anyway) and the hand-back run. Each is
	// logged once per re-ask cadence, not every poll: a permanently wrong
	// probe otherwise writes ~230 Info lines per 45-minute hold, forever. The
	// two holds share lastHoldLogAt on purpose — one "held" line per cadence
	// is the budget, whichever backoff is holding.
	if in.flapHold > 0 {
		if internetRecoveryHoldLogDue(now, e.lastHoldLogAt, in.retryDelay) {
			e.lastHoldLogAt = now
			d.flapHoldLog = true
		}
		return d
	}
	if in.handBackHold > 0 {
		if internetRecoveryHoldLogDue(now, e.lastHoldLogAt, in.retryDelay) {
			e.lastHoldLogAt = now
			d.holdLog = true
		}
		return d
	}
	e.noteRequest(now, pendingHandBack)
	e.startedAt = now
	d.action = actionHandBack
	d.handBackCode = handBackCodeProbeUnusable
	return d
}

// finishPreflight completes an actionPreflight round with the preflight's
// verdict. A passing preflight requests the rebuild. Any other verdict is
// scored by the SAME clause-2 and clause-3 helpers step uses, so the preflight
// cannot rate a verdict differently from a main round: Unreachable resets the
// window and withdraws whatever request is pending (its premise is gone,
// whichever kind it was); Blocked resets the window and withdraws nothing,
// because the probe not running is not evidence of an outage (probe.go's
// contract). The parameter is the three-valued verdict for
// that reason — a bool collapsed Blocked into Unreachable here and cost a
// bridgev2 wait cycle on a descriptor-exhaustion round (round-8 finding 2).
func (e *recoveryEpisode) finishPreflight(now time.Time, verdict recoveryVerdict) recoveryDecision {
	var d recoveryDecision
	e.noteConclusion(now, verdict)
	if verdict == verdictReachable {
		// The one place the outage belief is cleared: the same evidence that
		// authorizes the rebuild.
		e.outageConfirmed = false
		e.noteRequest(now, pendingRecovered)
		d.action = actionRecovered
		return d
	}
	e.resetStability(&d)
	e.withdrawIfVoided(verdict, &d)
	return d
}

// noteConclusion is the bookkeeping every conclusion performs, whether it came
// from a main round or the final preflight: a conclusion stamps lastVerdictAt;
// a Reachable one ends the current outage's clock; an Unreachable one sets the
// outage belief. Nothing clears the belief here — see finishPreflight. A
// Blocked verdict is not a conclusion and changes nothing.
func (e *recoveryEpisode) noteConclusion(now time.Time, verdict recoveryVerdict) {
	switch verdict {
	case verdictReachable:
		e.lastVerdictAt = now
		e.outageStartedAt = now
	case verdictUnreachable:
		e.lastVerdictAt = now
		e.outageConfirmed = true
	}
}

// resetStability is clause 2's non-reachable half: end any stability window in
// progress and report whether there was one to end.
func (e *recoveryEpisode) resetStability(d *recoveryDecision) {
	d.stabilityReset = e.stabilizing
	e.stabilizing = false
	e.stable.Reset()
}

// withdrawIfVoided is clause 3: a pending request whose premise the verdict
// destroys is withdrawn. Reports whether a withdrawal was decided.
func (e *recoveryEpisode) withdrawIfVoided(verdict recoveryVerdict, d *recoveryDecision) bool {
	if !e.pending.voidedBy(verdict) {
		return false
	}
	e.clearPending()
	d.action = actionWithdraw
	return true
}

// voidedBy reports whether a verdict destroys the premise a pending request
// was made on. The rule follows the premise, not the kind: pendingRecovered
// was sent because the link was stable, pendingHandBack because nothing was
// known to be down, and a confirmed Unreachable verdict contradicts both.
// (When hand-backs came from an outage ceiling they already implied a long
// confirmed outage and withdrawing one made no sense; now that a hand-back's
// only ground is the absence of evidence, new negative evidence must void it,
// or bridgev2 would reconnect to Apple 4-6 minutes into a confirmed outage.)
// Blocked is the absence of evidence and voids nothing.
func (p recoveryPending) voidedBy(verdict recoveryVerdict) bool {
	return p != pendingNone && verdict == verdictUnreachable
}

// noteRequest records a rebuild request of the given kind. A re-ask replaces
// the pending kind (a recovered request after a hand-back is the truthful one)
// but keeps the streak's first timestamp.
func (e *recoveryEpisode) noteRequest(now time.Time, kind recoveryPending) {
	e.attempt++
	e.pending = kind
	e.requestedAt = now
	if e.firstRequestedAt.IsZero() {
		e.firstRequestedAt = now
	}
	e.lastHoldLogAt = time.Time{}
}

func (e *recoveryEpisode) clearPending() {
	e.pending = pendingNone
	e.requestedAt = time.Time{}
	e.firstRequestedAt = time.Time{}
}

// internetRecoveryDeclineAlarmDue reports whether to warn that a requested rebuild
// has not happened. Measured from the FIRST request in a streak, never the most
// recent: re-asks land every retryDelay, so measuring from the latest one could
// never reach a 2x threshold and the alarm was dead code. Throttled to the re-ask
// cadence, because the threshold is otherwise a pure function of now and would
// emit an Error every poll interval forever once armed.
func internetRecoveryDeclineAlarmDue(now, firstRequestedAt, lastAlarmAt time.Time, retryDelay time.Duration) bool {
	if firstRequestedAt.IsZero() || now.Sub(firstRequestedAt) < 2*retryDelay {
		return false
	}
	return lastAlarmAt.IsZero() || now.Sub(lastAlarmAt) >= retryDelay
}

// internetRecoveryHoldLogDue throttles the backoff-hold notice to once per
// re-ask cadence. The first hold of a streak always logs.
func internetRecoveryHoldLogDue(now, lastHoldLogAt time.Time, interval time.Duration) bool {
	return lastHoldLogAt.IsZero() || now.Sub(lastHoldLogAt) >= interval
}

// runLoggedInternetProbe performs one logged probe round outside a recovery
// episode, where there is no rolling logger to throttle against.
func runLoggedInternetProbe(ctx context.Context, log zerolog.Logger, phase string) internetprobe.Result {
	var once internetProbeLogger
	return once.run(ctx, log, phase)
}

func logInternetProbeResult(log zerolog.Logger, phase string, result internetprobe.Result, round int, verbose bool) {
	event := log.Debug()
	if verbose {
		event = log.Info()
	}
	event.
		Str("platform", runtime.GOOS).
		Str("probe_phase", phase).
		Int("probe_round", round).
		Str("cloudflare_target", internetprobe.CloudflareTarget).
		Bool("cloudflare_reachable", result.Cloudflare == internetprobe.OutcomeReachable).
		Str("cloudflare_outcome", result.Cloudflare.String()).
		AnErr("cloudflare_error", result.CloudflareErr).
		Str("google_target", internetprobe.GoogleTarget).
		Bool("google_reachable", result.Google == internetprobe.OutcomeReachable).
		Str("google_outcome", result.Google.String()).
		AnErr("google_error", result.GoogleErr).
		Bool("internet_reachable", result.Reachable()).
		Bool("probe_blocked", result.Blocked()).
		Msg("Public Internet connectivity test completed")
}

// logBlockedProbe builds an Error-level event carrying why the probe could not
// reach a verdict. A host that cannot run the probe is an operator problem, so
// it must be visible in `corten-matrix logs` rather than inferred from silence.
func logBlockedProbe(log zerolog.Logger, result internetprobe.Result) *zerolog.Event {
	return log.Error().
		Str("platform", runtime.GOOS).
		Str("cloudflare_outcome", result.Cloudflare.String()).
		AnErr("cloudflare_error", result.CloudflareErr).
		Str("google_outcome", result.Google.String()).
		AnErr("google_error", result.GoogleErr)
}

func sendInternetRecoveryState(bridge *bridgev2.Bridge, bridgeState *bridgev2.BridgeStateQueue, state status.BridgeState) (sent bool) {
	if bridge == nil || bridge.IsStopping() || bridgeState == nil {
		return false
	}
	defer func() {
		if recover() != nil {
			sent = false
		}
	}()
	bridgeState.Send(state)
	return true
}

func internetRecoveryStateRetryDelay(autoReconnect time.Duration) time.Duration {
	if autoReconnect < time.Minute {
		autoReconnect = time.Minute
	}
	// bridgev2 jitters by +/-20% (rand over 0.4x, minus 0.2x), so its longest
	// wait is 1.2x. Retry the reconstruction signal only
	// after the complete window plus a margin, never the Apple connection itself.
	return autoReconnect + autoReconnect/5 + internetReconnectRetryMargin
}

func channelClosed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func internetRecoveryCanceled(ctx context.Context, recoveryDone <-chan struct{}) bool {
	select {
	case <-ctx.Done():
		return true
	case <-recoveryDone:
		return true
	default:
		return false
	}
}

func waitForInternetRecoveryContext(ctx context.Context, recoveryDone <-chan struct{}, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-recoveryDone:
		return false
	case <-timer.C:
		return true
	}
}
