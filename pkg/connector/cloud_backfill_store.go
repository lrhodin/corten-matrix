package connector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

type cloudBackfillStore struct {
	db      *dbutil.Database
	loginID networkid.UserLoginID
}

type cloudMessageRow struct {
	GUID        string
	RecordName  string
	CloudChatID string
	PortalID    string
	TimestampMS int64
	Sender      string
	IsFromMe    bool
	Text        string
	Subject     string
	Service     string
	Deleted     bool

	// Tapback/reaction fields
	TapbackType       *uint32
	TapbackTargetGUID string
	TapbackEmoji      string

	// Attachment metadata JSON (serialized []cloudAttachmentRow)
	AttachmentsJSON string

	// When the recipient read this message (Unix ms). Only for is_from_me messages.
	DateReadMS int64

	// Whether the CloudKit record has an attributedBody (rich text payload).
	// Regular user messages always have this; system messages (group renames,
	// participant changes) do not.
	HasBody bool

	// True once the body has been scrubbed because the message was bridged
	// to Matrix. Identifiers (guid, timestamps, record_name) are kept for
	// echo dedup and routing; text/subject/sender/attachments_json are NULL.
	BodyScrubbed bool
}

// cloudAttachmentRow holds CloudKit attachment metadata for a single attachment.
type cloudAttachmentRow struct {
	GUID           string `json:"guid"`
	MimeType       string `json:"mime_type,omitempty"`
	UTIType        string `json:"uti_type,omitempty"`
	Filename       string `json:"filename,omitempty"`
	FileSize       int64  `json:"file_size"`
	RecordName     string `json:"record_name"`
	HideAttachment bool   `json:"hide_attachment,omitempty"`
	HasAvid        bool   `json:"has_avid,omitempty"`
}

const (
	cloudZoneChats       = "chatManateeZone"
	cloudZoneMessages    = "messageManateeZone"
	cloudZoneAttachments = "attachmentManateeZone"
)

func newCloudBackfillStore(db *dbutil.Database, loginID networkid.UserLoginID) *cloudBackfillStore {
	return &cloudBackfillStore{db: db, loginID: loginID}
}

func (s *cloudBackfillStore) ensureSchema(ctx context.Context) error {
	// Privacy: secure_delete forces SQLite to zero out freed pages during
	// DELETE/UPDATE so NULLing text/subject/sender in scrubBridgedBodies does
	// not leave the original strings recoverable by file inspection until
	// VACUUM (which we don't run automatically because it rewrites the entire
	// DB and would block writes for the duration on a large backlog).
	// secure_delete is a PER-CONNECTION setting that is NOT persisted in the
	// DB file, so it is set via the _secure_delete DSN param in
	// ensureSecureDeleteDSN (cmd/mautrix-imessage/main.go) — that applies it
	// on every pooled connection, which a one-shot PRAGMA here could not do
	// (the pool hands scrub writes to arbitrary connections). Self-hosters who
	// want immediate page reclamation can run `sqlite3 bridge.db "VACUUM;"`
	// manually after first-boot scrub.

	queries := []string{
		`CREATE TABLE IF NOT EXISTS cloud_sync_state (
			login_id TEXT NOT NULL,
			zone TEXT NOT NULL,
			continuation_token TEXT,
			last_success_ts BIGINT,
			last_error TEXT,
			updated_ts BIGINT NOT NULL,
			PRIMARY KEY (login_id, zone)
		)`,
		`CREATE TABLE IF NOT EXISTS cloud_chat (
			login_id TEXT NOT NULL,
			cloud_chat_id TEXT NOT NULL,
			record_name TEXT NOT NULL DEFAULT '',
			group_id TEXT NOT NULL DEFAULT '',
			portal_id TEXT NOT NULL,
			service TEXT,
			display_name TEXT,
			group_photo_guid TEXT,
			participants_json TEXT,
			updated_ts BIGINT,
			created_ts BIGINT NOT NULL,
			PRIMARY KEY (login_id, cloud_chat_id)
		)`,
		`CREATE TABLE IF NOT EXISTS cloud_message (
			login_id TEXT NOT NULL,
			guid TEXT NOT NULL,
			chat_id TEXT,
			portal_id TEXT,
			timestamp_ms BIGINT NOT NULL,
			sender TEXT,
			is_from_me BOOLEAN NOT NULL,
			text TEXT,
			subject TEXT,
			service TEXT,
			deleted BOOLEAN NOT NULL DEFAULT FALSE,
			tapback_type INTEGER,
			tapback_target_guid TEXT,
			tapback_emoji TEXT,
			attachments_json TEXT,
			created_ts BIGINT NOT NULL,
			updated_ts BIGINT NOT NULL,
			PRIMARY KEY (login_id, guid)
		)`,
		`CREATE TABLE IF NOT EXISTS group_photo_cache (
			login_id TEXT NOT NULL,
			portal_id TEXT NOT NULL,
			ts BIGINT NOT NULL,
			data BLOB NOT NULL,
			PRIMARY KEY (login_id, portal_id)
		)`,
		`CREATE TABLE IF NOT EXISTS restore_override (
			login_id TEXT NOT NULL,
			portal_id TEXT NOT NULL,
			updated_ts BIGINT NOT NULL,
			PRIMARY KEY (login_id, portal_id)
		)`,
		`CREATE INDEX IF NOT EXISTS cloud_chat_portal_idx
			ON cloud_chat (login_id, portal_id, cloud_chat_id)`,
		`CREATE INDEX IF NOT EXISTS cloud_message_portal_ts_idx
			ON cloud_message (login_id, portal_id, timestamp_ms, guid)`,
		`CREATE INDEX IF NOT EXISTS cloud_message_chat_ts_idx
			ON cloud_message (login_id, chat_id, timestamp_ms, guid)`,
	}

	// Run table creation queries first (without indexes that depend on migrations)
	for _, query := range queries {
		if _, err := s.db.Exec(ctx, query); err != nil {
			return fmt.Errorf("failed to ensure cloud backfill schema: %w", err)
		}
	}

	// Migrations: add missing columns to cloud_chat (SQLite doesn't support IF NOT EXISTS on ALTER).
	// fwd_backfill_done: set to 1 when FetchMessages(forward) completes for a portal so that
	// preUploadCloudAttachments skips those portals on restart. Default 0 means "not yet done".
	// deleted: soft-deletes cloud_chat rows alongside cloud_message rows so restore-chat can
	// recover group name and participants.
	for _, col := range []struct{ name, def string }{
		{"record_name", "TEXT NOT NULL DEFAULT ''"},
		{"group_id", "TEXT NOT NULL DEFAULT ''"},
		{"group_photo_guid", "TEXT"},
		{"deleted", "BOOLEAN NOT NULL DEFAULT FALSE"},
		{"is_filtered", "INTEGER NOT NULL DEFAULT 0"},
		{"fwd_backfill_done", "BOOLEAN NOT NULL DEFAULT 0"},
	} {
		var exists int
		_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM pragma_table_info('cloud_chat') WHERE name=$1`, col.name).Scan(&exists)
		if exists == 0 {
			if _, err := s.db.Exec(ctx, fmt.Sprintf(`ALTER TABLE cloud_chat ADD COLUMN %s %s`, col.name, col.def)); err != nil {
				return fmt.Errorf("failed to add %s column to cloud_chat: %w", col.name, err)
			}
		}
	}

	// Migrations: add missing columns to cloud_message.
	for _, col := range []struct{ name, def string }{
		{"subject", "TEXT"},
		{"tapback_type", "INTEGER"},
		{"tapback_target_guid", "TEXT"},
		{"tapback_emoji", "TEXT"},
		{"attachments_json", "TEXT"},
		{"date_read_ms", "BIGINT NOT NULL DEFAULT 0"},
		{"record_name", "TEXT NOT NULL DEFAULT ''"},
		{"has_body", "BOOLEAN NOT NULL DEFAULT TRUE"},
		// Privacy: TRUE once the body has been scrubbed because the message
		// was bridged to Matrix. Set by scrubBridgedBodies and preserved
		// across CloudKit re-syncs by the upsert ON CONFLICT clause.
		{"body_scrubbed", "BOOLEAN NOT NULL DEFAULT FALSE"},
	} {
		var exists int
		_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM pragma_table_info('cloud_message') WHERE name=$1`, col.name).Scan(&exists)
		if exists == 0 {
			if _, err := s.db.Exec(ctx, fmt.Sprintf(`ALTER TABLE cloud_message ADD COLUMN %s %s`, col.name, col.def)); err != nil {
				return fmt.Errorf("failed to add %s column to cloud_message: %w", col.name, err)
			}
		}
	}

	// Privacy migration: pre-existing soft-deleted rows from before the
	// privacy branch never went through softDeleteMessageByGUID's inline
	// scrub. They sit with deleted=TRUE and original text/subject/sender,
	// and the periodic scrubber can't catch them because bridgev2 likely
	// purged its message row long ago (so the EXISTS predicate fails).
	// One-shot UPDATE on startup to scrub them in place. Idempotent: only
	// matches rows still holding plaintext that haven't been scrubbed.
	if _, err := s.db.Exec(ctx, `
		UPDATE cloud_message
		SET text=NULL, subject=NULL, sender='',
		    tapback_emoji=NULL,
		    body_scrubbed=TRUE
		WHERE login_id=$1
		  AND deleted=TRUE
		  AND body_scrubbed=FALSE
	`, s.loginID); err != nil {
		return fmt.Errorf("failed to scrub pre-existing soft-deleted rows: %w", err)
	}

	// Cleanup: permanently delete system/rename message rows that slipped into
	// the DB before the MsgType==0 ingest filter was added.  Two conditions
	// catch different eras of the DB:
	//   1. has_body=FALSE + no attachments + no tapback — rows that were stored
	//      after the has_body column was added but before the MsgType filter;
	//      their has_body is correctly FALSE (no attributedBody).
	//   2. text matches the portal's display_name + no attachments + no tapback
	//      — older rows whose has_body defaulted to TRUE but whose content
	//      reveals them as group-rename notifications.
	// This runs every startup and is idempotent: after the first pass it
	// matches zero rows.
	if _, err := s.db.Exec(ctx, `
		DELETE FROM cloud_message
		WHERE login_id = $1
		  AND COALESCE(attachments_json, '') = ''
		  AND tapback_type IS NULL
		  AND (
		    has_body = FALSE
		    OR (
		      text IS NOT NULL AND text <> ''
		      AND portal_id IN (
		        SELECT portal_id FROM cloud_chat c
		        WHERE c.login_id = $1
		          AND c.display_name IS NOT NULL AND c.display_name <> ''
		          AND c.display_name = cloud_message.text
		      )
		    )
		  )
	`, s.loginID); err != nil {
		return fmt.Errorf("failed to delete system messages: %w", err)
	}

	// Migration: add cloud_attachment_cache table if missing.
	// Persists record_name → MessageEventContent JSON so mxc URIs survive
	// bridge restarts. Pre-upload loads this at startup and skips already-cached
	// attachments, so a 27k-message thread never re-downloads across restarts.
	if _, err := s.db.Exec(ctx, `CREATE TABLE IF NOT EXISTS cloud_attachment_cache (
		login_id    TEXT    NOT NULL,
		record_name TEXT    NOT NULL,
		content_json BLOB   NOT NULL,
		created_ts  BIGINT  NOT NULL,
		PRIMARY KEY (login_id, record_name)
	)`); err != nil {
		return fmt.Errorf("failed to create cloud_attachment_cache table: %w", err)
	}

	// Create index that depends on record_name column (must be after migration)
	if _, err := s.db.Exec(ctx, `CREATE INDEX IF NOT EXISTS cloud_chat_record_name_idx
		ON cloud_chat (login_id, record_name) WHERE record_name <> ''`); err != nil {
		return fmt.Errorf("failed to create record_name index: %w", err)
	}

	// Create index for group_id lookups (messages reference chats by group_id UUID)
	if _, err := s.db.Exec(ctx, `CREATE INDEX IF NOT EXISTS cloud_chat_group_id_idx
		ON cloud_chat (login_id, group_id) WHERE group_id <> ''`); err != nil {
		return fmt.Errorf("failed to create group_id index: %w", err)
	}

	return nil
}

func (s *cloudBackfillStore) getSyncState(ctx context.Context, zone string) (*string, error) {
	var token sql.NullString
	err := s.db.QueryRow(ctx,
		`SELECT continuation_token FROM cloud_sync_state WHERE login_id=$1 AND zone=$2`,
		s.loginID, zone,
	).Scan(&token)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if !token.Valid {
		return nil, nil
	}
	return &token.String, nil
}

func (s *cloudBackfillStore) setSyncStateSuccess(ctx context.Context, zone string, token *string) error {
	nowMS := time.Now().UnixMilli()
	_, err := s.db.Exec(ctx, `
		INSERT INTO cloud_sync_state (login_id, zone, continuation_token, last_success_ts, last_error, updated_ts)
		VALUES ($1, $2, $3, $4, NULL, $5)
		ON CONFLICT (login_id, zone) DO UPDATE SET
			continuation_token=excluded.continuation_token,
			last_success_ts=excluded.last_success_ts,
			last_error=NULL,
			updated_ts=excluded.updated_ts
	`, s.loginID, zone, nullableString(token), nowMS, nowMS)
	return err
}

// clearSyncTokens removes only the sync continuation tokens for this login,
// forcing the next sync to re-download all records from CloudKit.
// Preserves cloud_chat, cloud_message, and the _version row.
func (s *cloudBackfillStore) clearSyncTokens(ctx context.Context) error {
	_, err := s.db.Exec(ctx,
		`DELETE FROM cloud_sync_state WHERE login_id=$1 AND zone != '_version'`,
		s.loginID,
	)
	return err
}

// clearZoneToken removes the continuation token for a specific zone,
// forcing the next sync for that zone to start from scratch.
func (s *cloudBackfillStore) clearZoneToken(ctx context.Context, zone string) error {
	_, err := s.db.Exec(ctx,
		`DELETE FROM cloud_sync_state WHERE login_id=$1 AND zone=$2`,
		s.loginID, zone,
	)
	return err
}

// getSyncVersion returns the stored sync schema version (0 if never set).
func (s *cloudBackfillStore) getSyncVersion(ctx context.Context) (int, error) {
	var token sql.NullString
	err := s.db.QueryRow(ctx,
		`SELECT continuation_token FROM cloud_sync_state WHERE login_id=$1 AND zone='_version'`,
		s.loginID,
	).Scan(&token)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	if !token.Valid {
		return 0, nil
	}
	v := 0
	fmt.Sscanf(token.String, "%d", &v)
	return v, nil
}

// setSyncVersion stores the sync schema version.
func (s *cloudBackfillStore) setSyncVersion(ctx context.Context, version int) error {
	nowMS := time.Now().UnixMilli()
	vStr := fmt.Sprintf("%d", version)
	_, err := s.db.Exec(ctx, `
		INSERT INTO cloud_sync_state (login_id, zone, continuation_token, updated_ts)
		VALUES ($1, '_version', $2, $3)
		ON CONFLICT (login_id, zone) DO UPDATE SET
			continuation_token=excluded.continuation_token,
			updated_ts=excluded.updated_ts
	`, s.loginID, vStr, nowMS)
	return err
}

// getChatSyncVersion returns the stored chat-specific sync version (0 if never set).
// This tracks chat-zone-only re-syncs independently of the full cloudSyncVersion,
// so we can force a targeted chat re-fetch (e.g. to populate group_photo_guid)
// without also re-downloading all messages.
func (s *cloudBackfillStore) getChatSyncVersion(ctx context.Context) (int, error) {
	var token sql.NullString
	err := s.db.QueryRow(ctx,
		`SELECT continuation_token FROM cloud_sync_state WHERE login_id=$1 AND zone='_chat_version'`,
		s.loginID,
	).Scan(&token)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	if !token.Valid {
		return 0, nil
	}
	v := 0
	fmt.Sscanf(token.String, "%d", &v)
	return v, nil
}

// setChatSyncVersion stores the chat-specific sync schema version.
func (s *cloudBackfillStore) setChatSyncVersion(ctx context.Context, version int) error {
	nowMS := time.Now().UnixMilli()
	vStr := fmt.Sprintf("%d", version)
	_, err := s.db.Exec(ctx, `
		INSERT INTO cloud_sync_state (login_id, zone, continuation_token, updated_ts)
		VALUES ($1, '_chat_version', $2, $3)
		ON CONFLICT (login_id, zone) DO UPDATE SET
			continuation_token=excluded.continuation_token,
			updated_ts=excluded.updated_ts
	`, s.loginID, vStr, nowMS)
	return err
}

