package main

import (
	"fmt"
	"math"
	"os"
	"runtime/debug"
)

// memLimitFraction is the share of detected system memory we let the Go heap
// use as a soft cap (GOMEMLIMIT). The remainder is headroom for allocations
// Go's GC does NOT govern — the Rust/uniffi FFI (CloudKit attachment downloads,
// image/video buffers) and the ffmpeg subprocess — plus the OS itself. Leaving
// ~20% keeps the soft limit useful without starving those.
const memLimitFraction = 0.80

// capMemoryLimitFromSystem sets a soft Go heap limit (GOMEMLIMIT) sized to the
// host's memory when the operator hasn't set one explicitly.
//
// On a small, swap-less box an aggressive backfill can drive the resident set
// past physical memory and get the whole process OOM-killed by the kernel
// (observed in the wild: a ~6 GB VPS with zero swap, RSS climbing past 5 GB
// mid-backfill, reaped and restarted in a loop). A GOMEMLIMIT makes the Go GC
// run harder as the heap approaches the cap instead of letting it grow
// unbounded between collections — trading some CPU for not being reaped.
//
// It is a SOFT limit and shapes the Go heap only, not FFI/ffmpeg memory, so it
// complements rather than replaces raising RAM or adding swap.
func capMemoryLimitFromSystem() {
	// Respect an explicit operator GOMEMLIMIT. Go applies the env var at
	// startup; SetMemoryLimit(-1) reads the current value without changing it.
	// The unset default is math.MaxInt64 — anything else means it was set.
	if cur := debug.SetMemoryLimit(-1); cur != math.MaxInt64 {
		fmt.Fprintf(os.Stderr, "[memlimit] GOMEMLIMIT already set (%d MiB), leaving as-is\n", cur/(1<<20))
		return
	}
	total := totalSystemMemory()
	if total == 0 {
		// Couldn't detect (unsupported platform or read failure) — don't guess.
		fmt.Fprintln(os.Stderr, "[memlimit] could not detect system memory; not setting GOMEMLIMIT")
		return
	}
	limit := int64(float64(total) * memLimitFraction)
	debug.SetMemoryLimit(limit)
	fmt.Fprintf(os.Stderr, "[memlimit] system memory %d MiB → GOMEMLIMIT %d MiB (%.0f%%); override with the GOMEMLIMIT env var\n",
		total/(1<<20), limit/(1<<20), memLimitFraction*100)
}
