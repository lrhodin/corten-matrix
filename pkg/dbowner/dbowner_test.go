package dbowner

import (
	"database/sql"
	"path/filepath"
	"testing"

	// Registers "sqlite3-fk-wal" — the driver name the install scripts write
	// into config.yaml. The bridge gets this transitively via
	// bridgev2/matrix/connector.go; importing it here keeps the test honest
	// about the exact driver production uses.
	_ "go.mau.fi/util/dbutil/litestream"
)

const driver = "sqlite3-fk-wal"

// newDB creates a database with a database_owner row set to owner. An empty
// owner means "create the table but leave it empty"; a "-" means "no table at
// all", i.e. a genuinely fresh install.
func newDB(t *testing.T, owner string) string {
	t.Helper()
	uri := "file:" + filepath.Join(t.TempDir(), "corten-matrix.db") + "?_txlock=immediate"
	db, err := sql.Open(driver, uri)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if owner == "-" {
		// Touch the file without the table.
		if _, err = db.Exec("CREATE TABLE unrelated (x INTEGER)"); err != nil {
			t.Fatalf("create unrelated: %v", err)
		}
		return uri
	}
	// Same shape dbutil creates (upgrades.go).
	if _, err = db.Exec(`CREATE TABLE database_owner (key INTEGER PRIMARY KEY, owner TEXT NOT NULL)`); err != nil {
		t.Fatalf("create database_owner: %v", err)
	}
	if owner != "" {
		if _, err = db.Exec("INSERT INTO database_owner (key, owner) VALUES (0, ?)", owner); err != nil {
			t.Fatalf("insert owner: %v", err)
		}
	}
	return uri
}

func readOwner(t *testing.T, uri string) string {
	t.Helper()
	db, err := sql.Open(driver, uri)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db.Close() }()
	var owner string
	if err = db.QueryRow("SELECT owner FROM database_owner WHERE key=0").Scan(&owner); err != nil {
		return ""
	}
	return owner
}

// The case that matters: an install predating the rename, which today
// crash-loops on ErrNotOwned.
func TestMigratesPreRenameInstall(t *testing.T) {
	uri := newDB(t, Old)
	migrated, err := Migrate(driver, uri)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !migrated {
		t.Fatal("expected the pre-rename owner to be migrated")
	}
	if got := readOwner(t, uri); got != New {
		t.Errorf("owner = %q, want %q", got, New)
	}
}

// Re-running must not thrash the row or report a spurious migration.
func TestAlreadyMigratedIsNoOp(t *testing.T) {
	uri := newDB(t, New)
	migrated, err := Migrate(driver, uri)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if migrated {
		t.Error("expected no migration for an already-current owner")
	}
	if got := readOwner(t, uri); got != New {
		t.Errorf("owner = %q, want %q", got, New)
	}
}

// The safety property: a database belonging to a different bridge must keep
// failing dbutil's ownership check rather than being adopted.
func TestForeignOwnerUntouched(t *testing.T) {
	for _, foreign := range []string{
		"megabridge/mautrix-whatsapp",
		"megabridge/mautrix-signal",
		"mautrix-imessage", // bare pre-megabridge name: mxmain's own migration owns this
		"something-else",
	} {
		uri := newDB(t, foreign)
		migrated, err := Migrate(driver, uri)
		if err != nil {
			t.Fatalf("Migrate(%q): %v", foreign, err)
		}
		if migrated {
			t.Errorf("Migrate(%q) adopted a database it must not touch", foreign)
		}
		if got := readOwner(t, uri); got != foreign {
			t.Errorf("owner = %q, want %q untouched", got, foreign)
		}
	}
}

// A fresh install has no table yet; dbutil inserts the owner itself on first
// init. Must not error, must not create anything.
func TestFreshInstallIsNoOp(t *testing.T) {
	for name, owner := range map[string]string{
		"no table": "-",
		"no row":   "",
	} {
		t.Run(name, func(t *testing.T) {
			uri := newDB(t, owner)
			migrated, err := Migrate(driver, uri)
			if err != nil {
				t.Fatalf("Migrate: %v", err)
			}
			if migrated {
				t.Error("expected no migration on a fresh database")
			}
		})
	}
}

// A missing file must not abort startup.
func TestMissingDatabaseIsNoOp(t *testing.T) {
	uri := "file:" + filepath.Join(t.TempDir(), "does-not-exist.db")
	if migrated, err := Migrate(driver, uri); err != nil || migrated {
		t.Errorf("Migrate = (%v, %v), want (false, nil)", migrated, err)
	}
}

// Unsupported or empty configuration must be inert.
func TestUnsupportedConfigIsNoOp(t *testing.T) {
	if migrated, err := Migrate(driver, ""); err != nil || migrated {
		t.Errorf("empty URI: got (%v, %v)", migrated, err)
	}
	if migrated, err := Migrate("mysql", "whatever"); err != nil || migrated {
		t.Errorf("unsupported driver: got (%v, %v)", migrated, err)
	}
}
