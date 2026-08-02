package connector

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// These tests exercise the store SQL against a real SQLite database.
//
// They exist because the SQL in these stores had no coverage at all: every
// other test in this package is pure logic, so a statement could be a hard
// syntax error and the suite would still pass. That is how a batch of
// SQLite-only constructs (BLOB columns, pragma_table_info, instr(), literal
// "?" placeholders, INSERT OR IGNORE, ORDER BY rowid) survived in code that is
// also expected to run on Postgres.
//
// Making those portable meant touching statements that SQLite users depend on
// every day, so the point of these tests is the other direction: proving the
// portable spelling still behaves correctly on SQLite, which is the only
// database this bridge supports.

const testSQLLoginID = networkid.UserLoginID("test-login")

// newTestSQLiteDB opens a fresh file-backed SQLite database through dbutil,
// the same wrapper the bridge uses, so placeholder rewriting and dialect
// detection behave exactly as they do in production.
func newTestSQLiteDB(t *testing.T) *dbutil.Database {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	raw, err := sql.Open("sqlite3", "file:"+path+"?_txlock=immediate")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// One connection keeps the raw-*sql.Tx paths (beginTx → PrepareContext)
	// and dbutil's ctx-carried transactions from interleaving on separate
	// connections and tripping SQLITE_BUSY in a test.
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = raw.Close() })

	db, err := dbutil.NewWithDB(raw, "sqlite3")
	if err != nil {
		t.Fatalf("wrap sqlite in dbutil: %v", err)
	}
	if db.Dialect != dbutil.SQLite {
		t.Fatalf("expected SQLite dialect, got %v", db.Dialect)
	}
	return db
}

// TestEnsureSchemaIsIdempotent runs every store's ensureSchema twice.
//
// This is the direct regression guard for replacing the pragma_table_info()
// column check with the dialect-branched columnExists helper. The old code
// discarded the query error and treated "couldn't tell" as "column missing";
// if the new helper ever regressed to that, the second run would try to
// ALTER TABLE ADD COLUMN over columns that already exist and fail. A single
// run would not catch it — the failure only appears once the columns are
// there.
func TestEnsureSchemaIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)

	backfill := newCloudBackfillStore(db, testSQLLoginID)
	profiles := newSharedProfileStore(db, testSQLLoginID)
	pending := newPendingAttachmentStore(db, testSQLLoginID)

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

	// columnExists must actually report the migrated columns as present —
	// an ensureSchema that silently swallowed its ALTERs would also pass the
	// loop above.
	for _, tc := range []struct{ table, column string }{
		{"cloud_chat", "fwd_backfill_done"},
		{"cloud_chat", "is_filtered"},
		{"cloud_message", "body_scrubbed"},
		{"cloud_message", "reply_to_guid"},
	} {
		exists, err := columnExists(ctx, db, tc.table, tc.column)
		if err != nil {
			t.Fatalf("columnExists(%s.%s): %v", tc.table, tc.column, err)
		}
		if !exists {
			t.Errorf("columnExists(%s.%s) = false, want true", tc.table, tc.column)
		}
	}
	// And it must report a column that was never added as absent, or the
	// migration loop would skip real work on a fresh database.
	exists, err := columnExists(ctx, db, "cloud_message", "definitely_not_a_column")
	if err != nil {
		t.Fatalf("columnExists(absent): %v", err)
	}
	if exists {
		t.Error("columnExists reported a nonexistent column as present")
	}
}

