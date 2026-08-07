package connector

import "testing"

func TestBackwardBackfillWaitsForForwardCompletionWithoutRetrying(t *testing.T) {
	if got := backwardBackfillShouldWaitForForward(false, true); !got {
		t.Fatal("expected backward backfill to wait while forward backfill is incomplete")
	}
	if got := backwardBackfillShouldWaitForForward(true, true); got {
		t.Fatal("did not expect a wait after forward backfill completed")
	}
	if got := backwardBackfillShouldWaitForForward(false, false); got {
		t.Fatal("did not expect a wait when the portal has no messages")
	}
}
