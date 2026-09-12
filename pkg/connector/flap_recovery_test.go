// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package connector

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/lrhodin/corten-matrix/pkg/internetprobe"
	"github.com/lrhodin/corten-matrix/pkg/rustpushgo"
)

// Tests for the flap-recovery run (flap_recovery.go) and the StatusKit
// deferral it drives. Numbered comments cite the spec item each test pins.
// Schedules and the lease are pinned with LITERALS, never with the constants:
// a fixture derived from the constant passes for any value of it.

// scaleHealthyLeaseForTest shrinks the lease so the real watchdog can serve it
// in milliseconds, and restores it afterward.
func scaleHealthyLeaseForTest(t *testing.T, lease time.Duration) {
	t.Helper()
	saved := flapRecoveryHealthyLease
	t.Cleanup(func() { flapRecoveryHealthyLease = saved })
	flapRecoveryHealthyLease = lease
}

// healthySinceConnect makes the watchdog's inbound-age seam report a real,
// recent frame: the age is far smaller than the time since startupTime (so it
// cannot be the seed), and under the healthy bound.
func healthySinceConnect(client *IMClient, idle uint64) {
	client.startupTime = time.Now().Add(-2 * time.Hour)
	apsSecondsSinceLastInbound = func(*rustpushgo.WrappedApsConnection) uint64 { return idle }
}

// Item 1: the run lives on IMConnector, so replacing the IMClient — which is
// what every rebuild does — does not erase it. Three successive clients share
// one connector; each sees the history the previous ones built.
func TestFlapRunSurvivesClientReplacements(t *testing.T) {
	main := &IMConnector{}
	login := newRecoveryTestClient().UserLogin.ID
	now := time.Unix(50_000, 0)

	first := newRecoveryTestClient()
	first.Main = main
	main.noteFlapRebuild(first.UserLogin.ID, now)

	second := newRecoveryTestClient()
	second.Main = main
	if got := second.Main.flapRebuildCount(login); got != 1 {
		t.Fatalf("after one client replacement the run reports %d rebuilds, want 1", got)
	}
	if _, ok := second.Main.flapRebuildDue(login, now.Add(time.Minute)); ok {
		t.Fatal("a second courier-failure rebuild one minute after the first was not held")
	}
	main.noteFlapRebuild(second.UserLogin.ID, now.Add(8*time.Minute))

	third := newRecoveryTestClient()
	third.Main = main
	if got := third.Main.flapRebuildCount(login); got != 2 {
		t.Fatalf("after two client replacements the run reports %d rebuilds, want 2", got)
	}
	if !third.Main.flapRecoveryDefersStatusKit(login) {
		t.Fatal("the third client must see a repeated flap run and defer StatusKit")
	}
}

// Item 2: the first courier-failure rebuild is not held; the next ones are
// held 7m, 14m, 28m, then 45m after the previous request. Pinned with
// literal durations.
func TestFlapRebuildScheduleIsPinnedWithLiterals(t *testing.T) {
	main := &IMConnector{}
	login := newRecoveryTestClient().UserLogin.ID
	at := time.Unix(60_000, 0)

	if wait, ok := main.flapRebuildDue(login, at); !ok || wait != 0 {
		t.Fatalf("the first courier-failure rebuild must not be held: ok=%v wait=%v", ok, wait)
	}
	main.noteFlapRebuild(login, at)

	holds := []time.Duration{7 * time.Minute, 14 * time.Minute, 28 * time.Minute, 45 * time.Minute, 45 * time.Minute}
	for i, hold := range holds {
		wait, ok := main.flapRebuildDue(login, at.Add(hold-time.Second))
		if ok || wait != time.Second {
			t.Fatalf("rebuild %d: one second before the %v hold expires: ok=%v wait=%v, want held with 1s left", i+2, hold, ok, wait)
		}
		if _, ok := main.flapRebuildDue(login, at.Add(hold)); !ok {
			t.Fatalf("rebuild %d: the %v hold did not expire on time", i+2, hold)
		}
		at = at.Add(hold)
		main.noteFlapRebuild(login, at)
	}
	if got := main.flapRebuildCount(login); got != len(holds)+1 {
		t.Fatalf("run length = %d, want %d", got, len(holds)+1)
	}
}

