// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

//go:build darwin && !ios

package main

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/lrhodin/corten-matrix/imessage/mac"
)

// dialog shows a macOS dialog and returns true if the user clicked the
// default button. The optional second button is always "Quit".
func dialog(title, msg string) bool {
	script := fmt.Sprintf(
		`display dialog %q with title %q buttons {"Quit","OK"} default button "OK"`,
		msg, title,
	)
	err := exec.Command("osascript", "-e", script).Run()
	return err == nil // user clicked OK
}

func dialogInfo(title, msg string) {
	script := fmt.Sprintf(
		`display dialog %q with title %q buttons {"OK"} default button "OK"`,
		msg, title,
	)
	exec.Command("osascript", "-e", script).Run()
}

func canReadChatDB() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	dbPath := filepath.Join(home, "Library", "Messages", "chat.db")
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		return false
	}
	defer db.Close()
	_, err = db.Query("SELECT 1 FROM message LIMIT 1")
	return err == nil
}

func requestContacts() (bool, error) {
	cs := mac.NewContactStore()
	err := cs.RequestContactAccess()
	if err != nil {
		return false, err
	}
	return cs.HasContactAccess, nil
}

// runSetupPermissions checks and prompts for FDA and Contacts permissions
// using native macOS dialogs.  It does NOT install the LaunchAgent — the
// install script handles that.
func runSetupPermissions() {
	title := "iMessage Bridge Setup"

	// ── Step 1: Full Disk Access ─────────────────────────────────
	if !canReadChatDB() {
		if !dialog(title, "Full Disk Access is required to read iMessages.\n\nClick OK, then add this app in the window that opens.") {
			os.Exit(0)
		}
		exec.Command("open", "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles").Run()
		for !canReadChatDB() {
			time.Sleep(2 * time.Second)
		}
		dialogInfo(title, "✓ Full Disk Access granted.")
	}

	// ── Step 2: Contacts ─────────────────────────────────────────
	granted, err := requestContacts()
	if err != nil {
		dialog(title, fmt.Sprintf("Contacts access error: %v\n\nPlease grant access in System Settings → Privacy & Security → Contacts.", err))
	} else if !granted {
		dialog(title, "Contacts access was denied.\n\nPlease enable it in System Settings → Privacy & Security → Contacts, then restart.")
	}

	status := "✓ Full Disk Access granted\n"
	if granted {
		status += "✓ Contacts access granted\n"
	} else {
		status += "⚠ Contacts access not granted (names won't resolve)\n"
	}
	dialogInfo(title, status)
}

func isSetupMode() bool {
	for _, arg := range os.Args[1:] {
		if strings.TrimLeft(arg, "-") == "setup" {
			return true
		}
	}
	return false
}


