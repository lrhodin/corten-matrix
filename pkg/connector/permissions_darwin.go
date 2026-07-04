// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

//go:build darwin && !ios

package connector

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"
	_ "github.com/mattn/go-sqlite3"
)

func canReadChatDB(log zerolog.Logger) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Warn().Err(err).Msg("canReadChatDB: failed to get home directory")
		return false
	}
	dbPath := filepath.Join(home, "Library", "Messages", "chat.db")
	if _, err := os.Stat(dbPath); err != nil {
		log.Warn().Err(err).Str("path", dbPath).Msg("canReadChatDB: chat.db not found")
		return false
	}
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		log.Warn().Err(err).Str("path", dbPath).Msg("canReadChatDB: failed to open chat.db")
		return false
	}
	defer db.Close()
	_, err = db.Query("SELECT 1 FROM message LIMIT 1")
	if err != nil {
		if isPermissionError(err) {
			log.Warn().Err(err).Str("path", dbPath).Msg("canReadChatDB: permission denied querying chat.db (Full Disk Access not granted?)")
		} else {
			log.Warn().Err(err).Str("path", dbPath).Msg("canReadChatDB: test query failed")
		}
		return false
	}
	log.Debug().Str("path", dbPath).Msg("canReadChatDB: chat.db is accessible")
	return true
}

func isPermissionError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "operation not permitted") ||
		strings.Contains(msg, "permission denied")
}

func showDialogAndOpenFDA(log zerolog.Logger) {
	log.Warn().Msg("Full Disk Access not granted — prompting user")
	script := `display dialog "corten-matrix needs Full Disk Access to read your iMessage history for backfill.\n\nClick OK to open System Settings, then enable the toggle for corten-matrix." with title "iMessage Bridge" buttons {"OK"} default button "OK"`
	exec.Command("osascript", "-e", script).Run()
	exec.Command("open", "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles").Run()
}

func waitForFDA(log zerolog.Logger) {
	for !canReadChatDB(log) {
		time.Sleep(2 * time.Second)
	}
	log.Info().Msg("Full Disk Access restored")
}
