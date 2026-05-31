// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	_ "embed"
	"strings"
	"text/template"

	up "go.mau.fi/util/configupgrade"
	"gopkg.in/yaml.v3"
)

//go:embed example-config.yaml
var ExampleConfig string

type IMConfig struct {
	DisplaynameTemplate string `yaml:"displayname_template"`
	displaynameTemplate *template.Template

	// CloudKitBackfill enables message history backfill (master on/off switch).
	// When false, the bridge only handles real-time messages via APNs push
	// and skips the device PIN / iCloud Keychain steps during login.
	// Default is false.
	CloudKitBackfill bool `yaml:"cloudkit_backfill"`

	// BackfillSource selects the backfill engine when CloudKitBackfill is true.
	// "cloudkit" (default) syncs from iCloud; "chatdb" reads the local macOS
	// chat.db (requires Full Disk Access).
	BackfillSource string `yaml:"backfill_source"`

	// VideoTranscoding enables automatic remuxing/transcoding of non-MP4
	// videos (e.g. QuickTime .mov) to MP4 for broad Matrix client
	// compatibility.  Requires ffmpeg to be installed.  Default is false.
	VideoTranscoding bool `yaml:"video_transcoding"`

	// HEICConversion enables automatic conversion of HEIC/HEIF images
	// to JPEG for broad Matrix client compatibility.
	// Requires libheif to be installed.  Default is false.
	HEICConversion bool `yaml:"heic_conversion"`

	// HEICJPEGQuality sets the JPEG output quality (1–100) used when
	// converting HEIC/HEIF images. Default is 95.
	HEICJPEGQuality int `yaml:"heic_jpeg_quality"`

	// URLPreviewsInBackfill controls whether the bridge fetches link-preview
	// metadata (og:/twitter: tags + thumbnail image) for messages that
	// contain a URL during backfill. Each URL-bearing message triggers up to
	// three HTTP round-trips (homeserver preview, page metadata, image
	// download + re-upload) inline with conversion, which on a slow uplink
	// dominates backfill wall-clock time. Setting this to false skips
	// preview generation for backfilled history only; live inbound messages
	// and outbound edits still build previews normally. Default true.
	URLPreviewsInBackfill bool `yaml:"url_previews_in_backfill"`

	// PreferredHandle overrides the outgoing iMessage identity.
	// Use the full URI format: "tel:+15551234567" or "mailto:user@example.com".
	// If empty, the handle chosen during login is used.
	PreferredHandle string `yaml:"preferred_handle"`

	// FaceTimeDisplayName overrides the display name pre-filled on the
	// FaceTime web join page (the value attached to `#n=…` in the ring-
	// notice link). If empty, the bridge reads the user's "First Last"
	// from the cached Apple Account SPD; if that's also unavailable it
	// falls back to the bare iMessage handle.
	FaceTimeDisplayName string `yaml:"facetime_display_name"`

	// DisableFaceTime turns off all bridge FaceTime integration: the
	// !facetime* slash commands aren't registered, inbound ring / missed /
	// answered-elsewhere notices aren't posted, and inbound peer-invite
	// notices are suppressed. Intended for users who own an Apple device
	// and answer FaceTime calls natively — the bridge wrapper adds nothing
	// in that case and just clutters the chat.
	DisableFaceTime bool `yaml:"disable_facetime"`

	// StatusKitShareOnStartup publishes share_status(available) once after
	// StatusKit init completes. Peer iOS reciprocates with a reshare (which
	// carries the key material needed to decrypt its subsequent presence
	// updates), so keeping this on dramatically improves the chance of
	// seeing contacts' Focus state in Matrix. Default true.
	StatusKitShareOnStartup bool `yaml:"statuskit_share_on_startup"`

	// StatusKitNotifications controls whether the bridge posts user-visible
	// notices when a contact toggles iOS 18 Focus / Do Not Disturb. When
	// true (the default), enabling DND on a contact's iPhone surfaces a
	// silent m.notice in the DM portal ("🔕 Name has notifications
	// silenced (Do Not Disturb).") plus a Matrix ghost presence update,
	// and clears when they turn it off. When false, OnStatusUpdate is a
	// no-op — StatusKit init, key exchange, and APNs subscription still
	// run (so the IDS-layer 4-service identity stays intact for peer
	// reshare eligibility), but no notices or presence updates reach the
	// user. Useful for users who find the notices noisy or who rely on
	// other Apple devices for Focus visibility.
	StatusKitNotifications bool `yaml:"statuskit_notifications"`

	// ReadReceipts controls whether the bridge sends read receipts to iMessage
	// contacts when you mark a message as read in Matrix. When false, iMessage
	// contacts will not see the "Read" indicator for your messages. Incoming
	// read receipts from iMessage contacts are always forwarded to Matrix.
	// Default is true.
	ReadReceipts bool `yaml:"read_receipts"`

	// TypingNotifications controls whether the bridge sends typing indicators
	// to iMessage contacts while you compose a reply in Matrix. When false,
	// iMessage contacts will not see the typing bubble. Incoming typing
	// indicators from iMessage contacts are always forwarded to Matrix.
	// Default is true.
	TypingNotifications bool `yaml:"typing_notifications"`

	// CardDAV is an external CardDAV server for contact name resolution.
	// When configured, this is used instead of iCloud CardDAV contacts.
	CardDAV CardDAVConfig `yaml:"carddav"`

	// DebugDisablePrivacy is a DEVELOPMENT-ONLY switch that reverts the
	// bridge's privacy protections so plaintext is observable for debugging.
	// MUST be false in any real deployment. Default false. When true:
	//
	//   1. Logs are no longer anonymized — raw iMessage handles
	//      (phone numbers / emails) and full URLs appear verbatim in logs
	//      instead of opaque tokens / scheme+host forms.
	//   2. The periodic body scrubber is disabled — plaintext message
	//      bodies (text/subject/sender) downloaded into the local
	//      cloud_message side-cache are NOT cleared after Matrix delivery,
	//      so they persist in mautrix-imessage.db.
	//   3. On the next CloudKit sync the bridge re-fills already-scrubbed
	//      rows: it clears the body_scrubbed flags and resets the CloudKit
	//      continuation tokens, forcing a full re-page that re-downloads the
	//      plaintext that was previously cleared. This is a one-shot per flip
	//      (once no scrubbed rows remain, sync goes back to incremental).
	//
	// Notes: only the CloudKit backfill source caches plaintext locally, so
	// effects 2 and 3 are CloudKit-specific (the chatdb source never stores
	// message bodies in our DB). This does NOT undo deletes/unsends — content
	// you delete or retract is still purged. Re-deletes nothing from Matrix.
	DebugDisablePrivacy bool `yaml:"debug_disable_privacy"`
}

