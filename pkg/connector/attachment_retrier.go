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
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"

	"github.com/lrhodin/corten-matrix/pkg/rustpushgo"
)

// attachmentRetrier replays MMCS attachment downloads that failed at push
// time. One instance per IMClient, spawned from Connect.
//
// The tick loop:
//
//  1. SELECT due rows from pending_attachment_retry.
//  2. For each row, try RetryMmcsFromDescriptor (Layer-1 retry applies
//     internally). On failure, consult the CloudKit backfill store for a
//     record_name and try safeCloudDownloadAttachment — this recovers the
//     "MMCS URL expired but CloudKit has the record" case.
//  3. On success, look up the notice-placeholder Matrix event in bridgev2's
//     message DB, queue a RemoteEventEdit that reuses convertAttachment for
//     all the HEIC/video/thumbnail processing, and delete the pending row.
//     Letting bridgev2 handle the m.replace dance keeps the ghost intent,
//     message row, and MXID tracking consistent with every other edit path.
//  4. On failure, bump attempt_count and reschedule with exponential backoff
//     (30s → 2m → 10m → 30m → 1h → 6h). Give up after MaxAttempts or
//     MaxLifetime and delete the row.
type attachmentRetrier struct {
	Client *IMClient
	log    zerolog.Logger
}

const (
	attachmentRetrierTickInterval = 10 * time.Second
	attachmentRetrierBatchLimit   = 50
	attachmentRetrierMaxAttempts  = 30
	attachmentRetrierMaxLifetime  = 7 * 24 * time.Hour
)

// Run blocks until stop is closed.
func (r *attachmentRetrier) Run(stop <-chan struct{}) {
	r.log = r.Client.UserLogin.Log.With().Str("component", "attachment_retrier").Logger()
	r.log.Info().Msg("Attachment retrier started")
	defer r.log.Info().Msg("Attachment retrier stopped")

	// First tick after a short delay so Connect finishes wiring everything.
	ticker := time.NewTicker(attachmentRetrierTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			r.tick(ctx)
			cancel()
		}
	}
}

func (r *attachmentRetrier) tick(ctx context.Context) {
	rows, err := r.Client.pendingAttachments.GetDue(ctx, time.Now(), attachmentRetrierBatchLimit)
	if err != nil {
		r.log.Err(err).Msg("Failed to fetch due pending attachments")
		return
	}
	if len(rows) == 0 {
		return
	}
	r.log.Debug().Int("count", len(rows)).Msg("Processing due pending attachments")
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		r.attempt(ctx, row)
	}
}

func (r *attachmentRetrier) attempt(ctx context.Context, row *pendingAttachmentRow) {
	log := r.log.With().
		Str("msg_guid", row.MessageGUID).
		Int("att_index", row.AttIndex).
		Str("file", row.Filename).
		Int("attempt", row.AttemptCount+1).
		Logger()

	// Stage 1: rustpush retry — reloads APS connection between tries and
	// re-runs the Layer-1 3-attempt backoff inside download_one_mmcs_attachment.
	data, err := r.downloadViaMMCS(ctx, row)
	if err != nil {
		log.Debug().Err(err).Msg("MMCS retry failed; falling back to CloudKit")
		// Stage 2: CloudKit fallback — useful when MMCS URL has expired but
		// CloudKit has caught up with the record.
		ckData, ckErr := r.downloadViaCloudKit(ctx, row)
		if ckErr != nil {
			log.Debug().AnErr("mmcs_err", err).AnErr("cloudkit_err", ckErr).
				Msg("Both MMCS and CloudKit downloads failed; rescheduling")
			r.reschedule(ctx, row, errors.Join(err, ckErr))
			return
		}
		data = ckData
	}

	if len(data) == 0 {
		log.Warn().Msg("Recovery download returned empty bytes; rescheduling")
		r.reschedule(ctx, row, errors.New("recovery returned empty bytes"))
		return
	}

	if err := r.deliverAsEdit(ctx, row, data); err != nil {
		log.Err(err).Msg("Failed to deliver recovered attachment")
		r.reschedule(ctx, row, err)
		return
	}
	if delErr := r.Client.pendingAttachments.Delete(ctx, row); delErr != nil {
		log.Warn().Err(delErr).Msg("Failed to delete pending row after success")
	}
	log.Info().Int("bytes", len(data)).Msg("Attachment recovered via background retry")
}

