package connector

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// fakeMatrixConnector answers GetCapabilities and nothing else. The embedded
// nil interface makes every other method of bridgev2.MatrixConnector panic if
// the drain loop ever reaches for one, which is the assertion we want: this
// loop must only ever ask whether the server can batch send.
type fakeMatrixConnector struct {
	bridgev2.MatrixConnector
	caps *bridgev2.MatrixCapabilities
}

func (f *fakeMatrixConnector) GetCapabilities() *bridgev2.MatrixCapabilities {
	return f.caps
}

// newDrainTestBridge builds the smallest *bridgev2.Bridge the drain loop's
// start gate reads: Matrix capabilities, backfill config, a non-nil DB (the
// loop's database access goes through the seams) and a cancelable
// BackgroundCtx — which is the same field Bridge.stop() cancels.
func newDrainTestBridge(t *testing.T, batchSending bool) (*bridgev2.Bridge, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	br := &bridgev2.Bridge{
		ID:     "imessage",
		Log:    zerolog.Nop(),
		DB:     &database.Database{},
		Matrix: &fakeMatrixConnector{caps: &bridgev2.MatrixCapabilities{BatchSending: batchSending}},
		Config: &bridgeconfig.BridgeConfig{
			Backfill: bridgeconfig.BackfillConfig{
				Enabled:            true,
				MaxInitialMessages: math.MaxInt32,
				Queue: bridgeconfig.BackfillQueueConfig{
					Enabled:    true,
					BatchSize:  10000,
					MaxBatches: -1,
					BatchDelay: 20,
				},
			},
		},
		BackgroundCtx: ctx,
	}
	t.Cleanup(func() {
		// Never leave a registry entry behind for a later test to trip over.
		cancel()
		synapseDrainMu.Lock()
		d := synapseDrainRunning[br]
		synapseDrainMu.Unlock()
		if d != nil {
			<-d.done
		}
	})
	return br, cancel
}

// drainRecorder is the test double for the two bridgev2 queue calls. It also
// detects the failure this whole feature has to avoid: two loops dispatching
// from one queue.
type drainRecorder struct {
	mu sync.Mutex
	// tasks holds every portal ID handed to doTask, in order.
	tasks []string
	// pending is the queue GetNext serves from.
	pending []*database.BackfillTask

	getNextCalls int32
	concurrent   int32
	// overlaps counts how many times more than one goroutine was inside
	// getNext at once.
	overlaps int32
	// entered is closed after the first getNext call.
	entered     chan struct{}
	enteredOnce sync.Once
}

func newDrainRecorder(portalIDs ...string) *drainRecorder {
	r := &drainRecorder{entered: make(chan struct{})}
	for _, id := range portalIDs {
		r.pending = append(r.pending, &database.BackfillTask{
			PortalKey:   networkid.PortalKey{ID: networkid.PortalID(id), Receiver: "login"},
			UserLoginID: "login",
		})
	}
	return r
}

func (r *drainRecorder) getNext(context.Context) (*database.BackfillTask, error) {
	if atomic.AddInt32(&r.concurrent, 1) > 1 {
		atomic.AddInt32(&r.overlaps, 1)
	}
	defer atomic.AddInt32(&r.concurrent, -1)
	atomic.AddInt32(&r.getNextCalls, 1)
	r.enteredOnce.Do(func() { close(r.entered) })
	// Give a second loop, if one exists, a real chance to overlap with this one.
	time.Sleep(time.Millisecond)

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) == 0 {
		return nil, nil
	}
	task := r.pending[0]
	r.pending = r.pending[1:]
	return task, nil
}

func (r *drainRecorder) do(_ context.Context, task *database.BackfillTask) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tasks = append(r.tasks, string(task.PortalKey.ID))
}

func (r *drainRecorder) delivered() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.tasks...)
}

// fastDrainOpts makes the loop run immediately and tightly. Production timings
// (a 2-minute grace, a 20-second batch delay) are asserted separately in
// TestSynapseDrainUsesGentleProductionTimings.
func fastDrainOpts(r *drainRecorder) []synapseDrainOption {
	return []synapseDrainOption{
		func(d *synapseBackfillDrainer) {
			d.startupGrace = 0
			d.maxStartupWait = 0
			d.readyPoll = time.Millisecond
			d.batchDelay = time.Millisecond
			d.getNext = r.getNext
			d.doTask = r.do
		},
	}
}

func newDrainTestClient(br *bridgev2.Bridge) *IMClient {
	return &IMClient{Main: &IMConnector{Bridge: br}}
}

// TestSynapseDrainNotStartedWhenBatchSendingAvailable is the no-double-dispatch
// guarantee: on a server where bridgev2's own RunBackfillQueue runs, this loop
// must not exist at all.
func TestSynapseDrainNotStartedWhenBatchSendingAvailable(t *testing.T) {
	br, _ := newDrainTestBridge(t, true)
	rec := newDrainRecorder("portal-1")

	if d := newDrainTestClient(br).startSynapseBackfillDrainIfNeeded(fastDrainOpts(rec)...); d != nil {
		t.Fatal("drain loop started even though the homeserver supports batch sending")
	}
	synapseDrainMu.Lock()
	registered := len(synapseDrainRunning)
	synapseDrainMu.Unlock()
	if registered != 0 {
		t.Fatalf("registry has %d entries, want 0", registered)
	}
	// Nothing may have been dispatched, and no goroutine may be waiting to.
	time.Sleep(20 * time.Millisecond)
	if n := atomic.LoadInt32(&rec.getNextCalls); n != 0 {
		t.Fatalf("getNext called %d times with batch sending available, want 0", n)
	}
}

