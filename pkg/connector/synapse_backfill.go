// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package connector

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

// Deep-history (backward) backfill on a homeserver without batch sending.
//
// bridgev2's own queue gives up before it starts on such a server:
//
//	func (br *Bridge) RunBackfillQueue() {
//	    ...
//	    if !br.Matrix.GetCapabilities().BatchSending {
//	        log.Warn().Msg("Backfill queue is enabled in config, but Matrix
//	                        server doesn't support batch sending")
//	        return
//	    }
//
// BatchSending comes from Beeper's /versions extension (bridgev2/matrix
// connector.go: `Capabilities.BatchSending = SpecVersions.Supports(
// BeeperFeatureBatchSending)`), which Synapse — and Conduit, and Dendrite —
// does not implement. So on every self-hosted install the `backfill_task`
// rows this connector queues (createPortalsFromCloudSync) and the ones
// bridgev2 queues itself (portal.go, on room creation) are written and then
// never read by anything: history stops at whatever the FORWARD pass
// delivered and there is no second pass, ever.
//
// Delivery is not the problem — `sendBackfill` already falls back to
// `sendLegacyBackfill` (individual timestamped events) when batch sending is
// unavailable, which is exactly how forward backfill works today on Synapse.
// The only missing piece is a loop that dispatches the queued tasks. This is
// that loop, and it does nothing else: it calls the same two exported
// bridgev2 entry points the real queue calls, in the same order —
// `DB.BackfillTask.GetNext` then `Bridge.DoBackfillTask` — so all task
// bookkeeping (MarkDispatched, is_done / queue_done, next_dispatch_min_ts,
// login resolution, the panic guard) stays inside bridgev2 where the rest of
// the bridge expects it to live.
//
// It runs ONLY when batch sending is unavailable. If bridgev2's queue is
// running, this one is not, so a task can never be dispatched twice.

const (
	// synapseDrainStartupGrace is how long the loop waits after being started
	// before it looks for its first task. The hook that starts it fires from a
	// single login's connect path, but the loop is bridge-wide: the grace
	// gives every other login in the process time to finish Connect, so a task
	// belonging to a login that hasn't connected yet isn't dispatched while
	// its client is still nil. (bridgev2's getPortalAndDoBackfillTask parks a
	// task on BackfillNextDispatchNever when no logged-in login owns the
	// portal; that is recoverable — EnsureExists re-arms exactly that sentinel
	// — but it is pointless churn to walk into.)
	synapseDrainStartupGrace = 2 * time.Minute

	// synapseDrainMaxStartupWait bounds the extra wait for the initial forward
	// backfill burst to finish (see the ready func). Deep-history backfill is
	// the lowest-priority work in the bridge, and running it against the same
	// single-writer SQLite connection while the bootstrap forward pass is
	// still delivering is how conversations got stranded before; but a counter
	// that never reaches zero must not postpone the drain forever.
	synapseDrainMaxStartupWait = 30 * time.Minute

	// synapseDrainReadyPoll is the poll interval for that wait.
	synapseDrainReadyPoll = 5 * time.Second

	// synapseDrainMinBatchDelay floors backfill.queue.batch_delay. mautrix's
	// example config ships 20s, but nothing in corten sets the key, so a
	// config generated without it yields 0 — and a 0 delay turns the loop into
	// a GetNext busy-poll against a database that is deliberately serialized to
	// one connection. (Upstream RunBackfillQueue has this hole too: its
	// noTasksFoundCount is initialized and reset but never incremented, so its
	// documented empty-queue backoff never actually applies. This loop
	// increments it.)
	synapseDrainMinBatchDelay = 5 * time.Second

	// synapseDrainStopPollInterval is the longest this loop sleeps without
	// re-checking Bridge.IsStopping. Bridge.stop() sets that flag first, then
	// disconnects logins, and only cancels BackgroundCtx at the very end, so
	// polling it is what keeps the loop from starting a backfill against
	// clients that are already being torn down.
	synapseDrainStopPollInterval = 5 * time.Second
)

// synapseBackfillDrainer is the drain loop for one bridge.
type synapseBackfillDrainer struct {
	br  *bridgev2.Bridge
	log zerolog.Logger

	batchDelay     time.Duration
	startupGrace   time.Duration
	maxStartupWait time.Duration
	readyPoll      time.Duration

	// ready reports whether the login that started the loop has finished its
	// initial forward backfill burst. nil means "don't wait for anything".
	ready func() bool

	// getNext and doTask are the bridgev2 queue API, behind fields so tests can
	// drive the loop without a full bridge database. Production always gets the
	// defaults assigned in startSynapseBackfillDrain.
	getNext func(context.Context) (*database.BackfillTask, error)
	doTask  func(context.Context, *database.BackfillTask)

	cancel context.CancelFunc
	// done is closed when the loop goroutine returns.
	done chan struct{}
}

