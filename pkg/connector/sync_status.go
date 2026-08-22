// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package connector

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2/commands"
)

// sync-status answers the one question users keep filing issues about: "is my
// backfill actually finished, and if not, what is stuck?"
//
// Everything below is READ-ONLY and derived from rows the bridge already
// writes. There are deliberately no counters: this bridge serializes its
// SQLite pool to a single connection because write contention during backfill
// was stranding conversations, so a report that wrote two rows per delivered
// message would make the problem it diagnoses worse. Anything that cannot be
// derived from existing rows is simply not reported (see the omissions noted
// on SyncStatusReport).
//
// The same code backs both entry points — the management-room `sync-status`
// command and `corten-matrix sync-status` — because the CLI has to work with
// no daemon running, which rules out anything that reads live process state.
// The two facts that only the running bridge knows (is a CloudKit sync in
// flight, does the homeserver support batch sending) are passed in as
// optional pointers and simply described as unknown when absent.

// ZoneSyncStatus is the persisted state of one CloudKit zone, straight out of
// cloud_sync_state.
type ZoneSyncStatus struct {
	Zone        string
	Present     bool
	HasToken    bool
	LastSuccess *time.Time
	LastError   string
	UpdatedAt   *time.Time
}

// SyncStatusReport is a point-in-time picture of CloudKit ingestion and Matrix
// delivery for one login, built entirely from persisted rows.
//
// Not reported, because deriving it would require writing on the message path:
// a live-message counter and a StatusKit-update counter. LastDeliveredAt is
// the honest read-only stand-in for the first — it says when Matrix last
// received anything, not how much has flowed. There is no read-only stand-in
// for the second: StatusKit Focus/DND changes rewrite room state and leave no
// row behind, so they are omitted entirely.
type SyncStatusReport struct {
	BridgeID string
	LoginID  string

	// HasLogin is false on a database no account has ever logged into — the
	// normal state when the CLI is run before setup finishes.
	HasLogin bool
	// CloudTablesPresent is false before the CloudKit store has ever created
	// its schema (backfill disabled, or first boot not finished).
	CloudTablesPresent bool

	// BootstrapComplete mirrors the gate the bridge itself uses: the chats and
	// messages zones have each recorded at least one successful sync.
	BootstrapComplete bool
	Zones             []ZoneSyncStatus

	ChatsIngested int
	ChatsFiltered int

	// The message buckets below are disjoint and sum to CandidateMessages.
	// Reactions are never counted anywhere: they bridge into bridgev2's
	// separate `reaction` table, so a reaction would otherwise sit in
	// "pending" forever.
	CandidateMessages int
	// DeliverableMessages is the set the bridge will actually deliver here.
	// It is NOT gated on the Matrix room existing yet, so a message whose
	// portal has not been created is deliverable-but-pending — which is what
	// makes the percentage a real progress signal.
	DeliverableMessages int
	// DeliveredMessages is a subset of DeliverableMessages by construction:
	// same predicate, plus a matching row in bridgev2's `message` table.
	DeliveredMessages int
	// FilteredChatMessages / EmptySystemMessages / BeyondCapMessages are the
	// rows that are NOT deliverable and never will be. Counting them as
	// deliverable is what keeps a percentage from ever reaching 100%.
	FilteredChatMessages int
	EmptySystemMessages  int
	BeyondCapMessages    int

	// MessageCap is the effective Backfill.MaxInitialMessages, or 0 when
	// backfill is uncapped.
	MessageCap          int
	BridgeFilteredChats bool

	// LastDeliveredAt is the newest timestamp in bridgev2's `message` table
	// for this login — "Matrix last heard from us at", nothing more.
	LastDeliveredAt *time.Time

	BackfillTasksPresent      bool
	BackfillTasksTotal        int
	BackfillTasksDone         int
	BackfillTasksUndispatched int

	// SyncRunning and BatchSending are known only inside the running bridge;
	// nil means "not knowable from here" and is rendered as such.
	SyncRunning  *bool
	BatchSending *bool
}

