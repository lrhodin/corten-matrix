// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package connector

import (
	"time"

	"maunium.net/go/mautrix/bridgev2/networkid"
)

// Flap-recovery run: the per-login memory that a courier keeps coming up and
// going down again across client rebuilds.
//
// This is a SEPARATE run from handBackRun, deliberately. The two measure
// different failure modes and so are refuted by different evidence:
//
//   - handBackRun counts rebuilds that never produced a working courier (an
//     unusable-probe hand-back, a wedge rebuild). ONE inbound APS frame refutes
//     that — the courier works — so the wedge watchdog clears it on the first
//     frame of the epoch, and a client that receives and later stalls (the
//     ordinary daily wedge) is rebuilt at once. That first-frame clear is
//     load-bearing for the wedge path and must not be weakened.
//   - flapRecoveryRun counts rebuilds after the courier FAILED ON A LINK THE
//     PROBE DID NOT CALL DOWN: a Generated -> Generating storm, or a sustained
//     regeneration failure (RetryFailed), either way recovered through the
//     public-only loop — by its 60-second stability window, or by the
//     unusable-probe hand-back on a host whose probe cannot run. A flapping
//     courier delivers frames between flaps, so one frame refutes nothing
//     here; only a lease of healthy time does. Folding this into handBackRun
//     would force one of the two clear rules on both — the first-frame clear
//     would make this run never widen, and the lease would hold the
//     daily-wedge rebuild that today rebuilds immediately.
//
// Separation is also what keeps confirmed-outage recovery on normal timing:
// an episode entered on a confirmed outage never consults or writes this run
// (internet_recovery.go, courierFailure), and it never consults handBackRun on
// its recovered path either. Only a replacement that itself starts flapping
// enters a courier-failure episode and meets the widening schedule.
//
// INVARIANT — what a live run asserts: "every client built for this login
// since the run began was built to replace a courier that failed with no
// outage evidence, and no client since then has accrued a full lease of
// healthy time within its own epoch". It is a statement about the whole
// sequence, not about the most recent sample: one frame, one quiet tick, or
// one clean Connect does not refute it, and an interruption after ten healthy
// minutes does not re-assert it either — it only pauses the accrual. It is
// refuted by exactly two things — a client whose healthy time within its
// epoch reaches flapRecoveryHealthyLease (noteCourierHealthy), or a logout
// (clearFlapRun) — and by a process restart, because the run is memory-only:
// a deliberate restart is the operator's own reconnect and starts clean.
//
// "Healthy time" (see noteCourierHealthy/noteCourierUnhealthy) accrues in
// stretches within the current client epoch. A stretch begins at the first
// receive-watchdog tick that finds the inbound-frame age under
// courierHealthyMaxIdleSecs — after a real frame has been seen this epoch,
// the seeded stamp is not health — and ends at the next APS connection event
// (Interrupted or RetryFailed) or the next tick that finds the age past the
// bound. A stretch that ends is BANKED, not discarded: what an interruption
// costs the run is the unhealthy time that follows it, never the healthy time
// before it. A rebuild (noteFlapRebuild) zeroes the bank, so the lease is
// always one epoch's own healthy time.
//
// Why cumulative rather than contiguous: rustpush's own receive watchdog
// (lib.rs, APNS_STALL_TIMEOUT = 300s) forces a transport-only reconnect on a
// link that stopped receiving, and the APS observer reports that reconnect as
// Interrupted (aps_connection_events.rs: Generated -> Generating). A link that
// stalls and self-heals every few minutes therefore interrupts on every cycle
// while never storming (the burst and sustained thresholds) and never wedging
// (receiveWedgeRecoverySecs), so no recovery episode fires and nothing rebuilds
// it. A contiguous lease was unreachable on that link, and the run pinned —
// with presence deferred — until a process restart. With accrual the lease is
// served in bounded time on ANY epoch that receives: an epoch that survives
// sees a frame at least every receiveWedgeRecoverySecs (600s, or the wedge
// watchdog rebuilds it), each frame is followed by healthy ticks until the
// age passes 300s, so at least about 240s of every 600s accrues and the lease
// is served within roughly 2.5x flapRecoveryHealthyLease — under 40 minutes —
// of the first confirmed frame. A non-receiving epoch is rebuilt at the wedge
// threshold, held at most handBackDelay's cap. No epoch stays deferred for
// more than about an hour. A courier that flaps at storm rate (5 in 60s, or
// 12 in 10 minutes) enters recovery — and is torn down, bank and all — before
// it can accrue the 15-minute lease, and a RetryFailed enters at once; a
// courier that flaps below storm rate is, for this run's purpose, not
// flapping: no rebuild is being requested, and the run measures rebuilds.
//
// A healthy link sustains a stretch indefinitely: rustpush sends a keepalive
// Ping every 60s (aps.rs), the Pong is broadcast on messages_cont like every
// other frame and stamps last_inbound_ms (lib.rs drain task), so the age the
// watchdog reads on an idle link is at most about 60s at every tick. The
// bound is rustpush's own stall definition — five missed keepalive cycles —
// so a quiet minute cannot end a stretch and a dead-but-not-yet-wedged link
// cannot pass as healthy for more than five of them.
//
// Writers of the fields, in full:
//   - noteFlapRebuild:      ConsecutiveRebuilds++, LastRebuild = now, HealthySince = zero, HealthyAccrued = 0
//   - noteCourierHealthy:   HealthySince = now (first tick of a stretch); deletes the run once accrued + current stretch >= lease
//   - noteCourierUnhealthy: HealthyAccrued += now - HealthySince (banks the stretch), HealthySince = zero
//   - clearFlapRun:         deletes the run (LogoutRemote)
type flapRecoveryRun struct {
	// ConsecutiveRebuilds is how many courier-failure rebuilds this run has
	// requested. The Nth request is held handBackDelay(N-1) after the (N-1)th:
	// the first is not held at all, then 7m, 14m, 28m, 45m.
	ConsecutiveRebuilds int
	// LastRebuild is when the most recent request was sent (stamped at the
	// send, unconditionally, exactly as noteHandBack does — an unobservable
	// queue drop must err toward fewer Apple attempts).
	LastRebuild time.Time
	// HealthySince is the start of the current healthy stretch, or zero while
	// the courier is not known to be healthy.
	HealthySince time.Time
	// HealthyAccrued is the banked healthy time of the stretches that have
	// already ended in this epoch. The lease is served when it plus the
	// current stretch reaches flapRecoveryHealthyLease.
	HealthyAccrued time.Duration
	// HealthyAfter is the latest interruption/rebuild boundary. A frame must
	// have arrived after this instant before a new healthy stretch may start.
	HealthyAfter time.Time
	// RebuildRequested preserves the lineage of a recovery state until an actual
	// replacement consumes it or explicit logout clears the run. A withdrawal
	// cannot safely erase this bit: bridgev2 may already have passed its final
	// state check and be about to construct the client. The bit alone changes no
	// delay or StatusKit behavior; only consumption advances the count.
	RebuildRequested bool
}

