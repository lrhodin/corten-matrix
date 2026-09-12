package connector

// Round-9 audit: the regression test the audit delivered failing under -race.
// It passes now that needsRetirement takes one snapshot under disconnectMu and
// Connect installs c.client under the same lock; assertions are unchanged.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lrhodin/corten-matrix/pkg/rustpushgo"
)

// ROUND-9 FINDING. needsRetirement() (client.go) is the predicate a load path
// uses to decide whether the IMClient bridgev2 already holds must be retired:
//
//	return c.client != nil || c.connectEpochActive() || c.hasOrphanedRecovery()
//
// The second and third members take disconnectMu, precisely so the epoch fields
// they read are ordered against Connect's writes and against disconnect(). The
// FIRST member takes no lock, yet c.client is written by Connect
// (`c.client = client`, under lifecycleMu only) and nil'd by disconnect() (under
// disconnectMu only). needsRetirement is called from the LOAD goroutine
// (IMConnector.LoadUserLogin / installReLoginClient -> retirePreviousClient)
// while the previous client's Connect is still running — that is the entire
// reason round 8 added the connectEpochActive() member — so the two accesses
// are genuinely concurrent with no happens-before edge between them.
//
// Consequence: an unsynchronized read of a pointer word. Benign in practice on
// the amd64/arm64 the bridge ships on, but it is a Go memory-model race, the
// race detector reports it, and it means the one predicate that exists to be
// read across goroutines reads one of its three inputs without the lock its two
// siblings take. It also makes the predicate three separately-timed reads
// rather than one snapshot. The fix is to read c.client under disconnectMu
// (Connect's commit-point write has to move under it too).
//
// Each iteration is one load-path/Connect race. Repeated because race
// detection over a ~20ms window is probabilistic per attempt; at this count a
// clean run means the race is genuinely gone, not that it was missed.
func TestNeedsRetirementReadsTheRustClientWithoutALock(t *testing.T) {
	const iterations = 40
	for i := range iterations {
		_, _, _ = installLifecycleSeams(t)
		client := newLifecycleTestClient()

		// Connect must not run past its FIRST commit point: the dummy Rust
		// client below would reach FFI. Flagging termination inside the seam is
		// how connect_lifecycle_test.go drives this same window.
		newRustClient = func(*rustpushgo.WrappedApsConnection, *rustpushgo.WrappedIdsUsers, *rustpushgo.WrappedIdsngmIdentity, *rustpushgo.WrappedOsConfig, **rustpushgo.WrappedTokenProvider, rustpushgo.MessageCallback, rustpushgo.UpdateUsersCallback) (*rustpushgo.Client, error) {
			// Stands in for the up-to-300s NewClient window a load path races.
			time.Sleep(2 * time.Millisecond)
			client.lifecycleTerminated.Store(true)
			return &rustpushgo.Client{}, nil
		}

		stopReaders := make(chan struct{})
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Exactly what retirePreviousClient does, from the load goroutine.
				for {
					select {
					case <-stopReaders:
						return
					default:
					}
					_ = client.needsRetirement()
				}
			}()
		}

		client.Connect(context.Background())
		close(stopReaders)
		wg.Wait()

		// Detach the dummy before any teardown can reach FFI.
		client.client = nil
		client.Disconnect()
		if t.Failed() {
			t.Logf("race reported on iteration %d", i)
			return
		}
	}
}