// SyncStatusOptions carries the bits of configuration the report needs. Both
// entry points fill it in: the bridge command from live config, the CLI by
// parsing the same config.yaml.
type SyncStatusOptions struct {
	// BridgeID and LoginID may be left empty, in which case they are read
	// from the single user_login row. The CLI has no other way to learn them.
	BridgeID string
	LoginID  string

	// MaxInitialMessages is the EFFECTIVE value after the connector's own
	// normalization (see PostInit): anything <= 0 or >= math.MaxInt32 means
	// uncapped. A real cap changes what "deliverable" means, so passing the
	// raw config value here when the connector would have overridden it
	// produces a wrong report rather than a slightly-off one.
	MaxInitialMessages int
	// BridgeFilteredChats mirrors IMConfig.BridgeFilteredChats: when true,
	// iCloud-filtered chats are bridged and so none of their messages land in
	// the "not bridgeable" bucket.
	BridgeFilteredChats bool

	SyncRunning  *bool
	BatchSending *bool
}

// effectiveCap turns the configured value into "0 = uncapped, N = cap".
func (o SyncStatusOptions) effectiveCap() int {
	if o.MaxInitialMessages <= 0 || o.MaxInitialMessages >= math.MaxInt32 {
		return 0
	}
	return o.MaxInitialMessages
}

// PendingMessages is deliverable-but-not-yet-in-Matrix.
func (r *SyncStatusReport) PendingMessages() int {
	if n := r.DeliverableMessages - r.DeliveredMessages; n > 0 {
		return n
	}
	return 0
}

// UnbridgeableMessages is the total of the three not-deliverable buckets.
func (r *SyncStatusReport) UnbridgeableMessages() int {
	return r.FilteredChatMessages + r.EmptySystemMessages + r.BeyondCapMessages
}

// DeliveredPercent is delivered/deliverable as a percentage, 100 when there is
// nothing to deliver.
func (r *SyncStatusReport) DeliveredPercent() float64 {
	if r.DeliverableMessages <= 0 {
		return 100
	}
	return 100 * float64(r.DeliveredMessages) / float64(r.DeliverableMessages)
}

// FullyCaughtUp reports that CloudKit bootstrap finished and every deliverable
// message reached Matrix.
func (r *SyncStatusReport) FullyCaughtUp() bool {
	return r.HasLogin && r.BootstrapComplete && r.PendingMessages() == 0
}

