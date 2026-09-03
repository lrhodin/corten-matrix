package connector

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
// The test copies the database again into t.TempDir() before touching it
// (see openRealDatabaseCopy), so the path you pass is never written to. It
// asserts that ensureSchema succeeds twice, that only the explicitly targeted
// legacy system rows are removed, and that the boolean predicates the queries
// use still match rows written by older versions of the bridge as integers.
func TestEnsureSchemaAgainstRealDatabase(t *testing.T) {
	db := openRealDatabaseCopy(t)

	ctx := context.Background()
	// Whichever login the database already has; the migrations are not
	// login-scoped, but the cleanup and boolean checks below need a real one.
	var loginID string
	_ = db.QueryRow(ctx, `SELECT login_id FROM cloud_chat LIMIT 1`).Scan(&loginID)

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

	legacySystemRows := 0
	if hasBody, _ := columnExists(ctx, db, "cloud_message", "has_body"); hasBody {
		if err := db.QueryRow(ctx, `
			SELECT COUNT(*) FROM cloud_message
			WHERE login_id=$1
			  AND COALESCE(attachments_json, '') = ''
			  AND tapback_type IS NULL
			  AND (
			    has_body=FALSE
			    OR (
			      text IS NOT NULL AND text <> ''
			      AND portal_id IN (
			        SELECT portal_id FROM cloud_chat c
			        WHERE c.login_id=$1
			          AND c.display_name IS NOT NULL AND c.display_name <> ''
			          AND c.display_name=cloud_message.text
			      )
			    )
			  )
		`, loginID).Scan(&legacySystemRows); err != nil {
			t.Fatalf("count intentional legacy system cleanup rows: %v", err)
		}
	}

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
		want := before
		if table == "cloud_message" {
			want -= legacySystemRows
		}
		if after != want {
			t.Errorf("%s row count after migration = %d, want %d (before=%d, intentional cleanup=%d)",
				table, after, want, before, legacySystemRows)
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
	db := openRealDatabaseCopy(t)

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
	if first == nil {
		// Every candidate was deleted or the backlog was empty, so no pass
		// ever loaded a set. Say so: the assertion below silently proves
		// nothing in that case, which is the steady state on a drained
		// database.
		t.Log("no pass loaded a delivered-ID set; the extension assertion did not run")
	} else if store.bridged != first {
		t.Fatal("second pass rebuilt the delivered-ID set on an unchanged database")
	}
	timed("scrubReactionText", func() (int64, error) { return store.scrubReactionText(ctx, bodyScrubGracePeriod) })

	var pendingCandidates int
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM cloud_message WHERE login_id=$1 AND body_scrubbed=FALSE`, loginID,
	).Scan(&pendingCandidates); err != nil {
		t.Fatal(err)
	}
	if first == nil {
		t.Logf("no scrub candidates, delivered-ID set never loaded; %d rows still unscrubbed", pendingCandidates)
		return
	}
	t.Logf("delivered-ID set: %d ids anchored at rowid %d; %d rows still unscrubbed",
		first.size(), first.maxRowID, pendingCandidates)
}

// TestCloudHousekeepingAgainstRealDatabase exercises the account-wide reads
// and cleanup writes that previously monopolized Keith's one SQLite connection.
// The individual operations are logged so regressions are visible before a
// branch is shipped to a slow host.
func TestCloudHousekeepingAgainstRealDatabase(t *testing.T) {
	db := openRealDatabaseCopy(t)
	ctx := context.Background()
	var loginID string
	if err := db.QueryRow(ctx, `SELECT login_id FROM cloud_message LIMIT 1`).Scan(&loginID); err != nil {
		t.Skipf("no cloud_message rows: %v", err)
	}
	store := newCloudBackfillStore(db, networkid.UserLoginID(loginID))

	timed := func(name string, fn func() (int, error)) {
		t.Helper()
		start := time.Now()
		n, err := fn()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Logf("%-36s %8d rows %10s", name, n, time.Since(start).Round(time.Millisecond))
	}
	timed("ensureSchema", func() (int, error) { return 0, store.ensureSchema(ctx) })
	timed("listPortalIDsWithNewestTimestamp", func() (int, error) {
		rows, err := store.listPortalIDsWithNewestTimestamp(ctx, false)
		return len(rows), err
	})
	timed("listPendingAttachmentMessages", func() (int, error) {
		rows, err := store.listPendingAttachmentMessages(ctx)
		return len(rows), err
	})
	timed("portalsFullyBackfilledNoNewContent", func() (int, error) {
		rows, err := store.portalsFullyBackfilledNoNewContent(ctx)
		return len(rows), err
	})
	timed("pruneOrphanedAttachmentCache", func() (int, error) {
		n, err := store.pruneOrphanedAttachmentCache(ctx)
		return int(n), err
	})
	timed("deleteOrphanedMessages", func() (int, error) {
		n, err := store.deleteOrphanedMessages(ctx)
		return int(n), err
	})

	// The scrub confirmation is only reached for rows a pass is about to
	// scrub, so a drained database never exercises it and an earlier version
	// of this benchmark reported nothing while the query took 1.5s per 200
	// guids in production. Drive it directly with real delivered ids.
	var bridgeID string
	if err := db.QueryRow(ctx, `SELECT bridge_id FROM message LIMIT 1`).Scan(&bridgeID); err != nil {
		t.Skipf("no bridgev2 message rows to confirm against: %v", err)
	}
	probe := make([]string, 0, 1000)
	rows, err := db.Query(ctx, `SELECT id FROM message WHERE bridge_id=$1 LIMIT 1000`, bridgeID)
	if err != nil {
		t.Fatalf("read ids to confirm: %v", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		_, normalized, _ := normalizeScrubGUID(id, true)
		probe = append(probe, strings.ToUpper(normalized))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	timed("confirmDeliveredGUIDs (1000 delivered)", func() (int, error) {
		confirmed, err := store.confirmDeliveredGUIDs(ctx, bridgeID, probe)
		return len(confirmed), err
	})
	missing := make([]string, 0, 1000)
	for i := 0; i < 1000; i++ {
		missing = append(missing, fmt.Sprintf("%08X-DEAD-4EAD-8EAD-DEADDEADDEAD", i))
	}
	timed("confirmDeliveredGUIDs (1000 absent)", func() (int, error) {
		confirmed, err := store.confirmDeliveredGUIDs(ctx, bridgeID, missing)
		return len(confirmed), err
	})
}

// openRealDatabaseCopy makes a standalone copy of the database named by
// CORTEN_TEST_REAL_DB and returns it wrapped in dbutil, or skips the test when
// the variable is unset. The copy is taken with VACUUM INTO on a read-only
// connection, so a source that still carries committed WAL frames is copied
// in full and is never written to; a byte copy of the main file alone would
// silently drop everything since its last checkpoint.
func openRealDatabaseCopy(t *testing.T) *dbutil.Database {
	t.Helper()
	src := os.Getenv("CORTEN_TEST_REAL_DB")
	if src == "" {
		t.Skip("set CORTEN_TEST_REAL_DB to a copy of a real bridge database to run this")
	}
	path := filepath.Join(t.TempDir(), "realdb.db")
	source, err := sql.Open("sqlite3", "file:"+src+"?mode=ro")
	if err != nil {
		t.Fatalf("open %s: %v", src, err)
	}
	if _, err := source.Exec(`VACUUM INTO ?`, path); err != nil {
		source.Close()
		t.Fatalf("copy %s with VACUUM INTO: %v", src, err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("close %s: %v", src, err)
	}

	raw, err := sql.Open("sqlite3", "file:"+path+"?_txlock=immediate")
	if err != nil {
		t.Fatalf("open copy: %v", err)
	}
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = raw.Close() })
	db, err := dbutil.NewWithDB(raw, "sqlite3")
	if err != nil {
		t.Fatalf("wrap copy in dbutil: %v", err)
	}
	return db
}
