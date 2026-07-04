//go:build !linux && !darwin

package main

// totalSystemMemory can't be determined on this platform; return 0 so the
// caller skips setting a GOMEMLIMIT.
func totalSystemMemory() uint64 { return 0 }
