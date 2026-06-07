// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/id"

	"github.com/lrhodin/imessage/pkg/rustpushgo"
)

func isRunningOnMacOS() bool {
	return runtime.GOOS == "darwin"
}

type IMConnector struct {
	Bridge *bridgev2.Bridge
	Config IMConfig
}

var _ bridgev2.NetworkConnector = (*IMConnector)(nil)

func (c *IMConnector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:          "iMessage",
		NetworkURL:           "https://support.apple.com/messages",
		NetworkIcon:          "mxc://maunium.net/tManJEpANASZvDVzvRvhILdl",
		NetworkID:            "imessage",
		BeeperBridgeType:     "imessagego",
		DefaultPort:          29332,
		DefaultCommandPrefix: "!im",
	}
}

func (c *IMConnector) Init(bridge *bridgev2.Bridge) {
	c.Bridge = bridge
}

func (c *IMConnector) Start(ctx context.Context) error {
	// Latch the DEVELOPMENT-ONLY privacy-disable switch into the package-global
	// read by the free log helpers (logSafeHandle/logSafeURL). Done here in
	// Start() because Init() runs before the config YAML is loaded, and before
	// tryAutoRestore below (which logs handles). See IMConfig.DebugDisablePrivacy.
	debugDisablePrivacy = c.Config.DebugDisablePrivacy
	if debugDisablePrivacy {
		c.Bridge.Log.Warn().Msg("debug_disable_privacy is ENABLED — log anonymization and the body scrubber are OFF, and plaintext will be re-pulled into the local DB. This is a development-only setting; do not use it in production.")
	}

	// Enable bridgev2's unknown-error auto-reconnect so the receive-wedge
	// watchdog's StateUnknownError actually rebuilds the client. bridgev2 treats
	// any value < 1min as "disabled" (bridgestate.go waitForUnknownErrorReconnect),
	// and the shipped/base config defaults this to null → 0 → the watchdog would
	// be a silent no-op. Default it when unset; an explicit larger value is kept.
	// Set here (not Init) because the config YAML is loaded before Start.
	if c.Bridge.Config.UnknownErrorAutoReconnect < time.Minute {
		c.Bridge.Config.UnknownErrorAutoReconnect = 5 * time.Minute
		c.Bridge.Log.Info().Msg("Defaulting unknown_error_auto_reconnect to 5m so the APNs receive-wedge watchdog can rebuild the client")
	}
	// bridgev2 counts unknown-error reconnects against UnknownErrorMaxAutoReconnects
	// and NEVER resets the counter in-process (bridgestate.go: incremented only).
	// The default (0/low) exhausts the budget after ~1 rebuild → the watchdog
	// silently reverts to manual-restart-only on a long-uptime deploy. Raise it so
	// recovery survives many wedges. Apple-safe even at this count: rebuilds are
	// rate-limited (10-min wedge threshold + 5-min reconnect delay ⇒ ≤~4/hr) AND
	// kept LIGHT (the 20+-handle IDS invite sweep + FT pre-mint are gated by the
	// full-connect cooldown), so even a long run of rebuilds stays far below any
	// abuse threshold; the finite bound still stops infinite churn on a truly
	// permanent failure. Counter refreshes on process restart.
	if c.Bridge.Config.UnknownErrorMaxAutoReconnects < 100 {
		c.Bridge.Config.UnknownErrorMaxAutoReconnects = 100
	}

	// iMessage's primary identifier IS the phone number, and the Matrix client
	// (Beeper) gates the call button — and treats a contact as callable/phone —
	// on a tel: identifier in the ghost's profile. bridgev2 STRIPS every tel:
	// from profiles unless this is set (ghost.go prepareContactInfo deletes
	// tel: when !PhoneNumbersInProfile). With the default (false), phone-only
	// contacts ended up email-only in their profile ("defaulting to email", no
	// call button) even though GetUserInfo includes the phone. Force it on:
	// exposing the phone is the whole point of an iMessage contact, and it's
	// what makes the call button work. Set in Start() (config YAML is loaded by
	// now); existing ghosts repopulate the tel: on their next contact refresh.
	if !c.Bridge.Config.PhoneNumbersInProfile {
		c.Bridge.Config.PhoneNumbersInProfile = true
		c.Bridge.Log.Info().Msg("Forcing phone_numbers_in_profile=true so contact phone numbers stay in Matrix profiles (call button + contact resolution)")
	}

	// Override backfill defaults for iMessage CloudKit sync.
	// Applied in Start() because Init() runs before config YAML is loaded.
	// Only apply when CloudKit backfill is enabled — otherwise leave the
	// mautrix defaults alone (backfill won't be used).
	if c.Config.CloudKitBackfill {
		// The mautrix defaults (max_initial_messages=50, batch_size=100) are too
		// low — CloudKit chats can have tens of thousands of messages, and many
		// small backward batch_send requests create fragmented DAG branches that
		// clients can't paginate through. High max_initial_messages ensures all
		// messages are delivered in one forward batch during room creation.
		cfg := &c.Bridge.Config.Backfill
		if !cfg.Enabled {
			cfg.Enabled = true
		}
		if cfg.MaxInitialMessages < 100 {
			cfg.MaxInitialMessages = math.MaxInt32 // uncapped — backfill everything CloudKit downloaded
		}
		// Catchup should match the initial cap — unlimited when uncapped,
		// capped when the user caps max_initial_messages.
		cfg.MaxCatchupMessages = cfg.MaxInitialMessages
		if !cfg.Queue.Enabled {
			cfg.Queue.Enabled = true
		}
		if cfg.Queue.BatchSize <= 100 {
			cfg.Queue.BatchSize = 10000
		}
		if cfg.MaxInitialMessages < math.MaxInt32 {
			// User explicitly capped initial messages — disable backward
			// backfill so the cap is the final word on message count.
			cfg.Queue.MaxBatches = 0
		} else if cfg.Queue.MaxBatches == 0 {
			cfg.Queue.MaxBatches = -1
		}
	}

	// Auto-restore: if the DB has no logins but we have valid backup session
	// state (session.json + keystore), create a user_login from the backup
	// instead of requiring a full re-login.
	c.tryAutoRestore(ctx)

	return nil
}

