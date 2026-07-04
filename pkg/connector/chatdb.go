// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/ffmpeg"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/lrhodin/corten-matrix/imessage"
)

// chatDB wraps the macOS chat.db iMessage API for backfill and contact
// resolution. It does NOT listen for incoming messages (rustpush handles that).
type chatDB struct {
	api imessage.API
}

// openChatDB attempts to open the local iMessage chat.db database.
// Returns nil if chat.db is not accessible (e.g., no Full Disk Access).
func openChatDB(log zerolog.Logger) *chatDB {
	if !canReadChatDB(log) {
		showDialogAndOpenFDA(log)
		waitForFDA(log)
	}

	adapter := newBridgeAdapter(&log)
	api, err := imessage.NewAPI(adapter)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to initialize chat.db API via imessage.NewAPI")
		return nil
	}

	return &chatDB{api: api}
}

// Close stops the chat.db API.
func (db *chatDB) Close() {
	if db.api != nil {
		db.api.Stop()
	}
}

// findGroupChatGUID finds a group chat GUID by matching the portal's members.
// The portalID is comma-separated members like "tel:+1555...,tel:+1555...".
func (db *chatDB) findGroupChatGUID(portalID string, c *IMClient) string {
	// Parse members from portal ID (lowercase for case-insensitive matching)
	portalMembers := strings.Split(portalID, ",")
	portalMemberSet := make(map[string]struct{})
	for _, m := range portalMembers {
		portalMemberSet[strings.ToLower(stripIdentifierPrefix(m))] = struct{}{}
	}

	// Search all group chats
	chats, err := db.api.GetChatsWithMessagesAfter(time.Time{})
	if err != nil {
		return ""
	}

	for _, chat := range chats {
		parsed := imessage.ParseIdentifier(chat.ChatGUID)
		if !parsed.IsGroup {
			continue
		}
		info, err := db.api.GetChatInfo(chat.ChatGUID, chat.ThreadID)
		if err != nil || info == nil {
			continue
		}

		// Build member set from chat.db (add self, lowercase for case-insensitive matching)
		chatMemberSet := make(map[string]struct{})
		chatMemberSet[strings.ToLower(stripIdentifierPrefix(c.handle))] = struct{}{}
		for _, m := range info.Members {
			chatMemberSet[strings.ToLower(stripIdentifierPrefix(m))] = struct{}{}
		}

		// Check if members match
		if len(chatMemberSet) == len(portalMemberSet) {
			match := true
			for m := range portalMemberSet {
				if _, ok := chatMemberSet[m]; !ok {
					match = false
					break
				}
			}
			if match {
				return chat.ChatGUID
			}
		}
	}
	return ""
}

// chatDBReplyTarget returns the correct MessageOptionalPartID for a reply,
// mapping chat.db balloon-part index to the emitted part IDs:
// bp<=0 -> base GUID (text body); bp>=1 -> {guid}_att{bp-1} (attachment).
// Negative part values are normalized to 0 (base-message semantics).
func chatDBReplyTarget(replyGUID string, replyPart int) *networkid.MessageOptionalPartID {
	targetID := replyGUID
	if replyPart >= 1 {
		targetID = fmt.Sprintf("%s_att%d", replyGUID, replyPart-1)
	}
	return &networkid.MessageOptionalPartID{MessageID: makeMessageID(targetID)}
}