// Item 3: one inbound frame clears the hand-back run (that is its job) but
// must NOT clear the flap run — a flapping courier delivers frames between
// flaps. Driven through the real watchdog with the lease at its production
// length, so many healthy ticks pass and none of them can end the run.
func TestOneInboundFrameDoesNotClearTheFlapRun(t *testing.T) {
	scaleEventLoopTimingForTest(t)
	_ = recordStates(t)
	internetProbeFunc = func(context.Context, string) internetprobe.Result { return reachableResult() }
	client := newRecoveryTestClient()
	healthySinceConnect(client, 30)
	client.Main.noteHandBack(client.UserLogin.ID, time.Now())
	client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())

	runWedgeWatchdogForTest(t, client, 100*time.Millisecond)

	if _, ok := client.Main.handBackDue(client.UserLogin.ID, time.Now()); !ok {
		t.Fatal("precondition: the frame must still clear the hand-back run (unchanged behavior)")
	}
	if got := client.Main.flapRebuildCount(client.UserLogin.ID); got != 1 {
		t.Fatalf("inbound frames cleared the flap run (count %d); only a full healthy lease may", got)
	}
	if client.Main.flapRunHealthySince(client.UserLogin.ID).IsZero() {
		t.Fatal("healthy ticks did not start the lease")
	}
}

// Item 4: the run ends after fifteen continuously healthy minutes — the lease
// is literally 15 minutes, a lease one second short does not clear, and an
// interruption inside it restarts the clock. Then the same through the real
// watchdog with the lease scaled down.
func TestFifteenHealthyMinutesClearTheFlapRun(t *testing.T) {
	if flapRecoveryHealthyLease != 15*time.Minute {
		t.Fatalf("flapRecoveryHealthyLease = %v, want the specified 15 minutes", flapRecoveryHealthyLease)
	}
	main := &IMConnector{}
	login := newRecoveryTestClient().UserLogin.ID
	t0 := time.Unix(70_000, 0)
	main.noteFlapRebuild(login, t0)

	if main.noteCourierHealthy(login, t0.Add(time.Minute)) {
		t.Fatal("the first healthy tick starts the lease; it cannot serve it")
	}
	if main.noteCourierHealthy(login, t0.Add(time.Minute+15*time.Minute-time.Second)) {
		t.Fatal("cleared one second before the lease was served")
	}
	// An interruption restarts the clock: fifteen minutes measured from the
	// original start no longer count.
	main.noteCourierUnhealthy(login)
	if main.noteCourierHealthy(login, t0.Add(16*time.Minute)) {
		t.Fatal("cleared on the first healthy tick after an interruption")
	}
	if main.noteCourierHealthy(login, t0.Add(16*time.Minute+15*time.Minute-time.Second)) {
		t.Fatal("cleared before the restarted lease was served")
	}
	if !main.noteCourierHealthy(login, t0.Add(31*time.Minute)) {
		t.Fatal("fifteen continuously healthy minutes did not clear the run")
	}
	if got := main.flapRebuildCount(login); got != 0 {
		t.Fatalf("run length after clearing = %d, want 0", got)
	}
	if _, ok := main.flapRebuildDue(login, t0.Add(31*time.Minute)); !ok {
		t.Fatal("a cleared run must give the next rebuild normal timing")
	}

	t.Run("through the real watchdog", func(t *testing.T) {
		scaleEventLoopTimingForTest(t)
		scaleHealthyLeaseForTest(t, 20*time.Millisecond)
		_ = recordStates(t)
		internetProbeFunc = func(context.Context, string) internetprobe.Result { return reachableResult() }
		client := newRecoveryTestClient()
		healthySinceConnect(client, 30)
		client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())

		runWedgeWatchdogForTest(t, client, 200*time.Millisecond)

		if got := client.Main.flapRebuildCount(client.UserLogin.ID); got != 0 {
			t.Fatalf("a served lease left the flap run at %d", got)
		}
	})
}

// Item 5: a fresh IMConnector — what a process restart builds — has no run:
// normal timing, no deferral. Memory-only by design.
func TestFreshConnectorStartsClean(t *testing.T) {
	main := &IMConnector{}
	login := newRecoveryTestClient().UserLogin.ID
	if wait, ok := main.flapRebuildDue(login, time.Now()); !ok || wait != 0 {
		t.Fatalf("fresh connector holds a rebuild: ok=%v wait=%v", ok, wait)
	}
	if main.flapRecoveryDefersStatusKit(login) {
		t.Fatal("fresh connector defers StatusKit")
	}
	if main.flapRebuildCount(login) != 0 {
		t.Fatal("fresh connector has a run")
	}
	if main.noteCourierHealthy(login, time.Now()) {
		t.Fatal("a healthy tick with no run must not report a clear")
	}
	if !main.flapRunHealthySince(login).IsZero() {
		t.Fatal("a healthy tick with no run must not start a lease")
	}
}

