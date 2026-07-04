// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// Layer-2 MMCS attachment recovery. When the Rust-side Layer-1 retry wrapper
// (pkg/rustpushgo/src/lib.rs `download_one_mmcs_attachment`) exhausts its
// 3 attempts for a live APNs-delivered attachment, the Go connector records
// the failure in `pending_attachment_retry` and sends a notice placeholder to
// the Matrix room. AttachmentRetrier drains the table on a timer, re-invoking
// the rustpush download (which re-runs Layer 1 with a freshly reloaded APS
// connection) and falling back to CloudKit if the MMCS URL has expired.
// On success the retrier edits the placeholder into the real m.image/m.video.

// pendingAttachmentRow is a single row in pending_attachment_retry — one per
// MMCS attachment whose push-time download failed and needs background
// recovery. The store is per-UserLogin (login_id is implicit).
type pendingAttachmentRow struct {
	LoginID        networkid.UserLoginID
	MessageGUID    string // Parent iMessage message UUID
	AttIndex       int    // 0-based attachment index within the message
	AttID          string // bridgev2 MessageID (guid or guid_attN)
	PortalID       string // networkid.PortalID of the room
	Sender         string // iMessage sender identifier; empty means IsFromMe
	TimestampMs    int64  // original message timestamp; used when queuing the edit
	Filename       string
	MimeType       string
	UtiType        string
	SizeBytes      int64
	MmcsDescriptor string // JSON (see MmcsDescriptor in lib.rs)
	CreatedAt      time.Time
	LastAttemptAt  time.Time // zero value means "never retried"
	AttemptCount   int
	NextAttemptAt  time.Time
}

type pendingAttachmentStore struct {
	db      *dbutil.Database
	loginID networkid.UserLoginID
}

func newPendingAttachmentStore(db *dbutil.Database, loginID networkid.UserLoginID) *pendingAttachmentStore {
	return &pendingAttachmentStore{db: db, loginID: loginID}
}

func (s *pendingAttachmentStore) ensureSchema(ctx context.Context) error {
	if _, err := s.db.Exec(ctx, `CREATE TABLE IF NOT EXISTS pending_attachment_retry (
		login_id          TEXT    NOT NULL,
		message_guid      TEXT    NOT NULL,
		att_index         INTEGER NOT NULL,
		att_id            TEXT    NOT NULL,
		portal_id         TEXT    NOT NULL,
		sender            TEXT    NOT NULL DEFAULT '',
		timestamp_ms      BIGINT  NOT NULL DEFAULT 0,
		filename          TEXT    NOT NULL,
		mime_type         TEXT    NOT NULL,
		uti_type          TEXT    NOT NULL DEFAULT '',
		size_bytes        BIGINT  NOT NULL,
		mmcs_descriptor   TEXT    NOT NULL,
		created_at        BIGINT  NOT NULL,
		last_attempt_at   BIGINT  NOT NULL,
		attempt_count     INTEGER NOT NULL DEFAULT 0,
		next_attempt_at   BIGINT  NOT NULL,
		PRIMARY KEY (login_id, message_guid, att_index)
	)`); err != nil {
		return fmt.Errorf("failed to create pending_attachment_retry table: %w", err)
	}
	if _, err := s.db.Exec(ctx, `CREATE INDEX IF NOT EXISTS pending_attachment_retry_due_idx
		ON pending_attachment_retry (login_id, next_attempt_at)`); err != nil {
		return fmt.Errorf("failed to create pending_attachment_retry due idx: %w", err)
	}
	return nil
}

