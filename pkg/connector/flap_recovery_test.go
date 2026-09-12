// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package connector

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
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
	apsLastInboundLowerBound = func(_ *rustpushgo.WrappedApsConnection, now time.Time, _ uint64) time.Time { return now }
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
func TestFlapRebuildIsCountedOnlyWhenReplacementConsumesRequest(t *testing.T) {
	main := &IMConnector{}
	login := networkid.UserLoginID("actual-rebuild")
	t0 := time.Unix(61_000, 0)

	main.markFlapRebuildRequested(login)
	main.markFlapRebuildRequested(login)
	if got := main.flapRebuildCount(login); got != 0 {
		t.Fatalf("sent but unacted request counted as %d rebuilds, want 0", got)
	}
	if wait, ok := main.flapRebuildDue(login, t0.Add(time.Minute)); !ok || wait != 0 {
		t.Fatalf("unacted first request held recovery: ok=%v wait=%v", ok, wait)
	}

	if !main.consumeFlapRebuildRequest(login, t0) {
		t.Fatal("replacement did not consume the pending flap request")
	}
	if got := main.flapRebuildCount(login); got != 1 {
		t.Fatalf("first actual replacement count = %d, want 1", got)
	}
	if main.flapRecoveryDefersStatusKit(login) {
		t.Fatal("first actual replacement deferred StatusKit")
	}
	if main.consumeFlapRebuildRequest(login, t0.Add(time.Second)) {
		t.Fatal("one pending request was consumed twice")
	}

	main.markFlapRebuildRequested(login)
	if !main.consumeFlapRebuildRequest(login, t0.Add(7*time.Minute)) {
		t.Fatal("second replacement did not consume its request")
	}
	if !main.flapRecoveryDefersStatusKit(login) {
		t.Fatal("second actual replacement did not defer StatusKit")
	}
}

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

// Item 4: the run ends once the epoch has accrued fifteen minutes of healthy
// time — the lease is literally 15 minutes, a lease one second short does not
// clear, an interruption pauses the accrual (the stretch before it is banked,
// the unhealthy time after it does not count) and does not restart it. Then
// the same through the real watchdog with the lease scaled down.
func TestOldPreInterruptionFrameCannotRestartHealthyLease(t *testing.T) {
	main := &IMConnector{}
	login := networkid.UserLoginID("lease-frame-order")
	t0 := time.Unix(90_000, 0)
	main.noteFlapRebuild(login, t0)

	if main.noteCourierActivity(login, t0.Add(time.Second), t0.Add(5*time.Second)) {
		t.Fatal("short first stretch unexpectedly served the lease")
	}
	main.noteCourierUnhealthy(login, t0.Add(10*time.Second))
	accrued := main.flapRunHealthyAccrued(login)

	// This is the last frame from before the interruption. Observing it on a
	// later watchdog tick must not restart accrual.
	if main.noteCourierActivity(login, t0.Add(9*time.Second), t0.Add(11*time.Second)) {
		t.Fatal("pre-interruption frame served the lease")
	}
	if !main.flapRunHealthySince(login).IsZero() {
		t.Fatal("pre-interruption frame restarted the healthy stretch")
	}
	if got := main.flapRunHealthyAccrued(login); got != accrued {
		t.Fatalf("pre-interruption frame changed accrued health from %v to %v", accrued, got)
	}

	if main.noteCourierActivity(login, t0.Add(12*time.Second), t0.Add(13*time.Second)) {
		t.Fatal("new post-interruption frame unexpectedly served the lease")
	}
	if got := main.flapRunHealthySince(login); !got.Equal(t0.Add(12 * time.Second)) {
		t.Fatalf("post-interruption stretch started at %v, want frame time %v", got, t0.Add(12*time.Second))
	}
}

