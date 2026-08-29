package connector

import (
	"context"
	"errors"
	"testing"
)

func TestWaitForForwardBackfillSlotCancellationSchedulesRetry(t *testing.T) {
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	retryScheduled := false
	err := waitForForwardBackfillSlot(ctx, sem, func() {
		retryScheduled = true
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForForwardBackfillSlot error = %v, want context.Canceled", err)
	}
	if !retryScheduled {
		t.Fatal("cancelled semaphore wait did not schedule a forward-backfill retry")
	}
	if len(sem) != 1 {
		t.Fatalf("semaphore occupancy = %d, want 1", len(sem))
	}
}

func TestWaitForForwardBackfillSlotAcquiresWithoutRetry(t *testing.T) {
	sem := make(chan struct{}, 1)
	retryScheduled := false
	if err := waitForForwardBackfillSlot(context.Background(), sem, func() {
		retryScheduled = true
	}); err != nil {
		t.Fatalf("waitForForwardBackfillSlot: %v", err)
	}
	if retryScheduled {
		t.Fatal("successful semaphore acquisition scheduled a retry")
	}
	if len(sem) != 1 {
		t.Fatalf("semaphore occupancy = %d, want 1", len(sem))
	}
}

func TestWaitForForwardBackfillSlotAlreadyCancelledDoesNotAcquire(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for i := 0; i < 100; i++ {
		sem := make(chan struct{}, 1)
		retryScheduled := false
		err := waitForForwardBackfillSlot(ctx, sem, func() {
			retryScheduled = true
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("attempt %d: error = %v, want context.Canceled", i, err)
		}
		if !retryScheduled {
			t.Fatalf("attempt %d: retry was not scheduled", i)
		}
		if len(sem) != 0 {
			t.Fatalf("attempt %d: cancelled call acquired semaphore", i)
		}
	}
}

func TestRunForwardBackfillRetryReleasesAccountingWhenRejected(t *testing.T) {
	released := false
	runForwardBackfillRetry(func() bool { return false }, func() { released = true })
	if !released {
		t.Fatal("rejected retry did not release forward-backfill accounting")
	}
}

func TestRunForwardBackfillRetryTransfersAccountingWhenAccepted(t *testing.T) {
	released := false
	runForwardBackfillRetry(func() bool { return true }, func() { released = true })
	if released {
		t.Fatal("accepted retry released accounting before the retry completed")
	}
}