// The registry is what makes the loop bridge-scoped rather than login-scoped.
//
// The start hook runs from a per-login connect path, and Connect is re-invoked
// on reconnect, relogin and session restore; bridgev2's StartLogins also calls
// it once per UserLogin row in the database. Every one of those would spawn a
// loop, and two loops share one `backfill_task` table: both would call GetNext,
// both would get the same row (ORDER BY next_dispatch_min_ts LIMIT 1) in the
// window before MarkDispatched lands, and the same history would be delivered
// twice into the same room.
//
// Keyed by *Bridge rather than a package-level sync.Once for two reasons: a
// test process (or any future in-process multi-bridge host) has more than one
// bridge, and a sync.Once can never re-fire — so a bridge that is stopped and
// started again in one process would silently never get its loop back. The
// entry is deleted when the loop exits, which makes a restart work and keeps
// the map from pinning a dead bridge.
var (
	synapseDrainMu      sync.Mutex
	synapseDrainRunning = map[*bridgev2.Bridge]*synapseBackfillDrainer{}
)

// synapseDrainOption tweaks a drainer before its goroutine starts. Only tests
// pass these; production uses the values derived from the bridge config.
type synapseDrainOption func(*synapseBackfillDrainer)

// startSynapseBackfillDrainIfNeeded starts the bridge-wide drain loop if this
// homeserver needs it and nothing has started it yet. Safe to call from every
// login on every connect.
func (c *IMClient) startSynapseBackfillDrainIfNeeded(opts ...synapseDrainOption) *synapseBackfillDrainer {
	if c == nil || c.Main == nil || c.Main.Bridge == nil {
		return nil
	}
	// Hold off the first pass until this login's bootstrap forward backfills
	// have drained. The counter is per-login and the loop is bridge-wide, so
	// this only covers the login that got there first — which on this bridge's
	// one-account-per-process deployment is the only one there is.
	ready := func() bool { return atomic.LoadInt64(&c.pendingInitialBackfills) <= 0 }
	log := c.Main.Bridge.Log.With().Str("component", "synapse backfill queue").Logger()
	return startSynapseBackfillDrain(c.Main.Bridge, log, ready, opts...)
}

// startSynapseBackfillDrain returns the running drainer for br — starting it
// first if needed — or nil if this bridge must not run one.
func startSynapseBackfillDrain(
	br *bridgev2.Bridge,
	log zerolog.Logger,
	ready func() bool,
	opts ...synapseDrainOption,
) *synapseBackfillDrainer {
	if reason := synapseDrainSkipReason(br); reason != "" {
		log.Debug().Str("reason", reason).Msg("Not starting the backward backfill drain loop")
		return nil
	}

	synapseDrainMu.Lock()
	defer synapseDrainMu.Unlock()
	if existing := synapseDrainRunning[br]; existing != nil {
		log.Debug().Msg("Backward backfill drain loop is already running for this bridge")
		return existing
	}

	// Bridge.stop() cancels BackgroundCtx as its last act, so it is the
	// bridge-wide shutdown signal. Deriving from it (rather than from a login's
	// stopChan) is what keeps one login's Disconnect from killing a loop the
	// whole bridge shares.
	base := br.BackgroundCtx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)

	batchDelay := time.Duration(br.Config.Backfill.Queue.BatchDelay) * time.Second
	if batchDelay < synapseDrainMinBatchDelay {
		batchDelay = synapseDrainMinBatchDelay
	}
	d := &synapseBackfillDrainer{
		br:             br,
		log:            log,
		batchDelay:     batchDelay,
		startupGrace:   synapseDrainStartupGrace,
		maxStartupWait: synapseDrainMaxStartupWait,
		readyPoll:      synapseDrainReadyPoll,
		ready:          ready,
		cancel:         cancel,
		done:           make(chan struct{}),
	}
	d.getNext = func(ctx context.Context) (*database.BackfillTask, error) {
		return br.DB.BackfillTask.GetNext(ctx)
	}
	d.doTask = func(ctx context.Context, task *database.BackfillTask) {
		// Same two lines as RunBackfillQueue. FromQueue only affects
		// AllowSlowFetch (which corten's FetchMessages ignores) and one log
		// branch, but mirroring the real queue keeps the two paths honest.
		task.FromQueue = true
		br.DoBackfillTask(ctx, task)
	}
	for _, opt := range opts {
		opt(d)
	}

	synapseDrainRunning[br] = d
	go d.run(ctx)
	return d
}