// clearAllData removes cloud cache data for this login: sync tokens,
// cached chats, and cached messages. Used on fresh bootstrap when the bridge
// DB was reset but the cloud tables survived.
func (s *cloudBackfillStore) clearAllData(ctx context.Context) error {
	for _, table := range []string{"cloud_sync_state", "cloud_chat", "cloud_message", "cloud_attachment_cache"} {
		if _, err := s.db.Exec(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE login_id=$1`, table),
			s.loginID,
		); err != nil {
			return fmt.Errorf("failed to clear %s: %w", table, err)
		}
	}
	return nil
}

// hasAnySyncState checks whether any sync state rows exist for this login.
// Used to detect an interrupted sync — if tokens exist but no portals were
// created yet, the sync was interrupted mid-flight and should resume, NOT restart.
func (s *cloudBackfillStore) hasAnySyncState(ctx context.Context) (bool, error) {
	var count int
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM cloud_sync_state WHERE login_id=$1`,
		s.loginID,
	).Scan(&count)
	return count > 0, err
}

func (s *cloudBackfillStore) setSyncStateError(ctx context.Context, zone, errMsg string) error {
	nowMS := time.Now().UnixMilli()
	_, err := s.db.Exec(ctx, `
		INSERT INTO cloud_sync_state (login_id, zone, continuation_token, last_error, updated_ts)
		VALUES ($1, $2, NULL, $3, $4)
		ON CONFLICT (login_id, zone) DO UPDATE SET
			last_error=excluded.last_error,
			updated_ts=excluded.updated_ts
	`, s.loginID, zone, errMsg, nowMS)
	return err
}

func (s *cloudBackfillStore) upsertChat(
	ctx context.Context,
	cloudChatID, recordName, groupID, portalID, service string,
	displayName, groupPhotoGuid *string,
	participants []string,
	updatedTS int64,
) error {
	participantsJSON, err := json.Marshal(participants)
	if err != nil {
		return err
	}
	nowMS := time.Now().UnixMilli()
	_, err = s.db.Exec(ctx, `
		INSERT INTO cloud_chat (
			login_id, cloud_chat_id, record_name, group_id, portal_id, service, display_name,
			group_photo_guid, participants_json, updated_ts, created_ts
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (login_id, cloud_chat_id) DO UPDATE SET
			record_name=excluded.record_name,
			group_id=excluded.group_id,
			portal_id=excluded.portal_id,
			service=excluded.service,
			display_name=CASE
				WHEN excluded.updated_ts >= COALESCE(cloud_chat.updated_ts, 0)
				THEN excluded.display_name
				ELSE cloud_chat.display_name
			END,
			group_photo_guid=CASE
				WHEN excluded.updated_ts >= COALESCE(cloud_chat.updated_ts, 0)
				THEN excluded.group_photo_guid
				ELSE cloud_chat.group_photo_guid
			END,
			participants_json=CASE
				WHEN COALESCE(cloud_chat.participants_json, '') IN ('', '[]')
				THEN excluded.participants_json
				ELSE cloud_chat.participants_json
			END,
			updated_ts=CASE
				WHEN excluded.updated_ts >= COALESCE(cloud_chat.updated_ts, 0)
				THEN excluded.updated_ts
				ELSE cloud_chat.updated_ts
			END
	`, s.loginID, cloudChatID, recordName, groupID, portalID, service, nullableString(displayName), nullableString(groupPhotoGuid), string(participantsJSON), updatedTS, nowMS)
	return err
}

// beginTx starts a database transaction for batch operations.
func (s *cloudBackfillStore) beginTx(ctx context.Context) (*sql.Tx, error) {
	return s.db.RawDB.BeginTx(ctx, nil)
}

// upsertMessageBatch inserts multiple messages in a single transaction.
func (s *cloudBackfillStore) upsertMessageBatch(ctx context.Context, rows []cloudMessageRow) error {
	if len(rows) == 0 {
		return nil
	}
	// CloudKit can occasionally return duplicate GUID entries within a fetch
	// window; keep only the newest row per GUID to make batch inserts idempotent.
	rowsByGUID := make(map[string]cloudMessageRow, len(rows))
	order := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.GUID == "" {
			continue
		}
		if prev, ok := rowsByGUID[row.GUID]; !ok {
			rowsByGUID[row.GUID] = row
			order = append(order, row.GUID)
		} else if row.TimestampMS >= prev.TimestampMS {
			rowsByGUID[row.GUID] = row
		}
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO cloud_message (
			login_id, guid, record_name, chat_id, portal_id, timestamp_ms,
			sender, is_from_me, text, subject, service, deleted,
			tapback_type, tapback_target_guid, tapback_emoji,
			attachments_json, date_read_ms, has_body,
			created_ts, updated_ts
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (login_id, guid) DO UPDATE SET
			record_name=excluded.record_name,
			chat_id=excluded.chat_id,
			portal_id=excluded.portal_id,
			timestamp_ms=excluded.timestamp_ms,
			sender=CASE WHEN cloud_message.body_scrubbed THEN cloud_message.sender ELSE excluded.sender END,
			is_from_me=excluded.is_from_me,
			text=CASE WHEN cloud_message.body_scrubbed THEN cloud_message.text ELSE excluded.text END,
			subject=CASE WHEN cloud_message.body_scrubbed THEN cloud_message.subject ELSE excluded.subject END,
			service=excluded.service,
			deleted=CASE WHEN cloud_message.deleted THEN cloud_message.deleted ELSE excluded.deleted END,
			tapback_type=excluded.tapback_type,
			tapback_target_guid=excluded.tapback_target_guid,
			tapback_emoji=CASE WHEN cloud_message.body_scrubbed THEN cloud_message.tapback_emoji ELSE excluded.tapback_emoji END,
			attachments_json=excluded.attachments_json,
			date_read_ms=CASE WHEN excluded.date_read_ms > cloud_message.date_read_ms THEN excluded.date_read_ms ELSE cloud_message.date_read_ms END,
			has_body=excluded.has_body,
			updated_ts=excluded.updated_ts
	`)
	if err != nil {
		return fmt.Errorf("failed to prepare batch statement: %w", err)
	}
	defer stmt.Close()

	nowMS := time.Now().UnixMilli()
	for _, guid := range order {
		row := rowsByGUID[guid]
		_, err = stmt.ExecContext(ctx,
			s.loginID, row.GUID, row.RecordName, row.CloudChatID, row.PortalID, row.TimestampMS,
			row.Sender, row.IsFromMe, row.Text, row.Subject, row.Service, row.Deleted,
			row.TapbackType, row.TapbackTargetGUID, row.TapbackEmoji,
			row.AttachmentsJSON, row.DateReadMS, row.HasBody,
			nowMS, nowMS,
		)
		if err != nil {
			if isUniqueConstraintErr(err) {
				continue
			}
			return fmt.Errorf("failed to insert message %s: %w", row.GUID, err)
		}
	}

	return tx.Commit()
}

func isUniqueConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	errText := strings.ToLower(err.Error())
	return strings.Contains(errText, "unique constraint") || strings.Contains(errText, "primary key")
}

// deleteMessageBatch soft-deletes individual messages by GUID (sets deleted=TRUE).
// This is for messages that CloudKit itself marks as deleted (individual message
// deletions), NOT for portal-level deletion (see deleteLocalChatByPortalID).
//
// Soft-delete is used here because:
//  1. Echo detection: hasMessageUUID checks cloud_message without filtering
//     by deleted. Keeping the row means re-delivered APNs echoes of the same
//     UUID are still recognised as known and suppressed.
//  2. Re-sync safety: a full CloudKit re-sync would re-import the message as
//     live. With a hard delete, the upsert inserts it with deleted=FALSE.
//     With a soft delete, the upsert conflict resolution preserves deleted=TRUE.
//
// The upsert conflict resolution (`deleted=CASE WHEN cloud_message.deleted
// THEN cloud_message.deleted ELSE excluded.deleted END`) ensures that a live
// re-delivery of the same GUID can never flip a soft-deleted row back to live.
func (s *cloudBackfillStore) deleteMessageBatch(ctx context.Context, guids []string) error {
	if len(guids) == 0 {
		return nil
	}
	nowMS := time.Now().UnixMilli()
	const chunkSize = 500
	for i := 0; i < len(guids); i += chunkSize {
		end := i + chunkSize
		if end > len(guids) {
			end = len(guids)
		}
		chunk := guids[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+2)
		args = append(args, s.loginID, nowMS)
		for j, g := range chunk {
			placeholders[j] = fmt.Sprintf("$%d", j+3)
			args = append(args, g)
		}

		query := fmt.Sprintf(
			`UPDATE cloud_message
			 SET deleted=TRUE,
			     text=NULL, subject=NULL, sender='',
			     tapback_emoji=NULL,
			     body_scrubbed=TRUE,
			     updated_ts=$2
			 WHERE login_id=$1 AND guid IN (%s) AND deleted=FALSE`,
			strings.Join(placeholders, ","),
		)
		if _, err := s.db.Exec(ctx, query, args...); err != nil {
			return fmt.Errorf("failed to soft-delete message batch: %w", err)
		}
	}
	return nil
}

// deleteChatBatch removes chats by cloud_chat_id in a single transaction.
func (s *cloudBackfillStore) deleteChatBatch(ctx context.Context, chatIDs []string) error {
	if len(chatIDs) == 0 {
		return nil
	}
	const chunkSize = 500
	for i := 0; i < len(chatIDs); i += chunkSize {
		end := i + chunkSize
		if end > len(chatIDs) {
			end = len(chatIDs)
		}
		chunk := chatIDs[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, s.loginID)
		for j, id := range chunk {
			placeholders[j] = fmt.Sprintf("$%d", j+2)
			args = append(args, id)
		}

		query := fmt.Sprintf(
			`DELETE FROM cloud_chat WHERE login_id=$1 AND cloud_chat_id IN (%s)`,
			strings.Join(placeholders, ","),
		)
		if _, err := s.db.Exec(ctx, query, args...); err != nil {
			return fmt.Errorf("failed to delete chat batch: %w", err)
		}
	}
	return nil
}

// lookupPortalIDsByRecordNames finds portal_ids for cloud_chat records matching
// the given CloudKit record_names. Used to resolve tombstoned (deleted) chats
// whose only identifier is the record_name.
func (s *cloudBackfillStore) lookupPortalIDsByRecordNames(ctx context.Context, recordNames []string) (map[string]string, error) {
	result := make(map[string]string, len(recordNames))
	if len(recordNames) == 0 {
		return result, nil
	}
	const chunkSize = 500
	for i := 0; i < len(recordNames); i += chunkSize {
		end := i + chunkSize
		if end > len(recordNames) {
			end = len(recordNames)
		}
		chunk := recordNames[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, s.loginID)
		for j, rn := range chunk {
			placeholders[j] = fmt.Sprintf("$%d", j+2)
			args = append(args, rn)
		}

		query := fmt.Sprintf(
			`SELECT record_name, portal_id FROM cloud_chat WHERE login_id=$1 AND record_name IN (%s)`,
			strings.Join(placeholders, ","),
		)
		rows, err := s.db.Query(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("failed to lookup portal IDs by record names: %w", err)
		}
		for rows.Next() {
			var rn, pid string
			if err := rows.Scan(&rn, &pid); err != nil {
				rows.Close()
				return nil, err
			}
			result[rn] = pid
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// deleteChatsByRecordNames removes cloud_chat entries by their CloudKit
// record_name (not cloud_chat_id). Needed for tombstoned records where the
// only identifier is the record_name.
func (s *cloudBackfillStore) deleteChatsByRecordNames(ctx context.Context, recordNames []string) error {
	if len(recordNames) == 0 {
		return nil
	}
	const chunkSize = 500
	for i := 0; i < len(recordNames); i += chunkSize {
		end := i + chunkSize
		if end > len(recordNames) {
			end = len(recordNames)
		}
		chunk := recordNames[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, s.loginID)
		for j, rn := range chunk {
			placeholders[j] = fmt.Sprintf("$%d", j+2)
			args = append(args, rn)
		}

		// Bump updated_ts so tail-timestamp gating uses the deletion time,
		// not the last CloudKit sync time.
		nowMS := time.Now().UnixMilli()
		args = append(args, nowMS)
		tsPlaceholder := fmt.Sprintf("$%d", len(args))
		query := fmt.Sprintf(
			`UPDATE cloud_chat SET deleted=TRUE, updated_ts=%s WHERE login_id=$1 AND record_name IN (%s) AND deleted=FALSE`,
			tsPlaceholder, strings.Join(placeholders, ","),
		)
		if _, err := s.db.Exec(ctx, query, args...); err != nil {
			return fmt.Errorf("failed to soft-delete chats by record name: %w", err)
		}
	}
	return nil
}

// deleteMessagesByChatIDs removes messages whose chat_id matches any of the
// given cloud_chat_ids. This prevents orphaned messages from keeping portals
// alive after their parent chat is deleted from CloudKit.
func (s *cloudBackfillStore) deleteMessagesByChatIDs(ctx context.Context, chatIDs []string) error {
	if len(chatIDs) == 0 {
		return nil
	}
	const chunkSize = 500
	for i := 0; i < len(chatIDs); i += chunkSize {
		end := i + chunkSize
		if end > len(chatIDs) {
			end = len(chatIDs)
		}
		chunk := chatIDs[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, s.loginID)
		for j, id := range chunk {
			placeholders[j] = fmt.Sprintf("$%d", j+2)
			args = append(args, id)
		}

		query := fmt.Sprintf(
			`DELETE FROM cloud_message WHERE login_id=$1 AND chat_id IN (%s)`,
			strings.Join(placeholders, ","),
		)
		if _, err := s.db.Exec(ctx, query, args...); err != nil {
			return fmt.Errorf("failed to delete messages by chat ID: %w", err)
		}
	}
	return nil
}

// upsertChatBatch inserts multiple chats in a single transaction.
func (s *cloudBackfillStore) upsertChatBatch(ctx context.Context, chats []cloudChatUpsertRow) error {
	if len(chats) == 0 {
		return nil
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO cloud_chat (
			login_id, cloud_chat_id, record_name, group_id, portal_id, service, display_name,
			group_photo_guid, participants_json, updated_ts, created_ts, is_filtered
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (login_id, cloud_chat_id) DO UPDATE SET
			record_name=excluded.record_name,
			group_id=excluded.group_id,
			portal_id=excluded.portal_id,
			service=excluded.service,
			display_name=CASE
				WHEN excluded.updated_ts >= COALESCE(cloud_chat.updated_ts, 0)
				THEN excluded.display_name
				ELSE cloud_chat.display_name
			END,
			group_photo_guid=CASE
				WHEN excluded.updated_ts >= COALESCE(cloud_chat.updated_ts, 0)
				THEN excluded.group_photo_guid
				ELSE cloud_chat.group_photo_guid
			END,
			participants_json=CASE
				WHEN COALESCE(cloud_chat.participants_json, '') IN ('', '[]')
				THEN excluded.participants_json
				ELSE cloud_chat.participants_json
			END,
			updated_ts=CASE
				WHEN excluded.updated_ts >= COALESCE(cloud_chat.updated_ts, 0)
				THEN excluded.updated_ts
				ELSE cloud_chat.updated_ts
			END,
			is_filtered=excluded.is_filtered,
			deleted=cloud_chat.deleted
	`)
	if err != nil {
		return fmt.Errorf("failed to prepare batch statement: %w", err)
	}
	defer stmt.Close()

	nowMS := time.Now().UnixMilli()
	for _, chat := range chats {
		_, err = stmt.ExecContext(ctx,
			s.loginID, chat.CloudChatID, chat.RecordName, chat.GroupID,
			chat.PortalID, chat.Service, chat.DisplayName,
			chat.GroupPhotoGuid, chat.ParticipantsJSON, chat.UpdatedTS, nowMS, chat.IsFiltered,
		)
		if err != nil {
			return fmt.Errorf("failed to insert chat %s: %w", chat.CloudChatID, err)
		}
	}

	return tx.Commit()
}

// hasMessageBatch checks existence of multiple GUIDs in a single query and
// returns the set of GUIDs that already exist.
func (s *cloudBackfillStore) hasMessageBatch(ctx context.Context, guids []string) (map[string]bool, error) {
	if len(guids) == 0 {
		return nil, nil
	}
	existing := make(map[string]bool, len(guids))
	// SQLite has a limit on the number of variables. Process in chunks.
	const chunkSize = 500
	for i := 0; i < len(guids); i += chunkSize {
		end := i + chunkSize
		if end > len(guids) {
			end = len(guids)
		}
		chunk := guids[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, s.loginID)
		for j, g := range chunk {
			placeholders[j] = fmt.Sprintf("$%d", j+2)
			args = append(args, g)
		}

		query := fmt.Sprintf(
			`SELECT guid FROM cloud_message WHERE login_id=$1 AND guid IN (%s)`,
			strings.Join(placeholders, ","),
		)
		rows, err := s.db.Query(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var guid string
			if err := rows.Scan(&guid); err != nil {
				rows.Close()
				return nil, err
			}
			existing[guid] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return existing, nil
}

// cloudChatUpsertRow holds the pre-serialized data for a batch chat upsert.
type cloudChatUpsertRow struct {
	CloudChatID      string
	RecordName       string
	GroupID          string
	PortalID         string
	Service          string
	DisplayName      any // nil or string
	GroupPhotoGuid   any // nil or string
	ParticipantsJSON string
	UpdatedTS        int64
	IsFiltered       int64
}

type recycleBinPortalState struct {
	PortalID           string
	Total              int
	Recoverable        int
	RecoverableSuffix  int
	NewestTS           int64
	HasLiveMessages    bool
	HasDeletedMessages bool
}

// Chat-level deletes move a short tail of the newest messages to Apple's
// recycle bin. Requiring at least a few consecutive newest messages helps
// distinguish a deleted chat from single-message deletes or stale recycle-bin
// entries from a chat that has since been restored.
func recycleBinDeleteThreshold(total int) int {
	if total <= 0 {
		return 0
	}
	if total < 3 {
		return total
	}
	return 3
}

func (s recycleBinPortalState) LooksDeleted() bool {
	threshold := recycleBinDeleteThreshold(s.Total)
	return threshold > 0 && s.RecoverableSuffix >= threshold
}

func (s recycleBinPortalState) LooksRestored() bool {
	return s.Recoverable > 0 && !s.LooksDeleted()
}

func (s recycleBinPortalState) NeedsUndelete() bool {
	return s.LooksRestored() && s.HasDeletedMessages && !s.HasLiveMessages
}

func normalizeRecoverableGUIDSet(recoverableGUIDs []string) map[string]bool {
	if len(recoverableGUIDs) == 0 {
		return nil
	}

	// Build a set for O(1) lookup.
	// Handle both plain GUIDs and pipe-delimited "guid|...|..." format
	// (in case the Rust library returns structured entries).
	guidSet := make(map[string]bool, len(recoverableGUIDs))
	for _, g := range recoverableGUIDs {
		if idx := strings.IndexByte(g, '|'); idx >= 0 {
			g = g[:idx]
		}
		if g != "" {
			guidSet[g] = true
		}
	}
	return guidSet
}

// classifyRecycleBinPortals matches recoverable message GUIDs from Apple's
// recycle-bin zones against local cloud_message rows and classifies each portal
// by how those GUIDs overlap with the newest messages in the conversation.
func (s *cloudBackfillStore) classifyRecycleBinPortals(ctx context.Context, recoverableGUIDs []string) ([]recycleBinPortalState, error) {
	guidSet := normalizeRecoverableGUIDSet(recoverableGUIDs)
	if len(guidSet) == 0 {
		return nil, nil
	}

	// Log a few sample GUIDs from each side for format comparison diagnostics.
	log := zerolog.Ctx(ctx)
	sampleRecycleBin := make([]string, 0, 5)
	for g := range guidSet {
		sampleRecycleBin = append(sampleRecycleBin, g)
		if len(sampleRecycleBin) >= 5 {
			break
		}
	}
	log.Info().Strs("sample_recycle_guids", sampleRecycleBin).
		Int("total_recycle_guids", len(guidSet)).
		Msg("classifyRecycleBinPortals: recycle bin GUID samples")

	// Scan every non-stub message, including soft-deleted rows. That lets us
	// detect restored portals that were previously soft-deleted locally.
	rows, err := s.db.Query(ctx, `
		SELECT portal_id, guid, timestamp_ms, deleted
		FROM cloud_message
		WHERE login_id=$1
		  AND portal_id IS NOT NULL AND portal_id <> ''
		  AND record_name <> ''
		ORDER BY portal_id ASC, timestamp_ms DESC, guid DESC
	`, s.loginID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	statesByPortal := make(map[string]*recycleBinPortalState)
	orderedPortalIDs := make([]string, 0)
	currentPortalID := ""
	var currentState *recycleBinPortalState
	recoverableSuffixOpen := false
	sampleDBGuids := make([]string, 0, 5)

	for rows.Next() {
		var portalID, guid string
		var timestampMS int64
		var deleted bool
		if err = rows.Scan(&portalID, &guid, &timestampMS, &deleted); err != nil {
			return nil, err
		}

		if len(sampleDBGuids) < 5 {
			sampleDBGuids = append(sampleDBGuids, guid)
		}

		if portalID != currentPortalID {
			currentPortalID = portalID
			currentState = &recycleBinPortalState{
				PortalID: portalID,
				NewestTS: timestampMS,
			}
			statesByPortal[portalID] = currentState
			orderedPortalIDs = append(orderedPortalIDs, portalID)
			recoverableSuffixOpen = true
		}

		currentState.Total++
		if deleted {
			currentState.HasDeletedMessages = true
		} else {
			currentState.HasLiveMessages = true
		}

		isRecoverable := guidSet[guid]
		if isRecoverable {
			currentState.Recoverable++
		}
		if recoverableSuffixOpen {
			if isRecoverable {
				currentState.RecoverableSuffix++
			} else {
				recoverableSuffixOpen = false
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	states := make([]recycleBinPortalState, 0, len(orderedPortalIDs))
	totalMessages := 0
	totalRecoverable := 0
	deletedPortals := 0
	restoredPortals := 0
	for _, portalID := range orderedPortalIDs {
		state := *statesByPortal[portalID]
		totalMessages += state.Total
		totalRecoverable += state.Recoverable
		if state.Recoverable == 0 {
			continue
		}
		if state.LooksDeleted() {
			deletedPortals++
		} else if state.LooksRestored() {
			restoredPortals++
		}
		states = append(states, state)
	}
	log.Info().Strs("sample_db_guids", sampleDBGuids).
		Int("total_db_messages", totalMessages).
		Msg("classifyRecycleBinPortals: cloud_message GUID samples")
	zerolog.Ctx(ctx).Info().
		Int("portals_checked", len(orderedPortalIDs)).
		Int("total_messages", totalMessages).
		Int("total_recoverable", totalRecoverable).
		Int("guid_set_size", len(guidSet)).
		Int("candidate_portals", len(states)).
		Int("deleted_portals", deletedPortals).
		Int("restored_portals", restoredPortals).
		Msg("classifyRecycleBinPortals stats")

	return states, nil
}

// insertDeletedChatTombstone inserts a cloud_chat row with deleted=TRUE.
// Used by seedDeletedChatsFromRecycleBin to persist delete knowledge across
// restarts on a fresh database. The ON CONFLICT clause ensures that if the
// chat already exists (from a prior sync), we don't overwrite it — only
// insert if it's genuinely new. If CloudKit later syncs the chat as live,
// upsertChatBatch will set deleted=FALSE automatically.
func (s *cloudBackfillStore) insertDeletedChatTombstone(
	ctx context.Context,
	cloudChatID, portalID, recordName, groupID, service string,
	displayName *string,
	participantsJSON string,
) error {
	nowMS := time.Now().UnixMilli()
	_, err := s.db.Exec(ctx, `
		INSERT INTO cloud_chat (
			login_id, cloud_chat_id, record_name, group_id, portal_id, service,
			display_name, participants_json, updated_ts, created_ts, deleted
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, TRUE)
		ON CONFLICT (login_id, cloud_chat_id) DO NOTHING
	`, s.loginID, cloudChatID, recordName, groupID, portalID, service, nullableString(displayName), participantsJSON, nowMS, nowMS)
	return err
}

func (s *cloudBackfillStore) ensureDeletedChatTombstoneByPortalID(ctx context.Context, portalID string) error {
	chatID := s.getChatIdentifierByPortalID(ctx, portalID)
	if chatID == "" {
		chatID = "synthetic:recoverable:" + portalID
	}

	recordName := ""
	recordNames, err := s.getCloudRecordNamesByPortalID(ctx, portalID)
	if err != nil {
		return err
	}
	if len(recordNames) > 0 {
		recordName = recordNames[0]
	}

	groupID := ""
	if strings.HasPrefix(portalID, "gid:") {
		groupID = strings.TrimPrefix(portalID, "gid:")
	}

	displayNameValue, err := s.getDisplayNameByPortalID(ctx, portalID)
	if err != nil {
		return err
	}
	var displayName *string
	if displayNameValue != "" {
		displayName = &displayNameValue
	}

	participants, err := s.getChatParticipantsByPortalID(ctx, portalID)
	if err != nil {
		return err
	}
	if len(participants) == 0 && !strings.HasPrefix(portalID, "gid:") && !strings.Contains(portalID, ",") {
		participants = []string{portalID}
	}
	participantsJSON, err := json.Marshal(participants)
	if err != nil {
		return err
	}

	service := "iMessage"
	if strings.HasPrefix(chatID, "SMS") {
		service = "SMS"
	}

	return s.insertDeletedChatTombstone(
		ctx,
		chatID,
		portalID,
		recordName,
		groupID,
		service,
		displayName,
		string(participantsJSON),
	)
}

func (s *cloudBackfillStore) getChatPortalID(ctx context.Context, cloudChatID string) (string, error) {
	var portalID string
	// Try matching by cloud_chat_id, record_name, or group_id.
	// CloudKit messages reference chats by group_id UUID (the chatID field),
	// while cloud_chat stores chat_identifier as cloud_chat_id and record hash as record_name.
	// Use LOWER() on group_id because CloudKit stores it uppercase but messages reference it lowercase.
	err := s.db.QueryRow(ctx,
		`SELECT portal_id FROM cloud_chat WHERE login_id=$1 AND (cloud_chat_id=$2 OR record_name=$2 OR LOWER(group_id)=LOWER($2))`,
		s.loginID, cloudChatID,
	).Scan(&portalID)
	if err != nil {
		if err == sql.ErrNoRows {
			// Messages use chat_identifier format like "SMS;-;+14158138533" or "iMessage;-;user@example.com"
			// but cloud_chat stores just the identifier part ("+14158138533" or "user@example.com"),
			// or the reverse: cloud_chat stores "any;-;email" but the message has bare "email".
			if parts := strings.SplitN(cloudChatID, ";-;", 2); len(parts) == 2 {
				// Has a service prefix — strip it and try the bare identifier.
				bareID := parts[1]
				if pid, err2 := s.getChatPortalID(ctx, bareID); err2 == nil && pid != "" {
					return pid, nil
				}
				// Bare lookup also failed. Try all service-prefix variants — the DB
				// row may store a different prefix than the message carries.
				for _, prefix := range []string{"iMessage;-;", "any;-;", "SMS;-;"} {
					candidate := prefix + bareID
					if candidate == cloudChatID {
						continue // already tried exact match above
					}
					var pid string
					if err2 := s.db.QueryRow(ctx,
						`SELECT portal_id FROM cloud_chat WHERE login_id=$1 AND (cloud_chat_id=$2 OR record_name=$2)`,
						s.loginID, candidate,
					).Scan(&pid); err2 == nil && pid != "" {
						return pid, nil
					}
				}
			} else if !strings.Contains(cloudChatID, ";") {
				// Bare chatId (no service prefix at all). Try adding known prefixes —
				// the DB row may have been seeded with "any;-;email" while the message
				// carries just "email".
				for _, prefix := range []string{"iMessage;-;", "any;-;", "SMS;-;"} {
					var pid string
					if err2 := s.db.QueryRow(ctx,
						`SELECT portal_id FROM cloud_chat WHERE login_id=$1 AND (cloud_chat_id=$2 OR record_name=$2)`,
						s.loginID, prefix+cloudChatID,
					).Scan(&pid); err2 == nil && pid != "" {
						return pid, nil
					}
				}
			}
			return "", nil
		}
		return "", err
	}
	return portalID, nil
}

// listDeletedPortalIDs returns portal_ids whose chat rows are still fully
// soft-deleted (at least one deleted row, no live replacement). Used to
// repopulate recentlyDeletedPortals on bridge restart so tombstoned chats from
// prior sessions stay protected without re-marking chats that were already
// restored locally.
func (s *cloudBackfillStore) listDeletedPortalIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.Query(ctx,
		`
			SELECT portal_id
			FROM cloud_chat
			WHERE login_id=$1 AND portal_id <> ''
			GROUP BY portal_id
			HAVING MAX(CASE WHEN deleted=TRUE THEN 1 ELSE 0 END) = 1
			   AND MAX(CASE WHEN deleted=FALSE THEN 1 ELSE 0 END) = 0
		`,
		s.loginID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var portalIDs []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		portalIDs = append(portalIDs, id)
	}
	return portalIDs, rows.Err()
}

func (s *cloudBackfillStore) setRestoreOverride(ctx context.Context, portalID string) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO restore_override (login_id, portal_id, updated_ts)
		VALUES ($1, $2, $3)
		ON CONFLICT (login_id, portal_id) DO UPDATE SET updated_ts=excluded.updated_ts
	`, s.loginID, portalID, time.Now().UnixMilli())
	return err
}

func (s *cloudBackfillStore) clearRestoreOverride(ctx context.Context, portalID string) error {
	_, err := s.db.Exec(ctx,
		`DELETE FROM restore_override WHERE login_id=$1 AND portal_id=$2`,
		s.loginID, portalID,
	)
	return err
}

func (s *cloudBackfillStore) hasRestoreOverride(ctx context.Context, portalID string) (bool, error) {
	var count int
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM restore_override WHERE login_id=$1 AND portal_id=$2`,
		s.loginID, portalID,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// listRestoreOverrides returns all portal IDs that currently have a restore
// override set. Returns nil on error.
func (s *cloudBackfillStore) listRestoreOverrides(ctx context.Context) []string {
	rows, err := s.db.Query(ctx,
		`SELECT portal_id FROM restore_override WHERE login_id=$1 ORDER BY portal_id`,
		s.loginID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err == nil {
			out = append(out, pid)
		}
	}
	return out
}

// portalHasChat returns true if the given portal_id has at least one
// cloud_chat record (i.e., the conversation was included in CloudKit chat
// sync). Portals with no chat record are orphaned — typically junk/spam
// that Apple filters from the chat zone.
func (s *cloudBackfillStore) portalHasChat(ctx context.Context, portalID string) (bool, error) {
	var count int
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM cloud_chat WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE`,
		s.loginID, portalID,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// portalIsExplicitlyDeleted returns true if the given portal_id has a
// cloud_chat row with deleted=TRUE. This indicates the user or Apple
// explicitly deleted the conversation; messages for such portals must not
// be ingested during CloudKit sync (they would resurrect zombie portals).
//
// Unlike portalHasChat (which requires a live row), this returns false for
// portals that simply have no cloud_chat row — e.g. recycle-bin-only chats
// on a fresh sync whose chat record never appeared in the main zone. Those
// portals are allowed through so their main-zone messages are stored.
func (s *cloudBackfillStore) portalIsExplicitlyDeleted(ctx context.Context, portalID string) (bool, error) {
	var count int
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM cloud_chat WHERE login_id=$1 AND portal_id=$2 AND deleted=TRUE`,
		s.loginID, portalID,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

type softDeletedPortalInfo struct {
	NewestTS int64
	Deleted  bool
}

// getSoftDeletedPortalInfo returns whether the portal is still fully
// soft-deleted (deleted rows with no live replacement) and the latest known
// timestamp for that deleted state. The timestamp is the max of:
//   - newest soft-deleted cloud_message timestamp
//   - cloud_chat.updated_ts, which is bumped on delete/undelete so chats with
//     no imported messages still have a meaningful cutoff.
//
// OpenBubbles unconditionally revives on any incoming message; we add
// tail-timestamp gating because APNs replays stale messages more aggressively
// than CloudKit (which is OpenBubbles' primary sync path).
func (s *cloudBackfillStore) getSoftDeletedPortalInfo(ctx context.Context, portalID string) (softDeletedPortalInfo, error) {
	var info softDeletedPortalInfo
	var deletedCount, liveCount int
	var newestMessageTS, newestChatTS sql.NullInt64
	err := s.db.QueryRow(ctx, `
		WITH chat_stats AS (
			SELECT
				COALESCE(SUM(CASE WHEN deleted=TRUE THEN 1 ELSE 0 END), 0) AS deleted_count,
				COALESCE(SUM(CASE WHEN deleted=FALSE THEN 1 ELSE 0 END), 0) AS live_count,
				MAX(updated_ts) AS newest_chat_ts
			FROM cloud_chat
			WHERE login_id=$1 AND portal_id=$2
		),
		message_stats AS (
			SELECT MAX(timestamp_ms) AS newest_message_ts
			FROM cloud_message
			WHERE login_id=$1 AND portal_id=$2 AND deleted=TRUE
		)
		SELECT
			cs.deleted_count,
			cs.live_count,
			ms.newest_message_ts,
			cs.newest_chat_ts
		FROM chat_stats cs
		CROSS JOIN message_stats ms
	`, s.loginID, portalID).Scan(&deletedCount, &liveCount, &newestMessageTS, &newestChatTS)
	if err != nil {
		return info, err
	}

	info.Deleted = deletedCount > 0 && liveCount == 0
	if newestMessageTS.Valid && newestMessageTS.Int64 > info.NewestTS {
		info.NewestTS = newestMessageTS.Int64
	}
	if newestChatTS.Valid && newestChatTS.Int64 > info.NewestTS {
		info.NewestTS = newestChatTS.Int64
	}
	return info, nil
}

func (s *cloudBackfillStore) hasChat(ctx context.Context, cloudChatID string) (bool, error) {
	var count int
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM cloud_chat WHERE login_id=$1 AND cloud_chat_id=$2`,
		s.loginID, cloudChatID,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// hasChatBatch checks existence of multiple cloud chat IDs in a single query
// and returns the set of IDs that already exist.
func (s *cloudBackfillStore) hasChatBatch(ctx context.Context, chatIDs []string) (map[string]bool, error) {
	if len(chatIDs) == 0 {
		return nil, nil
	}
	existing := make(map[string]bool, len(chatIDs))
	const chunkSize = 500
	for i := 0; i < len(chatIDs); i += chunkSize {
		end := i + chunkSize
		if end > len(chatIDs) {
			end = len(chatIDs)
		}
		chunk := chatIDs[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, s.loginID)
		for j, id := range chunk {
			placeholders[j] = fmt.Sprintf("$%d", j+2)
			args = append(args, id)
		}

		query := fmt.Sprintf(
			`SELECT cloud_chat_id FROM cloud_chat WHERE login_id=$1 AND cloud_chat_id IN (%s)`,
			strings.Join(placeholders, ","),
		)
		rows, err := s.db.Query(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			existing[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return existing, nil
}

// getAttachmentRecordName returns the CloudKit record_name for a single
// attachment of a message, looked up by (message_guid, att_index). Returns
// empty string + nil error if the cloud_message row or its attachments_json
// hasn't been ingested yet (CloudKit sync may still be catching up on the
// live APNs-delivered message). Used by the AttachmentRetrier as a fallback
// when the MMCS URL retry exhausts — if CloudKit has the record, the retrier
// can download via safeCloudDownloadAttachment instead.
func (s *cloudBackfillStore) getAttachmentRecordName(ctx context.Context, msgGUID string, attIndex int) (string, error) {
	var attsJSON string
	err := s.db.QueryRow(ctx,
		`SELECT COALESCE(attachments_json, '') FROM cloud_message WHERE login_id=$1 AND guid=$2`,
		s.loginID, msgGUID,
	).Scan(&attsJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	if attsJSON == "" {
		return "", nil
	}
	var atts []cloudAttachmentRow
	if err := json.Unmarshal([]byte(attsJSON), &atts); err != nil {
		return "", fmt.Errorf("unmarshal attachments_json for %s: %w", msgGUID, err)
	}
	if attIndex < 0 || attIndex >= len(atts) {
		return "", nil
	}
	return atts[attIndex].RecordName, nil
}

func (s *cloudBackfillStore) getChatParticipantsByPortalID(ctx context.Context, portalID string) ([]string, error) {
	var participantsJSON string
	err := s.db.QueryRow(ctx,
		`SELECT participants_json FROM cloud_chat WHERE login_id=$1 AND portal_id=$2 AND participants_json IS NOT NULL AND participants_json <> '' LIMIT 1`,
		s.loginID, portalID,
	).Scan(&participantsJSON)
	// Fallback: the portal ID's UUID might be a chat_id that differs from
	// the group_id. Try matching by group_id so gid:<chat_id> portals can
	// still find participants stored under gid:<group_id>.
	if err != nil && strings.HasPrefix(portalID, "gid:") {
		uuid := strings.TrimPrefix(portalID, "gid:")
		err = s.db.QueryRow(ctx,
			`SELECT participants_json FROM cloud_chat WHERE login_id=$1 AND (LOWER(group_id)=LOWER($2) OR LOWER(cloud_chat_id)=LOWER($2)) AND participants_json IS NOT NULL AND participants_json <> '' LIMIT 1`,
			s.loginID, uuid,
		).Scan(&participantsJSON)
	}
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	var participants []string
	if err = json.Unmarshal([]byte(participantsJSON), &participants); err != nil {
		return nil, err
	}
	// Normalize participants to portal ID format (e.g., tel:+14158138533)
	normalized := make([]string, 0, len(participants))
	for _, p := range participants {
		n := normalizeIdentifierForPortalID(p)
		if n != "" {
			normalized = append(normalized, n)
		}
	}
	return normalized, nil
}

// updateChatParticipants overwrites the persisted participant roster for an
// existing cloud_chat row, touching only participants_json (and updated_ts).
//
// Unlike upsertChat it never clobbers record_name / display_name / group_id /
// service, so it is safe to call from the real-time message path on every
// membership change. participants_json is the SOLE source of the outbound
// recipient list for gid: group portals (see portalToConversation), and the
// real-time persist in makePortalKey is insert-once — so without this a member
// who leaves is never dropped from the send list and keeps receiving messages.
//
// It targets the same row getChatParticipantsByPortalID reads: primary match
// by portal_id, with a gid:<uuid> fallback to group_id / cloud_chat_id. No row
// is created if none exists — first-time creation stays owned by makePortalKey.
func (s *cloudBackfillStore) updateChatParticipants(ctx context.Context, portalID string, participants []string) error {
	participantsJSON, err := json.Marshal(participants)
	if err != nil {
		return err
	}
	nowMS := time.Now().UnixMilli()
	res, err := s.db.Exec(ctx,
		`UPDATE cloud_chat SET participants_json=$3, updated_ts=$4 WHERE login_id=$1 AND portal_id=$2`,
		s.loginID, portalID, string(participantsJSON), nowMS,
	)
	if err != nil {
		return err
	}
	// Fallback for gid: portals whose row is keyed by group_id / cloud_chat_id
	// rather than portal_id (mirrors getChatParticipantsByPortalID's fallback).
	if strings.HasPrefix(portalID, "gid:") {
		if affected, aErr := res.RowsAffected(); aErr == nil && affected == 0 {
			uuid := strings.TrimPrefix(portalID, "gid:")
			if _, err = s.db.Exec(ctx,
				`UPDATE cloud_chat SET participants_json=$3, updated_ts=$4 WHERE login_id=$1 AND (LOWER(group_id)=LOWER($2) OR LOWER(cloud_chat_id)=LOWER($2))`,
				s.loginID, uuid, string(participantsJSON), nowMS,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

// syncStoredGroupRoster is the testable core of the self-heal: it reads the
// stored roster for portalID and overwrites it with `normalized` (a roster of
// pre-normalized member IDs) when they differ as sets. It is a no-op — writing
// nothing and reporting updated=false — when no row exists yet (first-time
// creation is owned by makePortalKey's insert path) or when the rosters already
// match (ignoring self-handle representation, via isSelf). Returns the old
// member count for logging and whether an update was written.
func syncStoredGroupRoster(ctx context.Context, store *cloudBackfillStore, portalID string, normalized []string, isSelf func(string) bool) (oldCount int, updated bool, err error) {
	existing, err := store.getChatParticipantsByPortalID(ctx, portalID)
	if err != nil {
		return 0, false, err
	}
	if len(existing) == 0 || participantSetsMatch(existing, normalized, isSelf) {
		return len(existing), false, nil
	}
	if err := store.updateChatParticipants(ctx, portalID, normalized); err != nil {
		return len(existing), false, err
	}
	return len(existing), true, nil
}

// getDisplayNameByPortalID returns the CloudKit display_name for a given portal_id.
// This is the user-set group name (cv_name from the iMessage protocol), NOT an
// auto-generated label. Returns empty string if none found or if the group is unnamed.
func (s *cloudBackfillStore) getDisplayNameByPortalID(ctx context.Context, portalID string) (string, error) {
	var displayName sql.NullString
	err := s.db.QueryRow(ctx,
		`SELECT display_name FROM cloud_chat WHERE login_id=$1 AND portal_id=$2 AND display_name IS NOT NULL AND display_name <> '' ORDER BY updated_ts DESC LIMIT 1`,
		s.loginID, portalID,
	).Scan(&displayName)
	if err == nil && displayName.Valid {
		return displayName.String, nil
	}
	// Fallback: the portal ID's UUID might be a chat_id that differs from
	// the group_id. Try matching by group_id so gid:<chat_id> portals can
	// still find the display_name stored under gid:<group_id>.
	if strings.HasPrefix(portalID, "gid:") {
		uuid := strings.TrimPrefix(portalID, "gid:")
		err = s.db.QueryRow(ctx,
			`SELECT display_name FROM cloud_chat WHERE login_id=$1 AND (LOWER(group_id)=LOWER($2) OR LOWER(cloud_chat_id)=LOWER($2)) AND display_name IS NOT NULL AND display_name <> '' ORDER BY updated_ts DESC LIMIT 1`,
			s.loginID, uuid,
		).Scan(&displayName)
		if err == nil && displayName.Valid {
			return displayName.String, nil
		}
	}
	return "", nil
}

// getChatIdentifierByPortalID returns the CloudKit chat_identifier (e.g.
// "iMessage;-;user@example.com") for a given portal_id. Used to construct the
// chat GUID for MoveToRecycleBin messages.
func (s *cloudBackfillStore) getChatIdentifierByPortalID(ctx context.Context, portalID string) string {
	var chatID string
	err := s.db.QueryRow(ctx,
		`SELECT cloud_chat_id FROM cloud_chat WHERE login_id=$1 AND portal_id=$2 AND cloud_chat_id <> '' AND cloud_chat_id NOT LIKE 'synthetic:%' AND cloud_chat_id NOT LIKE 'recycle:%' LIMIT 1`,
		s.loginID, portalID,
	).Scan(&chatID)
	if err != nil {
		return ""
	}
	return chatID
}

// getCloudRecordNamesByPortalID returns all non-empty chat record_names for a portal.
func (s *cloudBackfillStore) getCloudRecordNamesByPortalID(ctx context.Context, portalID string) ([]string, error) {
	rows, err := s.db.Query(ctx,
		`SELECT record_name FROM cloud_chat WHERE login_id=$1 AND portal_id=$2 AND record_name <> ''`,
		s.loginID, portalID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// getMessageRecordNamesByPortalID returns all non-empty message record_names for a portal.
func (s *cloudBackfillStore) getMessageRecordNamesByPortalID(ctx context.Context, portalID string) ([]string, error) {
	rows, err := s.db.Query(ctx,
		`SELECT record_name FROM cloud_message WHERE login_id=$1 AND portal_id=$2 AND record_name <> ''`,
		s.loginID, portalID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// getCloudRecordNamesByGroupID returns all non-empty chat record_names for ANY
// portal_id that shares the given group_id.
func (s *cloudBackfillStore) getCloudRecordNamesByGroupID(ctx context.Context, groupID string) ([]string, error) {
	rows, err := s.db.Query(ctx,
		`SELECT record_name FROM cloud_chat WHERE login_id=$1 AND (LOWER(group_id)=LOWER($2) OR LOWER(cloud_chat_id)=LOWER($2)) AND record_name <> ''`,
		s.loginID, groupID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// findPortalIDsByGroupID returns all distinct portal IDs associated with a
// CloudKit group UUID. Used to dedupe group restores when participant data is
// missing in delete/recover payloads.
func (s *cloudBackfillStore) findPortalIDsByGroupID(ctx context.Context, groupID string) ([]string, error) {
	rows, err := s.db.Query(ctx,
		`SELECT DISTINCT portal_id FROM cloud_chat WHERE login_id=$1 AND (LOWER(group_id)=LOWER($2) OR LOWER(cloud_chat_id)=LOWER($2)) AND portal_id <> ''`,
		s.loginID, groupID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var portalIDs []string
	for rows.Next() {
		var portalID string
		if err = rows.Scan(&portalID); err != nil {
			return nil, err
		}
		portalIDs = append(portalIDs, portalID)
	}
	return portalIDs, rows.Err()
}

// getGroupIDForPortalID returns the group_id associated with a portal in the
// cloud_chat table. This is useful for cross-referencing when a portal's UUID
// (extracted from the portal ID) might be a chat_id rather than the group_id.
// Returns "" if no group_id is found.
func (s *cloudBackfillStore) getGroupIDForPortalID(ctx context.Context, portalID string) string {
	var groupID string
	err := s.db.QueryRow(ctx,
		`SELECT group_id FROM cloud_chat WHERE login_id=$1 AND portal_id=$2 AND group_id <> '' LIMIT 1`,
		s.loginID, portalID,
	).Scan(&groupID)
	if err == nil {
		return groupID
	}
	// Fallback: after normalizeGroupChatPortalIDs, cloud_chat rows that used
	// gid:<chat_id> now have portal_id=gid:<group_id>. Search by cloud_chat_id
	// so callers using the old portal_id can still find the group_id.
	if strings.HasPrefix(portalID, "gid:") {
		uuid := strings.TrimPrefix(portalID, "gid:")
		err = s.db.QueryRow(ctx,
			`SELECT group_id FROM cloud_chat WHERE login_id=$1 AND LOWER(cloud_chat_id)=LOWER($2) AND group_id <> '' LIMIT 1`,
			s.loginID, uuid,
		).Scan(&groupID)
		if err == nil {
			return groupID
		}
	}
	return ""
}

// normalizeGroupChatPortalIDs unifies cloud_chat portal_ids so all rows for
// the same group use the canonical portal_id (gid:<group_id>). When the same
// group is ingested via different CloudKit records, one row may have
// portal_id=gid:<chat_id> while another has portal_id=gid:<group_id>. This
// inconsistency causes createPortalsFromCloudSync to see two distinct portals
// for the same group, leading to duplicates. Returns the number of rows updated.
func (s *cloudBackfillStore) normalizeGroupChatPortalIDs(ctx context.Context) (int64, error) {
	// For each cloud_chat row with a gid: portal_id where the UUID does NOT
	// match the row's own group_id, update portal_id to gid:<group_id>.
	// This only applies when group_id is known and the portal_id uses a
	// different UUID (i.e. the chat_id).
	res, err := s.db.Exec(ctx, `
		UPDATE cloud_chat
		SET portal_id = 'gid:' || LOWER(group_id)
		WHERE login_id = $1
		  AND group_id <> ''
		  AND portal_id LIKE 'gid:%'
		  AND LOWER(SUBSTR(portal_id, 5)) <> LOWER(group_id)
	`, s.loginID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// normalizeGroupMessagePortalIDs fixes cloud_message rows where the portal_id
// uses a UUID that differs from the canonical group_id → portal_id mapping in
// cloud_chat. This happens when resolveConversationID used the CloudKit chat_id
// UUID (before the getChatPortalID-first fix) instead of the group_id UUID.
// Returns the number of rows updated.
func (s *cloudBackfillStore) normalizeGroupMessagePortalIDs(ctx context.Context) (int64, error) {
	// Find cloud_message rows with gid: portal_ids where the UUID matches
	// a cloud_chat row's group_id but the portal_id doesn't match.
	// Update them to use the canonical portal_id from cloud_chat.
	res, err := s.db.Exec(ctx, `
		UPDATE cloud_message
		SET portal_id = (
			SELECT cc.portal_id FROM cloud_chat cc
			WHERE cc.login_id = cloud_message.login_id
			  AND (LOWER(cc.group_id) = LOWER(SUBSTR(cloud_message.portal_id, 5))
			       OR LOWER(cc.cloud_chat_id) = LOWER(SUBSTR(cloud_message.portal_id, 5)))
			  AND cc.portal_id <> cloud_message.portal_id
			  AND cc.portal_id <> ''
			LIMIT 1
		)
		WHERE login_id = $1
		  AND portal_id LIKE 'gid:%'
		  AND EXISTS (
			SELECT 1 FROM cloud_chat cc
			WHERE cc.login_id = cloud_message.login_id
			  AND (LOWER(cc.group_id) = LOWER(SUBSTR(cloud_message.portal_id, 5))
			       OR LOWER(cc.cloud_chat_id) = LOWER(SUBSTR(cloud_message.portal_id, 5)))
			  AND cc.portal_id <> cloud_message.portal_id
			  AND cc.portal_id <> ''
		  )
	`, s.loginID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// getMessageRecordNamesByGroupID returns all non-empty message record_names
// for portals that share the given group_id.
func (s *cloudBackfillStore) getMessageRecordNamesByGroupID(ctx context.Context, groupID string) ([]string, error) {
	rows, err := s.db.Query(ctx, `
		SELECT cm.record_name FROM cloud_message cm
		INNER JOIN cloud_chat cc ON cc.login_id=cm.login_id AND cc.portal_id=cm.portal_id
		WHERE cm.login_id=$1 AND LOWER(cc.group_id)=LOWER($2) AND cm.record_name <> ''
	`, s.loginID, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// getRecordNameByGUID returns the CloudKit record_name for a message GUID.
// Returns empty string if not found. Used for iCloud message deletion.
func (s *cloudBackfillStore) getRecordNameByGUID(ctx context.Context, guid string) string {
	var recordName string
	err := s.db.QueryRow(ctx,
		`SELECT record_name FROM cloud_message WHERE login_id=$1 AND guid=$2 AND record_name <> ''`,
		s.loginID, guid,
	).Scan(&recordName)
	if err != nil {
		return ""
	}
	return recordName
}

// softDeleteMessageByGUID marks a cloud_message row as deleted=TRUE so it won't
// be re-bridged on backfill, while preserving the UUID for echo detection.
func (s *cloudBackfillStore) softDeleteMessageByGUID(ctx context.Context, guid string) error {
	// UPDATE-then-stub-INSERT must be atomic: between the UPDATE reporting
	// 0 rows and the INSERT OR IGNORE running, a concurrent CloudKit upsert
	// could land a live (deleted=FALSE) row for this guid. INSERT OR IGNORE
	// would then no-op against it and the body would bridge undeleted. Wrap
	// both in a single txn so the upsert serializes either fully before
	// (UPDATE scrubs it) or fully after (its ON CONFLICT CASE preserves the
	// stub's deleted=TRUE). DoTxn reuses an outer txn if ctx already carries
	// one, so this stays correct when called inside a larger transaction.
	return s.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		// UPPER() on both sides: APNs delivers UUIDs uppercase while CloudKit
		// can store them lower/mixed; a case-sensitive = miss would silently
		// no-op and let the fail-closed delete path proceed thinking it scrubbed.
		// Matches the case-handling in getMessageTextByGUID/getMessageTimestampByGUID.
		res, err := s.db.Exec(ctx,
			`UPDATE cloud_message
			 SET deleted=TRUE,
			     text=NULL, subject=NULL, sender='',
			     tapback_emoji=NULL,
			     body_scrubbed=TRUE
			 WHERE login_id=$1 AND UPPER(guid)=UPPER($2)`,
			s.loginID, guid,
		)
		if err != nil {
			return fmt.Errorf("soft-delete message %s: %w", guid, err)
		}
		n, _ := res.RowsAffected()
		if n > 0 {
			return nil
		}
		// Stub-insert path: Apple-side delete can arrive before CloudKit sync
		// ever populates cloud_message (typical for messages sent and quickly
		// retracted on the iPhone, or for portals still pending creation that
		// hold the original in pendingPortalMsgs). Drop a deleted=TRUE stub
		// so the future CloudKit upsert's ON CONFLICT CASE preserves the
		// deletion (excluded.deleted ignored when cloud_message.deleted=TRUE).
		// Without this stub the row would arrive live and bridge the body
		// Apple already deleted on the user's devices.
		nowMS := time.Now().UnixMilli()
		_, err = s.db.Exec(ctx,
			`INSERT OR IGNORE INTO cloud_message
				(login_id, guid, timestamp_ms, is_from_me, deleted, body_scrubbed, created_ts, updated_ts)
			 VALUES ($1, $2, $3, FALSE, TRUE, TRUE, $4, $4)`,
			s.loginID, guid, nowMS, nowMS,
		)
		if err != nil {
			return fmt.Errorf("stub-insert deleted message %s: %w", guid, err)
		}
		return nil
	})
}

// getGroupPhotoByPortalID returns the group_photo_guid and record_name for
// the most recently updated cloud_chat row that has a group photo set.
// Returns ("", "", nil) if no photo is set.
func (s *cloudBackfillStore) getGroupPhotoByPortalID(ctx context.Context, portalID string) (guid, recordName string, err error) {
	var g, r sql.NullString
	err = s.db.QueryRow(ctx,
		`SELECT group_photo_guid, record_name FROM cloud_chat WHERE login_id=$1 AND portal_id=$2 AND group_photo_guid IS NOT NULL AND group_photo_guid <> '' ORDER BY updated_ts DESC LIMIT 1`,
		s.loginID, portalID,
	).Scan(&g, &r)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", "", nil
		}
		return "", "", err
	}
	if g.Valid {
		rStr := ""
		if r.Valid {
			rStr = r.String
		}
		return g.String, rStr, nil
	}
	return "", "", nil
}

// clearGroupPhotoGuid sets group_photo_guid = NULL for all cloud_chat rows
// matching a portal_id. Called when a real-time IconChange(cleared) arrives so
// that subsequent GetChatInfo calls know there is no custom photo.
func (s *cloudBackfillStore) clearGroupPhotoGuid(ctx context.Context, portalID string) error {
	_, err := s.db.Exec(ctx,
		`UPDATE cloud_chat SET group_photo_guid = NULL WHERE login_id = $1 AND portal_id = $2`,
		s.loginID, portalID,
	)
	return err
}

// saveGroupPhoto persists MMCS-downloaded group photo bytes and the IconChange
// timestamp (used as avatar ID) to the group_photo_cache table.
// UPSERT so it works regardless of whether a cloud_chat row exists yet.
func (s *cloudBackfillStore) saveGroupPhoto(ctx context.Context, portalID string, ts int64, data []byte) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO group_photo_cache (login_id, portal_id, ts, data) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (login_id, portal_id) DO UPDATE SET ts=excluded.ts, data=excluded.data`,
		s.loginID, portalID, ts, data,
	)
	return err
}

// getGroupPhoto returns the locally cached group photo bytes and timestamp for
// the given portal. Returns (0, nil, nil) if no cached photo exists.
func (s *cloudBackfillStore) getGroupPhoto(ctx context.Context, portalID string) (ts int64, data []byte, err error) {
	err = s.db.QueryRow(ctx,
		`SELECT ts, data FROM group_photo_cache WHERE login_id=$1 AND portal_id=$2`,
		s.loginID, portalID,
	).Scan(&ts, &data)
	if err == sql.ErrNoRows {
		return 0, nil, nil
	}
	return ts, data, err
}

// clearGroupPhoto removes the cached group photo for a portal.
// Called when an IconChange(cleared) arrives so GetChatInfo won't
// re-apply a stale photo after restart.
func (s *cloudBackfillStore) clearGroupPhoto(ctx context.Context, portalID string) error {
	_, err := s.db.Exec(ctx,
		`DELETE FROM group_photo_cache WHERE login_id=$1 AND portal_id=$2`,
		s.loginID, portalID,
	)
	return err
}

// updateDisplayNameByPortalID updates the display_name for all cloud_chat
// rows matching a portal_id. Used when a real-time rename event arrives to
// correct stale CloudKit data in the local cache.
func (s *cloudBackfillStore) updateDisplayNameByPortalID(ctx context.Context, portalID, displayName string) error {
	_, err := s.db.Exec(ctx,
		`UPDATE cloud_chat SET display_name=$1 WHERE login_id=$2 AND portal_id=$3`,
		displayName, s.loginID, portalID,
	)
	return err
}

// deleteLocalChatByPortalID soft-deletes the cloud_chat and cloud_message
// records for a portal (sets deleted=TRUE, preserves the rows).
//
// cloud_chat is soft-deleted so that restore-chat can recover group name and
// participant data. Queries that drive portal creation (listPortalIDsWithNewestTimestamp,
// portalHasChat) filter on deleted=FALSE so soft-deleted rows don't resurrect portals.
//
// cloud_message is soft-deleted, NOT hard-deleted. The rows' GUIDs must survive
// so that hasMessageUUID can detect stale APNs echoes even after:
//   - pending_cloud_deletion is cleared (CloudKit deletion finished)
//   - Bridge restarts (recentlyDeletedPortals is repopulated from pending entries,
//     but once cleared there is no in-memory protection)
//
// hasMessageUUID queries cloud_message without filtering on deleted, so
// soft-deleted UUIDs still block echoes. Fresh UUIDs from genuinely new
// messages are not in cloud_message → hasMessageUUID=false → portal allowed.
//
// All other cloud_message queries (listLatestMessages, hasPortalMessages,
// getOldestMessageTimestamp, FetchMessages) filter WHERE deleted=FALSE, so
// soft-deleted rows don't trigger backfill or portal resurrection.
func (s *cloudBackfillStore) deleteLocalChatByPortalID(ctx context.Context, portalID string) error {
	nowMS := time.Now().UnixMilli()
	// Only update rows not already deleted. Re-stamping already-deleted rows
	// would push updated_ts to now, breaking tail-timestamp gating (the
	// deleted tail would jump to the current time, suppressing all future
	// messages as "not newer than deleted tail").
	if _, err := s.db.Exec(ctx,
		`UPDATE cloud_chat SET deleted=TRUE, updated_ts=$3 WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE`,
		s.loginID, portalID, nowMS,
	); err != nil {
		return fmt.Errorf("failed to soft-delete cloud_chat records for portal %s: %w", portalID, err)
	}
	if _, err := s.db.Exec(ctx,
		`UPDATE cloud_message
		 SET deleted=TRUE,
		     text=NULL, subject=NULL, sender='',
		     tapback_emoji=NULL,
		     body_scrubbed=TRUE,
		     updated_ts=$3
		 WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE`,
		s.loginID, portalID, nowMS,
	); err != nil {
		return fmt.Errorf("failed to soft-delete cloud_message records for portal %s: %w", portalID, err)
	}
	// For gid: portals, also soft-delete rows stored under a different
	// portal_id that share the same group_id. CloudKit chat_id UUIDs can
	// differ from group_id UUIDs, causing rows to be stored under
	// gid:<group_id> while the bridge portal uses gid:<chat_id> or vice
	// versa. Without this, the mismatched rows survive and recreate the
	// portal on restart.
	if strings.HasPrefix(portalID, "gid:") {
		uuid := strings.TrimPrefix(portalID, "gid:")
		if _, err := s.db.Exec(ctx,
			`UPDATE cloud_chat SET deleted=TRUE, updated_ts=$3 WHERE login_id=$1 AND (LOWER(group_id)=LOWER($2) OR LOWER(cloud_chat_id)=LOWER($2)) AND deleted=FALSE`,
			s.loginID, uuid, nowMS,
		); err != nil {
			return fmt.Errorf("failed to soft-delete cloud_chat records by group_id %s: %w", uuid, err)
		}
		if _, err := s.db.Exec(ctx,
			`UPDATE cloud_message
			 SET deleted=TRUE,
			     text=NULL, subject=NULL, sender='',
			     tapback_emoji=NULL,
			     body_scrubbed=TRUE,
			     updated_ts=$3
			 WHERE login_id=$1 AND deleted=FALSE
			   AND portal_id IN (SELECT portal_id FROM cloud_chat WHERE login_id=$1 AND (LOWER(group_id)=LOWER($2) OR LOWER(cloud_chat_id)=LOWER($2)))`,
			s.loginID, uuid, nowMS,
		); err != nil {
			return fmt.Errorf("failed to soft-delete cloud_message records by group_id %s: %w", uuid, err)
		}
	}
	return nil
}

// undeleteCloudChatByPortalID clears the chat-level deleted flag without
// restoring transcript rows. Used when genuinely newer traffic revives a
// deleted chat: the chat shell becomes live again, while soft-deleted message
// rows continue to suppress stale UUID echoes.
func (s *cloudBackfillStore) undeleteCloudChatByPortalID(ctx context.Context, portalID string) error {
	if _, err := s.db.Exec(ctx,
		`UPDATE cloud_chat
		 SET deleted=FALSE, updated_ts=$3, fwd_backfill_done=0
		 WHERE login_id=$1 AND portal_id=$2 AND deleted=TRUE`,
		s.loginID, portalID, time.Now().UnixMilli(),
	); err != nil {
		return fmt.Errorf("failed to undelete cloud_chat for portal %s: %w", portalID, err)
	}
	return nil
}

// hardDeleteMessagesByPortalID permanently removes all cloud_message rows for
// a portal. Used during chat recovery to purge potentially stale rows before
// re-importing fresh messages from CloudKit. Unlike soft-delete (which preserves
// rows for echo detection), hard-delete is appropriate here because the fresh
// CloudKit fetch will re-populate the correct rows immediately after.
func (s *cloudBackfillStore) hardDeleteMessagesByPortalID(ctx context.Context, portalID string) (int64, error) {
	result, err := s.db.Exec(ctx,
		`DELETE FROM cloud_message WHERE login_id=$1 AND portal_id=$2`,
		s.loginID, portalID,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to hard-delete cloud_message rows for portal %s: %w", portalID, err)
	}
	n, _ := result.RowsAffected()
	return n, nil
}

// resetForwardBackfillDone unconditionally sets fwd_backfill_done=0 for all
// cloud_chat rows of a portal so forward backfill re-runs. Used during chat
// recovery where the cloud_chat may or may not be soft-deleted.
func (s *cloudBackfillStore) resetForwardBackfillDone(ctx context.Context, portalID string) error {
	_, err := s.db.Exec(ctx,
		`UPDATE cloud_chat SET fwd_backfill_done=0 WHERE login_id=$1 AND portal_id=$2`,
		s.loginID, portalID,
	)
	return err
}

// persistMessageUUID inserts a minimal cloud_message record for a realtime
// APNs message so the UUID survives restarts. CloudKit-synced messages are
// already stored via upsertMessageBatch; this covers the realtime path.
// Uses INSERT OR IGNORE so it's safe to call even if the message already exists.
func (s *cloudBackfillStore) persistMessageUUID(ctx context.Context, uuid, portalID string, timestampMS int64, isFromMe bool) error {
	nowMS := time.Now().UnixMilli()
	_, err := s.db.Exec(ctx, `
		INSERT OR IGNORE INTO cloud_message (login_id, guid, portal_id, timestamp_ms, is_from_me, created_ts, updated_ts)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, s.loginID, uuid, portalID, timestampMS, isFromMe, nowMS, nowMS)
	return err
}

// persistTapbackUUID inserts a minimal cloud_message record for a realtime APNs
// tapback so its UUID survives restarts. Unlike persistMessageUUID it sets
// tapback_type, ensuring getConversationReadByMe (which filters tapback_type IS
// NULL) does not treat the synthetic row as a substantive message and does not
// spuriously flip conversation read state for incoming reactions.
func (s *cloudBackfillStore) persistTapbackUUID(ctx context.Context, uuid, portalID string, timestampMS int64, isFromMe bool, tapbackType uint32) error {
	nowMS := time.Now().UnixMilli()
	_, err := s.db.Exec(ctx, `
		INSERT OR IGNORE INTO cloud_message (login_id, guid, portal_id, timestamp_ms, is_from_me, tapback_type, created_ts, updated_ts)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, s.loginID, uuid, portalID, timestampMS, isFromMe, tapbackType, nowMS, nowMS)
	return err
}

// hasMessageUUID checks if a message UUID exists in cloud_message for this login.
// Used for echo detection: if the UUID is known, the message is an echo of a
// previously-seen message and should not create a new portal.
func (s *cloudBackfillStore) hasMessageUUID(ctx context.Context, uuid string) (bool, error) {
	var count int
	// UPPER() on both sides: CloudKit GUIDs are lowercase, APNs UUIDs are
	// uppercase, and incoming SMS constant_uuid values may vary in case.
	// A case-sensitive match would miss cross-path duplicates (e.g. a message
	// CloudKit-backfilled as lowercase then re-delivered by APNs as uppercase).
	// Mirrors the pattern used in getMessageTimestampByGUID.
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM cloud_message WHERE login_id=$1 AND UPPER(guid)=UPPER($2) LIMIT 1`,
		s.loginID, uuid,
	).Scan(&count)
	return count > 0, err
}

// getMessageTimestampByGUID returns the Unix-millisecond send timestamp for a
// message UUID, and whether the row was found. Used to enforce the pre-startup
// receipt filter when the message is still being backfilled into the Matrix DB
// (so the Matrix DB lookup returns nothing but CloudKit already has the record).
// getMessageTextByGUID returns the text body of a message by UUID.
// Used when sending SMS/RCS reactions to include the original message text in the
// reaction string (e.g. "Loved "original message""). Returns "" if not found.
// Case-insensitive UUID comparison mirrors getMessageTimestampByGUID.
func (s *cloudBackfillStore) getMessageTextByGUID(ctx context.Context, uuid string) (string, error) {
	var text sql.NullString
	err := s.db.QueryRow(ctx,
		`SELECT text FROM cloud_message WHERE login_id=$1 AND UPPER(guid)=UPPER($2) AND tapback_type IS NULL LIMIT 1`,
		s.loginID, uuid,
	).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return text.String, nil
}

func (s *cloudBackfillStore) getMessageTimestampByGUID(ctx context.Context, uuid string) (int64, bool, error) {
	var ts int64
	// UPPER() on both sides: APNs delivers UUIDs as uppercase while CloudKit
	// GUIDs may be lowercase or mixed-case, so a case-sensitive = would miss them.
	err := s.db.QueryRow(ctx,
		`SELECT timestamp_ms FROM cloud_message WHERE login_id=$1 AND UPPER(guid)=UPPER($2) LIMIT 1`,
		s.loginID, uuid,
	).Scan(&ts)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	return ts, err == nil, err
}

// portalHasPreStartupOutgoingMessages returns true if the portal has any
// is_from_me messages with timestamp_ms < beforeMS. Used to detect portals
// that were backfilled this session: if the portal has outgoing messages
// predating startup, a live APNs read receipt arriving near startup is
// almost certainly a buffered re-delivery (APNs re-delivers them with
// TimestampMs = now on reconnect) rather than a genuine new read event.
func (s *cloudBackfillStore) portalHasPreStartupOutgoingMessages(ctx context.Context, portalID string, beforeMS int64) (bool, error) {
	var count int
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM cloud_message WHERE login_id=$1 AND portal_id=$2 AND is_from_me=TRUE AND timestamp_ms < $3 LIMIT 1`,
		s.loginID, portalID, beforeMS,
	).Scan(&count)
	return count > 0, err
}

// findPortalIDsByParticipants returns all distinct portal_ids from cloud_chat
// whose participants overlap with the given normalized participant list.
// Used to find duplicate group portals that have the same members but different
// group UUIDs. Participants are compared after normalization (tel:/mailto: prefix).
func (s *cloudBackfillStore) findPortalIDsByParticipants(ctx context.Context, normalizedTarget []string, isSelf func(string) bool) ([]string, error) {
	rows, err := s.db.Query(ctx,
		`SELECT DISTINCT portal_id, participants_json FROM cloud_chat WHERE login_id=$1 AND portal_id <> '' AND deleted=FALSE`,
		s.loginID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Build a set of target participants for fast lookup.
	targetSet := make(map[string]bool, len(normalizedTarget))
	for _, p := range normalizedTarget {
		targetSet[p] = true
	}

	var matches []string
	seen := make(map[string]bool)
	for rows.Next() {
		var portalID, participantsJSON string
		if err = rows.Scan(&portalID, &participantsJSON); err != nil {
			return nil, err
		}
		if seen[portalID] {
			continue
		}
		var participants []string
		if err = json.Unmarshal([]byte(participantsJSON), &participants); err != nil {
			continue
		}
		// Normalize and check overlap: match if all non-self participants overlap.
		normalized := make([]string, 0, len(participants))
		for _, p := range participants {
			n := normalizeIdentifierForPortalID(p)
			if n != "" {
				normalized = append(normalized, n)
			}
		}
		if participantSetsMatch(normalized, normalizedTarget, isSelf) {
			matches = append(matches, portalID)
			seen[portalID] = true
		}
	}
	return matches, rows.Err()
}

// participantSetsMatch checks if two normalized participant sets are equivalent
// (same members, ignoring order). Allows a difference of exactly 1 only if the
// single differing member is self (checked via isSelf predicate, which should
// test against all known user handles). Pass nil isSelf to disallow any difference.
func participantSetsMatch(a, b []string, isSelf func(string) bool) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	setA := make(map[string]bool, len(a))
	for _, p := range a {
		setA[p] = true
	}
	setB := make(map[string]bool, len(b))
	for _, p := range b {
		setB[p] = true
	}
	// Count members in A not in B, and vice versa; track whether ALL
	// differing members are self handles.
	allDiffAreSelf := true
	diff := 0
	for p := range setA {
		if !setB[p] {
			diff++
			if isSelf == nil || !isSelf(p) {
				allDiffAreSelf = false
			}
		}
	}
	for p := range setB {
		if !setA[p] {
			diff++
			if isSelf == nil || !isSelf(p) {
				allDiffAreSelf = false
			}
		}
	}
	return diff == 0 || (allDiffAreSelf && isSelf != nil)
}

// deleteLocalChatByGroupID removes all local cloud_chat and cloud_message records
// for any portal_id that shares the given group_id.
func (s *cloudBackfillStore) deleteLocalChatByGroupID(ctx context.Context, groupID string) error {
	// Find all portal_ids for this group
	rows, err := s.db.Query(ctx,
		`SELECT DISTINCT portal_id FROM cloud_chat WHERE login_id=$1 AND (LOWER(group_id)=LOWER($2) OR LOWER(cloud_chat_id)=LOWER($2)) AND portal_id <> ''`,
		s.loginID, groupID,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	var portalIDs []string
	for rows.Next() {
		var pid string
		if err = rows.Scan(&pid); err != nil {
			return err
		}
		portalIDs = append(portalIDs, pid)
	}
	if err = rows.Err(); err != nil {
		return err
	}

	for _, pid := range portalIDs {
		if err := s.deleteLocalChatByPortalID(ctx, pid); err != nil {
			return err
		}
	}
	return nil
}

// getOldestMessageTimestamp returns the oldest non-deleted message timestamp
// for a portal, or 0 if no messages exist.
func (s *cloudBackfillStore) getOldestMessageTimestamp(ctx context.Context, portalID string) (int64, error) {
	var ts sql.NullInt64
	err := s.db.QueryRow(ctx, `
		SELECT MIN(timestamp_ms)
		FROM cloud_message
		WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE
	`, s.loginID, portalID).Scan(&ts)
	if err != nil || !ts.Valid {
		return 0, err
	}
	return ts.Int64, nil
}

// getNewestMessageTimestamp returns the newest non-deleted message timestamp
// for a portal, or 0 if no messages exist.
func (s *cloudBackfillStore) getNewestMessageTimestamp(ctx context.Context, portalID string) (int64, error) {
	var ts sql.NullInt64
	err := s.db.QueryRow(ctx, `
		SELECT MAX(timestamp_ms)
		FROM cloud_message
		WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE
	`, s.loginID, portalID).Scan(&ts)
	if err != nil || !ts.Valid {
		return 0, err
	}
	return ts.Int64, nil
}

// getNewestBackfillableMessageTimestamp returns the newest timestamp for messages
// that FetchMessages can actually serve (deleted=FALSE, record_name <> ”).
// When requireContentful is true, rows must have text or attachments.
func (s *cloudBackfillStore) getNewestBackfillableMessageTimestamp(ctx context.Context, portalID string, requireContentful bool) (int64, error) {
	baseQuery := `
		SELECT MAX(timestamp_ms)
		FROM cloud_message
		WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE AND record_name <> ''
	`
	if requireContentful {
		// body_scrubbed rows had content at bridge time; treat them as contentful
		// so a fully-bridged-then-scrubbed portal's newest timestamp survives
		// (used by queueRecoveredPortalResync to gate ChatResync events).
		// tapback_type < 2000 (or NULL) keeps regular messages and excludes
		// scrubbed reactions (>= 2000), which aren't "content" in the same sense.
		baseQuery += " AND (COALESCE(text, '') <> '' OR COALESCE(attachments_json, '') <> '' OR (body_scrubbed = TRUE AND (tapback_type IS NULL OR tapback_type < 2000)))"
	}
	var ts sql.NullInt64
	err := s.db.QueryRow(ctx, baseQuery, s.loginID, portalID).Scan(&ts)
	if err != nil || !ts.Valid {
		return 0, err
	}
	return ts.Int64, nil
}

// healMisroutedGroupMessages fixes cloud_message rows that were incorrectly
// routed to the wrong portal (typically self-chat) due to the ";+;" CloudChatId
// routing bug. It uses two matching strategies:
//
//  1. cloud_chat_id match: the hex suffix after ";+;" in chat_id equals the
//     cloud_chat_id stored in cloud_chat (works when cloud_chat has a chat_id).
//
//  2. portal_id UUID match: the hex suffix after ";+;" equals the UUID part of
//     a "gid:<uuid>" portal_id (works for per-participant UUID portals whose
//     cloud_chat_id was seeded as empty).
//
// Both deleted and live cloud_chat rows are considered — messages should be
// assigned to their correct portal even if it's currently soft-deleted (it will
// be undeleted when the user runs !restore-chat).
// Returns the number of rows fixed.
func (s *cloudBackfillStore) healMisroutedGroupMessages(ctx context.Context) (int, error) {
	now := time.Now().UnixMilli()
	result, err := s.db.Exec(ctx, `
		UPDATE cloud_message AS m
		SET portal_id = cc.portal_id,
		    updated_ts = $2
		FROM cloud_chat cc
		WHERE m.login_id = $1
		  AND cc.login_id = $1
		  AND m.chat_id LIKE '%;+;%'
		  AND (
		    -- Strategy 1: cloud_chat_id matches the hex suffix after ";+;"
		    (cc.cloud_chat_id <> '' AND
		     LOWER(cc.cloud_chat_id) = LOWER(SUBSTR(m.chat_id, INSTR(m.chat_id, ';+;') + 3)))
		    OR
		    -- Strategy 2: portal_id is "gid:<uuid>" and uuid matches hex suffix
		    (cc.portal_id LIKE 'gid:%' AND
		     LOWER(SUBSTR(cc.portal_id, 5)) = LOWER(SUBSTR(m.chat_id, INSTR(m.chat_id, ';+;') + 3)))
		  )
		  AND m.portal_id <> cc.portal_id
		  AND cc.portal_id IS NOT NULL
		  AND cc.portal_id <> ''
	`, s.loginID, now)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()

	// Soft-delete cloud_chat rows for any portal that now has zero messages —
	// those portals were purely phantom portals created by the misrouting bug
	// (e.g. the self-chat). Keeping them live causes CloudKit sync to zombie
	// them back on the next resync. We only delete NON-gid: portals (DMs)
	// because a gid: portal with 0 messages might still need to be restored.
	if n > 0 {
		_, _ = s.db.Exec(ctx, `
			UPDATE cloud_chat
			SET deleted = TRUE, updated_ts = $2
			WHERE login_id = $1
			  AND deleted = FALSE
			  AND portal_id NOT LIKE 'gid:%'
			  AND portal_id IS NOT NULL AND portal_id <> ''
			  AND NOT EXISTS (
			    SELECT 1 FROM cloud_message
			    WHERE login_id = $1
			      AND portal_id = cloud_chat.portal_id
			      AND deleted = FALSE
			  )
		`, s.loginID, now)
	}

	return int(n), nil
}

func (s *cloudBackfillStore) hasPortalMessages(ctx context.Context, portalID string) (bool, error) {
	var count int
	err := s.db.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM cloud_message
		WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE AND record_name <> ''
	`, s.loginID, portalID).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// hasContentfulMessages checks if a portal has at least one non-deleted message
// with actual content (text or attachments). Seeded placeholder rows from the
// recycle bin have record_name but empty text/attachments — they don't count.
func (s *cloudBackfillStore) hasContentfulMessages(ctx context.Context, portalID string) (bool, error) {
	var count int
	err := s.db.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM cloud_message
		WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE AND record_name <> ''
		  AND (COALESCE(text, '') <> '' OR COALESCE(attachments_json, '') <> '' OR (body_scrubbed = TRUE AND (tapback_type IS NULL OR tapback_type < 2000)))
	`, s.loginID, portalID).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// countBackfillableMessages returns the number of rows FetchMessages can read
// for a portal (deleted=FALSE and record_name <> ”).
// When requireContentful is true, only rows with text or attachments count.
func (s *cloudBackfillStore) countBackfillableMessages(ctx context.Context, portalID string, requireContentful bool) (int, error) {
	query := `
		SELECT COUNT(*)
		FROM cloud_message
		WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE AND record_name <> ''
	`
	if requireContentful {
		query += " AND (COALESCE(text, '') <> '' OR COALESCE(attachments_json, '') <> '' OR (body_scrubbed = TRUE AND (tapback_type IS NULL OR tapback_type < 2000)))"
	}
	var count int
	if err := s.db.QueryRow(ctx, query, s.loginID, portalID).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

const cloudMessageSelectCols = `guid, COALESCE(chat_id, ''), portal_id, timestamp_ms, COALESCE(sender, ''), is_from_me,
	COALESCE(text, ''), COALESCE(subject, ''), COALESCE(service, ''), deleted,
	tapback_type, COALESCE(tapback_target_guid, ''), COALESCE(tapback_emoji, ''),
	COALESCE(attachments_json, ''), COALESCE(date_read_ms, 0), COALESCE(has_body, TRUE),
	COALESCE(body_scrubbed, FALSE)`

func (s *cloudBackfillStore) listBackwardMessages(
	ctx context.Context,
	portalID string,
	beforeTS int64,
	beforeGUID string,
	count int,
) ([]cloudMessageRow, error) {
	// Filter record_name <> '' to exclude stub rows from persistMessageUUID.
	query := `SELECT ` + cloudMessageSelectCols + `
		FROM cloud_message
		WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE AND record_name <> ''
	`
	args := []any{s.loginID, portalID}
	if beforeTS > 0 || beforeGUID != "" {
		query += ` AND (timestamp_ms < $3 OR (timestamp_ms = $3 AND guid < $4))`
		args = append(args, beforeTS, beforeGUID)
		query += ` ORDER BY timestamp_ms DESC, guid DESC LIMIT $5`
		args = append(args, count)
	} else {
		query += ` ORDER BY timestamp_ms DESC, guid DESC LIMIT $3`
		args = append(args, count)
	}
	return s.queryMessages(ctx, query, args...)
}

func (s *cloudBackfillStore) listForwardMessages(
	ctx context.Context,
	portalID string,
	afterTS int64,
	afterGUID string,
	count int,
) ([]cloudMessageRow, error) {
	// Filter record_name <> '' to exclude stub rows from persistMessageUUID.
	query := `SELECT ` + cloudMessageSelectCols + `
		FROM cloud_message
		WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE AND record_name <> ''
			AND (timestamp_ms > $3 OR (timestamp_ms = $3 AND guid > $4))
		ORDER BY timestamp_ms ASC, guid ASC
		LIMIT $5
	`
	return s.queryMessages(ctx, query, s.loginID, portalID, afterTS, afterGUID, count)
}

func (s *cloudBackfillStore) listLatestMessages(ctx context.Context, portalID string, count int) ([]cloudMessageRow, error) {
	// Filter record_name <> '' to exclude stub rows from persistMessageUUID
	// which have UUIDs for echo detection but no actual message content.
	query := `SELECT ` + cloudMessageSelectCols + `
		FROM cloud_message
		WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE AND record_name <> ''
		ORDER BY timestamp_ms DESC, guid DESC
		LIMIT $3
	`
	return s.queryMessages(ctx, query, s.loginID, portalID, count)
}

// listOldestMessages returns the oldest `count` non-deleted messages for a
// portal in chronological order (ASC). Used by forward backfill chunking to
// deliver messages starting from the beginning of conversation history.
func (s *cloudBackfillStore) listOldestMessages(ctx context.Context, portalID string, count int) ([]cloudMessageRow, error) {
	// Filter record_name <> '' to exclude stub rows from persistMessageUUID.
	query := `SELECT ` + cloudMessageSelectCols + `
		FROM cloud_message
		WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE AND record_name <> ''
		ORDER BY timestamp_ms ASC, guid ASC
		LIMIT $3
	`
	return s.queryMessages(ctx, query, s.loginID, portalID, count)
}

// listAllAttachmentMessages returns every non-deleted cloud_message row that
// has at least one attachment. Used by preUploadCloudAttachments to drive the
// pre-upload pass before portal creation.
func (s *cloudBackfillStore) listAllAttachmentMessages(ctx context.Context) ([]cloudMessageRow, error) {
	query := `SELECT ` + cloudMessageSelectCols + `
		FROM cloud_message
		WHERE login_id=$1
		  AND deleted=FALSE
		  AND attachments_json IS NOT NULL
		  AND attachments_json <> ''
		ORDER BY timestamp_ms ASC, guid ASC
	`
	return s.queryMessages(ctx, query, s.loginID)
}

func (s *cloudBackfillStore) queryMessages(ctx context.Context, query string, args ...any) ([]cloudMessageRow, error) {
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]cloudMessageRow, 0)
	for rows.Next() {
		var row cloudMessageRow
		if err = rows.Scan(
			&row.GUID,
			&row.CloudChatID,
			&row.PortalID,
			&row.TimestampMS,
			&row.Sender,
			&row.IsFromMe,
			&row.Text,
			&row.Subject,
			&row.Service,
			&row.Deleted,
			&row.TapbackType,
			&row.TapbackTargetGUID,
			&row.TapbackEmoji,
			&row.AttachmentsJSON,
			&row.DateReadMS,
			&row.HasBody,
			&row.BodyScrubbed,
		); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// portalWithNewestMessage pairs a portal ID with its newest message timestamp
// and message count. Used to prioritize portal creation during initial sync.
type portalWithNewestMessage struct {
	PortalID     string
	NewestTS     int64
	MessageCount int
}

// listPortalIDsWithNewestTimestamp returns all portal IDs from both messages
// and chat records, ordered by newest message timestamp descending (most
// recent activity first). Chat-only portals (no messages) are included with
// their updated_ts from the cloud_chat table so they still get portals created.
func (s *cloudBackfillStore) listPortalIDsWithNewestTimestamp(ctx context.Context) ([]portalWithNewestMessage, error) {
	rows, err := s.db.Query(ctx, `
		SELECT sub.portal_id, MAX(sub.newest_ts) AS newest_ts, SUM(sub.msg_count) AS msg_count FROM (
			SELECT portal_id, MAX(timestamp_ms) AS newest_ts, COUNT(*) AS msg_count
			FROM cloud_message
			WHERE login_id=$1 AND portal_id IS NOT NULL AND portal_id <> '' AND deleted=FALSE AND record_name <> ''
			GROUP BY portal_id

			UNION ALL

			SELECT cc.portal_id, COALESCE(cc.updated_ts, 0) AS newest_ts, 0 AS msg_count
			FROM cloud_chat cc
			WHERE cc.login_id=$1 AND cc.portal_id IS NOT NULL AND cc.portal_id <> ''
			AND COALESCE(cc.is_filtered, 0) = 0
			AND cc.deleted = FALSE
			AND cc.portal_id NOT IN (
				SELECT DISTINCT cm.portal_id FROM cloud_message cm
				WHERE cm.login_id=$1 AND cm.portal_id IS NOT NULL AND cm.portal_id <> '' AND cm.deleted=FALSE
			)
		) sub
		WHERE NOT EXISTS (
			SELECT 1 FROM cloud_chat fc
			WHERE fc.login_id=$1 AND fc.portal_id=sub.portal_id AND COALESCE(fc.is_filtered, 0) != 0
		)
		GROUP BY sub.portal_id
		ORDER BY newest_ts DESC
	`, s.loginID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []portalWithNewestMessage
	for rows.Next() {
		var p portalWithNewestMessage
		if err = rows.Scan(&p.PortalID, &p.NewestTS, &p.MessageCount); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// msgDebugPortalStat is one row returned by debugMessageStats.
type msgDebugPortalStat struct {
	PortalID      string
	Total         int
	FromMe        int
	NotFromMe     int
	EmptySender   int      // not-from-me rows with empty sender (filtered by cloudRowToBackfillMessages)
	SampleChats   []string // up to 5 distinct chat_ids seen for this portal
	SampleSenders []string // up to 5 distinct sender values for not-from-me rows
}

// debugMessageStats returns per-portal message statistics for the given
// primary portal_id AND any sibling portals that share the same group_id in
// cloud_chat. For DMs it only returns the one portal row.
// Used by the !msg-debug command.
func (s *cloudBackfillStore) debugMessageStats(ctx context.Context, portalID string) ([]msgDebugPortalStat, error) {
	// Step 1: collect all portal_ids to inspect — the given one plus siblings
	// that share the same group_id.
	siblingQuery := `
		SELECT DISTINCT cc2.portal_id
		FROM cloud_chat cc1
		JOIN cloud_chat cc2
		  ON cc2.login_id = cc1.login_id
		 AND cc2.group_id = cc1.group_id
		 AND cc2.group_id <> ''
		WHERE cc1.login_id=$1 AND cc1.portal_id=$2
	`
	sibRows, err := s.db.Query(ctx, siblingQuery, s.loginID, portalID)
	if err != nil {
		return nil, err
	}
	portals := []string{portalID}
	seenPortals := map[string]bool{portalID: true}
	for sibRows.Next() {
		var pid string
		if err = sibRows.Scan(&pid); err != nil {
			sibRows.Close()
			return nil, err
		}
		if !seenPortals[pid] {
			seenPortals[pid] = true
			portals = append(portals, pid)
		}
	}
	sibRows.Close()
	if err = sibRows.Err(); err != nil {
		return nil, err
	}

	// Step 2: for each portal, get message counts and sample chat_ids.
	var stats []msgDebugPortalStat
	for _, pid := range portals {
		var stat msgDebugPortalStat
		stat.PortalID = pid

		countErr := s.db.QueryRow(ctx, `
			SELECT
				COUNT(*),
				SUM(CASE WHEN is_from_me THEN 1 ELSE 0 END),
				SUM(CASE WHEN NOT is_from_me THEN 1 ELSE 0 END),
				SUM(CASE WHEN NOT is_from_me AND COALESCE(sender,'') = '' THEN 1 ELSE 0 END)
			FROM cloud_message
			WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE AND record_name <> ''
		`, s.loginID, pid).Scan(&stat.Total, &stat.FromMe, &stat.NotFromMe, &stat.EmptySender)
		if countErr != nil {
			continue
		}

		// Sample up to 5 distinct non-empty chat_ids from this portal.
		chatRows, chatErr := s.db.Query(ctx, `
			SELECT DISTINCT chat_id FROM cloud_message
			WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE AND COALESCE(chat_id,'') <> ''
			LIMIT 5
		`, s.loginID, pid)
		if chatErr == nil {
			for chatRows.Next() {
				var cid string
				if scanErr := chatRows.Scan(&cid); scanErr == nil {
					stat.SampleChats = append(stat.SampleChats, cid)
				}
			}
			chatRows.Close()
		}

		// Sample up to 5 distinct sender values for not-from-me rows.
		senderRows, senderErr := s.db.Query(ctx, `
			SELECT DISTINCT COALESCE(sender,'') FROM cloud_message
			WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE AND record_name <> '' AND NOT is_from_me
			LIMIT 5
		`, s.loginID, pid)
		if senderErr == nil {
			for senderRows.Next() {
				var snd string
				if scanErr := senderRows.Scan(&snd); scanErr == nil {
					stat.SampleSenders = append(stat.SampleSenders, snd)
				}
			}
			senderRows.Close()
		}

		stats = append(stats, stat)
	}
	return stats, nil
}

// debugFindPortalsByIdentifierSuffix scans cloud_message for portals whose
// stored chat_id ends with the given suffix (case-insensitive). This helps
// find DM messages that ended up under a different portal_id due to contact
// normalization differences (e.g. +19176138320 vs +1 917-613-8320).
// Returns up to 10 (portal_id, total_count) pairs ordered by count desc.
func (s *cloudBackfillStore) debugFindPortalsByIdentifierSuffix(ctx context.Context, suffix string) ([][2]string, error) {
	rows, err := s.db.Query(ctx, `
		SELECT portal_id, COUNT(*) as cnt
		FROM cloud_message
		WHERE login_id=$1 AND deleted=FALSE AND record_name <> ''
		  AND LOWER(COALESCE(chat_id,'')) LIKE '%' || LOWER($2)
		GROUP BY portal_id
		ORDER BY cnt DESC
		LIMIT 10
	`, s.loginID, suffix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var pid string
		var cnt int
		if err = rows.Scan(&pid, &cnt); err != nil {
			return nil, err
		}
		out = append(out, [2]string{pid, strconv.Itoa(cnt)})
	}
	return out, rows.Err()
}

// debugChatInfo returns cloud_chat metadata for a portal (whether the chat
// record has synced, and whether it's filtered/deleted). Used by !msg-debug
// to distinguish "chat not synced yet" from "chat synced but messages missing".
type debugChatInfo struct {
	Found       bool
	Deleted     bool
	IsFiltered  int64
	CloudChatID string
	GroupID     string
}

func (s *cloudBackfillStore) debugChatInfo(ctx context.Context, portalID string) (debugChatInfo, error) {
	var info debugChatInfo
	err := s.db.QueryRow(ctx, `
		SELECT cloud_chat_id, group_id, deleted, COALESCE(is_filtered, 0)
		FROM cloud_chat
		WHERE login_id=$1 AND portal_id=$2
		ORDER BY deleted ASC, length(cloud_chat_id) DESC
		LIMIT 1
	`, s.loginID, portalID).Scan(&info.CloudChatID, &info.GroupID, &info.Deleted, &info.IsFiltered)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return info, nil
		}
		return info, err
	}
	info.Found = true
	return info, nil
}

// debugTotalMessageCount returns the total number of non-deleted, non-stub
// cloud_message rows across ALL portals. Used by !msg-debug to show sync
// progress (how many messages have been ingested so far).
func (s *cloudBackfillStore) debugTotalMessageCount(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM cloud_message
		WHERE login_id=$1 AND deleted=FALSE AND record_name <> ''
	`, s.loginID).Scan(&count)
	return count, err
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// softDeletedPortal describes a portal that is still locally marked deleted
// and can be restored. Some portals only have soft-deleted cloud_chat rows
// (for example if the chat metadata was deleted before messages were imported),
// so restore-chat must not rely solely on cloud_message rows.
type softDeletedPortal struct {
	PortalID         string
	NewestTS         int64
	Count            int
	CloudChatID      string
	GroupID          string
	ParticipantsJSON string
}

// listSoftDeletedPortals returns portals whose cloud_chat rows are still
// soft-deleted with no live replacement. If message rows exist, they're
// included for ordering/counts, but chat-level deletion alone is enough to
// make the portal restorable.
func (s *cloudBackfillStore) listSoftDeletedPortals(ctx context.Context) ([]softDeletedPortal, error) {
	rows, err := s.db.Query(ctx, `
		WITH deleted_chats AS (
			SELECT
				portal_id,
				COALESCE(MAX(updated_ts), 0) AS chat_ts,
				MAX(cloud_chat_id) AS cloud_chat_id,
				MAX(group_id) AS group_id,
				MAX(participants_json) AS participants_json
			FROM cloud_chat
			WHERE login_id=$1 AND portal_id IS NOT NULL AND portal_id <> ''
			GROUP BY portal_id
			HAVING MAX(CASE WHEN deleted=TRUE THEN 1 ELSE 0 END) = 1
			   AND MAX(CASE WHEN deleted=FALSE THEN 1 ELSE 0 END) = 0
		),
		message_stats AS (
			SELECT
				portal_id,
				MAX(timestamp_ms) AS newest_ts,
				COUNT(*) AS msg_count,
				MAX(CASE WHEN deleted=FALSE THEN 1 ELSE 0 END) AS has_live,
				MAX(CASE WHEN deleted=TRUE THEN 1 ELSE 0 END) AS has_deleted
			FROM cloud_message
			WHERE login_id=$1 AND portal_id IS NOT NULL AND portal_id <> ''
			GROUP BY portal_id
		)
		SELECT
			dc.portal_id,
			COALESCE(ms.newest_ts, dc.chat_ts) AS newest_ts,
			COALESCE(ms.msg_count, 0) AS msg_count,
			COALESCE(dc.cloud_chat_id, '') AS cloud_chat_id,
			COALESCE(dc.group_id, '') AS group_id,
			COALESCE(dc.participants_json, '') AS participants_json
		FROM deleted_chats dc
		LEFT JOIN message_stats ms ON ms.portal_id=dc.portal_id
		WHERE COALESCE(ms.has_live, 0) = 0
		ORDER BY newest_ts DESC
	`, s.loginID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []softDeletedPortal
	for rows.Next() {
		var p softDeletedPortal
		if err = rows.Scan(&p.PortalID, &p.NewestTS, &p.Count, &p.CloudChatID, &p.GroupID, &p.ParticipantsJSON); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// cloudChatRecord holds the data needed to re-upload a chat record to CloudKit.
type cloudChatRecord struct {
	RecordName     string
	ChatIdentifier string
	GroupID        string
	Style          int64
	Service        string
	DisplayName    *string
	Participants   []string
}

// getCloudChatRecordByPortalID returns the full chat record data for a portal.
// Used by restore-chat to re-upload the record to CloudKit.
// Style is derived from the portal_id: gid: prefix = 43 (group), else 45 (DM).
func (s *cloudBackfillStore) getCloudChatRecordByPortalID(ctx context.Context, portalID string) (*cloudChatRecord, error) {
	var rec cloudChatRecord
	var displayName sql.NullString
	var participantsJSON string
	err := s.db.QueryRow(ctx, `
		SELECT record_name, cloud_chat_id, group_id, service, display_name, participants_json
		FROM cloud_chat WHERE login_id=$1 AND portal_id=$2 AND record_name <> '' LIMIT 1
	`, s.loginID, portalID).Scan(
		&rec.RecordName, &rec.ChatIdentifier, &rec.GroupID,
		&rec.Service, &displayName, &participantsJSON,
	)
	if err != nil {
		return nil, err
	}
	if displayName.Valid && displayName.String != "" {
		rec.DisplayName = &displayName.String
	}
	if participantsJSON != "" {
		_ = json.Unmarshal([]byte(participantsJSON), &rec.Participants)
	}
	// Derive style from portal_id: gid: = group (43), else DM (45)
	if strings.HasPrefix(portalID, "gid:") {
		rec.Style = 43
	} else {
		rec.Style = 45
	}
	return &rec, nil
}

// undeleteCloudMessagesByPortalID reverses a portal-level soft-delete by
// setting deleted=FALSE on all cloud_message rows for the portal.
// Returns the number of rows updated.
func (s *cloudBackfillStore) undeleteCloudMessagesByPortalID(ctx context.Context, portalID string) (int, error) {
	nowMS := time.Now().UnixMilli()
	// Un-soft-delete cloud_chat rows so GetChatInfo can resolve group name
	// and participants during the ChatResync that follows restore.
	if _, err := s.db.Exec(ctx,
		`UPDATE cloud_chat SET deleted=FALSE, updated_ts=$3 WHERE login_id=$1 AND portal_id=$2 AND deleted=TRUE`,
		s.loginID, portalID, nowMS,
	); err != nil {
		return 0, fmt.Errorf("failed to undelete cloud_chat for portal %s: %w", portalID, err)
	}

	// ALWAYS reset fwd_backfill_done regardless of deleted state. This ensures
	// forward backfill re-runs for the restored portal even if the cloud_chat
	// row wasn't soft-deleted (e.g., recover arrived before delete was persisted,
	// or the row was only tracked in-memory via recentlyDeletedPortals).
	if _, err := s.db.Exec(ctx,
		`UPDATE cloud_chat SET fwd_backfill_done=0 WHERE login_id=$1 AND portal_id=$2`,
		s.loginID, portalID,
	); err != nil {
		return 0, fmt.Errorf("failed to reset fwd_backfill_done for portal %s: %w", portalID, err)
	}

	result, err := s.db.Exec(ctx,
		`UPDATE cloud_message SET deleted=FALSE, updated_ts=$3
		 WHERE login_id=$1 AND portal_id=$2 AND deleted=TRUE`,
		s.loginID, portalID, nowMS,
	)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// clearBodyScrubByPortalID drops the body_scrubbed flag for every row in a
// portal so the next CloudKit upsert can repopulate text/subject/sender/
// tapback_emoji from Apple's source of truth. Called from the restore-chat
// pipeline before re-fetching messages: scrub protection in the upsert ON
// CONFLICT clause keeps NULL bodies sticky, which is correct for normal
// re-syncs but blocks user-triggered restore. Returns the number of rows
// updated.
//
// After restore completes and the messages are re-bridged, the periodic
// scrubBridgedBodies tick re-scrubs them (within bodyScrubInterval).
func (s *cloudBackfillStore) clearBodyScrubByPortalID(ctx context.Context, portalID string) (int, error) {
	// Bump updated_ts alongside the flag clear so the scrubber's grace
	// window predicate (updated_ts < now-graceWindow) skips these rows on
	// the next tick. Without this, a scrubber tick that races between
	// clearBodyScrubByPortalID and the restore's CloudKit upsert would
	// see body_scrubbed=FALSE + old updated_ts and re-scrub before upsert
	// could repopulate text.
	nowMS := time.Now().UnixMilli()
	result, err := s.db.Exec(ctx,
		`UPDATE cloud_message SET body_scrubbed=FALSE, updated_ts=$3
		 WHERE login_id=$1 AND portal_id=$2 AND body_scrubbed=TRUE`,
		s.loginID, portalID, nowMS,
	)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// clearAllBodyScrub drops the body_scrubbed flag from every non-deleted row
// for this login so the next CloudKit upsert can repopulate plaintext from
// Apple's source of truth. DEVELOPMENT-ONLY: used by the debug_disable_privacy
// re-fill path to undo the privacy scrubber's NULLing on a fresh full re-sync.
// Deleted rows are deliberately excluded so re-filling never resurrects content
// the user deleted/unsent (their body_scrubbed flag stays set, keeping the NULL
// sticky through the upsert ON CONFLICT clause). Returns the number of rows
// updated, which lets the caller skip the (expensive) sync-token reset once
// there is nothing left to re-fill. Bumps updated_ts so a racing scrubber tick's
// grace-window predicate skips these rows (the scrubber is disabled in this
// mode anyway, but this keeps the helper correct in isolation).
func (s *cloudBackfillStore) clearAllBodyScrub(ctx context.Context) (int64, error) {
	nowMS := time.Now().UnixMilli()
	result, err := s.db.Exec(ctx,
		`UPDATE cloud_message SET body_scrubbed=FALSE, updated_ts=$2
		 WHERE login_id=$1 AND body_scrubbed=TRUE AND deleted=FALSE`,
		s.loginID, nowMS,
	)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return n, nil
}

// seedChatFromRecycleBin inserts or updates a cloud_chat row with data from
// Apple's recycle bin. This ensures GetChatInfo can resolve group name,
// participants, and style even when the local cloud_chat table was wiped.
func (s *cloudBackfillStore) seedChatFromRecycleBin(ctx context.Context, portalID, chatID, groupID, displayName, groupPhotoGuid string, participants []string) {
	if chatID == "" {
		chatID = "recycle:" + portalID
	}
	nowMS := time.Now().UnixMilli()
	participantsJSON := "[]"
	if len(participants) > 0 {
		if b, err := json.Marshal(participants); err == nil {
			participantsJSON = string(b)
		}
	}
	var dnPtr *string
	if displayName != "" {
		dnPtr = &displayName
	}
	var photoPtr *string
	if groupPhotoGuid != "" {
		photoPtr = &groupPhotoGuid
	}
	_, _ = s.db.Exec(ctx, `
		INSERT INTO cloud_chat (login_id, cloud_chat_id, portal_id, group_id, display_name, group_photo_guid, participants_json, created_ts, updated_ts, deleted, fwd_backfill_done)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8, FALSE, 0)
		ON CONFLICT (login_id, cloud_chat_id) DO UPDATE SET
			portal_id=EXCLUDED.portal_id,
			display_name=COALESCE(EXCLUDED.display_name, cloud_chat.display_name),
			group_photo_guid=COALESCE(EXCLUDED.group_photo_guid, cloud_chat.group_photo_guid),
			participants_json=CASE WHEN COALESCE(cloud_chat.participants_json, '') IN ('', '[]') THEN EXCLUDED.participants_json ELSE cloud_chat.participants_json END,
			deleted=FALSE,
			updated_ts=EXCLUDED.updated_ts
	`, s.loginID, chatID, portalID, groupID, dnPtr, photoPtr, participantsJSON, nowMS)
}

// loadAttachmentCacheJSON returns every persisted record_name → content_json
// pair for this login. The caller deserialises the JSON into
// *event.MessageEventContent and populates the in-memory attachmentContentCache
// so pre-upload skips already-uploaded attachments without touching CloudKit.
func (s *cloudBackfillStore) loadAttachmentCacheJSON(ctx context.Context) (map[string][]byte, error) {
	rows, err := s.db.Query(ctx,
		`SELECT record_name, content_json FROM cloud_attachment_cache WHERE login_id=$1`,
		s.loginID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cache := make(map[string][]byte)
	for rows.Next() {
		var recordName string
		var contentJSON []byte
		if err := rows.Scan(&recordName, &contentJSON); err != nil {
			return nil, err
		}
		cache[recordName] = contentJSON
	}
	return cache, rows.Err()
}

// saveAttachmentCacheEntry persists a record_name → MessageEventContent JSON
// pair. Idempotent (upsert). Errors are silently ignored — the persistent cache
// is a best-effort optimisation; missing entries fall back to re-download.
func (s *cloudBackfillStore) saveAttachmentCacheEntry(ctx context.Context, recordName string, contentJSON []byte) {
	_, _ = s.db.Exec(ctx, `
		INSERT INTO cloud_attachment_cache (login_id, record_name, content_json, created_ts)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (login_id, record_name) DO UPDATE SET content_json=excluded.content_json
	`, s.loginID, recordName, contentJSON, time.Now().UnixMilli())
}

// markForwardBackfillDone marks all cloud_chat rows for portalID as having
// completed their initial forward FetchMessages call. Idempotent. Called from
// FetchMessages when the forward pass completes so that preUploadCloudAttachments
// skips this portal on the next restart instead of re-uploading every attachment.
//
// Self-healing: if the UPDATE hits 0 rows (no cloud_chat row matches this
// portal_id), a synthetic row is inserted so the flag persists. This covers
// APNs-created portals, CloudKit portal_id mismatches between cloud_message
// and cloud_chat, and any other case where a portal exists without a
// corresponding cloud_chat row.
func (s *cloudBackfillStore) markForwardBackfillDone(ctx context.Context, portalID string) {
	res, err := s.db.Exec(ctx,
		`UPDATE cloud_chat SET fwd_backfill_done=1 WHERE login_id=$1 AND portal_id=$2`,
		s.loginID, portalID,
	)

	// If the UPDATE failed (cancelled context, DB error) or matched 0 rows
	// (no cloud_chat entry for this portal), insert a synthetic row.
	// Use context.Background() — this MUST persist even during shutdown.
	needsInsert := err != nil || res == nil
	if !needsInsert {
		if rows, _ := res.RowsAffected(); rows == 0 {
			needsInsert = true
		}
	}
	if needsInsert {
		_, _ = s.db.Exec(context.Background(), `
			INSERT OR IGNORE INTO cloud_chat (login_id, cloud_chat_id, portal_id, created_ts, fwd_backfill_done)
			VALUES ($1, $2, $3, $4, 1)`,
			s.loginID, "synthetic:"+portalID, portalID, time.Now().UnixMilli(),
		)
	}
}

// isForwardBackfillDone returns true if forward backfill has completed for the
// given portal. Used by backward backfill to avoid permanently marking
// is_done=true before forward backfill has inserted the anchor message.
func (s *cloudBackfillStore) isForwardBackfillDone(ctx context.Context, portalID string) bool {
	var done bool
	err := s.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM cloud_chat WHERE login_id=$1 AND portal_id=$2 AND fwd_backfill_done=1)`,
		s.loginID, portalID,
	).Scan(&done)
	if err != nil {
		return false
	}
	return done
}

// getForwardBackfillDonePortals returns the set of portal IDs whose forward
// FetchMessages has completed at least once. Used by preUploadCloudAttachments
// to skip portals that don't need pre-upload on restart.
func (s *cloudBackfillStore) getForwardBackfillDonePortals(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.Query(ctx,
		`SELECT DISTINCT portal_id FROM cloud_chat WHERE login_id=$1 AND fwd_backfill_done=1`,
		s.loginID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	done := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		done[id] = true
	}
	return done, rows.Err()
}

// hasCloudReadReceipt checks whether a message UUID in cloud_message already
// has a valid date_read_ms (i.e., CloudKit recorded that it was read). Used to
// suppress duplicate APNs read receipts that arrive after backfill — the
// synthetic receipt already used the correct CloudKit timestamp, so the stale
// APNs receipt (with delivery-time instead of read-time) should be dropped.
// Uses case-insensitive GUID matching (APNs delivers uppercase, CloudKit may be mixed).
func (s *cloudBackfillStore) hasCloudReadReceipt(ctx context.Context, uuid string) (bool, error) {
	var dateReadMS int64
	err := s.db.QueryRow(ctx, `
		SELECT COALESCE(date_read_ms, 0)
		FROM cloud_message
		WHERE login_id=$1 AND UPPER(guid)=UPPER($2) AND is_from_me=TRUE
		LIMIT 1
	`, s.loginID, uuid).Scan(&dateReadMS)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return dateReadMS > 978307200000, nil
}

// isCloudBackfilledMessage checks whether a message UUID exists in the
// cloud_message table as a CloudKit-synced message. CloudKit entries (from
// upsertMessageBatch) have record_name populated, while real-time entries
// (from persistMessageUUID) have record_name=”. This distinguishes
// backfilled messages whose ghost receipts should be suppressed from
// real-time messages whose ghost receipts should go through.
func (s *cloudBackfillStore) isCloudBackfilledMessage(ctx context.Context, uuid string) (bool, error) {
	var exists bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM cloud_message
			WHERE login_id=$1 AND UPPER(guid)=UPPER($2) AND record_name != ''
		)
	`, s.loginID, uuid).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

// getConversationReadByMe returns true when the most recent non-tapback message
// in the conversation was sent by the user (is_from_me=true), meaning the user
// has read everything in the thread. If there are no messages in the local
// store, falls back to checking for a non-filtered cloud_chat row (portals with
// chat metadata but no stored messages are treated as read).
// Filtered (junk) chats and portals with no cloud_chat metadata are left unread.
//
// Must be called BEFORE markForwardBackfillDone (inserts synthetic rows).
func (s *cloudBackfillStore) getConversationReadByMe(ctx context.Context, portalID string) (bool, error) {
	// Primary check: direction of the most recent non-tapback message.
	// Reactions (tapback_type IS NOT NULL) are excluded: an incoming reaction
	// to something you sent does not create an unread state. The filter finds
	// the last substantive message and uses its direction as the read signal.
	var isFromMe bool
	err := s.db.QueryRow(ctx, `
		SELECT is_from_me FROM cloud_message
		WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE
		  AND tapback_type IS NULL
		ORDER BY timestamp_ms DESC, rowid DESC
		LIMIT 1
	`, s.loginID, portalID).Scan(&isFromMe)
	if err == nil {
		// Latest message direction determines read state:
		// outgoing → user has read the conversation; incoming → leave unread.
		return isFromMe, nil
	} else if err != sql.ErrNoRows {
		return false, err
	}
	// No messages in the local store — fall back to checking for a
	// non-filtered cloud_chat row. Portals with chat metadata but no
	// stored messages are treated as read.
	var count int
	err = s.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM cloud_chat
		WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE
		  AND COALESCE(is_filtered, 0) = 0
	`, s.loginID, portalID).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// pruneOrphanedAttachmentCache deletes cloud_attachment_cache entries whose
// record_name is not referenced by any live (non-deleted) cloud_message row.
// This prevents unbounded growth after portal deletions or message tombstones
// remove the messages that originally needed those cached attachments.
func (s *cloudBackfillStore) pruneOrphanedAttachmentCache(ctx context.Context) (int64, error) {
	result, err := s.db.Exec(ctx, `
		DELETE FROM cloud_attachment_cache
		WHERE login_id=$1
		  AND record_name NOT IN (
			SELECT DISTINCT json_extract(je.value, '$.record_name')
			FROM cloud_message, json_each(cloud_message.attachments_json) AS je
			WHERE cloud_message.login_id=$1
			  AND cloud_message.deleted=FALSE
			  AND cloud_message.attachments_json IS NOT NULL
			  AND cloud_message.attachments_json <> ''
			  AND json_extract(je.value, '$.record_name') IS NOT NULL
		  )
	`, s.loginID)
	if err != nil {
		return 0, fmt.Errorf("failed to prune orphaned attachment cache: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, nil
}

// scrubBridgedBodies nulls plaintext message content (text, subject, sender,
// tapback_emoji) on cloud_message rows whose corresponding Matrix event has
// been successfully delivered (an entry exists in bridgev2's `message` table)
// and which were ingested at least graceWindow ago. The grace window is a
// buffer against bridgev2 racing the scrubber on freshly-ingested messages.
//
// SMS reaction quoting (HandleMatrixReaction for IsSms portals quotes the
// original body as "Loved 'x'") reads cloud_message.text; once scrubbed it
// degrades to the unquoted reaction text, matching master's behavior when the
// body isn't locally available.
//
// Intentionally NOT scrubbed: attachments_json. It contains CloudKit
// record_names that AttachmentRetrier needs to repull files from Apple when
// a Matrix upload fails post-bridge, and that pruneOrphanedAttachmentCache
// uses to keep the mxc-URI cache in sync. The actual attachment bytes live
// in CloudKit; the JSON here is metadata pointing back to that source.
//
// Identifiers and routing columns (guid, record_name, timestamp_ms,
// is_from_me, portal_id, chat_id, service, tapback_type, tapback_target_guid,
// has_body, created_ts, date_read_ms, deleted, attachments_json) are retained.
//
// The body_scrubbed flag is preserved across CloudKit re-syncs by the
// upsert ON CONFLICT clause in upsertMessageBatch, so a re-sync of a
// previously-scrubbed message cannot re-populate the body. User-triggered
// restore-chat calls clearBodyScrubByPortalID first to bypass that gate
// so the fresh CloudKit fetch can repopulate text for re-bridging.
//
// Note on the grace window: `updated_ts` is the most-recent ingest time
// (bumped on every UPSERT, so restore-chat's CloudKit re-fetch resets it).
// Using updated_ts (not created_ts) gives the bridgev2 backfill pipeline a
// fresh 5-min window to read the re-populated text before the next
// scrubber tick wipes it again — the restore-chat race documented in the
// privacy audit. The membership check against bridgev2's `message` table is the
// load-bearing safety check; the timestamp window just buys backfill time.
//
// Chunked to avoid long-running DB locks on the first post-deploy run, which
// may scrub the entire backlog of already-delivered messages at once.
func (s *cloudBackfillStore) scrubBridgedBodies(ctx context.Context, bridgeID string, graceWindow time.Duration, excludePortals []string) (int64, error) {
	// DEVELOPMENT-ONLY: when privacy is disabled, leave plaintext in place.
	if debugDisablePrivacy {
		return 0, nil
	}
	cutoff := time.Now().Add(-graceWindow).UnixMilli()
	var total int64
	// Small chunks + a brief yield between chunks so the scrubber doesn't
	// contend with live upsertMessageBatch / APNs ingestion when the
	// first-boot backlog is large.
	const chunkSize = 1000

	// Build optional NOT IN clause for portals with active restore pipelines.
	// Large portals (50k+ messages) can take many minutes to backfill, and
	// the updated_ts grace window only buys ~5 min; without this exclusion
	// the scrubber would re-scrub partway through, and cloudRowToBackfillMessages'
	// BodyScrubbed skip would silently drop the un-backfilled tail.
	exclusionSQL := ""
	args := []any{s.loginID, cutoff, bridgeID}
	if len(excludePortals) > 0 {
		placeholders := make([]string, 0, len(excludePortals))
		for _, pid := range excludePortals {
			args = append(args, pid)
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
		}
		exclusionSQL = " AND portal_id NOT IN (" + strings.Join(placeholders, ",") + ")"
	}
	args = append(args, chunkSize)
	limitPlaceholder := fmt.Sprintf("$%d", len(args))

	// The outer UPDATE re-checks body_scrubbed=FALSE AND updated_ts < cutoff
	// at write time so concurrent upsert / clearBodyScrubByPortalID between
	// subquery eval and outer apply can't be silently overwritten. The
	// subquery picks candidate guids; the outer WHERE confirms the row's
	// state hasn't changed under us. Required because SQLite's IN-subquery
	// materializes the guid list once, then applies the UPDATE without
	// re-evaluating the predicate per row.
	// Match bridged rows by membership in the set of guids that have a `message`
	// row, computed ONCE per chunk, instead of a per-row correlated EXISTS with
	// UPPER()+LIKE (which can't use an index and ran ~25s over a 40k-row backlog,
	// tripping dbutil's 1s slow-query warning every chunk). bridgev2 stores the
	// base message id in `id`; part-suffixed ids (`<guid>_<part>`) are normalised
	// back to the base guid via substr-to-first-underscore (guids are UUIDs, no
	// underscores). UPPER() on both sides preserves the APNs-uppercase vs
	// CloudKit-mixed-case matching the EXISTS form had.
	query := `
		UPDATE cloud_message
		SET text=NULL,
		    subject=NULL,
		    sender='',
		    tapback_emoji=NULL,
		    body_scrubbed=TRUE
		WHERE login_id=$1
		  AND body_scrubbed=FALSE
		  AND updated_ts < $2
		  AND guid IN (
		    SELECT guid FROM cloud_message
		    WHERE login_id=$1
		      AND body_scrubbed=FALSE
		      AND (tapback_type IS NULL OR tapback_type < 2000)
		      AND updated_ts < $2
		      AND (
		        deleted=TRUE
		        OR UPPER(guid) IN (
		          SELECT UPPER(id) FROM message
		          WHERE bridge_id=$3 AND (room_receiver=$1 OR room_receiver='')
		          UNION
		          SELECT UPPER(substr(id, 1, instr(id, '_') - 1)) FROM message
		          WHERE bridge_id=$3 AND instr(id, '_') > 0
		            AND (room_receiver=$1 OR room_receiver='')
		        )
		      )` + exclusionSQL + `
		    LIMIT ` + limitPlaceholder + `
		  )
	`
	for {
		result, err := s.db.Exec(ctx, query, args...)
		if err != nil {
			return total, fmt.Errorf("failed to scrub bridged bodies: %w", err)
		}
		n, _ := result.RowsAffected()
		total += n
		if n < chunkSize {
			break
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return total, nil
}

// scrubReactionText nulls text/subject on reaction rows (tapback_type >= 2000),
// which store the SMS-style "Loved 'original message'" descriptor — leaking the
// quoted body. cloudTapbackToBackfill renders reactions purely from
// tapback_type, tapback_emoji, tapback_target_guid, and sender (it never reads
// text/subject), so dropping the text leaves a fully functional reaction —
// simple metadata only, like other bridges keep. No bridged/EXISTS gate is
// needed: bridged reactions live in bridgev2's `reaction` table (not `message`,
// so scrubBridgedBodies can't see them), and since nothing consumes the text,
// clearing it past the grace window is safe whether or not it was delivered.
// sender and tapback_emoji are preserved so re-backfill still attributes and
// renders the reaction (including custom emoji).
func (s *cloudBackfillStore) scrubReactionText(ctx context.Context, graceWindow time.Duration) (int64, error) {
	// DEVELOPMENT-ONLY: when privacy is disabled, leave plaintext in place.
	if debugDisablePrivacy {
		return 0, nil
	}
	cutoff := time.Now().Add(-graceWindow).UnixMilli()
	const chunkSize = 1000
	var total int64
	for {
		result, err := s.db.Exec(ctx, `
			UPDATE cloud_message
			SET text=NULL, subject=NULL, body_scrubbed=TRUE
			WHERE login_id=$1 AND tapback_type >= 2000
			  AND body_scrubbed=FALSE AND updated_ts < $2
			  AND guid IN (
			    SELECT guid FROM cloud_message
			    WHERE login_id=$1 AND tapback_type >= 2000
			      AND body_scrubbed=FALSE AND updated_ts < $2
			      AND (COALESCE(text, '') <> '' OR COALESCE(subject, '') <> '')
			    LIMIT $3
			  )
		`, s.loginID, cutoff, chunkSize)
		if err != nil {
			return total, fmt.Errorf("failed to scrub reaction text: %w", err)
		}
		n, _ := result.RowsAffected()
		total += n
		if n < chunkSize {
			break
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return total, nil
}

// scrubUnbridgedTail nulls plaintext on cloud_message rows that fall OUTSIDE
// the newest keepPerPortal messages of their portal — the tail that backfill
// can never reach. scrubBridgedBodies only clears rows already delivered to
// Matrix (its EXISTS gate), which leaves the bulk of a CloudKit-synced history
// (everything beyond the per-chat backfill cap) sitting in plaintext forever.
//
// Safe to call ONLY when the operator capped Backfill.MaxInitialMessages: in
// that mode FetchMessages short-circuits backward backfill to empty, so the
// sole reader of cloud_message content is the no-anchor forward path via
// listLatestMessages, which returns exactly the newest `count` rows ordered by
// (timestamp_ms DESC, guid DESC) over the non-deleted, contentful population.
// We mirror that: per portal we find the threshold timestamp (the
// keepPerPortal-th newest row) and scrub only rows strictly OLDER than it.
// Rows at the threshold timestamp are kept, so the retained set is always
// >= keepPerPortal and can never include a row forward backfill would deliver.
//
// New messages that arrived while the bridge was down become the NEWEST rows in
// their portal once CloudKit sync ingests them, so they sit above the threshold
// and are never scrubbed (their fresh updated_ts also keeps them inside the
// grace window) — they still backfill normally. Tapbacks are left alone for
// parity with scrubBridgedBodies; restore re-population still works because
// runRestoreBackfillPipeline clears body_scrubbed and re-fetches from CloudKit.
//
// Per-portal and indexed on (login_id, portal_id, timestamp_ms) so each
// statement stays well under dbutil's 1s slow-query log threshold — unlike a
// global window-function scan over the whole table.
func (s *cloudBackfillStore) scrubUnbridgedTail(ctx context.Context, keepPerPortal int, graceWindow time.Duration, excludePortals []string) (int64, error) {
	// DEVELOPMENT-ONLY: when privacy is disabled, leave plaintext in place.
	if debugDisablePrivacy {
		return 0, nil
	}
	if keepPerPortal <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-graceWindow).UnixMilli()
	exclude := make(map[string]struct{}, len(excludePortals))
	for _, pid := range excludePortals {
		exclude[pid] = struct{}{}
	}

	// Only portals whose contentful row count exceeds the cap have an
	// unreachable tail; the rest are entirely within the backfill window.
	rows, err := s.db.Query(ctx, `
		SELECT portal_id FROM cloud_message
		WHERE login_id=$1 AND deleted=FALSE AND record_name <> '' AND portal_id IS NOT NULL
		GROUP BY portal_id
		HAVING COUNT(*) > $2
	`, s.loginID, keepPerPortal)
	if err != nil {
		return 0, fmt.Errorf("list portals for tail scrub: %w", err)
	}
	var portals []string
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			rows.Close()
			return 0, err
		}
		portals = append(portals, pid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	const chunkSize = 1000
	var total int64
	for _, pid := range portals {
		if _, skip := exclude[pid]; skip {
			continue
		}
		// Threshold = timestamp of the keepPerPortal-th newest contentful row.
		// Walks keepPerPortal index entries; rows strictly older are unreachable.
		var threshold int64
		err := s.db.QueryRow(ctx, `
			SELECT timestamp_ms FROM cloud_message
			WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE AND record_name <> ''
			ORDER BY timestamp_ms DESC, guid DESC
			LIMIT 1 OFFSET $3
		`, s.loginID, pid, keepPerPortal-1).Scan(&threshold)
		if errors.Is(err, sql.ErrNoRows) {
			continue // fewer than keepPerPortal rows now (raced a deletion)
		} else if err != nil {
			return total, fmt.Errorf("tail-scrub threshold for portal %s: %w", pid, err)
		}
		for {
			result, err := s.db.Exec(ctx, `
				UPDATE cloud_message
				SET text=NULL, subject=NULL, sender='', tapback_emoji=NULL, body_scrubbed=TRUE
				WHERE login_id=$1 AND portal_id=$2
				  AND body_scrubbed=FALSE AND (tapback_type IS NULL OR tapback_type < 2000)
				  AND updated_ts < $3 AND timestamp_ms < $4
				  AND guid IN (
				    SELECT guid FROM cloud_message
				    WHERE login_id=$1 AND portal_id=$2
				      AND body_scrubbed=FALSE AND (tapback_type IS NULL OR tapback_type < 2000)
				      AND updated_ts < $3 AND timestamp_ms < $4
				    LIMIT $5
				  )
			`, s.loginID, pid, cutoff, threshold, chunkSize)
			if err != nil {
				return total, fmt.Errorf("scrub unbridged tail for portal %s: %w", pid, err)
			}
			n, _ := result.RowsAffected()
			total += n
			if n < chunkSize {
				break
			}
			select {
			case <-ctx.Done():
				return total, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	return total, nil
}

// orphanStubReapAge is how long a NULL-portal soft-delete stub must sit before
// deleteOrphanedMessages reaps it. softDeleteMessageByGUID stub-inserts a
// deleted=TRUE row with portal_id NULL when an Apple-side delete arrives for a
// guid CloudKit hasn't synced yet, so the future upsert preserves the deletion.
// A real CloudKit row would set portal_id non-NULL via the upsert; a stub still
// NULL after this window means the row never arrived and (since Apple already
// deleted the message) never will — only the in-flight sync that raced the
// delete could repopulate it, and that resolves within one sync cycle. 24h is
// far past that, so reaping here cannot strand the deletion or re-bridge a body.
const orphanStubReapAge = 24 * time.Hour

// deleteOrphanedMessages hard-deletes cloud_message rows that are already
// soft-deleted (deleted=TRUE) AND whose portal_id has no matching cloud_chat
// entry. This is conservative: DM portals legitimately have messages without
// cloud_chat rows, so we only clean up rows that are BOTH orphaned AND already
// marked deleted (from tombstone processing or portal deletion).
//
// NULL-portal stubs are a separate case: `portal_id NOT IN (...)` can't catch
// them because SQL's `NULL NOT IN (...)` is NULL (never TRUE), so they're
// reaped via the age-gated `portal_id IS NULL` branch instead.
func (s *cloudBackfillStore) deleteOrphanedMessages(ctx context.Context) (int64, error) {
	stubCutoff := time.Now().Add(-orphanStubReapAge).UnixMilli()
	result, err := s.db.Exec(ctx, `
		DELETE FROM cloud_message
		WHERE login_id=$1
		  AND deleted=TRUE
		  AND (
		    portal_id NOT IN (
		      SELECT DISTINCT portal_id FROM cloud_chat WHERE login_id=$1
		    )
		    OR (portal_id IS NULL AND created_ts < $2)
		  )
	`, s.loginID, stubCutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to delete orphaned messages: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, nil
}