// Items 6 and a process restart: the first courier-failure rebuild (run
// length 1) and a fresh connector both launch the StatusKit block exactly as
// before, and neither consumes the StatusKit-CloudKit first-call bypass.
func TestFirstFlapRecoveryStartsStatusKitNormally(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rebuilds int
	}{
		{"process restart", 0},
		{"first flap rebuild", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newRecoveryTestClient()
			for range tc.rebuilds {
				client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
			}
			client.launchOrDeferStatusKit(zerolog.Nop(), false)
			if !client.statusKitStartupLaunched() {
				t.Fatal("the StatusKit block was not launched")
			}
			if client.statusKitDeferred.Load() {
				t.Fatal("StatusKit is marked deferred on a normal epoch")
			}
			if client.statusKitCloudPassFirstCallDone.Load() {
				t.Fatal("the CloudKit first-call bypass was consumed on a normal epoch")
			}
		})
	}
}

// Item 7: a repeated courier-failure rebuild (run length >= 2) launches none
// of the block — InitStatuskit, subscriptions, the sweep and ShareStatus all
// live inside it — marks the epoch deferred, and consumes the CloudKit
// first-call bypass so the peer-key pass honors its inter-pass backoff.
func TestRepeatedFlapRecoveryDefersStatusKit(t *testing.T) {
	for _, rebuilds := range []int{2, 3, 5} {
		client := newRecoveryTestClient()
		for range rebuilds {
			client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
		}
		client.launchOrDeferStatusKit(zerolog.Nop(), true)
		if client.statusKitStartupLaunched() {
			t.Fatalf("run length %d: the StatusKit block was launched", rebuilds)
		}
		if !client.statusKitDeferred.Load() {
			t.Fatalf("run length %d: the epoch is not marked deferred", rebuilds)
		}
		if !client.statusKitCloudPassFirstCallDone.Load() {
			t.Fatalf("run length %d: the CloudKit first-call bypass is still available", rebuilds)
		}
		if !client.statusKitSkipHeavyIDSSweep {
			t.Fatalf("run length %d: Connect's sweep decision was not captured for the deferred launch", rebuilds)
		}
	}
}

// Item 8: teardown cancels the delayed activation. The lease is driven by the
// epoch's watchdog, which exits on the stop channel, so a teardown before the
// lease is served leaves StatusKit unlaunched however long we wait afterward;
// and an activation that lands on a closed stop channel starts nothing.
func TestTeardownCancelsDeferredStatusKitActivation(t *testing.T) {
	scaleEventLoopTimingForTest(t)
	scaleHealthyLeaseForTest(t, 60*time.Millisecond)
	_ = recordStates(t)
	internetProbeFunc = func(context.Context, string) internetprobe.Result { return reachableResult() }
	client := newRecoveryTestClient()
	healthySinceConnect(client, 30)
	client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
	client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
	client.launchOrDeferStatusKit(zerolog.Nop(), false)

	// Torn down well inside the lease; then wait past it.
	runWedgeWatchdogForTest(t, client, 10*time.Millisecond)
	time.Sleep(150 * time.Millisecond)
	if client.statusKitStartupLaunched() {
		t.Fatal("StatusKit was launched after the epoch was torn down")
	}
	if !client.statusKitDeferred.Load() {
		t.Fatal("the deferral was dropped by teardown; a replacement decides for itself")
	}
	// The stop channel is closed now: a lease clearing on this very tick must
	// still start nothing.
	client.activateDeferredStatusKit(client.stopChan, zerolog.Nop())
	if client.statusKitStartupLaunched() {
		t.Fatal("activation ran against a torn-down epoch")
	}
}

// Item 9: the served lease launches the deferred block exactly once — not on
// every later healthy tick, and not again from any other caller.
func TestHealthyLeaseActivatesDeferredStatusKitExactlyOnce(t *testing.T) {
	scaleEventLoopTimingForTest(t)
	scaleHealthyLeaseForTest(t, 20*time.Millisecond)
	_ = recordStates(t)
	internetProbeFunc = func(context.Context, string) internetprobe.Result { return reachableResult() }
	client := newRecoveryTestClient()
	healthySinceConnect(client, 30)
	client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
	client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
	client.launchOrDeferStatusKit(zerolog.Nop(), false)
	if client.statusKitStartupLaunched() {
		t.Fatal("precondition: the block must be deferred at Connect")
	}
	stop := client.stopChan

	runWedgeWatchdogForTest(t, client, 200*time.Millisecond)

	if got := client.statusKitStarts.Load(); got != 1 {
		t.Fatalf("StatusKit block launched %d times by the served lease, want exactly 1", got)
	}
	if client.statusKitDeferred.Load() {
		t.Fatal("the launch did not clear the deferred mark")
	}
	if client.Main.flapRebuildCount(client.UserLogin.ID) != 0 {
		t.Fatal("the served lease did not clear the run")
	}
	// Every other path is a no-op once launched.
	client.startStatusKit(zerolog.Nop(), "a second caller")
	client.activateDeferredStatusKit(stop, zerolog.Nop())
	client.launchOrDeferStatusKit(zerolog.Nop(), false)
	if got := client.statusKitStarts.Load(); got != 1 {
		t.Fatalf("StatusKit block launched %d times after repeat callers, want exactly 1", got)
	}
}