// downloadViaMMCS re-invokes the Rust wrapper's manual MMCS download path
// via retry_mmcs_from_descriptor. The call is wrapped in the same panic +
// timeout guard as other FFI calls to keep a single rustpush panic from
// killing the retrier goroutine.
func (r *attachmentRetrier) downloadViaMMCS(_ context.Context, row *pendingAttachmentRow) ([]byte, error) {
	if r.Client.client == nil {
		return nil, errors.New("rustpush client not ready")
	}
	type res struct {
		data []byte
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		var out res
		defer func() {
			if rec := recover(); rec != nil {
				out = res{err: errorFromRecovered("retry_mmcs_from_descriptor", rec)}
			}
			ch <- out
		}()
		d, e := r.Client.client.RetryMmcsFromDescriptor(row.MmcsDescriptor, row.Filename)
		out = res{data: d, err: e}
	}()
	select {
	case out := <-ch:
		return out.data, out.err
	case <-time.After(120 * time.Second):
		return nil, errors.New("retry_mmcs_from_descriptor timed out after 120s")
	}
}

// downloadViaCloudKit looks up the CloudKit record_name for (msg_guid,
// att_index) and downloads via safeCloudDownloadAttachment. Returns a
// not-ready error when CloudKit sync hasn't stored the row yet.
func (r *attachmentRetrier) downloadViaCloudKit(ctx context.Context, row *pendingAttachmentRow) ([]byte, error) {
	if r.Client.cloudStore == nil {
		return nil, errors.New("cloud store not initialized")
	}
	recordName, err := r.Client.cloudStore.getAttachmentRecordName(ctx, row.MessageGUID, row.AttIndex)
	if err != nil {
		return nil, err
	}
	if recordName == "" {
		return nil, errors.New("no CloudKit record yet for attachment")
	}
	data, err := safeCloudDownloadAttachment(r.Client.client, recordName)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errors.New("CloudKit returned empty data")
	}
	return data, nil
}

