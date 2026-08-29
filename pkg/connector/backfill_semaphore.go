package connector

import "context"

// waitForForwardBackfillSlot acquires a forward-backfill slot or schedules a
// retry before propagating cancellation. The retry callback must not depend on
// ctx because it runs after the cancelled request has unwound.
func waitForForwardBackfillSlot(ctx context.Context, sem chan struct{}, scheduleRetry func()) error {
	// A select chooses randomly when both cases are ready. Check first so an
	// already-cancelled request never acquires a newly-available slot and then
	// proceeds without scheduling its replacement.
	if err := ctx.Err(); err != nil {
		scheduleRetry()
		return err
	}
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		scheduleRetry()
		return ctx.Err()
	}
}

// runForwardBackfillRetry transfers ownership of the outstanding bootstrap
// accounting slot to an accepted retry. If the retry cannot be accepted, the
// original attempt remains terminal and must release that slot itself.
func runForwardBackfillRetry(queueRetry func() bool, releaseAccounting func()) {
	if !queueRetry() {
		releaseAccounting()
	}
}
