// Package dbowner repairs the database_owner row left behind when the bridge
// was renamed.
//
// bc2101c7 ("Corten matrix refactor", 2026-06-22) renamed BridgeMain.Name from
// "mautrix-imessage" to "corten-matrix". mxmain opens the database as
// dbutil.NewFromConfig("megabridge/"+br.Name, …), so the rename changed the
// expected owner of every database created before it. dbutil compares that
// string against the database_owner row and returns ErrNotOwned on a mismatch;
// mxmain's dberror handler exits 15. Under systemd's Restart=always that is a
// permanent crash loop, and the message printed — "Sharing the same database
// with different programs is not supported" — describes a different failure,
// because dberror.go only prints the rename hint when the stored owner equals
// br.Name exactly (which "megabridge/mautrix-imessage" never does).
//
// Nothing upstream migrates it: mxmain's legacymigrate handles only the bare
// pre-megabridge name and bails with "Unexpected database owner" otherwise.
//
// This package deliberately does not import mxmain, so it is testable against a
// real SQLite file without the bridge's cgo dependencies.
package dbowner

import (
	"database/sql"
	"strings"
)

const (
	// Old is this bridge's own former owner string, and the ONLY value Migrate
	// will ever adopt. A genuinely foreign owner — another mautrix bridge
	// sharing a database — must keep failing the ownership check.
	Old = "megabridge/mautrix-imessage"
	// New must track mxmain's "megabridge/"+BridgeMain.Name. The test in
	// cmd/corten-matrix asserts that, so a future rename can't silently
	// reintroduce the crash loop.
	New = "megabridge/corten-matrix"
)

// Migrate rewrites the database_owner row from Old to New, returning whether it
// changed anything.
//
// It is a no-op — nil error, false — for a fresh database (no table or no row),
// a database already on New, and any database owned by something else. Errors
// are returned for the caller to decide on; the caller in this bridge ignores
// them, because the real database open moments later reports the real problem
// with far better context than a pre-init helper can.
func Migrate(driver, uri string) (bool, error) {
	if uri == "" || (!strings.HasPrefix(driver, "sqlite") && driver != "postgres") {
		return false, nil
	}
	db, err := sql.Open(driver, uri)
	if err != nil {
		return false, err
	}
	defer func() { _ = db.Close() }()

	var owner string
	err = db.QueryRow("SELECT owner FROM database_owner WHERE key=0").Scan(&owner)
	if err != nil {
		// Missing table (fresh database) or no row: nothing to migrate. dbutil
		// inserts the correct owner itself on first init.
		return false, nil
	}
	if owner != Old {
		return false, nil
	}

	// Raw database/sql, so no dbutil placeholder rewriting: SQLite binds "?",
	// Postgres binds "$N".
	update := "UPDATE database_owner SET owner=? WHERE key=0 AND owner=?"
	if driver == "postgres" {
		update = "UPDATE database_owner SET owner=$1 WHERE key=0 AND owner=$2"
	}
	res, err := db.Exec(update, New, Old)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