// synapseDrainSkipReason returns "" if the drain loop should run for br, or a
// human-readable reason why it must not.
func synapseDrainSkipReason(br *bridgev2.Bridge) string {
	if br == nil || br.Config == nil || br.Matrix == nil || br.DB == nil {
		return "bridge is not fully initialized"
	}
	caps := br.Matrix.GetCapabilities()
	if caps == nil {
		return "Matrix capabilities are not known yet"
	}
	if caps.BatchSending {
		// The whole point of this loop is to cover for RunBackfillQueue. When
		// RunBackfillQueue is running, starting this one would mean two
		// dispatchers on one table.
		return "homeserver supports batch sending, so bridgev2 runs its own backfill queue"
	}
	cfg := &br.Config.Backfill
	if !cfg.Enabled {
		return "backfill is disabled in config"
	}
	if !cfg.Queue.Enabled {
		return "the backfill queue is disabled in config"
	}
	// Same expression FetchMessages short-circuits on: with a capped
	// max_initial_messages every backward request returns HasMore=false
	// immediately, so there is genuinely nothing to drain. Not starting is
	// cheaper than draining a queue whose every task is a no-op.
	if cfg.MaxInitialMessages < math.MaxInt32 {
		return "max_initial_messages is capped, so backward backfill is short-circuited"
	}
	return ""
}

func (d *synapseBackfillDrainer) run(ctx context.Context) {
	defer close(d.done)
	defer d.unregister()
	defer d.cancel()

	d.log.Info().
		Stringer("batch_delay", d.batchDelay).
		Stringer("startup_grace", d.startupGrace).
		Msg("Homeserver doesn't support batch sending — starting the connector's own backward backfill drain loop")
	defer d.log.Info().Msg("Backward backfill drain loop stopped")

	if !d.waitForStart(ctx) {
		return
	}

	emptyPasses := 0
	for {
		if d.stopping(ctx) {
			return
		}
		task, err := d.getNext(ctx)
		switch {
		case err != nil:
			d.log.Err(err).Msg("Failed to get next backward backfill task")
			if !d.sleep(ctx, bridgev2.BackfillQueueErrorBackoff) {
				return
			}
		case task == nil:
			emptyPasses++
			if !d.sleep(ctx, d.emptyBackoff(emptyPasses)) {
				return
			}
		default:
			emptyPasses = 0
			// Re-check immediately before dispatching: between GetNext and
			// here the bridge may have started shutting down, and a task
			// dispatched then would find its login already disconnected and
			// park the portal on BackfillNextDispatchNever.
			if d.stopping(ctx) {
				return
			}
			d.doTask(ctx, task)
			// DoBackfillTask has already pushed this portal's
			// next_dispatch_min_ts out by batch_delay; this delay is the
			// bridge-wide pacing between any two backfill batches.
			if !d.sleep(ctx, d.batchDelay) {
				return
			}
		}
	}
}

// waitForStart holds the loop off until it is reasonable to start dispatching.
// Returns false if the bridge stopped while waiting.
func (d *synapseBackfillDrainer) waitForStart(ctx context.Context) bool {
	if !d.sleep(ctx, d.startupGrace) {
		return false
	}
	if d.ready == nil {
		return true
	}
	deadline := time.Now().Add(d.maxStartupWait)
	for !d.ready() {
		if !time.Now().Before(deadline) {
			d.log.Warn().
				Stringer("waited", d.maxStartupWait).
				Msg("Initial forward backfill still hasn't finished — starting backward backfill anyway")
			return true
		}
		if !d.sleep(ctx, d.readyPoll) {
			return false
		}
	}
	return true
}

// emptyBackoff mirrors what RunBackfillQueue documents for an empty queue:
// batch_delay, plus batch_delay per consecutive empty pass, capped.
func (d *synapseBackfillDrainer) emptyBackoff(emptyPasses int) time.Duration {
	extra := d.batchDelay * time.Duration(emptyPasses)
	if extra > bridgev2.BackfillQueueMaxEmptyBackoff {
		extra = bridgev2.BackfillQueueMaxEmptyBackoff
	}
	return d.batchDelay + extra
}

// stopping reports whether the bridge is going away.
func (d *synapseBackfillDrainer) stopping(ctx context.Context) bool {
	return ctx.Err() != nil || d.br.IsStopping()
}

// sleep waits for dur, in slices short enough to notice IsStopping. Returns
// false if the bridge stopped (in which case the caller must return).
func (d *synapseBackfillDrainer) sleep(ctx context.Context, dur time.Duration) bool {
	deadline := time.Now().Add(dur)
	for {
		if d.stopping(ctx) {
			return false
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return true
		}
		if remaining > synapseDrainStopPollInterval {
			remaining = synapseDrainStopPollInterval
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

func (d *synapseBackfillDrainer) unregister() {
	synapseDrainMu.Lock()
	defer synapseDrainMu.Unlock()
	// Only clear the slot if it is still ours: a restart may already have
	// registered a successor.
	if synapseDrainRunning[d.br] == d {
		delete(synapseDrainRunning, d.br)
	}
}
