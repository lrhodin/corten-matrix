// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package connector

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"go.mau.fi/util/dbutil"
)

// These tests run GetSyncStatus against a real SQLite database built by the
// store's own ensureSchema, so the classification SQL is exercised against the
// actual column set rather than a hand-written approximation of it. That
// matters most for the bucket arithmetic: the whole point of the report is
// that the not-bridgeable rows are excluded from the denominator, and an
// approximate fixture schema is exactly where that would silently stop being
// true.

const syncStatusTestBridgeID = "corten"

// bridgev2 tables the report reads. Only the columns it touches are declared —
// enough for the SQL to run, not a copy of the framework's schema.
const syncStatusFrameworkSchema = `
	CREATE TABLE user_login (
		bridge_id TEXT NOT NULL,
		id        TEXT NOT NULL,
		PRIMARY KEY (bridge_id, id)
	);
	CREATE TABLE message (
		bridge_id     TEXT   NOT NULL,
		id            TEXT   NOT NULL,
		room_receiver TEXT   NOT NULL,
		timestamp     BIGINT NOT NULL
	);
	CREATE TABLE backfill_task (
		bridge_id     TEXT    NOT NULL,
		portal_id     TEXT    NOT NULL,
		user_login_id TEXT    NOT NULL,
		is_done       BOOLEAN NOT NULL,
		dispatched_at BIGINT
	);
`

// syncStatusFixture builds a database for one test case. Fixture rows go in
// AFTER ensureSchema, which hard-deletes system rows and scrubs soft-deleted
// ones on its way through — inserting first would quietly remove exactly the
// rows several of these cases are about.
type syncStatusFixture struct {
	name string
	// framework creates the bridgev2 tables. Off for the bare-database case.
	skipFramework bool
	// cloud skips the store's own ensureSchema, for the database that has
	// never run the CloudKit store.
	skipCloudSchema bool
	setup           func(t *testing.T, h *syncStatusHarness)
	opts            SyncStatusOptions
	check           func(t *testing.T, r *SyncStatusReport)
}

type syncStatusHarness struct {
	t   *testing.T
	ctx context.Context
	db  *dbutil.Database
}

func (h *syncStatusHarness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.db.Exec(h.ctx, query, args...); err != nil {
		h.t.Fatalf("exec %q: %v", strings.TrimSpace(query), err)
	}
}

// chat inserts a cloud_chat row. filtered mirrors iCloud's Unknown Senders bucket.
func (h *syncStatusHarness) chat(portalID, chatID string, filtered bool, deleted bool) {
	h.t.Helper()
	f := 0
	if filtered {
		f = 1
	}
	h.exec(`
		INSERT INTO cloud_chat (login_id, cloud_chat_id, record_name, group_id, portal_id,
			service, display_name, participants_json, updated_ts, created_ts, is_filtered, deleted)
		VALUES ($1, $2, $3, '', $4, 'iMessage', '', '[]', 1000, 1000, $5, $6)
	`, testSQLLoginID, chatID, "chatrec-"+chatID, portalID, f, deleted)
}

// msg describes one cloud_message row. The zero value is a plain, contentful,
// live message; each field turns on one of the conditions the report sorts on.
type msg struct {
	guid     string
	portal   string
	ts       int64
	text     string
	sender   string
	fromMe   bool
	attach   string
	scrubbed bool
	tapback  int // 0 = not a tapback; >= 2000 = reaction
	deleted  bool
	noBody   bool
	noSender bool // a not-from-me row with no sender: an iMessage system record
	noRecord bool // an echo-dedup stub: a GUID with no CloudKit record behind it
}

func (h *syncStatusHarness) msg(m msg) {
	h.t.Helper()
	sender := m.sender
	if sender == "" && !m.fromMe && !m.scrubbed && !m.noSender {
		sender = "tel:+15550001"
	}
	record := "rec-" + m.guid
	if m.noRecord {
		record = ""
	}
	var tapback any
	if m.tapback != 0 {
		tapback = m.tapback
	}
	h.exec(`
		INSERT INTO cloud_message (login_id, guid, chat_id, portal_id, timestamp_ms, sender,
			is_from_me, text, subject, service, deleted, tapback_type, attachments_json,
			created_ts, updated_ts, record_name, has_body, body_scrubbed)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, '', 'iMessage', $9, $10, $11, 1000, 1000, $12, $13, $14)
	`, testSQLLoginID, m.guid, "chat-"+m.portal, m.portal, m.ts, sender, m.fromMe,
		m.text, m.deleted, tapback, m.attach, record, !m.noBody, m.scrubbed)
}

