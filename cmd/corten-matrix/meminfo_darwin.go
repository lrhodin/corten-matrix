//go:build darwin

package main

import "golang.org/x/sys/unix"

// totalSystemMemory returns total physical RAM via the hw.memsize sysctl.
// Returns 0 on failure.
func totalSystemMemory() uint64 {
	v, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return v
}
