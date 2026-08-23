// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Tulir Asokan, Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/beeper/bridge-manager/api/beeperapi"

	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"
	"maunium.net/go/mautrix/id"

	"github.com/lrhodin/corten-matrix/pkg/cli"
	"github.com/lrhodin/corten-matrix/pkg/connector"
)

var (
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
	// libBuildID is set by build.sh via -ldflags to the librustpushgo.a hash. Go
	// doesn't track external cgo libs, so this forces a relink when the .a changes
	// (otherwise a stale binary built against an old library could ship).
	libBuildID = ""
)

var m = mxmain.BridgeMain{
	Name:        "corten-matrix",
	URL:         "https://github.com/lrhodin/corten-matrix",
	Description: "A Matrix-iMessage puppeting bridge (bridgev2).",
	Version:     "0.1.0",

	Connector: &connector.IMConnector{},
}

func init() {
	m.PostInit = func() {
		proc := m.Bridge.Commands.(*commands.Processor)
		for _, h := range connector.BridgeCommands() {
			proc.AddHandler(h)
		}
		// Let the user delete their management room from the client — bridgev2
		// swallows every event sent in it, so its delete never reaches a handler.
		connector.RegisterManagementRoomDelete(m.Matrix, m.Bridge)
		raiseMatrixHTTPTimeout()
	}
}