// TestSynapseDrainSkipReasons covers the rest of the start gate.
func TestSynapseDrainSkipReasons(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*bridgev2.Bridge)
		want   bool // true = should start
	}{
		{"synapse defaults", func(*bridgev2.Bridge) {}, true},
		{"backfill disabled", func(br *bridgev2.Bridge) { br.Config.Backfill.Enabled = false }, false},
		{"queue disabled", func(br *bridgev2.Bridge) { br.Config.Backfill.Queue.Enabled = false }, false},
		{
			// FetchMessages short-circuits every backward request in this
			// configuration, so there is nothing for the loop to do.
			"max_initial_messages capped",
			func(br *bridgev2.Bridge) { br.Config.Backfill.MaxInitialMessages = 5000 },
			false,
		},
		{
			"capabilities unknown",
			func(br *bridgev2.Bridge) { br.Matrix = &fakeMatrixConnector{caps: nil} },
			false,
		},
		{"no database", func(br *bridgev2.Bridge) { br.DB = nil }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			br, _ := newDrainTestBridge(t, false)
			tt.mutate(br)
			rec := newDrainRecorder()
			d := newDrainTestClient(br).startSynapseBackfillDrainIfNeeded(fastDrainOpts(rec)...)
			if (d != nil) != tt.want {
				t.Fatalf("started = %v, want %v (reason: %q)", d != nil, tt.want, synapseDrainSkipReason(br))
			}
		})
	}
}

// TestSynapseDrainStartsOnceForTwoLogins is the duplicate-delivery regression
// guard. Two logins share one Bridge, one database and one backfill_task table;
// each calls the start hook from its own Connect, concurrently. Exactly one
// loop must exist, and the one queued task must be delivered exactly once.
func TestSynapseDrainStartsOnceForTwoLogins(t *testing.T) {
	br, _ := newDrainTestBridge(t, false)
	rec := newDrainRecorder("portal-1")

	loginA := newDrainTestClient(br)
	loginB := newDrainTestClient(br)

	var wg sync.WaitGroup
	drainers := make([]*synapseBackfillDrainer, 2)
	for i, c := range []*IMClient{loginA, loginB} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			drainers[i] = c.startSynapseBackfillDrainIfNeeded(fastDrainOpts(rec)...)
		}()
	}
	wg.Wait()

	if drainers[0] == nil || drainers[1] == nil {
		t.Fatalf("both logins must get a drainer back, got %v and %v", drainers[0], drainers[1])
	}
	if drainers[0] != drainers[1] {
		// Not Fatal: the duplicate-delivery assertions below are the point of
		// the test, and they are worth seeing when this one breaks.
		t.Error("the two logins started two different drain loops — they would race on the same backfill_task rows")
	}
	synapseDrainMu.Lock()
	registered := len(synapseDrainRunning)
	synapseDrainMu.Unlock()
	if registered != 1 {
		t.Fatalf("registry has %d entries, want exactly 1", registered)
	}

	// A third call, standing in for Connect being re-invoked on reconnect,
	// relogin or session restore, must reuse the same loop.
	if again := loginA.startSynapseBackfillDrainIfNeeded(fastDrainOpts(rec)...); again != drainers[0] {
		t.Error("reconnect spawned a second drain loop")
	}

	waitForDelivery(t, rec, 1)
	// Let the loops (if there were more than one) keep spinning for a while.
	time.Sleep(50 * time.Millisecond)
	if got := rec.delivered(); len(got) != 1 || got[0] != "portal-1" {
		t.Fatalf("task delivery = %v, want exactly one delivery of portal-1", got)
	}
	if n := atomic.LoadInt32(&rec.overlaps); n != 0 {
		t.Fatalf("two goroutines were inside getNext at once %d times", n)
	}
}

