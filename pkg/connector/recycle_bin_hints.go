package connector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"

	"github.com/lrhodin/corten-matrix/pkg/rustpushgo"
)

type recoverableMessageMetadata struct {
	Version     int    `json:"v"`
	RecordName  string `json:"record_name"`
	CloudChatID string `json:"cloud_chat_id"`
	Sender      string `json:"sender"`
	IsFromMe    bool   `json:"is_from_me"`
	Service     string `json:"service"`
	TimestampMS int64  `json:"timestamp_ms"`
}

type recoverableMessagePortalHint struct {
	PortalID     string
	CloudChatID  string
	Service      string
	Participants []string
	NewestTS     int64
	Count        int
}

func recoverableGUIDFromEntry(entry string) string {
	if idx := strings.IndexByte(entry, '|'); idx >= 0 {
		return entry[:idx]
	}
	return entry
}

func parseRecoverableMessageMetadata(entry string) (recoverableMessageMetadata, bool) {
	var metadata recoverableMessageMetadata
	idx := strings.IndexByte(entry, '|')
	if idx < 0 || idx >= len(entry)-1 {
		return metadata, false
	}

	payload, err := base64.StdEncoding.DecodeString(entry[idx+1:])
	if err != nil {
		return metadata, false
	}
	if err = json.Unmarshal(payload, &metadata); err != nil {
		return metadata, false
	}
	if metadata.Version != 1 {
		return recoverableMessageMetadata{}, false
	}
	return metadata, true
}

func (metadata recoverableMessageMetadata) wrappedMessage(guid string) rustpushgo.WrappedCloudSyncMessage {
	return rustpushgo.WrappedCloudSyncMessage{
		RecordName:  metadata.RecordName,
		Guid:        guid,
		CloudChatId: metadata.CloudChatID,
		Sender:      metadata.Sender,
		IsFromMe:    metadata.IsFromMe,
		Service:     metadata.Service,
		TimestampMs: metadata.TimestampMS,
	}
}

func normalizeRecoverableParticipants(participants []string) []string {
	if len(participants) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(participants))
	normalized := make([]string, 0, len(participants))
	for _, participant := range participants {
		value := normalizeIdentifierForPortalID(participant)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	sort.Strings(normalized)
	return normalized
}

func (c *IMClient) participantSeedsForRecoverableMessage(portalID string, metadata recoverableMessageMetadata) []string {
	isGroup := strings.HasPrefix(portalID, "gid:") || strings.Contains(portalID, ",")
	if !isGroup {
		return normalizeRecoverableParticipants([]string{portalID})
	}

	participants := make([]string, 0, 4)
	if metadata.Sender != "" {
		participants = append(participants, metadata.Sender)
	}
	if metadata.IsFromMe && c.handle != "" {
		participants = append(participants, c.handle)
	}
	if strings.Contains(portalID, ",") {
		participants = append(participants, strings.Split(portalID, ",")...)
	}
	return normalizeRecoverableParticipants(participants)
}

func (c *IMClient) buildRecoverableMessagePortalHints(ctx context.Context, entries []string) []recoverableMessagePortalHint {
	type portalAccumulator struct {
		recoverableMessagePortalHint
		participantSet map[string]struct{}
	}

	byPortal := make(map[string]*portalAccumulator)
	for _, entry := range entries {
		metadata, ok := parseRecoverableMessageMetadata(entry)
		if !ok {
			continue
		}

		guid := recoverableGUIDFromEntry(entry)
		if guid == "" {
			continue
		}

		portalID := c.resolveConversationID(ctx, metadata.wrappedMessage(guid))
		if portalID == "" {
			continue
		}

		acc := byPortal[portalID]
		if acc == nil {
			acc = &portalAccumulator{
				recoverableMessagePortalHint: recoverableMessagePortalHint{
					PortalID: portalID,
				},
				participantSet: make(map[string]struct{}),
			}
			byPortal[portalID] = acc
		}

		acc.Count++
		if metadata.TimestampMS > acc.NewestTS {
			acc.NewestTS = metadata.TimestampMS
		}
		if acc.CloudChatID == "" && metadata.CloudChatID != "" {
			acc.CloudChatID = metadata.CloudChatID
		}
		if acc.Service == "" && metadata.Service != "" {
			acc.Service = metadata.Service
		}
		for _, participant := range c.participantSeedsForRecoverableMessage(portalID, metadata) {
			acc.participantSet[participant] = struct{}{}
		}
	}

	out := make([]recoverableMessagePortalHint, 0, len(byPortal))
	for _, acc := range byPortal {
		if len(acc.participantSet) > 0 {
			acc.Participants = make([]string, 0, len(acc.participantSet))
			for participant := range acc.participantSet {
				acc.Participants = append(acc.Participants, participant)
			}
			sort.Strings(acc.Participants)
		}
		out = append(out, acc.recoverableMessagePortalHint)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].NewestTS == out[j].NewestTS {
			return out[i].PortalID < out[j].PortalID
		}
		return out[i].NewestTS > out[j].NewestTS
	})
	return out
}
