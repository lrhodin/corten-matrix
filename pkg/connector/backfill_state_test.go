package connector

import "testing"

func ptrTo[T any](v T) *T { return &v }

// The wait decision is the guard on a permanent write: a backward task that does
// not wait gets is_done=true, and getNextBackfillQuery never returns a done task
// again. So every case here is really asking "is it safe to give up on this
// portal's history forever".
func TestBackwardBackfillShouldWaitForForward(t *testing.T) {
	tests := []struct {
		name        string
		forwardDone *bool
		hasMessages bool
		want        bool
	}{{
		name:        "forward incomplete with history waits",
		forwardDone: ptrTo(false),
		hasMessages: true,
		want:        true,
	}, {
		name:        "forward complete proceeds to backfill",
		forwardDone: ptrTo(true),
		hasMessages: true,
		want:        false,
	}, {
		// An unreadable flag is not evidence forward backfill finished. Waiting
		// costs a delay that self-heals; not waiting costs the history.
		name:        "unknown forward state waits",
		forwardDone: nil,
		hasMessages: true,
		want:        true,
	}, {
		// Nothing for forward backfill to anchor against, so a wait would never
		// end — this is the case that must NOT wait.
		name:        "no messages does not wait even when forward is incomplete",
		forwardDone: ptrTo(false),
		hasMessages: false,
		want:        false,
	}, {
		name:        "no messages does not wait when forward state is unknown",
		forwardDone: nil,
		hasMessages: false,
		want:        false,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := backwardBackfillShouldWaitForForward(tt.forwardDone, tt.hasMessages); got != tt.want {
				t.Fatalf("backwardBackfillShouldWaitForForward(%v, %v) = %v, want %v",
					tt.forwardDone, tt.hasMessages, got, tt.want)
			}
		})
	}
}