// TestExistingDatabaseSurvivesTheDialectChanges is the upgrade path.
//
// Every other test here starts from an empty file, which only proves the new
// code works on a fresh install. Real users are upgrading a SQLite database
// that the OLD code created — declared BLOB, with fwd_backfill_done written as
// the integers 0 and 1 rather than booleans. This builds that database by
// hand, runs the new ensureSchema over it, and checks that the rows are still
// there and still match the boolean predicates the queries now use.
func TestExistingDatabaseSurvivesTheDialectChanges(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)

	// The pre-change schema, verbatim in the spellings the old code emitted.
	legacy := []string{
		`CREATE TABLE cloud_chat (
			login_id TEXT NOT NULL, cloud_chat_id TEXT NOT NULL,
			record_name TEXT NOT NULL DEFAULT '', group_id TEXT NOT NULL DEFAULT '',
			portal_id TEXT NOT NULL, service TEXT, display_name TEXT,
			group_photo_guid TEXT, participants_json TEXT, updated_ts BIGINT,
			created_ts BIGINT NOT NULL,
			deleted BOOLEAN NOT NULL DEFAULT FALSE,
			is_filtered INTEGER NOT NULL DEFAULT 0,
			fwd_backfill_done BOOLEAN NOT NULL DEFAULT 0,
			PRIMARY KEY (login_id, cloud_chat_id))`,
		`CREATE TABLE cloud_message (
			login_id TEXT NOT NULL, guid TEXT NOT NULL, chat_id TEXT, portal_id TEXT,
			timestamp_ms BIGINT NOT NULL, sender TEXT, is_from_me BOOLEAN NOT NULL,
			text TEXT, subject TEXT, service TEXT,
			deleted BOOLEAN NOT NULL DEFAULT FALSE,
			tapback_type INTEGER, tapback_target_guid TEXT, tapback_emoji TEXT,
			attachments_json TEXT, created_ts BIGINT NOT NULL, updated_ts BIGINT NOT NULL,
			PRIMARY KEY (login_id, guid))`,
		`CREATE TABLE group_photo_cache (
			login_id TEXT NOT NULL, portal_id TEXT NOT NULL, ts BIGINT NOT NULL,
			data BLOB NOT NULL, PRIMARY KEY (login_id, portal_id))`,
		`CREATE TABLE shared_profiles (
			login_id TEXT NOT NULL, identifier TEXT NOT NULL,
			display_name TEXT NOT NULL DEFAULT '', first_name TEXT NOT NULL DEFAULT '',
			last_name TEXT NOT NULL DEFAULT '', avatar BLOB, record_key TEXT NOT NULL,
			decryption_key BLOB NOT NULL, has_poster BOOLEAN NOT NULL DEFAULT FALSE,
			updated_ts BIGINT NOT NULL, PRIMARY KEY (login_id, identifier))`,
	}
	for _, q := range legacy {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatalf("create legacy schema: %v", err)
		}
	}

	// Legacy rows: fwd_backfill_done stored as the integers 1 and 0, and a
	// BLOB avatar, exactly as the old code wrote them.
	now := time.Now().UnixMilli()
	if _, err := db.Exec(ctx, `
		INSERT INTO cloud_chat (login_id, cloud_chat_id, portal_id, created_ts, fwd_backfill_done)
		VALUES ($1, $2, $3, $4, 1)`,
		testSQLLoginID, "legacy-done", "gid:legacy-done", now); err != nil {
		t.Fatalf("insert legacy done chat: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO cloud_chat (login_id, cloud_chat_id, portal_id, created_ts, fwd_backfill_done)
		VALUES ($1, $2, $3, $4, 0)`,
		testSQLLoginID, "legacy-pending", "gid:legacy-pending", now); err != nil {
		t.Fatalf("insert legacy pending chat: %v", err)
	}
	legacyAvatar := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	if _, err := db.Exec(ctx, `
		INSERT INTO shared_profiles (login_id, identifier, record_key, decryption_key, avatar, updated_ts)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		testSQLLoginID, "tel:+15551111111", "rk", []byte{0x01}, legacyAvatar, now); err != nil {
		t.Fatalf("insert legacy profile: %v", err)
	}

	// Upgrade in place, twice, the way a restart would.
	backfill := newCloudBackfillStore(db, testSQLLoginID)
	profiles := newSharedProfileStore(db, testSQLLoginID)
	for pass := 1; pass <= 2; pass++ {
		if err := backfill.ensureSchema(ctx); err != nil {
			t.Fatalf("ensureSchema over legacy DB, pass %d: %v", pass, err)
		}
		if err := profiles.ensureSchema(ctx); err != nil {
			t.Fatalf("sharedProfileStore.ensureSchema over legacy DB, pass %d: %v", pass, err)
		}
	}

	// The integer-valued fwd_backfill_done rows must still satisfy the
	// boolean predicates the queries now use.
	//
	// Honest about what this proves: on SQLite, TRUE and FALSE are literally
	// aliases for 1 and 0, so this assertion cannot fail there and reverting
	// the boolean change would not break it. It is here to pin that the
	// change is a genuine NO-OP on the supported database — which is the
	// claim being made — not to catch a SQLite regression. The change exists
	// for Postgres, where `BOOLEAN = 1` is a hard error and no test here can
	// reach.
	if !backfill.isForwardBackfillDone(ctx, "gid:legacy-done") {
		t.Error("legacy fwd_backfill_done=1 row no longer matches fwd_backfill_done=TRUE")
	}
	if backfill.isForwardBackfillDone(ctx, "gid:legacy-pending") {
		t.Error("legacy fwd_backfill_done=0 row wrongly matches fwd_backfill_done=TRUE")
	}
	done, err := backfill.getForwardBackfillDonePortals(ctx)
	if err != nil {
		t.Fatalf("getForwardBackfillDonePortals: %v", err)
	}
	if !done["gid:legacy-done"] || done["gid:legacy-pending"] {
		t.Errorf("getForwardBackfillDonePortals = %v, want only gid:legacy-done", done)
	}

	// Legacy BLOB data must read back byte-identical after migration. Note
	// that sqlBlobType returns "BLOB" on SQLite, i.e. the declaration is
	// unchanged here — that is deliberate, and this asserts it, rather than
	// asserting that some new SQLite type name round-trips.
	rows, err := profiles.loadAll(ctx)
	if err != nil {
		t.Fatalf("loadAll over legacy DB: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("loadAll returned %d rows, want 1", len(rows))
	}
	if string(rows[0].Avatar) != string(legacyAvatar) {
		t.Errorf("legacy avatar read back as %v, want %v", rows[0].Avatar, legacyAvatar)
	}
}

// TestBatchUpsertsUseWorkingPlaceholders covers upsertMessageBatch and
// upsertChatBatch, the two statements that run through tx.PrepareContext on a
// raw *sql.Tx and therefore never pass through dbutil's $N → ?N rewrite. Their
// placeholders are generated per dialect by sqlPlaceholders; if that produced
// the wrong spelling, the prepare or the bind would fail here.
func TestBatchUpsertsUseWorkingPlaceholders(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}

	now := time.Now().UnixMilli()
	if err := store.upsertChatBatch(ctx, []cloudChatUpsertRow{{
		CloudChatID:      "chat-1",
		RecordName:       "rec-chat-1",
		GroupID:          "group-1",
		PortalID:         "gid:portal-1",
		Service:          "iMessage",
		DisplayName:      "Test Group",
		ParticipantsJSON: `["tel:+15551111111"]`,
		UpdatedTS:        now,
		IsFiltered:       0,
	}}); err != nil {
		t.Fatalf("upsertChatBatch: %v", err)
	}

	tapbackType := uint32(2000)
	rows := []cloudMessageRow{
		{
			GUID: "guid-1", RecordName: "rec-1", CloudChatID: "chat-1",
			PortalID: "gid:portal-1", TimestampMS: now, Sender: "tel:+15551111111",
			Text: "hello", Service: "iMessage", HasBody: true,
		},
		{
			GUID: "guid-2", RecordName: "rec-2", CloudChatID: "chat-1",
			PortalID: "gid:portal-1", TimestampMS: now + 1, IsFromMe: true,
			Text: "reply", Service: "iMessage", HasBody: true,
			ReplyToGUID: "guid-1", ReplyToPart: "0",
		},
		{
			GUID: "guid-3", CloudChatID: "chat-1", PortalID: "gid:portal-1",
			TimestampMS: now + 2, Sender: "tel:+15551111111",
			TapbackType: &tapbackType, TapbackTargetGUID: "guid-2",
			TapbackEmoji: "", Service: "iMessage",
		},
	}
	if err := store.upsertMessageBatch(ctx, rows); err != nil {
		t.Fatalf("upsertMessageBatch: %v", err)
	}
	// Re-running must take the ON CONFLICT path rather than erroring.
	if err := store.upsertMessageBatch(ctx, rows); err != nil {
		t.Fatalf("upsertMessageBatch (conflict path): %v", err)
	}

	var count int
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM cloud_message WHERE login_id=$1`, testSQLLoginID,
	).Scan(&count); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if count != 3 {
		t.Errorf("cloud_message row count = %d, want 3", count)
	}

	var text string
	if err := db.QueryRow(ctx,
		`SELECT text FROM cloud_message WHERE login_id=$1 AND guid=$2`, testSQLLoginID, "guid-1",
	).Scan(&text); err != nil {
		t.Fatalf("read back message text: %v", err)
	}
	if text != "hello" {
		t.Errorf("text = %q, want %q — placeholders may be binding out of order", text, "hello")
	}
}

// TestInsertOrIgnoreReplacementsAreNoOpOnConflict covers the four statements
// that changed from SQLite's INSERT OR IGNORE to the portable
// ON CONFLICT DO NOTHING. The conflict target has to name the right unique
// index or the insert errors instead of quietly doing nothing.
func TestInsertOrIgnoreReplacementsAreNoOpOnConflict(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	now := time.Now().UnixMilli()

	for i := 0; i < 2; i++ {
		if err := store.persistMessageUUID(ctx, "uuid-msg", "gid:portal-1", now, true); err != nil {
			t.Fatalf("persistMessageUUID call %d: %v", i+1, err)
		}
		if err := store.persistTapbackUUID(ctx, "uuid-tap", "gid:portal-1", now, false, 2000); err != nil {
			t.Fatalf("persistTapbackUUID call %d: %v", i+1, err)
		}
		if err := store.insertDeletedChatTombstone(
			ctx, "chat-tomb", "gid:portal-2", "rec", "grp", "iMessage", nil, "[]",
		); err != nil {
			t.Fatalf("insertDeletedChatTombstone call %d: %v", i+1, err)
		}
		// Stub-insert path: no cloud_message row exists for this guid, so
		// softDeleteMessageByGUID falls through to the ON CONFLICT insert.
		if err := store.softDeleteMessageByGUID(ctx, "uuid-never-synced"); err != nil {
			t.Fatalf("softDeleteMessageByGUID call %d: %v", i+1, err)
		}
		// markForwardBackfillDone's synthetic-row fallback.
		store.markForwardBackfillDone(ctx, "gid:portal-3")
	}

	for _, uuid := range []string{"uuid-msg", "uuid-tap", "uuid-never-synced"} {
		ok, err := store.hasMessageUUID(ctx, uuid)
		if err != nil {
			t.Fatalf("hasMessageUUID(%s): %v", uuid, err)
		}
		if !ok {
			t.Errorf("hasMessageUUID(%s) = false, want true", uuid)
		}
	}

	// The repeated calls must not have duplicated rows.
	var msgCount int
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM cloud_message WHERE login_id=$1`, testSQLLoginID,
	).Scan(&msgCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgCount != 3 {
		t.Errorf("cloud_message row count = %d, want 3 (duplicate inserts leaked)", msgCount)
	}

	// fwd_backfill_done round-trips through boolean literals rather than 0/1.
	if !store.isForwardBackfillDone(ctx, "gid:portal-3") {
		t.Error("isForwardBackfillDone(gid:portal-3) = false, want true")
	}
	done, err := store.getForwardBackfillDonePortals(ctx)
	if err != nil {
		t.Fatalf("getForwardBackfillDonePortals: %v", err)
	}
	if !done["gid:portal-3"] {
		t.Errorf("getForwardBackfillDonePortals missing gid:portal-3, got %v", done)
	}
}