func main() {
	m.InitVersion(Tag, Commit, BuildTime)

	// Let any host-command extensions registered by the build configuration
	// contribute their `help` rows.
	cli.ExtraHelpRows = connector.ExtraHostHelp()

	// Handle subcommands / flags before normal bridge startup.
	if len(os.Args) > 1 && os.Args[0] != "-" {
		// Give host-command extensions first refusal on the subcommand.
		if connector.HandleHostCommand(os.Args[1:], Tag, runtime.GOOS, runtime.GOARCH) {
			return
		}
		switch os.Args[1] {
		case "help", "-h", "--help":
			cli.PrintHelp()
			return
		case "fda-check":
			// Probe chat.db as our OWN TCC "responsible" process (via a transient
			// launchd job, see fda_darwin.go) so macOS attributes the access to THIS
			// binary and lists it under Full Disk Access during setup — not Terminal,
			// and not only once the bridge later runs under launchd. Exits 0 if
			// chat.db is readable, 1 otherwise. See scripts/install*.sh.
			os.Exit(fdaCheck())
		case "fda-probe":
			// Internal: spawned under launchd by fda-check. Attempting chat.db as
			// our own responsible process is what registers the binary with TCC /
			// Full Disk Access. Writes "0" (readable) / "1" (denied) to argv[2].
			res := []byte("1")
			home, _ := os.UserHomeDir()
			if f, err := os.Open(filepath.Join(home, "Library", "Messages", "chat.db")); err == nil {
				_ = f.Close()
				res = []byte("0")
			}
			if len(os.Args) > 2 {
				_ = os.WriteFile(os.Args[2], res, 0o644)
			}
			os.Exit(0)
		case "bridge-all":
			// ExecStart of the single service: run every configured account's
			// bridge under this one process (see pkg/cli.RunAllBridges).
			cli.RunAllBridges()
		case "setup", "setup-beeper", "start", "stop", "restart",
			"status", "logs", "bbctl", "reset", "uninstall",
			"install-service", "uninstall-service", "sync-status":
			// Host-side management CLI (the familiar ops, now via subcommands
			// instead of a Makefile). Docker-aware; see pkg/cli.
			cli.RunManagement(os.Args[1], os.Args[2:])
		case "login":
			// Remove "login" from args so flag parsing in PreInit works.
			os.Args = append(os.Args[:1], os.Args[2:]...)
			// A bare `corten-matrix login` (no -c, run from an arbitrary cwd)
			// otherwise makes PreInit look for ./config.yaml and abort with a
			// config error before the login flow runs. Default -c to the real
			// data-dir config (and chdir there so relative paths resolve), the
			// same way the service / install scripts launch the bridge.
			resolveLoginConfig()
			runInteractiveLogin(&m)
			return
		case "check-restore":
			// Validate that backup session state can be restored without
			// re-authentication. Exits 0 if valid, 1 if not.
			if connector.CheckSessionRestore() {
				fmt.Fprintln(os.Stderr, "[+] Backup session state is valid — login can be auto-restored")
				os.Exit(0)
			} else {
				fmt.Fprintln(os.Stderr, "[-] No valid backup session state — login required")
				os.Exit(1)
			}
		case "list-handles":
			// Print available iMessage handles (phone/email) from session state.
			handles := connector.ListHandles()
			if len(handles) == 0 {
				os.Exit(1)
			}
			for _, h := range handles {
				fmt.Println(h)
			}
			return
		case "carddav-setup":
			// Discover CardDAV URL + encrypt password for install scripts.
			runCardDAVSetup()
			return
		case "init-db":
			// Initialize the database schema and exit without starting the
			// bridge. Used by install scripts to create the DB before asking
			// setup questions, without connecting to Matrix or APNs.
			os.Args = append(os.Args[:1], os.Args[2:]...)
			m.PreInit()
			ensureSecureDeleteDSN(&m)
			ensureSQLiteWriteSerialization(&m)
			repairPermissions(&m)
			migrateDatabaseOwner(&m)
			m.Init()
			fmt.Fprintln(os.Stderr, "Database initialized successfully")
			os.Exit(0)
		default:
			// A first argument that is neither a known subcommand nor a flag is
			// a typo/unknown command. Without this, it silently fell through to
			// normal bridge startup and surfaced as a confusing
			// "failed to read config" error. Flags (-c config.yaml, etc.) start
			// with "-" and must still pass through to the bridge's flag parser.
			if !strings.HasPrefix(os.Args[1], "-") {
				fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", os.Args[1])
				cli.PrintHelp()
				os.Exit(2)
			}
		}
	}

	// --setup flag: check permissions (FDA + Contacts) via native dialogs.
	if isSetupMode() {
		// Remove --setup from args so it doesn't confuse the bridge.
		var filtered []string
		for _, a := range os.Args {
			if a != "--setup" && a != "-setup" {
				filtered = append(filtered, a)
			}
		}
		os.Args = filtered
		runSetupPermissions()
		return
	}

	// Raise the open-file-descriptor limit before opening any connections. On
	// macOS, launchd hands daemons a soft RLIMIT_NOFILE of just 256, which a
	// busy bridge (many portals, APNs, IDS, the appservice websocket, SQLite,
	// CardDAV) can exhaust over long uptime — after which new sockets fail with
	// "too many open files", the websocket can't reconnect, and the bridge goes
	// silent until restarted. No-op on non-macOS platforms. See rlimit_*.go.
	raiseFileLimit()

	// Cap the Go heap (GOMEMLIMIT) to a fraction of system memory so an
	// aggressive backfill can't drive the resident set past physical RAM and
	// get OOM-killed on small, swap-less hosts. Honors an explicit operator
	// GOMEMLIMIT and is cgroup-aware for Docker. See memlimit.go / meminfo_*.go.
	capMemoryLimitFromSystem()

	// Backfill any network config keys this build knows about but that are
	// missing from the on-disk config (e.g. configs generated before a key was
	// added). Runs before PreInit loads the config so the file is complete for
	// this run too. Append-only and parser-safe — it never overwrites existing
	// keys and never touches a config that doesn't parse. See ensure_config.go.
	ensureNetworkConfigKeys(configPathFromArgs())

	// Instead of m.Run(), manually call PreInit/Init/Start so we can
	// repair broken permissions before validateConfig() runs in Init().
	m.PreInit()
	ensureSecureDeleteDSN(&m)
	ensureSQLiteWriteSerialization(&m)
	repairPermissions(&m)
	migrateDatabaseOwner(&m)
	m.Init()
	m.Start()
	exitCode := m.WaitForInterrupt()
	m.Stop()
	os.Exit(exitCode)
}

// isSQLiteDatabaseType reports whether a bridgeconfig database.type names a
// SQLite-backed engine, so the SQLite-specific fixups below can share one
// definition instead of each spelling their own prefix test.
//
// "litestream" has to be in it: mxmain's initDB checks
// `dbConfig.Type == "sqlite3-fk-wal" || dbConfig.Type == "litestream"` when it
// warns about _txlock, and dbutil's ParseDialect maps both prefixes to the
// SQLite dialect. The litestream driver is the same mattn/go-sqlite3 driver
// registered with a different ConnectHook (persistent WAL, autocheckpoint off),
// so it takes the same DSN params and has the same one-writer-per-file
// constraint. Testing only for "sqlite" left a litestream config running with
// the upstream max_open_conns: 5 and no _secure_delete — precisely the
// "database is locked" backfill stranding the clamp below exists to prevent.
// The install scripts only ever write sqlite3-fk-wal, so that gap could only
// ever be reached from a hand-edited config.
func isSQLiteDatabaseType(dbType string) bool {
	return strings.HasPrefix(dbType, "sqlite") || strings.HasPrefix(dbType, "litestream")
}