// Insert adds a row. On PK conflict (same guid+att_index arriving twice —
// can happen when a stored APNs message replays after a reconnect) the
// existing row wins so we preserve attempt_count + created_at + next_attempt_at.
// Resetting those would let a row on attempt 25 start over at 0 every time
// the bridge restarts, defeating the give-up after-30 guard.
func (s *pendingAttachmentStore) Insert(ctx context.Context, row *pendingAttachmentRow) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO pending_attachment_retry (
			login_id, message_guid, att_index, att_id, portal_id,
			sender, timestamp_ms,
			filename, mime_type, uti_type, size_bytes, mmcs_descriptor,
			created_at, last_attempt_at, attempt_count, next_attempt_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (login_id, message_guid, att_index) DO NOTHING`,
		s.loginID, row.MessageGUID, row.AttIndex, row.AttID, row.PortalID,
		row.Sender, row.TimestampMs,
		row.Filename, row.MimeType, row.UtiType, row.SizeBytes, row.MmcsDescriptor,
		row.CreatedAt.UnixMilli(), row.LastAttemptAt.UnixMilli(),
		row.AttemptCount, row.NextAttemptAt.UnixMilli(),
	)
	return err
}

// GetDue returns up to `limit` rows whose next_attempt_at <= now, oldest first.
func (s *pendingAttachmentStore) GetDue(ctx context.Context, now time.Time, limit int) ([]*pendingAttachmentRow, error) {
	rows, err := s.db.Query(ctx, `
		SELECT login_id, message_guid, att_index, att_id, portal_id,
		       sender, timestamp_ms,
		       filename, mime_type, uti_type, size_bytes, mmcs_descriptor,
		       created_at, last_attempt_at, attempt_count, next_attempt_at
		FROM pending_attachment_retry
		WHERE login_id = ? AND next_attempt_at <= ?
		ORDER BY next_attempt_at ASC
		LIMIT ?`,
		s.loginID, now.UnixMilli(), limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pendingAttachmentRow
	for rows.Next() {
		r, err := scanPendingAttachmentRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateSchedule bumps attempt_count, last_attempt_at, and next_attempt_at.
func (s *pendingAttachmentStore) UpdateSchedule(ctx context.Context, row *pendingAttachmentRow) error {
	_, err := s.db.Exec(ctx, `
		UPDATE pending_attachment_retry
		SET attempt_count = ?, last_attempt_at = ?, next_attempt_at = ?
		WHERE login_id = ? AND message_guid = ? AND att_index = ?`,
		row.AttemptCount, row.LastAttemptAt.UnixMilli(), row.NextAttemptAt.UnixMilli(),
		s.loginID, row.MessageGUID, row.AttIndex,
	)
	return err
}

// Delete removes a row after success or final give-up.
func (s *pendingAttachmentStore) Delete(ctx context.Context, row *pendingAttachmentRow) error {
	_, err := s.db.Exec(ctx, `
		DELETE FROM pending_attachment_retry
		WHERE login_id = ? AND message_guid = ? AND att_index = ?`,
		s.loginID, row.MessageGUID, row.AttIndex,
	)
	return err
}

// GetOne is useful for manual inspection/debug.
func (s *pendingAttachmentStore) GetOne(ctx context.Context, msgGUID string, attIndex int) (*pendingAttachmentRow, error) {
	row := s.db.QueryRow(ctx, `
		SELECT login_id, message_guid, att_index, att_id, portal_id,
		       sender, timestamp_ms,
		       filename, mime_type, uti_type, size_bytes, mmcs_descriptor,
		       created_at, last_attempt_at, attempt_count, next_attempt_at
		FROM pending_attachment_retry
		WHERE login_id = ? AND message_guid = ? AND att_index = ?`,
		s.loginID, msgGUID, attIndex,
	)
	r, err := scanPendingAttachmentRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// scannable unifies *sql.Row and *sql.Rows for row-by-row decoding.
type scannable interface {
	Scan(dest ...any) error
}

func scanPendingAttachmentRow(sc scannable) (*pendingAttachmentRow, error) {
	r := &pendingAttachmentRow{}
	var createdMs, lastMs, nextMs int64
	if err := sc.Scan(
		&r.LoginID, &r.MessageGUID, &r.AttIndex, &r.AttID, &r.PortalID,
		&r.Sender, &r.TimestampMs,
		&r.Filename, &r.MimeType, &r.UtiType, &r.SizeBytes, &r.MmcsDescriptor,
		&createdMs, &lastMs, &r.AttemptCount, &nextMs,
	); err != nil {
		return nil, err
	}
	r.CreatedAt = time.UnixMilli(createdMs)
	if lastMs > 0 {
		r.LastAttemptAt = time.UnixMilli(lastMs)
	}
	r.NextAttemptAt = time.UnixMilli(nextMs)
	return r, nil
}