// Item 10: explicit StatusKit commands are not gated by the deferral. The
// command path reaches the Rust getter — which lazily builds the StatusKit
// client — on a deferred epoch exactly as on a normal one. This pins the
// ABSENCE of a gate: it fails if statusKitClientForCommand ever consults
// statusKitDeferred.
func TestExplicitStatusKitCommandsAreNotGatedByDeferral(t *testing.T) {
	saved := getStatusKitClient
	t.Cleanup(func() { getStatusKitClient = saved })
	var mu sync.Mutex
	calls := 0
	want := &rustpushgo.WrappedStatusKitClient{}
	getStatusKitClient = func(*rustpushgo.Client) (*rustpushgo.WrappedStatusKitClient, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return want, nil
	}

	client := newRecoveryTestClient()
	client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
	client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
	client.launchOrDeferStatusKit(zerolog.Nop(), false)
	if !client.statusKitDeferred.Load() {
		t.Fatal("precondition: the epoch must be deferred")
	}
	// A dummy Rust client the seam intercepts before any FFI call; detached
	// below so no teardown can reach it.
	client.client = &rustpushgo.Client{}
	defer func() { client.client = nil }()

	sk, err := client.statusKitClientForCommand()
	if err != nil || sk != want {
		t.Fatalf("command path on a deferred epoch: sk=%v err=%v, want the getter's client", sk, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("getter called %d times, want 1: the command path must reach it without a gate", calls)
	}
	if client.statusKitStartupLaunched() {
		t.Fatal("an explicit command launched the automatic startup block; only the lease may")
	}
}

// Item 11, through the real loop: a confirmed-outage episode requests its
// rebuild after the stability window regardless of the flap run, and does not
// lengthen it; a courier-failure episode with the same run is held.
func TestConfirmedOutageRecoveryIgnoresTheFlapRun(t *testing.T) {
	for _, tc := range []struct {
		name          string
		entry         recoveryVerdict
		wantRecovered bool
		wantRun       int
	}{
		{"confirmed outage: normal timing, run untouched", verdictUnreachable, true, 1},
		{"courier failure: held behind the run", verdictReachable, false, 1},
		{"courier failure with no prior run: normal timing, run started", verdictBlocked, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scaleRecoveryTimingForTest(t)
			internetProbeFunc = func(context.Context, string) internetprobe.Result { return reachableResult() }
			var mu sync.Mutex
			recovered := 0
			sendRecoveryState = func(_ *bridgev2.Bridge, _ *bridgev2.BridgeStateQueue, st status.BridgeState) bool {
				if st.Error == "im-internet-recovered" {
					mu.Lock()
					recovered++
					mu.Unlock()
				}
				return true
			}
			client := newRecoveryTestClient()
			if tc.entry != verdictBlocked {
				// One recent courier-failure rebuild: the next one is held 7m.
				client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
			}

			stop := runRecoveryLoopForTestFrom(t, client, tc.entry)
			time.Sleep(200 * time.Millisecond)
			stop()

			mu.Lock()
			got := recovered
			mu.Unlock()
			if (got > 0) != tc.wantRecovered {
				t.Fatalf("recovered requests = %d, want sent=%v", got, tc.wantRecovered)
			}
			if run := client.Main.flapRebuildCount(client.UserLogin.ID); run != tc.wantRun {
				t.Fatalf("flap run length = %d, want %d", run, tc.wantRun)
			}
		})
	}
}

