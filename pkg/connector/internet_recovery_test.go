package connector

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/lrhodin/corten-matrix/pkg/internetprobe"
	"github.com/lrhodin/corten-matrix/pkg/rustpushgo"
)

// newRecoveryTestClient builds the minimum IMClient the recovery loop needs: a
// Bridge (IsStopping reads an atomic, so the zero value is "running"), a non-nil
// bridge-state queue pointer (the send itself goes through the sendRecoveryState
// seam), and this epoch's channels. c.client stays nil, so teardown is a no-op.
func newRecoveryTestClient() *IMClient {
	return &IMClient{
		Main: &IMConnector{Bridge: &bridgev2.Bridge{
			Config: &bridgeconfig.BridgeConfig{UnknownErrorAutoReconnect: 5 * time.Minute},
		}},
		// The hand-back arm consults the connector-level backoff keyed by login ID.
		UserLogin:           &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "recovery-test"}},
		recoveryBridgeState: &bridgev2.BridgeStateQueue{},
		recoveryDone:        make(chan struct{}),
		stopChan:            make(chan struct{}),
	}
}

// runRecoveryLoopForTest starts the real loop on client and registers a cleanup
// that retires the episode and waits for the goroutine to exit. Every loop test
// MUST start the loop this way: a t.Fatal before Disconnect leaked the goroutine,
// which then read the package-level seams and timing vars while the NEXT test
// wrote them — one genuine regression produced 11 data-race reports on top of
// the intended failure, so the suite was least trustworthy exactly when it
// mattered. The returned stop is idempotent, for tests that must observe the
// loop's exit before asserting on what it sent.
func runRecoveryLoopForTest(t *testing.T, client *IMClient) (stop func()) {
	t.Helper()
	return runRecoveryLoopForTestFrom(t, client, verdictUnreachable)
}

// runRecoveryLoopForTestFrom is runRecoveryLoopForTest with an explicit entry
// verdict: verdictUnreachable models an episode entered on a confirmed outage
// (the common case), verdictReachable an Apple-specific failure on a healthy
// link — the only entry from which a run of Blocked rounds can hand back.
func runRecoveryLoopForTestFrom(t *testing.T, client *IMClient, entry recoveryVerdict) (stop func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		client.runPublicOnlyInternetRecovery(zerolog.Nop(), entry)
		close(done)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			client.Disconnect()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("recovery loop did not exit after Disconnect")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func TestInternetProbeResultLogIncludesPhaseTargetsAndOutcomes(t *testing.T) {
	var output bytes.Buffer
	logger := zerolog.New(&output)
	logInternetProbeResult(logger, "outage_recovery", internetprobe.Result{
		Cloudflare:    internetprobe.OutcomeUnreachable,
		CloudflareErr: errors.New("no route to host"),
		Google:        internetprobe.OutcomeReachable,
	}, 3, true)

	line := output.String()
	for _, want := range []string{
		`"level":"info"`,
		`"probe_phase":"outage_recovery"`,
		`"probe_round":3`,
		`"cloudflare_target":"1.1.1.1"`,
		`"cloudflare_reachable":false`,
		`"cloudflare_outcome":"unreachable"`,
		`"cloudflare_error":"no route to host"`,
		`"google_target":"8.8.8.8"`,
		`"google_reachable":true`,
		`"google_outcome":"reachable"`,
		`"internet_reachable":true`,
		`"probe_blocked":false`,
		`Public Internet connectivity test completed`,
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("probe log %q does not contain %q", line, want)
		}
	}
}

// A blocked probe is an operator problem, so it must be distinguishable in
// `corten-matrix logs` from an ordinary outage rather than looking like one.
func TestBlockedProbeLogsAtErrorWithPerTargetCause(t *testing.T) {
	var output bytes.Buffer
	logger := zerolog.New(&output)
	logBlockedProbe(logger, internetprobe.Result{
		Cloudflare:    internetprobe.OutcomeBlocked,
		CloudflareErr: syscall.EACCES,
		Google:        internetprobe.OutcomeBlocked,
		GoogleErr:     syscall.EPERM,
	}).Msg("probe unusable")

	line := output.String()
	for _, want := range []string{
		`"level":"error"`,
		`"cloudflare_outcome":"blocked"`,
		`"google_outcome":"blocked"`,
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("blocked-probe log %q does not contain %q", line, want)
		}
	}
}

// An overnight outage must not write an unbounded stream of identical Info
// lines, but entering and leaving the outage must stay visible at Info.
func TestProbeLoggerThrottlesSteadyStateButNotTransitions(t *testing.T) {
	offline := internetprobe.Result{
		Cloudflare: internetprobe.OutcomeUnreachable,
		Google:     internetprobe.OutcomeUnreachable,
	}
	online := internetprobe.Result{
		Cloudflare: internetprobe.OutcomeReachable,
		Google:     internetprobe.OutcomeReachable,
	}

	var probeLog internetProbeLogger
	for round := 1; round <= internetProbeVerboseRounds; round++ {
		gotRound, opening := probeLog.beginRound()
		if gotRound != round {
			t.Fatalf("round = %d, want %d", gotRound, round)
		}
		if !opening {
			t.Fatalf("round %d should be an opening round", round)
		}
		if !probeLog.recordResult(offline, opening) {
			t.Fatalf("round %d should log at Info", round)
		}
	}

	// Past the opening rounds an unchanged verdict drops to Debug.
	for range 200 {
		_, opening := probeLog.beginRound()
		if opening {
			t.Fatal("beginRound kept reporting opening rounds past the limit")
		}
		if probeLog.recordResult(offline, opening) {
			t.Fatal("an unchanged steady-state verdict should log at Debug")
		}
	}

	// Recovery is a change of verdict, so it goes back to Info.
	_, opening := probeLog.beginRound()
	if !probeLog.recordResult(online, opening) {
		t.Fatal("a change of verdict must log at Info even deep into an outage")
	}
	if probeLog.recordResult(online, false) {
		t.Fatal("the repeat of a changed verdict should drop back to Debug")
	}
}

// Both escape hatches out of Apple-free mode exist so a probe that can never
// succeed cannot become a permanently dead bridge. Keep them ordered against
// the poll interval and the stability window.
func TestInternetRecoveryFallbackCeilingsAreOrdered(t *testing.T) {
	if internetRecoveryBlockedGrace <= internetRecoveryPollInterval {
		t.Fatal("the unusable-probe grace must span several poll rounds")
	}
	if internetRecoveryHoldAlarmAfter <= internetRecoveryStablePeriod {
		t.Fatal("the hold alarm must outlast the stability window, or a healthy recovery could alarm")
	}
	if internetRecoveryHoldAlarmInterval < internetRecoveryPollInterval {
		t.Fatal("the hold alarm must be throttled to more than one poll round")
	}
	// Pinned with literals: the alarm cadence is what an operator is promised.
	if internetRecoveryHoldAlarmAfter != 30*time.Minute || internetRecoveryHoldAlarmInterval != 30*time.Minute {
		t.Fatalf("hold alarm after %v every %v, want 30m/30m", internetRecoveryHoldAlarmAfter, internetRecoveryHoldAlarmInterval)
	}
}

func TestConnectionEventLatchPreservesRetryFailurePriority(t *testing.T) {
	client := &IMClient{connectionEventWake: make(chan struct{}, 1)}
	for range 100 {
		client.OnConnectionEvent(rustpushgo.ApsConnectionEventInterrupted)
	}
	client.OnConnectionEvent(rustpushgo.ApsConnectionEventRetryFailed)

	select {
	case <-client.connectionEventWake:
	default:
		t.Fatal("connection event did not wake recovery loop")
	}
	event, ok := client.takeConnectionEvent()
	if !ok || event != rustpushgo.ApsConnectionEventRetryFailed {
		t.Fatalf("pending event = (%v, %v), want RetryFailed", event, ok)
	}
}

func TestConnectCannotBeginAfterLifecycleDisconnect(t *testing.T) {
	client := &IMClient{}
	client.Disconnect()

	if client.beginConnect() {
		client.endConnect()
		t.Fatal("Connect began after normal lifecycle teardown completed")
	}
}

func TestDisconnectWaitsForConnectLifecycle(t *testing.T) {
	client := &IMClient{
		stopChan:     make(chan struct{}),
		recoveryDone: make(chan struct{}),
	}
	client.lifecycleMu.Lock()
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		client.Disconnect()
		close(done)
	}()
	<-started

	select {
	case <-done:
		client.lifecycleMu.Unlock()
		t.Fatal("Disconnect completed while Connect lifecycle lock was held")
	default:
	}
	client.lifecycleMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Disconnect did not resume after Connect lifecycle lock was released")
	}
}

func TestDisconnectKeepsClosedStopChannelObservable(t *testing.T) {
	stop := make(chan struct{})
	recoveryDone := make(chan struct{})
	client := &IMClient{
		stopChan:     stop,
		recoveryDone: recoveryDone,
	}

	client.Disconnect()
	client.Disconnect() // must remain idempotent

	if client.stopChan == nil {
		t.Fatal("Disconnect replaced the closed stop channel with nil")
	}
	select {
	case <-stop:
	default:
		t.Fatal("Disconnect did not close the worker stop channel")
	}
	select {
	case <-recoveryDone:
	default:
		t.Fatal("lifecycle Disconnect did not cancel Internet recovery")
	}
}

func TestRecoveryDisconnectLeavesBridgeRecoveryAlive(t *testing.T) {
	recoveryDone := make(chan struct{})
	client := &IMClient{
		stopChan:     make(chan struct{}),
		recoveryDone: recoveryDone,
	}

	client.disconnectForInternetRecovery()

	select {
	case <-recoveryDone:
		t.Fatal("recovery teardown canceled its own bridge-owned recovery loop")
	default:
	}
	select {
	case <-client.stopChan:
	default:
		t.Fatal("recovery teardown did not stop client-epoch workers")
	}
}

func TestInternetRecoveryStateRetryDelayOutlastsBridgeJitter(t *testing.T) {
	const configured = 5 * time.Minute
	want := configured + configured/5 + internetReconnectRetryMargin
	if got := internetRecoveryStateRetryDelay(configured); got != want {
		t.Fatalf("retry delay = %s, want %s", got, want)
	}
}

