package main

import (
	"strings"
	"testing"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"
)

// TestSQLiteFixupsCoverLitestream pins the database types the SQLite fixups
// apply to.
//
// "litestream" is the one that got missed: mxmain's initDB treats it and
// sqlite3-fk-wal identically, and dbutil registers it as the same
// mattn/go-sqlite3 driver, so a litestream config is a SQLite config in every
// way that matters here. Guarding on the "sqlite" prefix alone left it with the
// upstream max_open_conns: 5 and no _secure_delete — five goroutines writing one
// file during backfill, which is how a portal's history gets stranded behind
// "database is locked", and freed plaintext pages left readable on disk.
func TestSQLiteFixupsCoverLitestream(t *testing.T) {
	for _, tc := range []struct {
		dbType string
		want   bool
	}{
		{dbType: "sqlite3-fk-wal", want: true},
		{dbType: "sqlite3-fk-wal-fullsync", want: true},
		{dbType: "litestream", want: true},
		{dbType: "postgres", want: false},
		{dbType: "pgx", want: false},
	} {
		t.Run(tc.dbType, func(t *testing.T) {
			if got := isSQLiteDatabaseType(tc.dbType); got != tc.want {
				t.Fatalf("isSQLiteDatabaseType(%q) = %v, want %v", tc.dbType, got, tc.want)
			}

			br := &mxmain.BridgeMain{Config: &bridgeconfig.Config{}}
			br.Config.Database = dbutil.Config{}
			br.Config.Database.Type = tc.dbType
			br.Config.Database.URI = "file:corten.db?_txlock=immediate"
			br.Config.Database.MaxOpenConns = 5

			ensureSecureDeleteDSN(br)
			ensureSQLiteWriteSerialization(br)

			gotClamp := br.Config.Database.MaxOpenConns == 1
			gotSecureDelete := strings.Contains(br.Config.Database.URI, "_secure_delete=on")
			if gotClamp != tc.want {
				t.Errorf("max_open_conns clamped = %v, want %v (got %d)",
					gotClamp, tc.want, br.Config.Database.MaxOpenConns)
			}
			if gotSecureDelete != tc.want {
				t.Errorf("_secure_delete applied = %v, want %v (URI %q)",
					gotSecureDelete, tc.want, br.Config.Database.URI)
			}
		})
	}
}
