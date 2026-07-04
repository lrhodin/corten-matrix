package connector

import "strings"

func isGroupPortalID(portalID string) bool {
	return strings.HasPrefix(portalID, "gid:") || strings.Contains(portalID, ",")
}

// isCarrierService reports whether a CloudKit chat service is SMS/RCS/MMS rather
// than iMessage. Carrier groups have no stable Apple group GUID (CloudKit returns
// them under several unstable group_id encodings), so they're keyed by participant
// set, not gid:. iMessage and unknown/empty services keep their gid: key.
func isCarrierService(service string) bool {
	switch strings.ToUpper(strings.TrimSpace(service)) {
	case "SMS", "RCS", "MMS":
		return true
	default:
		return false
	}
}

// normalizeUUID strips dashes and lowercases a UUID for comparison.
// APNs sends dashless (520464eb701340d7bd9e7ae51684e430) while CloudKit
// uses dashed (520464eb-7013-40d7-bd9e-7ae51684e430). This normalizes both.
func normalizeUUID(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "-", ""))
}

// groupPortalDedupKey returns a stable dedupe key for group portals.
// Prefer protocol group UUID when available; otherwise fall back to a
// normalized participant signature.
func groupPortalDedupKey(portalID, groupID string, participants []string) string {
	groupID = strings.TrimSpace(groupID)
	if groupID != "" {
		return "group:" + normalizeUUID(groupID)
	}
	if strings.HasPrefix(portalID, "gid:") {
		return "group:" + normalizeUUID(strings.TrimPrefix(portalID, "gid:"))
	}
	normalized := normalizeRecoverableParticipants(participants)
	if len(normalized) == 0 && strings.Contains(portalID, ",") {
		normalized = normalizeRecoverableParticipants(strings.Split(portalID, ","))
	}
	if len(normalized) > 0 {
		return "parts:" + strings.Join(normalized, ",")
	}
	return "portal:" + portalID
}