// FetchMessages retrieves historical messages from chat.db for backfill.
func (db *chatDB) FetchMessages(ctx context.Context, params bridgev2.FetchMessagesParams, c *IMClient) (*bridgev2.FetchMessagesResponse, error) {
	portalID := string(params.Portal.ID)
	log := zerolog.Ctx(ctx)

	var chatGUIDs []string
	if strings.Contains(portalID, ",") {
		// Group portal: find chat GUID by matching members
		chatGUID := db.findGroupChatGUID(portalID, c)
		if chatGUID != "" {
			chatGUIDs = []string{chatGUID}
		}
	} else {
		// Use contact-aware lookup: includes chat GUIDs for all of the
		// contact's phone numbers, so merged DM portals get complete history.
		chatGUIDs = c.getContactChatGUIDs(portalID)
	}

	log.Info().Str("portal_id", portalID).Strs("chat_guids", chatGUIDs).Bool("forward", params.Forward).Msg("FetchMessages called")

	if len(chatGUIDs) == 0 {
		log.Warn().Str("portal_id", portalID).Msg("Could not find chat GUID for portal")
		return &bridgev2.FetchMessagesResponse{HasMore: false, Forward: params.Forward}, nil
	}

	count := params.Count
	if count <= 0 {
		count = 50
	}

	// Fetch messages from ALL chat GUIDs and merge. For contacts with multiple
	// phone numbers, this combines messages from all numbers into one timeline.
	// Also tries multiple GUID formats (any/iMessage/SMS) per phone number.
	var messages []*imessage.Message
	var lastErr error

	// Respect the configured message cap. params.Count comes from the
	// framework's MaxInitialMessages setting.
	maxMessages := c.Main.Bridge.Config.Backfill.MaxInitialMessages

	for _, chatGUID := range chatGUIDs {
		var msgs []*imessage.Message
		if params.AnchorMessage != nil {
			if params.Forward {
				msgs, lastErr = db.api.GetMessagesSinceDate(chatGUID, params.AnchorMessage.Timestamp, "")
			} else {
				msgs, lastErr = db.api.GetMessagesBeforeWithLimit(chatGUID, params.AnchorMessage.Timestamp, count)
			}
		} else {
			// Fetch the most recent N messages (uncapped = MaxInt32, effectively all).
			msgs, lastErr = db.api.GetMessagesBeforeWithLimit(chatGUID, time.Now().Add(time.Minute), maxMessages)
		}
		if lastErr == nil {
			messages = append(messages, msgs...)
		}
	}

	if len(messages) == 0 && lastErr != nil {
		log.Error().Err(lastErr).Strs("chat_guids", chatGUIDs).Msg("Failed to fetch messages from chat.db")
		return nil, fmt.Errorf("failed to fetch messages from chat.db: %w", lastErr)
	}

	// Sort chronologically — messages may come from multiple chat GUIDs
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].Time.Before(messages[j].Time)
	})

	log.Info().Strs("chat_guids", chatGUIDs).Int("raw_message_count", len(messages)).Msg("Got messages from chat.db")

	// Get an intent for uploading media. The bot intent works for all uploads.
	intent := c.Main.Bridge.Bot

	backfillMessages := make([]*bridgev2.BackfillMessage, 0, len(messages))
	for _, msg := range messages {
		if msg.ItemType != imessage.ItemTypeMessage || msg.Tapback != nil {
			continue
		}
		sender := chatDBMakeEventSender(msg, c)
		if sender.Sender == "" && !sender.IsFromMe {
			continue
		}
		sender = c.canonicalizeDMSender(params.Portal.PortalKey, sender)

		// Strip U+FFFC (object replacement character) — inline attachment
		// placeholders from NSAttributedString that render as blank
		msg.Text = strings.ReplaceAll(msg.Text, "\uFFFC", "")
		msg.Text = strings.TrimSpace(msg.Text)

		// Only create a text part if there's actual text content
		if msg.Text != "" || msg.Subject != "" {
			cm, err := convertChatDBMessage(ctx, params.Portal, intent, msg, c.Main.Config.URLPreviewsInBackfill)
			if err == nil {
				backfillMessages = append(backfillMessages, &bridgev2.BackfillMessage{
					ConvertedMessage: cm,
					Sender:           sender,
					ID:               makeMessageID(msg.GUID),
					TxnID:            networkid.TransactionID(msg.GUID),
					Timestamp:        msg.Time,
					StreamOrder:      msg.Time.UnixMilli(),
				})
			}
		}

		// Live Photo handling: macOS stores the MOV companion as a sibling
		// file next to the HEIC on disk, but does NOT list it in the
		// attachment table. Bridge the original attachment first, then
		// check for and bridge any companion MOV alongside it.
		for i, att := range msg.Attachments {
			if att == nil {
				continue
			}
			attCm, err := convertChatDBAttachment(ctx, params.Portal, intent, msg, att, c.Main.Config.VideoTranscoding, c.Main.Config.HEICConversion, c.Main.Config.HEICJPEGQuality)
			if err != nil {
				log.Warn().Err(err).Str("guid", msg.GUID).Int("att_index", i).Msg("Failed to convert attachment, skipping")
				continue
			}
			partID := fmt.Sprintf("%s_att%d", msg.GUID, i)
			if msg.ReplyToGUID != "" {
				attCm.ReplyTo = chatDBReplyTarget(msg.ReplyToGUID, msg.ReplyToPart)
			}
			backfillMessages = append(backfillMessages, &bridgev2.BackfillMessage{
				ConvertedMessage: attCm,
				Sender:           sender,
				ID:               makeMessageID(partID),
				TxnID:            networkid.TransactionID(partID),
				Timestamp:        msg.Time.Add(time.Duration(i+1) * time.Millisecond),
				StreamOrder:      msg.Time.UnixMilli() + int64(i+1),
			})

			// If there's a Live Photo MOV companion on disk, bridge it too.
			movAtt := chatDBResolveLivePhoto(att, log)
			if movAtt.PathOnDisk != att.PathOnDisk {
				movCm, movErr := convertChatDBAttachment(ctx, params.Portal, intent, msg, movAtt, c.Main.Config.VideoTranscoding, c.Main.Config.HEICConversion, c.Main.Config.HEICJPEGQuality)
				if movErr != nil {
					log.Warn().Err(movErr).Str("guid", msg.GUID).Int("att_index", i).Msg("Failed to convert Live Photo MOV companion, skipping")
				} else {
					movID := fmt.Sprintf("%s_att%d_mov", msg.GUID, i)
					backfillMessages = append(backfillMessages, &bridgev2.BackfillMessage{
						ConvertedMessage: movCm,
						Sender:           sender,
						ID:               makeMessageID(movID),
						TxnID:            networkid.TransactionID(movID),
						Timestamp:        msg.Time.Add(time.Duration(i+1)*time.Millisecond + 500*time.Microsecond),
						StreamOrder:      msg.Time.UnixMilli() + int64(i+1),
					})
				}
			}
		}
	}

	return &bridgev2.FetchMessagesResponse{
		Messages:                backfillMessages,
		HasMore:                 len(messages) >= count,
		Forward:                 params.Forward,
		AggressiveDeduplication: params.Forward,
	}, nil
}