// ensureSecureDeleteDSN forces SQLite's secure_delete pragma on for every
// pooled connection by injecting it into the database DSN before the bridge
// opens the pool. secure_delete is a per-connection setting that is NOT
// persisted in the database file, so running `PRAGMA secure_delete=ON` once
// only affects whichever pooled connection executed it — other connections'
// writes would still leave freed plaintext pages readable on disk. The
// mattn/go-sqlite3 driver applies the _secure_delete DSN param on every
// connect, which is the only way to guarantee the privacy scrubber's NULLed
// plaintext is actually zeroed out of freed pages across the whole pool.
// In-memory only (not persisted to config.yaml). No-op for non-SQLite
// backends and when the operator already set the param.
func ensureSecureDeleteDSN(br *mxmain.BridgeMain) {
	if br.Config == nil || !isSQLiteDatabaseType(br.Config.Database.Type) {
		return
	}
	uri := br.Config.Database.URI
	if uri == "" || strings.Contains(uri, "_secure_delete") {
		return
	}
	sep := "?"
	if strings.Contains(uri, "?") {
		sep = "&"
	}
	br.Config.Database.URI = uri + sep + "_secure_delete=on"
}

// raiseMatrixHTTPTimeout lifts the appservice HTTP client's timeout above
// mautrix's 180s default.
//
// createRoom carries every initial state event plus the invites, and on a
// self-hosted Synapse under mass backfill it can take longer than 180s to
// return even though the server does finish creating the room. The client gives
// up, portal creation is retried, and the retry makes a SECOND room — the first
// one orphaned. (Beeper's BeeperLocalRoomID idempotency hint that would prevent
// this is a hungryserv extension; Synapse ignores it.)
//
// Mutating .Timeout in place rather than replacing the client keeps the cookie
// jar and the unix-socket transport MakeAppService may have configured. Ghost
// intents share this client — appservice.NewMautrixClient passes as.HTTPClient
// straight through — so they get the same headroom. Double-puppet clients do
// NOT: NewExternalMautrixClient builds its own 180s client when a separate
// homeserver URL is set. That is fine here, since those only send individual
// events, never a createRoom.
//
// AS is populated before PostInit runs (NewBridge -> Matrix.Init ->
// MakeAppService), so the hook always has a client to adjust; it warns and
// no-ops rather than panicking if that ever stops being true.
const defaultMatrixHTTPTimeout = 10 * time.Minute

func raiseMatrixHTTPTimeout() {
	timeout := defaultMatrixHTTPTimeout
	if raw := os.Getenv("CORTEN_MATRIX_HTTP_TIMEOUT"); raw != "" {
		if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
			timeout = time.Duration(secs) * time.Second
		} else {
			m.Log.Warn().Str("value", raw).
				Msg("Ignoring invalid CORTEN_MATRIX_HTTP_TIMEOUT (want positive integer seconds)")
		}
	}
	if m.Matrix == nil || m.Matrix.AS == nil || m.Matrix.AS.HTTPClient == nil {
		m.Log.Warn().Msg("Could not raise Matrix HTTP client timeout: appservice client not available")
		return
	}
	m.Matrix.AS.HTTPClient.Timeout = timeout
	m.Log.Info().Stringer("timeout", timeout).
		Msg("Raised Matrix appservice HTTP client timeout (stops createRoom timeouts from orphaning rooms under load)")
}

