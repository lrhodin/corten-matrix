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
//     public-only loop's 60-second stability window. A flapping courier
//     delivers frames between flaps, so one frame refutes nothing here; only
//     sustained health does. Folding this into handBackRun would force one of
//     the two clear rules on both — the first-frame clear would make this run
//     never widen, and the 15-minute lease would hold the daily-wedge rebuild
//     that today rebuilds immediately.
//
// Separation is also what keeps confirmed-outage recovery on normal timing:
// an episode entered on a confirmed outage never consults or writes this run
// (internet_recovery.go, courierFailure), and it never consults handBackRun on
// its recovered path either. Only a replacement that itself starts flapping
// enters a courier-failure episode and meets the widening schedule.
//
// INVARIANT — what a live run asserts: "every client built for this login
// since the run began was built to replace a courier that failed with no
// outage evidence, and no client since then has stayed healthy for a full
// lease". It is a statement about the whole sequence, not about the most
// recent sample: one frame, one quiet tick, or one clean Connect does not
// refute it, and a single interruption after ten healthy minutes re-asserts
// it (the lease restarts). It is refuted by exactly two things — a client
// that stays continuously healthy for flapRecoveryHealthyLease
// (noteCourierHealthy), or a logout (clearFlapRun) — and by a process restart,
// because the run is memory-only: a deliberate restart is the operator's own
// reconnect and starts clean.
//
// "Continuously healthy" (see noteCourierHealthy/noteCourierUnhealthy) means
// that within the current client epoch, for the whole lease: no APS connection
// event (Interrupted or RetryFailed) was observed, and every receive-watchdog
// tick found the inbound-frame age under courierHealthyMaxIdleSecs. A healthy
// link can sustain that indefinitely: rustpush sends a keepalive Ping every
// 60s (aps.rs), the Pong is broadcast on messages_cont like every other frame
// and stamps last_inbound_ms (lib.rs drain task), so the age the watchdog reads
// on an idle link is at most about 60s at every tick. The bound is rustpush's
// own stall definition — five missed keepalive cycles — so a quiet minute
// cannot reset the lease and a dead-but-not-yet-wedged link cannot pass as
// healthy for more than five of them.
//
// Writers of the fields, in full:
//   - noteFlapRebuild:     ConsecutiveRebuilds++, LastRebuild = now, HealthySince = zero
//   - noteCourierHealthy:  HealthySince = now (first healthy tick of a lease); deletes the run once the lease is served
//   - noteCourierUnhealthy: HealthySince = zero
//   - clearFlapRun:        deletes the run (LogoutRemote)
type flapRecoveryRun struct {
	// ConsecutiveRebuilds is how many courier-failure rebuilds this run has
	// requested. The Nth request is held handBackDelay(N-1) after the (N-1)th:
	// the first is not held at all, then 7m, 14m, 28m, 45m.
	ConsecutiveRebuilds int
	// LastRebuild is when the most recent request was sent (stamped at the
	// send, unconditionally, exactly as noteHandBack does — an unobservable
	// queue drop must err toward fewer Apple attempts).
	LastRebuild time.Time
	// HealthySince is the start of the current health lease, or zero while
	// the courier is not known to be healthy.
	HealthySince time.Time
}

// flapRecoveryHealthyLease is how long a courier must stay continuously
// healthy to end a flap run — and, for an epoch whose StatusKit startup was
// deferred, to start it. A var only so tests can shrink it; never reassigned in
// production.
var flapRecoveryHealthyLease = 15 * time.Minute

// courierHealthyMaxIdleSecs is the inbound-frame age above which a tick does
// not count as healthy. It is rustpush's APNS_STALL_TIMEOUT (lib.rs): five
// missed keepalive cycles, the point at which the Rust side itself forces a
// transport reconnect. Kept as a literal rather than derived from
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
// request of a run is never held; the run's clock starts at the first request.
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

// noteFlapRebuild records a courier-failure rebuild request, widening the next
// interval. The health lease is reset: the client being replaced is not the one
// whose health will refute the run.
func (c *IMConnector) noteFlapRebuild(login networkid.UserLoginID, now time.Time) {
	c.flapRunMu.Lock()
	defer c.flapRunMu.Unlock()
	run := c.flapRun(login)
	run.ConsecutiveRebuilds++
	run.LastRebuild = now
	run.HealthySince = time.Time{}
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

// flapRecoveryDefersStatusKit reports whether a Connect happening now is a
// REPEATED courier-failure rebuild, in which case the epoch brings core
// APNs/iMessage up normally and holds its optional StatusKit startup until the
// health lease clears the run. A fresh IMConnector (a process restart) has no
// run and never defers.
func (c *IMConnector) flapRecoveryDefersStatusKit(login networkid.UserLoginID) bool {
	return c.flapRebuildCount(login) >= flapRecoveryStatusKitDeferralThreshold
}

// noteCourierHealthy records one healthy receive-watchdog tick. The first
// healthy tick after a rebuild or an interruption starts the lease; a tick
// that finds the lease fully served ends the run and reports cleared=true,
// exactly once per run. With no run in progress there is nothing to lease and
// nothing is recorded.
func (c *IMConnector) noteCourierHealthy(login networkid.UserLoginID, now time.Time) (cleared bool) {
	c.flapRunMu.Lock()
	defer c.flapRunMu.Unlock()
	run := c.flapRuns[login]
	if run == nil {
		return false
	}
	if run.HealthySince.IsZero() {
		run.HealthySince = now
		return false
	}
	if now.Sub(run.HealthySince) < flapRecoveryHealthyLease {
		return false
	}
	delete(c.flapRuns, login)
	return true
}

// noteCourierUnhealthy ends the current health lease: an APS connection event
// (the courier left Generated, or a regeneration failed) or an inbound-frame
// age past the healthy bound. The run itself is untouched — this is the
// evidence that RE-ASSERTS it.
func (c *IMConnector) noteCourierUnhealthy(login networkid.UserLoginID) {
	c.flapRunMu.Lock()
	defer c.flapRunMu.Unlock()
	if run := c.flapRuns[login]; run != nil {
		run.HealthySince = time.Time{}
	}
}

// flapRunHealthySince exposes the lease start for logging; zero when no lease
// is running.
func (c *IMConnector) flapRunHealthySince(login networkid.UserLoginID) time.Time {
	c.flapRunMu.Lock()
	defer c.flapRunMu.Unlock()
	if run := c.flapRuns[login]; run != nil {
		return run.HealthySince
	}
	return time.Time{}
}

// clearFlapRun ends the run outright. Called by LogoutRemote, so a later
// re-login with the same ID starts with no backoff carried over.
func (c *IMConnector) clearFlapRun(login networkid.UserLoginID) {
	c.flapRunMu.Lock()
	defer c.flapRunMu.Unlock()
	delete(c.flapRuns, login)
}