// TestGetConversationReadByMeMatchesTheReceiptTarget guards the ORDER BY.
//
// The old tiebreaker was SQLite's implicit rowid; Postgres has no rowid, so it
// had to change. What it must change TO is `guid DESC` alone, matching every
// paging query in this file (listLatestMessages, listForwardMessages, and the
// windowed variants) — because those choose the message that a read receipt
// actually targets, and this query decides whether that receipt is sent at all.
// If the two orderings disagree, the bridge decides "not read by me" about a
// different message than the one it would have acknowledged.
//
// Adding `created_ts DESC` ahead of guid looks harmless and is not: created_ts
// is stamped once per batch, so it is less unique than guid, and it overrides
// guid rather than backing it up. This test constructs exactly that
// disagreement, so it fails if created_ts is reintroduced into the ORDER BY.
//
// The cross-check calls listLatestMessages itself rather than re-typing its
// ORDER BY, so changing the two apart is caught. It does NOT cover the other
// ways these can disagree (the paging queries' `record_name <> ”` filter, and
// lastMsg being a capped slice index) — those are pre-existing and documented
// on getConversationReadByMe, not fixed here.
func TestGetConversationReadByMeMatchesTheReceiptTarget(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}

	// Two messages tied on timestamp_ms. guid order and created_ts order
	// point at DIFFERENT rows with different directions:
	//   bbbb (outgoing) — highest guid, but the OLDER created_ts
	//   aaaa (incoming) — lowest guid, but the NEWER created_ts
	// Ordering by guid selects bbbb -> read by me. Ordering by created_ts
	// first selects aaaa -> not read by me. Only the former agrees with the
	// paging queries, which order by (timestamp_ms, guid).
	// record_name must be non-empty: the paging queries filter
	// `record_name <> ''` to skip persistMessageUUID's echo stubs, so rows
	// without it are invisible to the very query this test cross-checks
	// against.
	ts := time.Now().UnixMilli()
	for _, m := range []struct {
		guid      string
		isFromMe  bool
		createdTS int64
	}{
		{"bbbb-outgoing", true, ts - 1000},
		{"aaaa-incoming", false, ts + 1000},
	} {
		if _, err := db.Exec(ctx, `
			INSERT INTO cloud_message (login_id, guid, record_name, portal_id, timestamp_ms, is_from_me, created_ts, updated_ts)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
		`, testSQLLoginID, m.guid, "rec-"+m.guid, "gid:portal-1", ts, m.isFromMe, m.createdTS); err != nil {
			t.Fatalf("insert %s: %v", m.guid, err)
		}
	}

	// Cross-check against the REAL paging query rather than a hand-copied
	// ORDER BY, so this notices if the two are ever changed apart.
	// listLatestMessages is what feeds client.go's lastMsg, i.e. the message a
	// read receipt would actually target.
	latest, err := store.listLatestMessages(ctx, "gid:portal-1", 1)
	if err != nil {
		t.Fatalf("listLatestMessages: %v", err)
	}
	if len(latest) != 1 {
		t.Fatalf("listLatestMessages returned %d rows, want 1 — the fixture is invisible to "+
			"the paging queries, so this test would prove nothing", len(latest))
	}
	receiptTarget := latest[0].GUID
	if receiptTarget != "bbbb-outgoing" {
		t.Fatalf("test setup wrong: paging order picked %q, expected bbbb-outgoing", receiptTarget)
	}

	first, err := store.getConversationReadByMe(ctx, "gid:portal-1")
	if err != nil {
		t.Fatalf("getConversationReadByMe: %v", err)
	}
	if !first {
		t.Error("getConversationReadByMe = false, but the message a receipt would target " +
			"(bbbb-outgoing) is from me — the ORDER BY disagrees with the paging queries")
	}
	for i := 0; i < 5; i++ {
		got, err := store.getConversationReadByMe(ctx, "gid:portal-1")
		if err != nil {
			t.Fatalf("getConversationReadByMe repeat %d: %v", i, err)
		}
		if got != first {
			t.Fatalf("getConversationReadByMe is non-deterministic: %v then %v", first, got)
		}
	}
}

