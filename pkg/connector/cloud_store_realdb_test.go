package connector

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// TestEnsureSchemaAgainstRealDatabase runs the store migrations against an
// actual bridge database instead of a synthetic one.
//
// Skipped unless CORTEN_TEST_REAL_DB points at a SQLite file. Point it at a
// COPY of a real database to check that a schema change is safe for existing
// installs before shipping it:
//
//	CORTEN_TEST_REAL_DB=/tmp/copy-of-corten-matrix.db \
//	  go test ./pkg/connector/ -run TestEnsureSchemaAgainstRealDatabase -v
//
// The test copies the file again into t.TempDir() before touching it, so the
// path you pass is never written to. It asserts that ensureSchema succeeds
// twice, that no rows are lost, and that the boolean predicates the queries
// use still match rows written by older versions of the bridge as integers.
func TestEnsureSchemaAgainstRealDatabase(t *testing.T) {
	src := os.Getenv("CORTEN_TEST_REAL_DB")
	if src == "" {
		t.Skip("set CORTEN_TEST_REAL_DB to a copy of a real bridge database to run this")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	path := filepath.Join(t.TempDir(), "realdb.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("copy database: %v", err)
	}

	raw, err := sql.Open("sqlite3", "file:"+path+"?_txlock=immediate")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	raw.SetMaxOpenConns(1)
	defer raw.Close()
	db, err := dbutil.NewWithDB(raw, "sqlite3")
	if err != nil {
		t.Fatalf("wrap in dbutil: %v", err)
	}

	ctx := context.Background()
	countBefore := map[string]int{}
	for _, table := range []string{"cloud_chat", "cloud_message", "shared_profiles", "group_photo_cache"} {
		var n int
		if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			t.Logf("skipping row-count check for %s: %v", table, err)
			continue
		}
		countBefore[table] = n
	}
	t.Logf("row counts before migration: %v", countBefore)

	// Whichever login the database already has; the migrations are not
	// login-scoped, but the boolean checks below need a real one.
	var loginID string
	_ = db.QueryRow(ctx, `SELECT login_id FROM cloud_chat LIMIT 1`).Scan(&loginID)

	backfill := newCloudBackfillStore(db, networkid.UserLoginID(loginID))
	profiles := newSharedProfileStore(db, networkid.UserLoginID(loginID))
	pending := newPendingAttachmentStore(db, networkid.UserLoginID(loginID))
	for pass := 1; pass <= 2; pass++ {
		if err := backfill.ensureSchema(ctx); err != nil {
			t.Fatalf("cloudBackfillStore.ensureSchema pass %d: %v", pass, err)
		}
		if err := profiles.ensureSchema(ctx); err != nil {
			t.Fatalf("sharedProfileStore.ensureSchema pass %d: %v", pass, err)
		}
		if err := pending.ensureSchema(ctx); err != nil {
			t.Fatalf("pendingAttachmentStore.ensureSchema pass %d: %v", pass, err)
		}
	}

	for table, before := range countBefore {
		var after int
		if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM `+table).Scan(&after); err != nil {
			t.Fatalf("count %s after migration: %v", table, err)
		}
		if after != before {
			t.Errorf("%s row count changed across migration: %d -> %d", table, before, after)
		}
	}

	// fwd_backfill_done is declared BOOLEAN but older bridge versions wrote it
	// as the integers 0 and 1. The queries now compare against TRUE/FALSE, so
	// the two spellings have to select exactly the same rows on real data.
	var eqInt, eqBool int
	if err := db.QueryRow(ctx,
		`SELECT (SELECT COUNT(*) FROM cloud_chat WHERE fwd_backfill_done=1),
		        (SELECT COUNT(*) FROM cloud_chat WHERE fwd_backfill_done=TRUE)`,
	).Scan(&eqInt, &eqBool); err != nil {
		t.Fatalf("compare fwd_backfill_done spellings: %v", err)
	}
	if eqInt != eqBool {
		t.Errorf("fwd_backfill_done=1 matched %d rows but =TRUE matched %d", eqInt, eqBool)
	}
	t.Logf("fwd_backfill_done: =1 and =TRUE both match %d rows", eqBool)

	// getConversationReadByMe's ordering must be stable on real data.
	var portalID string
	if err := db.QueryRow(ctx,
		`SELECT portal_id FROM cloud_message WHERE login_id=$1 AND deleted=FALSE AND tapback_type IS NULL LIMIT 1`,
		loginID,
	).Scan(&portalID); err == nil && portalID != "" {
		first, err := backfill.getConversationReadByMe(ctx, portalID)
		if err != nil {
			t.Fatalf("getConversationReadByMe: %v", err)
		}
		for i := 0; i < 5; i++ {
			got, err := backfill.getConversationReadByMe(ctx, portalID)
			if err != nil {
				t.Fatalf("getConversationReadByMe repeat %d: %v", i, err)
			}
			if got != first {
				t.Fatalf("getConversationReadByMe is non-deterministic on real data: %v then %v", first, got)
			}
		}
	}
}

// TestScrubberAgainstRealDatabase runs the privacy scrubber's steady-state
// passes and the group portal normalization against a copy of a real bridge
// database, so their cost on a production-sized history can be read off the
// test log before shipping a change to them. Skipped unless
// CORTEN_TEST_REAL_DB is set; see TestEnsureSchemaAgainstRealDatabase.
//
// Beyond timing, it asserts what the incremental delivered-ID set promises: a
// second pass over an unchanged database extends the set built by the first
// pass instead of rebuilding it.
func TestScrubberAgainstRealDatabase(t *testing.T) {
	src := os.Getenv("CORTEN_TEST_REAL_DB")
	if src == "" {
		t.Skip("set CORTEN_TEST_REAL_DB to a copy of a real bridge database to run this")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	path := filepath.Join(t.TempDir(), "realdb.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("copy database: %v", err)
	}
	raw, err := sql.Open("sqlite3", "file:"+path+"?_txlock=immediate")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	raw.SetMaxOpenConns(1)
	defer raw.Close()
	db, err := dbutil.NewWithDB(raw, "sqlite3")
	if err != nil {
		t.Fatalf("wrap in dbutil: %v", err)
	}

	ctx := context.Background()
	var loginID, bridgeID string
	if err := db.QueryRow(ctx, `SELECT login_id FROM cloud_message LIMIT 1`).Scan(&loginID); err != nil {
		t.Skipf("no cloud_message rows to scrub: %v", err)
	}
	if err := db.QueryRow(ctx, `SELECT bridge_id FROM message LIMIT 1`).Scan(&bridgeID); err != nil {
		t.Skipf("no bridgev2 message rows: %v", err)
	}
	store := newCloudBackfillStore(db, networkid.UserLoginID(loginID))

	timed := func(name string, fn func() (int64, error)) int64 {
		t.Helper()
		start := time.Now()
		n, err := fn()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Logf("%-32s %8d rows %10s", name, n, time.Since(start).Round(time.Millisecond))
		return n
	}
	timed("ensureSchema", func() (int64, error) { return 0, store.ensureSchema(ctx) })
	timed("normalizeGroupMessagePortalIDs", func() (int64, error) { return store.normalizeGroupMessagePortalIDs(ctx) })
	timed("scrubBridgedBodies pass 1", func() (int64, error) {
		return store.scrubBridgedBodies(ctx, bridgeID, bodyScrubGracePeriod, nil)
	})
	first := store.bridged
	timed("scrubBridgedBodies pass 2", func() (int64, error) {
		return store.scrubBridgedBodies(ctx, bridgeID, bodyScrubGracePeriod, nil)
	})
	if first != nil && store.bridged != first {
		t.Fatal("second pass rebuilt the delivered-ID set on an unchanged database")
	}
	timed("scrubReactionText", func() (int64, error) { return store.scrubReactionText(ctx, bodyScrubGracePeriod) })

	var pendingCandidates int
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM cloud_message WHERE login_id=$1 AND body_scrubbed=FALSE`, loginID,
	).Scan(&pendingCandidates); err != nil {
		t.Fatal(err)
	}
	t.Logf("delivered-ID set: %d ids anchored at rowid %d; %d rows still unscrubbed",
		first.size(), first.maxRowID, pendingCandidates)
}