// ensureSQLiteWriteSerialization stops concurrent writers from colliding on a
// SQLite database file.
//
// The upstream config template ships `max_open_conns: 5`, which is right for
// Postgres and wrong for SQLite: five pooled connections mean five goroutines
// trying to write one file. During the first backfill — CloudKit sync writing
// cloud_message/cloud_chat, the backfill queue writing message rows, and the
// crypto store writing group sessions, all at once — they collide, SQLite
// returns SQLITE_BUSY past the 5s busy_timeout, and the caller sees
// "database is locked".
//
// That is not a cosmetic failure. A locked write inside a backfill batch send
// means bridgev2 never runs the batch's CompleteCallback, so the portal's
// forward backfill is never marked done and its history never lands. Serializing
// at the pool is the cheap, total fix: SQLite allows exactly one writer anyway,
// so queuing in Go costs nothing that SQLite wasn't already going to serialize.
//
// In-memory only (not persisted to config.yaml), and a no-op for non-SQLite
// backends (see isSQLiteDatabaseType — litestream counts). An explicit
// `max_open_conns: 1` is left alone silently; anything higher is clamped with a
// log line, because the operator asked for something we're overriding and
// should be able to see that.
func ensureSQLiteWriteSerialization(br *mxmain.BridgeMain) {
	if br.Config == nil || !isSQLiteDatabaseType(br.Config.Database.Type) {
		return
	}
	if br.Config.Database.MaxOpenConns > 1 {
		fmt.Fprintf(os.Stderr,
			"[database] clamping max_open_conns %d -> 1 for SQLite: concurrent writers on one "+
				"file produce \"database is locked\" during backfill, which silently strands a "+
				"portal's history. Use PostgreSQL if you need real write concurrency.\n",
			br.Config.Database.MaxOpenConns)
	}
	br.Config.Database.MaxOpenConns = 1
	if br.Config.Database.MaxIdleConns > 1 || br.Config.Database.MaxIdleConns == 0 {
		br.Config.Database.MaxIdleConns = 1
	}

	// Ask for a longer busy handler than the driver's default, for the writers
	// this pool does NOT own (a CLI subcommand against the same data dir).
	//
	// Honesty note: as of go.mau.fi/util v0.9.9 this does NOT take effect.
	// mattn/go-sqlite3 applies DSN pragmas inside Open and calls the driver's
	// ConnectHook afterwards, and dbutil registers both `sqlite3-fk-wal` and
	// `litestream` with a hook that ends in `PRAGMA busy_timeout = 5000` — so
	// the hook overwrites this value on every connection. Measured: opening
	// either driver with `_busy_timeout=30000` reports busy_timeout=5000, while
	// `_secure_delete=on` (which no hook touches) does survive. The param is
	// left in place because it is harmless, is what an operator reading the DSN
	// would expect, and starts working the day dbutil stops hard-setting the
	// pragma. The real protection against lock contention is the clamp above,
	// not this line — do not treat a 30s busy window as guaranteed.
	uri := br.Config.Database.URI
	if uri != "" && !strings.Contains(uri, "_busy_timeout") {
		sep := "?"
		if strings.Contains(uri, "?") {
			sep = "&"
		}
		br.Config.Database.URI = uri + sep + "_busy_timeout=30000"
	}
}

// resolveLoginConfig makes a bare `corten-matrix login` find the bridge config
// from any working directory. mxmain's PreInit defaults -c to ./config.yaml, so
// without this a login run from anywhere but the data dir aborts with a
// config.yaml error before the login flow ever starts. We replicate what the
// service and install scripts do (`cd $DATADIR && ... -c $DATADIR/config.yaml`):
// chdir into the data dir (so relative paths in the config resolve) and inject
// -c. It is a no-op when the caller already passed -c/--config or no config
// exists in the data dir, so existing invocations are unaffected.
func resolveLoginConfig() {
	for _, a := range os.Args[1:] {
		if a == "-c" || a == "--config" ||
			strings.HasPrefix(a, "-c=") || strings.HasPrefix(a, "--config=") {
			return
		}
	}
	dir := cli.DataDir()
	cfg := filepath.Join(dir, "config.yaml")
	if _, err := os.Stat(cfg); err == nil {
		_ = os.Chdir(dir)
		os.Args = append(os.Args, "-c", cfg)
	}
}