// TestInstrDialectHelperQueriesRun covers the two statements whose SQLite
// instr() became a per-dialect substitution. Both are large UPDATEs whose only
// realistic failure mode is "the SQL doesn't parse", so running them at all is
// the assertion; healMisroutedGroupMessages additionally gets a row that it
// should actually repair.
func TestInstrDialectHelperQueriesRun(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}

	// bridgev2's message table, which scrubBridgedBodies joins against. Only
	// the three columns the query touches are needed.
	if _, err := db.Exec(ctx, `CREATE TABLE IF NOT EXISTS message (
		id TEXT NOT NULL,
		bridge_id TEXT NOT NULL,
		room_receiver TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatalf("create message table: %v", err)
	}

	now := time.Now().UnixMilli()
	// A chat whose portal the message below should be re-pointed at, and a
	// message misrouted to a different portal with a ";+;<hex>" chat_id.
	if err := store.upsertChatBatch(ctx, []cloudChatUpsertRow{{
		CloudChatID: "DEADBEEF", PortalID: "gid:right-portal",
		Service: "iMessage", ParticipantsJSON: "[]", UpdatedTS: now,
	}}); err != nil {
		t.Fatalf("upsertChatBatch: %v", err)
	}
	if err := store.upsertMessageBatch(ctx, []cloudMessageRow{{
		GUID: "GUID-MISROUTED", CloudChatID: "DEADBEEF",
		PortalID:    "gid:wrong-portal",
		TimestampMS: now, Text: "misrouted", Service: "iMessage", HasBody: true,
	}}); err != nil {
		t.Fatalf("upsertMessageBatch: %v", err)
	}
	// chat_id is not a field on cloudMessageRow, so set the ";+;" form directly.
	if _, err := db.Exec(ctx,
		`UPDATE cloud_message SET chat_id=$1 WHERE login_id=$2 AND guid=$3`,
		"iMessage;+;DEADBEEF", testSQLLoginID, "GUID-MISROUTED",
	); err != nil {
		t.Fatalf("set chat_id: %v", err)
	}

	n, err := store.healMisroutedGroupMessages(ctx)
	if err != nil {
		t.Fatalf("healMisroutedGroupMessages: %v", err)
	}
	if n != 1 {
		t.Errorf("healMisroutedGroupMessages repaired %d rows, want 1", n)
	}
	var portalID string
	if err := db.QueryRow(ctx,
		`SELECT portal_id FROM cloud_message WHERE login_id=$1 AND guid=$2`,
		testSQLLoginID, "GUID-MISROUTED",
	).Scan(&portalID); err != nil {
		t.Fatalf("read back portal_id: %v", err)
	}
	if portalID != "gid:right-portal" {
		t.Errorf("portal_id = %q, want %q", portalID, "gid:right-portal")
	}

	// scrubBridgedBodies: a bridged message (part-suffixed id, so the
	// instr()-based normalisation is what has to match it) older than the
	// grace window must have its body cleared.
	if _, err := db.Exec(ctx,
		`INSERT INTO message (id, bridge_id, room_receiver) VALUES ($1, $2, $3)`,
		"GUID-MISROUTED_1", "test-bridge", string(testSQLLoginID),
	); err != nil {
		t.Fatalf("insert bridgev2 message row: %v", err)
	}
	if _, err := db.Exec(ctx,
		`UPDATE cloud_message SET updated_ts=$1 WHERE login_id=$2`,
		now-int64(time.Hour/time.Millisecond), testSQLLoginID,
	); err != nil {
		t.Fatalf("age messages: %v", err)
	}

	scrubbed, err := store.scrubBridgedBodies(ctx, "test-bridge", time.Minute, nil)
	if err != nil {
		t.Fatalf("scrubBridgedBodies: %v", err)
	}
	if scrubbed != 1 {
		t.Errorf("scrubBridgedBodies scrubbed %d rows, want 1", scrubbed)
	}
	var scrubbedText sql.NullString
	if err := db.QueryRow(ctx,
		`SELECT text FROM cloud_message WHERE login_id=$1 AND guid=$2`,
		testSQLLoginID, "GUID-MISROUTED",
	).Scan(&scrubbedText); err != nil {
		t.Fatalf("read back scrubbed text: %v", err)
	}
	if scrubbedText.Valid {
		t.Errorf("text = %q after scrub, want NULL", scrubbedText.String)
	}
}

// TestPendingAttachmentStoreRoundTrip covers pending_attachment_store.go,
// where every one of the seven queries used literal "?" placeholders. GetDue
// runs on the attachment retrier's 10s tick, so a broken statement there is a
// continuous error in production rather than a rare one.
func TestPendingAttachmentStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newPendingAttachmentStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}

	now := time.Now().Truncate(time.Millisecond)
	row := &pendingAttachmentRow{
		LoginID:        testSQLLoginID,
		MessageGUID:    "guid-1",
		AttIndex:       0,
		AttID:          "guid-1_att0",
		PortalID:       "gid:portal-1",
		Sender:         "tel:+15551111111",
		TimestampMs:    now.UnixMilli(),
		Filename:       "photo.heic",
		MimeType:       "image/heic",
		UtiType:        "public.heic",
		SizeBytes:      1234,
		MmcsDescriptor: `{"signature":"abc"}`,
		CreatedAt:      now,
		NextAttemptAt:  now.Add(-time.Minute),
	}
	if err := store.Insert(ctx, row); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// Second insert must hit ON CONFLICT DO NOTHING, not error.
	if err := store.Insert(ctx, row); err != nil {
		t.Fatalf("Insert (conflict path): %v", err)
	}

	due, err := store.GetDue(ctx, now, 10)
	if err != nil {
		t.Fatalf("GetDue: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("GetDue returned %d rows, want 1", len(due))
	}
	if due[0].Filename != "photo.heic" || due[0].AttID != "guid-1_att0" {
		t.Errorf("GetDue returned %+v, want the inserted row", due[0])
	}

	row.AttemptCount = 3
	row.LastAttemptAt = now
	row.NextAttemptAt = now.Add(time.Hour)
	if err := store.UpdateSchedule(ctx, row); err != nil {
		t.Fatalf("UpdateSchedule: %v", err)
	}
	got, err := store.GetOne(ctx, "guid-1", 0)
	if err != nil {
		t.Fatalf("GetOne: %v", err)
	}
	if got.AttemptCount != 3 {
		t.Errorf("AttemptCount = %d after UpdateSchedule, want 3", got.AttemptCount)
	}

	// Now scheduled an hour out, so it is no longer due.
	due, err = store.GetDue(ctx, now, 10)
	if err != nil {
		t.Fatalf("GetDue after reschedule: %v", err)
	}
	if len(due) != 0 {
		t.Errorf("GetDue returned %d rows after reschedule, want 0", len(due))
	}

	if err := store.Delete(ctx, row); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// GetOne reports "no such row" as (nil, nil), not an error.
	gone, err := store.GetOne(ctx, "guid-1", 0)
	if err != nil {
		t.Fatalf("GetOne after Delete: %v", err)
	}
	if gone != nil {
		t.Errorf("GetOne returned %+v after Delete, want nil", gone)
	}
}

// TestSharedProfileStoreRoundTrip covers the BLOB → dialect-typed binary
// columns in shared_profiles: []byte values must survive a write/read cycle
// unmodified.
func TestSharedProfileStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newSharedProfileStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}

	avatar := []byte{0x89, 0x50, 0x4E, 0x47, 0x00, 0xFF, 0xFE}
	decryptionKey := []byte{0x00, 0x01, 0x02, 0x03, 0xFF}
	if err := store.save(ctx, &sharedProfileRow{
		Identifier:    "tel:+15551111111",
		DisplayName:   "Test Person",
		FirstName:     "Test",
		LastName:      "Person",
		Avatar:        avatar,
		RecordKey:     "record-key",
		DecryptionKey: decryptionKey,
		HasPoster:     true,
		UpdatedTS:     time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	rows, err := store.loadAll(ctx)
	if err != nil {
		t.Fatalf("loadAll: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("loadAll returned %d rows, want 1", len(rows))
	}
	if string(rows[0].Avatar) != string(avatar) {
		t.Errorf("avatar round-trip = %v, want %v", rows[0].Avatar, avatar)
	}
	if string(rows[0].DecryptionKey) != string(decryptionKey) {
		t.Errorf("decryption_key round-trip = %v, want %v", rows[0].DecryptionKey, decryptionKey)
	}
}

// TestDialectHelpersSpellEachDialectCorrectly pins the helper output for both
// dialects without needing a live Postgres. A regression here would be
// invisible to every SQLite-backed test above.
func TestDialectHelpersSpellEachDialectCorrectly(t *testing.T) {
	sqlite := &dbutil.Database{Dialect: dbutil.SQLite}
	postgres := &dbutil.Database{Dialect: dbutil.Postgres}

	if got := sqlPlaceholders(sqlite, 3); got != "?, ?, ?" {
		t.Errorf("sqlPlaceholders(SQLite, 3) = %q, want %q", got, "?, ?, ?")
	}
	if got := sqlPlaceholders(postgres, 3); got != "$1, $2, $3" {
		t.Errorf("sqlPlaceholders(Postgres, 3) = %q, want %q", got, "$1, $2, $3")
	}
	if got := sqlPlaceholders(postgres, 12); got != "$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12" {
		t.Errorf("sqlPlaceholders(Postgres, 12) = %q", got)
	}
	if got := sqlInstrFunc(sqlite); got != "instr" {
		t.Errorf("sqlInstrFunc(SQLite) = %q, want instr", got)
	}
	if got := sqlInstrFunc(postgres); got != "strpos" {
		t.Errorf("sqlInstrFunc(Postgres) = %q, want strpos", got)
	}
	if got := sqlBlobType(sqlite); got != "BLOB" {
		t.Errorf("sqlBlobType(SQLite) = %q, want BLOB", got)
	}
	if got := sqlBlobType(postgres); got != "BYTEA" {
		t.Errorf("sqlBlobType(Postgres) = %q, want BYTEA", got)
	}
}