func TestHealthyAccrualStartsAtFrameTimeNotWatchdogTick(t *testing.T) {
	main := &IMConnector{}
	login := networkid.UserLoginID("lease-frame-time")
	t0 := time.Unix(91_000, 0)
	main.noteFlapRebuild(login, t0)

	main.noteCourierActivity(login, t0.Add(time.Second), t0.Add(time.Minute))
	main.noteCourierUnhealthy(login, t0.Add(61*time.Second))
	if got := main.flapRunHealthyAccrued(login); got != time.Minute {
		t.Fatalf("banked health = %v, want 1m from frame to interruption", got)
	}
}

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
	if !main.noteCourierHealthy(login, t0.Add(16*time.Minute)) {
		t.Fatal("fifteen healthy minutes did not clear the run")
	}
	if got := main.flapRebuildCount(login); got != 0 {
		t.Fatalf("run length after clearing = %d, want 0", got)
	}
	if _, ok := main.flapRebuildDue(login, t0.Add(16*time.Minute)); !ok {
		t.Fatal("a cleared run must give the next rebuild normal timing")
	}

	t.Run("an interruption pauses the accrual", func(t *testing.T) {
		main := &IMConnector{}
		main.noteFlapRebuild(login, t0)
		// Ten healthy minutes, banked by the interruption.
		main.noteCourierHealthy(login, t0.Add(time.Minute))
		main.noteCourierUnhealthy(login, t0.Add(11*time.Minute))
		if got := main.flapRunHealthyAccrued(login); got != 10*time.Minute {
			t.Fatalf("banked healthy time after the interruption = %v, want 10m", got)
		}
		if !main.flapRunHealthySince(login).IsZero() {
			t.Fatal("the interruption did not end the stretch")
		}
		// Nine unhealthy minutes count for nothing: at t0+20m the courier is
		// healthy again with 10m banked, so the lease is served at t0+25m —
		// not at t0+16m (wall clock) and not at t0+35m (a restarted lease).
		if main.noteCourierHealthy(login, t0.Add(20*time.Minute)) {
			t.Fatal("cleared on the first healthy tick after an interruption")
		}
		if main.noteCourierHealthy(login, t0.Add(24*time.Minute)) {
			t.Fatal("cleared at 14 accrued minutes: the unhealthy time was counted")
		}
		if main.noteCourierHealthy(login, t0.Add(25*time.Minute-time.Second)) {
			t.Fatal("cleared one second before the accrued lease was served")
		}
		if !main.noteCourierHealthy(login, t0.Add(25*time.Minute)) {
			t.Fatal("fifteen accrued healthy minutes did not clear the run: the interruption restarted the lease")
		}
	})

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

// Finding 1 (flap-run review): the link class this feature targets. rustpush's
// receive watchdog forces a transport reconnect at 300s of silence, which the
// APS observer reports as Interrupted, and the Go watchdog's own tick at that
// age is unhealthy too — so a courier that stalls and self-heals every five
// minutes ends a stretch on every cycle while never storming or wedging.
// Literal timeline, one cycle: frames resume at B; ticks at B+60..B+240 are
// healthy (a stretch from B+60); at B+300 the stall interrupts and the tick
// reads 300. Each cycle banks 240s, so three cycles bank 720s and the fourth
// cycle's stretch serves the lease 180s in, at B3+240 = t0+19m. A contiguous
// lease is never served on this link.
func TestSelfHealingStallLinkServesTheLease(t *testing.T) {
	main := &IMConnector{}
	login := newRecoveryTestClient().UserLogin.ID
	t0 := time.Unix(80_000, 0)
	main.noteFlapRebuild(login, t0)
	main.noteFlapRebuild(login, t0)
	if !main.flapRecoveryDefersStatusKit(login) {
		t.Fatal("precondition: a repeated run defers")
	}

	var clearedAt time.Time
	for cycle := 0; cycle < 6 && clearedAt.IsZero(); cycle++ {
		base := t0.Add(time.Duration(cycle) * 5 * time.Minute)
		for tick := 1; tick <= 4; tick++ {
			at := base.Add(time.Duration(tick) * time.Minute)
			if main.noteCourierHealthy(login, at) {
				clearedAt = at
				break
			}
		}
		if clearedAt.IsZero() {
			// The stall: the APS event loop and the watchdog tick both end
			// the stretch; the second is a no-op.
			main.noteCourierUnhealthy(login, base.Add(5*time.Minute))
			main.noteCourierUnhealthy(login, base.Add(5*time.Minute))
		}
	}
	if clearedAt.IsZero() {
		t.Fatal("six stall cycles never served the lease: the run is pinned on a self-healing link")
	}
	if want := t0.Add(19 * time.Minute); !clearedAt.Equal(want) {
		t.Fatalf("lease served at +%v, want +19m (three cycles of 240s banked plus 180s of the fourth stretch)", clearedAt.Sub(t0))
	}
	if main.flapRecoveryDefersStatusKit(login) || main.flapRebuildCount(login) != 0 {
		t.Fatal("the served lease did not end the run")
	}
}