// Round-10 blocker B2: a confirmed outage NEVER hands back, however long it
// lasts and however the link flaps. A flapping link is held quietly (the alarm
// measures the current outage, which every up phase resets); a dead link is
// held loudly — an alarm at holdAlarmAfter and every holdAlarmInterval after.
func TestConfirmedOutageIsHeldAppleFreeAndAlarms(t *testing.T) {
	timing := currentRecoveryTiming()
	const retryDelay = 7 * time.Minute
	start := time.Unix(10_000, 0)
	e := newRecoveryEpisode(start, verdictUnreachable)
	now := start
	step := func(verdict recoveryVerdict) recoveryDecision {
		now = now.Add(internetRecoveryPollInterval)
		return e.step(recoveryRound{now: now, verdict: verdict, retryDelay: retryDelay, timing: timing})
	}

	// Flapping for hours: up phases shorter than the stability window, down
	// phases shorter than the alarm threshold.
	for range 200 {
		for elapsed := time.Duration(0); elapsed < 20*time.Minute; elapsed += internetRecoveryPollInterval {
			if d := step(verdictUnreachable); d.action != actionNone || d.holdAlarm {
				t.Fatalf("flapping link at +%s: decision %+v, want nothing (no Apple contact, no alarm)", now.Sub(start), d)
			}
		}
		for elapsed := time.Duration(0); elapsed < 30*time.Second; elapsed += internetRecoveryPollInterval {
			if d := step(verdictReachable); d.action != actionNone {
				t.Fatalf("flapping link at +%s: action %s on a short up phase", now.Sub(start), d.action)
			}
		}
	}

	// Then dead for good: alarms at the threshold and at each interval, holds
	// throughout, never hands back.
	outageStart := now
	var alarms []time.Duration
	for now.Sub(outageStart) < 3*timing.holdAlarmAfter {
		d := step(verdictUnreachable)
		if d.action != actionNone {
			t.Fatalf("continuous outage at +%s: action %s, want the bridge held Apple-free", now.Sub(outageStart), d.action)
		}
		if d.holdAlarm {
			alarms = append(alarms, now.Sub(outageStart))
		}
	}
	if len(alarms) != 3 {
		t.Fatalf("alarms at %v, want exactly three (at the threshold, then once per interval)", alarms)
	}
	if alarms[0] != timing.holdAlarmAfter || alarms[1]-alarms[0] != timing.holdAlarmInterval {
		t.Fatalf("alarms at %v, want the first at %v and then every %v", alarms, timing.holdAlarmAfter, timing.holdAlarmInterval)
	}
}

// The alarm must fire (it was dead code when measured from the re-stamped
// timestamp) and must not spam (it is otherwise a pure function of now, so it
// would emit an Error every poll interval forever once armed).
func TestDeclineAlarmFiresOnceThenThrottles(t *testing.T) {
	retryDelay := internetRecoveryStateRetryDelay(5 * time.Minute)
	first := time.Unix(30_000, 0)
	var lastAlarm time.Time

	// Not yet due.
	if internetRecoveryDeclineAlarmDue(first.Add(2*retryDelay-time.Second), first, lastAlarm, retryDelay) {
		t.Fatal("alarm fired before 2x the retry delay")
	}
	// Due.
	due := first.Add(2 * retryDelay)
	if !internetRecoveryDeclineAlarmDue(due, first, lastAlarm, retryDelay) {
		t.Fatal("alarm never became due — this is the dead-code regression")
	}
	lastAlarm = due

	// Throttled across the whole next retryDelay at the real poll cadence.
	fires := 0
	for now := due.Add(internetRecoveryPollInterval); now.Before(due.Add(retryDelay)); now = now.Add(internetRecoveryPollInterval) {
		if internetRecoveryDeclineAlarmDue(now, first, lastAlarm, retryDelay) {
			fires++
		}
	}
	if fires != 0 {
		t.Fatalf("alarm fired %d extra times inside one retry delay; it must be throttled", fires)
	}
	// And re-arms exactly once the cadence has elapsed.
	if !internetRecoveryDeclineAlarmDue(due.Add(retryDelay), first, lastAlarm, retryDelay) {
		t.Fatal("alarm did not re-arm after the throttle window")
	}
}

func TestDeclineAlarmIgnoresAnUnstampedStreak(t *testing.T) {
	retryDelay := internetRecoveryStateRetryDelay(5 * time.Minute)
	if internetRecoveryDeclineAlarmDue(time.Unix(40_000, 0), time.Time{}, time.Time{}, retryDelay) {
		t.Fatal("alarm fired with no recorded first request")
	}
}

// scaleRecoveryTimingForTest shrinks the loop's timing knobs so the real loop can
// be driven in milliseconds, and restores them afterward.
func scaleRecoveryTimingForTest(t *testing.T) {
	t.Helper()
	poll, stable := internetRecoveryPollInterval, internetRecoveryStablePeriod
	grace, alarmAfter := internetRecoveryBlockedGrace, internetRecoveryHoldAlarmAfter
	alarmInterval := internetRecoveryHoldAlarmInterval
	probe, send, delay, notice := internetProbeFunc, sendRecoveryState, retryDelayFunc, sendRecoveryHoldNotice
	t.Cleanup(func() {
		internetRecoveryPollInterval, internetRecoveryStablePeriod = poll, stable
		internetRecoveryBlockedGrace, internetRecoveryHoldAlarmAfter = grace, alarmAfter
		internetRecoveryHoldAlarmInterval = alarmInterval
		internetProbeFunc, sendRecoveryState, retryDelayFunc, sendRecoveryHoldNotice = probe, send, delay, notice
	})
	internetRecoveryPollInterval = time.Millisecond
	internetRecoveryStablePeriod = 20 * time.Millisecond
	internetRecoveryBlockedGrace = 40 * time.Millisecond
	internetRecoveryHoldAlarmAfter = 100 * time.Millisecond
	internetRecoveryHoldAlarmInterval = 100 * time.Millisecond
	sendRecoveryHoldNotice = func(*IMClient, context.Context, zerolog.Logger, string) {}
}

func reachableResult() internetprobe.Result {
	return internetprobe.Result{Cloudflare: internetprobe.OutcomeReachable, Google: internetprobe.OutcomeReachable}
}
func unreachableResult() internetprobe.Result {
	return internetprobe.Result{Cloudflare: internetprobe.OutcomeUnreachable, Google: internetprobe.OutcomeUnreachable}
}
func blockedResult() internetprobe.Result {
	return internetprobe.Result{} // zero value is Blocked on both providers
}

// TestRecoveryLoopEndsOnlyOnAVerdictOrCancellation drives the REAL loop. Four
// audit rounds produced only pure-function tests of its predicates, and both of
// this feature's worst bugs were in where those predicates were read — an exit
// condition evaluated on one branch but not the branch the loop could persist
// on. Only a test that runs the loop can see that class.
//
// Since round 10 the loop has exactly two ways to ask bridgev2 for Apple: a
// stable, verified link (im-internet-recovered) and a probe that produced no
// verdict at all (im-internet-probe-unusable). Every confirmed-down script —
// continuous, flapping, or passing the main round but failing the preflight —
// holds Apple-free for as long as it runs, alarms, and ends only when canceled.
func TestRecoveryLoopEndsOnlyOnAVerdictOrCancellation(t *testing.T) {
	tests := []struct {
		name    string
		entry   recoveryVerdict
		verdict func(round int, phase string) internetprobe.Result
		wantErr status.BridgeStateErrorCode // "" means: must never ask
		alarms  bool
	}{
		{
			name:    "continuous outage holds Apple-free and alarms",
			entry:   verdictUnreachable,
			verdict: func(int, string) internetprobe.Result { return unreachableResult() },
			alarms:  true,
		},
		{
			name:    "unusable probe after an Apple-specific failure hands back at the blocked grace",
			entry:   verdictReachable,
			verdict: func(int, string) internetprobe.Result { return blockedResult() },
			wantErr: "im-internet-probe-unusable",
		},
		{
			// Root cause A2: the episode was entered on a confirmed outage, so
			// the most recent verdict is Unreachable and a run of Blocked
			// rounds is NOT "nothing is known to be down". It holds, and
			// alarms like any other confirmed outage.
			name:    "unusable probe after a confirmed outage is held, not handed back",
			entry:   verdictUnreachable,
			verdict: func(int, string) internetprobe.Result { return blockedResult() },
			alarms:  true,
		},
		{
			name:    "unusable probe with no verdict at entry hands back at the blocked grace",
			entry:   verdictBlocked,
			verdict: func(int, string) internetprobe.Result { return blockedResult() },
			wantErr: "im-internet-probe-unusable",
		},
		{
			// Wrong abstraction 2(a): the final preflight said down; the
			// Blocked rounds that follow must not hand back.
			name:  "a failed preflight followed by blocked rounds is held",
			entry: verdictReachable,
			verdict: func(round int, phase string) internetprobe.Result {
				if phase == "final_reconnect_preflight" {
					return unreachableResult()
				}
				if round <= 60 {
					return reachableResult()
				}
				return blockedResult()
			},
			alarms: true, // the belief is "down" and the hold is long: loud, as any confirmed outage
		},
		{
			// Wrong abstraction 2(b): one reachable sample is not a conclusion
			// that connectivity returned.
			name:  "a confirmed outage, one reachable round, then blocked rounds is held",
			entry: verdictUnreachable,
			verdict: func(round int, _ string) internetprobe.Result {
				if round == 1 {
					return reachableResult()
				}
				return blockedResult()
			},
			alarms: true,
		},
		{
			// The round-4 shape: main probe passes, final preflight fails,
			// forever. It used to end at the episode ceiling; now it holds.
			name:  "main probe passes but preflight always fails: held",
			entry: verdictUnreachable,
			verdict: func(_ int, phase string) internetprobe.Result {
				if phase == "final_reconnect_preflight" {
					return unreachableResult()
				}
				return reachableResult()
			},
		},
		{
			// The round-3 shape: up phases shorter than the stability window.
			// It used to end at the episode ceiling; now it holds, quietly.
			name:  "flapping link is held without an alarm",
			entry: verdictUnreachable,
			verdict: func(round int, _ string) internetprobe.Result {
				if (round/3)%2 == 0 {
					return reachableResult()
				}
				return unreachableResult()
			},
		},
		{
			name:    "healthy link recovers and requests a rebuild",
			entry:   verdictUnreachable,
			verdict: func(int, string) internetprobe.Result { return reachableResult() },
			wantErr: "im-internet-recovered",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scaleRecoveryTimingForTest(t)

			var mu sync.Mutex
			round := 0
			internetProbeFunc = func(_ context.Context, phase string) internetprobe.Result {
				mu.Lock()
				defer mu.Unlock()
				round++
				return tc.verdict(round, phase)
			}

			sent := make(chan status.BridgeStateErrorCode, 8)
			sendRecoveryState = func(_ *bridgev2.Bridge, _ *bridgev2.BridgeStateQueue, st status.BridgeState) bool {
				if st.StateEvent == status.StateUnknownError {
					select {
					case sent <- st.Error:
					default:
					}
				}
				return true
			}
			var noticeMu sync.Mutex
			var notices []string
			sendRecoveryHoldNotice = func(_ *IMClient, _ context.Context, _ zerolog.Logger, text string) {
				noticeMu.Lock()
				notices = append(notices, text)
				noticeMu.Unlock()
			}

			client := newRecoveryTestClient()
			stop := runRecoveryLoopForTestFrom(t, client, tc.entry)

			if tc.wantErr != "" {
				select {
				case got := <-sent:
					if got != tc.wantErr {
						t.Fatalf("request = %q, want %q", got, tc.wantErr)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("loop never requested a rebuild on a script that must produce one")
				}
				stop()
				return
			}

			// Many alarm thresholds' worth of scaled time: any request at all
			// is Apple contact on a confirmed-down verdict.
			select {
			case got := <-sent:
				t.Fatalf("loop asked bridgev2 for Apple with %q while the probe said down", got)
			case <-time.After(500 * time.Millisecond):
			}
			stop() // the episode must end cleanly on cancellation (checked by the helper)
			noticeMu.Lock()
			defer noticeMu.Unlock()
			if tc.alarms && len(notices) == 0 {
				t.Fatal("a long confirmed outage produced no management-room notice: the hold is silent, not loud")
			}
			if !tc.alarms && len(notices) != 0 {
				t.Fatalf("a flapping or preflight-failing link raised %d hold notices; the alarm must measure the current outage", len(notices))
			}
			if tc.alarms && !strings.Contains(notices[0], "will not be contacted") {
				t.Fatalf("notice %q does not tell the operator Apple is being held off", notices[0])
			}
		})
	}
}