// flapRecoveryHealthyLease is how much healthy time a courier must accrue
// within one epoch to end a flap run — and, for an epoch whose StatusKit
// startup was deferred, to start it. A var only so tests can shrink it; never
// reassigned in production.
var flapRecoveryHealthyLease = 15 * time.Minute

// courierHealthyMaxIdleSecs is the inbound-frame age at and above which a tick
// does not count as healthy. It is rustpush's APNS_STALL_TIMEOUT (lib.rs):
// five missed keepalive cycles, the point at which the Rust side itself forces
// a transport reconnect. Kept as a literal rather than derived from
// receiveWedgeRecoverySecs so the two thresholds stay independently pinned.
const courierHealthyMaxIdleSecs uint64 = 300

// flapRecoveryStatusKitDeferralThreshold is the rebuild count from which an
// epoch defers its StatusKit startup: the FIRST courier-failure rebuild (count
// 1) preserves today's behavior exactly, every later one is "repeated".
const flapRecoveryStatusKitDeferralThreshold = 2

func (c *IMConnector) flapRun(login networkid.UserLoginID) *flapRecoveryRun {
	if c.flapRuns == nil {
		c.flapRuns = make(map[networkid.UserLoginID]*flapRecoveryRun)
	}
	run := c.flapRuns[login]
	if run == nil {
		run = &flapRecoveryRun{}
		c.flapRuns[login] = run
	}
	return run
}

// flapRebuildDue reports whether a courier-failure rebuild may be requested
// now, and if not, how much longer the widening schedule holds it. The first
// actual replacement of a run is never held; the run's clock starts only when
// LoadUserLogin constructs that replacement, not when a lossy state is sent.
func (c *IMConnector) flapRebuildDue(login networkid.UserLoginID, now time.Time) (wait time.Duration, ok bool) {
	c.flapRunMu.Lock()
	defer c.flapRunMu.Unlock()
	run := c.flapRuns[login]
	if run == nil || run.ConsecutiveRebuilds == 0 || run.LastRebuild.IsZero() {
		return 0, true
	}
	required := handBackDelay(run.ConsecutiveRebuilds)
	if elapsed := now.Sub(run.LastRebuild); elapsed < required {
		return required - elapsed, false
	}
	return 0, true
}

// markFlapRebuildRequested records that bridgev2 was asked to replace the
// client. It deliberately does not advance the widening schedule: BridgeState
// delivery is lossy, so only LoadUserLogin consuming the request proves a new
// APS ResourceManager was actually constructed.
func (c *IMConnector) markFlapRebuildRequested(login networkid.UserLoginID) {
	c.flapRunMu.Lock()
	defer c.flapRunMu.Unlock()
	c.flapRun(login).RebuildRequested = true
}

// consumeFlapRebuildRequest is called exactly when LoadUserLogin constructs the
// requested replacement. It advances the run once, even if recovery sent the
// same request repeatedly before bridgev2 acted.
func (c *IMConnector) consumeFlapRebuildRequest(login networkid.UserLoginID, now time.Time) bool {
	c.flapRunMu.Lock()
	defer c.flapRunMu.Unlock()
	run := c.flapRuns[login]
	if run == nil || !run.RebuildRequested {
		return false
	}
	run.RebuildRequested = false
	c.noteFlapRebuildLocked(run, now)
	return true
}