// The same link through the real watchdog: the inbound age cycles four
// healthy ticks then two past the bound, so no contiguous stretch can reach
// the lease, and the deferred StatusKit block still activates.
func TestSelfHealingStallLinkActivatesDeferredStatusKit(t *testing.T) {
	scaleEventLoopTimingForTest(t)
	scaleHealthyLeaseForTest(t, 20*time.Millisecond)
	_ = recordStates(t)
	internetProbeFunc = func(context.Context, string) internetprobe.Result { return reachableResult() }
	client := newRecoveryTestClient()
	client.startupTime = time.Now().Add(-2 * time.Hour)
	var ticks atomic.Int64
	apsSecondsSinceLastInbound = func(*rustpushgo.WrappedApsConnection) uint64 {
		if n := ticks.Add(1) - 1; n%6 < 4 {
			return 30
		}
		return 400
	}
	apsLastInboundLowerBound = func(_ *rustpushgo.WrappedApsConnection, now time.Time, _ uint64) time.Time { return now }
	client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
	client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
	client.launchOrDeferStatusKit(zerolog.Nop(), false)

	runWedgeWatchdogForTest(t, client, 300*time.Millisecond)

	if got := client.Main.flapRebuildCount(client.UserLogin.ID); got != 0 {
		t.Fatalf("a self-healing stall link left the flap run at %d after %d ticks", got, ticks.Load())
	}
	if !client.statusKitStartupLaunched() {
		t.Fatal("the deferred StatusKit block never activated on a self-healing stall link")
	}
}