// A Blocked round must not withdraw a pending rebuild: that changes
// prev.Timestamp, makes bridgev2 decline, and costs another of its wait cycles on
// the strength of one EACCES round. probe.go's contract says Blocked is "unknown",
// never "Internet down".
func TestBlockedRoundDoesNotWithdrawAPendingRebuild(t *testing.T) {
	scaleRecoveryTimingForTest(t)
	// Push both escape hatches out of reach so the Blocked rounds can ONLY be
	// handled by the Blocked arm. Without this the probeUnusable hatch absorbs
	// them and the test stops proving anything about that arm.
	internetRecoveryBlockedGrace = time.Hour

	var mu sync.Mutex
	round := 0
	internetProbeFunc = func(_ context.Context, _ string) internetprobe.Result {
		mu.Lock()
		defer mu.Unlock()
		round++
		// Stabilize, request a rebuild, then inject Blocked rounds forever.
		if round > 60 {
			return blockedResult()
		}
		return reachableResult()
	}

	var states []status.BridgeStateEvent
	var stateMu sync.Mutex
	sendRecoveryState = func(_ *bridgev2.Bridge, _ *bridgev2.BridgeStateQueue, st status.BridgeState) bool {
		stateMu.Lock()
		states = append(states, st.StateEvent)
		stateMu.Unlock()
		return true
	}

	client := newRecoveryTestClient()
	stop := runRecoveryLoopForTest(t, client)
	time.Sleep(400 * time.Millisecond)
	stop()

	stateMu.Lock()
	defer stateMu.Unlock()
	sawRebuild := false
	for i, st := range states {
		if st == status.StateUnknownError {
			sawRebuild = true
			continue
		}
		// The opening teardown notice is expected; a withdrawal AFTER a rebuild
		// request is the defect.
		if sawRebuild && st == status.StateTransientDisconnect {
			t.Fatalf("state %d withdrew a pending rebuild after a Blocked round: %v", i, states)
		}
	}
	if !sawRebuild {
		t.Fatalf("expected a rebuild request before the Blocked rounds, got %v", states)
	}
}

// After an unusable-probe hand-back the loop must keep working. Without
// re-arming its clocks, the hand-back arm absorbs EVERY later round: the
// stability window and final preflight never run again, the decline alarm goes
// dark, and a fully reachable round reports "no connectivity signal". So a link
// whose probe starts working again after a hand-back must still be able to
// produce a normal im-internet-recovered request.
func TestLoopStillRecoversNormallyAfterAnUnusableProbeHandBack(t *testing.T) {
	scaleRecoveryTimingForTest(t)
	retryDelayFunc = func(time.Duration) time.Duration { return 10 * time.Millisecond }

	var mu sync.Mutex
	healthy := false
	internetProbeFunc = func(_ context.Context, _ string) internetprobe.Result {
		mu.Lock()
		defer mu.Unlock()
		if healthy {
			return reachableResult()
		}
		return blockedResult()
	}

	handedBack := make(chan struct{})
	recovered := make(chan struct{})
	var once, onceR sync.Once
	sendRecoveryState = func(_ *bridgev2.Bridge, _ *bridgev2.BridgeStateQueue, st status.BridgeState) bool {
		switch st.Error {
		case "im-internet-probe-unusable":
			once.Do(func() { close(handedBack) })
		case "im-internet-recovered":
			onceR.Do(func() { close(recovered) })
		}
		return true
	}

	client := newRecoveryTestClient()
	stop := runRecoveryLoopForTestFrom(t, client, verdictReachable)

	select {
	case <-handedBack:
	case <-time.After(10 * time.Second):
		t.Fatal("never reached the unusable-probe hand-back")
	}
	// bridgev2 declined (the loop is still alive), and now the probe works and
	// the link is up.
	mu.Lock()
	healthy = true
	mu.Unlock()

	select {
	case <-recovered:
	case <-time.After(10 * time.Second):
		t.Fatal("after a hand-back the loop never recovered normally — the hand-back arm has latched and the stability/preflight path is unreachable")
	}
	stop()
}

// Root cause A1 (round 11). An unusable-probe hand-back rests on "nothing is
// known to be down". When a confirmed Unreachable verdict arrives while it is
// pending, that premise is gone and bridgev2 must NOT act on it 4-6 minutes
// later — so the loop withdraws it (StateTransientDisconnect changes
// prev.Timestamp and bridgev2's pending unknownErrorReconnect declines). The
// round-6 concern that a hand-back could be self-canceled one poll later does
// not apply: the precondition keeps a hand-back from being sent while the
// most recent verdict is Unreachable, so a withdrawal here is always on
// genuinely new negative evidence.
func TestUnusableProbeHandBackIsWithdrawnByALaterOutageVerdict(t *testing.T) {
	scaleRecoveryTimingForTest(t)

	var mu sync.Mutex
	sawHandBack := false
	internetProbeFunc = func(context.Context, string) internetprobe.Result {
		mu.Lock()
		defer mu.Unlock()
		if sawHandBack {
			return unreachableResult()
		}
		return blockedResult()
	}

	handedBack := make(chan struct{})
	withdrawn := make(chan struct{})
	rebuiltAgain := make(chan status.BridgeStateErrorCode, 1)
	var onceH, onceW sync.Once
	sendRecoveryState = func(_ *bridgev2.Bridge, _ *bridgev2.BridgeStateQueue, st status.BridgeState) bool {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case st.Error == "im-internet-probe-unusable" && !sawHandBack:
			sawHandBack = true
			onceH.Do(func() { close(handedBack) })
		case sawHandBack && st.StateEvent == status.StateTransientDisconnect:
			onceW.Do(func() { close(withdrawn) })
		case sawHandBack && st.StateEvent == status.StateUnknownError:
			select {
			case rebuiltAgain <- st.Error:
			default:
			}
		}
		return true
	}

	client := newRecoveryTestClient()
	stop := runRecoveryLoopForTestFrom(t, client, verdictReachable)

	select {
	case <-handedBack:
	case <-time.After(10 * time.Second):
		t.Fatal("never reached the unusable-probe hand-back")
	}
	select {
	case <-withdrawn:
	case <-time.After(10 * time.Second):
		t.Fatal("a confirmed outage arrived while an unusable-probe hand-back was pending and the loop did not withdraw it: bridgev2 will reconnect to Apple into the outage")
	}
	// And nothing may re-ask while the verdict stays Unreachable.
	select {
	case code := <-rebuiltAgain:
		t.Fatalf("after the withdrawal the loop asked bridgev2 for Apple again with %q on a confirmed outage", code)
	case <-time.After(300 * time.Millisecond):
	}
	stop()
}

func TestHandBackDelayBacksOffAndCaps(t *testing.T) {
	if got := handBackDelay(0); got != recoveryHandBackBaseDelay {
		t.Errorf("handBackDelay(0) = %s, want the base delay", got)
	}
	if got := handBackDelay(1); got != recoveryHandBackBaseDelay {
		t.Errorf("handBackDelay(1) = %s, want the base delay", got)
	}
	prev := handBackDelay(1)
	for n := 2; n <= 12; n++ {
		got := handBackDelay(n)
		if got < prev {
			t.Fatalf("handBackDelay(%d) = %s went backwards from %s", n, got, prev)
		}
		if got > recoveryHandBackMaxDelay {
			t.Fatalf("handBackDelay(%d) = %s exceeds the cap %s", n, got, recoveryHandBackMaxDelay)
		}
		prev = got
	}
	if handBackDelay(99) != recoveryHandBackMaxDelay {
		t.Error("a long run must saturate at the cap, never overflow")
	}
}

// The backoff must survive the client rebuild it asks for — that is the whole
// point of keeping it on IMConnector rather than IMClient.
func TestHandBackBackoffSurvivesClientRebuild(t *testing.T) {
	main := &IMConnector{}
	const login = networkid.UserLoginID("login-1")
	start := time.Unix(50_000, 0)

	if wait, ok := main.handBackDue(login, start); !ok {
		t.Fatalf("first hand-back should be allowed immediately, wait=%s", wait)
	}
	main.noteHandBack(login, start)

	// Immediately after, it must hold.
	if _, ok := main.handBackDue(login, start.Add(time.Minute)); ok {
		t.Fatal("a second hand-back one minute later must be held off")
	}
	// After the first interval, allowed again — and the interval then widens.
	next := start.Add(handBackDelay(1))
	if _, ok := main.handBackDue(login, next); !ok {
		t.Fatal("hand-back should be allowed once the interval has elapsed")
	}
	main.noteHandBack(login, next)
	if _, ok := main.handBackDue(login, next.Add(handBackDelay(1))); ok {
		t.Fatal("the second interval must be longer than the first")
	}
	if _, ok := main.handBackDue(login, next.Add(handBackDelay(2))); !ok {
		t.Fatal("hand-back should be allowed once the widened interval has elapsed")
	}

	// Only a real connection clears the run.
	main.clearHandBacks(login)
	if _, ok := main.handBackDue(login, next); !ok {
		t.Fatal("clearHandBacks must reset the run")
	}
}

func TestFlapStormClassifierCatchesBurstAndSustained(t *testing.T) {
	now := time.Unix(60_000, 0)

	// Literal counts and spacings, NOT the constants: a fixture built from the
	// threshold moves with it, and the policy these numbers encode (5 in 60s;
	// 12 in 10m, i.e. 72/hour) is what this test exists to pin.
	var burst []time.Time
	for i := range 5 {
		burst = append(burst, now.Add(-time.Duration(i)*time.Second))
	}
	if isBurst, _, _ := classifyFlapStorm(burst, now); !isBurst {
		t.Error("5 interruptions inside 60 seconds must be detected as a burst")
	}

	// The band a short window alone misses: 45-second spacing never reaches 5
	// in 60s but sustains 80/hour.
	var sustained []time.Time
	for i := range 12 {
		sustained = append(sustained, now.Add(-time.Duration(i)*45*time.Second))
	}
	isBurst, isSustained, _ := classifyFlapStorm(sustained, now)
	if isBurst {
		t.Error("45-second spacing should not trip the burst window")
	}
	if !isSustained {
		t.Error("12 interruptions inside 10 minutes must be detected as sustained")
	}
	if _, isSustained, _ := classifyFlapStorm(sustained[:11], now); isSustained {
		t.Error("11 interruptions inside 10 minutes is below the sustained threshold")
	}

	// A healthy link with the odd interruption trips neither.
	few := []time.Time{now.Add(-9 * time.Minute), now.Add(-4 * time.Minute), now}
	if b, sus, _ := classifyFlapStorm(few, now); b || sus {
		t.Error("occasional interruptions must not be treated as a storm")
	}
}