// The pure clause: a ready window with a flap hold yields no action and a
// throttled hold log; with no hold it yields the preflight as before. Pinned
// on step directly so the loop test above cannot pass by timing alone.
func TestStepHoldsAReadyRebuildBehindTheFlapRun(t *testing.T) {
	now := time.Unix(100_000, 0)
	timing := currentRecoveryTiming()
	ready := func() *recoveryEpisode {
		e := newRecoveryEpisode(now.Add(-2*time.Minute), verdictReachable)
		e.stabilizing = true
		e.stable.Observe(now.Add(-2*time.Minute), true, timing.stablePeriod)
		return e
	}
	held := ready().step(recoveryRound{now: now, verdict: verdictReachable, retryDelay: 7 * time.Minute, timing: timing, flapHold: 6 * time.Minute})
	if held.action != actionNone || !held.flapHoldLog {
		t.Fatalf("held round: action=%v flapHoldLog=%v, want none/logged", held.action, held.flapHoldLog)
	}
	e := ready()
	e.lastHoldLogAt = now.Add(-time.Minute)
	if d := e.step(recoveryRound{now: now, verdict: verdictReachable, retryDelay: 7 * time.Minute, timing: timing, flapHold: 6 * time.Minute}); d.flapHoldLog {
		t.Fatal("the hold log must be throttled to the re-ask cadence")
	}
	free := ready().step(recoveryRound{now: now, verdict: verdictReachable, retryDelay: 7 * time.Minute, timing: timing})
	if free.action != actionPreflight || free.flapHoldLog {
		t.Fatalf("unheld round: action=%v flapHoldLog=%v, want preflight/unlogged", free.action, free.flapHoldLog)
	}
}

// An APS connection event — the courier left Generated — ends the health
// lease through the real event loop, without touching the run's length.
func TestAPSEventEndsTheHealthLease(t *testing.T) {
	scaleEventLoopTimingForTest(t)
	_ = recordStates(t)
	internetProbeFunc = func(context.Context, string) internetprobe.Result { return reachableResult() }
	client := newRecoveryTestClient()
	client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
	client.Main.noteCourierHealthy(client.UserLogin.ID, time.Now())
	if client.Main.flapRunHealthySince(client.UserLogin.ID).IsZero() {
		t.Fatal("precondition: the lease must be running")
	}
	runEventLoopForTest(t, client)

	client.OnConnectionEvent(rustpushgo.ApsConnectionEventInterrupted)
	deadline := time.Now().Add(2 * time.Second)
	for !client.Main.flapRunHealthySince(client.UserLogin.ID).IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("an APS interruption did not end the health lease")
		}
		time.Sleep(time.Millisecond)
	}
	if got := client.Main.flapRebuildCount(client.UserLogin.ID); got != 1 {
		t.Fatalf("an interruption changed the run length to %d", got)
	}
}

// The healthy bound is rustpush's stall definition, five missed 60s keepalive
// cycles: an age of 299s is a healthy tick, 301s ends the lease. Pinned with
// literals so the bound cannot drift toward the 600s wedge threshold, where a
// dead link would pass as healthy for ten minutes at a time.
func TestWatchdogHealthBoundIsTheRustStallTimeout(t *testing.T) {
	if courierHealthyMaxIdleSecs != 300 {
		t.Fatalf("courierHealthyMaxIdleSecs = %d, want 300", courierHealthyMaxIdleSecs)
	}
	for _, tc := range []struct {
		idle        uint64
		wantLeasing bool
	}{
		{299, true},
		{301, false},
	} {
		scaleEventLoopTimingForTest(t)
		_ = recordStates(t)
		internetProbeFunc = func(context.Context, string) internetprobe.Result { return reachableResult() }
		client := newRecoveryTestClient()
		healthySinceConnect(client, tc.idle)
		client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
		// A lease already running, so the unhealthy branch has something to end.
		client.Main.noteCourierHealthy(client.UserLogin.ID, time.Now())

		runWedgeWatchdogForTest(t, client, 50*time.Millisecond)

		if leasing := !client.Main.flapRunHealthySince(client.UserLogin.ID).IsZero(); leasing != tc.wantLeasing {
			t.Fatalf("idle %ds: lease running=%v, want %v", tc.idle, leasing, tc.wantLeasing)
		}
	}
}

// LogoutRemote ends the run, so a re-login with the same ID starts clean.
func TestLogoutClearsTheFlapRun(t *testing.T) {
	_, _, _ = installLifecycleSeams(t)
	client := newLifecycleTestClient()
	client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
	client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
	client.LogoutRemote(context.Background())
	if client.Main.flapRecoveryDefersStatusKit(client.UserLogin.ID) || client.Main.flapRebuildCount(client.UserLogin.ID) != 0 {
		t.Fatal("LogoutRemote must clear the flap run")
	}
}
