// corten-matrix - host-side management CLI.
//
// `corten-matrix sync-status` (and `sync-status 1` for the second account)
// prints the same backfill report as the management-room `sync-status`
// command, but without needing the daemon: it reads the account's config.yaml,
// opens the database it names, and runs read-only queries. That matters
// because the state people want to inspect — "did my history actually finish
// importing?" — is most interesting when the bridge is wedged, stopped, or has
// not been added to a Matrix client yet.
//
// The queries themselves live in pkg/connector alongside the schema they read,
// so the two entry points cannot drift apart.

package cli

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"go.mau.fi/util/dbutil"
	"gopkg.in/yaml.v3"

	"github.com/lrhodin/corten-matrix/pkg/connector"
)

// syncStatusConfig is the slice of config.yaml this report needs. It is a
// hand-rolled struct rather than bridgeconfig.Config because that pulls in the
// framework's whole validation path, which would refuse to load a config the
// bridge itself is happily running on (missing registration file, unreachable
// homeserver) and turn a diagnostic into another thing that is broken.
type syncStatusConfig struct {
	Database struct {
		Type string `yaml:"type"`
		URI  string `yaml:"uri"`
	} `yaml:"database"`

	// Backfill.MaxInitialMessages is the TOP-LEVEL backfill key. The generated
	// config carries a second key with the same name under backfill.threads,
	// and the two routinely differ — the installer writes 2147483647 (the
	// uncapped sentinel) at the top level when the user declines a cap, while
	// the threads key keeps its default of 50. Reading the wrong one produces
	// a deliverable count that is wildly low and a percentage that still looks
	// plausible, so the nesting here is load-bearing. Unknown keys, including
	// threads, are ignored by yaml.v3.
	Backfill struct {
		MaxInitialMessages int `yaml:"max_initial_messages"`
	} `yaml:"backfill"`

	Network struct {
		CloudKitBackfill    bool `yaml:"cloudkit_backfill"`
		BridgeFilteredChats bool `yaml:"bridge_filtered_chats"`
	} `yaml:"network"`
}

func parseSyncStatusConfig(data []byte) (syncStatusConfig, error) {
	var cfg syncStatusConfig
	err := yaml.Unmarshal(data, &cfg)
	return cfg, err
}

// effectiveMaxInitialMessages mirrors the override the connector applies at
// startup (see PostInit): with CloudKit backfill on, anything below 100 is
// treated as "the user did not really mean to cap this" and forced to
// math.MaxInt32. Without that mirroring, a config left at the mautrix default
// of 50 would make the CLI report a 50-message-per-chat cap that the running
// bridge does not apply.
func (c syncStatusConfig) effectiveMaxInitialMessages() int {
	if c.Network.CloudKitBackfill && c.Backfill.MaxInitialMessages < 100 {
		return math.MaxInt32
	}
	return c.Backfill.MaxInitialMessages
}

// runSyncStatus implements `corten-matrix sync-status [1]`.
func runSyncStatus(args []string) {
	dir := cortenDataDir()
	who := ""
	if len(args) > 0 && args[0] == "1" {
		dir = secondDataDir()
		who = " (second account)"
	}
	configPath := filepath.Join(dir, "config.yaml")

	data, err := os.ReadFile(configPath)
	if err != nil {
		die("Could not read %s: %v", configPath, err)
	}
	cfg, err := parseSyncStatusConfig(data)
	if err != nil {
		die("Could not parse %s: %v", configPath, err)
	}
	if cfg.Database.Type == "" || cfg.Database.URI == "" {
		die("No database configured in %s", configPath)
	}

	db, err := dbutil.NewWithDialect(cfg.Database.URI, cfg.Database.Type)
	if err != nil {
		die("Could not open the database: %v", err)
	}
	defer db.Close()
	// One connection: the bridge may be running against this same file, and
	// everything below is read-only, so there is no reason to open a pool that
	// competes with it. No migrations are run — dbutil only upgrades when it
	// is explicitly asked to.
	db.RawDB.SetMaxOpenConns(1)

	report, err := connector.GetSyncStatus(context.Background(), db, connector.SyncStatusOptions{
		// BridgeID and LoginID are left empty: only the database knows them,
		// and GetSyncStatus reads them from the single user_login row.
		MaxInitialMessages:  cfg.effectiveMaxInitialMessages(),
		BridgeFilteredChats: cfg.Network.BridgeFilteredChats,
		// SyncRunning and BatchSending stay nil — both are properties of the
		// running process, and inventing a value here would be a guess the
		// report then states as fact.
	})
	if err != nil {
		die("Could not read sync status from %s: %v", cfg.Database.URI, err)
	}

	fmt.Printf("\n  %scorten-matrix sync-status%s%s\n  %s%s%s\n\n",
		cBold+cAccent, cReset, who, cDim, configPath, cReset)
	fmt.Println(report.Format())
}