// The sustained clause must count only interruptions inside its own window.
// recordInterruption happens to prune to that window, but the classifier must
// not depend on its caller's hygiene: a history that was pruned less often, or
// passed in unpruned, would otherwise count hours-old samples as a storm.
func TestFlapStormSustainedClauseIsWindowed(t *testing.T) {
	now := time.Unix(61_000, 0)
	var stale []time.Time
	for i := range 40 {
		stale = append(stale, now.Add(-11*time.Minute-time.Duration(i)*time.Minute))
	}
	if b, sus, _ := classifyFlapStorm(stale, now); b || sus {
		t.Fatal("40 interruptions all older than the sustained window must not be a storm")
	}
	// The same history plus 12 in-window samples is a storm again.
	recent := append([]time.Time(nil), stale...)
	for i := range 12 {
		recent = append(recent, now.Add(-time.Duration(i)*45*time.Second))
	}
	if _, sus, _ := classifyFlapStorm(recent, now); !sus {
		t.Fatal("12 in-window interruptions must be a storm regardless of stale history")
	}
}

func TestRecordInterruptionPrunesToTheLongestWindow(t *testing.T) {
	now := time.Unix(70_000, 0)
	history := []time.Time{
		now.Add(-2 * internetFlapSustainedWindow), // stale
		now.Add(-internetFlapSustainedWindow),     // exactly at the cutoff: dropped
		now.Add(-time.Minute),                     // kept
	}
	got := recordInterruption(history, now)
	if len(got) != 2 {
		t.Fatalf("history = %v, want the in-window entry plus now", got)
	}
	if !got[len(got)-1].Equal(now) {
		t.Error("the new sample must be appended last")
	}
}

// The hand-back backoff must actually hold inside the loop, not just compute a
// duration. With the episode-local retry gate opened wide, only the
// connector-level spacing stands between one hand-back and a storm of them.
func TestLoopHoldsRepeatHandBacksBehindTheBackoff(t *testing.T) {
	scaleRecoveryTimingForTest(t)
	// Open the episode-local gate so the connector-level backoff is the only
	// thing limiting repeats. handBackDelay(1) is minutes, far longer than this
	// test runs, so a correct implementation permits exactly one.
	retryDelayFunc = func(time.Duration) time.Duration { return time.Millisecond }

	internetProbeFunc = func(context.Context, string) internetprobe.Result {
		return blockedResult()
	}

	var mu sync.Mutex
	handBacks := 0
	first := make(chan struct{})
	var once sync.Once
	sendRecoveryState = func(_ *bridgev2.Bridge, _ *bridgev2.BridgeStateQueue, st status.BridgeState) bool {
		if st.Error == "im-internet-probe-unusable" {
			mu.Lock()
			handBacks++
			mu.Unlock()
			once.Do(func() { close(first) })
		}
		return true
	}

	client := newRecoveryTestClient()
	stop := runRecoveryLoopForTestFrom(t, client, verdictReachable)

	select {
	case <-first:
	case <-time.After(10 * time.Second):
		t.Fatal("never produced an initial hand-back")
	}
	// Give the loop many poll intervals to misbehave.
	time.Sleep(300 * time.Millisecond)
	stop()

	mu.Lock()
	defer mu.Unlock()
	if handBacks != 1 {
		t.Fatalf("hand-backs = %d, want exactly 1 — consecutive hand-backs must be held behind the connector-level backoff", handBacks)
	}
}