// deliverAsEdit queues a RemoteEventEdit via bridgev2 that replaces the
// notice placeholder with the real attachment content. Uses a ConvertEditFunc
// which constructs a synthetic attachmentMessage with the downloaded bytes,
// then runs it through convertAttachment so we share the entire
// HEIC/video/thumbnail/upload pipeline with the push-time path.
func (r *attachmentRetrier) deliverAsEdit(ctx context.Context, row *pendingAttachmentRow, data []byte) error {
	portalKey := networkid.PortalKey{
		ID:       networkid.PortalID(row.PortalID),
		Receiver: row.LoginID,
	}

	// Give up early if the notice placeholder is gone (e.g. user redacted it
	// or the portal was wiped). Without this check, bridgev2 would queue the
	// edit, find no target, and we'd keep looping forever.
	notice, err := r.Client.Main.Bridge.DB.Message.GetPartByID(
		ctx, row.LoginID,
		networkid.MessageID(row.AttID),
		networkid.PartID(attachmentPartID(row.AttIndex)),
	)
	if err != nil {
		return err
	}
	if notice == nil {
		r.log.Warn().
			Str("msg_guid", row.MessageGUID).
			Int("att_index", row.AttIndex).
			Msg("Notice placeholder no longer in bridgev2 DB; dropping pending row")
		_ = r.Client.pendingAttachments.Delete(ctx, row)
		return nil
	}

	// Build a synthetic attachmentMessage that carries the downloaded bytes
	// as inline data. convertAttachment accepts these just like the push-time
	// path (which treats MMCS attachments as inline once bytes are present).
	senderCopy := row.Sender
	synthMsg := &rustpushgo.WrappedMessage{
		Uuid:        row.MessageGUID,
		Sender:      &senderCopy,
		TimestampMs: uint64(row.TimestampMs),
	}
	synthAtt := &rustpushgo.WrappedAttachment{
		MimeType:   row.MimeType,
		Filename:   row.Filename,
		UtiType:    row.UtiType,
		Size:       uint64(row.SizeBytes),
		IsInline:   true,
		InlineData: &data,
	}
	synthAttMsg := &attachmentMessage{
		WrappedMessage: synthMsg,
		Attachment:     synthAtt,
		Index:          row.AttIndex,
	}

	videoTranscoding := r.Client.Main.Config.VideoTranscoding
	heicConversion := r.Client.Main.Config.HEICConversion
	heicQuality := r.Client.Main.Config.HEICJPEGQuality

	sender := r.Client.canonicalizeDMSender(portalKey, r.Client.makeEventSender(&senderCopy))
	ts := time.UnixMilli(row.TimestampMs)
	if ts.IsZero() || ts.Before(time.Unix(0, 0).Add(time.Hour)) {
		ts = time.Now()
	}

	r.Client.Main.Bridge.QueueRemoteEvent(r.Client.UserLogin, &simplevent.Message[*attachmentMessage]{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventEdit,
			PortalKey: portalKey,
			Sender:    sender,
			Timestamp: ts,
			LogContext: func(lc zerolog.Context) zerolog.Context {
				return lc.Str("attachment_recovery", row.AttID)
			},
		},
		// ID must differ from TargetMessage — bridgev2 uses the edit's own ID as
		// a dedup key for the edit event itself, while TargetMessage identifies
		// the notice placeholder being replaced. Include attempt count + millis
		// so every retry produces a distinct edit event, even if this retry
		// crashes mid-flight and the same row is picked up again.
		Data: synthAttMsg,
		ID: makeMessageID(row.AttID + "_l2recov_" +
			strconv.Itoa(row.AttemptCount) + "_" +
			strconv.FormatInt(time.Now().UnixMilli(), 10)),
		TargetMessage: makeMessageID(row.AttID),
		ConvertEditFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, existing []*database.Message, attMsg *attachmentMessage) (*bridgev2.ConvertedEdit, error) {
			cm, err := convertAttachment(ctx, portal, intent, attMsg, videoTranscoding, heicConversion, heicQuality)
			if err != nil {
				return nil, err
			}
			if cm == nil || len(cm.Parts) == 0 {
				return nil, errors.New("convertAttachment returned no parts")
			}
			// Match the existing parts with the converted parts by partID,
			// falling back to "use the last converted part as the edit".
			// convertAttachment emits one main part (id=attN) and optionally
			// one preview part (id=attN-preview); the main one is always last.
			target := findMatchingPart(existing, attMsg.Index)
			if target == nil && len(existing) > 0 {
				target = existing[0]
			}
			main := cm.Parts[len(cm.Parts)-1]
			return &bridgev2.ConvertedEdit{
				ModifiedParts: []*bridgev2.ConvertedEditPart{{
					Part:    target,
					Type:    main.Type,
					Content: main.Content,
					Extra:   main.Extra,
				}},
			}, nil
		},
	})
	return nil
}

func (r *attachmentRetrier) reschedule(ctx context.Context, row *pendingAttachmentRow, cause error) {
	row.AttemptCount++
	row.LastAttemptAt = time.Now()
	if row.AttemptCount >= attachmentRetrierMaxAttempts || time.Since(row.CreatedAt) > attachmentRetrierMaxLifetime {
		r.log.Warn().Err(cause).
			Str("msg_guid", row.MessageGUID).
			Int("att_index", row.AttIndex).
			Int("attempts", row.AttemptCount).
			Msg("Giving up on pending attachment retry")
		_ = r.Client.pendingAttachments.Delete(ctx, row)
		return
	}
	row.NextAttemptAt = time.Now().Add(attachmentRetrierBackoff(row.AttemptCount))
	if err := r.Client.pendingAttachments.UpdateSchedule(ctx, row); err != nil {
		r.log.Warn().Err(err).
			Str("msg_guid", row.MessageGUID).
			Msg("Failed to update pending row schedule")
	}
}

func attachmentRetrierBackoff(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 30 * time.Second
	case 2:
		return 2 * time.Minute
	case 3:
		return 10 * time.Minute
	case 4:
		return 30 * time.Minute
	case 5:
		return 1 * time.Hour
	default:
		return 6 * time.Hour
	}
}

// findMatchingPart returns the existing notice-placeholder row whose PartID
// matches "att{idx}", so bridgev2 edits the right row instead of guessing.
func findMatchingPart(existing []*database.Message, idx int) *database.Message {
	wantID := networkid.PartID(attachmentPartID(idx))
	for _, m := range existing {
		if m != nil && m.PartID == wantID {
			return m
		}
	}
	return nil
}

