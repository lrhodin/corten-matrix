// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package connector

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/id"

	"github.com/lrhodin/corten-matrix/pkg/rustpushgo"
)

func isRunningOnMacOS() bool {
	return runtime.GOOS == "darwin"
}

type IMConnector struct {
	Bridge *bridgev2.Bridge
	Config IMConfig

	// Internet-recovery hand-back backoff, per login. Kept here rather than on
	// IMClient because a hand-back exists to make bridgev2 replace the IMClient.
	handBackMu sync.Mutex
	handBacks  map[networkid.UserLoginID]*handBackRun
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
	// First FFI call in the bridge process: route Rust logging into the
	// bridge logger before any InitLogger fallback can claim it.
	installRustLogSink(bridge.Log)
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
	// Effectively unbounded. The counter is incremented and NEVER reset in
	// process (bridgestate.go), so ANY finite cap is a scheduled outage: the
	// budget is spent by recovery *succeeding* — every wedge rebuild, every
	// restored outage — so the more reliably recovery works, the sooner the
	// bridge dies. Exhaustion is silent (a Warn, no bridge state, no notice),
	// and because the Internet-recovery path tears the client down BEFORE
	// asking for the rebuild, the login is left dead rather than merely
	// degraded, with Restart=always no help because the process never exits.
	// The cap bought no Apple safety anyway. Each rebuild source has its own
	// bound, and only the first two widen: the recovery loop's unusable-probe
	// hand-backs and the wedge watchdog's rebuild are behind the widening
	// hand-back backoff (7m to 45m); the recovery loop's normal exit,
	// im-internet-recovered, is bounded only by its stability window (60s of
	// continuously reachable probes plus a passing preflight) and bridgev2's
	// own 4-6m wait, so a courier that keeps flapping on a reachable link is
	// rebuilt about every 5-7 minutes, ~10 courier reconnects per hour, for as
	// long as it flaps — and it cannot be put behind the hand-back run, which
	// a flapping courier clears by delivering frames. That is accepted: it is
	// well under the ~120/hour rustpush's own retry loop makes, the
	// Apple-heavy IDS sweep on each rebuild is gated by fullConnectIDSCooldown
	// (30m) independently of how often rebuilds happen, and it is the sweep,
	// not the courier reconnect, that the account-disable history is about.
	// A rebuild re-registers with IDS only when identity_manager.rs
	// decides it must anyway: a service with no stored registration or whose
	// data hash changed (IdentityResource::new, `.unwrap_or(true)`), or a
	// stored registration already past its renewal time (schedule_rereg
	// returns at once when calculate_rereg_time_s() <= 0 and the outer loop
	// re-registers). None of those hold on an ordinary rebuild — it is a
	// courier reconnect, roughly what an iPhone does on any network change.
	if c.Bridge.Config.UnknownErrorMaxAutoReconnects < math.MaxInt32 {
		c.Bridge.Config.UnknownErrorMaxAutoReconnects = math.MaxInt32
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
		AccountPersistBlob:       state.AccountPersistBlob,
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

// handBackRun tracks consecutive Internet-recovery hand-backs for one login. It
// lives here, not on IMClient, because a hand-back's whole purpose is to make
// bridgev2 replace the IMClient — so per-client state is destroyed exactly when
// the backoff needs to widen.
type handBackRun struct {
	last  time.Time
	count int
}

// handBackDue reports whether enough time has passed since the previous
// hand-back for this login, and if not, how much longer to wait.
func (c *IMConnector) handBackDue(login networkid.UserLoginID, now time.Time) (wait time.Duration, ok bool) {
	c.handBackMu.Lock()
	defer c.handBackMu.Unlock()
	run := c.handBacks[login]
	if run == nil || run.last.IsZero() {
		return 0, true
	}
	required := handBackDelay(run.count)
	if elapsed := now.Sub(run.last); elapsed < required {
		return required - elapsed, false
	}
	return 0, true
}

// noteHandBack records a hand-back, widening the next interval.
func (c *IMConnector) noteHandBack(login networkid.UserLoginID, now time.Time) {
	c.handBackMu.Lock()
	defer c.handBackMu.Unlock()
	if c.handBacks == nil {
		c.handBacks = make(map[networkid.UserLoginID]*handBackRun)
	}
	run := c.handBacks[login]
	if run == nil {
		run = &handBackRun{}
		c.handBacks[login] = run
	}
	run.last = now
	run.count++
}

// clearHandBacks resets the backoff. Called by the receive-wedge watchdog when
// the first inbound APS frame of a connect epoch arrives — the only evidence
// that a rebuild produced a working courier connection — and by LogoutRemote.
// Deliberately NOT by markConnected: every rebuild reaches that whether or not
// APS came up (see markConnected), and clearing there meant no rebuild source
// ever widened.
func (c *IMConnector) clearHandBacks(login networkid.UserLoginID) {
	c.handBackMu.Lock()
	defer c.handBackMu.Unlock()
	delete(c.handBacks, login)
}

// loadUserLoginDisconnectTimeout bounds how long a load waits for a prior
// client to tear down. bridgev2's NewLogin holds the bridge-wide cacheLock across
// this call and Disconnect() blocks on lifecycleMu, which Connect holds for its
// whole body, so an unbounded wait stalls every portal and ghost operation.
// Exceeding it fails the load rather than racing a duplicate APNs connection.
// A var only so tests can shrink it; never reassigned in production.
var loadUserLoginDisconnectTimeout = 30 * time.Second

// retirePreviousClient is the ONE way a load path retires the IMClient that
// bridgev2 already holds for a login, shared by IMConnector.LoadUserLogin and
// the re-login closure in completeLoginWithMeta so the two cannot drift: both
// call sites face the same hazard and must fail the same way.
//
// The predicate is IMClient.needsRetirement: a Rust client installed, a live
// connect epoch, or an orphaned recovery loop. Each of the two earlier
// predicates missed one of those. `client != nil` alone skipped the client
// Internet recovery had torn down with its loop still armed, which could push
// StateUnknownError on the SHARED ul.BridgeState (NewLogin reuses the same
// UserLogin for the same user+ID) and make bridgev2 tear down the healthy
// replacement. Adding hasOrphanedRecovery() still skipped a client in the
// MIDDLE of Connect: c.client is nil until NewClient returns (up to 300s) and
// stopChan is open, so neither half matched, the load installed a replacement,
// and the first Connect went on to finish — a second APS connection on the
// same device token, StateConnected on the shared queue, and a wedge watchdog
// and APS event loop on a stopChan nobody would close.
//
// Retiring a mid-Connect client means Disconnect() waits on lifecycleMu for
// that Connect, which is why the wait is bounded: bridgev2's NewLogin holds
// the bridge-wide cacheLock across this call. Disconnect flags the client
// before it waits, so the running Connect abandons itself at its next commit
// point instead of finishing (see terminationRequested), and the background
// teardown completes the moment it returns.
//
// On timeout it returns an error and the caller must NOT continue: building a
// second APSConnection on the same persisted device token while the first is
// still live is the duplicate-token "early eof" storm, which rustpush drives
// with an unbounded <=30s retry loop. Failing the load releases the cacheLock
// and lets bridgev2 (or the user) retry; the abandoned teardown finishes in
// the background so the next attempt finds a clean slate. Disconnect() is
// idempotent.
func retirePreviousClient(previous *IMClient, log zerolog.Logger) error {
	if previous == nil || !previous.needsRetirement() {
		return nil
	}
	log.Info().Msg("Retiring the previous iMessage client before installing a new one (avoids a duplicate APNs connection on the same device token, and retires any armed recovery loop)")
	if previous.disconnectWithTimeout(loadUserLoginDisconnectTimeout) {
		return nil
	}
	log.Error().Dur("timeout", loadUserLoginDisconnectTimeout).
		Msg("Previous iMessage client teardown did not finish in time; aborting this load rather than opening a duplicate APNs connection on the same device token")
	return fmt.Errorf("previous iMessage client teardown did not finish within %s; not opening a duplicate APNs connection", loadUserLoginDisconnectTimeout)
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
	// See retirePreviousClient for why the predicate and the abort both matter.
	if existing, ok := login.Client.(*IMClient); ok {
		if err := retirePreviousClient(existing, log); err != nil {
			return err
		}
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
			AccountPersistBlob:       meta.AccountPersistBlob,
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
		backwardDeferCounts:     make(map[string]int),
	}

	login.Client = client
	return nil
}
