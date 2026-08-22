package connector

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"go.mau.fi/util/dbutil"
)

// These tests cover the ordering rule between the privacy scrubber and CloudKit
// backfill. Both read the same cloud_message rows, and the scrubber wins ties:
// once a row is body_scrubbed, cloudRowToBackfillMessages drops it. If that
// happens to every deliverable row a portal has, forward backfill converts to
// zero messages and marks the portal done with an empty room — real history
// stranded on disk with nothing in the log to say so.
//
// Two mechanisms are under test: the hold that keeps the scrubbers off a portal
// that has not delivered yet (pendingBackfillGateSQL), and the pieces
// FetchMessages uses to notice and recover when it happens anyway
// (anyScrubbedDeliverable, rescrubEmptyRowsSince).

// scrubRaceFixture builds a store with bridgev2's message table alongside it,
// since scrubBridgedBodies only scrubs rows that have a delivered message row.
func scrubRaceFixture(t *testing.T) (context.Context, *dbutil.Database, *cloudBackfillStore) {
	t.Helper()
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	if _, err := db.Exec(ctx, `CREATE TABLE IF NOT EXISTS message (
		id TEXT NOT NULL,
		bridge_id TEXT NOT NULL,
		room_receiver TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatalf("create message table: %v", err)
	}
	return ctx, db, store
}

// TestScrubHoldsOffPendingBackfillPortals is the ordering rule: a delivered body
// may be scrubbed, but not while its portal is still waiting for the forward
// backfill that puts the rest of its history in the room.
//
// The negative cases carry as much weight as the positive one. The hold must be
// narrow — it suspends a privacy control — so a portal that already delivered, a
// portal Apple filtered into the junk bucket (which never becomes a portal at
// all, so it would otherwise be exempt forever), and a portal old enough to be
// past the hold window must all still scrub.
func TestScrubHoldsOffPendingBackfillPortals(t *testing.T) {
	ctx, db, store := scrubRaceFixture(t)
	const bridgeID = "test-bridge"
	now := time.Now().UnixMilli()

	type portal struct {
		id         string
		chatID     string
		isFiltered int64
		createdTS  int64
		done       bool
	}
	portals := []portal{
		// Still waiting for its first forward backfill: held.
		{id: "tel:+15550001111", chatID: "C-PENDING", createdTS: now},
		// Forward backfill already delivered: nothing left to protect.
		{id: "tel:+15550002222", chatID: "C-DONE", createdTS: now, done: true},
		// Filtered chat. bridge_filtered_chats is off by default, so these never
		// become portals and never reach fwd_backfill_done — holding them would
		// mean holding them forever.
		{id: "tel:+15550003333", chatID: "C-FILTERED", createdTS: now, isFiltered: 1},
		// Known since long before the hold window. Whatever is wrong with this
		// portal, privacy stops waiting on it.
		{id: "tel:+15550004444", chatID: "C-STALE",
			createdTS: time.Now().Add(-2 * pendingBackfillScrubHold).UnixMilli()},
	}

	for _, p := range portals {
		if err := store.upsertChatBatch(ctx, []cloudChatUpsertRow{{
			CloudChatID: p.chatID, PortalID: p.id, Service: "iMessage",
			ParticipantsJSON: "[]", UpdatedTS: now, IsFiltered: p.isFiltered,
		}}); err != nil {
			t.Fatalf("upsertChatBatch %s: %v", p.id, err)
		}
		if _, err := db.Exec(ctx,
			`UPDATE cloud_chat SET created_ts=$3 WHERE login_id=$1 AND cloud_chat_id=$2`,
			testSQLLoginID, p.chatID, p.createdTS,
		); err != nil {
			t.Fatalf("set created_ts %s: %v", p.id, err)
		}
		if p.done {
			store.markForwardBackfillDone(ctx, p.id)
		}

		guid := "GUID-" + p.chatID
		if err := store.upsertMessageBatch(ctx, []cloudMessageRow{{
			GUID: guid, CloudChatID: p.chatID, PortalID: p.id,
			TimestampMS: now, Text: "plaintext", Service: "iMessage", HasBody: true,
		}}); err != nil {
			t.Fatalf("upsertMessageBatch %s: %v", p.id, err)
		}
		// Delivered to Matrix — the precondition scrubBridgedBodies requires.
		if _, err := db.Exec(ctx,
			`INSERT INTO message (id, bridge_id, room_receiver) VALUES ($1, $2, $3)`,
			guid, bridgeID, string(testSQLLoginID),
		); err != nil {
			t.Fatalf("insert bridgev2 message row %s: %v", p.id, err)
		}
	}

	// Age every row past the grace window so only the hold is under test.
	if _, err := db.Exec(ctx,
		`UPDATE cloud_message SET updated_ts=$2 WHERE login_id=$1`,
		testSQLLoginID, now-int64(time.Hour/time.Millisecond),
	); err != nil {
		t.Fatalf("age messages: %v", err)
	}

	if _, err := store.scrubBridgedBodies(ctx, bridgeID, time.Minute, nil); err != nil {
		t.Fatalf("scrubBridgedBodies: %v", err)
	}

	textOf := func(guid string) sql.NullString {
		t.Helper()
		var text sql.NullString
		if err := db.QueryRow(ctx,
			`SELECT text FROM cloud_message WHERE login_id=$1 AND guid=$2`,
			testSQLLoginID, guid,
		).Scan(&text); err != nil {
			t.Fatalf("read back %s: %v", guid, err)
		}
		return text
	}

	if got := textOf("GUID-C-PENDING"); !got.Valid {
		t.Error("body of a portal still awaiting forward backfill was scrubbed — " +
			"conversion drops scrubbed rows, so this is how a portal ends up marked done with an empty room")
	}
	for _, guid := range []string{"GUID-C-DONE", "GUID-C-FILTERED", "GUID-C-STALE"} {
		if got := textOf(guid); got.Valid {
			t.Errorf("%s still has plaintext %q after scrub — the hold must not exempt this portal", guid, got.String)
		}
	}
}

// TestScrubBridgedBodiesStillScrubsDeletedRowsWhilePending pins the one branch
// the hold deliberately does not cover. A row the user deleted or unsent must
// lose its plaintext immediately: there is no backfill left to protect (every
// backfill reader filters deleted=FALSE), and delaying it would keep content the
// user explicitly removed readable on disk for up to a day.
func TestScrubBridgedBodiesStillScrubsDeletedRowsWhilePending(t *testing.T) {
	ctx, db, store := scrubRaceFixture(t)
	const bridgeID = "test-bridge"
	now := time.Now().UnixMilli()

	if err := store.upsertChatBatch(ctx, []cloudChatUpsertRow{{
		CloudChatID: "C-PENDING", PortalID: "tel:+15550001111", Service: "iMessage",
		ParticipantsJSON: "[]", UpdatedTS: now,
	}}); err != nil {
		t.Fatalf("upsertChatBatch: %v", err)
	}
	if err := store.upsertMessageBatch(ctx, []cloudMessageRow{{
		GUID: "GUID-DELETED", CloudChatID: "C-PENDING", PortalID: "tel:+15550001111",
		TimestampMS: now, Text: "unsent", Service: "iMessage", HasBody: true, Deleted: true,
	}}); err != nil {
		t.Fatalf("upsertMessageBatch: %v", err)
	}
	if _, err := db.Exec(ctx,
		`UPDATE cloud_message SET updated_ts=$2 WHERE login_id=$1`,
		testSQLLoginID, now-int64(time.Hour/time.Millisecond),
	); err != nil {
		t.Fatalf("age messages: %v", err)
	}

	scrubbed, err := store.scrubBridgedBodies(ctx, bridgeID, time.Minute, nil)
	if err != nil {
		t.Fatalf("scrubBridgedBodies: %v", err)
	}
	if scrubbed != 1 {
		t.Fatalf("scrubBridgedBodies scrubbed %d deleted rows, want 1 — the hold must not delay deleted content", scrubbed)
	}
}

// TestScrubUnbridgedTailHoldsOffPendingBackfillPortals covers the same hold on
// the tail scrubber, which is the more dangerous of the two: it clears rows that
// were never delivered, choosing them by position rather than by delivery. Its
// threshold counts every non-deleted row while listLatestMessages returns only
// contentful ones, so before delivery the "unreachable" tail can still contain
// rows forward backfill was going to send.
func TestScrubUnbridgedTailHoldsOffPendingBackfillPortals(t *testing.T) {
	ctx, db, store := scrubRaceFixture(t)
	now := time.Now().UnixMilli()

	for _, p := range []struct {
		id, chatID string
		done       bool
	}{
		{id: "tel:+15550001111", chatID: "C-PENDING"},
		{id: "tel:+15550002222", chatID: "C-DONE", done: true},
	} {
		if err := store.upsertChatBatch(ctx, []cloudChatUpsertRow{{
			CloudChatID: p.chatID, PortalID: p.id, Service: "iMessage",
			ParticipantsJSON: "[]", UpdatedTS: now,
		}}); err != nil {
			t.Fatalf("upsertChatBatch %s: %v", p.id, err)
		}
		if p.done {
			store.markForwardBackfillDone(ctx, p.id)
		}
		// Four rows each, keeping two: rows 1 and 2 are the tail.
		for i := 1; i <= 4; i++ {
			guid := p.chatID + "-" + string(rune('0'+i))
			if err := store.upsertMessageBatch(ctx, []cloudMessageRow{{
				GUID: guid, RecordName: "rec-" + guid, CloudChatID: p.chatID, PortalID: p.id,
				TimestampMS: now + int64(i), Text: "plaintext", Service: "iMessage", HasBody: true,
			}}); err != nil {
				t.Fatalf("upsertMessageBatch %s: %v", guid, err)
			}
		}
	}
	if _, err := db.Exec(ctx,
		`UPDATE cloud_message SET updated_ts=$2 WHERE login_id=$1`,
		testSQLLoginID, now-int64(time.Hour/time.Millisecond),
	); err != nil {
		t.Fatalf("age messages: %v", err)
	}

	scrubbed, err := store.scrubUnbridgedTail(ctx, 2, time.Minute, nil)
	if err != nil {
		t.Fatalf("scrubUnbridgedTail: %v", err)
	}
	if scrubbed != 2 {
		t.Errorf("scrubUnbridgedTail scrubbed %d rows, want 2 (the delivered portal's tail only)", scrubbed)
	}
	var pendingPlaintext int
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM cloud_message WHERE login_id=$1 AND portal_id=$2 AND COALESCE(text,'') <> ''`,
		testSQLLoginID, "tel:+15550001111",
	).Scan(&pendingPlaintext); err != nil {
		t.Fatalf("count pending portal rows: %v", err)
	}
	if pendingPlaintext != 4 {
		t.Errorf("portal awaiting forward backfill kept %d/4 rows with text — its tail was scrubbed before delivery", pendingPlaintext)
	}
}