// tryAutoRestore checks if the database is empty but valid session state
// exists in the backup files.  If so, it creates a user_login entry from
// the backup, avoiding the need for a full Apple ID re-authentication.
func (c *IMConnector) tryAutoRestore(ctx context.Context) {
	log := c.Bridge.Log.With().Str("component", "imessage").Logger()

	// Only restore if there are no existing logins.
	usersWithLogins, err := c.Bridge.DB.UserLogin.GetAllUserIDsWithLogins(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to check existing logins for auto-restore")
		return
	}
	if len(usersWithLogins) > 0 {
		return // DB already has logins, nothing to restore
	}

	// Check for backup session state
	state := loadSessionState(log)
	if state.IDSUsers == "" || state.IDSIdentity == "" || state.APSState == "" {
		log.Debug().Msg("No complete backup session state found, skipping auto-restore")
		return
	}

	// Validate against keystore
	rustpushgo.InitLogger()
	session := &cachedSessionState{
		IDSIdentity: state.IDSIdentity,
		APSState:    state.APSState,
		IDSUsers:    state.IDSUsers,
		source:      "backup file (auto-restore)",
	}
	if !session.validate(log) {
		log.Info().Msg("Backup session state failed keystore validation, skipping auto-restore")
		return
	}
	// Chat.db mode doesn't join the keychain clique (no CloudKit), so
	// trustedpeers.plist is never written. Only require clique state
	// when CloudKit backfill is active.
	if c.Config.UseCloudKitBackfill() && !hasKeychainCliqueState(log) {
		log.Info().Msg("Skipping auto-restore: keychain trust circle not initialized (will require interactive login)")
		return
	}

	// Extract login ID and username from the cached IDS users
	users := rustpushgo.NewWrappedIdsUsers(&state.IDSUsers)
	loginID := networkid.UserLoginID(users.LoginId(0))
	if loginID == "" {
		log.Warn().Msg("Backup session has no login ID, skipping auto-restore")
		return
	}

	handles := users.GetHandles()
	username := string(loginID)
	if len(handles) > 0 {
		username = handles[0]
	}

	// Find the admin user to attach this login to
	adminMXID := ""
	for userID, perm := range c.Bridge.Config.Permissions {
		if perm.Admin {
			adminMXID = userID
			break
		}
	}
	if adminMXID == "" {
		log.Warn().Msg("No admin user in config, skipping auto-restore")
		return
	}

	user, err := c.Bridge.GetUserByMXID(ctx, id.UserID(adminMXID))
	if err != nil {
		log.Warn().Err(err).Msg("Failed to get admin user for auto-restore")
		return
	}

	log.Info().
		Str("login_id", string(loginID)).
		Str("username", username).
		Msg("Auto-restoring login from backup session state")

	platform := state.Platform
	if platform == "" {
		platform = runtime.GOOS
	}

	meta := &UserLoginMetadata{
		Platform:                 platform,
		HardwareKey:              state.HardwareKey,
		DeviceID:                 state.DeviceID,
		ChatsSynced:              false,
		APSState:                 state.APSState,
		IDSUsers:                 state.IDSUsers,
		IDSIdentity:              state.IDSIdentity,
		AccountUsername:          state.AccountUsername,
		AccountHashedPasswordHex: state.AccountHashedPasswordHex,
		AccountPET:               state.AccountPET,
		AccountADSID:             state.AccountADSID,
		AccountDSID:              state.AccountDSID,
		AccountSPDBase64:         state.AccountSPDBase64,
		MmeDelegateJSON:          state.MmeDelegateJSON,
	}

	_, err = user.NewLogin(ctx, &database.UserLogin{
		ID:         loginID,
		RemoteName: username,
		RemoteProfile: status.RemoteProfile{
			Name: username,
		},
		Metadata: meta,
	}, &bridgev2.NewLoginParams{
		DeleteOnConflict: true,
	})
	if err != nil {
		log.Err(err).Msg("Failed to auto-restore login from backup")
		return
	}

	log.Info().Str("login_id", string(loginID)).Msg("Successfully auto-restored login from backup session state")
}