// The decision table, exhaustively. Every clause of recoveryEpisode.step and
// finishPreflight has at least one case that cites it, and every case that
// corresponds to one of this feature's historical defects says which. Timing is
// production-valued and explicit, so nothing here depends on the package vars.
func TestRecoveryStepDecisionTable(t *testing.T) {
	now := time.Unix(100_000, 0)
	timing := recoveryTiming{
		stablePeriod:      60 * time.Second,
		blockedGrace:      2 * time.Minute,
		holdAlarmAfter:    30 * time.Minute,
		holdAlarmInterval: 30 * time.Minute,
	}
	const retryDelay = 7 * time.Minute
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	// Episode builders. Each starts from a fresh episode and sets only what the
	// case is about, so the zero values are the documented defaults.
	// fresh models an episode entered on an Apple-specific failure: the last
	// verdict was Reachable a minute ago. afterOutage models the common entry,
	// a confirmed outage.
	fresh := func() *recoveryEpisode { return newRecoveryEpisode(ago(time.Minute), verdictReachable) }
	afterOutage := func(d time.Duration) *recoveryEpisode { return newRecoveryEpisode(ago(d), verdictUnreachable) }
	stabilizingFor := func(e *recoveryEpisode, d time.Duration) *recoveryEpisode {
		e.stabilizing = true
		e.stable.Observe(ago(d), true, timing.stablePeriod)
		return e
	}
	pendingSince := func(e *recoveryEpisode, kind recoveryPending, d time.Duration) *recoveryEpisode {
		e.pending, e.requestedAt, e.firstRequestedAt, e.attempt = kind, ago(d), ago(d), 1
		return e
	}

	type stepCase struct {
		name    string
		episode func() *recoveryEpisode
		verdict recoveryVerdict
		hold    time.Duration
		want    recoveryDecision
		pending recoveryPending
		// Optional extra assertion on the resulting episode.
		after func(t *testing.T, e *recoveryEpisode)
	}
	cases := []stepCase{
		{
			name:    "clause 2: first reachable round opens the stability window",
			episode: fresh,
			verdict: verdictReachable,
			want:    recoveryDecision{stabilityStarted: true},
		},
		{
			name:    "clause 6: a full stability window earns the preflight",
			episode: func() *recoveryEpisode { return stabilizingFor(fresh(), timing.stablePeriod) },
			verdict: verdictReachable,
			want:    recoveryDecision{action: actionPreflight},
		},
		{
			name:    "clause 2: unreachable during stabilization resets the window",
			episode: func() *recoveryEpisode { return stabilizingFor(fresh(), 30*time.Second) },
			verdict: verdictUnreachable,
			want:    recoveryDecision{stabilityReset: true},
		},
		{
			name:    "clause 2: blocked during stabilization resets the window",
			episode: func() *recoveryEpisode { return stabilizingFor(fresh(), 30*time.Second) },
			verdict: verdictBlocked,
			want:    recoveryDecision{stabilityReset: true},
		},
		{
			// Round-8 survivor: without the reset, the hold alarm measures
			// from the EPISODE start regardless of recovery and cries wolf on
			// a flapping link.
			name: "clause 1: a reachable round ends the current outage, so a stale outage clock cannot raise the hold alarm",
			episode: func() *recoveryEpisode {
				e := stabilizingFor(fresh(), 30*time.Second)
				e.outageStartedAt = ago(timing.holdAlarmAfter)
				return e
			},
			verdict: verdictReachable,
			want:    recoveryDecision{},
			after: func(t *testing.T, e *recoveryEpisode) {
				if !e.outageStartedAt.Equal(now) {
					t.Errorf("outageStartedAt = %v, want %v (this round)", e.outageStartedAt, now)
				}
			},
		},
		{
			// Round-8 survivor: shortening the window is the Apple-unsafe
			// direction and was uncovered; lengthening it already failed
			// "a full stability window earns the preflight".
			name:    "clause 2: one second short of the window is not stable",
			episode: func() *recoveryEpisode { return stabilizingFor(fresh(), timing.stablePeriod-time.Second) },
			verdict: verdictReachable,
			want:    recoveryDecision{},
		},
		{
			// Mutation 11 target.
			name:    "clause 3: unreachable withdraws a pending recovered request",
			episode: func() *recoveryEpisode { return pendingSince(fresh(), pendingRecovered, time.Minute) },
			verdict: verdictUnreachable,
			want:    recoveryDecision{action: actionWithdraw},
			pending: pendingNone,
			after: func(t *testing.T, e *recoveryEpisode) {
				// Round-8 survivor: a withdrawal we made ourselves must not
				// leave the decline alarm measuring from the withdrawn request,
				// or a later streak emits a spurious "may need a bridge
				// restart" Error.
				if !e.firstRequestedAt.IsZero() {
					t.Errorf("firstRequestedAt = %v after a withdrawal, want zero", e.firstRequestedAt)
				}
				if !e.requestedAt.IsZero() {
					t.Errorf("requestedAt = %v after a withdrawal, want zero", e.requestedAt)
				}
			},
		},
		{
			name: "clause 3: withdrawal outranks a due hold alarm on the same round",
			episode: func() *recoveryEpisode {
				e := pendingSince(fresh(), pendingRecovered, 10*time.Minute)
				e.outageStartedAt = ago(timing.holdAlarmAfter)
				return e
			},
			verdict: verdictUnreachable,
			want:    recoveryDecision{action: actionWithdraw},
			pending: pendingNone,
		},
		{
			// probe.go's contract: Blocked is "unknown", never "Internet down".
			name:    "clause 3: blocked does not withdraw a pending recovered request",
			episode: func() *recoveryEpisode { return pendingSince(fresh(), pendingRecovered, time.Minute) },
			verdict: verdictBlocked,
			want:    recoveryDecision{},
			pending: pendingRecovered,
		},
		{
			// Root cause A1: a hand-back's premise is "nothing is known to be
			// down"; a confirmed outage destroys it, so the hand-back is
			// withdrawn like a recovered request would be.
			name:    "clause 3: unreachable withdraws a pending hand-back too — its premise is gone",
			episode: func() *recoveryEpisode { return pendingSince(fresh(), pendingHandBack, time.Minute) },
			verdict: verdictUnreachable,
			want:    recoveryDecision{action: actionWithdraw},
			pending: pendingNone,
		},
		{
			name:    "clause 3: blocked does not withdraw a pending hand-back",
			episode: func() *recoveryEpisode { return pendingSince(fresh(), pendingHandBack, time.Minute) },
			verdict: verdictBlocked,
			want:    recoveryDecision{},
			pending: pendingHandBack,
		},
		{
			name: "clause 4: an old unanswered request on a reachable link raises the decline alarm",
			episode: func() *recoveryEpisode {
				e := pendingSince(stabilizingFor(fresh(), timing.stablePeriod), pendingRecovered, retryDelay)
				e.firstRequestedAt = ago(2 * retryDelay)
				return e
			},
			verdict: verdictReachable,
			want:    recoveryDecision{action: actionPreflight, declineAlarm: true},
			pending: pendingRecovered,
		},
		{
			name: "clause 4: the decline alarm is throttled to the re-ask cadence",
			episode: func() *recoveryEpisode {
				e := pendingSince(stabilizingFor(fresh(), timing.stablePeriod), pendingRecovered, retryDelay)
				e.firstRequestedAt = ago(2 * retryDelay)
				e.lastDeclineAlarmAt = ago(time.Minute)
				return e
			},
			verdict: verdictReachable,
			want:    recoveryDecision{action: actionPreflight},
			pending: pendingRecovered,
		},
		{
			// THE round-7 finding. The re-ask gate is shut and the link is
			// healthy. The old arms routed this into the hand-back arm forever;
			// the gate now comes first, so the window keeps accumulating and
			// nothing is requested until the gate opens.
			name: "clause 5: a recent request keeps the gate shut on a healthy link",
			episode: func() *recoveryEpisode {
				e := pendingSince(stabilizingFor(fresh(), timing.stablePeriod), pendingHandBack, time.Minute)
				e.startedAt = ago(3 * time.Hour)
				return e
			},
			verdict: verdictReachable,
			want:    recoveryDecision{},
			pending: pendingHandBack,
			after: func(t *testing.T, e *recoveryEpisode) {
				if !e.stabilizing {
					t.Error("the stability window must survive a gated round")
				}
			},
		},
		{
			// Root cause A1 again, from the gate's side: the withdrawal is
			// decided in clause 3, before the re-ask gate is consulted, so a
			// recent hand-back does not shield itself from new negative
			// evidence.
			name: "clause 3: a recent hand-back is still withdrawn by an unreachable verdict",
			episode: func() *recoveryEpisode {
				e := pendingSince(fresh(), pendingHandBack, time.Minute)
				e.outageStartedAt = ago(timing.holdAlarmAfter + time.Minute)
				return e
			},
			verdict: verdictUnreachable,
			want:    recoveryDecision{action: actionWithdraw},
			pending: pendingNone,
		},
		{
			// Mutation-1 territory: the connector backoff is holding. The
			// normal path must still win on a stable link, or the backoff
			// starves recovery.
			name: "clause 6: the normal exit wins even while the backoff holds",
			episode: func() *recoveryEpisode {
				e := pendingSince(stabilizingFor(fresh(), timing.stablePeriod), pendingHandBack, retryDelay)
				e.startedAt = ago(3 * time.Hour)
				return e
			},
			verdict: verdictReachable,
			hold:    45 * time.Minute,
			want:    recoveryDecision{action: actionPreflight},
			pending: pendingHandBack,
		},
		{
			// Round-10 B2: the round-4 shape (main probe passes, preflight
			// always fails) used to end at the episode ceiling. A reachable
			// round that is not yet stable now holds, however old the episode.
			name: "clause 7: a reachable round that never stabilizes is held, not handed back",
			episode: func() *recoveryEpisode {
				e := stabilizingFor(fresh(), 10*time.Second)
				e.startedAt = ago(3 * time.Hour)
				return e
			},
			verdict: verdictReachable,
			want:    recoveryDecision{},
		},
		{
			name: "clause 7: an unusable probe hands back after the blocked grace",
			episode: func() *recoveryEpisode {
				e := fresh()
				e.lastVerdictAt = ago(timing.blockedGrace)
				return e
			},
			verdict: verdictBlocked,
			want:    recoveryDecision{action: actionHandBack, handBackCode: handBackCodeProbeUnusable},
			pending: pendingHandBack,
		},
		{
			// Root cause A2: the precondition. The probe has said nothing for
			// the whole grace, but the last thing it DID say was Unreachable.
			name: "clause 7: the hatch is ineligible while the episode believes the network is down",
			episode: func() *recoveryEpisode {
				e := fresh()
				e.outageConfirmed, e.lastVerdictAt = true, ago(timing.blockedGrace)
				return e
			},
			verdict: verdictBlocked,
			want:    recoveryDecision{},
		},
		{
			name:    "clause 7: an episode entered on a confirmed outage cannot hand back on blocked rounds",
			episode: func() *recoveryEpisode { return afterOutage(timing.blockedGrace) },
			verdict: verdictBlocked,
			want:    recoveryDecision{},
		},
		{
			name:    "clause 7: an episode entered with no verdict at all can hand back after the grace",
			episode: func() *recoveryEpisode { return newRecoveryEpisode(ago(timing.blockedGrace), verdictBlocked) },
			verdict: verdictBlocked,
			want:    recoveryDecision{action: actionHandBack, handBackCode: handBackCodeProbeUnusable},
			pending: pendingHandBack,
		},
		{
			name:    "clause 7: blocked rounds after a confirmed outage are held and still alarm",
			episode: func() *recoveryEpisode { return afterOutage(timing.holdAlarmAfter) },
			verdict: verdictBlocked,
			want:    recoveryDecision{holdAlarm: true},
		},
		{
			// Wrong abstraction 2(b): one Reachable SAMPLE after a confirmed
			// outage is not a conclusion that connectivity returned.
			name: "clause 7: a single reachable sample after an outage does not re-arm the hatch",
			episode: func() *recoveryEpisode {
				e := afterOutage(time.Hour)
				e.step(recoveryRound{now: ago(timing.blockedGrace), verdict: verdictReachable, retryDelay: retryDelay, timing: timing})
				return e
			},
			verdict: verdictBlocked,
			// The one sample opened a stability window, which the Blocked round
			// resets (clause 2); it decides nothing — no hand-back, no alarm.
			want: recoveryDecision{stabilityReset: true},
		},
		{
			// Only the evidence that authorizes a rebuild — a passed preflight
			// after a full window — clears the belief and re-arms the hatch.
			name: "clause 7: a rebuild-grade recovery re-arms the hatch after an outage",
			episode: func() *recoveryEpisode {
				e := afterOutage(time.Hour)
				e.finishPreflight(ago(timing.blockedGrace), verdictReachable)
				e.requestedAt = ago(retryDelay) // the gate has since opened
				return e
			},
			verdict: verdictBlocked,
			want:    recoveryDecision{action: actionHandBack, handBackCode: handBackCodeProbeUnusable},
			pending: pendingHandBack,
		},
		{
			// Wrong abstraction 2(a): a failed final preflight is an Unreachable
			// conclusion like any other and must set the belief.
			name: "clause 7: blocked rounds after a failed preflight cannot hand back",
			episode: func() *recoveryEpisode {
				e := stabilizingFor(fresh(), timing.stablePeriod)
				e.finishPreflight(ago(timing.blockedGrace), verdictUnreachable)
				return e
			},
			verdict: verdictBlocked,
			want:    recoveryDecision{},
		},
		{
			name: "clause 7: an unusable probe inside the grace does nothing",
			episode: func() *recoveryEpisode {
				e := fresh()
				e.lastVerdictAt = ago(timing.blockedGrace - time.Second)
				return e
			},
			verdict: verdictBlocked,
			want:    recoveryDecision{},
		},
		{
			// Round-10 B2, the load-bearing case: a confirmed outage NEVER
			// hands back. At the alarm threshold it alarms and keeps holding.
			name: "clause 7: a continuous outage is held Apple-free and alarms at the threshold",
			episode: func() *recoveryEpisode {
				e := fresh()
				e.outageStartedAt = ago(timing.holdAlarmAfter)
				return e
			},
			verdict: verdictUnreachable,
			want:    recoveryDecision{holdAlarm: true},
			after: func(t *testing.T, e *recoveryEpisode) {
				if !e.lastHoldAlarmAt.Equal(now) {
					t.Error("the alarm must stamp its throttle clock")
				}
			},
		},
		{
			name: "clause 7: a continuous outage below the threshold is held silently",
			episode: func() *recoveryEpisode {
				e := fresh()
				e.outageStartedAt = ago(timing.holdAlarmAfter - time.Second)
				e.startedAt = ago(3 * time.Hour)
				return e
			},
			verdict: verdictUnreachable,
			want:    recoveryDecision{},
		},
		{
			name: "clause 7: the hold alarm is throttled to its interval",
			episode: func() *recoveryEpisode {
				e := fresh()
				e.outageStartedAt = ago(2 * timing.holdAlarmAfter)
				e.lastHoldAlarmAt = ago(timing.holdAlarmInterval - time.Second)
				return e
			},
			verdict: verdictUnreachable,
			want:    recoveryDecision{},
		},
		{
			name: "clause 7: the hold alarm fires again once its interval elapses",
			episode: func() *recoveryEpisode {
				e := fresh()
				e.outageStartedAt = ago(2 * timing.holdAlarmAfter)
				e.lastHoldAlarmAt = ago(timing.holdAlarmInterval)
				return e
			},
			verdict: verdictUnreachable,
			want:    recoveryDecision{holdAlarm: true},
		},
		{
			// The round-3 shape: a flapping link resets the outage clock forever.
			// It used to be ended by the episode clock; now it is simply held,
			// and quietly — the alarm measures the current outage.
			name: "clause 7: a flapping link with an old episode is held without an alarm",
			episode: func() *recoveryEpisode {
				e := fresh()
				e.outageStartedAt = ago(time.Minute)
				e.startedAt = ago(3 * time.Hour)
				return e
			},
			verdict: verdictUnreachable,
			want:    recoveryDecision{},
		},
		{
			name: "clause 7: a blocked round inside the grace neither alarms nor hands back",
			episode: func() *recoveryEpisode {
				e := fresh()
				e.outageStartedAt = ago(2 * timing.holdAlarmAfter)
				e.lastVerdictAt = ago(time.Second)
				return e
			},
			verdict: verdictBlocked,
			want:    recoveryDecision{},
		},
		{
			name: "clause 7: a re-ask after the gate opens is a hand-back again while the probe is still unusable",
			episode: func() *recoveryEpisode {
				e := pendingSince(fresh(), pendingHandBack, retryDelay)
				e.lastVerdictAt = ago(timing.blockedGrace + retryDelay)
				return e
			},
			verdict: verdictBlocked,
			want:    recoveryDecision{action: actionHandBack, handBackCode: handBackCodeProbeUnusable},
			pending: pendingHandBack,
			after: func(t *testing.T, e *recoveryEpisode) {
				if e.attempt != 2 {
					t.Errorf("attempt = %d, want 2", e.attempt)
				}
				if !e.firstRequestedAt.Equal(ago(retryDelay)) {
					t.Error("a re-ask must not re-stamp the streak's first request")
				}
			},
		},
		{
			// Finding 8: the hold used to log at Info every poll interval.
			name: "clause 7: a hand-back re-arms the hold log so the next hold logs again",
			episode: func() *recoveryEpisode {
				e := fresh()
				e.lastVerdictAt = ago(timing.blockedGrace)
				e.lastHoldLogAt = ago(time.Minute)
				return e
			},
			verdict: verdictBlocked,
			want:    recoveryDecision{action: actionHandBack, handBackCode: handBackCodeProbeUnusable},
			pending: pendingHandBack,
			after: func(t *testing.T, e *recoveryEpisode) {
				// Logging-only survivor: without the clear, the first hold of
				// the NEXT streak is throttled by a stamp from the previous one.
				if !e.lastHoldLogAt.IsZero() {
					t.Errorf("lastHoldLogAt = %v after a hand-back, want zero", e.lastHoldLogAt)
				}
			},
		},
		{
			name: "clause 7: the backoff hold is logged on its first round",
			episode: func() *recoveryEpisode {
				e := pendingSince(fresh(), pendingHandBack, retryDelay)
				e.lastVerdictAt = ago(timing.blockedGrace + retryDelay)
				return e
			},
			verdict: verdictBlocked,
			hold:    5 * time.Minute,
			want:    recoveryDecision{holdLog: true},
			pending: pendingHandBack,
			after: func(t *testing.T, e *recoveryEpisode) {
				if e.attempt != 1 {
					t.Error("a held hand-back must not count as a request")
				}
				if e.startedAt.Equal(now) {
					t.Error("a held hand-back must not re-arm the episode clock")
				}
			},
		},
		{
			name: "clause 7: the backoff hold is silent inside the throttle interval",
			episode: func() *recoveryEpisode {
				e := pendingSince(fresh(), pendingHandBack, retryDelay)
				e.lastVerdictAt = ago(timing.blockedGrace + retryDelay)
				e.lastHoldLogAt = ago(retryDelay - time.Second)
				return e
			},
			verdict: verdictBlocked,
			hold:    5 * time.Minute,
			want:    recoveryDecision{},
			pending: pendingHandBack,
		},
		{
			name: "clause 7: the backoff hold logs again once the throttle interval elapses",
			episode: func() *recoveryEpisode {
				e := pendingSince(fresh(), pendingHandBack, retryDelay)
				e.lastVerdictAt = ago(timing.blockedGrace + retryDelay)
				e.lastHoldLogAt = ago(retryDelay)
				return e
			},
			verdict: verdictBlocked,
			hold:    5 * time.Minute,
			want:    recoveryDecision{holdLog: true},
			pending: pendingHandBack,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.episode()
			got := e.step(recoveryRound{now: now, verdict: tc.verdict, retryDelay: retryDelay, handBackHold: tc.hold, timing: timing})
			if got != tc.want {
				t.Fatalf("decision = %+v, want %+v", got, tc.want)
			}
			if e.pending != tc.pending {
				t.Fatalf("pending = %s, want %s", e.pending, tc.pending)
			}
			if tc.after != nil {
				tc.after(t, e)
			}
		})
	}

	t.Run("finishPreflight", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			episode func() *recoveryEpisode
			verdict recoveryVerdict
			want    recoveryDecision
			pending recoveryPending
			after   func(t *testing.T, e *recoveryEpisode)
		}{
			{
				name:    "a passing preflight requests a rebuild",
				episode: func() *recoveryEpisode { return stabilizingFor(fresh(), timing.stablePeriod) },
				verdict: verdictReachable,
				want:    recoveryDecision{action: actionRecovered},
				pending: pendingRecovered,
				after: func(t *testing.T, e *recoveryEpisode) {
					if e.outageConfirmed || !e.lastVerdictAt.Equal(now) {
						t.Errorf("a passing preflight must stamp the conclusion and clear the belief: confirmed=%v at=%v", e.outageConfirmed, e.lastVerdictAt)
					}
				},
			},
			{
				name: "a passing preflight after a confirmed outage clears the belief",
				episode: func() *recoveryEpisode {
					return stabilizingFor(afterOutage(time.Hour), timing.stablePeriod)
				},
				verdict: verdictReachable,
				want:    recoveryDecision{action: actionRecovered},
				pending: pendingRecovered,
				after: func(t *testing.T, e *recoveryEpisode) {
					if e.outageConfirmed {
						t.Error("the rebuild-grade evidence must clear the outage belief")
					}
				},
			},
			{
				name: "a passing preflight after a hand-back re-asks with the truthful kind",
				episode: func() *recoveryEpisode {
					return pendingSince(stabilizingFor(fresh(), timing.stablePeriod), pendingHandBack, retryDelay)
				},
				verdict: verdictReachable,
				want:    recoveryDecision{action: actionRecovered},
				pending: pendingRecovered,
			},
			{
				name:    "a failing preflight resets the window and sets the outage belief",
				episode: func() *recoveryEpisode { return stabilizingFor(fresh(), timing.stablePeriod) },
				verdict: verdictUnreachable,
				want:    recoveryDecision{stabilityReset: true},
				after: func(t *testing.T, e *recoveryEpisode) {
					// Logging-only survivor: the reset must also end the window
					// state, or the next reachable round never logs a new start.
					if e.stabilizing {
						t.Error("a reset preflight left stabilizing set")
					}
					// Wrong abstraction 2(a): a failed preflight is a conclusion.
					if !e.outageConfirmed || !e.lastVerdictAt.Equal(now) {
						t.Errorf("a failed preflight must set the belief and stamp the conclusion: confirmed=%v at=%v", e.outageConfirmed, e.lastVerdictAt)
					}
				},
			},
			{
				name: "a failing preflight withdraws a pending recovered request",
				episode: func() *recoveryEpisode {
					return pendingSince(stabilizingFor(fresh(), timing.stablePeriod), pendingRecovered, retryDelay)
				},
				verdict: verdictUnreachable,
				want:    recoveryDecision{action: actionWithdraw, stabilityReset: true},
				pending: pendingNone,
			},
			{
				// Same premise rule as step's clause 3: a preflight that finds
				// the network down destroys "nothing is known to be down".
				name: "a failing preflight withdraws a pending hand-back too",
				episode: func() *recoveryEpisode {
					return pendingSince(stabilizingFor(fresh(), timing.stablePeriod), pendingHandBack, retryDelay)
				},
				verdict: verdictUnreachable,
				want:    recoveryDecision{action: actionWithdraw, stabilityReset: true},
				pending: pendingNone,
			},
			{
				name: "a blocked preflight leaves a pending hand-back standing",
				episode: func() *recoveryEpisode {
					return pendingSince(stabilizingFor(fresh(), timing.stablePeriod), pendingHandBack, retryDelay)
				},
				verdict: verdictBlocked,
				want:    recoveryDecision{stabilityReset: true},
				pending: pendingHandBack,
			},
			{
				// Round-8 finding 2. A preflight that could not run is the
				// absence of evidence, exactly as in clause 3 of step: the
				// window resets, the pending request stands.
				name: "a blocked preflight resets the window but does not withdraw a pending recovered request",
				episode: func() *recoveryEpisode {
					return pendingSince(stabilizingFor(fresh(), timing.stablePeriod), pendingRecovered, retryDelay)
				},
				verdict: verdictBlocked,
				want:    recoveryDecision{stabilityReset: true},
				pending: pendingRecovered,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				e := tc.episode()
				got := e.finishPreflight(now, tc.verdict)
				if got != tc.want {
					t.Fatalf("decision = %+v, want %+v", got, tc.want)
				}
				if e.pending != tc.pending {
					t.Fatalf("pending = %s, want %s", e.pending, tc.pending)
				}
				if tc.after != nil {
					tc.after(t, e)
				}
			})
		}
	})
}

