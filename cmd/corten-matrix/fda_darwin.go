//go:build darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// fdaCheck probes chat.db from a transient launchd job, so the bridge binary
// itself is the process macOS TCC evaluates — the same context the installed
// service runs in. A probe run straight from Terminal is attributed to Terminal
// (which usually already has Full Disk Access), so the bridge never gets denied
// and never appears in the Full Disk Access list. Running it under launchd makes
// it its own responsible process, so the denied access registers it in the list
// for the user to grant. Returns 0 if chat.db is readable, 1 otherwise.
func fdaCheck() int {
	self, err := os.Executable()
	if err != nil {
		return 1
	}
	uid := strconv.Itoa(os.Getuid())
	const label = "com.lrhodin.corten-matrix.fdaprobe"
	result := filepath.Join(os.TempDir(), label+"."+strconv.Itoa(os.Getpid()))
	_ = os.Remove(result)

	// Keep the transient probe plist OUT of ~/Library/LaunchAgents — launchctl
	// bootstrap loads it from any path, and writing it there makes it flash in
	// and out of the user's agents folder during setup. A temp path is invisible.
	plistPath := filepath.Join(os.TempDir(), label+".plist")
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>` + label + `</string>
  <key>ProgramArguments</key><array><string>` + self + `</string><string>fda-probe</string><string>` + result + `</string></array>
  <key>RunAtLoad</key><true/>
  <key>ProcessType</key><string>Background</string>
</dict></plist>`
	if os.WriteFile(plistPath, []byte(plist), 0o644) != nil {
		return 1
	}
	defer os.Remove(plistPath)

	_ = exec.Command("launchctl", "bootout", "gui/"+uid+"/"+label).Run()
	if err := exec.Command("launchctl", "bootstrap", "gui/"+uid, plistPath).Run(); err != nil {
		_ = exec.Command("launchctl", "load", plistPath).Run()
	}

	code := 1
	for i := 0; i < 60; i++ { // up to ~6s for the probe to write its result
		if b, err := os.ReadFile(result); err == nil && len(b) > 0 {
			if b[0] == '0' {
				code = 0
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = exec.Command("launchctl", "bootout", "gui/"+uid+"/"+label).Run()
	_ = os.Remove(result)
	return code
}