// A rebuild zeroes the epoch's healthy time — the stretch and the bank — so
// the lease is always the replacement's own. Fourteen minutes banked by the
// client being replaced must not let the replacement clear after one.
func TestFlapRebuildZeroesHealthyTime(t *testing.T) {
	main := &IMConnector{}
	login := newRecoveryTestClient().UserLogin.ID
	t0 := time.Unix(90_000, 0)
	main.noteFlapRebuild(login, t0)
	main.noteCourierHealthy(login, t0.Add(time.Minute))
	main.noteCourierUnhealthy(login, t0.Add(15*time.Minute))
	main.noteCourierHealthy(login, t0.Add(16*time.Minute))
	if got := main.flapRunHealthyAccrued(login); got != 14*time.Minute {
		t.Fatalf("precondition: banked = %v, want 14m", got)
	}

	main.noteFlapRebuild(login, t0.Add(17*time.Minute))
	if got := main.flapRunHealthyAccrued(login); got != 0 {
		t.Fatalf("banked healthy time survived the rebuild: %v", got)
	}
	if !main.flapRunHealthySince(login).IsZero() {
		t.Fatal("the running stretch survived the rebuild")
	}
	if main.noteCourierHealthy(login, t0.Add(18*time.Minute)) {
		t.Fatal("the replacement inherited the replaced client's healthy time")
	}
	if main.noteCourierHealthy(login, t0.Add(33*time.Minute-time.Second)) {
		t.Fatal("cleared before the replacement's own lease was served")
	}
	if !main.noteCourierHealthy(login, t0.Add(33*time.Minute)) {
		t.Fatal("the replacement's own fifteen minutes did not clear the run")
	}
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

func TestAutomaticStatusKitAccessIsGatedWhileExplicitCommandsRemainAvailable(t *testing.T) {
	saved := getStatusKitClient
	t.Cleanup(func() { getStatusKitClient = saved })
	calls := 0
	want := &rustpushgo.WrappedStatusKitClient{}
	getStatusKitClient = func(*rustpushgo.Client) (*rustpushgo.WrappedStatusKitClient, error) {
		calls++
		return want, nil
	}

	client := newRecoveryTestClient()
	client.client = &rustpushgo.Client{}
	defer func() { client.client = nil }()
	client.statusKitDeferred.Store(true)

	if sk, err := client.automaticStatusKitClient(); err == nil || sk != nil {
		t.Fatalf("automatic getter during deferral = (%v, %v), want nil/error", sk, err)
	}
	if calls != 0 {
		t.Fatalf("automatic getter reached Rust %d times during deferral", calls)
	}
	if sk, err := client.statusKitClientForCommand(); err != nil || sk != want {
		t.Fatalf("explicit getter during deferral = (%v, %v), want client/nil", sk, err)
	}
	if calls != 1 {
		t.Fatalf("explicit getter reached Rust %d times, want 1", calls)
	}
}

func TestAutomaticStatusKitCloudPassIsGatedDuringDeferral(t *testing.T) {
	client := newRecoveryTestClient()
	client.client = &rustpushgo.Client{}
	defer func() { client.client = nil }()
	client.statusKitDeferred.Store(true)
	if err := client.syncCloudStatusKitPeersForce(context.Background(), zerolog.Nop(), true); err != nil {
		t.Fatalf("deferred automatic CloudKit pass returned error: %v", err)
	}
	if client.statusKitPassInFlight.Load() {
		t.Fatal("deferred automatic CloudKit pass entered the Apple-facing body")
	}
}

func TestAutomaticPresenceSubscriptionIsGatedDuringDeferral(t *testing.T) {
	client := newRecoveryTestClient()
	client.client = &rustpushgo.Client{}
	defer func() { client.client = nil }()
	client.statusKitDeferred.Store(true)

	client.subscribeToContactPresence(zerolog.Nop())
	if !client.lastPresenceSubscribe.IsZero() {
		t.Fatal("automatic presence subscription entered its Apple-facing path during StatusKit deferral")
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
		wantPending   bool
	}{
		{"confirmed outage: normal timing, run untouched", verdictUnreachable, true, 1, false},
		{"courier failure: held behind the run", verdictReachable, false, 1, false},
		{"courier failure with no prior run: request pending, run not advanced", verdictBlocked, true, 0, true},
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
			if pending := client.Main.flapRebuildRequested(client.UserLogin.ID); pending != tc.wantPending {
				t.Fatalf("pending rebuild = %v, want %v", pending, tc.wantPending)
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
	now := time.Now()
	client.Main.noteFlapRebuild(client.UserLogin.ID, now.Add(-2*time.Minute))
	// A stretch that began a minute ago, so the interruption has something
	// to bank.
	client.Main.noteCourierHealthy(client.UserLogin.ID, now.Add(-time.Minute))
	if client.Main.flapRunHealthySince(client.UserLogin.ID).IsZero() {
		t.Fatal("precondition: the lease must be running")
	}
	runEventLoopForTest(t, client)

	client.OnConnectionEvent(rustpushgo.ApsConnectionEventInterrupted)
	deadline := time.Now().Add(2 * time.Second)
	for !client.Main.flapRunHealthySince(client.UserLogin.ID).IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("an APS interruption did not end the healthy stretch")
		}
		time.Sleep(time.Millisecond)
	}
	if got := client.Main.flapRebuildCount(client.UserLogin.ID); got != 1 {
		t.Fatalf("an interruption changed the run length to %d", got)
	}
	if got := client.Main.flapRunHealthyAccrued(client.UserLogin.ID); got < time.Minute {
		t.Fatalf("the interruption discarded the stretch instead of banking it: accrued %v, want >= 1m", got)
	}
}

func TestDiscardedAPSEventStillEndsTheHealthLease(t *testing.T) {
	client := newRecoveryTestClient()
	now := time.Now()
	client.Main.noteFlapRebuild(client.UserLogin.ID, now.Add(-2*time.Minute))
	client.Main.noteCourierHealthy(client.UserLogin.ID, now.Add(-time.Minute))
	if client.Main.flapRunHealthySince(client.UserLogin.ID).IsZero() {
		t.Fatal("precondition: the lease must be running")
	}

	// There is no event loop consuming the latch. The callback itself must end
	// the stretch because controller policy may later coalesce or discard it.
	client.OnConnectionEvent(rustpushgo.ApsConnectionEventRetryFailed)
	if !client.Main.flapRunHealthySince(client.UserLogin.ID).IsZero() {
		t.Fatal("an APS event left the healthy stretch running until controller consumption")
	}
	if got := client.Main.flapRunHealthyAccrued(client.UserLogin.ID); got < time.Minute {
		t.Fatalf("discarded APS event failed to bank the stretch: accrued %v", got)
	}
}

// The healthy bound is rustpush's stall definition, five missed 60s keepalive
// cycles: an age of 299s is a healthy tick, 300s and 301s end the stretch (the
// bound is exclusive, as rustpush's own `idle >= APNS_STALL_TIMEOUT` is
// inclusive). Pinned with
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
		{300, false},
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

func TestLogoutWaitsForRecoveryWriterBeforeClearingFlapRun(t *testing.T) {
	_, _, _ = installLifecycleSeams(t)
	client := newLifecycleTestClient()
	client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())

	client.internetRecoveryMu.Lock()
	locked := true
	defer func() {
		if locked {
			client.internetRecoveryMu.Unlock()
		}
	}()
	done := make(chan struct{})
	go func() {
		client.LogoutRemote(context.Background())
		close(done)
	}()

	deadline := time.Now().Add(time.Second)
	for !client.terminationRequested() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !client.terminationRequested() {
		t.Fatal("logout did not begin")
	}
	select {
	case <-done:
		t.Fatal("logout cleared state without waiting for the active recovery writer")
	default:
	}

	// Model the recovery writer's final action while it still owns the mutex.
	client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
	client.internetRecoveryMu.Unlock()
	locked = false
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("logout did not finish after the recovery writer exited")
	}
	if got := client.Main.flapRebuildCount(client.UserLogin.ID); got != 0 {
		t.Fatalf("recovery recreated flap state after logout: count=%d", got)
	}
}

