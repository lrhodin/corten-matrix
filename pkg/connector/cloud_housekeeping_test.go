package connector

import (
	"context"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"
)

func TestLegacySystemMessageCleanupIsTargetedAndOneShot(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM cloud_maintenance WHERE login_id=$1 AND task=$2`,
		testSQLLoginID, legacySystemMessageCleanupTask); err != nil {
		t.Fatalf("clear maintenance marker: %v", err)
	}

	now := time.Now().UnixMilli()
	if _, err := db.Exec(ctx, `
		INSERT INTO cloud_chat (login_id, cloud_chat_id, portal_id, display_name, created_ts)
		VALUES ($1, 'rename-chat', 'rename-portal', 'New Group Name', $2)
	`, testSQLLoginID, now); err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	for _, row := range []struct {
		guid, portal, text, attachments string
		hasBody                         bool
		tapback                         any
	}{
		{"no-body", "ordinary-portal", "", "", false, nil},
		{"rename", "rename-portal", "New Group Name", "", true, nil},
		{"real-message", "ordinary-portal", "hello", "", true, nil},
		{"attachment-system", "ordinary-portal", "", `[{"record_name":"keep"}]`, false, nil},
		{"reaction-system", "ordinary-portal", "", "", false, 2000},
	} {
		if _, err := db.Exec(ctx, `
			INSERT INTO cloud_message
				(login_id, guid, portal_id, timestamp_ms, is_from_me, text,
				 attachments_json, tapback_type, has_body, created_ts, updated_ts)
			VALUES ($1, $2, $3, $4, FALSE, $5, $6, $7, $8, $4, $4)
		`, testSQLLoginID, row.guid, row.portal, now, row.text, row.attachments, row.tapback, row.hasBody); err != nil {
			t.Fatalf("insert %s: %v", row.guid, err)
		}
	}

	if err := store.deleteLegacySystemMessages(ctx); err != nil {
		t.Fatalf("deleteLegacySystemMessages: %v", err)
	}
	for guid, want := range map[string]bool{
		"no-body": false, "rename": false, "real-message": true,
		"attachment-system": true, "reaction-system": true,
	} {
		var count int
		if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM cloud_message WHERE login_id=$1 AND guid=$2`,
			testSQLLoginID, guid).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", guid, err)
		}
		if got := count == 1; got != want {
			t.Errorf("row %s exists = %v, want %v", guid, got, want)
		}
	}

	if _, err := db.Exec(ctx, `
		INSERT INTO cloud_message
			(login_id, guid, timestamp_ms, is_from_me, has_body, created_ts, updated_ts)
		VALUES ($1, 'after-marker', $2, FALSE, FALSE, $2, $2)
	`, testSQLLoginID, now); err != nil {
		t.Fatalf("insert post-marker fixture: %v", err)
	}
	if err := store.deleteLegacySystemMessages(ctx); err != nil {
		t.Fatalf("second deleteLegacySystemMessages: %v", err)
	}
	var count int
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM cloud_message WHERE guid='after-marker'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Error("completed maintenance task ran a second time")
	}
}

func TestPruneOrphanedAttachmentCacheUsesLiveReferences(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	now := time.Now().UnixMilli()
	for _, row := range []struct {
		guid, recordName string
		deleted          bool
	}{
		{"live-message", "live-cache", false},
		{"deleted-message", "deleted-cache", true},
	} {
		attachments := `[{"guid":"attachment","record_name":"` + row.recordName + `","file_size":1}]`
		if _, err := db.Exec(ctx, `
			INSERT INTO cloud_message
				(login_id, guid, timestamp_ms, is_from_me, attachments_json, deleted, created_ts, updated_ts)
			VALUES ($1, $2, $3, FALSE, $4, $5, $3, $3)
		`, testSQLLoginID, row.guid, now, attachments, row.deleted); err != nil {
			t.Fatalf("insert message %s: %v", row.guid, err)
		}
	}
	for _, name := range []string{"live-cache", "deleted-cache", "unreferenced-cache"} {
		if _, err := db.Exec(ctx, `
			INSERT INTO cloud_attachment_cache (login_id, record_name, content_json, created_ts)
			VALUES ($1, $2, '{}', $3)
		`, testSQLLoginID, name, now); err != nil {
			t.Fatalf("insert cache %s: %v", name, err)
		}
	}
	if live, err := store.messageStillReferencesAttachment(ctx, "live-message", "live-cache"); err != nil || !live {
		t.Errorf("live attachment reference = %v, %v; want true", live, err)
	}
	if live, err := store.messageStillReferencesAttachment(ctx, "deleted-message", "deleted-cache"); err != nil || live {
		t.Errorf("deleted attachment reference = %v, %v; want false", live, err)
	}

	pruned, err := store.pruneOrphanedAttachmentCache(ctx)
	if err != nil {
		t.Fatalf("pruneOrphanedAttachmentCache: %v", err)
	}
	if pruned != 2 {
		t.Errorf("pruned = %d, want 2", pruned)
	}
	var remaining string
	if err := db.QueryRow(ctx, `SELECT record_name FROM cloud_attachment_cache`).Scan(&remaining); err != nil {
		t.Fatalf("read remaining cache: %v", err)
	}
	if remaining != "live-cache" {
		t.Errorf("remaining cache = %q, want live-cache", remaining)
	}
}

