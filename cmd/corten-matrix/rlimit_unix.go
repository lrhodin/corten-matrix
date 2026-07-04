//go:build darwin || linux

package main

import (
	"fmt"
	"os"
	"syscall"
)

// desiredFileLimit is the open-file soft limit we want the bridge to run with.
// 65536 is comfortably below the per-process kernel maximum on both macOS
// (kern.maxfilesperproc, typically 184320) and Linux (fs.nr_open, typically
// 1048576), while giving a long-running bridge enormous headroom.
const desiredFileLimit = 65536

// raiseFileLimit raises the process's open-file-descriptor soft limit.
//
// Service managers start the bridge with a low default soft RLIMIT_NOFILE —
// macOS launchd uses just 256, and a plain Linux/systemd default is 1024. A
// busy bridge keeps many descriptors open at once (the APNs connection, IDS
// HTTP requests, the appservice websocket — which the homeserver also tunnels
// HTTP through — the SQLite pool, CardDAV) and during bursts like backfilling a
// large history can blow past a small ceiling. Once the limit is hit, every new
// socket fails with "too many open files": the appservice websocket can't
// reopen after its (normal) hourly server-side recycle and APNs can't
// re-establish, so the bridge silently stops passing messages while staying
// alive — until it is restarted (which resets the count). Heavy-message
// accounts hit this first.
//
// A process may only raise its soft limit up to its hard limit, so this is
// best-effort: on macOS the hard limit is effectively unlimited and the higher
// target always takes; on Linux the systemd unit's LimitNOFILE governs the hard
// cap and this call lifts the soft limit to match (falling back to the
// historical OPEN_MAX of 10240). On any failure we log and continue rather than
// abort. Safe above 1024 because the Go and tokio runtimes poll with
// epoll/kqueue, not select().
func raiseFileLimit() {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		fmt.Fprintf(os.Stderr, "[rlimit] failed to read RLIMIT_NOFILE: %v\n", err)
		return
	}
	if lim.Cur >= desiredFileLimit {
		// Already raised (service-manager limit applied, or operator override).
		return
	}
	old := lim.Cur
	for _, target := range []uint64{desiredFileLimit, 10240} {
		if target <= old {
			continue
		}
		if lim.Max != 0 && target > lim.Max {
			continue
		}
		lim.Cur = target
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err == nil {
			fmt.Fprintf(os.Stderr, "[rlimit] raised open-file limit %d → %d\n", old, target)
			return
		}
	}
	fmt.Fprintf(os.Stderr, "[rlimit] could not raise open-file limit above %d "+
		"(raise the service LimitNOFILE; launchd default is 256, systemd 1024); "+
		"the bridge may exhaust descriptors over long uptime\n", old)
}
