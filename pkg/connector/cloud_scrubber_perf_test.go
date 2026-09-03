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
		`SELECT COALESCE(sql, '') FROM sqlite_master WHERE type='index' AND name='cloud_message_scrub_cover_idx'`,
	).Scan(&indexSQL); err != nil {
		t.Fatalf("read cloud_message_scrub_cover_idx: %v", err)
	}
	if !strings.Contains(indexSQL, "WHERE body_scrubbed") {
		t.Fatalf("cloud_message_scrub_cover_idx is not partial: %s", indexSQL)
	}

	// A database that already carries the earlier two-column index must end
	// up with only the covering one.
	if _, err := db.Exec(ctx, `CREATE INDEX cloud_message_scrub_idx
		ON cloud_message (login_id, updated_ts) WHERE body_scrubbed=FALSE`); err != nil {
		t.Fatalf("create legacy index: %v", err)
	}
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("second ensureSchema: %v", err)
	}
	var legacy int
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='cloud_message_scrub_idx'`,
	).Scan(&legacy); err != nil {
		t.Fatalf("count legacy index: %v", err)
	}
	if legacy != 0 {
		t.Fatal("legacy cloud_message_scrub_idx survived ensureSchema")
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
		`EXPLAIN QUERY PLAN SELECT guid, COALESCE(deleted, FALSE), COALESCE(portal_id, '')
		 FROM cloud_message
		 WHERE login_id=$1 AND body_scrubbed=FALSE
		   AND (tapback_type IS NULL OR tapback_type < 2000) AND updated_ts < $2
		 ORDER BY updated_ts ASC`,
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
	if got := plan.String(); !strings.Contains(got, "COVERING INDEX cloud_message_scrub_cover_idx") {
		t.Fatalf("candidate query is not served from cloud_message_scrub_cover_idx alone; plan:\n%s", got)
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
		"ABC-1":        true,
		"guid-2":       true,
		"guid-3":       true,
		"wrong-bridge": false,
		"wrong-login":  false,
	} {
		if got := set.contains(guid); got != want {
			t.Errorf("set contains %q = %v, want %v", guid, got, want)
		}
	}
	if set.size() != 3 {
		t.Errorf("set size = %d, want 3", set.size())
	}
}

func TestNormalizeScrubGUIDPacksUUIDs(t *testing.T) {
	const upper = "0F0E0D0C-0B0A-0908-0706-050403020100"
	key, _, packed := normalizeScrubGUID(upper, false)
	if !packed {
		t.Fatalf("%s was not packed", upper)
	}
	lowerKey, _, packed := normalizeScrubGUID(strings.ToLower(upper)+"_att3", true)
	if !packed || lowerKey != key {
		t.Fatalf("lowercase part-suffixed form packed to %x, want %x", lowerKey, key)
	}
	if _, _, packed := normalizeScrubGUID(upper+"_att3", false); packed {
		t.Fatal("part suffix must not be stripped from a cloud guid")
	}
	for _, id := range []string{"", "not-a-uuid", "0F0E0D0C-0B0A-0908-0706-05040302010G", "0F0E0D0C0B0A09080706050403020100"} {
		if _, raw, packed := normalizeScrubGUID(id, true); packed || raw != strings.ToLower(id) {
			t.Errorf("%q: packed=%v raw=%q", id, packed, raw)
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
	bridged := newBridgedIDSet("bridge")
	bridged.add(guid)
	scrubbed, err := store.scrubBatchIfEligible(ctx, cutoff, bridged, candidates)
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

// scrubIncrementalFixture returns a store with the scrubber schema and a
// bridgev2 message table, plus helpers that insert an aged plaintext row and
// read a row's body_scrubbed flag.
func scrubIncrementalFixture(t *testing.T) (context.Context, *dbutil.Database, *cloudBackfillStore, func(guid, portal string), func(guid string) bool) {
	t.Helper()
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	createScrubberBridgeMessageTable(t, db, ctx)
	old := time.Now().Add(-time.Hour).UnixMilli()
	insertAged := func(guid, portal string) {
		t.Helper()
		if err := store.upsertMessageBatch(ctx, []cloudMessageRow{{
			GUID: guid, PortalID: portal, TimestampMS: old,
			Text: "secret " + guid, Service: "iMessage", HasBody: true,
		}}); err != nil {
			t.Fatalf("upsert %s: %v", guid, err)
		}
		if _, err := db.Exec(ctx,
			`UPDATE cloud_message SET updated_ts=$1 WHERE login_id=$2 AND guid=$3`,
			old, testSQLLoginID, guid,
		); err != nil {
			t.Fatalf("age %s: %v", guid, err)
		}
	}
	scrubbed := func(guid string) bool {
		t.Helper()
		var flag bool
		if err := db.QueryRow(ctx,
			`SELECT body_scrubbed FROM cloud_message WHERE login_id=$1 AND guid=$2`,
			testSQLLoginID, guid,
		).Scan(&flag); err != nil {
			t.Fatalf("read body_scrubbed of %s: %v", guid, err)
		}
		return flag
	}
	return ctx, db, store, insertAged, scrubbed
}

func TestScrubBridgedBodiesExtendsDeliveredSetIncrementally(t *testing.T) {
	ctx, db, store, insertAged, scrubbed := scrubIncrementalFixture(t)
	const bridgeID = "bridge"

	insertAged("11111111-1111-4111-8111-111111111111", "gid:p")
	insertAged("22222222-2222-4222-8222-222222222222", "gid:p")
	insertScrubberBridgeMessage(t, db, ctx, "11111111-1111-4111-8111-111111111111", bridgeID, string(testSQLLoginID))

	if n, err := store.scrubBridgedBodies(ctx, bridgeID, time.Minute, nil); err != nil || n != 1 {
		t.Fatalf("first pass = %d, %v; want 1 row", n, err)
	}
	first := store.bridged
	if first == nil || first.maxRowID != 1 || first.size() != 1 {
		t.Fatalf("first pass set = %+v, want one id anchored at rowid 1", first)
	}

	// Delivered since the last pass: a part-suffixed lowercase id for the
	// second row, then a row for another login that must move the rowid
	// anchor without counting as delivered here.
	insertScrubberBridgeMessage(t, db, ctx, "22222222-2222-4222-8222-222222222222_att0", bridgeID, "")
	insertScrubberBridgeMessage(t, db, ctx, "33333333-3333-4333-8333-333333333333", bridgeID, "other-login")
	insertAged("33333333-3333-4333-8333-333333333333", "gid:p")

	if n, err := store.scrubBridgedBodies(ctx, bridgeID, time.Minute, nil); err != nil || n != 1 {
		t.Fatalf("second pass = %d, %v; want 1 row", n, err)
	}
	if store.bridged != first {
		t.Fatal("second pass rebuilt the delivered set instead of extending it")
	}
	if first.maxRowID != 3 || first.maxRowIDMsg != "33333333-3333-4333-8333-333333333333" {
		t.Fatalf("anchor = (%d, %q), want (3, other login's row)", first.maxRowID, first.maxRowIDMsg)
	}
	if !scrubbed("22222222-2222-4222-8222-222222222222") || scrubbed("33333333-3333-4333-8333-333333333333") {
		t.Fatal("second pass scrubbed the wrong rows")
	}

	// A message row can land before CloudKit hands over the cloud row. The set
	// must still know about it when the cloud row finally ages into range.
	insertScrubberBridgeMessage(t, db, ctx, "44444444-4444-4444-8444-444444444444", bridgeID, string(testSQLLoginID))
	if n, err := store.scrubBridgedBodies(ctx, bridgeID, time.Minute, nil); err != nil || n != 0 {
		t.Fatalf("pass with no cloud row = %d, %v; want 0", n, err)
	}
	insertAged("44444444-4444-4444-8444-444444444444", "gid:p")
	if n, err := store.scrubBridgedBodies(ctx, bridgeID, time.Minute, nil); err != nil || n != 1 {
		t.Fatalf("late cloud row pass = %d, %v; want 1", n, err)
	}
	if store.bridged != first || !scrubbed("44444444-4444-4444-8444-444444444444") {
		t.Fatal("late cloud row was not scrubbed from the retained set")
	}
}

func TestScrubBridgedBodiesRebuildsDeliveredSetAfterRowIDReuse(t *testing.T) {
	ctx, db, store, insertAged, scrubbed := scrubIncrementalFixture(t)
	const bridgeID = "bridge"
	for _, guid := range []string{"guid-1", "guid-2", "guid-3"} {
		insertAged(guid, "gid:p")
		insertScrubberBridgeMessage(t, db, ctx, guid, bridgeID, string(testSQLLoginID))
	}
	if n, err := store.scrubBridgedBodies(ctx, bridgeID, time.Minute, nil); err != nil || n != 3 {
		t.Fatalf("first pass = %d, %v; want 3", n, err)
	}
	first := store.bridged

	// Removing the newest rows makes SQLite hand their rowids to the next
	// inserts, so a plain "rowid above the watermark" read would miss them.
	if _, err := db.Exec(ctx, `DELETE FROM message WHERE rowid >= 2`); err != nil {
		t.Fatalf("delete newest message rows: %v", err)
	}
	insertScrubberBridgeMessage(t, db, ctx, "guid-4", bridgeID, string(testSQLLoginID))
	insertScrubberBridgeMessage(t, db, ctx, "guid-5", bridgeID, string(testSQLLoginID))
	var maxRowID int64
	if err := db.QueryRow(ctx, `SELECT MAX(rowid) FROM message`).Scan(&maxRowID); err != nil {
		t.Fatal(err)
	}
	if maxRowID != 3 {
		t.Fatalf("max rowid after reinsert = %d, want 3 (rowids were not reused)", maxRowID)
	}
	insertAged("guid-4", "gid:p")
	insertAged("guid-5", "gid:p")

	if n, err := store.scrubBridgedBodies(ctx, bridgeID, time.Minute, nil); err != nil || n != 2 {
		t.Fatalf("pass after rowid reuse = %d, %v; want 2", n, err)
	}
	if store.bridged == first {
		t.Fatal("anchor mismatch did not rebuild the delivered set")
	}
	if !scrubbed("guid-4") || !scrubbed("guid-5") {
		t.Fatal("rows delivered under reused rowids were not scrubbed")
	}
}

func TestClearBodyScrubInvalidatesDeliveredSet(t *testing.T) {
	ctx, db, store, insertAged, _ := scrubIncrementalFixture(t)
	const bridgeID = "bridge"
	insertAged("guid-1", "gid:p")
	insertScrubberBridgeMessage(t, db, ctx, "guid-1", bridgeID, string(testSQLLoginID))
	// An undelivered aged row keeps every later pass consulting the set.
	insertAged("guid-undelivered", "gid:q")
	if n, err := store.scrubBridgedBodies(ctx, bridgeID, time.Minute, nil); err != nil || n != 1 {
		t.Fatalf("first pass = %d, %v; want 1", n, err)
	}
	first := store.bridged
	if n, err := store.scrubBridgedBodies(ctx, bridgeID, time.Minute, nil); err != nil || n != 0 || store.bridged != first {
		t.Fatalf("steady-state pass = %d, %v, rebuilt=%v; want 0 and the same set", n, err, store.bridged != first)
	}

	if _, err := store.clearBodyScrubByPortalID(ctx, "gid:p"); err != nil {
		t.Fatalf("clearBodyScrubByPortalID: %v", err)
	}
	if _, err := store.scrubBridgedBodies(ctx, bridgeID, time.Minute, nil); err != nil {
		t.Fatalf("pass after clear: %v", err)
	}
	if store.bridged == first {
		t.Fatal("clearBodyScrubByPortalID did not force the delivered set to be rebuilt")
	}
}

func TestScrubReactionTextClearsDescriptorsByGUID(t *testing.T) {
	ctx, db, store, _, scrubbed := scrubIncrementalFixture(t)
	old := time.Now().Add(-time.Hour).UnixMilli()
	tapback := uint32(2001)
	if err := store.upsertMessageBatch(ctx, []cloudMessageRow{
		{GUID: "react-text", PortalID: "gid:p", TimestampMS: old, Service: "iMessage",
			TapbackType: &tapback, Text: "Loved 'the secret'", Sender: "tel:+1555", TapbackEmoji: "x"},
		{GUID: "react-subject", PortalID: "gid:p", TimestampMS: old, Service: "iMessage",
			TapbackType: &tapback, Subject: "Loved 'the secret'"},
		{GUID: "react-empty", PortalID: "gid:p", TimestampMS: old, Service: "iMessage",
			TapbackType: &tapback},
		{GUID: "plain", PortalID: "gid:p", TimestampMS: old, Service: "iMessage",
			Text: "not a reaction", HasBody: true},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := db.Exec(ctx, `UPDATE cloud_message SET updated_ts=$1 WHERE login_id=$2`, old, testSQLLoginID); err != nil {
		t.Fatalf("age rows: %v", err)
	}

	n, err := store.scrubReactionText(ctx, time.Minute)
	if err != nil || n != 2 {
		t.Fatalf("scrubReactionText = %d, %v; want 2", n, err)
	}
	if !scrubbed("react-text") || !scrubbed("react-subject") || scrubbed("react-empty") || scrubbed("plain") {
		t.Fatal("reaction scrub touched the wrong rows")
	}
	var text, subject sql.NullString
	var sender, emoji string
	if err := db.QueryRow(ctx,
		`SELECT text, subject, COALESCE(sender, ''), COALESCE(tapback_emoji, '') FROM cloud_message WHERE login_id=$1 AND guid='react-text'`,
		testSQLLoginID,
	).Scan(&text, &subject, &sender, &emoji); err != nil {
		t.Fatal(err)
	}
	if text.Valid || subject.Valid || sender != "tel:+1555" || emoji != "x" {
		t.Fatalf("react-text after scrub: text=%v subject=%v sender=%q emoji=%q", text, subject, sender, emoji)
	}
	if n, err := store.scrubReactionText(ctx, time.Minute); err != nil || n != 0 {
		t.Fatalf("second scrubReactionText = %d, %v; want 0", n, err)
	}
}

func TestNormalizeGroupMessagePortalIDsUsesCloudChatMapping(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := store.upsertChatBatch(ctx, []cloudChatUpsertRow{
		{CloudChatID: "CHAT-1", GroupID: "GROUP-1", PortalID: "gid:group-1", Service: "iMessage", ParticipantsJSON: "[]", UpdatedTS: now},
		{CloudChatID: "chat-2", GroupID: "", PortalID: "gid:chat-2", Service: "iMessage", ParticipantsJSON: "[]", UpdatedTS: now},
		{CloudChatID: "chat-3", GroupID: "group-3", PortalID: "", Service: "iMessage", ParticipantsJSON: "[]", UpdatedTS: now},
		// A chain: rows in gid:link-a belong in gid:link-b, and rows already in
		// gid:link-b belong in gid:link-c. Each row must move exactly once.
		{CloudChatID: "link-a", GroupID: "", PortalID: "gid:link-b", Service: "iMessage", ParticipantsJSON: "[]", UpdatedTS: now},
		{CloudChatID: "link-b", GroupID: "", PortalID: "gid:link-c", Service: "iMessage", ParticipantsJSON: "[]", UpdatedTS: now},
	}); err != nil {
		t.Fatalf("upsertChatBatch: %v", err)
	}
	rows := []cloudMessageRow{
		{GUID: "by-chat-id", PortalID: "gid:chat-1"},
		{GUID: "by-group-id-case", PortalID: "gid:GROUP-1"},
		{GUID: "already-canonical", PortalID: "gid:group-1"},
		{GUID: "self-mapping", PortalID: "gid:chat-2"},
		{GUID: "unmapped-chat", PortalID: "gid:chat-3"},
		{GUID: "dm", PortalID: "tel:+15550001111"},
		{GUID: "chain-first", PortalID: "gid:link-a"},
		{GUID: "chain-second", PortalID: "gid:link-b"},
	}
	for i := range rows {
		rows[i].TimestampMS = now
		rows[i].Service = "iMessage"
	}
	if err := store.upsertMessageBatch(ctx, rows); err != nil {
		t.Fatalf("upsertMessageBatch: %v", err)
	}

	n, err := store.normalizeGroupMessagePortalIDs(ctx)
	if err != nil || n != 4 {
		t.Fatalf("normalizeGroupMessagePortalIDs = %d, %v; want 4", n, err)
	}
	for guid, want := range map[string]string{
		"by-chat-id":        "gid:group-1",
		"by-group-id-case":  "gid:group-1",
		"already-canonical": "gid:group-1",
		"self-mapping":      "gid:chat-2",
		"unmapped-chat":     "gid:chat-3",
		"dm":                "tel:+15550001111",
		"chain-first":       "gid:link-b",
		"chain-second":      "gid:link-c",
	} {
		var got string
		if err := db.QueryRow(ctx, `SELECT portal_id FROM cloud_message WHERE login_id=$1 AND guid=$2`, testSQLLoginID, guid).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", guid, err)
		}
		if got != want {
			t.Errorf("%s portal_id = %q, want %q", guid, got, want)
		}
	}
	// The chain's second hop is itself a stale portal, so a second run moves
	// the rows that landed there in the first run: that is what the single
	// statement did too, one hop per bootstrap.
	if n, err := store.normalizeGroupMessagePortalIDs(ctx); err != nil || n != 1 {
		t.Fatalf("second normalizeGroupMessagePortalIDs = %d, %v; want 1 (chain-first's second hop)", n, err)
	}
	if n, err := store.normalizeGroupMessagePortalIDs(ctx); err != nil || n != 0 {
		t.Fatalf("third normalizeGroupMessagePortalIDs = %d, %v; want 0", n, err)
	}
}

func TestNormalizeGroupMessagePortalIDsHandlesMappingCycleAtomically(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := store.upsertChatBatch(ctx, []cloudChatUpsertRow{
		{CloudChatID: "cycle-a", PortalID: "gid:cycle-b", Service: "iMessage", ParticipantsJSON: "[]", UpdatedTS: now},
		{CloudChatID: "cycle-b", PortalID: "gid:cycle-a", Service: "iMessage", ParticipantsJSON: "[]", UpdatedTS: now},
	}); err != nil {
		t.Fatalf("upsert cycle chats: %v", err)
	}
	if err := store.upsertMessageBatch(ctx, []cloudMessageRow{
		{GUID: "from-cycle-a", PortalID: "gid:cycle-a", TimestampMS: now, Service: "iMessage"},
		{GUID: "from-cycle-b", PortalID: "gid:cycle-b", TimestampMS: now, Service: "iMessage"},
	}); err != nil {
		t.Fatalf("upsert cycle messages: %v", err)
	}

	if n, err := store.normalizeGroupMessagePortalIDs(ctx); err != nil || n != 2 {
		t.Fatalf("normalizeGroupMessagePortalIDs = %d, %v; want 2", n, err)
	}
	for guid, want := range map[string]string{
		"from-cycle-a": "gid:cycle-b",
		"from-cycle-b": "gid:cycle-a",
	} {
		var got string
		if err := db.QueryRow(ctx, `SELECT portal_id FROM cloud_message WHERE login_id=$1 AND guid=$2`,
			testSQLLoginID, guid).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", guid, err)
		}
		if got != want {
			t.Errorf("%s portal_id = %q, want %q", guid, got, want)
		}
	}
}

func TestScrubBridgedBodiesDropsDeliveredSetWhenRebuildFails(t *testing.T) {
	ctx, db, store, insertAged, scrubbed := scrubIncrementalFixture(t)
	const bridgeID = "bridge"
	insertAged("guid-1", "gid:p")
	insertScrubberBridgeMessage(t, db, ctx, "guid-1", bridgeID, string(testSQLLoginID))
	// An undelivered aged row keeps every later pass consulting the set.
	insertAged("guid-undelivered", "gid:q")
	if n, err := store.scrubBridgedBodies(ctx, bridgeID, time.Minute, nil); err != nil || n != 1 {
		t.Fatalf("first pass = %d, %v; want 1", n, err)
	}
	first := store.bridged

	// Plaintext is handed back, then the rebuild the invalidation demands
	// fails. The stale set must not survive as if it were current.
	store.invalidateBridgedGUIDSet()
	if _, err := db.Exec(ctx, `ALTER TABLE message RENAME TO message_hidden`); err != nil {
		t.Fatalf("hide message table: %v", err)
	}
	if _, err := store.scrubBridgedBodies(ctx, bridgeID, time.Minute, nil); err == nil {
		t.Fatal("pass without a message table succeeded")
	}
	if store.bridged != nil {
		t.Fatal("failed rebuild left the previous delivered set in place")
	}
	if _, err := db.Exec(ctx, `ALTER TABLE message_hidden RENAME TO message`); err != nil {
		t.Fatalf("restore message table: %v", err)
	}

	insertAged("guid-2", "gid:p")
	insertScrubberBridgeMessage(t, db, ctx, "guid-2", bridgeID, string(testSQLLoginID))
	if n, err := store.scrubBridgedBodies(ctx, bridgeID, time.Minute, nil); err != nil || n != 1 {
		t.Fatalf("pass after recovery = %d, %v; want 1", n, err)
	}
	if store.bridged == nil || store.bridged == first || !scrubbed("guid-2") {
		t.Fatal("pass after recovery did not rebuild the delivered set")
	}
}

func TestScrubBridgedBodiesDropsDeliveredSetWhenExtensionFails(t *testing.T) {
	ctx, db, store, insertAged, scrubbed := scrubIncrementalFixture(t)
	const bridgeID = "bridge"
	insertAged("guid-1", "gid:p")
	insertScrubberBridgeMessage(t, db, ctx, "guid-1", bridgeID, string(testSQLLoginID))
	insertAged("guid-undelivered", "gid:q")
	if n, err := store.scrubBridgedBodies(ctx, bridgeID, time.Minute, nil); err != nil || n != 1 {
		t.Fatalf("first pass = %d, %v; want 1", n, err)
	}
	first := store.bridged

	// No invalidation this time: the incremental read itself fails.
	if _, err := db.Exec(ctx, `ALTER TABLE message RENAME TO message_hidden`); err != nil {
		t.Fatalf("hide message table: %v", err)
	}
	if _, err := store.scrubBridgedBodies(ctx, bridgeID, time.Minute, nil); err == nil {
		t.Fatal("pass without a message table succeeded")
	}
	if store.bridged != nil {
		t.Fatal("failed extension left the previous delivered set in place")
	}
	if _, err := db.Exec(ctx, `ALTER TABLE message_hidden RENAME TO message`); err != nil {
		t.Fatalf("restore message table: %v", err)
	}
	insertAged("guid-2", "gid:p")
	insertScrubberBridgeMessage(t, db, ctx, "guid-2", bridgeID, string(testSQLLoginID))
	if n, err := store.scrubBridgedBodies(ctx, bridgeID, time.Minute, nil); err != nil || n != 1 {
		t.Fatalf("pass after recovery = %d, %v; want 1", n, err)
	}
	if store.bridged == nil || store.bridged == first || !scrubbed("guid-2") {
		t.Fatal("pass after recovery did not rebuild the delivered set")
	}
}

func TestExtendBridgedGUIDSetRechecksAnchorAfterRead(t *testing.T) {
	ctx, db, store, insertAged, _ := scrubIncrementalFixture(t)
	const bridgeID = "bridge"
	insertAged("guid-1", "gid:p")
	insertScrubberBridgeMessage(t, db, ctx, "guid-1", bridgeID, string(testSQLLoginID))
	if n, err := store.scrubBridgedBodies(ctx, bridgeID, time.Minute, nil); err != nil || n != 1 {
		t.Fatalf("first pass = %d, %v; want 1", n, err)
	}
	set := store.bridged

	// A set whose anchor still holds extends and stays complete.
	insertScrubberBridgeMessage(t, db, ctx, "guid-2", bridgeID, string(testSQLLoginID))
	if complete, err := store.extendBridgedGUIDSet(ctx, set); err != nil || !complete {
		t.Fatalf("extend with a valid anchor = %v, %v; want complete", complete, err)
	}
	if !set.contains("guid-2") || set.maxRowID != 2 {
		t.Fatalf("extend did not fold the new row: contains=%v anchor=%d", set.contains("guid-2"), set.maxRowID)
	}

	// Once the anchor row is gone the extension is reported incomplete even
	// though the range read itself succeeds.
	if _, err := db.Exec(ctx, `DELETE FROM message WHERE rowid=2`); err != nil {
		t.Fatal(err)
	}
	if complete, err := store.extendBridgedGUIDSet(ctx, set); err != nil || complete {
		t.Fatalf("extend with a deleted anchor = %v, %v; want incomplete", complete, err)
	}
}
