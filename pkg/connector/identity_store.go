// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/rs/zerolog"

	"github.com/lrhodin/corten-matrix/pkg/rustpushgo"
)

// PersistedSessionState holds all the session data that needs to survive
// database resets (DB deletion, config wipes, etc.). Persisted to a JSON file
// at ~/.local/share/corten-matrix/session.json.
//
// On re-authentication, the bridge reads this file to reuse:
//   - IDSIdentity: cryptographic device keys (avoids new key generation)
//   - APSState: APS push connection state (preserves push token)
//   - IDSUsers: IDS registration data (avoids calling register() endpoint)
//
// Together these prevent Apple from treating re-login as a "new device",
// which would trigger "X added a new Mac" notifications to contacts.
type PersistedSessionState struct {
	IDSIdentity     string `json:"ids_identity,omitempty"`
	APSState        string `json:"aps_state,omitempty"`
	IDSUsers        string `json:"ids_users,omitempty"`
	PreferredHandle string `json:"preferred_handle,omitempty"`

	// Login platform and device identity (needed for auto-restore on Linux)
	Platform    string `json:"platform,omitempty"`
	HardwareKey string `json:"hardware_key,omitempty"`
	DeviceID    string `json:"device_id,omitempty"`

	// iCloud account persist data (for TokenProvider restoration across restarts)
	AccountUsername          string `json:"account_username,omitempty"`
	AccountHashedPasswordHex string `json:"account_hashed_password_hex,omitempty"`
	AccountPET               string `json:"account_pet,omitempty"`
	AccountADSID             string `json:"account_adsid,omitempty"`
	AccountDSID              string `json:"account_dsid,omitempty"`
	AccountSPDBase64         string `json:"account_spd_base64,omitempty"`

	// Cached MobileMe delegate for seeding on restore
	MmeDelegateJSON string `json:"mme_delegate_json,omitempty"`

	// Opaque IDS delivery-key cache (base64). Bookkeeping that rides alongside
	// the registration data and is preserved across saves (see saveSessionState).
	IDSKeyCache string `json:"ids_key_cache,omitempty"`
}

// sessionFilePath returns the path to the persisted session state file:
// ~/.local/share/corten-matrix/session.json
func sessionFilePath() (string, error) {
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataDir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataDir, "corten-matrix", "session.json"), nil
}

// legacyIdentityFilePath returns the old v1 identity file path for migration:
// ~/.local/share/corten-matrix/identity.plist
func legacyIdentityFilePath() (string, error) {
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataDir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataDir, "corten-matrix", "identity.plist"), nil
}

// trustedPeersFilePath returns the keychain trust state path:
// ~/.local/share/corten-matrix/trustedpeers.plist
func trustedPeersFilePath() (string, error) {
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataDir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataDir, "corten-matrix", "trustedpeers.plist"), nil
}

// hasKeychainCliqueState returns true if trustedpeers.plist appears to contain
// a keychain user identity (i.e. trust circle has been joined).
func hasKeychainCliqueState(log zerolog.Logger) bool {
	path, err := trustedPeersFilePath()
	if err != nil {
		log.Debug().Err(err).Msg("Failed to determine trusted peers file path")
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	// trustedpeers.plist is written by Rust as XML plist, where a joined clique
	// includes either userIdentity or user_identity key.
	if bytes.Contains(data, []byte("<key>userIdentity</key>")) || bytes.Contains(data, []byte("<key>user_identity</key>")) {
		return true
	}
	log.Info().Str("path", path).Msg("Trusted peers state exists but has no user identity (not in clique)")
	return false
}

// sessionStateMu serializes read-modify-write of session.json across the bridge.
// Several code paths persist it (login, registration/auth events, periodic state
// updates). os.WriteFile is not atomic across concurrent writers, so without this
// two saves could interleave into corrupt JSON or clobber each other's fields.
var sessionStateMu sync.Mutex

// saveSessionState writes the full session state to the JSON file (locked).
func saveSessionState(log zerolog.Logger, state PersistedSessionState) {
	sessionStateMu.Lock()
	defer sessionStateMu.Unlock()
	saveSessionStateLocked(log, state)
}

// saveSessionStateLocked is saveSessionState without the lock; callers must hold
// sessionStateMu. Creates parent directories if needed. Errors are logged, not fatal.
func saveSessionStateLocked(log zerolog.Logger, state PersistedSessionState) {
	path, err := sessionFilePath()
	if err != nil {
		log.Warn().Err(err).Msg("Failed to determine session file path, skipping save")
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		log.Warn().Err(err).Str("path", path).Msg("Failed to create session file directory")
		return
	}
	// Preserve the opaque IDS key cache across saves. Callers that rebuild the
	// struct from login metadata don't carry it, so re-read it from the existing
	// file when the incoming state doesn't set it — otherwise every metadata-
	// driven save would drop the cached blob.
	if state.IDSKeyCache == "" {
		if existing, rerr := os.ReadFile(path); rerr == nil && len(existing) > 0 {
			var prev PersistedSessionState
			if json.Unmarshal(existing, &prev) == nil {
				state.IDSKeyCache = prev.IDSKeyCache
			}
		}
	}
	data, err := json.Marshal(state)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to marshal session state")
		return
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		log.Warn().Err(err).Str("path", path).Msg("Failed to write session file")
		return
	}
	log.Info().Str("path", path).
		Bool("has_identity", state.IDSIdentity != "").
		Bool("has_aps_state", state.APSState != "").
		Bool("has_ids_users", state.IDSUsers != "").
		Msg("Saved session state to file")
}

