// Package internetprobe provides the bridge's Apple-independent Internet
// reachability check and recovery-stability tracker.
package internetprobe

import (
	"context"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

const (
	CloudflareTarget = "1.1.1.1"
	GoogleTarget     = "8.8.8.8"

	pingTimeout = 3 * time.Second
)

// Runner executes one reachability probe for a numeric IP address.
type Runner func(context.Context, string) bool

// Result records the outcome of both Apple-independent public probes.
type Result struct {
	Cloudflare bool
	Google     bool
}

func (r Result) Reachable() bool {
	return r.Cloudflare || r.Google
}

// Probe pings both public targets and reports each result.
func Probe(ctx context.Context) Result {
	return ProbeWith(ctx, ping)
}

// Reachable pings both public targets and reports whether either replied.
func Reachable(ctx context.Context) bool {
	return Probe(ctx).Reachable()
}

// ProbeWith is Probe with an injectable runner for deterministic tests.
// Both probes are always launched, even if one returns successfully first.
func ProbeWith(ctx context.Context, runner Runner) Result {
	type targetResult struct {
		target    string
		reachable bool
	}
	targets := [...]string{CloudflareTarget, GoogleTarget}
	results := make(chan targetResult, len(targets))
	var wg sync.WaitGroup
	wg.Add(len(targets))
	for _, target := range targets {
		go func() {
			defer wg.Done()
			results <- targetResult{target: target, reachable: runner(ctx, target)}
		}()
	}
	wg.Wait()
	close(results)

	var result Result
	for probe := range results {
		switch probe.target {
		case CloudflareTarget:
			result.Cloudflare = probe.reachable
		case GoogleTarget:
			result.Google = probe.reachable
		}
	}
	return result
}

// ReachableWith is Reachable with an injectable runner for deterministic tests.
func ReachableWith(ctx context.Context, runner Runner) bool {
	return ProbeWith(ctx, runner).Reachable()
}

func ping(ctx context.Context, target string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	command := "ping"
	args := []string{"-n", "-c", "1", target}
	switch runtime.GOOS {
	case "darwin":
		// launchd's PATH is intentionally sparse; use macOS's stable path.
		command = "/sbin/ping"
	case "windows":
		args = []string{"-n", "1", target}
	}
	return exec.CommandContext(probeCtx, command, args...).Run() == nil
}

// Stability tracks successful reachability samples using caller-provided
// monotonic time values. A failed sample always starts the window over.
type Stability struct {
	since time.Time
}

// Observe records one sample and reports whether successful samples span the
// requested stability window.
func (s *Stability) Observe(now time.Time, reachable bool, window time.Duration) bool {
	if !reachable {
		s.since = time.Time{}
		return false
	}
	if s.since.IsZero() {
		s.since = now
		return window <= 0
	}
	return now.Sub(s.since) >= window
}

// Reset discards any partial stability window.
func (s *Stability) Reset() {
	s.since = time.Time{}
}