// CardDAVConfig configures an external CardDAV server for contact name resolution.
// Supports Google (with app passwords), Nextcloud, Radicale, Fastmail, etc.
type CardDAVConfig struct {
	// Email address used for CardDAV auto-discovery (RFC 6764 .well-known/carddav).
	// Also used as the username if Username is empty.
	Email string `yaml:"email"`

	// URL is the CardDAV server URL. Leave empty to auto-discover from Email.
	// Example: https://www.googleapis.com/carddav/v1/principals/you@gmail.com/lists/default/
	URL string `yaml:"url"`

	// Username for HTTP Basic authentication. Defaults to Email if empty.
	Username string `yaml:"username"`

	// PasswordEncrypted is the AES-256-GCM encrypted app password (base64).
	// Set by the install script via the carddav-setup subcommand.
	PasswordEncrypted string `yaml:"password_encrypted"`
}

// IsConfigured returns true if the CardDAV config has enough info to connect.
func (c *CardDAVConfig) IsConfigured() bool {
	return c.Email != "" && c.PasswordEncrypted != ""
}

// GetUsername returns the effective username (falls back to Email).
func (c *CardDAVConfig) GetUsername() string {
	if c.Username != "" {
		return c.Username
	}
	return c.Email
}

type umIMConfig IMConfig

func (c *IMConfig) UnmarshalYAML(node *yaml.Node) error {
	err := node.Decode((*umIMConfig)(c))
	if err != nil {
		return err
	}
	return c.PostProcess()
}

func (c *IMConfig) PostProcess() error {
	var err error
	c.displaynameTemplate, err = template.New("displayname").Parse(c.DisplaynameTemplate)
	return err
}

type DisplaynameParams struct {
	FirstName string
	LastName  string
	Nickname  string
	Phone     string
	Email     string
	ID        string
}

func (c *IMConfig) FormatDisplayname(params DisplaynameParams) string {
	var buf strings.Builder
	err := c.displaynameTemplate.Execute(&buf, &params)
	if err != nil {
		return params.ID
	}
	name := strings.TrimSpace(buf.String())
	if name == "" {
		return params.ID
	}
	return name
}

// UseChatDBBackfill returns true when backfill is enabled and sourced from chat.db.
func (c *IMConfig) UseChatDBBackfill() bool {
	return c.CloudKitBackfill && c.BackfillSource == "chatdb"
}

// UseCloudKitBackfill returns true when backfill is enabled and sourced from CloudKit.
func (c *IMConfig) UseCloudKitBackfill() bool {
	return c.CloudKitBackfill && c.BackfillSource != "chatdb"
}

func upgradeConfig(helper up.Helper) {
	helper.Copy(up.Str, "displayname_template")
	helper.Copy(up.Bool, "cloudkit_backfill")
	helper.Copy(up.Str, "backfill_source")
	helper.Copy(up.Bool, "video_transcoding")
	helper.Copy(up.Bool, "heic_conversion")
	helper.Copy(up.Int, "heic_jpeg_quality")
	helper.Copy(up.Bool, "url_previews_in_backfill")
	helper.Copy(up.Str, "preferred_handle")
	helper.Copy(up.Str, "facetime_display_name")
	helper.Copy(up.Bool, "disable_facetime")
	helper.Copy(up.Bool, "statuskit_share_on_startup")
	helper.Copy(up.Bool, "statuskit_notifications")
	helper.Copy(up.Bool, "read_receipts")
	helper.Copy(up.Bool, "typing_notifications")
	helper.Copy(up.Str, "carddav", "email")
	helper.Copy(up.Str, "carddav", "url")
	helper.Copy(up.Str, "carddav", "username")
	helper.Copy(up.Str, "carddav", "password_encrypted")
	helper.Copy(up.Bool, "debug_disable_privacy")
}

func (c *IMConnector) GetConfig() (string, any, up.Upgrader) {
	return ExampleConfig, &c.Config, up.SimpleUpgrader(upgradeConfig)
}
