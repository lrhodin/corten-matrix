package connector

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

func createScrubberBridgeMessageTable(t *testing.T, db *dbutil.Database, ctx context.Context) {
	t.Helper()
	if _, err := db.Exec(ctx, `CREATE TABLE IF NOT EXISTS message (
		id TEXT NOT NULL,
		bridge_id TEXT NOT NULL,
		room_receiver TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatalf("create message table: %v", err)
	}
}

func insertScrubberBridgeMessage(t *testing.T, db *dbutil.Database, ctx context.Context, id, bridgeID, receiver string) {
	t.Helper()
	if _, err := db.Exec(ctx,
		`INSERT INTO message (id, bridge_id, room_receiver) VALUES ($1, $2, $3)`,
		id, bridgeID, receiver,
	); err != nil {
		t.Fatalf("insert bridgev2 message row %q: %v", id, err)
	}
}

// TestEnsureSchemaCreatesScrubIndex verifies that the privacy scrubbers can
// find old, un-scrubbed rows without scanning the entire cloud_message table.
func TestEnsureSchemaCreatesScrubIndex(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}

	var indexSQL string
	if err := db.QueryRow(ctx,
		`SELECT COALESCE(sql, '') FROM sqlite_master WHERE type='index' AND name='cloud_message_scrub_idx'`,
	).Scan(&indexSQL); err != nil {
		t.Fatalf("read cloud_message_scrub_idx: %v", err)
	}
	if !strings.Contains(indexSQL, "WHERE body_scrubbed") {
		t.Fatalf("cloud_message_scrub_idx is not partial: %s", indexSQL)
	}

	if err := store.upsertMessageBatch(ctx, []cloudMessageRow{{
		GUID: "plan-check-guid", PortalID: "gid:plan", TimestampMS: 1,
		Service: "iMessage",
	}}); err != nil {
		t.Fatalf("upsertMessageBatch: %v", err)
	}
	if _, err := db.Exec(ctx, `ANALYZE`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}
	rows, err := db.Query(ctx,
		`EXPLAIN QUERY PLAN SELECT guid FROM cloud_message
		 WHERE login_id=$1 AND body_scrubbed=FALSE AND updated_ts < $2`,
		testSQLLoginID, time.Now().UnixMilli(),
	)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan rows: %v", err)
	}
	if got := plan.String(); !strings.Contains(got, "cloud_message_scrub_idx") {
		t.Fatalf("candidate query does not use cloud_message_scrub_idx; plan:\n%s", got)
	}
}

func TestLoadBridgedGUIDSetNormalizesAndScopesIDs(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	createScrubberBridgeMessageTable(t, db, ctx)

	otherLogin := networkid.UserLoginID("other-login")
	insertScrubberBridgeMessage(t, db, ctx, "ABC-1", "bridge", string(testSQLLoginID))
	insertScrubberBridgeMessage(t, db, ctx, "guid-2_att0", "bridge", "")
	insertScrubberBridgeMessage(t, db, ctx, strings.ToUpper("guid-3"), "bridge", string(testSQLLoginID))
	insertScrubberBridgeMessage(t, db, ctx, "wrong-bridge", "other-bridge", string(testSQLLoginID))
	insertScrubberBridgeMessage(t, db, ctx, "wrong-login", "bridge", string(otherLogin))

	set, err := store.loadBridgedGUIDSet(ctx, "bridge")
	if err != nil {
		t.Fatalf("loadBridgedGUIDSet: %v", err)
	}
	for guid, want := range map[string]bool{
		"abc-1":        true,
		"guid-2":       true,
		"guid-3":       true,
		"wrong-bridge": false,
		"wrong-login":  false,
	} {
		_, got := set[guid]
		if got != want {
			t.Errorf("set contains %q = %v, want %v", guid, got, want)
		}
	}
}

func TestScrubBridgedBodiesMultiChunkPreservesEligibility(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	createScrubberBridgeMessageTable(t, db, ctx)

	const bridgeID = "test-bridge"
	now := time.Now().UnixMilli()
	old := now - int64(time.Hour/time.Millisecond)

	bulk := make([]cloudMessageRow, 0, 2500)
	for i := 0; i < 2500; i++ {
		guid := fmt.Sprintf("delivered-%04d", i)
		bulk = append(bulk, cloudMessageRow{
			GUID: guid, PortalID: "gid:bulk", TimestampMS: old,
			Text: "secret " + guid, Sender: "tel:+1555", Service: "iMessage", HasBody: true,
		})
		insertScrubberBridgeMessage(t, db, ctx, guid, bridgeID, string(testSQLLoginID))
	}
	if err := store.upsertMessageBatch(ctx, bulk); err != nil {
		t.Fatalf("upsert bulk messages: %v", err)
	}

	tapbackType := uint32(2001)
	special := []cloudMessageRow{
		{GUID: "fresh-delivered", PortalID: "gid:bulk", TimestampMS: now,
			Text: "fresh secret", Service: "iMessage", HasBody: true},
		{GUID: "undelivered-old", PortalID: "gid:bulk", TimestampMS: old,
			Text: "undelivered secret", Service: "iMessage", HasBody: true},
		{GUID: "tapback-old", PortalID: "gid:bulk", TimestampMS: old,
			Text: "Loved 'x'", Service: "iMessage", TapbackType: &tapbackType},
		{GUID: "deleted-old", PortalID: "gid:bulk", TimestampMS: old,
			Text: "deleted secret", Deleted: true, Service: "iMessage", HasBody: true},
		{GUID: "restore-portal-row", PortalID: "gid:restore", TimestampMS: old,
			Text: "restoring secret", Service: "iMessage", HasBody: true},
	}
	insertScrubberBridgeMessage(t, db, ctx, "fresh-delivered", bridgeID, string(testSQLLoginID))
	insertScrubberBridgeMessage(t, db, ctx, "restore-portal-row", bridgeID, string(testSQLLoginID))
	if err := store.upsertMessageBatch(ctx, special); err != nil {
		t.Fatalf("upsert special messages: %v", err)
	}
	if _, err := db.Exec(ctx,
		`UPDATE cloud_message SET updated_ts=$1 WHERE login_id=$2 AND guid <> $3`,
		old, testSQLLoginID, "fresh-delivered",
	); err != nil {
		t.Fatalf("age messages: %v", err)
	}

	textOf := func(guid string) sql.NullString {
		t.Helper()
		var text sql.NullString
		if err := db.QueryRow(ctx,
			`SELECT text FROM cloud_message WHERE login_id=$1 AND guid=$2`,
			testSQLLoginID, guid,
		).Scan(&text); err != nil {
			t.Fatalf("read text of %s: %v", guid, err)
		}
		return text
	}

	total, err := store.scrubBridgedBodies(ctx, bridgeID, time.Minute, []string{"gid:restore"})
	if err != nil {
		t.Fatalf("scrubBridgedBodies: %v", err)
	}
	if total != 2501 {
		t.Fatalf("scrubbed %d rows, want 2501", total)
	}
	for _, guid := range []string{"fresh-delivered", "undelivered-old", "restore-portal-row", "tapback-old"} {
		if text := textOf(guid); !text.Valid || text.String == "" {
			t.Errorf("%s text was cleared, want preserved", guid)
		}
	}
	if text := textOf("deleted-old"); text.Valid && text.String != "" {
		t.Errorf("deleted-old text = %q, want NULL", text.String)
	}

	again, err := store.scrubBridgedBodies(ctx, bridgeID, time.Minute, []string{"gid:restore"})
	if err != nil {
		t.Fatalf("second scrubBridgedBodies: %v", err)
	}
	if again != 0 {
		t.Fatalf("second pass scrubbed %d rows, want 0", again)
	}
}

func TestScrubBatchRechecksPendingBackfillAtWriteTime(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}

	now := time.Now().UnixMilli()
	old := now - int64(time.Hour/time.Millisecond)
	const portalID = "gid:newly-pending"
	const guid = "pending-race-guid"
	if err := store.upsertMessageBatch(ctx, []cloudMessageRow{{
		GUID: guid, PortalID: portalID, CloudChatID: "pending-race-chat",
		TimestampMS: old, Text: "must survive", Service: "iMessage", HasBody: true,
	}}); err != nil {
		t.Fatalf("upsert message: %v", err)
	}
	if _, err := db.Exec(ctx,
		`UPDATE cloud_message SET updated_ts=$1 WHERE login_id=$2 AND guid=$3`,
		old, testSQLLoginID, guid,
	); err != nil {
		t.Fatalf("age message: %v", err)
	}

	cutoff := now - int64(time.Minute/time.Millisecond)
	candidates, err := store.scrubCandidates(ctx, cutoff, nil)
	if err != nil {
		t.Fatalf("scrubCandidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %v, want one row", candidates)
	}

	// The portal becomes known-pending after candidate enumeration but before
	// the UPDATE. The write-time gate must prevent stale candidate state from
	// deleting plaintext that the first forward backfill still needs.
	if err := store.upsertChatBatch(ctx, []cloudChatUpsertRow{{
		CloudChatID: "pending-race-chat", PortalID: portalID, Service: "iMessage",
		ParticipantsJSON: "[]", UpdatedTS: now,
	}}); err != nil {
		t.Fatalf("upsert pending chat: %v", err)
	}
	scrubbed, err := store.scrubBatchIfEligible(ctx, cutoff,
		map[string]struct{}{guid: {}}, candidates)
	if err != nil {
		t.Fatalf("scrubBatchIfEligible: %v", err)
	}
	if scrubbed != 0 {
		t.Fatalf("scrubbed %d rows, want 0 after portal became pending", scrubbed)
	}
	var text sql.NullString
	if err := db.QueryRow(ctx,
		`SELECT text FROM cloud_message WHERE login_id=$1 AND guid=$2`,
		testSQLLoginID, guid,
	).Scan(&text); err != nil {
		t.Fatalf("read message text: %v", err)
	}
	if !text.Valid || text.String != "must survive" {
		t.Fatalf("message text = %q (valid=%v), want preserved", text.String, text.Valid)
	}
}