// noteFlapRebuild records an actual replacement directly. Production uses
// consumeFlapRebuildRequest; this entry remains useful for deterministic state
// tests and callers that already possess proof of construction.
func (c *IMConnector) noteFlapRebuild(login networkid.UserLoginID, now time.Time) {
	c.flapRunMu.Lock()
	defer c.flapRunMu.Unlock()
	c.noteFlapRebuildLocked(c.flapRun(login), now)
}

func (c *IMConnector) noteFlapRebuildLocked(run *flapRecoveryRun, now time.Time) {
	run.ConsecutiveRebuilds++
	run.LastRebuild = now
	run.HealthySince = time.Time{}
	run.HealthyAccrued = 0
	run.HealthyAfter = now
}

// flapRebuildCount is the run's length, for logging and for Connect's
// StatusKit decision; zero when there is no run.
func (c *IMConnector) flapRebuildCount(login networkid.UserLoginID) int {
	c.flapRunMu.Lock()
	defer c.flapRunMu.Unlock()
	if run := c.flapRuns[login]; run != nil {
		return run.ConsecutiveRebuilds
	}
	return 0
}

func (c *IMConnector) flapRebuildRequested(login networkid.UserLoginID) bool {
	c.flapRunMu.Lock()
	defer c.flapRunMu.Unlock()
	run := c.flapRuns[login]
	return run != nil && run.RebuildRequested
}

// flapRecoveryDefersStatusKit reports whether a Connect happening now is a
// REPEATED courier-failure rebuild, in which case the epoch brings core
// APNs/iMessage up normally and holds its optional StatusKit startup until the
// health lease clears the run. A fresh IMConnector (a process restart) has no
// run and never defers.
func (c *IMConnector) flapRecoveryDefersStatusKit(login networkid.UserLoginID) bool {
	return c.flapRebuildCount(login) >= flapRecoveryStatusKitDeferralThreshold
}

// noteCourierActivity records a watchdog observation whose last inbound frame is
// conservatively known to be no earlier than frameAt. A new stretch may start
// only when that frame is after the latest rebuild/interruption boundary.
func (c *IMConnector) noteCourierActivity(login networkid.UserLoginID, frameAt, now time.Time) (cleared bool) {
	c.flapRunMu.Lock()
	defer c.flapRunMu.Unlock()
	run := c.flapRuns[login]
	if run == nil || !frameAt.After(run.HealthyAfter) {
		return false
	}
	if frameAt.After(now) {
		frameAt = now
	}
	if run.HealthySince.IsZero() {
		run.HealthySince = frameAt
	}
	if run.HealthyAccrued+now.Sub(run.HealthySince) < flapRecoveryHealthyLease {
		return false
	}
	delete(c.flapRuns, login)
	return true
}

// noteCourierHealthy is the direct-observation form used by state tests.
func (c *IMConnector) noteCourierHealthy(login networkid.UserLoginID, now time.Time) bool {
	return c.noteCourierActivity(login, now, now)
}

// noteCourierUnhealthy ends and banks the current stretch and establishes a
// boundary that requires a later inbound frame before accrual can resume.
func (c *IMConnector) noteCourierUnhealthy(login networkid.UserLoginID, now time.Time) {
	c.flapRunMu.Lock()
	defer c.flapRunMu.Unlock()
	run := c.flapRuns[login]
	if run == nil {
		return
	}
	if !run.HealthySince.IsZero() {
		if stretch := now.Sub(run.HealthySince); stretch > 0 {
			run.HealthyAccrued += stretch
		}
		run.HealthySince = time.Time{}
	}
	if now.After(run.HealthyAfter) {
		run.HealthyAfter = now
	}
}

// flapRunHealthySince exposes the current stretch's start for logging and
// tests; zero when no stretch is running.
func (c *IMConnector) flapRunHealthySince(login networkid.UserLoginID) time.Time {
	c.flapRunMu.Lock()
	defer c.flapRunMu.Unlock()
	if run := c.flapRuns[login]; run != nil {
		return run.HealthySince
	}
	return time.Time{}
}

// flapRunHealthyAccrued exposes the banked healthy time of the ended stretches
// for logging and tests; zero when there is no run.
func (c *IMConnector) flapRunHealthyAccrued(login networkid.UserLoginID) time.Duration {
	c.flapRunMu.Lock()
	defer c.flapRunMu.Unlock()
	if run := c.flapRuns[login]; run != nil {
		return run.HealthyAccrued
	}
	return 0
}

// clearFlapRun ends the run outright. Called by LogoutRemote, so a later
// re-login with the same ID starts with no backoff carried over.
func (c *IMConnector) clearFlapRun(login networkid.UserLoginID) {
	c.flapRunMu.Lock()
	defer c.flapRunMu.Unlock()
	delete(c.flapRuns, login)
}