// Finding 2 (flap-run review): the deferred launch must carry Connect's
// invite-sweep decision to the block itself. Observed where it lands —
// runStatusKitStartup records the argument it received — not where it was
// captured, so a launch that passes a constant fails for one of the two
// values. Both a deferred activation (through the real watchdog) and a
// normal Connect launch are driven, for both decisions.
func TestStatusKitLaunchCarriesConnectsSweepDecision(t *testing.T) {
	waitLaunched := func(t *testing.T, client *IMClient) int32 {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for {
			if got := client.statusKitLaunchedWith.Load(); got != statusKitLaunchNotYet {
				return got
			}
			if time.Now().After(deadline) {
				t.Fatal("runStatusKitStartup never ran")
			}
			time.Sleep(time.Millisecond)
		}
	}
	want := func(skip bool) int32 {
		if skip {
			return statusKitLaunchSkippedSweep
		}
		return statusKitLaunchWithSweep
	}
	for _, skip := range []bool{true, false} {
		name := "sweep"
		if skip {
			name = "sweep skipped"
		}
		t.Run("deferred activation/"+name, func(t *testing.T) {
			scaleEventLoopTimingForTest(t)
			scaleHealthyLeaseForTest(t, 20*time.Millisecond)
			_ = recordStates(t)
			internetProbeFunc = func(context.Context, string) internetprobe.Result { return reachableResult() }
			client := newRecoveryTestClient()
			healthySinceConnect(client, 30)
			client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
			client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
			client.launchOrDeferStatusKit(zerolog.Nop(), skip)
			if client.statusKitStartupLaunched() {
				t.Fatal("precondition: the block must be deferred at Connect")
			}

			runWedgeWatchdogForTest(t, client, 200*time.Millisecond)

			if !client.statusKitStartupLaunched() {
				t.Fatal("the served lease did not launch the block")
			}
			if got := waitLaunched(t, client); got != want(skip) {
				t.Fatalf("skipHeavyIDSSweep=%v at Connect, but the deferred launch ran the block with %d, want %d", skip, got, want(skip))
			}
		})
		t.Run("normal launch/"+name, func(t *testing.T) {
			client := newRecoveryTestClient()
			client.launchOrDeferStatusKit(zerolog.Nop(), skip)
			if got := waitLaunched(t, client); got != want(skip) {
				t.Fatalf("skipHeavyIDSSweep=%v at Connect, but the launch ran the block with %d, want %d", skip, got, want(skip))
			}
		})
	}
}