// TestAnyScrubbedDeliverable covers the in-memory check FetchMessages uses to
// decide whether an empty conversion is worth a CloudKit round trip. It runs on
// rows already in hand precisely so the empty path adds no query to a pool
// clamped at one connection, so it has to be right about which rows conversion
// actually drops for being scrubbed.
func TestAnyScrubbedDeliverable(t *testing.T) {
	reaction := uint32(2000)
	regular := uint32(0)

	for _, tc := range []struct {
		name string
		rows []cloudMessageRow
		want bool
	}{
		{name: "no rows"},
		{
			name: "live rows only",
			rows: []cloudMessageRow{{GUID: "A", Text: "hi"}, {GUID: "B", Text: "there"}},
		},
		{
			// Scrubbed reactions still render (cloudTapbackToBackfill reads
			// tapback_type, which the scrubber preserves), so they are not a
			// reason to go back to CloudKit.
			name: "scrubbed reaction only",
			rows: []cloudMessageRow{{GUID: "A", BodyScrubbed: true, TapbackType: &reaction}},
		},
		{
			name: "scrubbed message among live rows",
			rows: []cloudMessageRow{{GUID: "A", Text: "hi"}, {GUID: "B", BodyScrubbed: true}},
			want: true,
		},
		{
			// tapback_type 0 is a regular message, not a reaction — the
			// NULL-vs-0 distinction isCloudReactionRow exists for.
			name: "scrubbed message with tapback_type 0",
			rows: []cloudMessageRow{{GUID: "A", BodyScrubbed: true, TapbackType: &regular}},
			want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := anyScrubbedDeliverable(tc.rows); got != tc.want {
				t.Errorf("anyScrubbedDeliverable = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRescrubEmptyRowsSince covers the undo half of a failed rehydrate. Clearing
// body_scrubbed drops the sticky-NULL protection in upsertMessageBatch, so if
// CloudKit cannot repopulate the rows the flag has to go back on — otherwise a
// later sync could quietly refill plaintext that will never be delivered and
// therefore never be scrubbed again.
//
// The two rows it must NOT touch are the point: a row CloudKit did restore
// (content is back, it is deliverable again) and a row that was empty for
// unrelated reasons and predates the attempt (inventing a scrub flag there would
// block CloudKit from ever filling it in).
func TestRescrubEmptyRowsSince(t *testing.T) {
	ctx, db, store := scrubRaceFixture(t)
	now := time.Now().UnixMilli()

	if err := store.upsertMessageBatch(ctx, []cloudMessageRow{
		{GUID: "G-STILL-EMPTY", PortalID: "p1", CloudChatID: "C1", TimestampMS: now, Service: "iMessage", HasBody: true},
		{GUID: "G-RESTORED", PortalID: "p1", CloudChatID: "C1", TimestampMS: now, Text: "came back", Service: "iMessage", HasBody: true},
		{GUID: "G-OLD-EMPTY", PortalID: "p1", CloudChatID: "C1", TimestampMS: now, Service: "iMessage", HasBody: true},
		{GUID: "G-OTHER-PORTAL", PortalID: "p2", CloudChatID: "C2", TimestampMS: now, Service: "iMessage", HasBody: true},
	}); err != nil {
		t.Fatalf("upsertMessageBatch: %v", err)
	}

	clearedFrom := now - 1000
	// The old empty row predates the rehydrate attempt.
	if _, err := db.Exec(ctx,
		`UPDATE cloud_message SET updated_ts=$2 WHERE login_id=$1 AND guid='G-OLD-EMPTY'`,
		testSQLLoginID, clearedFrom-1,
	); err != nil {
		t.Fatalf("age G-OLD-EMPTY: %v", err)
	}

	n, err := store.rescrubEmptyRowsSince(ctx, "p1", clearedFrom)
	if err != nil {
		t.Fatalf("rescrubEmptyRowsSince: %v", err)
	}
	if n != 1 {
		t.Errorf("rescrubEmptyRowsSince re-armed %d rows, want 1", n)
	}

	scrubbedOf := func(guid string) bool {
		t.Helper()
		var scrubbed bool
		if err := db.QueryRow(ctx,
			`SELECT body_scrubbed FROM cloud_message WHERE login_id=$1 AND guid=$2`,
			testSQLLoginID, guid,
		).Scan(&scrubbed); err != nil {
			t.Fatalf("read back %s: %v", guid, err)
		}
		return scrubbed
	}
	if !scrubbedOf("G-STILL-EMPTY") {
		t.Error("G-STILL-EMPTY kept body_scrubbed=FALSE with no content — a later CloudKit sync could refill plaintext nothing will scrub again")
	}
	if scrubbedOf("G-RESTORED") {
		t.Error("G-RESTORED was re-scrubbed even though CloudKit gave its text back")
	}
	if scrubbedOf("G-OLD-EMPTY") {
		t.Error("G-OLD-EMPTY was scrubbed, but it predates the rehydrate attempt — this must only undo what the attempt cleared")
	}
	if scrubbedOf("G-OTHER-PORTAL") {
		t.Error("a row in another portal was re-scrubbed")
	}
}