// ============================================================================
// chat.db ↔ portal ID conversion
// ============================================================================

// portalIDToChatGUIDs converts a DM portal ID to possible chat.db GUIDs.
// Returns multiple possible GUIDs to try, since macOS versions differ:
// Tahoe+ uses "any;-;" while older uses "iMessage;-;" or "SMS;-;".
//
// Note: Group portal IDs (comma-separated) are handled by findGroupChatGUID instead.
func portalIDToChatGUIDs(portalID string) []string {
	// DMs: strip tel:/mailto: prefix and try multiple service prefixes
	localID := stripIdentifierPrefix(portalID)
	if localID == "" {
		return nil
	}
	// Strip legacy (sms...) suffix from pre-fix portal IDs so chat.db GUID
	// candidates match: "tel:+12155167207(smsft)" → localID "+12155167207(smsft)"
	// → stripped "+12155167207", producing "any;-;+12155167207" which matches chat.db.
	localID = stripSmsSuffix(localID)
	return []string{
		"any;-;" + localID,
		"iMessage;-;" + localID,
		"SMS;-;" + localID,
	}
}

// identifierToPortalID converts a chat.db Identifier to a clean portal ID.
func identifierToPortalID(id imessage.Identifier) networkid.PortalID {
	if id.IsGroup {
		return networkid.PortalID(id.String())
	}
	localID := id.LocalID
	// Strip Apple SMS service suffixes: "+12155167207(smsft)" → "+12155167207",
	// "787473(smsft)" → "787473". These are native Apple formats that appear in
	// chat.db for SMS Forwarding service types.
	localID = stripSmsSuffix(localID)
	if strings.HasPrefix(localID, "+") {
		return networkid.PortalID("tel:" + localID)
	}
	if strings.Contains(localID, "@") {
		return networkid.PortalID("mailto:" + localID)
	}
	// Short codes and numeric-only identifiers (e.g., "242733") are SMS-based.
	// Rustpush creates these with "tel:" prefix, so we must match.
	if isNumeric(localID) {
		return networkid.PortalID("tel:" + localID)
	}
	return networkid.PortalID(localID)
}