// TestRecoveryIsIndependentOfTimingOrder drives the pure transition function
// through a simulated outage-then-recovery on every ordering of the timing
// knobs an operator can move against each other: the re-ask gate
// (unknown_error_auto_reconnect, floor only) and the connector backoff. The
// round-7 latch needed one specific ordering; this test asserts that NO
// ordering can keep a healthy link from producing im-internet-recovered within
// one gate of the link coming back — and, since round 10, that no ordering
// makes a confirmed outage contact Apple, however long it runs. It runs in
// simulated time, so the real production values are used unscaled.
func TestRecoveryIsIndependentOfTimingOrder(t *testing.T) {
	timing := recoveryTiming{
		stablePeriod:      60 * time.Second,
		blockedGrace:      2 * time.Minute,
		holdAlarmAfter:    30 * time.Minute,
		holdAlarmInterval: 30 * time.Minute,
	}
	const poll = 10 * time.Second
	for _, retryDelay := range []time.Duration{
		internetRecoveryStateRetryDelay(5 * time.Minute),  // the forced floor
		internetRecoveryStateRetryDelay(60 * time.Minute), // unknown_error_auto_reconnect: 60m
	} {
		for _, hold := range []time.Duration{0, recoveryHandBackMaxDelay, 4 * time.Hour} {
			name := "retry=" + retryDelay.String() + "/hold=" + hold.String()
			t.Run(name, func(t *testing.T) {
				start := time.Unix(200_000, 0)
				e := newRecoveryEpisode(start, verdictUnreachable)
				now := start
				outageEnd := start.Add(3 * time.Hour)
				var recoveredAt time.Time
				alarms := 0
				deadline := outageEnd.Add(timing.stablePeriod + retryDelay + 2*poll)
				for now.Before(deadline) {
					now = now.Add(poll)
					verdict := verdictUnreachable
					if !now.Before(outageEnd) {
						verdict = verdictReachable
					}
					d := e.step(recoveryRound{now: now, verdict: verdict, retryDelay: retryDelay, handBackHold: hold, timing: timing})
					switch d.action {
					case actionHandBack:
						t.Fatalf("hand-back requested at +%s on a %s verdict: a confirmed outage must never contact Apple", now.Sub(start), verdict)
					case actionWithdraw:
						t.Fatalf("withdrawal at +%s with nothing to withdraw", now.Sub(start))
					case actionPreflight:
						if after := e.finishPreflight(now, verdictReachable); after.action == actionRecovered {
							recoveredAt = now
						}
					}
					if d.holdAlarm {
						alarms++
					}
					if !recoveredAt.IsZero() {
						break
					}
				}
				if alarms != 5 {
					t.Fatalf("hold alarms = %d over a 3h outage, want 5 (at 30m, then every 30m until the link returns at 3h)", alarms)
				}
				if recoveredAt.IsZero() {
					t.Fatalf("the link came back at +%s and no im-internet-recovered followed within stability+retryDelay — the episode latched", outageEnd.Sub(start))
				}
			})
		}
	}
}

