package internetprobe

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestProbeWithReportsEachPublicTarget(t *testing.T) {
	result := ProbeWith(context.Background(), func(_ context.Context, target string) bool {
		return target == GoogleTarget
	})
	if result.Cloudflare {
		t.Fatal("Cloudflare result = reachable, want unreachable")
	}
	if !result.Google {
		t.Fatal("Google result = unreachable, want reachable")
	}
	if !result.Reachable() {
		t.Fatal("either successful target should establish reachability")
	}
}

func TestReachableWithProbesBothPublicTargetsAndAcceptsEither(t *testing.T) {
	var mu sync.Mutex
	called := make(map[string]int)
	runner := func(_ context.Context, target string) bool {
		mu.Lock()
		called[target]++
		mu.Unlock()
		return target == GoogleTarget
	}

	if !ReachableWith(context.Background(), runner) {
		t.Fatal("one successful public target should establish reachability")
	}
	if called[CloudflareTarget] != 1 || called[GoogleTarget] != 1 {
		t.Fatalf("probe calls = %#v, want one call to each public target", called)
	}
}

func TestReachableWithRejectsWhenBothTargetsFail(t *testing.T) {
	if ReachableWith(context.Background(), func(context.Context, string) bool { return false }) {
		t.Fatal("both failed public targets must report offline")
	}
}

func TestStabilityRequiresFullContinuousWindow(t *testing.T) {
	const window = 60 * time.Second
	start := time.Unix(1_000, 0)
	var stability Stability

	if stability.Observe(start, true, window) {
		t.Fatal("first successful sample must start, not complete, stabilization")
	}
	if stability.Observe(start.Add(window-time.Second), true, window) {
		t.Fatal("stability completed before the full window")
	}
	if !stability.Observe(start.Add(window), true, window) {
		t.Fatal("stability did not complete at the full window")
	}
}

func TestStabilityFailureResetsWindow(t *testing.T) {
	const window = 60 * time.Second
	start := time.Unix(2_000, 0)
	var stability Stability

	stability.Observe(start, true, window)
	stability.Observe(start.Add(50*time.Second), false, window)
	if stability.Observe(start.Add(70*time.Second), true, window) {
		t.Fatal("first success after failure must begin a new window")
	}
	if stability.Observe(start.Add(129*time.Second), true, window) {
		t.Fatal("reset window completed too early")
	}
	if !stability.Observe(start.Add(130*time.Second), true, window) {
		t.Fatal("reset window did not complete after 60 seconds")
	}
}
