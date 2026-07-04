//go:build !darwin && !linux

package main

// raiseFileLimit is a no-op on platforms without the syscall.Rlimit path used
// in rlimit_unix.go (e.g. Windows). On macOS and Linux the real implementation
// raises the open-file soft limit at startup.
func raiseFileLimit() {}