// Round-7 audit finding #1, now pinned for the one hand-back that remains.
//
// The old hand-back arm re-armed its clock ONLY after a successful send; both
// of its gated exits (the episode-local retryDelay gate and the connector-level
// backoff hold) left it stale, and once the hand-back clause came due nothing
// could clear it, so the arm absorbed EVERY later round and the stability
// window, the final preflight, the decline alarm and the im-internet-recovered
// request were dead for the rest of the episode — on a link the probe reported
// as fully healthy.
//
// This fixture makes the re-ask gate (800ms) far longer than the scaled
// unusable-probe grace (40ms), sends one unusable-probe hand-back, keeps the
// probe unusable while the gate is shut, and then makes the link perfectly
// healthy. The transition function evaluates the re-ask gate BEFORE the
// hand-back clause, so the ordering of those knobs is not load-bearing
// (TestRecoveryIsIndependentOfTimingOrder covers every ordering in simulated
// time); this test drives the real loop for one of them.
func TestLoopStillReachesTheRecoveredPathAfterTheRetryGateAbsorbsAHandBack(t *testing.T) {
	scaleRecoveryTimingForTest(t)
	retryDelayFunc = func(time.Duration) time.Duration { return 800 * time.Millisecond }

	var mu sync.Mutex
	healthy := false
	internetProbeFunc = func(context.Context, string) internetprobe.Result {
		mu.Lock()
		defer mu.Unlock()
		if healthy {
			return reachableResult()
		}
		return blockedResult()
	}

	handedBack := make(chan struct{})
	recovered := make(chan struct{})
	var onceH, onceR sync.Once
	sendRecoveryState = func(_ *bridgev2.Bridge, _ *bridgev2.BridgeStateQueue, st status.BridgeState) bool {
		switch st.Error {
		case "im-internet-probe-unusable":
			onceH.Do(func() { close(handedBack) })
		case "im-internet-recovered":
			onceR.Do(func() { close(recovered) })
		}
		return true
	}

	client := newRecoveryTestClient()
	runRecoveryLoopForTestFrom(t, client, verdictReachable)

	select {
	case <-handedBack:
	case <-time.After(10 * time.Second):
		t.Fatal("never reached the unusable-probe hand-back")
	}
	// bridgev2 declined the hand-back (the loop is still alive), and the link is
	// now healthy. The loop must be able to run the stability window and final
	// preflight again and re-ask once its own retry gate opens.
	mu.Lock()
	healthy = true
	mu.Unlock()

	select {
	case <-recovered:
	case <-time.After(10 * time.Second):
		t.Fatal("a fully reachable link never produced im-internet-recovered: the hand-back clause latched on a retryDelay break, so it absorbs every round and the stability/preflight path is dead for the rest of the episode")
	}
}

// The other gated exit of the old hand-back arm: the connector-level backoff.
// Here the backoff is already widened (two hand-backs on record, so the next is
// held for 14 minutes — far longer than this test runs), the outage runs for a
// long time with the hold in force, and then the link comes back. A latched
// loop sits behind the hold forever; the transition function must run the
// stability window and preflight and produce im-internet-recovered, and must
// never have sent a hand-back of any kind.
func TestLoopStillReachesTheRecoveredPathWhileTheConnectorBackoffHolds(t *testing.T) {
	scaleRecoveryTimingForTest(t)
	retryDelayFunc = func(time.Duration) time.Duration { return 10 * time.Millisecond }

	var mu sync.Mutex
	healthy := false
	internetProbeFunc = func(context.Context, string) internetprobe.Result {
		mu.Lock()
		defer mu.Unlock()
		if healthy {
			return reachableResult()
		}
		return unreachableResult()
	}

	recovered := make(chan struct{})
	var onceR sync.Once
	var stateMu sync.Mutex
	var handBacks int
	sendRecoveryState = func(_ *bridgev2.Bridge, _ *bridgev2.BridgeStateQueue, st status.BridgeState) bool {
		switch st.Error {
		case "im-internet-probe-unusable":
			stateMu.Lock()
			handBacks++
			stateMu.Unlock()
		case "im-internet-recovered":
			onceR.Do(func() { close(recovered) })
		}
		return true
	}

	client := newRecoveryTestClient()
	start := time.Now()
	client.Main.noteHandBack(client.UserLogin.ID, start)
	client.Main.noteHandBack(client.UserLogin.ID, start)
	runRecoveryLoopForTest(t, client)

	// Many scaled alarm thresholds, with the hold in force.
	time.Sleep(600 * time.Millisecond)
	mu.Lock()
	healthy = true
	mu.Unlock()

	select {
	case <-recovered:
	case <-time.After(10 * time.Second):
		t.Fatal("a healthy link never produced im-internet-recovered while the connector backoff held — the hold starved the stability path")
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	if handBacks != 0 {
		t.Fatalf("hand-backs = %d, want 0: the connector backoff was holding for the whole test", handBacks)
	}
}

func TestHoldLogThrottlesToTheInterval(t *testing.T) {
	now := time.Unix(80_000, 0)
	const interval = 7 * time.Minute
	if !internetRecoveryHoldLogDue(now, time.Time{}, interval) {
		t.Fatal("the first hold of a streak must log")
	}
	if internetRecoveryHoldLogDue(now.Add(interval-time.Second), now, interval) {
		t.Fatal("logged again inside the interval")
	}
	if !internetRecoveryHoldLogDue(now.Add(interval), now, interval) {
		t.Fatal("did not log again once the interval elapsed")
	}
}

// ---------------------------------------------------------------------------
// The APS event loop, the confirmation window, and the wedge watchdog
// ---------------------------------------------------------------------------

// scaleEventLoopTimingForTest shrinks the confirmation delay and wedge poll so
// the real loops can be driven in milliseconds, alongside the recovery knobs.
func scaleEventLoopTimingForTest(t *testing.T) {
	t.Helper()
	scaleRecoveryTimingForTest(t)
	confirm, wedge, inbound := internetOutageConfirmDelay, receiveWedgePollInterval, apsSecondsSinceLastInbound
	t.Cleanup(func() {
		internetOutageConfirmDelay, receiveWedgePollInterval, apsSecondsSinceLastInbound = confirm, wedge, inbound
	})
	internetOutageConfirmDelay = time.Millisecond
	receiveWedgePollInterval = time.Millisecond
}

// recordStates installs a sendRecoveryState seam that records every state and
// returns a guarded accessor.
func recordStates(t *testing.T) func() []status.BridgeState {
	t.Helper()
	var mu sync.Mutex
	var states []status.BridgeState
	sendRecoveryState = func(_ *bridgev2.Bridge, _ *bridgev2.BridgeStateQueue, st status.BridgeState) bool {
		mu.Lock()
		defer mu.Unlock()
		states = append(states, st)
		return true
	}
	return func() []status.BridgeState {
		mu.Lock()
		defer mu.Unlock()
		return append([]status.BridgeState(nil), states...)
	}
}

func enteredRecovery(states []status.BridgeState) bool {
	for _, st := range states {
		if st.StateEvent == status.StateTransientDisconnect && st.Error == "im-internet-offline" {
			return true
		}
	}
	return false
}

// runEventLoopForTest starts the real APS event loop and registers a cleanup
// that stops it (and any recovery episode it entered) and waits for exit.
func runEventLoopForTest(t *testing.T, client *IMClient) {
	t.Helper()
	client.connectionEventWake = make(chan struct{}, 1)
	stop := client.stopChan
	done := make(chan struct{})
	go func() {
		client.runAPSConnectionEventLoop(stop, zerolog.Nop())
		close(done)
	}()
	t.Cleanup(func() {
		client.Disconnect()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("APS event loop did not exit after Disconnect")
		}
	})
}

// Mutation 12, directly: the confirmation window authorizes a teardown only
// when every round fails; one recovered or blocked round aborts it.
func TestConfirmPublicOutageRequiresEveryRoundToFail(t *testing.T) {
	scaleEventLoopTimingForTest(t)
	for _, tc := range []struct {
		name    string
		verdict func(round int) internetprobe.Result
		want    bool
	}{
		{"all rounds fail", func(int) internetprobe.Result { return unreachableResult() }, true},
		{"the last round recovers", func(r int) internetprobe.Result {
			if r == internetOutageConfirmRounds {
				return reachableResult()
			}
			return unreachableResult()
		}, false},
		{"a round is blocked", func(r int) internetprobe.Result {
			if r == 2 {
				return blockedResult()
			}
			return unreachableResult()
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			round := 0
			internetProbeFunc = func(context.Context, string) internetprobe.Result {
				mu.Lock()
				defer mu.Unlock()
				round++
				return tc.verdict(round)
			}
			client := newRecoveryTestClient()
			var probeLog internetProbeLogger
			got := client.confirmPublicOutage(context.Background(), client.stopChan, zerolog.Nop(), &probeLog)
			if got != tc.want {
				t.Fatalf("confirmed = %v, want %v", got, tc.want)
			}
		})
	}
}

// Mutations 12 and 13 through the loop: an interruption whose confirmation
// window recovers must NOT tear the client down, and an event latched during
// that window (the observer's RetryFailed escalation lands inside it) must be
// dropped rather than driving an unconditional teardown on the next pass.
func TestEventLoopDropsAnEventLatchedDuringAFailedConfirmation(t *testing.T) {
	scaleEventLoopTimingForTest(t)
	states := recordStates(t)
	client := newRecoveryTestClient()

	var mu sync.Mutex
	round := 0
	internetProbeFunc = func(_ context.Context, phase string) internetprobe.Result {
		mu.Lock()
		defer mu.Unlock()
		round++
		switch phase {
		case "aps_interruption_classification":
			return unreachableResult()
		case "outage_confirmation":
			// The observer escalates to RetryFailed mid-window.
			client.OnConnectionEvent(rustpushgo.ApsConnectionEventRetryFailed)
			return reachableResult()
		}
		t.Errorf("unexpected probe phase %q: the latched RetryFailed was acted on", phase)
		return reachableResult()
	}

	runEventLoopForTest(t, client)
	client.OnConnectionEvent(rustpushgo.ApsConnectionEventInterrupted)
	time.Sleep(200 * time.Millisecond)

	if enteredRecovery(states()) {
		t.Fatal("the client was torn down for an outage the confirmation window watched recover")
	}
	if _, ok := client.takeConnectionEvent(); ok {
		t.Fatal("the RetryFailed latched during the failed confirmation was not dropped")
	}
}

func TestEventLoopEntersRecoveryOnlyWhenTheOutageIsConfirmed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event rustpushgo.ApsConnectionEvent
		probe internetprobe.Result
		want  bool
	}{
		{"interrupted on a reachable link reconnects the transport", rustpushgo.ApsConnectionEventInterrupted, reachableResult(), false},
		{"interrupted with a blocked probe reconnects the transport", rustpushgo.ApsConnectionEventInterrupted, blockedResult(), false},
		{"interrupted and confirmed unreachable enters recovery", rustpushgo.ApsConnectionEventInterrupted, unreachableResult(), true},
		{"retry failure on a reachable link still enters recovery", rustpushgo.ApsConnectionEventRetryFailed, reachableResult(), true},
		{"retry failure with a blocked probe still enters recovery", rustpushgo.ApsConnectionEventRetryFailed, blockedResult(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scaleEventLoopTimingForTest(t)
			states := recordStates(t)
			internetProbeFunc = func(context.Context, string) internetprobe.Result { return tc.probe }
			client := newRecoveryTestClient()
			runEventLoopForTest(t, client)
			client.OnConnectionEvent(tc.event)
			time.Sleep(150 * time.Millisecond)
			if got := enteredRecovery(states()); got != tc.want {
				t.Fatalf("entered recovery = %v, want %v (states %v)", got, tc.want, states())
			}
		})
	}
}