// repairPermissions detects and fixes broken bridge.permissions before the
// bridge's validateConfig() rejects the config. This handles cases where
// bbctl generated a config with an empty or invalid username, leaving
// permissions with only example.com defaults.
func repairPermissions(br *mxmain.BridgeMain) {
	if br.Config == nil {
		return
	}
	configured := br.Config.Bridge.Permissions.IsConfigured()
	fmt.Fprintf(os.Stderr, "[permissions] IsConfigured=%v entries=%d\n", configured, len(br.Config.Bridge.Permissions))
	for key := range br.Config.Bridge.Permissions {
		fmt.Fprintf(os.Stderr, "[permissions]   %q\n", key)
	}
	if configured {
		return
	}

	// Permissions are not configured — try to derive the correct MXID
	// from bbctl's saved credentials.
	username := loadBBCtlUsername()
	if username == "" {
		fmt.Fprintf(os.Stderr, "[permissions] loadBBCtlUsername returned empty — cannot repair\n")
		return
	}

	mxid := id.NewUserID(username, "beeper.com")

	// Remove bogus entries (example.com defaults, empty username variants,
	// wildcard relay) from the in-memory map so findAdminUser() doesn't
	// pick them over the real MXID. Patterns match fixPermissionsOnDisk()
	// and the shell fix_permissions() function.
	for key := range br.Config.Bridge.Permissions {
		if strings.Contains(key, "example.com") || key == "*" ||
			key == "@:" || key == "@" || strings.HasPrefix(key, "@:") {
			delete(br.Config.Bridge.Permissions, key)
		}
	}

	br.Config.Bridge.Permissions[string(mxid)] = &bridgeconfig.PermissionLevelAdmin

	// Also persist the fix to config.yaml so this is a one-time repair.
	if br.ConfigPath != "" {
		fixPermissionsOnDisk(br.ConfigPath, string(mxid))
	}

	fmt.Fprintf(os.Stderr, "Auto-repaired bridge.permissions: %s → admin\n", mxid)
}

// loadBBCtlUsername reads the username from bbctl's config.json.
func loadBBCtlUsername() string {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(configDir, "bbctl", "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var cfg struct {
		Environments map[string]struct {
			Username    string `json:"username"`
			AccessToken string `json:"access_token"`
		} `json:"environments"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return ""
	}
	if prod, ok := cfg.Environments["prod"]; ok {
		if prod.Username != "" {
			return prod.Username
		}
		// Username empty but have credentials — try whoami as last resort
		if strings.HasPrefix(prod.AccessToken, "syt_") {
			resp, err := beeperapi.Whoami("beeper.com", prod.AccessToken)
			if err == nil && resp.UserInfo.Username != "" {
				return resp.UserInfo.Username
			}
		}
	}
	return ""
}

// fixPermissionsOnDisk patches the config.yaml file to set the correct admin
// MXID and remove all bogus permission entries (example.com defaults, empty
// username variants). Matches the same patterns as the in-memory cleanup in
// repairPermissions() and the shell repair function in the install scripts.
func fixPermissionsOnDisk(configPath string, mxid string) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return
	}

	// isBogusPermLine returns true for any permissions entry that should be
	// removed: example.com defaults, empty-username variants, wildcard relay.
	isBogusPermLine := func(trimmed string) bool {
		// Empty-username patterns: "@:beeper.com", "@": ...
		if strings.Contains(trimmed, `"@":`) || strings.Contains(trimmed, `"@:`) {
			return true
		}
		// Example.com defaults: "@admin:example.com", "example.com"
		if strings.Contains(trimmed, "example.com") {
			return true
		}
		// Wildcard relay entry from example config
		if strings.HasPrefix(trimmed, `"*":`) {
			return true
		}
		return false
	}

	lines := strings.Split(string(data), "\n")
	inPerms := false
	replaced := false
	var out []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Track whether we're inside the permissions block.
		if strings.HasPrefix(trimmed, "permissions:") {
			inPerms = true
			out = append(out, line)
			continue
		}
		// A non-indented, non-empty line exits the permissions block.
		if inPerms && trimmed != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inPerms = false
		}

		if inPerms && isBogusPermLine(trimmed) {
			if !replaced && strings.Contains(trimmed, ": admin") {
				// Replace the first admin line with the correct MXID.
				indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
				out = append(out, indent+`"`+mxid+`": admin`)
				replaced = true
			}
			// Drop all other bogus lines (example.com user, wildcard relay, etc.)
			continue
		}
		out = append(out, line)
	}
	_ = os.WriteFile(configPath, []byte(strings.Join(out, "\n")), 0600)
}
