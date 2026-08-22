package connector

import (
	"context"
	"testing"
)

// TestReconcileStrandedBackfills covers the repair for the permanent-empty-room
// state: a backfill task marked done, CloudKit rows present, and not one
// bridged message to show for it.
//
// The three negative cases matter as much as the positive one. Re-arming a
// portal that is merely empty (no CloudKit rows) or one that backfilled fine
// (messages present) would put the bridge back into a backfill it already
// completed, and re-arming the same portal twice would make the repair fire on
// every startup forever for a portal whose CloudKit rows are all system
// records — real content zero, so still no bridged messages afterwards.
func TestReconcileStrandedBackfills(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}

	if _, err := db.Exec(ctx, `CREATE TABLE backfill_task (
		bridge_id            TEXT NOT NULL,
		portal_id            TEXT NOT NULL,
		portal_receiver      TEXT NOT NULL,
		user_login_id        TEXT NOT NULL,
		batch_count          INTEGER NOT NULL DEFAULT 0,
		is_done              BOOLEAN NOT NULL,
		queue_done           BOOLEAN NOT NULL DEFAULT false,
		completed_at         BIGINT,
		next_dispatch_min_ts BIGINT NOT NULL DEFAULT 0,
		PRIMARY KEY (bridge_id, portal_id, portal_receiver)
	)`); err != nil {
		t.Fatalf("create backfill_task: %v", err)
	}
	if _, err := db.Exec(ctx, `CREATE TABLE message (
		bridge_id     TEXT NOT NULL,
		id            TEXT NOT NULL,
		part_id       TEXT NOT NULL,
		room_id       TEXT NOT NULL,
		room_receiver TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create message: %v", err)
	}

	const bridgeID = "imessage"
	const receiver = string(testSQLLoginID)

	addTask := func(portalID string, isDone bool) {
		t.Helper()
		if _, err := db.Exec(ctx,
			`INSERT INTO backfill_task (bridge_id, portal_id, portal_receiver, user_login_id, is_done, queue_done, completed_at)
			 VALUES ($1, $2, $3, $4, $5, $5, 1000)`,
			bridgeID, portalID, receiver, string(testSQLLoginID), isDone,
		); err != nil {
			t.Fatalf("insert backfill_task %s: %v", portalID, err)
		}
	}
	addCloudMessage := func(portalID, guid string) {
		t.Helper()
		if _, err := db.Exec(ctx,
			`INSERT INTO cloud_message (login_id, record_name, guid, portal_id, sender, is_from_me, service, timestamp_ms, created_ts, updated_ts, deleted)
			 VALUES ($1, $2, $3, $4, 'tel:+15550001111', FALSE, 'iMessage', 1000, 1, 1, FALSE)`,
			string(testSQLLoginID), "rec-"+guid, guid, portalID,
		); err != nil {
			t.Fatalf("insert cloud_message %s: %v", guid, err)
		}
	}
	addBridgedMessage := func(portalID, guid string) {
		t.Helper()
		if _, err := db.Exec(ctx,
			`INSERT INTO message (bridge_id, id, part_id, room_id, room_receiver) VALUES ($1, $2, '', $3, $4)`,
			bridgeID, guid, portalID, receiver,
		); err != nil {
			t.Fatalf("insert message %s: %v", guid, err)
		}
	}
	isDone := func(portalID string) bool {
		t.Helper()
		var done bool
		if err := db.QueryRow(ctx,
			`SELECT is_done FROM backfill_task WHERE bridge_id=$1 AND portal_id=$2 AND portal_receiver=$3`,
			bridgeID, portalID, receiver,
		).Scan(&done); err != nil {
			t.Fatalf("read is_done for %s: %v", portalID, err)
		}
		return done
	}

	// stranded: marked done, CloudKit rows present, nothing bridged.
	addTask("tel:+15550001111", true)
	addCloudMessage("tel:+15550001111", "guid-stranded")

	// healthy: marked done and the messages actually landed.
	addTask("tel:+15550002222", true)
	addCloudMessage("tel:+15550002222", "guid-healthy")
	addBridgedMessage("tel:+15550002222", "guid-healthy")

	// genuinely empty: marked done, no CloudKit rows at all. Nothing to repair.
	addTask("tel:+15550003333", true)

	// still running: not done yet, so not this function's business.
	addTask("tel:+15550004444", false)
	addCloudMessage("tel:+15550004444", "guid-running")

	repaired, err := store.reconcileStrandedBackfills(ctx)
	if err != nil {
		t.Fatalf("reconcileStrandedBackfills: %v", err)
	}
	if repaired != 1 {
		t.Errorf("expected exactly 1 portal repaired, got %d", repaired)
	}
	if isDone("tel:+15550001111") {
		t.Error("stranded portal should have been re-armed (is_done cleared)")
	}
	if !isDone("tel:+15550002222") {
		t.Error("healthy portal must be left alone")
	}
	if !isDone("tel:+15550003333") {
		t.Error("portal with no CloudKit rows must be left alone")
	}

	// One shot. Put the task back in the done state exactly as a second failed
	// backfill would, and confirm the repair does not fire again — otherwise a
	// portal whose CloudKit rows never produce a bridged message re-arms on
	// every startup for good.
	if _, err := db.Exec(ctx,
		`UPDATE backfill_task SET is_done=true WHERE portal_id=$1`, "tel:+15550001111",
	); err != nil {
		t.Fatalf("re-mark done: %v", err)
	}
	repaired, err = store.reconcileStrandedBackfills(ctx)
	if err != nil {
		t.Fatalf("reconcileStrandedBackfills second pass: %v", err)
	}
	if repaired != 0 {
		t.Errorf("expected 0 repairs on the second pass, got %d", repaired)
	}
	if !isDone("tel:+15550001111") {
		t.Error("second pass must not re-arm a portal it has already repaired once")
	}
}