// Finding 3 (flap-run review): the inbound stamp rustpushgo seeds at
// LoadUserLogin is not health. A courier that never delivers a frame reads a
// small, constant age that is never smaller than the time since Connect, so
// receive is never confirmed — and until it is, no tick feeds the lease, the
// run is not cleared, and a deferred block is not activated.
func TestSeededInboundStampDoesNotFeedTheLease(t *testing.T) {
	scaleEventLoopTimingForTest(t)
	scaleHealthyLeaseForTest(t, 5*time.Millisecond)
	_ = recordStates(t)
	internetProbeFunc = func(context.Context, string) internetprobe.Result { return reachableResult() }
	client := newRecoveryTestClient()
	// Connect just happened; the seed is 30s old and stays 30s old (no
	// frame). 30 < time since startupTime is false for the whole run.
	client.startupTime = time.Now()
	apsSecondsSinceLastInbound = func(*rustpushgo.WrappedApsConnection) uint64 { return 30 }
	client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
	client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
	client.launchOrDeferStatusKit(zerolog.Nop(), false)

	runWedgeWatchdogForTest(t, client, 100*time.Millisecond)

	if !client.Main.flapRunHealthySince(client.UserLogin.ID).IsZero() || client.Main.flapRunHealthyAccrued(client.UserLogin.ID) != 0 {
		t.Fatal("the seeded stamp fed the lease before any real frame")
	}
	if got := client.Main.flapRebuildCount(client.UserLogin.ID); got != 2 {
		t.Fatalf("a courier that never received cleared the run (count %d)", got)
	}
	if client.statusKitStartupLaunched() {
		t.Fatal("a courier that never received activated the deferred StatusKit block")
	}
}

// Finding 4 (flap-run review): the serving tick hands the watchdog's OWN stop
// channel to the activation, so a teardown that lands between the tick's
// dispatch and the activation starts nothing. Driven deterministically: the
// inbound-age seam blocks inside the tick, the epoch is torn down while it is
// blocked, then the tick is released to serve an already-accrued lease.
func TestServingTickHonorsTheEpochStopChannel(t *testing.T) {
	scaleEventLoopTimingForTest(t)
	_ = recordStates(t)
	internetProbeFunc = func(context.Context, string) internetprobe.Result { return reachableResult() }
	client := newRecoveryTestClient()
	client.startupTime = time.Now().Add(-2 * time.Hour)
	t0 := time.Now().Add(-16 * time.Minute)
	client.Main.noteFlapRebuild(client.UserLogin.ID, t0.Add(-time.Second))
	client.Main.noteFlapRebuild(client.UserLogin.ID, t0.Add(-time.Second))
	client.launchOrDeferStatusKit(zerolog.Nop(), false)
	// A stretch older than the lease: the first tick serves it.
	client.Main.noteCourierHealthy(client.UserLogin.ID, t0)

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	apsSecondsSinceLastInbound = func(*rustpushgo.WrappedApsConnection) uint64 {
		once.Do(func() {
			close(entered)
			<-release
		})
		return 30
	}
	apsLastInboundLowerBound = func(_ *rustpushgo.WrappedApsConnection, now time.Time, _ uint64) time.Time { return now }
	client.connection = &rustpushgo.WrappedApsConnection{}
	closeConn := closeAPSConnection
	t.Cleanup(func() { closeAPSConnection = closeConn })
	closeAPSConnection = func(*rustpushgo.WrappedApsConnection) {}
	done := make(chan struct{})
	go func() {
		client.runReceiveWedgeWatchdog(client.stopChan, zerolog.Nop())
		close(done)
	}()

	<-entered
	client.Disconnect() // closes the epoch's stop channel mid-tick
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wedge watchdog did not exit")
	}

	if got := client.Main.flapRebuildCount(client.UserLogin.ID); got != 0 {
		t.Fatalf("precondition: the released tick must have served the lease (run length %d)", got)
	}
	if client.statusKitStartupLaunched() {
		t.Fatal("the serving tick activated StatusKit against a torn-down epoch: it did not consult the epoch's stop channel")
	}
	if !client.statusKitDeferred.Load() {
		t.Fatal("the deferral was dropped by teardown; a replacement decides for itself")
	}
}

