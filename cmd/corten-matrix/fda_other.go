//go:build !darwin

package main

import (
	"os"
	"path/filepath"
)

// fdaCheck on non-macOS just probes chat.db directly — there is no TCC / Full
// Disk Access there, and chat.db is macOS-only, so this path is effectively
// unused (the install scripts only call fda-check on Darwin).
func fdaCheck() int {
	home, _ := os.UserHomeDir()
	f, err := os.Open(filepath.Join(home, "Library", "Messages", "chat.db"))
	if err != nil {
		return 1
	}
	_ = f.Close()
	return 0
}