// delivered records that a message reached Matrix. id is written verbatim so
// tests can cover the uppercase-GUID and `<guid>_<part>` spellings bridgev2
// actually stores. tsMS is given in milliseconds for symmetry with
// cloud_message, and written out in nanoseconds because that is what bridgev2
// stores in message.timestamp.
func (h *syncStatusHarness) delivered(id, roomReceiver string, tsMS int64) {
	h.t.Helper()
	h.exec(`INSERT INTO message (bridge_id, id, room_receiver, timestamp) VALUES ($1, $2, $3, $4)`,
		syncStatusTestBridgeID, id, roomReceiver, tsMS*int64(time.Millisecond))
}

func TestGetSyncStatus(t *testing.T) {
	loginID := string(testSQLLoginID)
	now := time.Now().UnixMilli()

	// zonesSynced marks the chats + messages zones successful, which is the
	// bootstrap gate the bridge itself uses.
	zonesSynced := func(h *syncStatusHarness) {
		for _, zone := range []string{cloudZoneChats, cloudZoneMessages} {
			h.exec(`
				INSERT INTO cloud_sync_state (login_id, zone, continuation_token, last_success_ts, last_error, updated_ts)
				VALUES ($1, $2, 'tok', $3, NULL, $3)
			`, testSQLLoginID, zone, now)
		}
	}

	cases := []syncStatusFixture{
		{
			name: "fully caught up",
			setup: func(t *testing.T, h *syncStatusHarness) {
				zonesSynced(h)
				h.chat("tel:+15550100", "c1", false, false)
				for i := 1; i <= 3; i++ {
					guid := fmt.Sprintf("guid-%d", i)
					h.msg(msg{guid: guid, portal: "tel:+15550100", ts: int64(i) * 1000, text: "hello"})
					h.delivered(guid, loginID, int64(i)*1000)
				}
			},
			check: func(t *testing.T, r *SyncStatusReport) {
				assertCounts(t, r, counts{candidates: 3, deliverable: 3, delivered: 3})
				if !r.BootstrapComplete {
					t.Error("BootstrapComplete = false, want true")
				}
				if !r.FullyCaughtUp() {
					t.Error("FullyCaughtUp = false, want true")
				}
				if got := r.DeliveredPercent(); got != 100 {
					t.Errorf("DeliveredPercent = %v, want 100", got)
				}
				// bridgev2 stores message.timestamp in nanoseconds; reading it
				// as milliseconds would put the last delivery in 1970 and make
				// every "N ago" in the report nonsense.
				if r.LastDeliveredAt == nil {
					t.Fatal("LastDeliveredAt = nil, want the newest message timestamp")
				} else if got := r.LastDeliveredAt.UnixMilli(); got != 3000 {
					t.Errorf("LastDeliveredAt = %d ms, want 3000", got)
				}
			},
		},
		{
			name: "partially delivered",
			setup: func(t *testing.T, h *syncStatusHarness) {
				zonesSynced(h)
				h.chat("tel:+15550100", "c1", false, false)
				for i := 1; i <= 4; i++ {
					h.msg(msg{guid: fmt.Sprintf("guid-%d", i), portal: "tel:+15550100", ts: int64(i) * 1000, text: "hello"})
				}
				// Delivered in the two spellings bridgev2 really writes: an
				// uppercase APNs guid, and a part-suffixed id from a message
				// that produced several events.
				h.delivered("GUID-1", loginID, 1000)
				h.delivered("guid-2_1", "", 2000)
				h.exec(`INSERT INTO backfill_task (bridge_id, portal_id, user_login_id, is_done, dispatched_at)
					VALUES ($1, 'tel:+15550100', $2, FALSE, NULL)`, syncStatusTestBridgeID, testSQLLoginID)
			},
			check: func(t *testing.T, r *SyncStatusReport) {
				assertCounts(t, r, counts{candidates: 4, deliverable: 4, delivered: 2})
				if got := r.PendingMessages(); got != 2 {
					t.Errorf("PendingMessages = %d, want 2", got)
				}
				if got := r.DeliveredPercent(); got != 50 {
					t.Errorf("DeliveredPercent = %v, want 50", got)
				}
				if r.FullyCaughtUp() {
					t.Error("FullyCaughtUp = true, want false")
				}
				if r.BackfillTasksUndispatched != 1 {
					t.Errorf("BackfillTasksUndispatched = %d, want 1", r.BackfillTasksUndispatched)
				}
			},
		},
		{
			name: "nothing ingested yet",
			setup: func(t *testing.T, h *syncStatusHarness) {
				// Logged in, schema created, CloudKit has not delivered anything.
			},
			check: func(t *testing.T, r *SyncStatusReport) {
				assertCounts(t, r, counts{})
				if r.BootstrapComplete {
					t.Error("BootstrapComplete = true, want false before any zone succeeds")
				}
				if len(r.Zones) != 3 {
					t.Fatalf("len(Zones) = %d, want 3", len(r.Zones))
				}
				for _, z := range r.Zones {
					if z.Present {
						t.Errorf("zone %s reported present with no row", z.Zone)
					}
				}
				if r.FullyCaughtUp() {
					t.Error("FullyCaughtUp = true, want false before bootstrap")
				}
				if !strings.Contains(r.Format(), "initial sync has not finished") {
					t.Error("Format did not mention that the initial sync is unfinished")
				}
			},
		},
		{
			name: "rows that will never bridge are kept out of the denominator",
			setup: func(t *testing.T, h *syncStatusHarness) {
				zonesSynced(h)
				h.chat("tel:+15550100", "c1", false, false)
				// iCloud filed this one under Unknown Senders.
				h.chat("tel:+15559999", "c2", true, false)
				// One portal, two chat rows (iMessage + SMS for the same
				// handle) where only one is filtered: still bridgeable.
				h.chat("tel:+15550200", "c3-sms", true, false)
				h.chat("tel:+15550200", "c3-im", false, false)

				h.msg(msg{guid: "ok-1", portal: "tel:+15550100", ts: 5000, text: "hello"})
				h.msg(msg{guid: "ok-2", portal: "tel:+15550200", ts: 5000, text: "hello"})
				h.msg(msg{guid: "filtered-1", portal: "tel:+15559999", ts: 5000, text: "spam"})
				// Empty/system shapes: no body at all, and a not-from-me row
				// with no sender (cloudRowToBackfillMessages drops both).
				h.msg(msg{guid: "system-1", portal: "tel:+15550100", ts: 4000, noBody: true})
				h.msg(msg{guid: "system-2", portal: "tel:+15550100", ts: 3000, text: "hi", noSender: true})
				// A reaction: bridges into the `reaction` table, so it must not
				// be counted as a message anywhere.
				h.msg(msg{guid: "react-1", portal: "tel:+15550100", ts: 6000, text: "Loved x", tapback: 2000})
				// Soft-deleted, and an echo-dedup stub with no record_name:
				// backfill cannot read either.
				h.msg(msg{guid: "gone-1", portal: "tel:+15550100", ts: 2000, text: "bye", deleted: true})
				h.msg(msg{guid: "stub-1", portal: "tel:+15550100", ts: 1000, noRecord: true})

				h.delivered("ok-1", loginID, 5000)
				h.delivered("ok-2", loginID, 5000)
			},
			check: func(t *testing.T, r *SyncStatusReport) {
				assertCounts(t, r, counts{candidates: 5, deliverable: 2, delivered: 2, filtered: 1, empty: 2})
				// Three conversations across four cloud_chat rows, and only
				// the genuinely-skipped one counts as skipped: tel:+15550200
				// keeps its room despite the filtered SMS sibling.
				if r.ChatsIngested != 3 {
					t.Errorf("ChatsIngested = %d, want 3 portals (not 4 chat rows)", r.ChatsIngested)
				}
				if r.ChatsFiltered != 1 {
					t.Errorf("ChatsFiltered = %d, want 1 — the portal with a bridgeable sibling is not skipped", r.ChatsFiltered)
				}
				if got := r.DeliveredPercent(); got != 100 {
					t.Errorf("DeliveredPercent = %v, want 100 once the unbridgeable rows are excluded", got)
				}
				out := r.Format()
				for _, want := range []string{"Unknown Senders", "empty or system rows"} {
					if !strings.Contains(out, want) {
						t.Errorf("Format missing %q:\n%s", want, out)
					}
				}
			},
		},
		{
			name: "capped backfill puts the older tail outside deliverable",
			opts: SyncStatusOptions{MaxInitialMessages: 2},
			setup: func(t *testing.T, h *syncStatusHarness) {
				zonesSynced(h)
				h.chat("tel:+15550100", "c1", false, false)
				// Newest first: a reaction, then four messages. The reaction
				// takes a slot, because listLatestMessages — which IS the
				// forward backfill at room creation — does not filter it out.
				h.msg(msg{guid: "react-1", portal: "tel:+15550100", ts: 5000, text: "Loved", tapback: 2000})
				h.msg(msg{guid: "m-4", portal: "tel:+15550100", ts: 4000, text: "newest"})
				h.msg(msg{guid: "m-3", portal: "tel:+15550100", ts: 3000, text: "older"})
				h.msg(msg{guid: "m-2", portal: "tel:+15550100", ts: 2000, text: "older still"})
				h.msg(msg{guid: "m-1", portal: "tel:+15550100", ts: 1000, text: "oldest"})
				h.delivered("m-4", loginID, 4000)
			},
			check: func(t *testing.T, r *SyncStatusReport) {
				assertCounts(t, r, counts{candidates: 4, deliverable: 1, delivered: 1, beyondCap: 3})
				if got := r.DeliveredPercent(); got != 100 {
					t.Errorf("DeliveredPercent = %v, want 100 — the tail is not pending", got)
				}
				if !r.FullyCaughtUp() {
					t.Error("FullyCaughtUp = false, want true with the tail excluded")
				}
				out := r.Format()
				if !strings.Contains(out, "backward backfill is switched off entirely") {
					t.Errorf("Format did not explain the cap:\n%s", out)
				}
			},
		},
		{
			name: "uncapped sentinel is not treated as a cap",
			opts: SyncStatusOptions{MaxInitialMessages: math.MaxInt32},
			setup: func(t *testing.T, h *syncStatusHarness) {
				zonesSynced(h)
				h.chat("tel:+15550100", "c1", false, false)
				for i := 1; i <= 3; i++ {
					h.msg(msg{guid: fmt.Sprintf("guid-%d", i), portal: "tel:+15550100", ts: int64(i) * 1000, text: "hello"})
				}
			},
			check: func(t *testing.T, r *SyncStatusReport) {
				assertCounts(t, r, counts{candidates: 3, deliverable: 3})
				if r.MessageCap != 0 {
					t.Errorf("MessageCap = %d, want 0 (uncapped)", r.MessageCap)
				}
				if strings.Contains(r.Format(), "backward backfill is switched off") {
					t.Error("Format claimed a cap on an uncapped install")
				}
			},
		},
		{
			name: "bridge_filtered_chats brings the filtered chats back in",
			opts: SyncStatusOptions{BridgeFilteredChats: true},
			setup: func(t *testing.T, h *syncStatusHarness) {
				zonesSynced(h)
				h.chat("tel:+15559999", "c2", true, false)
				h.msg(msg{guid: "filtered-1", portal: "tel:+15559999", ts: 5000, text: "spam"})
			},
			check: func(t *testing.T, r *SyncStatusReport) {
				assertCounts(t, r, counts{candidates: 1, deliverable: 1})
			},
		},
		{
			name: "scrubbed rows still count as content",
			setup: func(t *testing.T, h *syncStatusHarness) {
				zonesSynced(h)
				h.chat("tel:+15550100", "c1", false, false)
				// The privacy scrubber nulls text and sender once a row is
				// bridged. Read as "empty", these would file a healthy
				// long-running bridge under "not bridgeable".
				h.msg(msg{guid: "scrubbed-1", portal: "tel:+15550100", ts: 1000, scrubbed: true})
				h.delivered("scrubbed-1", loginID, 1000)
			},
			check: func(t *testing.T, r *SyncStatusReport) {
				assertCounts(t, r, counts{candidates: 1, deliverable: 1, delivered: 1})
			},
		},
		{
			name:            "database with no CloudKit tables",
			skipCloudSchema: true,
			check: func(t *testing.T, r *SyncStatusReport) {
				if !r.HasLogin {
					t.Error("HasLogin = false, want true")
				}
				if r.CloudTablesPresent {
					t.Error("CloudTablesPresent = true, want false")
				}
				if !strings.Contains(r.Format(), "CloudKit backfill has never initialized") {
					t.Errorf("Format did not explain the missing CloudKit schema:\n%s", r.Format())
				}
			},
		},
		{
			name:            "database with no bridgev2 tables at all",
			skipFramework:   true,
			skipCloudSchema: true,
			check: func(t *testing.T, r *SyncStatusReport) {
				if r.HasLogin {
					t.Error("HasLogin = true on a database with no user_login table")
				}
				if !strings.Contains(r.Format(), "No iMessage account has logged in") {
					t.Errorf("Format did not explain the empty database:\n%s", r.Format())
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := &syncStatusHarness{t: t, ctx: ctx, db: newTestSQLiteDB(t)}
			if !tc.skipFramework {
				if _, err := h.db.RawDB.Exec(syncStatusFrameworkSchema); err != nil {
					t.Fatalf("create framework schema: %v", err)
				}
				h.exec(`INSERT INTO user_login (bridge_id, id) VALUES ($1, $2)`,
					syncStatusTestBridgeID, testSQLLoginID)
			}
			if !tc.skipCloudSchema {
				if err := newCloudBackfillStore(h.db, testSQLLoginID).ensureSchema(ctx); err != nil {
					t.Fatalf("ensureSchema: %v", err)
				}
			}
			if tc.setup != nil {
				tc.setup(t, h)
			}

			report, err := GetSyncStatus(ctx, h.db, tc.opts)
			if err != nil {
				t.Fatalf("GetSyncStatus: %v", err)
			}
			assertInvariants(t, report)
			if tc.check != nil {
				tc.check(t, report)
			}
			// Format must never panic, whatever shape the report is.
			if report.Format() == "" {
				t.Error("Format returned an empty string")
			}
		})
	}
}

type counts struct {
	candidates  int
	deliverable int
	delivered   int
	filtered    int
	empty       int
	beyondCap   int
}

func assertCounts(t *testing.T, r *SyncStatusReport, want counts) {
	t.Helper()
	for _, c := range []struct {
		name      string
		got, want int
	}{
		{"CandidateMessages", r.CandidateMessages, want.candidates},
		{"DeliverableMessages", r.DeliverableMessages, want.deliverable},
		{"DeliveredMessages", r.DeliveredMessages, want.delivered},
		{"FilteredChatMessages", r.FilteredChatMessages, want.filtered},
		{"EmptySystemMessages", r.EmptySystemMessages, want.empty},
		{"BeyondCapMessages", r.BeyondCapMessages, want.beyondCap},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

// assertInvariants holds for every report, and is where a miscounted bucket
// shows up as "the percentage can never reach 100%".
func assertInvariants(t *testing.T, r *SyncStatusReport) {
	t.Helper()
	if r.DeliveredMessages > r.DeliverableMessages {
		t.Errorf("delivered %d > deliverable %d", r.DeliveredMessages, r.DeliverableMessages)
	}
	if sum := r.DeliverableMessages + r.UnbridgeableMessages(); sum != r.CandidateMessages {
		t.Errorf("deliverable %d + unbridgeable %d = %d, want candidates %d",
			r.DeliverableMessages, r.UnbridgeableMessages(), sum, r.CandidateMessages)
	}
	if pct := r.DeliveredPercent(); pct > 100 || pct < 0 {
		t.Errorf("DeliveredPercent = %v, want 0..100", pct)
	}
}
