package cli

import (
	"database/sql"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	// Registers the "sqlite3" driver the test config names. The bridge binary
	// links it too (via litestream's sqlite3-fk-wal), but pkg/cli itself has
	// no reason to import a database driver outside a test.
	_ "github.com/mattn/go-sqlite3"
)

// The generated config.yaml carries max_initial_messages TWICE — once at
// backfill.max_initial_messages, which is the cap the bridge applies, and once
// at backfill.threads.max_initial_messages, which governs thread backfill and
// is routinely left at its much smaller default. Reading the wrong one gives a
// deliverable count that is far too low and a delivery percentage that still
// looks entirely plausible, so this is the failure mode worth a test of its
// own rather than a comment.
func TestParseSyncStatusConfigPicksTheTopLevelMessageCap(t *testing.T) {
	const configYAML = `
database:
    type: sqlite3-fk-wal
    uri: file:/home/u/.local/share/corten-matrix/bridge.db?_txlock=immediate
backfill:
    enabled: true
    max_initial_messages: 2147483647
    max_catchup_messages: 500
    threads:
        max_initial_messages: 50
    queue:
        enabled: true
        batch_size: 10000
network:
    cloudkit_backfill: true
    bridge_filtered_chats: true
`
	cfg, err := parseSyncStatusConfig([]byte(configYAML))
	if err != nil {
		t.Fatalf("parseSyncStatusConfig: %v", err)
	}
	if cfg.Backfill.MaxInitialMessages != math.MaxInt32 {
		t.Errorf("MaxInitialMessages = %d, want %d (the top-level key, not threads' 50)",
			cfg.Backfill.MaxInitialMessages, math.MaxInt32)
	}
	if cfg.Database.Type != "sqlite3-fk-wal" || cfg.Database.URI == "" {
		t.Errorf("database = %+v, want the configured type and URI", cfg.Database)
	}
	if !cfg.Network.CloudKitBackfill || !cfg.Network.BridgeFilteredChats {
		t.Errorf("network = %+v, want both flags true", cfg.Network)
	}
}

func TestEffectiveMaxInitialMessages(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want int
	}{
		{
			name: "uncapped sentinel stays uncapped",
			yaml: "backfill:\n    max_initial_messages: 2147483647\n    threads:\n        max_initial_messages: 50\nnetwork:\n    cloudkit_backfill: true\n",
			want: math.MaxInt32,
		},
		{
			name: "a real cap is preserved",
			yaml: "backfill:\n    max_initial_messages: 500\n    threads:\n        max_initial_messages: 50\nnetwork:\n    cloudkit_backfill: true\n",
			want: 500,
		},
		{
			// PostInit forces anything under 100 to the uncapped sentinel when
			// CloudKit backfill is on, so reporting a 50-message cap the bridge
			// does not apply would be a lie.
			name: "under 100 is forced uncapped with CloudKit backfill on",
			yaml: "backfill:\n    max_initial_messages: 50\nnetwork:\n    cloudkit_backfill: true\n",
			want: math.MaxInt32,
		},
		{
			// The same override is guarded on CloudKit backfill being enabled,
			// so without it the configured value stands.
			name: "under 100 stands with CloudKit backfill off",
			yaml: "backfill:\n    max_initial_messages: 50\nnetwork:\n    cloudkit_backfill: false\n",
			want: 50,
		},
		{
			name: "only the threads key present reads as unset",
			yaml: "backfill:\n    threads:\n        max_initial_messages: 50\nnetwork:\n    cloudkit_backfill: true\n",
			want: math.MaxInt32,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseSyncStatusConfig([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("parseSyncStatusConfig: %v", err)
			}
			if got := cfg.effectiveMaxInitialMessages(); got != tc.want {
				t.Errorf("effectiveMaxInitialMessages = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestRunSyncStatusEndToEnd drives the whole CLI path — locate config.yaml,
// open the database it names, run the read-only report — against a database
// built here. It is the only place the CLI half is exercised as a unit,
// because `corten-matrix sync-status` cannot be reached from the binary until
// main.go's subcommand switch lists it.
func TestRunSyncStatusEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", home)
	dir := filepath.Join(home, "corten-matrix")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(dir, "bridge.db")

	raw, err := sql.Open("sqlite3", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	_, err = raw.Exec(`
		CREATE TABLE user_login (bridge_id TEXT NOT NULL, id TEXT NOT NULL);
		CREATE TABLE message (bridge_id TEXT NOT NULL, id TEXT NOT NULL,
			room_receiver TEXT NOT NULL, timestamp BIGINT NOT NULL);
		INSERT INTO user_login (bridge_id, id) VALUES ('corten', 'login-1');
	`)
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	config := "database:\n    type: sqlite3\n    uri: file:" + dbPath +
		"\nbackfill:\n    max_initial_messages: 2147483647\n    threads:\n        max_initial_messages: 50\n" +
		"network:\n    cloudkit_backfill: true\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	out := captureStdout(t, func() { runSyncStatus(nil) })
	// A logged-in database whose CloudKit store has never initialized: the
	// report has to say so rather than showing zeros as if they were progress.
	if !strings.Contains(out, "CloudKit backfill has never initialized") {
		t.Errorf("report did not describe the uninitialized database:\n%s", out)
	}
}

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		_, _ = io.Copy(&sb, r)
		done <- sb.String()
	}()
	fn()
	os.Stdout = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}