// loadSessionState reads the persisted session state from the JSON file.
// Falls back to the legacy identity.plist file (v1 format) if the new file
// doesn't exist. Returns a zero-value struct if nothing is found.
func loadSessionState(log zerolog.Logger) PersistedSessionState {
	sessionStateMu.Lock()
	defer sessionStateMu.Unlock()
	return loadSessionStateLocked(log)
}

// loadSessionStateLocked is loadSessionState without the lock; callers must hold
// sessionStateMu.
func loadSessionStateLocked(log zerolog.Logger) PersistedSessionState {
	// Try new JSON format first
	path, err := sessionFilePath()
	if err != nil {
		log.Debug().Err(err).Msg("Failed to determine session file path")
		return PersistedSessionState{}
	}
	data, err := os.ReadFile(path)
	if err == nil && len(data) > 0 {
		var state PersistedSessionState
		if err := json.Unmarshal(data, &state); err != nil {
			log.Warn().Err(err).Str("path", path).Msg("Failed to parse session file")
			return PersistedSessionState{}
		}
		log.Info().Str("path", path).
			Bool("has_identity", state.IDSIdentity != "").
			Bool("has_aps_state", state.APSState != "").
			Bool("has_ids_users", state.IDSUsers != "").
			Msg("Loaded session state from file")
		return state
	}

	// Fall back to legacy identity.plist (v1 format — identity only)
	legacyPath, err := legacyIdentityFilePath()
	if err != nil {
		return PersistedSessionState{}
	}
	legacyData, err := os.ReadFile(legacyPath)
	if err != nil || len(legacyData) == 0 {
		return PersistedSessionState{}
	}
	log.Info().Str("path", legacyPath).Msg("Migrating legacy identity file to new session format")
	state := PersistedSessionState{
		IDSIdentity: string(legacyData),
	}
	// Migrate: save in new format and remove old file (already under the lock)
	saveSessionStateLocked(log, state)
	_ = os.Remove(legacyPath)
	return state
}

// ListHandles returns the available iMessage handles (phone numbers and
// email addresses) from the backup session state. Returns nil if no valid
// session state is found. Intended for CLI use (list-handles subcommand).
// Reads session.json directly to avoid any logger output.
func ListHandles() []string {
	// InitLogger must be called before any Rust FFI (NewWrappedIdsUsers).
	// Without it the Rust side has no logger and may panic on Linux.
	rustpushgo.InitLogger()

	path, err := sessionFilePath()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil
	}
	var state PersistedSessionState
	if err := json.Unmarshal(data, &state); err != nil || state.IDSUsers == "" {
		return nil
	}
	users := rustpushgo.NewWrappedIdsUsers(&state.IDSUsers)
	return users.GetHandles()
}

// CheckSessionRestore validates that backup session state (session.json +
// keystore) exists and the IDS user keys are present in the keystore.
// Returns true if login can be auto-restored without re-authentication.
// This is intended to be called from the CLI (check-restore subcommand)
// before starting the bridge.
func CheckSessionRestore() bool {
	log := zerolog.New(zerolog.NewConsoleWriter()).With().Timestamp().Logger()

	// Initialize keystore (loads from XDG path, migrates if needed)
	rustpushgo.InitLogger()

	state := loadSessionState(log)
	if state.IDSUsers == "" || state.IDSIdentity == "" || state.APSState == "" {
		return false
	}

	session := &cachedSessionState{
		IDSIdentity: state.IDSIdentity,
		APSState:    state.APSState,
		IDSUsers:    state.IDSUsers,
		source:      "backup file (check-restore)",
	}
	if !session.validate(log) {
		return false
	}
	if !hasKeychainCliqueState(log) {
		log.Info().Msg("Session restore check failed: keychain trust circle not initialized")
		return false
	}
	return true
}