func (c *IMConnector) GetLoginFlows() []bridgev2.LoginFlow {
	flows := []bridgev2.LoginFlow{}
	if isRunningOnMacOS() {
		flows = append(flows, bridgev2.LoginFlow{
			Name:        "Apple ID",
			Description: "Log in with your Apple ID to send and receive iMessages",
			ID:          LoginFlowIDAppleID,
		})
	}
	flows = append(flows, bridgev2.LoginFlow{
		Name:        "Apple ID (External Key)",
		Description: "Log in using a hardware key extracted from a Mac. Works on any platform.",
		ID:          LoginFlowIDExternalKey,
	})
	return flows
}

func (c *IMConnector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	switch flowID {
	case LoginFlowIDAppleID:
		if !isRunningOnMacOS() {
			return nil, fmt.Errorf("Apple ID login requires macOS. Use 'External Key' login on other platforms.")
		}
		return &AppleIDLogin{User: user, Main: c}, nil
	case LoginFlowIDExternalKey:
		return &ExternalKeyLogin{User: user, Main: c}, nil
	default:
		return nil, fmt.Errorf("unknown login flow: %s", flowID)
	}
}

func (c *IMConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	meta := login.Metadata.(*UserLoginMetadata)
	log := c.Bridge.Log.With().Str("component", "imessage").Logger()

	rustpushgo.InitLogger()

	// If this login already has a live client, this is a reconnect/recreate
	// (not first startup) — fully disconnect it BEFORE opening a new APNs
	// connection on the same device token. bridgev2's recreateClient does NOT
	// disconnect the old client (it only reassigns login.Client), so without
	// this the old connection lingers and the new one becomes a duplicate that
	// Apple drops ("early eof"), which rustpush's no-backoff reconnect loop
	// turns into the self-sustaining receive-stall storm. Disconnect closes the
	// old connection and stops its goroutines, so the rebuild starts clean.
	if existing, ok := login.Client.(*IMClient); ok && existing != nil && existing.client != nil {
		log.Info().Msg("LoadUserLogin: disconnecting existing client before reconnect (avoids a duplicate APNs connection on the same device token)")
		existing.Disconnect()
	}

	var cfg *rustpushgo.WrappedOsConfig
	var err error

	if meta.HardwareKey != "" {
		// Cross-platform mode: use hardware key with open-absinthe NAC emulation.
		if meta.DeviceID != "" {
			cfg, err = rustpushgo.CreateConfigFromHardwareKeyWithDeviceId(meta.HardwareKey, meta.DeviceID)
		} else {
			cfg, err = rustpushgo.CreateConfigFromHardwareKey(meta.HardwareKey)
		}
	} else if isRunningOnMacOS() {
		// Local macOS mode: use IOKit + AAAbsintheContext.
		if meta.DeviceID != "" {
			cfg, err = rustpushgo.CreateLocalMacosConfigWithDeviceId(meta.DeviceID)
		} else {
			cfg, err = rustpushgo.CreateLocalMacosConfig()
		}
	} else {
		return fmt.Errorf("no hardware key configured and not running on macOS — re-login with 'External Key' flow")
	}
	if err != nil {
		return fmt.Errorf("failed to create config: %w", err)
	}

	usersStr := &meta.IDSUsers
	identityStr := &meta.IDSIdentity
	apsStateStr := &meta.APSState

	// Eagerly persist full session state to the backup file so it survives DB resets.
	//
	// Guard against overwriting a good backup with empty state. client.Connect
	// (client.go ValidateKeystore path) wipes meta.IDSUsers/IDSIdentity/APSState
	// from the DB when the keystore is missing and flips to StateBadCredentials;
	// on the NEXT LoadUserLogin the meta is empty here, and without this guard
	// we'd blow away session.json — escalating a recoverable key-loss into a
	// full re-auth because tryAutoRestore on a future boot now finds no backup.
	if meta.IDSUsers == "" && meta.IDSIdentity == "" && meta.APSState == "" {
		log.Warn().Msg("LoadUserLogin: meta has no IDSUsers/IDSIdentity/APSState; skipping session.json overwrite to preserve existing backup")
	} else {
		saveSessionState(log, PersistedSessionState{
			IDSIdentity:              meta.IDSIdentity,
			APSState:                 meta.APSState,
			IDSUsers:                 meta.IDSUsers,
			PreferredHandle:          meta.PreferredHandle,
			Platform:                 meta.Platform,
			HardwareKey:              meta.HardwareKey,
			DeviceID:                 meta.DeviceID,
			AccountUsername:          meta.AccountUsername,
			AccountHashedPasswordHex: meta.AccountHashedPasswordHex,
			AccountPET:               meta.AccountPET,
			AccountADSID:             meta.AccountADSID,
			AccountDSID:              meta.AccountDSID,
			AccountSPDBase64:         meta.AccountSPDBase64,
			MmeDelegateJSON:          meta.MmeDelegateJSON,
		})
	}

	client := &IMClient{
		Main:                    c,
		UserLogin:               login,
		config:                  cfg,
		users:                   rustpushgo.NewWrappedIdsUsers(usersStr),
		identity:                rustpushgo.NewWrappedIdsngmIdentity(identityStr),
		connection:              rustpushgo.Connect(cfg, rustpushgo.NewWrappedApsState(apsStateStr)),
		contactsReady:           false,
		contactsReadyCh:         make(chan struct{}),
		cloudStore:              newCloudBackfillStore(c.Bridge.DB.Database, login.ID),
		sharedProfileStore:      newSharedProfileStore(c.Bridge.DB.Database, login.ID),
		pendingAttachments:      newPendingAttachmentStore(c.Bridge.DB.Database, login.ID),
		fordCache:               NewFordKeyCache(),
		recentUnsends:           make(map[string]time.Time),
		recentOutboundUnsends:   make(map[string]time.Time),
		recentSmsReactionEchoes: make(map[string]time.Time),
		smsPortals:              make(map[string]bool),
		sharedStreamAssetCache:  make(map[string]map[string]struct{}),
		sharedAlbumRooms:        make(map[string]id.RoomID),
		imGroupNames:            make(map[string]string),
		imGroupGuids:            make(map[string]string),
		imGroupParticipants:     make(map[string][]string),
		gidAliases:              make(map[string]string),
		lastGroupForMember:      make(map[string]networkid.PortalKey),
		restorePipelines:        make(map[string]bool),
		forwardBackfillSem:      make(chan struct{}, 3),
	}

	login.Client = client
	return nil
}