// TestSynapseDrainStopsOnBridgeShutdown proves the loop ends with the bridge
// and does no work afterwards. Cancelling BackgroundCtx is exactly what
// Bridge.stop() does (bridge.go: cancelBackgroundCtx()).
func TestSynapseDrainStopsOnBridgeShutdown(t *testing.T) {
	br, cancel := newDrainTestBridge(t, false)
	rec := newDrainRecorder()

	d := newDrainTestClient(br).startSynapseBackfillDrainIfNeeded(fastDrainOpts(rec)...)
	if d == nil {
		t.Fatal("drain loop did not start on a homeserver without batch sending")
	}
	select {
	case <-rec.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("drain loop never polled the queue")
	}

	cancel() // == Bridge.stop()
	select {
	case <-d.done:
	case <-time.After(5 * time.Second):
		t.Fatal("drain loop did not stop after the bridge shut down")
	}

	// No work after shutdown: the poll count must be frozen.
	after := atomic.LoadInt32(&rec.getNextCalls)
	time.Sleep(50 * time.Millisecond)
	if now := atomic.LoadInt32(&rec.getNextCalls); now != after {
		t.Fatalf("getNext ran %d more times after shutdown", now-after)
	}
	if got := rec.delivered(); len(got) != 0 {
		t.Fatalf("tasks delivered after shutdown: %v", got)
	}
	// The registry must not pin a dead bridge.
	synapseDrainMu.Lock()
	_, stillRegistered := synapseDrainRunning[br]
	synapseDrainMu.Unlock()
	if stillRegistered {
		t.Fatal("drainer stayed in the registry after its loop exited")
	}
}

// TestSynapseDrainRestartsAfterShutdown is why the registry isn't a sync.Once:
// a bridge that stops and starts again in one process must get its loop back.
func TestSynapseDrainRestartsAfterShutdown(t *testing.T) {
	br, cancel := newDrainTestBridge(t, false)
	rec := newDrainRecorder()
	client := newDrainTestClient(br)

	first := client.startSynapseBackfillDrainIfNeeded(fastDrainOpts(rec)...)
	if first == nil {
		t.Fatal("drain loop did not start")
	}
	cancel()
	<-first.done

	ctx, cancel2 := context.WithCancel(context.Background())
	t.Cleanup(cancel2)
	br.BackgroundCtx = ctx
	second := client.startSynapseBackfillDrainIfNeeded(fastDrainOpts(rec)...)
	if second == nil {
		t.Fatal("drain loop did not restart after the bridge was restarted")
	}
	if second == first {
		t.Fatal("restart reused the dead drainer")
	}
	cancel2()
	<-second.done
}

// TestSynapseDrainWaitsForForwardBackfill checks the constraint that keeps deep
// history off the single SQLite writer while the bootstrap forward pass is
// still delivering.
func TestSynapseDrainWaitsForForwardBackfill(t *testing.T) {
	br, _ := newDrainTestBridge(t, false)
	rec := newDrainRecorder()
	client := newDrainTestClient(br)
	atomic.StoreInt64(&client.pendingInitialBackfills, 3)

	opts := append(fastDrainOpts(rec), func(d *synapseBackfillDrainer) {
		d.maxStartupWait = 10 * time.Second
	})
	d := client.startSynapseBackfillDrainIfNeeded(opts...)
	if d == nil {
		t.Fatal("drain loop did not start")
	}
	time.Sleep(30 * time.Millisecond)
	if n := atomic.LoadInt32(&rec.getNextCalls); n != 0 {
		t.Fatalf("drained %d times while forward backfill was still pending, want 0", n)
	}

	atomic.StoreInt64(&client.pendingInitialBackfills, 0)
	select {
	case <-rec.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("drain loop never started after forward backfill finished")
	}
}

// TestSynapseDrainUsesGentleProductionTimings pins the defaults that keep the
// loop off the database: no busy-polling when batch_delay is missing from the
// config, and a growing, capped backoff while the queue is empty.
func TestSynapseDrainUsesGentleProductionTimings(t *testing.T) {
	br, _ := newDrainTestBridge(t, false)
	br.Config.Backfill.Queue.BatchDelay = 0 // key absent from config.yaml

	// Don't let the loop run; only inspect what start built.
	rec := newDrainRecorder()
	d := newDrainTestClient(br).startSynapseBackfillDrainIfNeeded(func(d *synapseBackfillDrainer) {
		d.getNext = rec.getNext
		d.doTask = rec.do
	})
	if d == nil {
		t.Fatal("drain loop did not start")
	}
	if d.batchDelay < synapseDrainMinBatchDelay {
		t.Fatalf("batch delay = %s, want at least %s — a zero delay busy-polls the database",
			d.batchDelay, synapseDrainMinBatchDelay)
	}
	if d.startupGrace != synapseDrainStartupGrace {
		t.Fatalf("startup grace = %s, want %s", d.startupGrace, synapseDrainStartupGrace)
	}
	if first, second := d.emptyBackoff(1), d.emptyBackoff(2); second <= first {
		t.Fatalf("empty-queue backoff didn't grow: %s then %s", first, second)
	}
	if capped := d.emptyBackoff(1 << 20); capped > d.batchDelay+bridgev2.BackfillQueueMaxEmptyBackoff {
		t.Fatalf("empty-queue backoff = %s, want it capped at %s",
			capped, d.batchDelay+bridgev2.BackfillQueueMaxEmptyBackoff)
	}
	// The loop is still inside its 2-minute grace; the test bridge's cleanup
	// cancels BackgroundCtx and waits for it.
	if n := atomic.LoadInt32(&rec.getNextCalls); n != 0 {
		t.Fatalf("drained %d times during the startup grace, want 0", n)
	}
}

func waitForDelivery(t *testing.T, rec *drainRecorder, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(rec.delivered()) >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("only %d tasks delivered, want %d", len(rec.delivered()), want)
}