func attachmentPartID(idx int) string {
	return fmt.Sprintf("att%d", idx)
}

// makeAttID mirrors the ID-construction rule in IMClient.handleMessage:
// the first attachment of a text-less message uses the raw UUID; every
// other attachment gets "UUID_attN". Must stay in sync with that site —
// otherwise the notice placeholder and the recovery edit target different
// bridgev2 message rows.
func makeAttID(uuid string, index int, hasText bool) string {
	if index == 0 && !hasText {
		return uuid
	}
	return fmt.Sprintf("%s_att%d", uuid, index)
}

// enqueuePendingMMCSRecovery persists an entry in pending_attachment_retry
// for a MMCS attachment whose push-time download exhausted the Layer-1
// retries. Called from the ConvertMessageFunc closure that wraps
// convertAttachment — the guard there matches IsInline==false plus a nil
// InlineData and a present MmcsDescriptorJson, which is exactly the shape
// download_mmcs_attachments leaves on failure.
func (c *IMClient) enqueuePendingMMCSRecovery(ctx context.Context, portal *bridgev2.Portal, attMsg *attachmentMessage) {
	if c.pendingAttachments == nil || portal == nil {
		return
	}
	att := attMsg.Attachment
	if att == nil || att.MmcsDescriptorJson == nil || *att.MmcsDescriptorJson == "" {
		return
	}

	hasText := attMsg.WrappedMessage != nil && attMsg.WrappedMessage.Text != nil &&
		strings.TrimRight(*attMsg.WrappedMessage.Text, "\ufffc \n") != ""
	attID := makeAttID(attMsg.Uuid, attMsg.Index, hasText)

	sender := ""
	if attMsg.WrappedMessage != nil && attMsg.WrappedMessage.Sender != nil {
		sender = *attMsg.WrappedMessage.Sender
	}
	ts := int64(0)
	if attMsg.WrappedMessage != nil {
		ts = int64(attMsg.WrappedMessage.TimestampMs)
	}

	now := time.Now()
	row := &pendingAttachmentRow{
		LoginID:        c.UserLogin.ID,
		MessageGUID:    attMsg.Uuid,
		AttIndex:       attMsg.Index,
		AttID:          attID,
		PortalID:       string(portal.ID),
		Sender:         sender,
		TimestampMs:    ts,
		Filename:       att.Filename,
		MimeType:       att.MimeType,
		UtiType:        att.UtiType,
		SizeBytes:      int64(att.Size),
		MmcsDescriptor: *att.MmcsDescriptorJson,
		CreatedAt:      now,
		LastAttemptAt:  time.Time{},
		AttemptCount:   0,
		NextAttemptAt:  now.Add(attachmentRetrierBackoff(1)),
	}
	// Bounded retry on transient DB failures (SQLITE_BUSY, locked). A failed
	// Insert means the notice placeholder ships but nothing will ever edit it
	// into the real attachment, so make a best effort before giving up.
	var insertErr error
	for attempt := 1; attempt <= 3; attempt++ {
		insertErr = c.pendingAttachments.Insert(ctx, row)
		if insertErr == nil {
			break
		}
		if ctx.Err() != nil {
			break
		}
		time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
	}
	if insertErr != nil {
		zerolog.Ctx(ctx).Err(insertErr).
			Str("msg_guid", attMsg.Uuid).
			Int("att_index", attMsg.Index).
			Str("file", att.Filename).
			Msg("Failed to enqueue MMCS attachment for background retry (notice will stick)")
		return
	}
	zerolog.Ctx(ctx).Info().
		Str("msg_guid", attMsg.Uuid).
		Int("att_index", attMsg.Index).
		Str("file", att.Filename).
		Str("att_id", attID).
		Msg("Enqueued MMCS attachment for background retry")
}

// errorFromRecovered formats a recovered panic into an error without pulling
// in the full debug stack — which is already logged by safeCloudDownloadAttachment
// for the CloudKit-side panics.
func errorFromRecovered(ffiMethod string, rec any) error {
	return fmt.Errorf("FFI panic in %s: %v", ffiMethod, rec)
}