// ============================================================================
// chat.db message conversion
// ============================================================================

func chatDBMakeEventSender(msg *imessage.Message, c *IMClient) bridgev2.EventSender {
	if msg.IsFromMe {
		return bridgev2.EventSender{
			IsFromMe:    true,
			SenderLogin: c.UserLogin.ID,
			Sender:      makeUserID(c.handle),
		}
	}
	return bridgev2.EventSender{
		IsFromMe: false,
		Sender:   makeUserID(addIdentifierPrefix(stripSmsSuffix(msg.Sender.LocalID))),
	}
}

func convertChatDBMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, msg *imessage.Message, urlPreviewsInBackfill bool) (*bridgev2.ConvertedMessage, error) {
	content := &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    msg.Text,
	}
	if msg.Subject != "" {
		if msg.Text != "" {
			content.Body = fmt.Sprintf("**%s**\n%s", msg.Subject, msg.Text)
			content.Format = event.FormatHTML
			content.FormattedBody = fmt.Sprintf("<strong>%s</strong><br/>%s", msg.Subject, msg.Text)
		} else {
			content.Body = msg.Subject
		}
	}
	if msg.IsEmote {
		content.MsgType = event.MsgEmote
	}

	// URL preview: detect URL and fetch og: metadata + image
	if urlPreviewsInBackfill {
		if detectedURL := urlRegex.FindString(msg.Text); detectedURL != "" {
			content.BeeperLinkPreviews = []*event.BeeperLinkPreview{
				fetchURLPreview(ctx, portal.Bridge, intent, portal.MXID, detectedURL),
			}
		}
	}

	cm := &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{{
			Type:    event.EventMessage,
			Content: content,
		}},
	}
	if msg.ReplyToGUID != "" {
		cm.ReplyTo = chatDBReplyTarget(msg.ReplyToGUID, msg.ReplyToPart)
	}
	return cm, nil
}

