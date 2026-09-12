package connector

// Round-8 audit: the two regression tests the audit delivered failing. Both now
// pass with their assertions intact; the corrections are IMClient.needsRetirement
// plus the early lifecycleTerminated flag (finding 1) and the verdict-typed
// finishPreflight (finding 2).

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/lrhodin/corten-matrix/pkg/internetprobe"
)

// FINDING 1. retirePreviousClient's predicate is
//
//	previous.client == nil && !previous.hasOrphanedRecovery()  =>  skip
//
// A client that is IN THE MIDDLE of Connect satisfies both halves: Connect
// creates a fresh OPEN stopChan at its top (so hasOrphanedRecovery is false by
// design, per its own doc comment) and does not assign c.client until after
// safeRestoreTokenProvider and newRustClient have returned — a window the file
// itself documents as up to 300s. So the load proceeds, installs a second
// client, and the first Connect goes on to reach markConnected and launch its
// own APS event loop and receive-wedge watchdog on a stopChan nobody will ever
// close: a second APS connection on the same persisted device token, which is
// the duplicate-token "early eof" storm the function exists to prevent.
//
// connectEpochActive() is the predicate that reports this correctly and was not
// consulted; needsRetirement() now unions all three states, and Disconnect
// flags the client before waiting so the retired Connect abandons itself.
func TestRetirePreviousClientRetiresAnInProgressConnectEpoch(t *testing.T) {
	_, _, _ = installLifecycleSeams(t)

	// Exactly the state Connect leaves between its epoch-channel setup and
	// `c.client = client`: epoch live, no Rust client yet.
	previous := newLifecycleTestClient()
	previous.stopChan = make(chan struct{})
	previous.recoveryDone = make(chan struct{})
	previous.recoveryBridgeState = &bridgev2.BridgeStateQueue{}

	if !previous.connectEpochActive() {
		t.Fatal("precondition: this fixture must look like a live connect epoch")
	}
	if previous.client != nil || previous.hasOrphanedRecovery() {
		t.Fatal("precondition: a mid-Connect client has no Rust client and no orphaned recovery")
	}

	err := retirePreviousClient(previous, zerolog.Nop())
	if err != nil {
		// Acceptable: refusing the load is the documented safe answer.
		return
	}
	if previous.connectEpochActive() {
		t.Fatal("retirePreviousClient returned nil without retiring a LIVE connect epoch: " +
			"the load will install a second client while the first Connect is still running, " +
			"which then reaches markConnected and starts its own APS loop and wedge watchdog " +
			"on a stopChan nobody closes — a duplicate APNs connection on the same device token")
	}
}

// FINDING 2. step() carries three verdicts and clause 3 deliberately requires
// verdictUnreachable to withdraw, because probe.go's contract is that Blocked
// ("this host would not let the probe run") is the ABSENCE of evidence and must
// never be read as "Internet down". The decision table pins that:
// "clause 3: blocked does not withdraw a pending recovered request".
//
// finishPreflight took a bool, not a recoveryVerdict. The loop called it as
// finishPreflight(now, preflight.Reachable()), which collapsed Blocked and
// Unreachable into one value — so a Blocked final preflight withdrew a pending
// recovered request, costing a full bridgev2 reconnect wait cycle on no
// evidence. Because the parameter was a bool, no case in the finishPreflight
// subtable could express this; it was a missing dimension, not a missing case.
// It now takes the verdict and scores it with the same clause-2/3 helpers as
// step, so the two cannot diverge.
//
// Reachability: the main round's probe and the preflight are separate Probe()
// calls seconds apart. Blocked requires every dial to return EACCES/EPERM/
// EMFILE/ENFILE/EAFNOSUPPORT; EMFILE/ENFILE are transient descriptor pressure,
// which probe.go explicitly classifies as Blocked precisely because it is "we
// could not ask" rather than "the network is down".
func TestBlockedFinalPreflightDoesNotWithdrawAPendingRebuild(t *testing.T) {
	scaleRecoveryTimingForTest(t)
	// Push both escape hatches out of reach so only the preflight path can act.
	internetRecoveryBlockedGrace = time.Hour
	internetRecoveryOfflineCeiling = time.Hour
	internetRecoveryEpisodeCeiling = time.Hour
	retryDelayFunc = func(time.Duration) time.Duration { return 10 * time.Millisecond }

	var mu sync.Mutex
	preflights := 0
	internetProbeFunc = func(_ context.Context, phase string) internetprobe.Result {
		mu.Lock()
		defer mu.Unlock()
		// The link itself is healthy for every ordinary round, all the way
		// through: this test is only about what a Blocked PREFLIGHT does.
		if phase != "final_reconnect_preflight" {
			return reachableResult()
		}
		preflights++
		if preflights == 1 {
			// The first preflight passes, so a recovered request is now pending.
			return reachableResult()
		}
		// Descriptor exhaustion: the probe could not run. No evidence of an outage.
		return blockedResult()
	}

	var stateMu sync.Mutex
	var states []status.BridgeState
	sendRecoveryState = func(_ *bridgev2.Bridge, _ *bridgev2.BridgeStateQueue, st status.BridgeState) bool {
		stateMu.Lock()
		states = append(states, st)
		stateMu.Unlock()
		return true
	}

	client := newRecoveryTestClient()
	stop := runRecoveryLoopForTest(t, client)
	time.Sleep(300 * time.Millisecond)
	stop()

	stateMu.Lock()
	defer stateMu.Unlock()
	sawRecovered := false
	for i, st := range states {
		if st.StateEvent == status.StateUnknownError && st.Error == "im-internet-recovered" {
			sawRecovered = true
			continue
		}
		if sawRecovered && st.StateEvent == status.StateTransientDisconnect {
			t.Fatalf("state %d withdrew a pending rebuild because the FINAL PREFLIGHT was Blocked, "+
				"not Unreachable: finishPreflight takes a bool and loses the distinction clause 3 "+
				"of step preserves. states=%v", i, states)
		}
	}
	if !sawRecovered {
		t.Fatalf("expected an im-internet-recovered request from the first (passing) preflight, got %v", states)
	}
}
