// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package connector

import (
	"maunium.net/go/mautrix/bridgev2/database"
)

type PortalMetadata struct {
	ThreadID   string `json:"thread_id,omitempty"`
	SenderGuid string `json:"sender_guid,omitempty"` // Persistent iMessage group UUID
	GroupName  string `json:"group_name,omitempty"`   // iMessage cv_name for outbound routing
	IsSms      bool   `json:"is_sms,omitempty"`       // True if this portal routes through SMS
}

type GhostMetadata struct{}

type MessageMetadata struct {
	HasAttachments bool `json:"has_attachments,omitempty"`

	// SiblingUUID is set when a Matrix m.image with caption was split into two
	// iMessages (attachment + follow-up text). Stored on the primary DB row so
	// redact/unsend can remove both halves together.
	SiblingUUID string `json:"sibling_uuid,omitempty"`

	// ContinuationUUIDs holds the extra iMessages produced when one oversized
	// Matrix message had to be sent as several. Kept separate from SiblingUUID,
	// which pairs an attachment with its caption and holds exactly one UUID.
	// Without this a Matrix redaction would unsend only the first chunk, leaving
	// the rest on the recipient's device while Matrix shows the message deleted.
	ContinuationUUIDs []string `json:"continuation_uuids,omitempty"`
}

type UserLoginMetadata struct {
	Platform    string `json:"platform,omitempty"`
	ChatsSynced bool   `json:"chats_synced,omitempty"`
	WelcomeSent bool   `json:"welcome_sent,omitempty"`

	// Persisted rustpush state (restored across restarts)
	APSState    string `json:"aps_state,omitempty"`
	IDSUsers    string `json:"ids_users,omitempty"`
	IDSIdentity string `json:"ids_identity,omitempty"`
	DeviceID    string `json:"device_id,omitempty"`

	// Hardware key for cross-platform (non-macOS) operation.
	// Base64-encoded JSON HardwareConfig extracted from a real Mac.
	HardwareKey string `json:"hardware_key,omitempty"`

	// PreferredHandle is the user-chosen handle for outgoing messages
	// (e.g. "tel:+15551234567" or "mailto:user@example.com").
	PreferredHandle string `json:"preferred_handle,omitempty"`

	// iCloud account persist data for TokenProvider restoration.
	// Allows CardDAV contacts and CloudKit to work across restarts.
	AccountUsername          string `json:"account_username,omitempty"`
	AccountHashedPasswordHex string `json:"account_hashed_password_hex,omitempty"`
	AccountPET               string `json:"account_pet,omitempty"`
	AccountADSID             string `json:"account_adsid,omitempty"`
	AccountDSID              string `json:"account_dsid,omitempty"`
	AccountSPDBase64         string `json:"account_spd_base64,omitempty"`

	// Cached MobileMe delegate JSON — seeded on restore so contacts work
	// without needing to refresh (which requires a still-valid PET).
	MmeDelegateJSON string `json:"mme_delegate_json,omitempty"`
}

func (c *IMConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		Portal: func() any {
			return &PortalMetadata{}
		},
		Ghost: func() any {
			return &GhostMetadata{}
		},
		Message: func() any {
			return &MessageMetadata{}
		},
		Reaction: nil,
		UserLogin: func() any {
			return &UserLoginMetadata{}
		},
	}
}