// syncStatusTableExists reports whether a table is present, so a partial or
// brand-new database degrades into a shorter report instead of an error. The
// CLI is expected to be pointed at exactly such a database — someone runs it
// while setup is still going — so this is the normal path, not an edge case.
func syncStatusTableExists(ctx context.Context, db *dbutil.Database, table string) bool {
	var count int
	var err error
	switch db.Dialect {
	case dbutil.Postgres:
		err = db.QueryRow(ctx,
			`SELECT COUNT(*) FROM information_schema.tables WHERE table_name=$1`, table).Scan(&count)
	default:
		err = db.QueryRow(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=$1`, table).Scan(&count)
	}
	return err == nil && count > 0
}

// portalBridgeableSQL is the "will this portal get a Matrix room at all" test,
// mirroring listPortalIDsWithNewestTimestamp exactly — including the shape that
// only skips a portal when EVERY cloud_chat row behind it is filtered. One
// portal can carry both an iMessage and an SMS chat row for the same handle,
// and a bare EXISTS(filtered) suppressed the whole portal on the strength of
// the filtered sibling, which is a bug this repo has already fixed once.
//
// It is shared by the chat count and the message classification so the two can
// never disagree about which conversations are being skipped.
func portalBridgeableSQL(portalCol, loginParam, bridgeFilteredParam string) string {
	return bridgeFilteredParam + `
		OR NOT EXISTS (
		  SELECT 1 FROM cloud_chat fc
		  WHERE fc.login_id=` + loginParam + ` AND fc.portal_id=` + portalCol + `
		    AND COALESCE(fc.is_filtered, 0) <> 0
		)
		OR EXISTS (
		  SELECT 1 FROM cloud_chat fc
		  WHERE fc.login_id=` + loginParam + ` AND fc.portal_id=` + portalCol + `
		    AND COALESCE(fc.is_filtered, 0) = 0 AND fc.deleted = FALSE
		)`
}

// deliveredGUIDSetSQL builds the subquery of iMessage GUIDs that reached
// Matrix, in the same spelling scrubBridgedBodies uses to decide a row is
// safe to scrub — the two have to agree or the bridge would scrub bodies the
// report still counts as pending.
//
// Two normalizations matter. bridgev2 stores the base message ID in `id`, but
// a row that produced several events (text plus attachments) is stored as
// `<guid>_<part>`; guids are UUIDs and contain no underscore, so cutting at
// the first underscore recovers the base. And APNs hands us uppercase guids
// where CloudKit uses mixed case, so both sides are upper-cased.
//
// Parameters: $1 login_id (matched against room_receiver, which bridgev2
// leaves empty for shared portals), $2 bridge_id.
func deliveredGUIDSetSQL(db *dbutil.Database) string {
	instr := sqlInstrFunc(db)
	return `
		SELECT UPPER(m.id) FROM message m
		WHERE m.bridge_id=$2 AND (m.room_receiver=$1 OR m.room_receiver='')
		UNION
		SELECT UPPER(substr(m.id, 1, ` + instr + `(m.id, '_') - 1)) FROM message m
		WHERE m.bridge_id=$2 AND ` + instr + `(m.id, '_') > 0
		  AND (m.room_receiver=$1 OR m.room_receiver='')`
}

// GetSyncStatus builds the report from persisted state alone. It touches no
// connector or client state, which is what lets the CLI run it against a
// database with no daemon attached.
func GetSyncStatus(ctx context.Context, db *dbutil.Database, opts SyncStatusOptions) (*SyncStatusReport, error) {
	report := &SyncStatusReport{
		BridgeID:            opts.BridgeID,
		LoginID:             opts.LoginID,
		MessageCap:          opts.effectiveCap(),
		BridgeFilteredChats: opts.BridgeFilteredChats,
		SyncRunning:         opts.SyncRunning,
		BatchSending:        opts.BatchSending,
	}

	// Both ids are filled in from the database when the caller does not know
	// them. The bridge ID matters as much as the login: every delivery query
	// joins on it, so inheriting an empty one would report 0% delivered on a
	// perfectly healthy bridge.
	if report.LoginID == "" || report.BridgeID == "" {
		if !syncStatusTableExists(ctx, db, "user_login") {
			// Database exists but bridgev2 has never migrated it.
			return report, nil
		}
		// One login per database: the two-account setup gives each account
		// its own data dir, config and database.
		var bridgeID, loginID string
		err := db.QueryRow(ctx, `SELECT bridge_id, id FROM user_login ORDER BY id LIMIT 1`).
			Scan(&bridgeID, &loginID)
		if err != nil {
			if err == sql.ErrNoRows {
				return report, nil
			}
			return nil, fmt.Errorf("failed to look up the login: %w", err)
		}
		if report.LoginID == "" {
			report.LoginID = loginID
		}
		if report.BridgeID == "" {
			report.BridgeID = bridgeID
		}
	}
	report.HasLogin = report.LoginID != ""
	if !report.HasLogin {
		return report, nil
	}

	report.CloudTablesPresent = syncStatusTableExists(ctx, db, "cloud_message") &&
		syncStatusTableExists(ctx, db, "cloud_chat") &&
		syncStatusTableExists(ctx, db, "cloud_sync_state")

	if report.CloudTablesPresent {
		if err := report.readZones(ctx, db); err != nil {
			return nil, err
		}
		if err := report.readChatCounts(ctx, db); err != nil {
			return nil, err
		}
		if syncStatusTableExists(ctx, db, "message") {
			if err := report.readMessageCounts(ctx, db); err != nil {
				return nil, err
			}
		}
	}
	if syncStatusTableExists(ctx, db, "message") {
		if err := report.readLastDelivered(ctx, db); err != nil {
			return nil, err
		}
	}
	if syncStatusTableExists(ctx, db, "backfill_task") {
		if err := report.readBackfillTasks(ctx, db); err != nil {
			return nil, err
		}
	}
	return report, nil
}

// readZones fills in per-zone last success / last error. A zone with no row has
// never been synced at all, which is a different (and more useful) statement
// than "synced, no error".
func (r *SyncStatusReport) readZones(ctx context.Context, db *dbutil.Database) error {
	rows, err := db.Query(ctx, `
		SELECT zone, continuation_token, last_success_ts, last_error, updated_ts
		FROM cloud_sync_state
		WHERE login_id=$1
	`, r.LoginID)
	if err != nil {
		return fmt.Errorf("failed to read CloudKit sync state: %w", err)
	}
	defer rows.Close()

	found := make(map[string]ZoneSyncStatus, 3)
	for rows.Next() {
		var zone string
		var token, lastErr sql.NullString
		var lastSuccess, updated sql.NullInt64
		if err := rows.Scan(&zone, &token, &lastSuccess, &lastErr, &updated); err != nil {
			return fmt.Errorf("failed to read CloudKit sync state: %w", err)
		}
		z := ZoneSyncStatus{
			Zone:      zone,
			Present:   true,
			HasToken:  token.Valid && token.String != "",
			LastError: lastErr.String,
		}
		if lastSuccess.Valid && lastSuccess.Int64 > 0 {
			t := time.UnixMilli(lastSuccess.Int64)
			z.LastSuccess = &t
		}
		if updated.Valid && updated.Int64 > 0 {
			t := time.UnixMilli(updated.Int64)
			z.UpdatedAt = &t
		}
		found[zone] = z
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to read CloudKit sync state: %w", err)
	}

	for _, zone := range []string{cloudZoneChats, cloudZoneMessages, cloudZoneAttachments} {
		z, ok := found[zone]
		if !ok {
			z = ZoneSyncStatus{Zone: zone}
		}
		r.Zones = append(r.Zones, z)
	}
	// The same two-zone gate the bridge uses before it lets APNs create portals.
	r.BootstrapComplete = found[cloudZoneChats].LastSuccess != nil &&
		found[cloudZoneMessages].LastSuccess != nil
	return nil
}

// readChatCounts counts ingested conversations, and separately how many of
// them are being skipped because iCloud filed them under "Unknown Senders" —
// the usual explanation for "some of my chats never appeared".
//
// Counting is per PORTAL, not per cloud_chat row: one portal routinely carries
// several rows (an iMessage chat and an SMS chat for the same handle both key
// to tel:+…), so counting rows overstates how many conversations exist. The
// skipped count uses the same predicate portal creation does rather than
// COUNT(is_filtered), because a portal with one filtered and one unfiltered
// chat row is still bridged — counting it as skipped would tell the user a
// conversation is missing when it is right there.
func (r *SyncStatusReport) readChatCounts(ctx context.Context, db *dbutil.Database) error {
	var total int
	var skipped sql.NullInt64
	err := db.QueryRow(ctx, `
		SELECT COUNT(*), SUM(CASE WHEN p.bridgeable = 0 THEN 1 ELSE 0 END)
		FROM (
			SELECT DISTINCT cc.portal_id,
				CASE WHEN `+portalBridgeableSQL("cc.portal_id", "$1", "$2")+`
					THEN 1 ELSE 0 END AS bridgeable
			FROM cloud_chat cc
			WHERE cc.login_id=$1 AND cc.deleted=FALSE
			  AND cc.portal_id IS NOT NULL AND cc.portal_id <> ''
		) p
	`, r.LoginID, r.BridgeFilteredChats).Scan(&total, &skipped)
	if err != nil {
		return fmt.Errorf("failed to count chats: %w", err)
	}
	r.ChatsIngested = total
	r.ChatsFiltered = int(skipped.Int64)
	return nil
}

// readMessageCounts is the heart of the report: one pass over cloud_message
// that sorts every row into exactly one bucket.
//
// The buckets are evaluated in priority order — filtered chat, then empty /
// system row, then beyond the cap, then deliverable — so they are disjoint and
// sum to CandidateMessages. That ordering is what stops the same row being
// both "not bridgeable" and "pending", which is how a percentage ends up
// unable to reach 100%.
//
// Why each predicate is what it is:
//
//   - Not deleted, a non-empty record_name, and a non-empty portal_id is
//     exactly what listLatestMessages / listBackwardMessages read, so it is
//     exactly the set backfill can see. The record_name test drops the
//     echo-dedup stubs persistMessageUUID writes, which carry a GUID and
//     nothing else.
//   - Reactions (tapback_type >= 2000) are dropped from the counts entirely,
//     because they bridge into bridgev2's `reaction` table and would never
//     appear in `message` — counted as deliverable they would be pending
//     forever. They still take part in the ROW_NUMBER ranking below, because
//     listLatestMessages does not filter them and they really do consume slots
//     under a cap.
//   - "Contentful" reuses the predicate from countBackfillableMessages, and
//     body_scrubbed=TRUE counts as content: on a long-running bridge most
//     successfully delivered rows have had their text nulled by the privacy
//     scrubber, and treating those as empty would file the bulk of a healthy
//     database under "not bridgeable". The second half of the clause is
//     cloudRowToBackfillMessages' empty-sender drop (a not-from-me row with no
//     sender is a system record and never becomes a Matrix event).
//   - The filtered-chat test is portalBridgeableSQL, shared with the chat
//     count so the two can never disagree about which conversations are
//     skipped.
//   - The cap window is the newest N rows per portal by (timestamp_ms DESC,
//     guid DESC), which is listLatestMessages' exact ordering — that call, with
//     count = MaxInitialMessages, IS the forward backfill at room creation.
//     Ranking happens before the contentful filter because that is the order
//     the store applies it in.
func (r *SyncStatusReport) readMessageCounts(ctx context.Context, db *dbutil.Database) error {
	query := `
		SELECT
			COUNT(*),
			SUM(CASE WHEN w.portal_bridgeable = 0 THEN 1 ELSE 0 END),
			SUM(CASE WHEN w.portal_bridgeable = 1 AND w.contentful = 0 THEN 1 ELSE 0 END),
			SUM(CASE WHEN w.portal_bridgeable = 1 AND w.contentful = 1
			              AND $3 > 0 AND w.rn > $3 THEN 1 ELSE 0 END),
			SUM(CASE WHEN w.portal_bridgeable = 1 AND w.contentful = 1
			              AND ($3 = 0 OR w.rn <= $3) THEN 1 ELSE 0 END),
			SUM(CASE WHEN w.portal_bridgeable = 1 AND w.contentful = 1
			              AND ($3 = 0 OR w.rn <= $3) AND w.delivered = 1 THEN 1 ELSE 0 END)
		FROM (
			SELECT
				CASE WHEN cm.tapback_type IS NOT NULL AND cm.tapback_type >= 2000
					THEN 1 ELSE 0 END AS is_reaction,
				CASE WHEN (COALESCE(cm.text, '') <> ''
				           OR COALESCE(cm.attachments_json, '') <> ''
				           OR cm.body_scrubbed = TRUE)
				          AND (cm.body_scrubbed = TRUE
				               OR cm.is_from_me = TRUE
				               OR COALESCE(cm.sender, '') <> '')
					THEN 1 ELSE 0 END AS contentful,
				CASE WHEN ` + portalBridgeableSQL("cm.portal_id", "$1", "$4") + `
					THEN 1 ELSE 0 END AS portal_bridgeable,
				CASE WHEN UPPER(cm.guid) IN (` + deliveredGUIDSetSQL(db) + `
				          ) THEN 1 ELSE 0 END AS delivered,
				ROW_NUMBER() OVER (
					PARTITION BY cm.portal_id
					ORDER BY cm.timestamp_ms DESC, cm.guid DESC
				) AS rn
			FROM cloud_message cm
			WHERE cm.login_id=$1 AND cm.deleted=FALSE AND cm.record_name <> ''
			  AND cm.portal_id IS NOT NULL AND cm.portal_id <> ''
		) w
		WHERE w.is_reaction = 0
	`
	var candidates int
	var filtered, empty, beyondCap, deliverable, delivered sql.NullInt64
	err := db.QueryRow(ctx, query, r.LoginID, r.BridgeID, r.MessageCap, r.BridgeFilteredChats).
		Scan(&candidates, &filtered, &empty, &beyondCap, &deliverable, &delivered)
	if err != nil {
		return fmt.Errorf("failed to count messages: %w", err)
	}
	r.CandidateMessages = candidates
	r.FilteredChatMessages = int(filtered.Int64)
	r.EmptySystemMessages = int(empty.Int64)
	r.BeyondCapMessages = int(beyondCap.Int64)
	r.DeliverableMessages = int(deliverable.Int64)
	r.DeliveredMessages = int(delivered.Int64)
	return nil
}

// readLastDelivered reads when Matrix last received anything for this login.
// This is the read-only stand-in for a live-message counter: it says something
// arrived recently, not how much has arrived.
func (r *SyncStatusReport) readLastDelivered(ctx context.Context, db *dbutil.Database) error {
	var newest sql.NullInt64
	err := db.QueryRow(ctx, `
		SELECT MAX(m.timestamp) FROM message m
		WHERE m.bridge_id=$2 AND (m.room_receiver=$1 OR m.room_receiver='')
	`, r.LoginID, r.BridgeID).Scan(&newest)
	if err != nil {
		return fmt.Errorf("failed to read the last delivered message: %w", err)
	}
	if newest.Valid && newest.Int64 > 0 {
		// bridgev2 stores message timestamps in nanoseconds.
		t := time.Unix(0, newest.Int64)
		r.LastDeliveredAt = &t
	}
	return nil
}

// readBackfillTasks summarizes bridgev2's backfill queue table. A pile of
// tasks that were never dispatched is the visible symptom of a queue that is
// not running — see the note Format prints about batch sending.
func (r *SyncStatusReport) readBackfillTasks(ctx context.Context, db *dbutil.Database) error {
	var total int
	var done, undispatched sql.NullInt64
	err := db.QueryRow(ctx, `
		SELECT COUNT(*),
		       SUM(CASE WHEN bt.is_done = TRUE THEN 1 ELSE 0 END),
		       SUM(CASE WHEN bt.dispatched_at IS NULL THEN 1 ELSE 0 END)
		FROM backfill_task bt
		WHERE bt.bridge_id=$1 AND bt.user_login_id=$2
	`, r.BridgeID, r.LoginID).Scan(&total, &done, &undispatched)
	if err != nil {
		return fmt.Errorf("failed to read backfill tasks: %w", err)
	}
	r.BackfillTasksPresent = true
	r.BackfillTasksTotal = total
	r.BackfillTasksDone = int(done.Int64)
	r.BackfillTasksUndispatched = int(undispatched.Int64)
	return nil
}

// zoneLabel is the short human name for a CloudKit zone constant.
func zoneLabel(zone string) string {
	switch zone {
	case cloudZoneChats:
		return "chats"
	case cloudZoneMessages:
		return "messages"
	case cloudZoneAttachments:
		return "attachments"
	default:
		return zone
	}
}

// syncStatusAgo renders "3m14s ago", or "never" for a nil timestamp.
func syncStatusAgo(t *time.Time) string {
	if t == nil {
		return "never"
	}
	d := time.Since(*t)
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%s ago", d.Round(time.Second))
}

// newestZoneActivity is the most recent updated_ts across zones. Outside the
// running bridge this is the only available hint that a sync is in flight.
func (r *SyncStatusReport) newestZoneActivity() *time.Time {
	var newest *time.Time
	for _, z := range r.Zones {
		if z.UpdatedAt != nil && (newest == nil || z.UpdatedAt.After(*newest)) {
			newest = z.UpdatedAt
		}
	}
	return newest
}

// Format renders the report as Markdown for the management room, which reads
// acceptably as plain text in a terminal too.
func (r *SyncStatusReport) Format() string {
	var sb strings.Builder

	if !r.HasLogin {
		sb.WriteString("**Sync status**\n")
		sb.WriteString("No iMessage account has logged in on this database yet, so there is nothing to report. Run `corten-matrix setup` (or `login`) first.\n")
		return sb.String()
	}
	if !r.CloudTablesPresent {
		sb.WriteString("**Sync status**\n")
		sb.WriteString("CloudKit backfill has never initialized on this database — either `cloudkit_backfill` is off, or the bridge has not finished its first startup. Only live messages are being bridged.\n")
		if r.LastDeliveredAt != nil {
			sb.WriteString(fmt.Sprintf("Matrix last received a message %s.\n", syncStatusAgo(r.LastDeliveredAt)))
		}
		return sb.String()
	}

	sb.WriteString("**CloudKit → database**\n")
	switch {
	case !r.BootstrapComplete:
		sb.WriteString("Status: ⏳ initial sync has not finished (room creation stays gated until it does)\n")
	case r.SyncRunning != nil && *r.SyncRunning:
		sb.WriteString("Status: ⏳ a sync is running right now\n")
	case r.SyncRunning != nil:
		sb.WriteString("Status: ✅ caught up\n")
	default:
		if recent := r.newestZoneActivity(); recent != nil && time.Since(*recent) < 5*time.Minute {
			sb.WriteString(fmt.Sprintf("Status: ⏳ probably still syncing — a zone was updated %s\n", syncStatusAgo(recent)))
		} else {
			sb.WriteString("Status: ✅ caught up, judging by the last zone update (whether a sync is in flight is only visible inside the running bridge)\n")
		}
	}
	for _, z := range r.Zones {
		if !z.Present {
			sb.WriteString(fmt.Sprintf("  %-12s never synced\n", zoneLabel(z.Zone)))
			continue
		}
		line := fmt.Sprintf("  %-12s last success: %s", zoneLabel(z.Zone), syncStatusAgo(z.LastSuccess))
		if z.LastError != "" {
			line += fmt.Sprintf("  ⚠️ last error: %s", z.LastError)
		}
		sb.WriteString(line + "\n")
	}
	sb.WriteString(fmt.Sprintf("Chats ingested: %d", r.ChatsIngested))
	if r.ChatsFiltered > 0 {
		sb.WriteString(fmt.Sprintf(" (%d skipped, filed by iCloud under Unknown Senders)", r.ChatsFiltered))
	}
	sb.WriteString("\n\n")

	sb.WriteString("**Database → Matrix**\n")
	sb.WriteString(fmt.Sprintf("Delivered: %d / %d bridgeable messages (%.1f%%), %d pending\n",
		r.DeliveredMessages, r.DeliverableMessages, r.DeliveredPercent(), r.PendingMessages()))
	if r.LastDeliveredAt != nil {
		sb.WriteString(fmt.Sprintf("Matrix last received a message %s\n", syncStatusAgo(r.LastDeliveredAt)))
	}

	if unbridgeable := r.UnbridgeableMessages(); unbridgeable > 0 {
		sb.WriteString(fmt.Sprintf("Not bridgeable (%d, excluded from the total above):\n", unbridgeable))
		if r.FilteredChatMessages > 0 {
			sb.WriteString(fmt.Sprintf("  %d in chats iCloud filed under Unknown Senders — set `bridge_filtered_chats: true` to bridge them\n", r.FilteredChatMessages))
		}
		if r.EmptySystemMessages > 0 {
			sb.WriteString(fmt.Sprintf("  %d empty or system rows (group renames, participant changes, senderless records) that never become Matrix events\n", r.EmptySystemMessages))
		}
		if r.BeyondCapMessages > 0 {
			sb.WriteString(fmt.Sprintf("  %d older than the newest %d per chat, which `max_initial_messages` caps delivery at\n", r.BeyondCapMessages, r.MessageCap))
		}
	}

	if r.MessageCap > 0 {
		sb.WriteString(fmt.Sprintf("\nA cap of %d is set, so backward backfill is switched off entirely: each chat gets its newest %d messages at room creation and nothing older is ever requested. The older messages counted above are not pending — the bridge will not deliver them by design, and the cap cannot be changed by re-running setup once the database exists.\n", r.MessageCap, r.MessageCap))
	}

	if r.PendingMessages() > 0 {
		sb.WriteString("\n")
		if r.BatchSending != nil && !*r.BatchSending {
			sb.WriteString("This homeserver does not support Beeper batch sending, so bridgev2's own backfill queue never starts. The bridge drains the queue itself instead, delivering older messages as individual events. That drain waits for the initial sync and each chat's first batch to finish before it starts, so pending can sit still for a while and then move.\n")
		} else if r.BatchSending == nil {
			sb.WriteString("Note: bridgev2's backfill queue only runs on a homeserver that supports Beeper batch sending. Where it does not, the bridge drains the queue itself and delivers older messages as individual events — but only when no per-chat message cap is set. Run `sync-status` in the management room to see which case this is.\n")
		}
		if r.BackfillTasksPresent && r.BackfillTasksUndispatched > 0 {
			sb.WriteString(fmt.Sprintf("Backfill queue: %d of %d tasks done, %d never dispatched.\n",
				r.BackfillTasksDone, r.BackfillTasksTotal, r.BackfillTasksUndispatched))
		}
		sb.WriteString("Pending counts bridgeable messages that have not reached Matrix. If it holds steady with no sync running, the remainder is not recoverable from CloudKit — search the log for \"marking done with nothing delivered\".\n")
	} else if r.FullyCaughtUp() {
		sb.WriteString("\n✅ Backfill is complete — every bridgeable message has been delivered.\n")
	}

	sb.WriteString("\nReactions are not counted here: they bridge separately from messages.\n")
	return sb.String()
}

// cmdSyncStatus is the management-room half of the report. `corten-matrix
// sync-status` prints the same thing without a running bridge.
var cmdSyncStatus = &commands.FullHandler{
	Name:          "sync-status",
	Aliases:       []string{"syncstatus"},
	Func:          fnSyncStatus,
	RequiresLogin: true,
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Report whether backfill has finished, and what is holding it up if it hasn't: per-zone CloudKit state, how many messages have reached Matrix, and which rows will never be bridged.",
	},
}

func fnSyncStatus(ce *commands.Event) {
	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("Not logged in.")
		return
	}
	client, ok := login.Client.(*IMClient)
	if !ok || client == nil {
		ce.Reply("Bridge client not available.")
		return
	}

	opts := SyncStatusOptions{
		BridgeID:            string(ce.Bridge.ID),
		LoginID:             string(login.ID),
		MaxInitialMessages:  ce.Bridge.Config.Backfill.MaxInitialMessages,
		BridgeFilteredChats: client.Main.Config.BridgeFilteredChats,
	}
	// Both of these are only knowable from inside the running process, which
	// is the whole reason the report takes them as pointers.
	client.cloudSyncRunningLock.RLock()
	syncing := client.cloudSyncRunning
	client.cloudSyncRunningLock.RUnlock()
	opts.SyncRunning = &syncing
	batchSending := ce.Bridge.Matrix.GetCapabilities().BatchSending
	opts.BatchSending = &batchSending

	report, err := GetSyncStatus(ce.Ctx, ce.Bridge.DB.Database, opts)
	if err != nil {
		ce.Reply("Failed to read sync status: %v", err)
		return
	}
	ce.Reply("%s", report.Format())
}