// A reconnect storm publishes no Failed state, so the loop must classify it
// itself: interruptions on a REACHABLE link that exceed the burst threshold
// enter recovery instead of allowing yet another immediate reconnect.
func TestEventLoopTreatsABurstOfInterruptionsAsAStorm(t *testing.T) {
	scaleEventLoopTimingForTest(t)
	states := recordStates(t)
	internetProbeFunc = func(context.Context, string) internetprobe.Result { return reachableResult() }
	client := newRecoveryTestClient()
	runEventLoopForTest(t, client)

	deadline := time.Now().Add(5 * time.Second)
	for !enteredRecovery(states()) && time.Now().Before(deadline) {
		client.OnConnectionEvent(rustpushgo.ApsConnectionEventInterrupted)
		time.Sleep(5 * time.Millisecond)
	}
	if !enteredRecovery(states()) {
		t.Fatal("a burst of interruptions on a reachable link never entered recovery")
	}
}

func TestEventLoopTreatsBlockedProbeInterruptionsAsAStorm(t *testing.T) {
	scaleEventLoopTimingForTest(t)
	states := recordStates(t)
	internetProbeFunc = func(context.Context, string) internetprobe.Result { return blockedResult() }
	client := newRecoveryTestClient()
	runEventLoopForTest(t, client)

	deadline := time.Now().Add(5 * time.Second)
	for !enteredRecovery(states()) && time.Now().Before(deadline) {
		client.OnConnectionEvent(rustpushgo.ApsConnectionEventInterrupted)
		time.Sleep(5 * time.Millisecond)
	}
	if !enteredRecovery(states()) {
		t.Fatal("a burst of interruptions with locally blocked probes never entered conservative recovery")
	}
}

// Mutation 14: a wedge during an Internet outage must enter Apple-free
// recovery, not rebuild into the outage every wedge cycle.
func TestWedgeWatchdogClassifiesTheInternetBeforeRebuilding(t *testing.T) {
	for _, tc := range []struct {
		name         string
		probe        internetprobe.Result
		wantRecovery bool
		wantRebuild  bool
	}{
		{"reachable: rebuild", reachableResult(), false, true},
		{"blocked: rebuild on no evidence", blockedResult(), false, true},
		{"unreachable: Apple-free recovery", unreachableResult(), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scaleEventLoopTimingForTest(t)
			states := recordStates(t)
			internetProbeFunc = func(context.Context, string) internetprobe.Result { return tc.probe }
			apsSecondsSinceLastInbound = func(*rustpushgo.WrappedApsConnection) uint64 { return receiveWedgeRecoverySecs }

			client := newRecoveryTestClient()
			client.connection = &rustpushgo.WrappedApsConnection{}
			// Every close of an APS connection goes through this seam, so the
			// dummy pointer never reaches FFI.
			closeConn := closeAPSConnection
			t.Cleanup(func() { closeAPSConnection = closeConn })
			closeAPSConnection = func(*rustpushgo.WrappedApsConnection) {}
			done := make(chan struct{})
			go func() {
				client.runReceiveWedgeWatchdog(client.stopChan, zerolog.Nop())
				close(done)
			}()
			t.Cleanup(func() {
				client.Disconnect()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("wedge watchdog did not exit")
				}
			})
			time.Sleep(150 * time.Millisecond)

			got := states()
			rebuilt := false
			for _, st := range got {
				if st.StateEvent == status.StateUnknownError && st.Error == "im-receive-wedged" {
					rebuilt = true
				}
			}
			if enteredRecovery(got) != tc.wantRecovery || rebuilt != tc.wantRebuild {
				t.Fatalf("recovery=%v rebuild=%v, want recovery=%v rebuild=%v (states %v)", enteredRecovery(got), rebuilt, tc.wantRecovery, tc.wantRebuild, got)
			}
		})
	}
}

// runWedgeWatchdogForTest runs the real watchdog on client for `for`, then
// stops it and waits for it to exit. The dummy connection never reaches FFI:
// the inbound-age and close seams intercept every call on it.
func runWedgeWatchdogForTest(t *testing.T, client *IMClient, run time.Duration) {
	t.Helper()
	client.connection = &rustpushgo.WrappedApsConnection{}
	closeConn := closeAPSConnection
	t.Cleanup(func() { closeAPSConnection = closeConn })
	closeAPSConnection = func(*rustpushgo.WrappedApsConnection) {}
	done := make(chan struct{})
	go func() {
		client.runReceiveWedgeWatchdog(client.stopChan, zerolog.Nop())
		close(done)
	}()
	time.Sleep(run)
	client.Disconnect()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wedge watchdog did not exit")
	}
}

func countWedgeRebuilds(states []status.BridgeState) int {
	n := 0
	for _, st := range states {
		if st.StateEvent == status.StateUnknownError && st.Error == "im-receive-wedged" {
			n++
		}
	}
	return n
}

// Round-8 F6: the wedge rebuild shares the recovery loop's widening hand-back
// backoff, and the run is cleared by evidence of receiving — not by a rebuild
// merely completing. Without both halves a client that is rebuilt and never
// receives is rebuilt every wedge threshold + bridgev2 delay forever, with the
// StatusKit IDS sweep on every second one.
func TestWedgeWatchdogSharesTheHandBackBackoff(t *testing.T) {
	// A wedged connection that has never received since Connect began: the
	// inbound age (the full threshold) exceeds the time since startupTime.
	wedgedSinceConnect := func(client *IMClient) {
		client.startupTime = time.Now()
		apsSecondsSinceLastInbound = func(*rustpushgo.WrappedApsConnection) uint64 { return receiveWedgeRecoverySecs }
	}

	t.Run("held while the previous rebuild is recent", func(t *testing.T) {
		scaleEventLoopTimingForTest(t)
		states := recordStates(t)
		internetProbeFunc = func(context.Context, string) internetprobe.Result { return reachableResult() }
		client := newRecoveryTestClient()
		wedgedSinceConnect(client)
		client.Main.noteHandBack(client.UserLogin.ID, time.Now())

		runWedgeWatchdogForTest(t, client, 150*time.Millisecond)

		if n := countWedgeRebuilds(states()); n != 0 {
			t.Fatalf("watchdog requested %d rebuilds while the hand-back backoff was holding; want 0", n)
		}
		if _, ok := client.Main.handBackDue(client.UserLogin.ID, time.Now()); ok {
			t.Fatal("the run was cleared by a connection that never received a frame")
		}
	})

	t.Run("fires once the backoff elapses and widens it", func(t *testing.T) {
		scaleEventLoopTimingForTest(t)
		states := recordStates(t)
		internetProbeFunc = func(context.Context, string) internetprobe.Result { return reachableResult() }
		client := newRecoveryTestClient()
		wedgedSinceConnect(client)
		client.Main.noteHandBack(client.UserLogin.ID, time.Now().Add(-handBackDelay(1)-time.Minute))

		runWedgeWatchdogForTest(t, client, 150*time.Millisecond)

		if n := countWedgeRebuilds(states()); n != 1 {
			t.Fatalf("watchdog requested %d rebuilds once the backoff had elapsed; want exactly 1 (one-shot)", n)
		}
		wait, ok := client.Main.handBackDue(client.UserLogin.ID, time.Now())
		if ok || wait < handBackDelay(2)-time.Minute {
			t.Fatalf("the rebuild did not widen the run: due=%v wait=%v, want a hold of about %v", ok, wait, handBackDelay(2))
		}
	})

	t.Run("an inbound frame after Connect began clears the run", func(t *testing.T) {
		scaleEventLoopTimingForTest(t)
		states := recordStates(t)
		internetProbeFunc = func(context.Context, string) internetprobe.Result { return reachableResult() }
		client := newRecoveryTestClient()
		client.startupTime = time.Now().Add(-2 * time.Hour)
		apsSecondsSinceLastInbound = func(*rustpushgo.WrappedApsConnection) uint64 { return 30 }
		client.Main.noteHandBack(client.UserLogin.ID, time.Now())

		runWedgeWatchdogForTest(t, client, 150*time.Millisecond)

		if n := countWedgeRebuilds(states()); n != 0 {
			t.Fatalf("a healthy connection requested %d rebuilds", n)
		}
		if _, ok := client.Main.handBackDue(client.UserLogin.ID, time.Now()); !ok {
			t.Fatal("a frame received after Connect began must clear the hand-back run")
		}
	})

	t.Run("the seeded stamp from before Connect is not a frame", func(t *testing.T) {
		scaleEventLoopTimingForTest(t)
		_ = recordStates(t)
		internetProbeFunc = func(context.Context, string) internetprobe.Result { return reachableResult() }
		client := newRecoveryTestClient()
		// LoadUserLogin seeds last_inbound_ms before Connect stamps startupTime,
		// so a quiet connection's age is always older than the Connect.
		client.startupTime = time.Now()
		apsSecondsSinceLastInbound = func(*rustpushgo.WrappedApsConnection) uint64 { return 5 }
		client.Main.noteHandBack(client.UserLogin.ID, time.Now())

		runWedgeWatchdogForTest(t, client, 150*time.Millisecond)

		if _, ok := client.Main.handBackDue(client.UserLogin.ID, time.Now()); ok {
			t.Fatal("the run was cleared on a stamp older than this Connect — that is the seed, not a frame")
		}
	})
}

// The re-ask gate bounds Apple contact on a healthy link that bridgev2 keeps
// declining: one im-internet-recovered per retryDelay, not one per poll. A
// decision table pins the clause; this pins the rate through the real loop.
func TestLoopReAsksAtMostOncePerRetryDelay(t *testing.T) {
	scaleRecoveryTimingForTest(t)
	const retryDelay = 100 * time.Millisecond
	retryDelayFunc = func(time.Duration) time.Duration { return retryDelay }
	internetProbeFunc = func(context.Context, string) internetprobe.Result { return reachableResult() }

	var mu sync.Mutex
	var recovered []time.Time
	sendRecoveryState = func(_ *bridgev2.Bridge, _ *bridgev2.BridgeStateQueue, st status.BridgeState) bool {
		if st.Error == "im-internet-recovered" {
			mu.Lock()
			recovered = append(recovered, time.Now())
			mu.Unlock()
		}
		return true
	}

	client := newRecoveryTestClient()
	stop := runRecoveryLoopForTest(t, client)
	const window = 500 * time.Millisecond
	time.Sleep(window)
	stop()

	// A count, not per-pair spacing: the gate is measured on the round's clock,
	// which is stamped before the preflight probe, so wall-clock send times can
	// sit a few hundred microseconds inside retryDelay. Ungated, the loop would
	// re-ask every poll interval — hundreds of requests in this window.
	mu.Lock()
	defer mu.Unlock()
	maxRequests := int(window/retryDelay) + 2
	if len(recovered) < 2 || len(recovered) > maxRequests {
		t.Fatalf("recovered requests in %s = %d, want between 2 and %d: the gate is not bounding Apple contact", window, len(recovered), maxRequests)
	}
}