func convertChatDBAttachment(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, msg *imessage.Message, att *imessage.Attachment, videoTranscoding, heicConversion bool, heicQuality int) (*bridgev2.ConvertedMessage, error) {
	mimeType := att.GetMimeType()
	fileName := att.GetFileName()

	data, err := att.Read()
	if err != nil {
		return nil, fmt.Errorf("failed to read attachment %s: %w", att.PathOnDisk, err)
	}

	// Convert CAF Opus voice messages to OGG Opus for Matrix/Beeper clients
	var durationMs int
	if mimeType == "audio/x-caf" || strings.HasSuffix(strings.ToLower(fileName), ".caf") {
		data, mimeType, fileName, durationMs = convertAudioForMatrix(data, mimeType, fileName)
	}

	// Remux/transcode non-MP4 videos to MP4 for broad Matrix client compatibility.
	log := zerolog.Ctx(ctx)
	if videoTranscoding && ffmpeg.Supported() && strings.HasPrefix(mimeType, "video/") && mimeType != "video/mp4" {
		origMime := mimeType
		origSize := len(data)
		converted, method, convertErr := transcodeToMP4(ctx, log, data, mimeType)
		if convertErr != nil {
			log.Warn().Err(convertErr).Str("guid", msg.GUID).Str("original_mime", origMime).
				Msg("FFmpeg video conversion failed after retries, uploading original")
		} else {
			log.Info().Str("guid", msg.GUID).Str("original_mime", origMime).
				Str("method", method).Int("original_bytes", origSize).Int("converted_bytes", len(converted)).
				Msg("Video transcoded to MP4")
			data = converted
			mimeType = "video/mp4"
			fileName = strings.TrimSuffix(fileName, filepath.Ext(fileName)) + ".mp4"
		}
	}

	// Convert HEIC/HEIF images to JPEG since most Matrix clients can't display HEIC.
	var heicImg image.Image
	data, mimeType, fileName, heicImg = maybeConvertHEIC(log, data, mimeType, fileName, heicQuality, heicConversion)

	// Extract image dimensions and generate thumbnail
	var imgWidth, imgHeight int
	var thumbData []byte
	var thumbW, thumbH int
	if heicImg != nil {
		b := heicImg.Bounds()
		imgWidth, imgHeight = b.Dx(), b.Dy()
		if imgWidth > 800 || imgHeight > 800 {
			thumbData, thumbW, thumbH = scaleAndEncodeThumb(heicImg, imgWidth, imgHeight)
		}
	} else if strings.HasPrefix(mimeType, "image/") || looksLikeImage(data) {
		if mimeType == "image/gif" {
			if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
				imgWidth, imgHeight = cfg.Width, cfg.Height
			}
		} else if img, fmtName, _ := decodeImageData(data); img != nil {
			b := img.Bounds()
			imgWidth, imgHeight = b.Dx(), b.Dy()
			// Re-encode TIFF as JPEG for compatibility (PNG is fine as-is)
			if fmtName == "tiff" {
				var buf bytes.Buffer
				if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err == nil {
					data = buf.Bytes()
					mimeType = "image/jpeg"
					fileName = strings.TrimSuffix(fileName, filepath.Ext(fileName)) + ".jpg"
				}
			}
			if imgWidth > 800 || imgHeight > 800 {
				thumbData, thumbW, thumbH = scaleAndEncodeThumb(img, imgWidth, imgHeight)
			}
		}
	}

	content := &event.MessageEventContent{
		MsgType: mimeToMsgType(mimeType),
		Body:    fileName,
		Info: &event.FileInfo{
			MimeType: mimeType,
			Size:     len(data),
			Width:    imgWidth,
			Height:   imgHeight,
		},
	}

	// Mark as voice message if this was a CAF voice recording
	if durationMs > 0 {
		content.MSC3245Voice = &event.MSC3245Voice{}
		content.MSC1767Audio = &event.MSC1767Audio{
			Duration: durationMs,
		}
	}

	if intent != nil {
		url, encFile, err := intent.UploadMedia(ctx, "", data, fileName, mimeType)
		if err != nil {
			return nil, fmt.Errorf("failed to upload attachment: %w", err)
		}
		if encFile != nil {
			content.File = encFile
		} else {
			content.URL = url
		}

		// Upload image thumbnail
		if thumbData != nil {
			thumbURL, thumbEnc, thumbErr := intent.UploadMedia(ctx, "", thumbData, "thumbnail.jpg", "image/jpeg")
			if thumbErr == nil {
				if thumbEnc != nil {
					content.Info.ThumbnailFile = thumbEnc
				} else {
					content.Info.ThumbnailURL = thumbURL
				}
				content.Info.ThumbnailInfo = &event.FileInfo{
					MimeType: "image/jpeg",
					Size:     len(thumbData),
					Width:    thumbW,
					Height:   thumbH,
				}
			}
		}
	}

	return &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{{
			Type:    event.EventMessage,
			Content: content,
		}},
	}, nil
}

// chatDBResolveLivePhoto checks if a HEIC/JPG attachment has a companion .MOV
// file on disk (Apple stores Live Photo videos as sibling files but doesn't
// list them in the attachment table). If found, returns a new Attachment
// pointing to the MOV. Returns the original attachment unchanged if no
// companion is found.
func chatDBResolveLivePhoto(att *imessage.Attachment, log *zerolog.Logger) *imessage.Attachment {
	path := att.PathOnDisk
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return att
		}
		path = filepath.Join(home, path[2:])
	}

	lower := strings.ToLower(filepath.Base(path))
	if !strings.HasSuffix(lower, ".heic") && !strings.HasSuffix(lower, ".jpg") && !strings.HasSuffix(lower, ".jpeg") {
		return att
	}

	// Check for sibling .MOV in the same directory
	dir := filepath.Dir(path)
	base := filenameBase(filepath.Base(path))
	movPath := filepath.Join(dir, base+".MOV")

	if _, err := os.Stat(movPath); err != nil {
		// Try lowercase extension
		movPath = filepath.Join(dir, base+".mov")
		if _, err := os.Stat(movPath); err != nil {
			return att // No companion MOV found — keep the HEIC
		}
	}

	log.Info().Str("heic", filepath.Base(path)).Str("mov", filepath.Base(movPath)).
		Msg("Live Photo: swapping HEIC for MOV companion")

	return &imessage.Attachment{
		GUID:       att.GUID,
		PathOnDisk: movPath,
		FileName:   base + ".MOV",
		MimeType:   "video/quicktime",
	}
}
