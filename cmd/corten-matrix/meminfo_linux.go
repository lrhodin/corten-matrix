//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// totalSystemMemory returns the memory ceiling the bridge should size its Go
// heap limit against: the smaller of host RAM and any cgroup memory limit, so
// it does the right thing inside a memory-capped Docker container where host
// RAM is misleading. Returns 0 if it can't be determined.
func totalSystemMemory() uint64 {
	var host uint64
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err == nil {
		host = uint64(info.Totalram) * uint64(info.Unit)
	}
	if limit := cgroupMemoryLimit(); limit > 0 && (host == 0 || limit < host) {
		return limit
	}
	return host
}

// cgroupMemoryLimit reads the container memory limit from cgroup v2 then v1.
// A near-max sentinel (or the literal "max") means unlimited — return 0 so the
// caller falls back to host RAM.
func cgroupMemoryLimit() uint64 {
	const unlimitedThreshold = uint64(1) << 62
	for _, p := range []string{
		"/sys/fs/cgroup/memory.max",                   // cgroup v2
		"/sys/fs/cgroup/memory/memory.limit_in_bytes", // cgroup v1
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(b))
		if s == "max" {
			return 0
		}
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil || v == 0 || v >= unlimitedThreshold {
			continue
		}
		return v
	}
	return 0
}