// Finding 6 (flap-run review): the "healthy time now accrues" notice is
// logged once per epoch, not once per stretch. The stall link this lease is
// built for starts a new stretch every few minutes, so a per-stretch line is
// the unthrottled log the review found.
func TestLeaseAccrualIsLoggedOncePerEpoch(t *testing.T) {
	scaleEventLoopTimingForTest(t)
	_ = recordStates(t)
	internetProbeFunc = func(context.Context, string) internetprobe.Result { return reachableResult() }
	client := newRecoveryTestClient()
	client.startupTime = time.Now().Add(-2 * time.Hour)
	// Healthy, unhealthy, healthy, ...: a new stretch every other tick.
	var ticks atomic.Int64
	apsSecondsSinceLastInbound = func(*rustpushgo.WrappedApsConnection) uint64 {
		if ticks.Add(1)%2 == 1 {
			return 30
		}
		return 400
	}
	apsLastInboundLowerBound = func(_ *rustpushgo.WrappedApsConnection, now time.Time, _ uint64) time.Time { return now }
	client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
	client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
	client.launchOrDeferStatusKit(zerolog.Nop(), false)

	var mu sync.Mutex
	var output bytes.Buffer
	log := zerolog.New(&lockedWriter{mu: &mu, w: &output})
	client.connection = &rustpushgo.WrappedApsConnection{}
	closeConn := closeAPSConnection
	t.Cleanup(func() { closeAPSConnection = closeConn })
	closeAPSConnection = func(*rustpushgo.WrappedApsConnection) {}
	done := make(chan struct{})
	go func() {
		client.runReceiveWedgeWatchdog(client.stopChan, log)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	client.Disconnect()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wedge watchdog did not exit")
	}

	mu.Lock()
	text := output.String()
	mu.Unlock()
	if stretches := ticks.Load() / 2; stretches < 5 {
		t.Fatalf("precondition: only %d stretches were driven", stretches)
	}
	if got := strings.Count(text, "healthy time now accrues toward the lease"); got != 1 {
		t.Fatalf("the accrual notice was logged %d times over many stretches, want exactly once per epoch\n%s", got, text)
	}
	if strings.Contains(text, "the health lease has started") {
		t.Fatal("the notice still claims a lease has started")
	}
}

// lockedWriter serializes a test log buffer against the reading test.
type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// A deferred epoch whose run is gone — served by the retired predecessor's
// final tick racing this Connect's decision — has nothing left to wait for:
// it activates on its first healthy tick instead of holding presence for a
// lease no run will ever serve.
func TestDeferredEpochWithoutARunActivatesOnFirstHealthyTick(t *testing.T) {
	scaleEventLoopTimingForTest(t)
	_ = recordStates(t)
	internetProbeFunc = func(context.Context, string) internetprobe.Result { return reachableResult() }
	client := newRecoveryTestClient()
	healthySinceConnect(client, 30)
	client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
	client.Main.noteFlapRebuild(client.UserLogin.ID, time.Now())
	client.launchOrDeferStatusKit(zerolog.Nop(), false)
	if !client.statusKitDeferred.Load() {
		t.Fatal("precondition: the epoch must be deferred")
	}
	// The run ends under the deferred epoch, with the production lease in
	// force so no tick can serve one.
	client.Main.clearFlapRun(client.UserLogin.ID)

	runWedgeWatchdogForTest(t, client, 100*time.Millisecond)

	if !client.statusKitStartupLaunched() {
		t.Fatal("a deferred epoch with no run never activated StatusKit")
	}
	if client.statusKitDeferred.Load() {
		t.Fatal("the activation did not clear the deferred mark")
	}
}