func TestDeleteOrphanedMessagesPreservesLiveAndKnownPortalRows(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := db.Exec(ctx, `
		INSERT INTO cloud_chat (login_id, cloud_chat_id, portal_id, created_ts)
		VALUES ($1, 'known-chat', 'known-portal', $2)
	`, testSQLLoginID, now); err != nil {
		t.Fatalf("insert known chat: %v", err)
	}
	for _, row := range []struct {
		guid    string
		portal  any
		age     time.Duration
		deleted bool
	}{
		{"known-deleted", "known-portal", 48 * time.Hour, true},
		{"orphan-deleted", "missing-portal", 48 * time.Hour, true},
		{"orphan-live", "missing-portal", 48 * time.Hour, false},
		{"old-null-stub", nil, 48 * time.Hour, true},
		{"recent-null-stub", nil, time.Hour, true},
	} {
		created := time.Now().Add(-row.age).UnixMilli()
		if _, err := db.Exec(ctx, `
			INSERT INTO cloud_message
				(login_id, guid, portal_id, timestamp_ms, is_from_me, deleted, created_ts, updated_ts)
			VALUES ($1, $2, $3, $4, FALSE, $5, $4, $6)
		`, testSQLLoginID, row.guid, row.portal, created, row.deleted, now); err != nil {
			t.Fatalf("insert %s: %v", row.guid, err)
		}
	}

	deleted, err := store.deleteOrphanedMessages(ctx)
	if err != nil {
		t.Fatalf("deleteOrphanedMessages: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}
	for guid, want := range map[string]bool{
		"known-deleted": true, "orphan-deleted": false, "orphan-live": true,
		"old-null-stub": false, "recent-null-stub": true,
	} {
		var count int
		if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM cloud_message WHERE guid=$1`, guid).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", guid, err)
		}
		if got := count == 1; got != want {
			t.Errorf("row %s exists = %v, want %v", guid, got, want)
		}
	}
}

func TestCachedAttachmentContentFallsBackToPersistedCache(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLiteDB(t)
	store := newCloudBackfillStore(db, testSQLLoginID)
	if err := store.ensureSchema(ctx); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	store.saveAttachmentCacheEntry(ctx, "image-record",
		[]byte(`{"msgtype":"m.image","body":"photo.png","url":"mxc://example/photo","info":{"mimetype":"image/png"}}`))
	store.saveAttachmentCacheEntry(ctx, "stale-video-record",
		[]byte(`{"msgtype":"m.video","body":"clip.mov","url":"mxc://example/clip","info":{"mimetype":"video/quicktime"}}`))

	c := &IMClient{cloudStore: store}

	// Nothing in memory yet: the persisted entry is the answer, and it is
	// kept in memory afterwards.
	content, err := c.cachedAttachmentContent(ctx, "image-record")
	if err != nil || content == nil || content.Body != "photo.png" {
		t.Fatalf("cachedAttachmentContent(image-record) = %+v, %v; want the persisted content", content, err)
	}
	if _, ok := c.attachmentContentCache.Load("image-record"); !ok {
		t.Fatal("persisted hit was not kept in memory")
	}
	if _, err := db.Exec(ctx, `DELETE FROM cloud_attachment_cache WHERE record_name='image-record'`); err != nil {
		t.Fatal(err)
	}
	if content, err := c.cachedAttachmentContent(ctx, "image-record"); err != nil || content == nil {
		t.Fatalf("second lookup = %+v, %v; want the in-memory copy", content, err)
	}

	// A video that still needs transcoding is a miss wherever it came from.
	if content, err := c.cachedAttachmentContent(ctx, "stale-video-record"); err != nil || content != nil {
		t.Fatalf("stale video lookup = %+v, %v; want a miss", content, err)
	}
	c.attachmentContentCache.Store("stale-video-record", &event.MessageEventContent{
		Info: &event.FileInfo{MimeType: "video/quicktime"},
	})
	if content, err := c.cachedAttachmentContent(ctx, "stale-video-record"); err != nil || content != nil {
		t.Fatalf("stale in-memory video lookup = %+v, %v; want a miss", content, err)
	}
	if _, ok := c.attachmentContentCache.Load("stale-video-record"); ok {
		t.Fatal("stale in-memory video entry was not dropped")
	}

	if content, err := c.cachedAttachmentContent(ctx, "unknown-record"); err != nil || content != nil {
		t.Fatalf("unknown lookup = %+v, %v; want a miss", content, err)
	}
}
