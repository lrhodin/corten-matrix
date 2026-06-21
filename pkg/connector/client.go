// mautrix-imessage - A Matrix-iMessage puppeting bridge.
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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image"
	"image/jpeg"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "image/gif"
	_ "image/png"

	_ "golang.org/x/image/tiff"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.mau.fi/util/ffmpeg"
	"go.mau.fi/util/ptr"
	"golang.org/x/sync/semaphore"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	matrixfmt "maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/pushrules"

	"github.com/lrhodin/imessage/imessage"
	"github.com/lrhodin/imessage/pkg/rustpushgo"
)

// IMClient implements bridgev2.NetworkAPI using the rustpush iMessage protocol
// library for real-time messaging. Contact resolution uses iCloud CardDAV.

// deletedPortalEntry tracks why a portal was marked as deleted.
// Entries in recentlyDeletedPortals serve two purposes:
//  1. Echo suppression: APNs messages with known UUIDs are dropped;
//     unknown UUIDs are only allowed through if they're newer than the
//     deleted chat's known tail.
//  2. Re-import guard: ingestCloudMessages marks messages as deleted=TRUE
//     for portals in this map, preventing periodic re-syncs from re-importing
//     messages with deleted=FALSE and triggering backfill resurrection.
type deletedPortalEntry struct {
	deletedAt   time.Time
	isTombstone bool
}

// recycleBinCandidate represents a portal that has messages in Apple's recycle
// bin. Stored after bootstrap and shown to the user via bridgebot notification
// so they can choose which chats to delete with !delete-stale.
type recycleBinCandidate struct {
	portalID    string
	displayName string
	recoverable int
	total       int
}

// failedAttachmentEntry tracks a CloudKit attachment download/upload failure
// with a retry count. After maxAttachmentRetries attempts the attachment is
// abandoned to avoid infinite retries on permanently corrupted records.
//
// abandoned is sticky once set: the pre-upload filter keeps the entry instead
// of deleting it, so an exhausted attachment stays skipped for the rest of the
// session. Deleting it (the old behavior) reset the counter and made the same
// dead attachment re-download on every sync sweep — an infinite loop.
type failedAttachmentEntry struct {
	lastError string
	retries   int
	abandoned bool
}

// unrecoverableAttachmentMarker is the prefix the Rust download path stamps on
// errors that mean CloudKit no longer serves the asset at all (Apple aged out
// the MMCS blob: no body / no matching reference / missing identifiers). Such
// attachments are tombstoned immediately so the bridge never re-attempts them,
// even across restarts. See cloud_download_attachment in pkg/rustpushgo.
const unrecoverableAttachmentMarker = "ATTACHMENT_UNRECOVERABLE"

// isUnrecoverableAttachmentError reports whether a download error means the
// asset is permanently gone (vs a transient/network failure worth retrying).
func isUnrecoverableAttachmentError(errMsg string) bool {
	return strings.Contains(errMsg, unrecoverableAttachmentMarker)
}

type restoreStatusFunc func(format string, args ...any)

type restorePipelineOptions struct {
	PortalID       string
	PortalKey      networkid.PortalKey
	Source         string
	DisplayName    string
	Participants   []string
	ChatID         string
	GroupID        string
	GroupPhotoGuid string
	RecoverOnApple bool
	Notify         restoreStatusFunc
}

const maxAttachmentRetries = 3

// recordAttachmentFailure increments the retry count for a failed attachment.
// Returns the updated entry so callers can log the retry count.
func (c *IMClient) recordAttachmentFailure(recordName, errMsg string) *failedAttachmentEntry {
	entry := &failedAttachmentEntry{lastError: errMsg, retries: 1}
	if prev, loaded := c.failedAttachments.Load(recordName); loaded {
		old := prev.(*failedAttachmentEntry)
		entry.retries = old.retries + 1
		entry.abandoned = old.abandoned
	}
	// Permanently-gone assets (Apple no longer serves them) get a persistent
	// tombstone on the FIRST failure so the bridge never re-downloads them,
	// even across restarts. Transient failures fall back to the in-session
	// retry budget: abandoned (skipped) for the rest of THIS session once
	// exhausted, but re-tried after a restart in case the failure was a blip.
	if isUnrecoverableAttachmentError(errMsg) {
		entry.abandoned = true
		c.markAttachmentDead(recordName, errMsg)
	} else if entry.retries >= maxAttachmentRetries {
		entry.abandoned = true
	}
	c.failedAttachments.Store(recordName, entry)
	return entry
}

// ensureDeadAttachmentsLoaded populates the in-memory deadAttachments set from
// the persistent cloud_attachment_dead table, exactly once per session. Loading
// is lazy (first skip-check or first failure) so it works for both the bulk
// startup pre-upload and the per-chunk capped-backfill path.
func (c *IMClient) ensureDeadAttachmentsLoaded() {
	c.deadAttachmentsOnce.Do(func() {
		if c.cloudStore == nil {
			return
		}
		names, err := c.cloudStore.loadDeadAttachments(context.Background())
		if err != nil {
			c.UserLogin.Log.Warn().Err(err).
				Msg("Failed to load dead-attachment tombstones; may re-attempt aged-out attachments")
			return
		}
		for _, name := range names {
			c.deadAttachments.Store(name, struct{}{})
		}
		if len(names) > 0 {
			c.UserLogin.Log.Debug().Int("count", len(names)).
				Msg("Loaded dead-attachment tombstones (CloudKit no longer serves these)")
		}
	})
}

// isDeadAttachment reports whether record_name is tombstoned as permanently
// un-downloadable, so callers skip it without touching CloudKit.
func (c *IMClient) isDeadAttachment(recordName string) bool {
	c.ensureDeadAttachmentsLoaded()
	_, dead := c.deadAttachments.Load(recordName)
	return dead
}

// markAttachmentDead tombstones an attachment that CloudKit no longer serves,
// in memory and persistently. Best-effort: a failed DB write just means the
// skip won't survive a restart (the attachment re-fails and re-tombstones).
func (c *IMClient) markAttachmentDead(recordName, reason string) {
	if _, loaded := c.deadAttachments.LoadOrStore(recordName, struct{}{}); loaded {
		return // already tombstoned this session
	}
	if c.cloudStore != nil {
		c.cloudStore.saveDeadAttachment(context.Background(), recordName, reason)
	}
}

type IMClient struct {
	Main      *IMConnector
	UserLogin *bridgev2.UserLogin

	// Rustpush (primary — real-time send/receive)
	client     *rustpushgo.Client
	config     *rustpushgo.WrappedOsConfig
	users      *rustpushgo.WrappedIdsUsers
	identity   *rustpushgo.WrappedIdsngmIdentity
	connection *rustpushgo.WrappedApsConnection
	handle     string   // Primary iMessage handle used for sending (e.g., tel:+1234567890)
	allHandles []string // All registered handles (for IsThisUser checks)

	// iCloud token provider (auth for CardDAV, CloudKit, etc.)
	tokenProvider **rustpushgo.WrappedTokenProvider

	// Contact source for name resolution (iCloud or external CardDAV)
	contacts contactSource

	// sharedProfiles is the in-memory cache of shared iMessage profile records
	// (Name & Photo Sharing) received via ShareProfile / UpdateProfile
	// messages, keyed by sender identifier (e.g. "tel:+1234567890"). Values
	// are *sharedProfileRow. Fronts sharedProfileStore which persists them
	// across restarts; see pkg/connector/shared_profile.go.
	sharedProfiles     sync.Map
	sharedProfileStore *sharedProfileStore

	// statusKitPresence tracks the last-known availability state per contact
	// handle, keyed by iMessage identifier string (e.g. "tel:+1234567890").
	// Stored as bool (true = available). Used to suppress duplicate bot notices
	// when StatusKit re-delivers the same presence state on reconnect.
	statusKitPresence sync.Map // map[string]bool

	// statusKitPresenceByPortal mirrors statusKitPresence but keyed by the
	// resolved DM portal ID — i.e. canonical-per-peer regardless of which
	// alias (mailto: vs tel:) StatusKit routed the notification through.
	// Peer iOS publishes status to every alias of the same Apple ID, so
	// without this canonical dedup the bridge fires N notices per state
	// change for a peer with N registered handles.
	statusKitPresenceByPortal sync.Map // map[networkid.PortalID]string

	// statusKitPortalCache memoizes the resolved DM portal ID for a StatusKit
	// presence handle. Populated after a successful IDS correlation lookup
	// (see resolveStatusPortalViaIDS) so we only pay the IDS round trip once
	// per unresolved handle per session. Values are networkid.PortalID.
	statusKitPortalCache sync.Map // map[string]networkid.PortalID

	// statusKitIDSGate paces and adaptively backs off batch IDS lookups
	// driven by StatusKit alias resolution. Per-handle 6h negative cache
	// (statusKitIDSAttemptKeyPrefix) prevents re-querying the same handle;
	// this gate prevents a *burst* of distinct unknown handles from firing
	// validate_targets in parallel — keeping the bridge's IDS traffic
	// indistinguishable from a single user catching up on chats.
	statusKitIDSGate idsRateGate

	// sharedStreamAssetCache tracks the last observed asset GUID set per shared
	// album for this session. The Shared Streams watcher uses it to suppress
	// false-positive "new content" notices from Apple's getchanges endpoint,
	// which also fires on metadata-only album updates.
	sharedStreamAssetCache   map[string]map[string]struct{}
	sharedStreamAssetCacheMu sync.Mutex

	// sharedAlbumRooms caches dedicated Matrix rooms created for browsing
	// shared album content. Keyed by album GUID; ephemeral per session.
	sharedAlbumRooms   map[string]id.RoomID
	sharedAlbumRoomsMu sync.Mutex

	// statusKitBotRulePushed is set to true after we successfully install a
	// sender push rule via the double puppet that silences push notifications
	// from the bridge bot. Done once per session; the rule is durable on the
	// homeserver so re-installs across sessions are harmless but redundant.
	statusKitBotRulePushed atomic.Bool

	// inviteInFlight tracks handles with an in-flight StatusKit invite.
	// The DB-side dispatch latch (statusKitInvitedOkKeyPrefix) is written
	// *after* the FFI returns, so two concurrent paths (sweep + new-portal
	// hook + post-backfill hook) can both miss the latch at startup and
	// double-invoke InviteToStatusSharing for the same peer. Peer iOS
	// treats repeat invites as spam, so LoadOrStore-then-skip provides
	// in-memory single-flight per handle. Entry is deleted in a defer.
	inviteInFlight sync.Map // map[string]struct{}

	// statusKitSweepRunning is true while inviteContactsToStatusSharingOpts
	// is iterating its paced loop. The new-portal hook checks this and
	// skips during a sweep so a burst of fresh portals doesn't spawn
	// unpaced concurrent FFI calls alongside the sweep's 1.5s-paced ones.
	// The next sweep (or the soft-expired re-invite path) picks up the
	// missed handles, so skipping is safe.
	statusKitSweepRunning atomic.Bool

	// statusKitCloudPassFirstCallDone gates the inter-pass-backoff bypass
	// for the first StatusKit-CloudKit pass after process startup. If a
	// prior session left the persistent backoff state in "in-failure"
	// (e.g. the bridge was kicked from the iCloud trust circle), the user
	// would otherwise be locked out for up to statusKitPassMaxBackoff
	// even after fixing trust on another Apple device and restarting the
	// bridge. Letting one pass through per process start gives restart
	// its natural "let me try again" semantics; if that pass also fails,
	// the persisted backoff resumes and steady-state behavior takes over.
	statusKitCloudPassFirstCallDone atomic.Bool

	// statusKitPassInFlight is a single-flight guard so only one
	// syncCloudStatusKitPeers pass runs at a time. The periodic pull loop, the
	// post-backfill trigger, and the cloud-sync phases can all call it; two
	// concurrent passes would race on the shared continuation-token / pass-meta
	// rows and double the CKKS round-trips. Released in a defer.
	statusKitPassInFlight atomic.Bool

	// lastDecryptFailureRepull is the unix-millisecond time of the last CloudKit
	// re-pull forced by a presence decrypt failure (a stale peer key). Cooldown-
	// gates onPresenceDecryptFailed so a burst of undecryptable presence updates
	// can't hammer CKKS and re-trigger the clique-kick. Accessed atomically.
	lastDecryptFailureRepull int64

	// statusKitShareMu serializes the cooldown-check / publish /
	// cooldown-write sequence in publishStatusKitAvailableAfterInvite.
	// Without it two concurrent callers can both pass the cooldown check
	// and double-publish ShareStatus.
	statusKitShareMu sync.Mutex

	// lastPresenceSubscribe timestamps the most recent call to
	// subscribeToContactPresence. OnKeysReceived triggers re-subscription
	// when new keys arrive, but multiple key-sharing messages can arrive in
	// quick succession — the debounce prevents redundant APNs subscription
	// storms.
	lastPresenceSubscribe     time.Time
	lastPresenceSubscribeLock sync.Mutex

	// Trailing-edge coalescing for OnKeysReceived. A key-sharing burst (a
	// heavily-keyed account at startup fires OnKeysReceived dozens of times in
	// seconds) would otherwise trigger a full chunked SubscribeToStatus per
	// event — a storm against Apple that gets worse the more keys a user has.
	// schedulePresenceSubscribe collapses a burst into ONE re-subscribe that
	// runs after the burst settles. See schedulePresenceSubscribe.
	presenceSubscribeLock    sync.Mutex
	presenceSubscribeGen     uint64
	presenceSubscribePending bool
	presenceSubscribeFirstAt time.Time

	// statusKitInviteMu / statusKitNextInviteAt enforce a GLOBAL minimum
	// interval between StatusKit invite dispatches, shared across every caller:
	// the periodic sweep, the new-portal hook, and concurrent reconnect-
	// triggered sweeps. The sweep's per-loop delay only bounded one sweep; the
	// new-portal hook fires once per portal during CloudKit backfill and was
	// otherwise unpaced, so a fresh start could still burst Apple's IDS. Every
	// invite funnels through this gate so the aggregate rate trickles.
	statusKitInviteMu     sync.Mutex
	statusKitNextInviteAt time.Time

	// Contacts readiness gate for CloudKit message sync.
	contactsReady     bool
	contactsReadyLock sync.RWMutex
	contactsReadyCh   chan struct{}

	// CloudKit sync gate: prevents APNs messages from creating new portals
	// until the initial CloudKit sync establishes the authoritative set of
	// portals. Without this, is_from_me echoes arriving on reconnect can
	// resurrect deleted portals before CloudKit sync confirms they're gone.
	cloudSyncDone     bool
	cloudSyncDoneLock sync.RWMutex

	// cloudSyncRunning is true while any CloudKit sync cycle (bootstrap or
	// periodic re-sync) is actively paging and importing records. Used by
	// handleChatRecover to defer portal creation until messages are imported.
	cloudSyncRunning     bool
	cloudSyncRunningLock sync.RWMutex

	// Cloud backfill local cache store.
	cloudStore *cloudBackfillStore

	// Layer-2 MMCS attachment recovery: persists descriptors for attachments
	// whose push-time download exhausted the rustpushgo retry (Layer 1).
	// AttachmentRetrier drains this on a timer with its own longer-horizon
	// backoff (30s → 2m → 10m → 30m → 1h → 6h) and falls back to CloudKit
	// if the MMCS URL has expired. See pending_attachment_store.go +
	// attachment_retrier.go.
	pendingAttachments *pendingAttachmentStore
	retrierOnce        sync.Once

	// Ford key cache — reimplementation of the 94f7b8e Ford cross-batch
	// deduplication fix in Go. Populated aggressively during CloudKit
	// attachment sync from every record's `lqa.protection_info` (and
	// `avid.protection_info` for Live Photos), consulted on download to
	// recover from MMCS dedup key mismatches. See pkg/connector/ford_cache.go.
	fordCache *FordKeyCache

	// Chat.db backfill (macOS with Full Disk Access only)
	chatDB *chatDB

	// Background goroutine lifecycle
	stopChan chan struct{}

	// Unsend re-delivery suppression
	recentUnsends     map[string]time.Time
	recentUnsendsLock sync.Mutex

	// SMS reaction echo suppression: tracks UUIDs of SMS reaction messages sent
	// from Matrix so the outgoing echo from the iPhone relay is not processed as
	// a duplicate plain-text message in the Matrix room.
	recentSmsReactionEchoes     map[string]time.Time
	recentSmsReactionEchoesLock sync.Mutex

	// Outbound unsend echo suppression: tracks target UUIDs of unsends
	// initiated from Matrix so the APNs echo doesn't get double-processed.
	recentOutboundUnsends     map[string]time.Time
	recentOutboundUnsendsLock sync.Mutex

	// Outbound delete echo suppression: tracks portal IDs where a chat delete
	// SMS portal tracking: portal IDs known to be SMS-only contacts
	smsPortals     map[string]bool
	smsPortalsLock sync.RWMutex

	// Initial sync gate: closed once initial sync completes (or is skipped),
	// so real-time messages don't race ahead of backfill.

	// Group portal fuzzy-matching index: maps each member to the set of
	// group portal IDs containing that member. Lazily populated from DB.
	groupPortalIndex map[string]map[string]bool
	groupPortalMu    sync.RWMutex

	// Actual iMessage group names (cv_name) keyed by portal ID.
	// Populated from incoming messages; used for outbound routing.
	imGroupNames   map[string]string
	imGroupNamesMu sync.RWMutex

	// In-memory group participants cache keyed by portal ID.
	// Populated synchronously in makePortalKey so resolveGroupMembers
	// can find participants before the async cloud_chat DB write completes.
	imGroupParticipants   map[string][]string
	imGroupParticipantsMu sync.RWMutex

	// Persistent iMessage group UUIDs (sender_guid/gid) keyed by portal ID.
	// Populated from incoming messages; used for outbound routing so that
	// Apple Messages recipients match messages to the correct group thread.
	imGroupGuids   map[string]string
	imGroupGuidsMu sync.RWMutex

	// gidAliases maps unknown gid-based portal IDs (e.g. "gid:uuid-b") to the
	// resolved existing portal ID (e.g. "gid:uuid-a"). Populated when another
	// rustpush client (like OpenBubbles) uses a different gid for the same
	// group conversation. Avoids repeated participant-matching on each message.
	gidAliases   map[string]string
	gidAliasesMu sync.RWMutex

	// Last active group portal per member. Updated on every incoming group
	// message so typing indicators route to the correct group.
	lastGroupForMember   map[string]networkid.PortalKey
	lastGroupForMemberMu sync.RWMutex

	// queuedPortals tracks portal_id → newest_ts for portals already queued
	// this session. Prevents re-queuing on periodic syncs unless CloudKit
	// has newer messages.
	queuedPortals map[string]int64

	// recentlyDeletedPortals tracks portal IDs that were deleted this
	// session. Populated by:
	// - CloudKit tombstones (ingestCloudChats, during sync before cloudSyncDone)
	// All paths use hasMessageUUID for echo detection: known UUIDs are
	// dropped, fresh UUIDs are allowed through for new conversations.
	recentlyDeletedPortals   map[string]deletedPortalEntry
	recentlyDeletedPortalsMu sync.RWMutex

	// recycleBinCandidates stores portal IDs found in Apple's recycle bin
	// during bootstrap. NOT auto-deleted — shown to the user via bridgebot
	// notification so they can decide which to remove with !delete-stale.
	recycleBinCandidates   []recycleBinCandidate
	recycleBinCandidatesMu sync.Mutex

	// restoreMu serializes chat restores to prevent race conditions when
	// multiple restores run concurrently (e.g. undeleting DB rows, refreshing
	// metadata, and queuing ChatResync events for two portals at once).
	restoreMu sync.Mutex
	// restorePipelines tracks portals with an active restore worker.
	// Used to dedupe duplicate restore requests for the same portal.
	restorePipelines   map[string]bool
	restorePipelinesMu sync.Mutex
	// cloudSyncRunMu serializes manual restore-triggered CloudKit sync passes
	// with the main sync controller so continuation token updates don't race.
	cloudSyncRunMu sync.Mutex

	// forwardBackfillSem limits concurrent forward backfills to avoid
	// overwhelming CloudKit/Matrix with simultaneous attachment downloads.
	forwardBackfillSem chan struct{}

	// attachmentContentCache maps CloudKit record_name → *event.MessageEventContent.
	// Populated by preUploadCloudAttachments, which runs in the cloud sync
	// goroutine BEFORE createPortalsFromCloudSync. Checked first by
	// downloadAndUploadAttachment so that FetchMessages (portal event loop)
	// never blocks on a CloudKit download — it just reads the pre-built content.
	attachmentContentCache sync.Map

	// failedAttachments tracks CloudKit record_name → *failedAttachmentEntry
	// for attachments that failed to download or upload. preUploadCloudAttachments
	// retries these on delayed re-syncs (15s/60s/3min) even for portals with
	// fwd_backfill_done=1, ensuring transient failures don't cause permanent
	// attachment loss. Capped at maxAttachmentRetries to avoid infinite retries
	// on permanently corrupted CloudKit records.
	failedAttachments sync.Map

	// deadAttachments is a persistent negative cache of CloudKit record_names
	// that CloudKit no longer serves (Apple aged out the underlying MMCS blob).
	// Backed by the cloud_attachment_dead table so the skip survives restarts —
	// without it the bridge re-downloads-and-re-fails the same dead old
	// attachments on every sync sweep. Loaded once via deadAttachmentsOnce.
	// Values are struct{}.
	deadAttachments     sync.Map
	deadAttachmentsOnce sync.Once

	// startupTime records when this session connected. Used to suppress
	// read receipts for messages that pre-date this session: re-delivered
	// APNs receipts arrive with TimestampMs = delivery time (now), not the
	// actual read time, so they always show the wrong "Seen at" timestamp.
	startupTime time.Time

	// msgBuffer reorders incoming APNs messages by timestamp before dispatch.
	// APNs delivers messages grouped by sender rather than interleaved
	// chronologically, which causes out-of-order chat history in Matrix
	// (since Matrix stores events in insertion order). The buffer collects
	// messages, tapbacks, and edits, then flushes them sorted by timestamp
	// after a quiet window or when a size limit is reached.
	msgBuffer *messageBuffer

	// pendingPortalMsgs holds messages that need portal creation but arrived
	// before CloudKit sync established the authoritative set of portals.
	// Without this, the framework drops events where CreatePortal=false and
	// portal.MXID="", and the UUIDs are already persisted so CloudKit won't
	// re-deliver them. Flushed by setCloudSyncDone with CreatePortal=true.
	pendingPortalMsgs   []rustpushgo.WrappedMessage
	pendingPortalMsgsMu sync.Mutex

	// pendingInitialBackfills counts how many forward FetchMessages calls are
	// still outstanding from the bootstrap createPortalsFromCloudSync pass.
	// The APNs message buffer is held until this counter reaches 0, ensuring
	// buffered APNs messages are delivered to Matrix AFTER the CloudKit backfill
	// (not interleaved with it). Set by createPortalsFromCloudSync before the
	// first setCloudSyncDone; decremented by onForwardBackfillDone.
	pendingInitialBackfills int64

	// apnsBufferFlushedAt is the unix-millisecond time when the APNs buffer
	// was flushed after all forward backfills completed. Zero until that event.
	// Used to enforce a grace window: stale read receipts that were queued in
	// the APNs buffer are delivered immediately on flush, so we suppress all
	// read receipts for receiptGraceMs after the flush.
	apnsBufferFlushedAt int64 // atomic
}

var _ bridgev2.NetworkAPI = (*IMClient)(nil)
var _ bridgev2.EditHandlingNetworkAPI = (*IMClient)(nil)
var _ bridgev2.ReactionHandlingNetworkAPI = (*IMClient)(nil)
var _ bridgev2.ReadReceiptHandlingNetworkAPI = (*IMClient)(nil)
var _ bridgev2.TypingHandlingNetworkAPI = (*IMClient)(nil)
var _ bridgev2.IdentifierResolvingNetworkAPI = (*IMClient)(nil)
var _ bridgev2.BackfillingNetworkAPI = (*IMClient)(nil)
var _ bridgev2.BackfillingNetworkAPIWithLimits = (*IMClient)(nil)
var _ bridgev2.DeleteChatHandlingNetworkAPI = (*IMClient)(nil)
var _ rustpushgo.MessageCallback = (*IMClient)(nil)
var _ rustpushgo.UpdateUsersCallback = (*IMClient)(nil)
var _ rustpushgo.StatusCallback = (*IMClient)(nil)

// ============================================================================
// APNs message reorder buffer
// ============================================================================

const (
	// messageBufferQuietWindow is how long to wait after the last message
	// before flushing the buffer. Balances latency vs. reordering accuracy.
	messageBufferQuietWindow = 500 * time.Millisecond

	// messageBufferMaxSize is the maximum number of messages to hold before
	// force-flushing, even if the quiet window hasn't elapsed.
	messageBufferMaxSize = 50
)

// bufferedMessage is a message waiting in the reorder buffer.
type bufferedMessage struct {
	msg       rustpushgo.WrappedMessage
	timestamp uint64
}

// messageBuffer collects incoming APNs messages and dispatches them in
// chronological (timestamp) order. While CloudKit sync is in progress
// (!cloudSyncDone), messages accumulate without flushing — this prevents
// APNs messages from being dispatched before CloudKit backfill completes,
// which would cause older CloudKit messages to appear after newer APNs
// messages in Matrix. Once cloudSyncDone fires, setCloudSyncDone triggers
// a flush. After sync, normal quiet-window / max-size flushing resumes.
//
// Only regular messages, tapbacks, and edits are buffered; time-sensitive
// events (typing, read/delivery receipts) and control events (unsends,
// renames, participant changes) bypass the buffer entirely.
type messageBuffer struct {
	mu      sync.Mutex
	entries []bufferedMessage
	timer   *time.Timer
	client  *IMClient
}

// add inserts a message into the buffer. While CloudKit sync is in progress,
// or while initial forward backfills are pending, messages accumulate without
// flushing. After both conditions clear, the buffer flushes on a quiet window
// (500ms) or when the max size (50) is reached.
func (b *messageBuffer) add(msg rustpushgo.WrappedMessage) {
	b.mu.Lock()
	b.entries = append(b.entries, bufferedMessage{
		msg:       msg,
		timestamp: msg.TimestampMs,
	})

	// Hold only while CloudKit data is being downloaded (!isCloudSyncDone).
	// Once the data download is done, messages flow immediately so real-time
	// messages are never silently swallowed. The initial buffer is flushed
	// by setCloudSyncDone / onForwardBackfillDone with ordering preserved.
	if !b.client.isCloudSyncDone() {
		b.mu.Unlock()
		return
	}

	if len(b.entries) >= messageBufferMaxSize {
		if b.timer != nil {
			b.timer.Stop()
			b.timer = nil
		}
		b.mu.Unlock()
		go b.flush() // background, consistent with the time.AfterFunc quiet-window path
		return
	}

	if b.timer != nil {
		b.timer.Stop()
	}
	b.timer = time.AfterFunc(messageBufferQuietWindow, b.flush)
	b.mu.Unlock()
}

// flush sorts all buffered messages by timestamp and dispatches them.
func (b *messageBuffer) flush() {
	b.mu.Lock()
	if len(b.entries) == 0 {
		b.mu.Unlock()
		return
	}
	entries := b.entries
	b.entries = nil
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.mu.Unlock()

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].timestamp < entries[j].timestamp
	})

	for _, e := range entries {
		b.client.dispatchBuffered(e.msg)
	}
}

// stop cancels the flush timer and discards pending messages.
// Called during Disconnect — remaining messages will be re-delivered
// by CloudKit on next sync.
func (b *messageBuffer) stop() {
	b.mu.Lock()
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.entries = nil
	b.mu.Unlock()
}

// sendGhostReadReceipt sends a "they read my message" receipt from the ghost
// user with the correct iMessage read timestamp. The standard framework path
// (QueueRemoteEvent → MarkRead) also calls SetReadMarkers, but goes through
// ASIntent.MarkRead which strips BeeperReadExtra["ts"] for non-custom-puppet
// users. By calling SetReadMarkers directly on the ghost's underlying Matrix
// client we include the custom timestamp, giving Hungry the best chance of
// honoring it. Falls back to QueueRemoteEvent if the direct path fails.
func (c *IMClient) sendGhostReadReceipt(
	log *zerolog.Logger,
	ghostUserID networkid.UserID,
	portalKey networkid.PortalKey,
	guid string,
	readTime time.Time,
) {
	ctx := context.Background()

	// Step 1: Get the ghost user.
	ghost, err := c.Main.Bridge.GetGhostByID(ctx, ghostUserID)
	if err != nil || ghost == nil || ghost.Intent == nil {
		log.Warn().Err(err).Str("ghost_user_id", string(ghostUserID)).
			Msg("Ghost not found for read receipt, falling back to QueueRemoteEvent")
		c.queueGhostReceiptFallback(ghostUserID, portalKey, guid, readTime)
		return
	}

	// Step 2: Look up the portal to get its Matrix room ID.
	portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, portalKey)
	if err != nil || portal == nil || portal.MXID == "" {
		log.Debug().Err(err).Str("portal_id", string(portalKey.ID)).
			Msg("Portal not found for ghost read receipt, falling back to QueueRemoteEvent")
		c.queueGhostReceiptFallback(ghostUserID, portalKey, guid, readTime)
		return
	}

	// Step 3: Look up the target message in the bridge DB and ensure we target
	// the same room as the portal to avoid "target event in different room".
	dbMessages, err := c.Main.Bridge.DB.Message.GetAllPartsByID(ctx, portalKey.Receiver, makeMessageID(guid))
	if err != nil || len(dbMessages) == 0 {
		log.Debug().Err(err).Str("portal_id", string(portalKey.ID)).Str("guid", guid).
			Msg("Target message not in bridge DB, falling back to QueueRemoteEvent with ReadUpTo")
		c.queueGhostReceiptFallback(ghostUserID, portalKey, guid, readTime)
		return
	}
	targetMsg := dbMessages[0]
	for _, candidate := range dbMessages {
		if candidate.HasFakeMXID() {
			continue
		}
		if candidate.Room.ID == portalKey.ID && candidate.Room.Receiver == portalKey.Receiver {
			targetMsg = candidate
			break
		}
	}
	if targetMsg == nil || targetMsg.HasFakeMXID() {
		log.Debug().
			Str("portal_id", string(portalKey.ID)).
			Str("guid", guid).
			Msg("No usable Matrix event ID for ghost read receipt, falling back to QueueRemoteEvent")
		c.queueGhostReceiptFallback(ghostUserID, portalKey, guid, readTime)
		return
	}
	if targetMsg.Room.ID != portalKey.ID || targetMsg.Room.Receiver != portalKey.Receiver {
		log.Debug().
			Str("portal_id", string(portalKey.ID)).
			Str("target_room_portal", string(targetMsg.Room.ID)).
			Str("guid", guid).
			Msg("Skipping direct ghost read receipt due to portal/target room mismatch")
		c.queueGhostReceiptFallback(ghostUserID, targetMsg.Room, guid, readTime)
		return
	}

	// Step 4: Type-assert to access the ghost's underlying Matrix client
	// and call SetReadMarkers directly (bypasses IsCustomPuppet check so
	// BeeperReadExtra["ts"] is included, giving Hungry the iMessage read time).
	// Do NOT use SetBeeperInboxState here — Hungry returns HTTP 400 for ghost
	// (appservice puppet) users because they have no Beeper inbox.
	asIntent, ok := ghost.Intent.(*matrixfmt.ASIntent)
	if !ok {
		log.Warn().Str("portal_id", string(portalKey.ID)).
			Msg("Ghost intent is not *matrix.ASIntent, falling back to QueueRemoteEvent")
		c.queueGhostReceiptFallback(ghostUserID, portalKey, guid, readTime)
		return
	}

	extraData := map[string]any{
		"ts": readTime.UnixMilli(),
	}
	req := &mautrix.ReqSetReadMarkers{
		Read:                 targetMsg.MXID,
		FullyRead:            targetMsg.MXID,
		BeeperReadExtra:      extraData,
		BeeperFullyReadExtra: extraData,
	}
	err = asIntent.Matrix.SetReadMarkers(ctx, portal.MXID, req)
	if err != nil {
		log.Warn().Err(err).Str("portal_id", string(portalKey.ID)).
			Stringer("event_id", targetMsg.MXID).
			Msg("SetReadMarkers failed for ghost, falling back to QueueRemoteEvent")
		c.queueGhostReceiptFallback(ghostUserID, portalKey, guid, readTime)
		return
	}

	log.Info().
		Str("portal_id", string(portalKey.ID)).
		Str("guid", guid).
		Stringer("event_id", targetMsg.MXID).
		Int64("read_time_ms", readTime.UnixMilli()).
		Str("read_time", readTime.UTC().Format(time.RFC3339)).
		Msg("Set ghost read receipt via SetReadMarkers with correct timestamp")
}

// queueGhostReceiptFallback sends a ghost read receipt via the standard
// QueueRemoteEvent path. The homeserver may ignore BeeperReadExtra["ts"]
// (showing server time instead), but the receipt itself will still be created.
func (c *IMClient) queueGhostReceiptFallback(
	ghostUserID networkid.UserID,
	portalKey networkid.PortalKey,
	guid string,
	readTime time.Time,
) {
	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Receipt{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventReadReceipt,
			PortalKey: portalKey,
			Sender: bridgev2.EventSender{
				IsFromMe: false,
				Sender:   ghostUserID,
			},
			Timestamp: readTime,
		},
		LastTarget: makeMessageID(guid),
		ReadUpTo:   readTime,
	})
}

// onForwardBackfillDone is called when a single forward FetchMessages call
// completes (either returning early with no messages, or via CompleteCallback
// after bridgev2 delivers the batch to Matrix). It decrements the bootstrap
// pending counter and, when it reaches exactly 0, flushes the APNs buffer.
//
// Using == 0 (not <= 0) means the flush fires exactly once: the portal whose
// decrement lands at 0. Portals that cause the counter to go negative (e.g.
// a re-sync FetchMessages that we didn't account for) do not trigger a flush.
func (c *IMClient) onForwardBackfillDone() {
	remaining := atomic.AddInt64(&c.pendingInitialBackfills, -1)
	if remaining == 0 {
		log.Info().Msg("All initial forward backfills complete — flushing APNs buffer")
		atomic.StoreInt64(&c.apnsBufferFlushedAt, time.Now().UnixMilli())
		if c.msgBuffer != nil {
			c.msgBuffer.flush()
		}
		c.flushPendingPortalMsgs()

		// Re-run ghost name refresh now that all backfill ghosts are in the DB.
		// If contacts loaded before backfill finished, the earlier
		// refreshGhostNamesFromContacts call (triggered by setContactsReady)
		// may have scanned the DB before backfill ghosts existed, leaving them
		// with fallback display names. Multi-handle contacts are especially
		// affected because canonicalizeDMSender only remaps within the same
		// portal, so multi-handle senders still create additional ghosts
		// that also need display names set.
		c.contactsReadyLock.RLock()
		contactsReady := c.contactsReady
		c.contactsReadyLock.RUnlock()
		if contactsReady {
			go c.refreshGhostNamesFromContacts(log.Logger)
		}

		// Fresh-bridge path: ghosts only exist after backfill creates them.
		// subscribeAfterInit runs at connect time with zero ghosts on a fresh
		// install and returns without inviting. This post-backfill hook fires
		// the sweep once ghosts exist. Warm restart already has ghosts and
		// invites via subscribeAfterInit, so this is a no-op dup there — the
		// per-handle invite is idempotent on peer's side (re-delivery just
		// hits the server-side retry loop).
		go c.inviteContactsToStatusSharing(log.Logger)

		// StatusKit-CloudKit drain: the cloud-sync controller's three
		// delayed re-syncs (15s/75s/4m15s after bootstrap) all fire well
		// before forward backfill completes on accounts with substantial
		// history — backfill regularly takes 20+ minutes. Without an
		// explicit post-backfill trigger, syncCloudStatusKitPeers' internal
		// settle-window gate would skip every delayed re-sync and the next
		// trigger wouldn't arrive until the next bridge restart, leaving
		// peer keys un-pulled.
		//
		// Sleep slightly longer than the gate's settle window so when the
		// gate runs it definitely sees ">60s elapsed since backfill done".
		// On warm restart with a fast/empty backfill, the 12h success floor
		// will short-circuit the call — no redundant CKKS round-trip.
		go func() {
			select {
			case <-time.After(75 * time.Second):
			case <-c.stopChan:
				return
			}
			ctx := context.Background()
			skLog := c.UserLogin.Log.With().Str("component", "cloud_sync").Str("source", "post_backfill").Logger()
			if err := c.syncCloudStatusKitPeers(ctx, skLog); err != nil {
				skLog.Info().Err(err).Msg("StatusKit-CloudKit post-backfill pass: errored (continuing)")
			}
		}()
	}
}

// ============================================================================
// Lifecycle
// ============================================================================

func (c *IMClient) loadSenderGuidsFromDB(log zerolog.Logger) {
	ctx := context.Background()
	portals, err := c.Main.Bridge.GetAllPortalsWithMXID(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to load portals for sender_guid cache")
		return
	}

	// Migrate portal IDs that contain stale SMS suffixes before populating caches.
	c.migrateSmsSuffixPortals(log, ctx, portals)

	loadedGuids := 0
	for _, portal := range portals {
		if portal.Receiver != c.UserLogin.ID {
			continue // Skip portals for other users
		}
		if meta, ok := portal.Metadata.(*PortalMetadata); ok {
			if meta.SenderGuid != "" {
				c.imGroupGuidsMu.Lock()
				c.imGroupGuids[string(portal.ID)] = meta.SenderGuid
				c.imGroupGuidsMu.Unlock()
				loadedGuids++
			}
			if meta.IsSms {
				c.updatePortalSMS(string(portal.ID), true)
			}
			// NOTE: Do NOT pre-populate imGroupNames from portal metadata.
			// The metadata GroupName can be stale (polluted by previous CloudKit
			// sync cycles). Loading it on startup would cause resolveGroupName
			// and refreshGroupPortalNamesFromContacts to revert correct room
			// names back to stale values. Instead, let imGroupNames be populated
			// only by real-time APNs messages (makePortalKey / handleRename).
			// Outbound routing (portalToConversation) has its own metadata fallback.
		}
	}
	if loadedGuids > 0 {
		log.Info().Int("count", loadedGuids).Msg("Pre-populated sender_guid cache from database")
	}
}

// migrateSmsSuffixPortals re-IDs portals whose IDs contain stale Apple SMS
// suffixes like "(smsft)" or "(smsfp)". After stripSmsSuffix was added to
// portal ID normalization, new lookups produce clean IDs, orphaning existing
// portals that were created with the suffixed form. This runs once at startup
// to reconcile old portal rows with the new normalized format.
func (c *IMClient) migrateSmsSuffixPortals(log zerolog.Logger, ctx context.Context, portals []*bridgev2.Portal) {
	migrated := 0
	for _, portal := range portals {
		if portal.Receiver != c.UserLogin.ID {
			continue
		}
		oldID := string(portal.ID)

		// Strip SMS suffixes from each member in the (possibly comma-separated) portal ID.
		members := strings.Split(oldID, ",")
		changed := false
		for i, m := range members {
			stripped := stripSmsSuffix(m)
			if stripped != m {
				members[i] = stripped
				changed = true
			}
		}
		if !changed {
			continue
		}

		// Re-sort after stripping to maintain canonical order.
		sort.Strings(members)
		newID := strings.Join(members, ",")
		if newID == oldID {
			continue
		}

		oldKey := portal.PortalKey
		newKey := networkid.PortalKey{
			ID:       networkid.PortalID(newID),
			Receiver: portal.Receiver,
		}

		result, _, err := c.reIDPortalWithCacheUpdate(ctx, oldKey, newKey)
		if err != nil {
			log.Warn().Err(err).
				Str("old_portal_id", oldID).
				Str("new_portal_id", newID).
				Msg("Failed to migrate SMS-suffixed portal ID")
			continue
		}
		log.Info().
			Str("old_portal_id", oldID).
			Str("new_portal_id", newID).
			Int("result", int(result)).
			Msg("Migrated SMS-suffixed portal ID")
		migrated++
	}
	if migrated > 0 {
		log.Info().Int("count", migrated).Msg("Finished migrating SMS-suffixed portal IDs")
	}
}

// safeRestoreTokenProvider wraps RestoreTokenProvider with a panic recovery.
// Uniffi converts Rust panics to Go panics (CALL_UNEXPECTED_ERROR); without
// recovery here the whole process crashes instead of degrading gracefully.
func safeRestoreTokenProvider(
	config *rustpushgo.WrappedOsConfig,
	conn *rustpushgo.WrappedApsConnection,
	username, hashedPwHex, pet, spdBase64 string,
) (tp *rustpushgo.WrappedTokenProvider, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("RestoreTokenProvider panicked: %v", r)
		}
	}()
	return rustpushgo.RestoreTokenProvider(config, conn, username, hashedPwHex, pet, spdBase64)
}

func (c *IMClient) Connect(ctx context.Context) {
	c.startupTime = time.Now()
	log := c.UserLogin.Log.With().Str("component", "imessage").Logger()
	c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnecting})

	rustpushgo.InitLogger()

	// Validate that the software keystore still has the signing keys referenced
	// by the saved user state.  If the keystore file was deleted/reset while the
	// bridge DB kept the old state, every IDS operation would fail with
	// "Keystore error Key not found".  Detect this early and ask the user to
	// re-login instead of producing a cryptic send-time error.
	if c.users != nil && !c.users.ValidateKeystore() {
		log.Error().Msg("Keystore keys missing for saved user state — clearing stale login, please re-login")
		meta := c.UserLogin.Metadata.(*UserLoginMetadata)
		meta.IDSUsers = ""
		meta.IDSIdentity = ""
		meta.APSState = ""
		if err := c.UserLogin.Save(ctx); err != nil {
			log.Error().Err(err).Msg("Failed to persist cleared login state after key loss")
		}
		c.UserLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateBadCredentials,
			Message:    "Signing keys lost — please re-login to iMessage",
		})
		return
	}

	// Restore token provider from persisted credentials if not already set
	if c.tokenProvider == nil || *c.tokenProvider == nil {
		meta := c.UserLogin.Metadata.(*UserLoginMetadata)
		if meta.AccountUsername != "" && meta.AccountPET != "" && meta.AccountSPDBase64 != "" {
			log.Info().Msg("Restoring iCloud TokenProvider from persisted credentials")
			tp, err := safeRestoreTokenProvider(c.config, c.connection,
				meta.AccountUsername, meta.AccountHashedPasswordHex,
				meta.AccountPET, meta.AccountSPDBase64)
			if err != nil {
				log.Warn().Err(err).Msg("Failed to restore TokenProvider — cloud services unavailable")
			} else {
				c.tokenProvider = &tp
				// Seed the persisted MobileMe delegate so CloudKit / keychain
				// ops have something to work with on first use. The wrapper's
				// RestoreTokenProvider path intentionally returns a
				// WrappedTokenProvider with empty mme_delegate_bytes (see
				// pkg/rustpushgo/src/lib.rs::restore_token_provider — "callers
				// must seed_mme_delegate_json() from persisted state before
				// using keychain/contacts features"), so we seed it here. The
				// delegate is whatever was captured during the most recent
				// successful login; if it's expired, CloudKit calls will
				// surface an auth error and the user can re-login.
				if meta.MmeDelegateJSON != "" {
					if seedErr := tp.SeedMmeDelegateJson(meta.MmeDelegateJSON); seedErr != nil {
						log.Warn().Err(seedErr).Msg("Failed to seed persisted MobileMe delegate — CloudKit unavailable until re-login")
					} else {
						log.Info().Msg("Seeded persisted MobileMe delegate into restored TokenProvider")
					}
				} else {
					log.Warn().Msg("TokenProvider restored but no persisted MobileMe delegate — CloudKit unavailable until re-login captures a fresh one")
				}
			}
		}
	}

	client, err := rustpushgo.NewClient(c.connection, c.users, c.identity, c.config, c.tokenProvider, c, c)
	if err != nil {
		log.Err(err).Msg("Failed to create rustpush client")
		c.UserLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateBadCredentials,
			Message:    fmt.Sprintf("Failed to connect: %v", err),
		})
		return
	}
	c.client = client

	// Hydrate the persistent alias→portal cache and pre-warm from the ghost
	// table so the first StatusKit presence update after a restart can
	// resolve previously-known aliases without IDS round-trips. Both
	// helpers are read-only and safe to run before StatusKit init.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Warn().Interface("panic", r).Msg("StatusKit alias-resolver init panicked")
			}
		}()
		bgCtx := context.Background()
		c.hydrateAliasPortalCacheFromKV(bgCtx, log)
		c.prewarmAliasPortalCache(bgCtx, log)
	}()

	// GSA /circle "Apple Device" announce intentionally DISABLED.
	//
	// update_postdata("Apple Device", ["icloud","imessage","facetime"]) posts
	// to gsas.apple.com/grandslam/GsService2/postdata declaring the bridge as
	// a full Apple Device with FaceTime enrolled on the iCloud account.
	// Empirically this hijacks the account's FT enrollment: the user's real
	// Mac stopped receiving the "This Mac has access to FaceTime" confirmation,
	// inbound calls connect but peer can't see the bridge's video, and outbound
	// calls connect without media. Signaling (link tap, LetMeIn approve,
	// JoinEvent) keeps working because that rides IDS, but Apple's server-side
	// FT media negotiation routes to the announce-declared device profile
	// which the bridge can't actually fulfill.
	//
	// The original motivation (StatusKit reshare eligibility on peer iOS) is
	// carried by the 4-service IDS register bundle (MADRID+MULTIPLEX+FACETIME+
	// VIDEO) landed in ce0c90bf — peer iOS gates reshare on the IDS identity
	// shape, not on the GSA /circle enrollment.
	//
	// The Rust-side announce_apple_device_if_needed method is left in place
	// (unused) so re-enabling is a one-line change if we ever find a safe
	// device_name / services combination that doesn't disturb FT.

	// Get our handle (precedence: config > login metadata > first handle)
	handles := client.GetHandles()
	c.allHandles = handles
	if len(handles) > 0 {
		c.handle = handles[0]
		preferred := c.Main.Config.PreferredHandle
		if preferred == "" {
			if meta, ok := c.UserLogin.Metadata.(*UserLoginMetadata); ok {
				preferred = meta.PreferredHandle
			}
		}
		if preferred != "" {
			found := false
			for _, h := range handles {
				if h == preferred {
					c.handle = h
					found = true
					break
				}
			}
			if !found {
				log.Warn().Str("preferred", preferred).Strs("available", handles).
					Msg("Preferred handle not found among registered handles, using first available")
			}
		} else {
			log.Warn().Strs("available", handles).
				Msg("No preferred_handle configured — using first available. Run the install script to select one.")
		}
	}

	// Persist the selected handle to metadata so it's stable across restarts.
	if c.handle != "" {
		if meta, ok := c.UserLogin.Metadata.(*UserLoginMetadata); ok && meta.PreferredHandle != c.handle {
			meta.PreferredHandle = c.handle
			log.Info().Str("handle", logSafeHandle(c.handle)).Msg("Persisted selected handle to metadata")
		}
	}

	log.Info().Str("selected_handle", logSafeHandle(c.handle)).Strs("handles", logSafeHandles(handles)).Msg("Connected to iMessage")

	// Pre-mint the OpenBubbles-style rotating FaceTime link slots ("next"
	// for outbound, "nextincomingcall" for inbound). Done in the
	// background because it's network-dependent and not load-bearing for
	// connect; a failed pre-mint means the first outbound/inbound call
	// mints on-demand (same as legacy behavior). With the bridge's long
	// uptime, pre-minting here gives Apple's FT server days of
	// identity-propagation time before the link is actually used.
	// See lastFullConnectKVKey: skip the Apple-heavy IDS sweep (FT pre-mint +
	// StatusKit contact invites) when a full connect happened within the
	// cooldown — a reconnect/rebuild must not re-fire that burst at Apple
	// mid-storm. Degrades safe: any KV/parse miss → treated as "not recent" →
	// runs exactly as before. Receiving is unaffected (handled by the APS
	// reconnect + StatusKit subscribe, neither gated here).
	skipHeavyIDSSweep := false
	if raw := c.Main.Bridge.DB.KV.Get(context.Background(), lastFullConnectKVKey); raw != "" {
		if last, perr := time.Parse(time.RFC3339, raw); perr == nil && time.Since(last) < fullConnectIDSCooldown {
			skipHeavyIDSSweep = true
			log.Info().Time("last_full_connect", last).Msg("Skipping FT pre-mint + StatusKit invite sweep — full connect within cooldown (avoids a redundant mid-reconnect IDS burst to Apple)")
		}
	}
	if !skipHeavyIDSSweep {
		c.Main.Bridge.DB.KV.Set(context.Background(), lastFullConnectKVKey, time.Now().Format(time.RFC3339))
	}

	if c.handle != "" && !skipHeavyIDSSweep {
		go func(handle string) {
			ft, ftErr := c.client.GetFacetimeClient()
			if ftErr != nil {
				log.Warn().Err(ftErr).Msg("Pre-mint FaceTime links: GetFacetimeClient failed; links will be minted on demand")
				return
			}
			premintFaceTimeLinks(ft, handle)
			log.Info().Str("handle", logSafeHandle(handle)).Msg("Pre-minted FaceTime link slots (next, nextincomingcall)")
		}(c.handle)
	}

	if c.Main.Config.VideoTranscoding {
		if ffmpeg.Supported() {
			log.Info().Msg("Video transcoding enabled (ffmpeg found)")
		} else {
			log.Warn().Msg("Video transcoding enabled in config but ffmpeg not found — install ffmpeg to enable video conversion")
		}
	}

	if c.Main.Config.HEICConversion {
		log.Info().Msg("HEIC conversion enabled (libheif)")
	}

	// Persist state after connect (APS tokens, IDS keys, device ID)
	c.persistState(log)

	// Reset StatusKit APNs channel cursors to 1 BEFORE init so the client
	// loads the reset state and APNs replays the current presence for each
	// contact on the next subscription. Keys are preserved.
	if c.client != nil {
		c.client.ResetStatuskitCursors()
	}

	// Initialize StatusKit presence system (non-fatal — runs in background).
	// Once initialized, the Rust receive loop intercepts StatusKit APNs
	// messages and invokes OnStatusUpdate for subscribed handles.
	go func() {
		if c.client == nil {
			return
		}
		// Wrapped in a 30s timeout to prevent silent goroutine hangs if the
		// Rust future (StatusKitClient::new → request_topics) never completes.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Warn().Interface("panic", r).Msg("StatusKit init panicked — treating as init failure")
					done <- fmt.Errorf("statuskit init panicked: %v", r)
				}
			}()
			done <- c.client.InitStatuskit(c)
		}()
		subscribeAfterInit := func(err error) {
			// Guard against Disconnect having nil'd c.client during the
			// init window (e.g. bridge shutdown or reconnect cycle while
			// InitStatuskit was still running past its 30s timeout).
			if c.client == nil {
				return
			}
			// Registration state is independent of StatusKit init — log it
			// unconditionally so we can see MULTIPLEX presence even when
			// init fails. If MULTIPLEX is absent, key exchange cannot work
			// regardless of how many invites we send.
			services := c.client.GetRegisteredServices()
			multiplexPresent := false
			for _, s := range services {
				if s == "com.apple.private.alloy.multiplex1" {
					multiplexPresent = true
					break
				}
			}
			log.Info().
				Bool("multiplex_registered", multiplexPresent).
				Strs("registered_services", services).
				Bool("init_ok", err == nil).
				Msg("StatusKit startup")

			if err != nil {
				log.Warn().Err(err).Msg("StatusKit initialization failed — presence updates unavailable")
				return
			}
			log.Info().Msg("StatusKit presence system initialized")
			// subscribeToContactPresence may have raced ahead of InitStatuskit
			// and failed with "StatusKit not initialized". Re-run it now that
			// the StatusKit client is guaranteed to be ready.
			c.subscribeToContactPresence(log)
			// Delayed re-subscribe (~5s): catches peers whose reshare landed
			// in the brief window between StatusKitClient::new spawning the
			// APNs recv loop and the Rust-side status_callback being installed.
			// Reshares in that window have their on_keys_received() call
			// silently dropped by the `if let Some(cb)` guard in the recv loop;
			// the bridge has the keys but never re-subscribes for that peer's
			// presence channel. The 5s delay is comfortably larger than the
			// callback-install gap (sub-millisecond on typical hardware) and
			// the first round of inbound reshares (peer-network RTT bounded).
			go func() {
				time.Sleep(5 * time.Second)
				if c.client == nil {
					return
				}
				log.Info().Msg("StatusKit: delayed re-subscribe (catches install-callback race)")
				c.subscribeToContactPresence(log)
			}()
			// Fan-out StatusKit invites matching OB-Android's shape: one
			// handle per IDS message, paced with a small inter-send delay.
			// OB invites per-chat-activation (and on app resume); bridge
			// approximates with a startup sweep. Empirically the batched
			// 23-handles-in-one-invite call we used previously appeared to
			// either trigger peer-side filtering or not propagate to each
			// peer's distribution set the way one-at-a-time invites do —
			// 12h on passive-only yielded zero real-iOS reshares, whereas
			// OB (which invites one at a time) gets reshares fine.
			// Gated by the full-connect cooldown (see lastFullConnectKVKey): a
			// reconnect/rebuild must not re-fire this 20+-handle IDS invite burst
			// at Apple. skipHeavyIDSSweep is captured from Connect's scope.
			if !skipHeavyIDSSweep {
				go c.inviteContactsToStatusSharing(log)
			}
			// Share status "available" once at startup. Empirically, peer iOS
			// reciprocates a share with its own reshare, which is what gives
			// us the key material to decrypt their subsequent presence
			// updates. OB-Android only calls share_status from its zen-mode
			// hooks (StatusQuery.kt on OS DND change) — but bridge has no OS
			// DND to hook, and gating share behind a bot command puts a
			// per-user setup step in the way that most users won't discover.
			// So: share "available" unconditionally on startup. Bridge has no
			// Focus mode of its own; "available" is the only truthful state.
			// Gated on statuskit_share_on_startup (default true, user-overridable).
			if c.Main.Config.StatusKitShareOnStartup {
				go func() {
					defer func() {
						if r := recover(); r != nil {
							log.Warn().Interface("panic", r).Msg("StatusKit startup share panicked")
						}
					}()
					sk, err := c.client.GetStatuskitClient()
					if err != nil || sk == nil {
						log.Debug().Err(err).Msg("StatusKit startup share skipped — client not ready")
						return
					}
					if err := c.safeRefreshPetTokenThrottled(); err != nil {
						log.Debug().Err(err).Msg("StatusKit startup share: PET refresh skipped")
					}
					shareErr := sk.ShareStatus(true, nil)
					if shareErr != nil && strings.Contains(shareErr.Error(), "Auth Missing") &&
						c.tokenProvider != nil && *c.tokenProvider != nil {
						// Warm-restore (restore_token_provider) injects ONLY the PET;
						// the full GSA token map — including com.apple.gs.sharedchannels.auth
						// that share_status needs — is repopulated only by a full
						// login_email_pass. The GSA-announce prime that used to do that
						// is disabled, and upstream get_token won't lazily fetch an
						// *absent* token (it only re-logs-in for an expired-but-present
						// one), so the map can lack sharedchannels.auth → "Auth Missing".
						// The proactive refresh above is throttle-skipped on a quick
						// restart, so force a full refresh here to rebuild the map,
						// then retry once. One prime fixes the whole process.
						//
						// This deliberately bypasses the proactive 5-minute throttle
						// (the warm-restore map is genuinely incomplete EVERY restart,
						// so the throttle would wrongly leave us silent). To still
						// protect against a crash-restart loop hammering Apple's GSA
						// login, honor a short floor: skip the prime if any token
						// refresh ran in the last 60s. Matches the CloudKit recovery's
						// 60s min-interval; a normal restart (minutes apart) clears it,
						// a tight crash loop (seconds) is bounded to ~1 login/60s.
						const statusKitPrimeFloor = 60 * time.Second
						recentRefresh := false
						if raw := c.Main.Bridge.DB.KV.Get(context.Background(), petRefreshKVKey); raw != "" {
							if ts, perr := strconv.ParseInt(raw, 10, 64); perr == nil && time.Since(time.Unix(ts, 0)) < statusKitPrimeFloor {
								recentRefresh = true
							}
						}
						if recentRefresh {
							log.Warn().Msg("StatusKit startup share hit Auth Missing, but a token refresh ran <60s ago — skipping prime to avoid hammering Apple GSA (crash-loop guard)")
						} else {
							log.Info().Msg("StatusKit startup share hit Auth Missing — priming GSA tokens (warm-restore map is PET-only) and retrying")
							if rErr := safeRefreshPetToken(*c.tokenProvider); rErr != nil {
								log.Warn().Err(rErr).Msg("StatusKit startup share: GSA token prime failed")
							} else {
								c.Main.Bridge.DB.KV.Set(context.Background(), petRefreshKVKey, strconv.FormatInt(time.Now().Unix(), 10))
								shareErr = sk.ShareStatus(true, nil)
							}
						}
					}
					if shareErr != nil {
						log.Warn().Err(shareErr).Msg("StatusKit startup share_status failed")
						return
					}
					// Stamp the post-invite cooldown key so the sweep's
					// publishStatusKitAvailableAfterInvite (which fires
					// seconds later when the bootstrap sweep finishes
					// invites) coalesces with this publish instead of
					// firing a redundant ShareStatus. Restart-within-5min
					// is the only case that "loses" a publish; the next
					// state change or sweep republishes.
					c.Main.Bridge.DB.KV.Set(context.Background(), database.Key(statusKitLastPostInviteShareKey), time.Now().Format(time.RFC3339))
					log.Info().Msg("StatusKit startup share_status(available) published")
				}()
			} else {
				log.Info().Msg("StatusKit startup share disabled via config (statuskit_share_on_startup: false)")
			}

			// Complement the `StatusKit startup` line above with the peer-key
			// count, which is only available once the StatusKit client is ready.
			if sk, skErr := c.client.GetStatuskitClient(); skErr == nil && sk != nil {
				log.Info().Int("known_peer_keys", len(sk.GetKnownHandles())).Msg("StatusKit peer keys loaded")
			}
		}
		select {
		case err := <-done:
			subscribeAfterInit(err)
		case <-ctx.Done():
			// The Rust FFI call (StatusKitClient::new → request_topics) is
			// taking longer than 30s. Don't block the connect flow, but keep
			// a goroutine alive to subscribe as soon as it eventually finishes.
			// The Rust side WILL set shared_statuskit/status_callback once
			// StatusKitClient::new completes; we just need to subscribe then.
			log.Warn().Msg("StatusKit initialization taking >30s — will subscribe when ready")
			go func() { subscribeAfterInit(<-done) }()
		}
	}()

	// Pre-populate sender_guid cache from existing portal metadata
	go c.loadSenderGuidsFromDB(log)

	// Start periodic state saver (every 5 minutes)
	c.stopChan = make(chan struct{})
	c.msgBuffer = &messageBuffer{client: c}
	go c.periodicStateSave(log)
	go c.periodicPetRefresh(log)
	go c.periodicStatusSharingReinvite(log)
	go c.startSharedStreamsWatcher(log)

	// Ensure shared-profile schema and hydrate the in-memory cache from the
	// DB. Runs on every bridge start so existing installs pick up the table
	// without needing a fresh login or reconfiguration.
	if err := c.ensureSharedProfileSchema(context.Background()); err != nil {
		log.Warn().Err(err).Msg("Failed to ensure shared_profiles schema")
	} else {
		c.loadSharedProfilesIntoCache(context.Background(), log)
		// Independent of CardDAV: push cached state to ghosts immediately
		// and re-fetch each row from CloudKit. Decoupled from
		// setContactsReady so a slow MobileMe-delegate retry doesn't gate
		// the share-profile path (it only depends on ProfilesClient /
		// keychain init, not on contacts).
		go c.refreshSharedProfilesOnConnect(log)
	}

	// Ensure Layer-2 MMCS attachment retry schema and spawn the background
	// worker. The worker is cheap when the queue is empty (one DB SELECT per
	// tick) so running it unconditionally matches the shared_profiles pattern.
	// sync.Once guards against Connect being re-invoked (reconnect, relogin,
	// session restore) — two retrier goroutines on the same table would race
	// on GetDue and emit duplicate edits for the same attachment.
	if c.pendingAttachments != nil {
		if err := c.pendingAttachments.ensureSchema(context.Background()); err != nil {
			log.Warn().Err(err).Msg("Failed to ensure pending_attachment_retry schema")
		} else {
			c.retrierOnce.Do(func() {
				go (&attachmentRetrier{Client: c}).Run(c.stopChan)
			})
		}
	}

	// Ensure CloudKit backfill schema/storage is available.
	cloudStoreReady := true
	if err = c.ensureCloudSyncStore(context.Background()); err != nil {
		cloudStoreReady = false
		log.Error().Err(err).Msg("Failed to initialize cloud backfill store")
	} else {
		// Fix any group messages that were mis-routed to the wrong portal
		// (e.g., self-chat) due to the ";+;" CloudChatId routing bug.
		if healed, healErr := c.cloudStore.healMisroutedGroupMessages(context.Background()); healErr != nil {
			log.Warn().Err(healErr).Msg("Failed to heal mis-routed group messages")
		} else if healed > 0 {
			log.Info().Int("healed", healed).Msg("Healed mis-routed group messages at startup")
		}
	}
	c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})

	// Backstop the APNs receive path: if it goes fully silent (no frames, not
	// even keepalive Pongs) for far longer than a healthy link ever would,
	// auto-rebuild the client. This recovers the daily "can send but stopped
	// receiving" reconnect-storm stall without a manual restart. Passed
	// c.stopChan by value so it tracks this connect epoch.
	go c.runReceiveWedgeWatchdog(c.stopChan, log.With().Str("component", "receive_wedge_watchdog").Logger())

	// Eagerly silence bridge bot push notifications so the rule is in place
	// before any bot message (StatusKit notices, admin messages, etc.) fires.
	// Run in a goroutine to avoid blocking Connect on the homeserver round-trip.
	go c.ensureBotPushRuleSilenced(ctx)

	go c.maybeSendManagementRoomWelcome(context.Background(), log)

	// Set up contact source: external CardDAV > local macOS > iCloud CardDAV
	if c.Main.Config.CardDAV.IsConfigured() {
		extContacts := newExternalCardDAVClient(c.Main.Config.CardDAV, log)
		if extContacts != nil {
			c.contacts = extContacts
			log.Info().Str("email", logSafeHandle(c.Main.Config.CardDAV.Email)).Msg("Using external CardDAV for contacts")
			if syncErr := c.contacts.SyncContacts(log); syncErr != nil {
				log.Warn().Err(syncErr).Msg("Initial external CardDAV sync failed")
			} else {
				c.setContactsReady(log)
			}
			go c.periodicCloudContactSync(log)
		} else {
			log.Warn().Msg("External CardDAV configured but failed to initialize")
		}
	} else if c.Main.Config.UseChatDBBackfill() {
		// Chat.db mode: use local macOS Contacts (no iCloud dependency).
		c.contacts = newLocalContactSource(log)
		if c.contacts != nil {
			if syncErr := c.contacts.SyncContacts(log); syncErr != nil {
				log.Warn().Err(syncErr).Msg("Initial local contacts sync failed")
			} else {
				c.setContactsReady(log)
			}
		} else {
			log.Warn().Msg("Local macOS contacts unavailable — contact names will not be resolved")
		}
	} else {
		cloudContacts := newCloudContactsClient(c.client, log)
		if cloudContacts != nil {
			c.contacts = cloudContacts
			log.Info().Str("url_host", logSafeURL(cloudContacts.baseURL)).Msg("Cloud contacts available (iCloud CardDAV)")
			if syncErr := cloudContacts.SyncContacts(log); syncErr != nil {
				log.Warn().Err(syncErr).Msg("Initial CardDAV sync failed")
			} else {
				c.setContactsReady(log)
				c.persistMmeDelegate(log)
			}
			go c.periodicCloudContactSync(log)
		} else {
			// No cloud contacts available — retry periodically.
			// The MobileMe delegate may have been expired on startup;
			// periodic retries will pick up a fresh delegate once available.
			log.Warn().Msg("Cloud contacts unavailable on startup, will retry periodically")
			go c.retryCloudContacts(log)
		}
	}

	if c.Main.Config.UseChatDBBackfill() {
		// Chat.db backfill: read local macOS Messages database
		c.chatDB = openChatDB(log)
		if c.chatDB != nil {
			log.Info().Msg("Chat.db available for backfill")
			go c.runChatDBInitialSync(log)
		} else {
			log.Warn().Msg("Chat.db backfill configured but chat.db not accessible")
		}
		// No CloudKit gate needed — open immediately
		c.setCloudSyncDone()
	} else if cloudStoreReady && c.Main.Config.UseCloudKitBackfill() {
		c.startCloudSyncController(log)
		// Pass c.stopChan by value so the goroutine captures the current
		// channel at launch — Disconnect closes that channel and the loop
		// exits deterministically. The next Connect launches a fresh
		// goroutine against the new stopChan; an idempotent scrubber UPDATE
		// means overlapping ticks during transient reconnect are harmless.
		// sync.Once is wrong here because it never re-fires post-Disconnect.
		// Skip the scrubber entirely when privacy is disabled for debugging —
		// the scrub functions already no-op in that mode, but not launching the
		// loop avoids pointless 5-minute ticks.
		if !debugDisablePrivacy {
			go c.runBodyScrubLoop(log.With().Str("component", "body_scrub").Logger(), c.stopChan)
		}
		// Periodically re-run the StatusKit-CloudKit peer-key pull. The cloud
		// sync only fires it at startup; without this loop, peer keys that
		// reshare in later (the common case on fresh/large accounts) are never
		// pulled until the next restart. Self-gated to a gentle cadence — see
		// runStatusKitPullLoop / statusKitFreshFloor.
		go c.runStatusKitPullLoop(log.With().Str("component", "statuskit_pull").Logger(), c.stopChan)
	} else {
		if !c.Main.Config.CloudKitBackfill {
			log.Info().Msg("CloudKit backfill disabled by config — skipping cloud sync")
		}
		// No CloudKit — open the APNs portal-creation gate immediately
		// so real-time messages can create portals without waiting.
		c.setCloudSyncDone()
	}

}

// maybeSendManagementRoomWelcome posts a one-time welcome notice to the user's
// management room on first successful connect, then sets WelcomeSent in
// UserLoginMetadata so subsequent connects don't repost. The room itself is
// auto-created by bridgev2's GetManagementRoom on first call.
func (c *IMClient) maybeSendManagementRoomWelcome(ctx context.Context, log zerolog.Logger) {
	meta, ok := c.UserLogin.Metadata.(*UserLoginMetadata)
	if !ok || meta.WelcomeSent {
		return
	}
	mgmtRoom, err := c.UserLogin.User.GetManagementRoom(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Welcome: failed to get management room")
		return
	}
	prefix := c.Main.Bridge.Config.CommandPrefix
	markdown := fmt.Sprintf(managementRoomWelcomeMarkdown, prefix)
	content := format.RenderMarkdown(markdown, true, false)
	if _, err := c.Main.Bridge.Bot.SendMessage(ctx, mgmtRoom, event.EventMessage, &event.Content{Parsed: content}, nil); err != nil {
		log.Warn().Err(err).Str("management_room", string(mgmtRoom)).Msg("Welcome: failed to send welcome message")
		return
	}
	meta.WelcomeSent = true
	if err := c.UserLogin.Save(ctx); err != nil {
		log.Warn().Err(err).Msg("Welcome: failed to persist welcome_sent flag")
		return
	}
	log.Info().Str("management_room", string(mgmtRoom)).Msg("Welcome: posted management-room welcome notice")
}

// managementRoomWelcomeMarkdown is the body posted on first connect. The
// single %s (used as %[1]s) is the bridge command prefix (e.g. "!im").
const managementRoomWelcomeMarkdown = `### Welcome to your iMessage bridge

You're signed in. This is your **management room** — the bot uses it to deliver bridge-wide notices and it's where you run bridge commands.

#### Get started

- ` + "`start-chat`" + ` — open a new chat. Type ` + "`start-chat`" + ` and the bot asks ` + "`1` (phone)" + ` or ` + "`2` (email)" + `, then prompts for the value. For phones, include the country code with a leading ` + "`+`" + ` — e.g. ` + "`+15551234567`" + ` for the US, ` + "`+442079460958`" + ` for the UK. Spaces, dashes, and parens get stripped automatically. You can also skip the picker by passing the identifier directly: ` + "`start-chat +15551234567`" + ` or ` + "`start-chat someone@icloud.com`" + `.
- ` + "`contacts`" + ` — sync names from your iCloud contacts so chats show real names instead of phone numbers.
- ` + "`restore-chat`" + ` — bring back a deleted iMessage chat. Lists everything that can be restored; reply with the number you want.
- ` + "`help`" + ` — every available command, grouped by section.

#### Sign out cleanly

- ` + "`logout`" + ` — sign out of the bridge. Lists your active handles; reply with a number (or ` + "`all`" + `) to confirm. The bot tells you exactly how to finish the cleanup at appleid.apple.com so Apple stops treating the bridge as a registered device.

#### Where to type commands

- **Here in the management room**: type the command bare — ` + "`start-chat`" + `, ` + "`help`" + `, ` + "`logout`" + `.
- **Inside a chat room** (a DM or group): prefix with ` + "`%[1]s`" + ` — e.g. ` + "`%[1]s facetime`" + ` to start a call from a DM.
- To abort an interactive command (like the ` + "`start-chat`" + ` picker): type ` + "`cancel`" + ` here, or ` + "`%[1]s cancel`" + ` from a portal room.

#### Notices the bot will post here

- FaceTime invites, missed-call notices, and "answered elsewhere" updates when there's no portal yet
- Chat-restore confirmations and recycle-bin activity
- StatusKit / presence updates
- Shared-album / Shared Streams activity

Run ` + "`help`" + ` any time for the full command list.
`

func (c *IMClient) Disconnect() {
	if c.msgBuffer != nil {
		c.msgBuffer.stop()
	}
	if c.stopChan != nil {
		close(c.stopChan)
		c.stopChan = nil
	}
	// Close the APNs ResourceManager before stopping the Client. The death
	// signal aborts any ongoing generate() reconnect attempt inside the Tokio
	// runtime, freeing worker threads that would otherwise prevent client.Stop()
	// from being scheduled. Without this ordering, a prolonged reconnect storm
	// can exhaust all Tokio workers and cause client.Stop() to block forever.
	//
	// Do NOT nil c.connection here — the watchdog goroutine may still be in its
	// 5-second poll sleep and will call SecondsSinceLastInbound() before it sees
	// the stopChan close. Close() is safe to call on a live WrappedApsConnection;
	// it only signals the ResourceManager to stop, it does not free the Go object.
	if c.connection != nil {
		c.connection.Close()
	}
	if c.client != nil {
		// Stop() is an async Rust/UniFFI call. Wrap it with a timeout so that a
		// wedged Tokio runtime cannot block the reconnect path indefinitely.
		stopDone := make(chan struct{})
		go func() {
			c.client.Stop()
			close(stopDone)
		}()
		select {
		case <-stopDone:
		case <-time.After(10 * time.Second):
			c.UserLogin.Log.Warn().Msg("client.Stop() timed out during disconnect; proceeding to destroy")
		}
		c.client.Destroy()
		c.client = nil
	}
	if c.chatDB != nil {
		c.chatDB.Close()
		c.chatDB = nil
	}
}

func (c *IMClient) IsLoggedIn() bool {
	return c.client != nil
}

// receiveWedgeRecoverySecs: if NO inbound APS frame arrives for this long —
// not even the ~60s keepalive Pong — the receive path is wedged (a reconnect
// storm or half-open socket that the in-Rust 300s request_update couldn't
// clear), and we escalate to a full client rebuild. 10 min is well past the
// 60s Pong cadence and the 300s cheap recovery, so a healthy or merely-quiet
// link (which always sees Pongs) never trips it.
const receiveWedgeRecoverySecs uint64 = 600

// A reconnect/rebuild re-runs Connect, which re-fires the Apple-heavy IDS work:
// the FaceTime link pre-mint and the StatusKit contact-invite sweep (a 20+-handle
// per-handle IDS query burst). Mid-reconnect-storm that burst has been observed
// escalating to a temporary Apple account disable. Gate it behind a cooldown so a
// Connect within this window of the last full one reuses the recent sweep instead
// of re-querying Apple. Receive recovery (APS reconnect + StatusKit subscribe) is
// NOT gated — only the outbound invite/mint sweep. Global key (single-login bridge,
// matching the existing statuskit/pet KV keys).
const lastFullConnectKVKey = database.Key("im.last_full_connect")

// Must exceed the wedge-rebuild cycle (≈10-min wedge threshold + 5-min reconnect
// delay ≈ 15 min) so consecutive auto-rebuilds reliably skip the sweep instead of
// landing right on the boundary; 30 min also still allows a periodic real sweep.
const fullConnectIDSCooldown = 30 * time.Minute

// runReceiveWedgeWatchdog watches the connection-level "seconds since last
// inbound frame" signal and, on a prolonged wedge, asks bridgev2 to rebuild
// the client by reporting StateUnknownError (→ recreateClient → Connect,
// bounded by UnknownErrorMaxAutoReconnects so it can't loop forever). It is the
// automated form of the manual restart that recovers the daily "can send but
// stopped receiving" stall. One-shot: after firing it returns; the rebuilt
// client starts a fresh watchdog. Gated by `stop` (taken by value so it tracks
// this connect epoch, like the body-scrub loop) so a normal teardown ends it.
// Polls c.connection (which Disconnect only closes, never destroys), so the
// poll can never touch a freed handle.
func (c *IMClient) runReceiveWedgeWatchdog(stop chan struct{}, log zerolog.Logger) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			conn := c.connection
			if conn == nil {
				continue
			}
			idle := conn.SecondsSinceLastInbound()
			if idle >= receiveWedgeRecoverySecs {
				log.Warn().
					Uint64("idle_secs", idle).
					Msg("APNs receive path wedged far past the keepalive cadence and the in-process reconnect — forcing a full client rebuild (StateUnknownError → recreateClient → Connect)")
				c.UserLogin.BridgeState.Send(status.BridgeState{
					StateEvent: status.StateUnknownError,
					Error:      "im-receive-wedged",
					Message:    "iMessage stopped receiving; reconnecting",
				})
				return
			}
		}
	}
}

func (c *IMClient) LogoutRemote(ctx context.Context) {
	c.Disconnect()
}

func (c *IMClient) IsThisUser(_ context.Context, userID networkid.UserID) bool {
	return c.isMyHandle(string(userID))
}

func (c *IMClient) GetCapabilities(ctx context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	base := caps
	if portal.RoomType == database.RoomTypeDM {
		base = capsDM
	}
	if !c.Main.Config.ReadReceipts || !c.Main.Config.TypingNotifications {
		modified := *base
		modified.ReadReceipts = c.Main.Config.ReadReceipts
		modified.TypingNotifications = c.Main.Config.TypingNotifications
		// Vary the capability ID so bridgev2's UpdateCapabilities re-advertises
		// the reduced feature set. It short-circuits when caps.GetID() matches
		// the stored CapState.ID, so reusing base.ID would leave clients showing
		// stale support after the flag is toggled. Mirrors the "+dm" convention.
		if !c.Main.Config.ReadReceipts {
			modified.ID += "+nordr"
		}
		if !c.Main.Config.TypingNotifications {
			modified.ID += "+notyp"
		}
		return &modified
	}
	return base
}

// ============================================================================
// Callbacks from rustpush
// ============================================================================

// OnStatusUpdate is called by StatusKit when a contact's presence changes.
// Reflects the contact's Focus/DND on the DM's chat title — appending a 🌙 to
// their name while silenced and removing it when available (a room-state change
// updated in place, so it never bumps or unarchives the chat), similar to the
// moon Apple shows next to a name. Also sets Matrix ghost presence for clients
// that render it.
//
// IMPORTANT: this function is called from a Rust FFI callback (inside the APNs
// receive loop). It must return quickly — any blocking work, especially any
// call back into Rust (e.g. ResolveHandle), would block the receive loop and
// can deadlock on a single-threaded tokio runtime. All non-trivial work is
// therefore dispatched to a goroutine immediately after the fast dedup check.
// debugDisablePrivacy is a process-global DEVELOPMENT-ONLY switch mirroring
// IMConfig.DebugDisablePrivacy. It lives at package scope (rather than being
// threaded through every call site) so the free log helpers below — which have
// no access to config — can honor it. Set once in IMConnector.Start() before
// any login connects; read-only afterward, so no synchronization is needed.
// When true the log anonymizers below pass values through verbatim.
var debugDisablePrivacy bool

// logSafeHandle returns a stable, opaque UUID-form token derived from a user
// handle (phone number or email) so logs can correlate a user across lines
// without recording the PII itself. Deterministic per handle and not reversible
// (SHA-256, first 16 bytes formatted as a UUID).
func logSafeHandle(handle string) string {
	if handle == "" {
		return ""
	}
	if debugDisablePrivacy {
		return handle
	}
	sum := sha256.Sum256([]byte(handle))
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// logSafeHandles maps logSafeHandle over a slice for logging handle lists
// (e.g. a portal's participants) without recording the raw PII.
func logSafeHandles(handles []string) []string {
	out := make([]string, len(handles))
	for i, h := range handles {
		out[i] = logSafeHandle(h)
	}
	return out
}

func (c *IMClient) OnStatusUpdate(user string, mode *string, available bool) {
	log := c.UserLogin.Log.With().
		Str("component", "statuskit").
		Str("user", logSafeHandle(user)).
		Logger()

	if !c.Main.Config.StatusKitNotifications {
		log.Debug().Msg("StatusKit notifications disabled in config — dropping update")
		return
	}

	// Suppress duplicate notices: only act when the state actually changes.
	// Apple sends available=true for BOTH DND-on and DND-off; the real
	// discriminator is whether mode is set. Track by mode string so that
	// DND→Sleep (two different silenced modes) still fires two notices.
	modeKey := "available"
	if mode != nil && *mode != "" {
		modeKey = *mode
	}
	// kvKeyFor returns the KV store key used to persist StatusKit state for
	// a given user handle across bridge restarts.
	kvKeyFor := func(u string) database.Key {
		return database.Key("statuskit.presence." + u)
	}

	if prev, loaded := c.statusKitPresence.Load(user); loaded {
		if prev.(string) == modeKey {
			log.Debug().Bool("available", available).Str("mode", modeKey).Msg("StatusKit: presence unchanged, skipping notice")
			return
		}
	} else {
		// Not in memory (first call this session for this user): check KV store
		// for the state persisted from the previous bridge run.  If it matches
		// the incoming state, warm the in-memory cache and skip the notice so
		// users don't see a flood of "has notifications turned on/off" messages
		// every time the bridge restarts.
		if c.Main.Bridge.DB.KV.Get(context.Background(), kvKeyFor(user)) == modeKey {
			c.statusKitPresence.Store(user, modeKey)
			log.Debug().Bool("available", available).Str("mode", modeKey).Msg("StatusKit: presence unchanged (restored from DB), skipping notice")
			return
		}
	}

	log.Info().
		Bool("available", available).
		Str("mode_key", modeKey).
		Msg("StatusKit: received presence update — dispatching to goroutine")

	// Capture values for the goroutine (mode is a pointer; copy the string).
	var modeCopy *string
	if mode != nil {
		s := *mode
		modeCopy = &s
	}

	// Optimistic dedup: store the new modeKey BEFORE dispatching so that
	// rapid-fire re-deliveries of the same state (e.g. APNs replay on
	// reconnect) are caught by the check above before the goroutine has
	// had a chance to complete and store the key itself.
	// Also persist to the KV store so the dedup survives a bridge restart.
	c.statusKitPresence.Store(user, modeKey)
	c.Main.Bridge.DB.KV.Set(context.Background(), kvKeyFor(user), modeKey)

	// Dispatch ALL blocking work — ghost lookup, portal resolution, IDS
	// fallback, and Matrix send — to a goroutine so this callback returns
	// to Rust immediately and does not block the APNs receive loop.
	go func() {
		ctx := context.Background()

		// Apple sends available=true for both DND-on and DND-off; the mode
		// field is the real signal. mode non-nil/non-empty = DND/Focus active.
		silenced := modeCopy != nil && *modeCopy != ""
		presence := event.PresenceOnline
		statusMsg := ""
		if silenced {
			presence = event.PresenceUnavailable
			statusMsg = *modeCopy
		}

		// findPortal returns an existing, active portal or nil.
		findPortal := func(id networkid.PortalID) *bridgev2.Portal {
			p, err := c.Main.Bridge.GetExistingPortalByKey(ctx, networkid.PortalKey{
				ID:       id,
				Receiver: c.UserLogin.ID,
			})
			if err != nil || p == nil || p.MXID == "" {
				return nil
			}
			return p
		}

		normalizedUser := normalizeIdentifierForPortalID(user)

		// Resolve the portal AND the ghost handle to use for sending.
		//
		// For mailto: handles the ghost often has no MXID (never joined a
		// Matrix room) so ghost.Intent is nil. We must use the tel: ghost
		// instead — it IS in the portal and has a valid MXID. portal.ID is
		// the tel: handle, so we derive the ghost from the resolved portal.
		//
		// Resolution order for mailto: handles:
		//   (0) statusKitPortalCache — learned from inbound message traffic,
		//       covers any peer who has ever messaged bridge regardless of
		//       whether they appear in iCloud contacts.
		//   (1) address-book phones (fast, no network)
		//   (2) IDS correlation — self-bounding 5s tokio timeout in Rust
		//   (3) mailto: portal itself as absolute last resort
		var portal *bridgev2.Portal

		// (0) Fast path: check the learned sender→portal cache first. This
		// is populated from every inbound message, so any peer who has
		// messaged bridge has a direct mapping here that doesn't depend on
		// iCloud CardDAV contact completeness (the common failure mode for
		// reshare correlation).
		for _, key := range [2]string{user, normalizedUser} {
			if key == "" {
				continue
			}
			if cached, ok := c.statusKitPortalCache.Load(key); ok {
				if p := findPortal(cached.(networkid.PortalID)); p != nil {
					portal = p
					log.Info().Str("user", logSafeHandle(user)).Str("portal_id", string(p.ID)).Msg("StatusKit: resolved via learned sender cache")
					break
				}
			}
		}

		if portal == nil && strings.HasPrefix(normalizedUser, "mailto:") {
			// (1) Address-book.
			contact := c.lookupContact(user)
			if contact != nil {
				for _, altID := range contactPortalIDs(contact) {
					if !strings.HasPrefix(altID, "tel:") {
						continue
					}
					if p := findPortal(networkid.PortalID(altID)); p != nil {
						log.Info().Str("tel_handle", altID).Msg("StatusKit: resolved mailto→tel via address book")
						portal = p
						break
					}
				}
			}

			// (2) IDS correlation (self-bounding, safe to call from goroutine).
			// Cached wrapper: skips repeat round-trips against handles
			// that recently returned no result (negative cooldown), and
			// short-circuits via the persistent alias_portal store on
			// previously-resolved handles.
			if portal == nil {
				if altPortal := c.resolveStatusPortalViaIDSCached(ctx, log, user); altPortal != nil {
					log.Info().Str("user", logSafeHandle(user)).Msg("StatusKit: resolved mailto→tel via IDS correlation")
					portal = altPortal
				}
			}

			// (2.5) Cluster transitive lookup. Fires when contacts and IDS
			// both came up empty — common for hidden-alias mailto:s where
			// Apple's IDS returns LookupFailed (6001). Looks for sibling
			// handles on the same StatusKit channel id; the first one that
			// resolves via the live chain (or persisted alias→portal) hands
			// the unknown handle its portal.
			if portal == nil {
				if p := c.resolveViaCluster(ctx, log, user); p != nil {
					portal = p
				}
			}

			// (3) mailto: portal as last resort.
			if portal == nil {
				if p := findPortal(networkid.PortalID(normalizedUser)); p != nil {
					log.Info().Str("portal_id", normalizedUser).Msg("StatusKit: using mailto: portal as last resort")
					portal = p
				}
			}
		} else if portal == nil {
			portalID := c.resolveContactPortalID(normalizedUser)
			portalID = c.resolveExistingDMPortalID(string(portalID))
			portal = findPortal(portalID)

			if portal == nil {
				contact := c.lookupContact(user)
				if contact != nil {
					for _, altID := range contactPortalIDs(contact) {
						if altID == normalizedUser {
							continue
						}
						altPortalID := c.resolveContactPortalID(altID)
						altPortalID = c.resolveExistingDMPortalID(string(altPortalID))
						if p := findPortal(altPortalID); p != nil {
							log.Info().Str("alt_handle", altID).Msg("StatusKit: resolved DM portal via contact store")
							portal = p
							break
						}
					}
				}
			}

			// Cluster transitive lookup for the tel: branch too — covers
			// peers who publish from a tel: alias the bridge has not seen
			// before but who share a channel id with a known sibling.
			if portal == nil {
				if p := c.resolveViaCluster(ctx, log, user); p != nil {
					portal = p
				}
			}
		}

		if portal == nil || portal.MXID == "" {
			log.Warn().
				Str("user", normalizedUser).
				Msg("StatusKit: no DM portal found for presence notice")
			return
		}

		// Canonical (portal-keyed) dedup. The early dedup at the top of
		// OnStatusUpdate keys by raw alias, so a peer with N registered
		// handles produces N goroutines that all reach this point and
		// resolve to the same portal. Collapse them here using portal ID
		// as the canonical key so only the first alias of each state
		// change actually sends the notice. Persisted to KV under a
		// distinct prefix so the dedup survives bridge restarts.
		portalKey := database.Key("statuskit.presence.portal." + string(portal.ID))
		// We successfully decrypted and resolved a presence event for this portal —
		// refresh the moon-TTL freshness clock BEFORE the dedup early-returns, so a
		// same-state re-confirmation still counts as "still hearing valid presence"
		// and keeps a legitimate moon alive. A moon only goes stale when these stop.
		c.touchPresenceAt(ctx, portal.ID)
		if prev, loaded := c.statusKitPresenceByPortal.Load(portal.ID); loaded {
			if prev.(string) == modeKey {
				log.Debug().
					Str("portal_id", string(portal.ID)).
					Str("alias", user).
					Str("mode", modeKey).
					Msg("StatusKit: presence unchanged for canonical portal, skipping duplicate alias notice")
				return
			}
		} else if c.Main.Bridge.DB.KV.Get(ctx, portalKey) == modeKey {
			c.statusKitPresenceByPortal.Store(portal.ID, modeKey)
			log.Debug().
				Str("portal_id", string(portal.ID)).
				Str("alias", user).
				Str("mode", modeKey).
				Msg("StatusKit: presence unchanged for canonical portal (restored from DB), skipping duplicate alias notice")
			return
		}
		c.statusKitPresenceByPortal.Store(portal.ID, modeKey)
		c.Main.Bridge.DB.KV.Set(ctx, portalKey, modeKey)

		// Use the ghost keyed to the portal's handle — for a tel: portal this
		// is the tel: ghost which has an MXID and is a member of the room.
		// The mailto: ghost typically has no MXID (never joined a room) so
		// ghost.Intent would be nil. Prefer portal ghost; fall back to the
		// mailto: ghost if the portal ghost is unavailable.
		ghostHandle := string(portal.ID)
		ghost, err := c.Main.Bridge.GetGhostByID(ctx, networkid.UserID(ghostHandle))
		if err != nil || ghost == nil || ghost.Intent == nil {
			// Fallback: try the original mailto: ghost.
			ghost, err = c.Main.Bridge.GetGhostByID(ctx, networkid.UserID(user))
			if err != nil || ghost == nil || ghost.Intent == nil {
				log.Warn().Err(err).Str("portal_handle", ghostHandle).Str("mailto_handle", user).
					Msg("StatusKit: no usable ghost found — skipping notice")
				return
			}
			log.Debug().Str("ghost", user).Msg("StatusKit: using mailto: ghost as fallback")
		} else {
			log.Debug().Str("ghost", ghostHandle).Msg("StatusKit: using portal ghost (tel:)")
		}

		// Set Matrix presence using whichever ghost we resolved.
		if asIntent, ok := ghost.Intent.(*matrixfmt.ASIntent); ok {
			if err := asIntent.Matrix.SetPresence(ctx, mautrix.ReqPresence{
				Presence:  presence,
				StatusMsg: statusMsg,
			}); err != nil {
				log.Warn().Err(err).Msg("StatusKit: failed to set Matrix presence for ghost")
			}
		}

		// Reflect the focus state in the DM TITLE (room name) instead of
		// posting a timeline notice: the contact's name normally, the name
		// plus a trailing 🌙 while their Focus/DND is on. The title is
		// persistent in the header and the chat-list row, never buried, and is
		// updated in place with ExcludeChangesFromTimeline — so no wall, no
		// tombstones, no inbox churn. DMs only: a group has a single shared
		// title, so per-member focus can't ride on it (groups no longer
		// surface focus at all).
		//
		// Routed through computeDMTitle so this live path and the periodic
		// refreshDMPortalNamesFromContacts loop ALWAYS agree — the refresh
		// re-applies the identical title every contact-sync tick (covering
		// restarts and any toggle missed while the bridge was down), and a
		// live change updates it immediately. The portal-keyed presence state
		// was stored just above, so computeDMTitle sees the new state.
		//
		// Logs carry only the opaque room MXID + a boolean — never the
		// contact name or handle.
		// Only touch the title when there's something to own: the peer is
		// silenced (add 🌙), or we already own the name (NameIsCustom — to
		// maintain it or clear the moon). A pristine, never-silenced DM is left
		// on implicit naming so we never mass-seize NameIsCustom (which would
		// also freeze ghost avatar updates) across DMs. portal.Name may be
		// empty for implicitly-named DMs, so a title!=Name gate alone isn't safe.
		if portal.RoomType == database.RoomTypeDM && (silenced || portal.NameIsCustom) {
			nameField := c.dmFocusName(ctx, portal)
			if nameField == nil {
				// Contact name not resolved yet — leave the title untouched
				// rather than blanking it. A later refresh tick will set it.
				return
			}
			chatInfo := &bridgev2.ChatInfo{
				Name:                       nameField,
				ExcludeChangesFromTimeline: true,
			}
			// Re-supply the room avatar whenever we stamp an explicit title (the
			// moon). NameIsCustom=true otherwise freezes the ghost avatar out of
			// the DM, so the photo vanishes for the duration of the focus. The
			// release path (DefaultChatName) hands naming back to the framework,
			// which restores the avatar itself — leave it nil there.
			if nameField != bridgev2.DefaultChatName {
				chatInfo.Avatar = c.dmFocusContactAvatar(ctx, portal)
			}
			c.UserLogin.QueueRemoteEvent(&simplevent.ChatInfoChange{
				EventMeta: simplevent.EventMeta{
					Type: bridgev2.RemoteEventChatInfoChange,
					PortalKey: networkid.PortalKey{
						ID:       portal.ID,
						Receiver: c.UserLogin.ID,
					},
					LogContext: func(lc zerolog.Context) zerolog.Context {
						return lc.Str("portal_mxid", string(portal.MXID)).
							Str("source", "focus_status").
							Bool("silenced", silenced)
					},
				},
				ChatInfoChange: &bridgev2.ChatInfoChange{
					ChatInfo: chatInfo,
				},
			})
			log.Info().Str("portal_mxid", string(portal.MXID)).Bool("silenced", silenced).
				Msg("StatusKit: updated DM title for focus change")
		}
	}()
}

// presenceMoonTTL is how long a silenced (Focus/DND) state survives without
// being re-confirmed by a freshly-decrypted presence event before isPortalSilenced
// treats it as STALE and clears the 🌙. This self-heals "stuck moons": when a peer
// rotates their StatusKit key and we can no longer decrypt their "now available"
// update, the persisted focus mode would otherwise pin the moon forever. 16h is
// comfortably longer than any normal Focus (Sleep ~8–10h, Work ~9h) so a genuine
// DND is never prematurely cleared — only a state we've stopped hearing about.
const presenceMoonTTL = 16 * time.Hour

// presenceAtRefreshThrottle bounds how often touchPresenceAt writes the freshness
// timestamp to KV, so a single presence change fanned across a peer's N aliases
// doesn't write N times.
const presenceAtRefreshThrottle = 5 * time.Minute

// presenceAtKey is the KV key holding the unix-millis timestamp of the last
// successfully-processed presence event for a portal — the freshness clock the
// moon-TTL reads.
func presenceAtKey(portalID networkid.PortalID) database.Key {
	return database.Key("statuskit.presence.portal." + string(portalID) + ".at")
}

// touchPresenceAt refreshes the moon-TTL freshness clock for this portal. It
// fires when a presence event reaches portal-level processing — i.e. a state
// CHANGE (the handle-level dedup at the top of OnStatusUpdate, ~line 1829, lets
// changes through) — so the clock tracks "time of last state change we processed"
// for this peer. Same-state re-deliveries caught by that earlier handle dedup do
// NOT reach here, so the TTL measures time-since-last-change, not time-since-last-
// heard. That's the right signal for the bug we're fixing: a stuck moon is a
// missed "now available" transition, which leaves the clock frozen at the DND-on
// change and clears at TTL. Known tradeoff: a genuinely continuous Focus lasting
// >presenceMoonTTL also clears (rare; self-corrects on the peer's next change).
// Throttled to avoid KV churn on alias fan-out.
func (c *IMClient) touchPresenceAt(ctx context.Context, portalID networkid.PortalID) {
	key := presenceAtKey(portalID)
	if raw := c.Main.Bridge.DB.KV.Get(ctx, key); raw != "" {
		if ms, err := strconv.ParseInt(raw, 10, 64); err == nil && time.Since(time.UnixMilli(ms)) < presenceAtRefreshThrottle {
			return
		}
	}
	c.Main.Bridge.DB.KV.Set(ctx, key, strconv.FormatInt(time.Now().UnixMilli(), 10))
}

// isPortalSilenced reports whether the peer of the given DM portal currently
// has iMessage Focus/DND on. Map-first (statusKitPresenceByPortal), KV-fallback:
// the in-memory map is cold after a restart, but the per-portal presence state
// is persisted, so the fallback lets the periodic title refresh PRESERVE the 🌙
// across restarts instead of stripping it until the next live toggle. A stored
// value of "available" (or absent) = not silenced; any focus mode = silenced.
//
// Silenced states are additionally gated on presenceMoonTTL: a focus mode whose
// last-state-change is older than the TTL is treated as stale (not silenced) so a
// stuck moon self-clears on the next title refresh (which runs every ~15min via
// setContactsReady, regardless of contact changes). A pre-TTL row with no
// freshness timestamp is stamped now, giving already-stuck moons a TTL window to
// clear instead of sticking forever.
//
// Note: depends on StatusKitNotifications being enabled — when it's off,
// OnStatusUpdate early-returns before recording presence, so this is always
// false and no moon appears (intended).
func (c *IMClient) isPortalSilenced(ctx context.Context, portalID networkid.PortalID) bool {
	silenced := false
	if v, ok := c.statusKitPresenceByPortal.Load(portalID); ok {
		s, _ := v.(string)
		silenced = s != "" && s != "available"
	} else {
		s := c.Main.Bridge.DB.KV.Get(ctx, database.Key("statuskit.presence.portal."+string(portalID)))
		silenced = s != "" && s != "available"
	}
	if !silenced {
		return false
	}
	raw := c.Main.Bridge.DB.KV.Get(ctx, presenceAtKey(portalID))
	if raw == "" {
		// Pre-TTL row (set before this code shipped, or never timestamped). Stamp
		// it now so a genuinely-stuck moon gets a TTL window and self-clears later
		// instead of persisting forever.
		c.Main.Bridge.DB.KV.Set(ctx, presenceAtKey(portalID), strconv.FormatInt(time.Now().UnixMilli(), 10))
		return true
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return true
	}
	return time.Since(time.UnixMilli(ms)) < presenceMoonTTL
}

// dmBaseName returns the name a DM's title shows WITHOUT the moon, matching the
// authority that NORMALLY names that chat — so adding/removing 🌙 only ever
// toggles the suffix and never swaps the underlying name (which is what caused
// the "David Brustein" ↔ "David" flip):
//   - self-chat (Note to Self): resolveContactDisplayname — exactly what the
//     connector itself sets for the self-chat (GetChatInfo). Using ghost.Name
//     here would disagree with the connector and flip the title on every toggle.
//   - regular DM: ghost.Name — what the framework derives (and what
//     DefaultChatName restores), and it carries shared-profile names that
//     resolveContactDisplayname would miss.
func (c *IMClient) dmBaseName(ctx context.Context, portal *bridgev2.Portal) string {
	if c.isMyHandle(string(portal.ID)) {
		return c.resolveContactDisplayname(string(portal.ID))
	}
	for _, uid := range []networkid.UserID{portal.OtherUserID, networkid.UserID(portal.ID)} {
		if uid == "" {
			continue
		}
		if ghost, err := c.Main.Bridge.GetGhostByID(ctx, uid); err == nil && ghost != nil && ghost.Name != "" {
			return ghost.Name
		}
	}
	return c.resolveContactDisplayname(string(portal.ID))
}

// computeDMTitle returns a DM's room title: dmBaseName plus a trailing " 🌙"
// while the peer's iMessage Focus/DND is on. SINGLE source of truth for the
// title — both the live focus path (OnStatusUpdate) and the periodic refresh
// (refreshDMPortalNamesFromContacts) call it, so they can never disagree.
func (c *IMClient) computeDMTitle(ctx context.Context, portal *bridgev2.Portal) string {
	base := c.dmBaseName(ctx, portal)
	if base != "" && c.isPortalSilenced(ctx, portal.ID) {
		base += " 🌙"
	}
	return base
}

// dmFocusName returns the ChatInfo.Name to set for a DM given the peer's focus
// state:
//   - silenced  → the moon title ("<name> 🌙"), which we own (NameIsCustom=true).
//   - available, regular DM → see the release-path comment below; depends on
//     private_chat_portal_meta.
//   - available, self-chat → the bare connector name. The connector permanently
//     owns the self-chat name (NameIsCustom is always set by GetChatInfo), so
//     releasing would just fight GetChatInfo and flip; restore the same bare
//     name dmBaseName uses, keeping it stable.
//
// Returns nil when the contact name hasn't resolved yet, meaning "leave the name
// alone" — never stamp an empty/lone-🌙 title (that blanks the DM).
func (c *IMClient) dmFocusName(ctx context.Context, portal *bridgev2.Portal) *string {
	if c.isPortalSilenced(ctx, portal.ID) {
		title := c.computeDMTitle(ctx, portal)
		// computeDMTitle returns "" when the contact name isn't resolved yet
		// (e.g. contacts not loaded). Don't stamp that — it would blank the DM
		// or, with the moon appended, leave a nameless "🌙". Leave the name as-is.
		if title == "" {
			return nil
		}
		return &title
	}
	if c.isMyHandle(string(portal.ID)) {
		base := c.dmBaseName(ctx, portal)
		return &base
	}
	// Releasing the moon (peer available again): the title must return to the
	// plain contact name. HOW depends on private_chat_portal_meta:
	//   - true:  hand the title back with DefaultChatName, which RELEASES ownership
	//     (NameIsCustom=false) so the framework restores the bare ghost name AND
	//     re-enables ghost name+avatar sync. (Re-stamping would keep NameIsCustom
	//     true forever and freeze the ghost avatar — which matters only here.)
	//   - false: the framework will NOT restore the name — UpdateInfoFromGhost
	//     early-returns at portal.go:4960 when PrivateChatPortalMeta is off — so
	//     DefaultChatName leaves an explicit empty m.room.name and the DM renders
	//     blank (the "lost contact name" bug). Re-stamp the bare contact name
	//     ourselves. NameIsCustom stays true, but under PCPM=false the framework
	//     never syncs DM name/avatar from the ghost anyway (ghost.go:320), so
	//     nothing is frozen that wasn't already manual.
	if c.Main.Bridge.Config.PrivateChatPortalMeta {
		return bridgev2.DefaultChatName
	}
	base := c.dmBaseName(ctx, portal)
	if base == "" {
		return nil
	}
	return &base
}

// dmFocusContactAvatar returns the DM peer's contact photo as a ChatInfo avatar,
// or nil when no contact/photo is available. Stamping an explicit DM title (the
// moon) sets NameIsCustom=true, which makes the framework stop syncing the
// ghost's avatar to the room (UpdateInfoFromGhost early-returns when
// NameIsCustom is set) — so a mooned DM silently loses its contact photo. The
// self-chat path at GetChatInfo already re-supplies the avatar for the same
// reason; the moon stamp sites must do the same. Mirrors GetUserInfo's
// contact-avatar block (same AvatarID format → idempotent with the ghost's).
func (c *IMClient) dmFocusContactAvatar(ctx context.Context, portal *bridgev2.Portal) *bridgev2.Avatar {
	if c.contacts == nil {
		return nil
	}
	portalID := string(portal.ID)
	contact, _ := c.contacts.GetContactInfo(stripIdentifierPrefix(portalID))
	if contact == nil || len(contact.Avatar) == 0 {
		return nil
	}
	avatarHash := sha256.Sum256(contact.Avatar)
	avatarData := contact.Avatar
	return &bridgev2.Avatar{
		ID: networkid.AvatarID(fmt.Sprintf("contact:%s:%s", portalID, hex.EncodeToString(avatarHash[:8]))),
		Get: func(ctx context.Context) ([]byte, error) {
			return avatarData, nil
		},
	}
}

// ensureBotPushRuleSilenced installs push rules via the double puppet so
// that homeservers that respect Matrix push rules suppress notifications from
// the bridge bot. Called eagerly at Connect() time as a one-shot install.
//
// Note: on Beeper/Hungry this is a no-op in practice — Hungry's proprietary
// push gateway ignores Matrix push rules for DM rooms. Push suppression for
// Hungry is instead achieved via ReqBeeperBatchSend.SendNotification:false
// in the OnStatusUpdate send path.
//
// Two rules are installed for belt-and-suspenders coverage:
//   - An override rule (evaluated first, before any DM-room override rules)
//     with an event_match condition on the sender field.
//   - A sender rule (evaluated last) as a secondary catch-all.
//
// Both rules are durable on the homeserver and persist across restarts.
// statusKitBotRulePushed guards against redundant API calls within a session.
// A no-op if the double puppet is not available or the assertion fails.
func (c *IMClient) ensureBotPushRuleSilenced(ctx context.Context) {
	if c.statusKitBotRulePushed.Load() {
		return
	}
	dp := c.UserLogin.User.DoublePuppet(ctx)
	if dp == nil {
		return
	}
	asIntent, ok := dp.(*matrixfmt.ASIntent)
	if !ok {
		return
	}
	log := c.UserLogin.Log.With().Str("component", "statuskit").Logger()
	botMXID := c.Main.Bridge.Bot.GetMXID().String()

	// Override rule (highest priority — evaluated before DM override rules).
	overrideRuleID := botMXID + ".dont_notify"
	err := asIntent.Matrix.PutPushRule(ctx, "global", pushrules.OverrideRule, overrideRuleID,
		&mautrix.ReqPutPushRule{
			Conditions: []pushrules.PushCondition{
				{
					Kind:    pushrules.KindEventMatch,
					Key:     "sender",
					Pattern: botMXID,
				},
			},
			Actions: []pushrules.PushActionType{pushrules.ActionDontNotify},
		})
	if err != nil {
		log.Debug().Err(err).Str("bot_mxid", botMXID).Msg("Failed to install bot override push rule")
		return
	}

	// Sender rule (secondary catch-all for homeservers without override support).
	err = asIntent.Matrix.PutPushRule(ctx, "global", pushrules.SenderRule, botMXID,
		&mautrix.ReqPutPushRule{
			Actions: []pushrules.PushActionType{pushrules.ActionDontNotify},
		})
	if err != nil {
		log.Debug().Err(err).Str("bot_mxid", botMXID).Msg("Failed to install bot sender push rule")
		// Override rule succeeded; treat as partial success.
	}

	c.statusKitBotRulePushed.Store(true)
	log.Debug().Str("bot_mxid", botMXID).Msg("Bot push rules installed — bridge bot messages will not notify")
}

// resolveStatusPortalViaIDS is the last-resort portal resolver for StatusKit
// presence. When a contact's iCloud Apple ID (e.g. mailto:aap724@icloud.com)
// doesn't appear in the contact store but still shares a person with a handle
// we do have a portal for (e.g. tel:+12012337620), the only link between them
// lives in IDS — specifically the sender_correlation_identifier returned by a
// Madrid lookup. We batch-lookup the incoming handle plus every known ghost in
// a single IDS query, then match on correlation ID to find the aliased portal.
// Results are memoized in statusKitPortalCache so we only pay the IDS round
// trip once per unresolved handle per session.
func (c *IMClient) resolveStatusPortalViaIDS(ctx context.Context, log zerolog.Logger, user string) *bridgev2.Portal {
	if c.client == nil {
		return nil
	}
	if cached, ok := c.statusKitPortalCache.Load(user); ok {
		portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, networkid.PortalKey{
			ID:       cached.(networkid.PortalID),
			Receiver: c.UserLogin.ID,
		})
		if err == nil && portal != nil && portal.MXID != "" {
			return portal
		}
	}

	rows, err := c.Main.Bridge.DB.RawDB.QueryContext(ctx, "SELECT id FROM ghost WHERE bridge_id=$1", c.Main.Bridge.ID)
	if err != nil {
		log.Warn().Err(err).Msg("StatusKit IDS fallback: failed to query ghosts")
		return nil
	}
	var knownHandles []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		if id == user {
			continue
		}
		knownHandles = append(knownHandles, id)
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("StatusKit IDS fallback: ghost row iteration error")
	}
	rows.Close()
	if len(knownHandles) == 0 {
		return nil
	}

	// Fast path: check the in-memory IDS cache populated from message
	// processing. No network call, no blocking. If the correlation ID is
	// already cached (common when the contact has sent messages before),
	// this resolves instantly and we skip the slow validate_targets call.
	if aliases := c.client.ResolveHandleCached(user, knownHandles); len(aliases) > 0 {
		log.Info().Str("user", logSafeHandle(user)).Int("aliases", len(aliases)).Msg("StatusKit IDS fallback: resolved from cache (no network)")
		if portal := c.findPortalForAliases(ctx, log, user, aliases); portal != nil {
			return portal
		}
	}

	// Slow path: validate_targets queries Apple IDS. Can block or hang;
	// caller is responsible for applying a timeout.
	aliases, err := c.client.ResolveHandle(user, knownHandles)
	if err != nil {
		log.Warn().Err(err).Str("user", logSafeHandle(user)).Msg("StatusKit IDS fallback: ResolveHandle failed")
		return nil
	}
	return c.findPortalForAliases(ctx, log, user, aliases)
}

// findPortalForAliases iterates aliases returned by IDS correlation and
// returns the first portal found. Prefers tel: aliases over mailto: so the
// presence notice lands in the phone portal rather than the email portal.
func (c *IMClient) findPortalForAliases(ctx context.Context, log zerolog.Logger, user string, aliases []string) *bridgev2.Portal {
	// Two-pass: tel: first, then anything else.
	for _, preferTel := range []bool{true, false} {
		for _, alias := range aliases {
			if alias == user {
				continue
			}
			if preferTel != strings.HasPrefix(alias, "tel:") {
				continue
			}
			aliasPortalID := c.resolveContactPortalID(alias)
			aliasPortalID = c.resolveExistingDMPortalID(string(aliasPortalID))
			altPortal, altErr := c.Main.Bridge.GetExistingPortalByKey(ctx, networkid.PortalKey{
				ID:       aliasPortalID,
				Receiver: c.UserLogin.ID,
			})
			if altErr == nil && altPortal != nil && altPortal.MXID != "" {
				log.Info().
					Str("original", user).
					Str("alias", alias).
					Str("resolved_portal_id", string(aliasPortalID)).
					Msg("StatusKit: resolved DM portal via IDS correlation")
				c.rememberAliasPortal(ctx, user, aliasPortalID)
				return altPortal
			}
		}
	}
	return nil
}

// OnKeysReceived is called by StatusKit when a key-sharing message arrives.
// New encryption keys mean we can now subscribe to APNs presence channels
// for handles that previously had no keys. Re-subscribe to pick them up,
// and reciprocate by sending our key to any handle that keyed us but hasn't
// received our invite yet (iOS does this automatically; the bridge must do
// it explicitly or the exchange stays one-sided).
func (c *IMClient) OnKeysReceived() {
	log := c.UserLogin.Log.With().
		Str("component", "statuskit").
		Logger()
	log.Info().Msg("StatusKit: key-sharing message received — scheduling presence re-subscribe")
	c.schedulePresenceSubscribe(log)
}

// OnStatusDecryptFailed is called by StatusKit when a presence update on the
// presence.mode.status topic could not be decoded — almost always a peer who
// rotated their StatusKit channel key, leaving the copy we pulled from CloudKit
// stale. We force a rate-limited CloudKit peer-key re-pull to fetch the fresh key
// right away (instead of waiting for the steady-state pull floor), which then
// re-subscribes and lets the peer's real presence flow again — clearing a moon
// that would otherwise be stuck. sender is best-effort (the peer handle when the
// envelope exposes it) and used only for logging.
func (c *IMClient) OnStatusDecryptFailed(sender *string) {
	if !c.Main.Config.StatusKitNotifications {
		return
	}
	log := c.UserLogin.Log.With().Str("component", "statuskit").Logger()
	s := ""
	if sender != nil {
		s = *sender
	}
	log.Info().Str("sender", s).Msg("StatusKit: presence decrypt failed (likely stale peer key) — forcing rate-limited CloudKit re-pull")
	c.onPresenceDecryptFailed(log)
}

// OnReshareSender is called once per reshare with the peer handle that sent
// it. Peer iOS fans a reshare across every alias of our account (tel: + each
// mailto:) with the same channel id but a different `from` handle on each
// copy. Upstream state keys by channel, so every reshare overwrites the
// previous `from` — only the last sender survives in state.keys. Without this
// hook the bridge learns just one alias per peer and presence updates on the
// others have no portal mapping. Eagerly resolving the sender's portal here
// and stamping it into statusKitPortalCache preserves the mapping across
// overwrites so OnStatusUpdate's cache lookup always hits regardless of which
// alias Apple routes the next presence message to.
func (c *IMClient) OnReshareSender(sender string, channelId string) {
	if sender == "" {
		return
	}
	// Stamp reshare_seen for both raw and normalized sender forms before any
	// cache short-circuit. The soft-latch in inviteContactsToStatusSharingOpts
	// reads these to distinguish "invite dispatched and peer reciprocated"
	// (permanent latch) from "dispatched but no reshare ever observed"
	// (TTL'd latch — re-invite after statusKitInvitedOkTTL).
	normalized := normalizeIdentifierForPortalID(sender)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	c.Main.Bridge.DB.KV.Set(ctx, database.Key(statusKitReshareSeenKeyPrefix+sender), now)
	if normalized != sender {
		c.Main.Bridge.DB.KV.Set(ctx, database.Key(statusKitReshareSeenKeyPrefix+normalized), now)
	}
	clusterLog := c.UserLogin.Log.With().
		Str("component", "statuskit").
		Str("sender", sender).
		Str("channel_id", channelId).
		Logger()
	// Persist the cluster observation regardless of whether the sender is
	// already cached as resolved — alias correlation feeds off the full
	// observation history, not just first sightings.
	c.recordReshareObservation(ctx, clusterLog, channelId, sender)
	if normalized != sender {
		c.recordReshareObservation(ctx, clusterLog, channelId, normalized)
	}
	if _, ok := c.statusKitPortalCache.Load(sender); ok {
		return
	}
	if normalized != sender {
		if _, ok := c.statusKitPortalCache.Load(normalized); ok {
			return
		}
	}
	log := c.UserLogin.Log.With().
		Str("component", "statuskit").
		Str("sender", sender).
		Logger()
	go c.eagerResolveReshareSender(sender, normalized, log)
}

// eagerResolveReshareSender runs the same resolution chain OnStatusUpdate uses
// (learned cache → address-book mailto→tel → IDS correlation → direct portal)
// and populates statusKitPortalCache on success. Idempotent; cheap no-op on
// cache hit. Runs in a goroutine to avoid blocking the Rust FFI caller.
func (c *IMClient) eagerResolveReshareSender(sender, normalizedUser string, log zerolog.Logger) {
	ctx := context.Background()

	findPortal := func(id networkid.PortalID) *bridgev2.Portal {
		p, err := c.Main.Bridge.GetExistingPortalByKey(ctx, networkid.PortalKey{
			ID:       id,
			Receiver: c.UserLogin.ID,
		})
		if err != nil || p == nil || p.MXID == "" {
			return nil
		}
		return p
	}

	if strings.HasPrefix(normalizedUser, "mailto:") {
		if contact := c.lookupContact(sender); contact != nil {
			for _, altID := range contactPortalIDs(contact) {
				if !strings.HasPrefix(altID, "tel:") {
					continue
				}
				if p := findPortal(networkid.PortalID(altID)); p != nil {
					c.rememberAliasPortal(ctx, sender, p.ID)
					log.Info().Str("resolved_portal_id", string(p.ID)).Msg("StatusKit: eager-resolved reshare sender via address book")
					return
				}
			}
		}
		if altPortal := c.resolveStatusPortalViaIDSCached(ctx, log, sender); altPortal != nil {
			c.rememberAliasPortal(ctx, sender, altPortal.ID)
			log.Info().Str("resolved_portal_id", string(altPortal.ID)).Msg("StatusKit: eager-resolved reshare sender via IDS correlation")
			return
		}
		if p := findPortal(networkid.PortalID(normalizedUser)); p != nil {
			c.rememberAliasPortal(ctx, sender, p.ID)
			log.Info().Str("resolved_portal_id", string(p.ID)).Msg("StatusKit: eager-resolved reshare sender via direct mailto: portal")
			return
		}
	} else {
		portalID := c.resolveContactPortalID(normalizedUser)
		portalID = c.resolveExistingDMPortalID(string(portalID))
		if p := findPortal(portalID); p != nil {
			c.rememberAliasPortal(ctx, sender, p.ID)
			log.Info().Str("resolved_portal_id", string(p.ID)).Msg("StatusKit: eager-resolved reshare sender (tel: direct)")
			return
		}
	}
	// No live chain hit. The cluster transitive resolver may still find a
	// portal via a sibling alias once another sender on the same channel
	// has been resolved.
	if portal := c.resolveViaCluster(ctx, log, sender); portal != nil {
		log.Info().Str("resolved_portal_id", string(portal.ID)).Msg("StatusKit: eager-resolved reshare sender via cluster transitive lookup")
		return
	}
	log.Debug().Msg("StatusKit: eager-resolve found no portal for reshare sender — will retry when presence arrives")
}

// OnMessage is called by rustpush when a message is received via APNs.
func (c *IMClient) OnMessage(msg rustpushgo.WrappedMessage) {
	log := c.UserLogin.Log.With().
		Str("component", "imessage").
		Str("msg_uuid", msg.Uuid).
		Logger()
	// Send delivery receipt if requested
	if msg.SendDelivered && msg.Sender != nil && !msg.IsDelivered && !msg.IsReadReceipt {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error().Interface("panic", r).Str("stack", string(debug.Stack())).Msg("Panic in SendDeliveryReceipt")
				}
			}()
			conv := c.makeConversation(msg.Participants, msg.GroupName)
			if c.client == nil {
				return
			}
			if err := c.client.SendDeliveryReceipt(conv, c.handle); err != nil {
				log.Warn().Err(err).Msg("Failed to send delivery receipt")
			}
		}()
	}

	if msg.IsDelivered {
		// Delivery receipts for our own outgoing sends can arrive while the
		// rustpush send call is still unwinding. Handle them asynchronously so
		// bridgev2 can commit the outgoing message row before the receipt lookup
		// retry loop runs.
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error().Interface("panic", r).Str("stack", string(debug.Stack())).Msg("Panic in handleDeliveryReceipt")
				}
			}()
			c.handleDeliveryReceipt(log, msg)
		}()
		return
	}

	if msg.IsReadReceipt {
		c.handleReadReceipt(log, msg)
		return
	}
	if msg.IsTyping {
		c.handleTyping(log, msg)
		return
	}
	if msg.IsError {
		log.Warn().
			Str("for_uuid", ptrStringOr(msg.ErrorForUuid, "")).
			Uint64("status", ptrUint64Or(msg.ErrorStatus, 0)).
			Str("status_str", ptrStringOr(msg.ErrorStatusStr, "")).
			Msg("Received iMessage error")
		return
	}
	if msg.IsPeerCacheInvalidate {
		log.Debug().Msg("Peer cache invalidated")
		return
	}
	if msg.IsMoveToRecycleBin || msg.IsPermanentDelete {
		c.handleChatDelete(log, msg)
		return
	}
	if msg.IsRecoverChat {
		c.handleChatRecover(log, msg)
		return
	}
	// Unsends bypass the buffer — they need to apply immediately and
	// don't contribute to chat ordering.
	if msg.IsUnsend {
		c.handleUnsend(log, msg)
		return
	}
	// Rename, participant changes, and group photo changes bypass the buffer
	// — they're control events, not content messages.
	if msg.IsRename {
		c.handleRename(log, msg)
		return
	}
	if msg.IsParticipantChange {
		c.handleParticipantChange(log, msg)
		return
	}
	if msg.IsIconChange {
		go c.handleIconChange(log, msg)
		return
	}
	if msg.IsShareProfile && msg.Sender != nil && *msg.Sender != "" {
		go c.handleSharedProfile(log, msg)
		// Only swallow the message when it's a standalone profile-sharing
		// control event (Message::ShareProfile / Message::UpdateProfile).
		// iOS also piggybacks profile keys on regular text messages and
		// reactions via the embedded_profile field — those still need to
		// flow through the buffer so the user actually sees the message.
		isStandalone := msg.Text == nil && !msg.IsTapback && !msg.IsEdit && !msg.IsUnsend
		if isStandalone {
			return
		}
	}
	// Profile-sharing control messages without an embedded CloudKit record:
	// log only so we can see whether peers are flipping share_contacts /
	// updating their dismissed list. No state to apply on the bridge side.
	if msg.IsUpdateProfile && !msg.IsShareProfile {
		sender := ""
		if msg.Sender != nil {
			sender = *msg.Sender
		}
		shareContacts := false
		if msg.UpdateProfileShareContacts != nil {
			shareContacts = *msg.UpdateProfileShareContacts
		}
		log.Info().
			Str("sender", sender).
			Bool("share_contacts", shareContacts).
			Msg("Received UpdateProfile without embedded CloudKit record")
		return
	}
	if msg.IsUpdateProfileSharing {
		sender := ""
		if msg.Sender != nil {
			sender = *msg.Sender
		}
		log.Info().
			Str("sender", sender).
			Int("dismissed", len(msg.UpdateProfileSharingDismissed)).
			Int("all", len(msg.UpdateProfileSharingAll)).
			Msg("Received UpdateProfileSharing")
		return
	}

	// "Notify Anyway" — sender deliberately broke through our Focus/DND.
	// Post a silent bot notice in the relevant room so the user can see it.
	if msg.IsNotifyAnyways {
		if c.cloudStore != nil {
			if known, _ := c.cloudStore.hasMessageUUID(context.Background(), msg.Uuid); known {
				return
			}
			if err := c.cloudStore.persistMessageUUID(context.Background(), msg.Uuid, "", int64(msg.TimestampMs), false); err != nil {
				log.Warn().Err(err).Str("uuid", msg.Uuid).Msg("Failed to persist NotifyAnyway UUID; duplicates possible on restart")
			}
		}
		go c.handleNotifyAnyways(log, msg)
		return
	}
	if msg.IsSetTranscriptBackground {
		return
	}

	// Buffer regular messages, tapbacks, and edits for timestamp-based
	// reordering. APNs delivers messages grouped by sender rather than
	// interleaved chronologically; the buffer sorts them before dispatch.
	if c.msgBuffer != nil {
		c.msgBuffer.add(msg)
	} else {
		c.dispatchBuffered(msg)
	}
}

// dispatchBuffered routes a message that has been through the reorder buffer
// to its appropriate handler.
func (c *IMClient) dispatchBuffered(msg rustpushgo.WrappedMessage) {
	log := c.UserLogin.Log.With().
		Str("component", "imessage").
		Str("msg_uuid", msg.Uuid).
		Logger()

	if msg.IsTapback {
		c.handleTapback(log, msg)
		return
	}
	if msg.IsEdit {
		c.handleEdit(log, msg)
		return
	}

	c.handleMessage(log, msg)
}

// flushPendingPortalMsgs replays messages that were held during the CloudKit
// sync window because their portals didn't exist yet. Called by setCloudSyncDone
// after the sync gate opens, so handleMessage will see createPortal=true.
func (c *IMClient) flushPendingPortalMsgs() {
	c.pendingPortalMsgsMu.Lock()
	held := c.pendingPortalMsgs
	c.pendingPortalMsgs = nil
	c.pendingPortalMsgsMu.Unlock()

	if len(held) == 0 {
		return
	}

	log := c.UserLogin.Log.With().Str("component", "imessage").Logger()
	log.Info().Int("count", len(held)).Msg("Replaying held messages after CloudKit sync completion")

	// Sort held messages by timestamp so they replay in chronological order.
	sort.Slice(held, func(i, j int) bool {
		return held[i].TimestampMs < held[j].TimestampMs
	})

	for _, msg := range held {
		msgLog := log.With().Str("msg_uuid", msg.Uuid).Logger()
		c.handleMessage(msgLog, msg)
	}
}

// reviveDeletedPortalShell clears chat-level deleted state without restoring
// soft-deleted transcript rows. The chat becomes live again so a new portal
// can be created, while old message UUIDs remain soft-deleted for stale-echo
// suppression. OpenBubbles unconditionally revives on any incoming message;
// we gate on tail-timestamp to avoid APNs replay false-positives.
func (c *IMClient) reviveDeletedPortalShell(ctx context.Context, portalID string) error {
	if c.cloudStore != nil {
		if err := c.cloudStore.undeleteCloudChatByPortalID(ctx, portalID); err != nil {
			return err
		}
	}
	c.recentlyDeletedPortalsMu.Lock()
	delete(c.recentlyDeletedPortals, portalID)
	c.recentlyDeletedPortalsMu.Unlock()
	return nil
}

// UpdateUsers is called when IDS keys are refreshed.
// NOTE: This callback runs on the Tokio async runtime thread.  We must NOT
// make blocking FFI calls back into Rust (e.g. connection.State()) on this
// thread or the runtime will panic with "Cannot block the current thread
// from within a runtime".  Spawn a goroutine so the callback returns
// immediately and the blocking work happens on a regular OS thread.
func (c *IMClient) UpdateUsers(users *rustpushgo.WrappedIdsUsers) {
	c.users = users

	go func() {
		log := c.UserLogin.Log.With().Str("component", "imessage").Logger()
		// Persist all state (APS tokens, IDS keys, identity, device ID) — not just
		// IDSUsers — so a crash between periodic saves doesn't lose APS state.
		c.persistState(log)
		log.Debug().Msg("IDS users updated, full state persisted")
	}()
}

// ============================================================================
// Incoming message handlers
// ============================================================================

func (c *IMClient) handleMessage(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	if c.wasUnsent(msg.Uuid) {
		log.Debug().Str("uuid", msg.Uuid).Msg("Suppressing re-delivery of unsent message")
		return
	}
	if c.wasSmsReactionEcho(msg.Uuid) {
		log.Debug().Str("uuid", msg.Uuid).Msg("Suppressing SMS reaction echo")
		return
	}

	// Skip APNs messages that were already bridged (e.g. via CloudKit backfill
	// or a previous session). After the initial backfill completes and the APNs
	// buffer flushes, delayed APNs deliveries can arrive with IsStoredMessage=false
	// for messages that CloudKit already bridged. Check the Bridge DB for any
	// message whose UUID is already known to prevent duplicates.
	if msg.Uuid != "" {
		portalKey := c.makePortalKey(msg.Participants, msg.GroupName, msg.Sender, msg.SenderGuid)
		if dbMsgs, err := c.Main.Bridge.DB.Message.GetAllPartsByID(
			context.Background(), c.UserLogin.ID, makeMessageID(msg.Uuid),
		); err == nil && len(dbMsgs) > 0 {
			// Only skip if the existing message is in the SAME portal.
			// Apple's SMS relay can reuse UUIDs across different short-code
			// conversations, causing false-positive dedup drops.
			if dbMsgs[0].Room.ID == portalKey.ID {
				log.Debug().Str("uuid", msg.Uuid).Bool("is_stored", msg.IsStoredMessage).Msg("Skipping message already in bridge DB")
				return
			}
			log.Info().Str("uuid", msg.Uuid).
				Str("existing_portal", string(dbMsgs[0].Room.ID)).
				Str("new_portal", string(portalKey.ID)).
				Msg("UUID collision across portals — allowing message through")
		}
		// Also check for UUID_* suffix variants (e.g. UUID_att0, UUID_att1, UUID_avid).
		// Two past regressions left image messages in the bridge DB with suffixed
		// IDs instead of the base UUID:
		//
		//   1. Pre-0755816: the attachment-index condition used the raw msg.Text
		//      value (non-empty for "\ufffc" placeholder) instead of the stripped
		//      form, so ALL image-only APNs messages were stored as UUID_att0.
		//
		//   2. baf5354 era: injectLivePhotoCompanion appended the MOV companion
		//      at original index 1 and the HEIC-drop filter kept it there, so
		//      Live Photo companions were stored as UUID_att1.
		//
		// An exact-match lookup on UUID misses both. A LIKE prefix query catches
		// all suffix forms with a single round-trip.
		rows, likeErr := c.Main.Bridge.DB.Database.Query(
			context.Background(),
			`SELECT 1 FROM message
			 WHERE bridge_id=$1 AND (room_receiver=$2 OR room_receiver='') AND id LIKE $3
			 LIMIT 1`,
			c.Main.Bridge.ID, c.UserLogin.ID, string(makeMessageID(msg.Uuid))+"_%",
		)
		if likeErr == nil {
			found := rows.Next()
			_ = rows.Close()
			if found {
				log.Debug().Str("uuid", msg.Uuid).Bool("is_stored", msg.IsStoredMessage).Msg("Skipping message: UUID suffix variant found in bridge DB")
				return
			}
		}
	}

	sender := c.makeEventSender(msg.Sender)
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName, msg.Sender, msg.SenderGuid)
	sender = c.canonicalizeDMSender(portalKey, sender)

	// Keep the stored gid: group roster in sync with the sender's current view
	// of the conversation. Only trust live (non-stored) messages — a stored
	// backfill message carries the membership as it was at that message's time,
	// which may be older than the current roster. This is what retroactively
	// drops a member who already left (or was never removed from the stale
	// insert-once roster) the next time anyone in the group sends a message.
	if !msg.IsStoredMessage {
		c.persistGroupParticipantsIfChanged(string(portalKey.ID), msg.Participants)
	}

	// Eagerly learn sender-handle → portal mapping for StatusKit presence
	// correlation. A peer iPhone that reshares Focus keys addresses the APS
	// payload to whichever of the user's handles they use to reach them — so
	// a reshare can arrive at bridge with a sender handle that has no matching
	// portal ID (e.g. mailto:x@y.com when the portal is tel:+X). The iCloud
	// CardDAV-based resolver often fails for peers missing from contacts. By
	// stamping the cache on every inbound 1:1 message, we ensure any peer
	// who's ever messaged bridge has a direct lookup path for presence.
	if msg.Sender != nil && *msg.Sender != "" && !isGroupPortalID(string(portalKey.ID)) && portalKey.ID != "unknown" {
		c.rememberAliasPortal(context.Background(), *msg.Sender, portalKey.ID)
	}

	// Drop messages that couldn't be resolved to a real portal.
	// makePortalKey returns ID:"unknown" when participants and sender are both
	// empty/unresolvable. Letting these through creates a junk "unknown" room.
	if portalKey.ID == "unknown" {
		log.Warn().
			Str("msg_uuid", msg.Uuid).
			Strs("participants", msg.Participants).
			Msg("Dropping message: could not resolve portal key (no participants/sender)")
		return
	}

	// SMS/RCS group reactions and other messages sometimes arrive from the
	// iPhone relay with an empty participant list. makePortalKey() then falls
	// back to [sender, our_number] and computes a DM portal key, which can split
	// one group chat into spurious DM portals.
	//
	// Only redirect DM->group when there is unambiguous evidence that this
	// payload belongs to a known group. If evidence is ambiguous, keep the DM
	// portal to avoid misrouting legitimate 1:1 SMS traffic.
	if msg.IsSms && isComputedDMPortalID(portalKey.ID) {
		if groupKey, ok := c.resolveSMSGroupRedirectPortal(msg); ok {
			log.Debug().
				Str("sender", ptr.Val(msg.Sender)).
				Str("sender_guid", ptr.Val(msg.SenderGuid)).
				Str("dm_portal", string(portalKey.ID)).
				Str("group_portal", string(groupKey.ID)).
				Msg("Redirecting SMS message from DM portal to known group portal")
			portalKey = groupKey
		}
	}

	// Track SMS portals so outbound replies use the correct service type.
	// Unconditional so SMS→iMessage transitions are reflected immediately.
	smsChanged := c.updatePortalSMS(string(portalKey.ID), msg.IsSms)

	// Only create new portals after CloudKit sync is done.
	cloudSyncDone := c.isCloudSyncDone()
	createPortal := cloudSyncDone

	// Suppress stale echoes that would resurrect deleted portals.
	// Two cases:
	// 1. Portal is mid-deletion (still has MXID, but in recentlyDeletedPortals):
	//    Drop known UUIDs — they're echoes of messages we already bridged.
	//    Unknown UUIDs are only allowed through if they advance the deleted tail.
	// 2. Portal is fully gone (no MXID): Drop known UUIDs — CloudKit sync
	//    knew about this message but chose not to create a portal.
	portalID := string(portalKey.ID)
	c.recentlyDeletedPortalsMu.RLock()
	deletedEntry, isDeletedPortal := c.recentlyDeletedPortals[portalID]
	c.recentlyDeletedPortalsMu.RUnlock()
	backgroundCtx := context.Background()
	msgTS := int64(msg.TimestampMs)
	existingPortal, _ := c.Main.Bridge.GetExistingPortalByKey(backgroundCtx, portalKey)

	// Persist IsSms change to DB immediately so it survives a crash.
	// Without this, the in-memory update above would be lost on restart
	// because loadSenderGuidsFromDB only loads IsSms=true entries.
	if smsChanged && existingPortal != nil {
		meta, ok := existingPortal.Metadata.(*PortalMetadata)
		if !ok {
			meta = &PortalMetadata{}
		}
		if meta.IsSms != msg.IsSms {
			meta.IsSms = msg.IsSms
			existingPortal.Metadata = meta
			if err := existingPortal.Save(backgroundCtx); err != nil {
				log.Warn().Err(err).
					Str("portal_id", portalID).
					Bool("is_sms", msg.IsSms).
					Msg("Failed to persist IsSms change to database")
			} else {
				log.Debug().
					Str("portal_id", portalID).
					Bool("is_sms", msg.IsSms).
					Msg("Persisted IsSms change to database")
			}
		}
	}
	missingPortal := existingPortal == nil || existingPortal.MXID == ""

	// Lazy-load soft-deleted portal info. Only queries the DB when we
	// actually need it (deleted portal checks or soft-delete guard), not
	// on every message to a missing portal.
	var softDeletedInfo softDeletedPortalInfo
	softDeletedInfoLoaded := false
	loadSoftDeletedInfo := func() bool {
		if softDeletedInfoLoaded {
			return softDeletedInfo.Deleted
		}
		softDeletedInfoLoaded = true
		if c.cloudStore == nil {
			return false
		}
		info, err := c.cloudStore.getSoftDeletedPortalInfo(backgroundCtx, portalID)
		if err != nil {
			log.Warn().Err(err).Str("portal_id", portalID).Msg("Failed to query soft-deleted portal state")
			return false
		}
		softDeletedInfo = info
		return info.Deleted
	}

	// reviveAndAllow clears the chat-level deleted state and flushes
	// held messages so the genuinely newer message can create a portal.
	// Returns true if the revive succeeded (or wasn't needed).
	reviveAndAllow := func(reason string) bool {
		revived := true
		if softDeletedInfo.Deleted {
			if err := c.reviveDeletedPortalShell(backgroundCtx, portalID); err != nil {
				log.Warn().Err(err).Str("portal_id", portalID).Msg("Failed to revive deleted chat shell before allowing newer message")
				revived = false
			}
		}
		log.Info().
			Str("portal_id", portalID).
			Str("msg_uuid", msg.Uuid).
			Bool("is_tombstone", deletedEntry.isTombstone).
			Int64("deleted_tail_ts", softDeletedInfo.NewestTS).
			Uint64("msg_ts", msg.TimestampMs).
			Msg(reason)
		if c.msgBuffer != nil {
			c.msgBuffer.flush()
		}
		c.flushPendingPortalMsgs()
		return revived
	}

	// Suppress stale echoes that would resurrect deleted portals.
	if isDeletedPortal && createPortal {
		if missingPortal {
			// Compare against the deleted chat's own latest known timestamp.
			// OpenBubbles unconditionally revives on any message; we add
			// tail-timestamp gating to handle APNs replays that OpenBubbles
			// doesn't encounter (it syncs via CloudKit, not realtime APNs).
			isSoftDeleted := loadSoftDeletedInfo()
			if isSoftDeleted && softDeletedInfo.NewestTS > 0 && msgTS <= softDeletedInfo.NewestTS {
				log.Info().
					Str("portal_id", portalID).
					Str("msg_uuid", msg.Uuid).
					Bool("is_tombstone", deletedEntry.isTombstone).
					Int64("deleted_tail_ts", softDeletedInfo.NewestTS).
					Uint64("msg_ts", msg.TimestampMs).
					Msg("Dropped message: replay for deleted portal is not newer than deleted tail")
				return
			}
			// Use deletedAt as the suppression threshold when available:
			// APNs can replay messages sent ANY time before re-connect,
			// including messages sent after startup but before the delete.
			// startupTime is too coarse — deletedAt catches the exact boundary.
			suppressBefore := deletedEntry.deletedAt.UnixMilli()
			if suppressBefore <= 0 {
				suppressBefore = c.startupTime.UnixMilli()
			}
			if !isSoftDeleted && suppressBefore > 0 && int64(msg.TimestampMs) < suppressBefore {
				log.Info().
					Str("portal_id", portalID).
					Str("msg_uuid", msg.Uuid).
					Bool("is_tombstone", deletedEntry.isTombstone).
					Uint64("msg_ts", msg.TimestampMs).
					Int64("suppress_before_ts", suppressBefore).
					Msg("Dropped message: pre-delete message for deleted portal (stale echo fallback)")
				return
			}
			if reviveAndAllow("Newer message for deleted portal — reviving chat shell and allowing new conversation") {
				isDeletedPortal = false
			}
			// Fall through with createPortal=true to create the new portal.
		} else {
			// Portal still has an MXID (mid-deletion): route to existing room
			// but don't create a fresh one.
			createPortal = false
		}
	}

	if c.cloudStore != nil {
		if known, _ := c.cloudStore.hasMessageUUID(backgroundCtx, msg.Uuid); known {
			if isDeletedPortal {
				// Portal mid-deletion with a known UUID — stale echo.
				log.Info().
					Str("portal_id", portalID).
					Str("msg_uuid", msg.Uuid).
					Msg("Dropped message: known UUID for recently-deleted portal (mid-deletion echo)")
				return
			}
			if createPortal && missingPortal {
				// Portal fully deleted. No bridge portal exists.
				log.Info().
					Str("portal_id", portalID).
					Str("msg_uuid", msg.Uuid).
					Msg("Dropped message: known UUID with no portal (stale echo of deleted chat)")
				return
			}
		}
	}

	// Suppress stale APNs re-delivery for chats that are still soft-deleted in
	// our DB, including after restart when recentlyDeletedPortals is empty.
	// Only genuinely newer traffic is allowed to recreate the portal.
	if createPortal && missingPortal && !isDeletedPortal && loadSoftDeletedInfo() {
		if softDeletedInfo.NewestTS > 0 && msgTS <= softDeletedInfo.NewestTS {
			log.Info().
				Str("portal_id", portalID).
				Str("msg_uuid", msg.Uuid).
				Int64("deleted_tail_ts", softDeletedInfo.NewestTS).
				Uint64("msg_ts", msg.TimestampMs).
				Msg("Dropped message: soft-deleted chat replay is not newer than deleted tail")
			return
		}
		reviveAndAllow("Newer message for soft-deleted chat — reviving chat shell and allowing portal recreation")
	}

	// Hold messages for portals that don't exist yet while CloudKit sync
	// is in progress. Without this, the framework drops events where
	// CreatePortal=false and portal.MXID="". We intentionally skip UUID
	// persistence here so that replayed messages aren't mistakenly treated
	// as known echoes by hasMessageUUID on the second pass.
	if !createPortal {
		if missingPortal {
			c.pendingPortalMsgsMu.Lock()
			c.pendingPortalMsgs = append(c.pendingPortalMsgs, msg)
			c.pendingPortalMsgsMu.Unlock()
			log.Info().
				Str("portal_id", portalID).
				Str("msg_uuid", msg.Uuid).
				Msg("Held message pending CloudKit sync completion (portal doesn't exist yet)")
			return
		}
	}

	// Persist this message UUID so cross-restart echoes are detected.
	// hasMessageUUID checks cloud_message regardless of the deleted flag,
	// so soft-deleted UUIDs from prior portal deletions still match.
	if c.cloudStore != nil {
		if err := c.cloudStore.persistMessageUUID(context.Background(), msg.Uuid, string(portalKey.ID), int64(msg.TimestampMs), sender.IsFromMe); err != nil {
			log.Warn().Err(err).Str("uuid", msg.Uuid).Msg("Failed to persist message UUID; duplicates may occur on restart")
		}
	}
	c.maybeNotifyIncomingFaceTimeInvite(log, &msg, portalKey, sender.IsFromMe, createPortal)
	if createPortal || sender.IsFromMe {
		log.Info().
			Bool("create_portal", createPortal).
			Bool("cloud_sync_done", cloudSyncDone).
			Bool("is_stored_message", msg.IsStoredMessage).
			Bool("is_from_me", sender.IsFromMe).
			Str("portal_id", string(portalKey.ID)).
			Msg("Portal creation decision for message")
	}

	hasText := msg.Text != nil && *msg.Text != "" && strings.TrimRight(*msg.Text, "\ufffc \n") != ""
	if hasText {
		c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Message[*rustpushgo.WrappedMessage]{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventMessage,
				PortalKey:    portalKey,
				CreatePortal: createPortal,
				Sender:       sender,
				Timestamp:    time.UnixMilli(int64(msg.TimestampMs)),
				LogContext: func(lc zerolog.Context) zerolog.Context {
					return lc.Str("msg_uuid", msg.Uuid)
				},
			},
			Data:               &msg,
			ID:                 makeMessageID(msg.Uuid),
			ConvertMessageFunc: convertMessage,
		})
	}

	// Live Photo handling: bridge both the still image and the video.
	attIndex := 0
	for _, att := range msg.Attachments {
		// Skip rich link sideband attachments (handled in convertMessage)
		if att.MimeType == "x-richlink/meta" || att.MimeType == "x-richlink/image" {
			continue
		}
		attID := msg.Uuid
		if attIndex > 0 || hasText {
			attID = fmt.Sprintf("%s_att%d", msg.Uuid, attIndex)
		}
		attMsg := &attachmentMessage{
			WrappedMessage: &msg,
			Attachment:     &att,
			Index:          attIndex,
		}
		attIndex++
		c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Message[*attachmentMessage]{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventMessage,
				PortalKey:    portalKey,
				CreatePortal: createPortal,
				Sender:       sender,
				Timestamp:    time.UnixMilli(int64(msg.TimestampMs)),
				LogContext: func(lc zerolog.Context) zerolog.Context {
					return lc.Str("msg_uuid", attID)
				},
			},
			Data: attMsg,
			ID:   makeMessageID(attID),
			ConvertMessageFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, data *attachmentMessage) (*bridgev2.ConvertedMessage, error) {
				// Layer-2 MMCS retry enqueue: when download_mmcs_attachments
				// (pkg/rustpushgo/src/lib.rs) exhausts the Layer-1 retries,
				// it leaves the attachment non-inline with no bytes but
				// keeps the MMCS descriptor JSON. Record that shape so the
				// background retrier can replay the download later and edit
				// the notice placeholder into the real m.image/m.video.
				if data.Attachment != nil && !data.Attachment.IsInline &&
					data.Attachment.InlineData == nil &&
					data.Attachment.MmcsDescriptorJson != nil &&
					*data.Attachment.MmcsDescriptorJson != "" {
					c.enqueuePendingMMCSRecovery(ctx, portal, data)
				}
				return convertAttachment(ctx, portal, intent, data, c.Main.Config.VideoTranscoding, c.Main.Config.HEICConversion, c.Main.Config.HEICJPEGQuality)
			},
		})
	}
}

func (c *IMClient) handleTapback(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	// Skip stored (buffered) tapbacks — CloudKit backfill handles those via
	// BackfillReaction at the correct historical position. Processing them here
	// as live events gives them current server time, placing them at the end of
	// the room timeline and making Beeper show a stale reaction as the room
	// preview instead of the most recent message.
	// Same pattern as handleRename and handleParticipantChange.
	// Regression introduced by PR #20 (lwittwer/pr/tapback-edit-dedup, 693d353):
	// that commit added IsStoredMessage guards to handleEdit but omitted them
	// for handleTapback, leaving stored tapbacks to fire as live events.
	if msg.IsStoredMessage {
		log.Debug().Str("uuid", msg.Uuid).Msg("Skipping stored tapback")
		return
	}

	// Deduplicate: skip tapbacks already processed (e.g. via CloudKit backfill)
	// to prevent duplicate reactions and notifications from stale APNs re-delivery.
	// Uses the same cloud_message UUID table as handleMessage (primary key lookup).
	if msg.Uuid != "" && c.cloudStore != nil {
		if known, _ := c.cloudStore.hasMessageUUID(context.Background(), msg.Uuid); known {
			log.Debug().Str("uuid", msg.Uuid).Bool("is_stored", msg.IsStoredMessage).Msg("Skipping tapback already in message store")
			return
		}
	}

	targetGUID := ptrStringOr(msg.TapbackTargetUuid, "")

	// Resolve portal by target message UUID first as a safety net.
	// Tapbacks usually have correct participants from the payload, but
	// self-reflections can still have participants=[self, self].
	portalKey := c.resolvePortalByTargetMessage(log, targetGUID)
	if portalKey.ID == "" {
		portalKey = c.makePortalKey(msg.Participants, msg.GroupName, msg.Sender, msg.SenderGuid)
	}

	// Persist the tapback UUID so cross-restart APNs re-deliveries are caught
	// by the hasMessageUUID check above. Uses persistTapbackUUID (not
	// persistMessageUUID) to set tapback_type, preventing getConversationReadByMe
	// from treating the synthetic row as a substantive message and spuriously
	// flipping the conversation to unread for incoming reactions.
	// APNs TapbackType is a 0-6 index; cloud_message stores raw 2000-2006 / 3000-3006.
	if msg.Uuid != "" && c.cloudStore != nil {
		sender := c.makeEventSender(msg.Sender)
		storedType := uint32(2000) // sentinel default (Love) when TapbackType is nil
		if msg.TapbackType != nil {
			storedType = *msg.TapbackType + 2000
			if msg.TapbackRemove {
				storedType += 1000 // removals: 3000-3006, matching TapbackRemoveOffset
			}
		}
		if err := c.cloudStore.persistTapbackUUID(context.Background(), msg.Uuid, string(portalKey.ID), int64(msg.TimestampMs), sender.IsFromMe, storedType); err != nil {
			log.Warn().Err(err).Str("uuid", msg.Uuid).Msg("Failed to persist tapback UUID; duplicates may occur on restart")
		}
	}

	// Sticker tapbacks (type 7) carry an image placed on top of a message
	// bubble. Matrix reactions are text-only, so bridge the sticker as an
	// image message replying to the target instead.
	if msg.TapbackType != nil && *msg.TapbackType == 7 && msg.StickerData != nil && len(*msg.StickerData) > 0 {
		stickerData := *msg.StickerData
		stickerMime := "image/png"
		if msg.StickerMime != nil && *msg.StickerMime != "" {
			stickerMime = *msg.StickerMime
		}
		c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Message[*stickerTapbackData]{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventMessage,
				PortalKey: portalKey,
				Sender:    c.canonicalizeDMSender(portalKey, c.makeEventSender(msg.Sender)),
				Timestamp: time.UnixMilli(int64(msg.TimestampMs)),
			},
			Data: &stickerTapbackData{
				ImageData: stickerData,
				MimeType:  stickerMime,
				TargetID:  targetGUID,
			},
			ID: makeMessageID(msg.Uuid),
			ConvertMessageFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, data *stickerTapbackData) (*bridgev2.ConvertedMessage, error) {
				return convertStickerTapback(ctx, intent, data)
			},
		})
		return
	}

	emoji := tapbackTypeToEmoji(msg.TapbackType, msg.TapbackEmoji)

	evtType := bridgev2.RemoteEventReaction
	if msg.TapbackRemove {
		evtType = bridgev2.RemoteEventReactionRemove
	}

	tapbackPart := 0
	if msg.TapbackTargetPart != nil {
		tapbackPart = int(*msg.TapbackTargetPart)
	}
	tapbackTargetMsgID := c.resolveTapbackTargetID(targetGUID, tapbackPart)

	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Reaction{
		EventMeta: simplevent.EventMeta{
			Type:      evtType,
			PortalKey: portalKey,
			Sender:    c.canonicalizeDMSender(portalKey, c.makeEventSender(msg.Sender)),
			Timestamp: time.UnixMilli(int64(msg.TimestampMs)),
		},
		TargetMessage: tapbackTargetMsgID,
		Emoji:         emoji,
	})
}

func (c *IMClient) handleEdit(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	targetGUID := ptrStringOr(msg.EditTargetUuid, "")

	// Resolve portal by target message UUID first. Edit reflections from the
	// user's own devices have participants=[self, self], so makePortalKey can't
	// determine the correct DM portal.
	portalKey := c.resolvePortalByTargetMessage(log, targetGUID)
	if portalKey.ID == "" {
		portalKey = c.makePortalKey(msg.Participants, msg.GroupName, msg.Sender, msg.SenderGuid)
	}

	newText := ptrStringOr(msg.EditNewText, "")

	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Message[string]{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventEdit,
			PortalKey: portalKey,
			Sender:    c.canonicalizeDMSender(portalKey, c.makeEventSender(msg.Sender)),
			Timestamp: time.UnixMilli(int64(msg.TimestampMs)),
		},
		Data:          newText,
		ID:            makeMessageID(msg.Uuid),
		TargetMessage: makeMessageID(targetGUID),
		ConvertEditFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, existing []*database.Message, text string) (*bridgev2.ConvertedEdit, error) {
			var targetPart *database.Message
			if len(existing) > 0 {
				targetPart = existing[0]
			}
			return &bridgev2.ConvertedEdit{
				ModifiedParts: []*bridgev2.ConvertedEditPart{{
					Part: targetPart,
					Type: event.EventMessage,
					Content: &event.MessageEventContent{
						MsgType: event.MsgText,
						Body:    text,
					},
				}},
			}, nil
		},
	})
}

func (c *IMClient) handleUnsend(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	targetGUID := ptrStringOr(msg.UnsendTargetUuid, "")

	// Suppress echo of unsends initiated from Matrix.
	if c.wasOutboundUnsend(targetGUID) {
		log.Debug().Str("target_uuid", targetGUID).Msg("Suppressing echo of outbound unsend")
		return
	}

	c.trackUnsend(targetGUID)

	// Resolve portal by target message UUID first. Unsend reflections from the
	// user's own devices have participants=[self, self], so makePortalKey can't
	// determine the correct DM portal.
	portalKey := c.resolvePortalByTargetMessage(log, targetGUID)
	if portalKey.ID == "" {
		portalKey = c.makePortalKey(msg.Participants, msg.GroupName, msg.Sender, msg.SenderGuid)
	}

	// Scrub-before-emit, fail-closed: same pattern as handleMessageDelete.
	// QueueRemoteEvent will (eventually) cause bridgev2 to delete its message
	// row; once gone, the periodic scrubber's EXISTS predicate is permanently
	// FALSE for this guid. Skip the emit on scrub error so the Matrix event
	// stays visible rather than leaking plaintext.
	if c.cloudStore != nil {
		// 10s (> the SQLite busy_timeout of 5s) so a one-off lock contention
		// with the first-boot chunked scrubber doesn't cancel the write early
		// and strand this Apple-side unsend as a stale, still-visible Matrix
		// event (APNs won't replay it).
		scrubCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := c.cloudStore.softDeleteMessageByGUID(scrubCtx, targetGUID)
		cancel()
		if err != nil {
			log.Error().Err(err).Str("target_uuid", targetGUID).
				Msg("Skipping unsend MessageRemove emit because cloud_message scrub failed — preserving privacy at the cost of a stale Matrix event")
			return
		}
	}

	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.MessageRemove{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventMessageRemove,
			PortalKey: portalKey,
			Sender:    c.canonicalizeDMSender(portalKey, c.makeEventSender(msg.Sender)),
			Timestamp: time.UnixMilli(int64(msg.TimestampMs)),
		},
		TargetMessage: makeMessageID(targetGUID),
	})
}

func (c *IMClient) handleRename(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	// Skip stored (backfilled) rename messages to prevent spurious
	// room name change events from historical renames delivered on reconnect.
	if msg.IsStoredMessage {
		log.Debug().Str("uuid", msg.Uuid).Msg("Skipping stored rename message")
		return
	}
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName, msg.Sender, msg.SenderGuid)
	newName := ptrStringOr(msg.NewChatName, "")

	// Update the cached iMessage group name to the NEW name so outbound
	// messages (portalToConversation) use it. makePortalKey cached whatever
	// was in the conversation envelope (msg.GroupName), which may be the old
	// name. Also persist to portal metadata so it survives restarts.
	if newName != "" {
		portalID := string(portalKey.ID)
		c.imGroupNamesMu.Lock()
		c.imGroupNames[portalID] = newName
		c.imGroupNamesMu.Unlock()

		go func() {
			ctx := context.Background()
			portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, portalKey)
			if err == nil && portal != nil {
				meta := &PortalMetadata{}
				if existing, ok := portal.Metadata.(*PortalMetadata); ok {
					*meta = *existing
				}
				if meta.GroupName != newName {
					meta.GroupName = newName
					portal.Metadata = meta
					_ = portal.Save(ctx)
				}
			}
			// Also correct the stale CloudKit display_name in cloud_chat
			// so resolveGroupName doesn't fall back to the old name.
			if c.cloudStore != nil {
				if err := c.cloudStore.updateDisplayNameByPortalID(ctx, portalID, newName); err != nil {
					log.Warn().Err(err).Str("portal_id", portalID).Msg("Failed to update cloud_chat display_name after rename")
				}
			}
		}()
	}

	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.ChatInfoChange{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventChatInfoChange,
			PortalKey: portalKey,
			Sender:    c.makeEventSender(msg.Sender),
			Timestamp: time.UnixMilli(int64(msg.TimestampMs)),
		},
		ChatInfoChange: &bridgev2.ChatInfoChange{
			ChatInfo: &bridgev2.ChatInfo{
				Name: &newName,
			},
		},
	})
}

func (c *IMClient) handleParticipantChange(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	// Skip stored (backfilled) participant change messages to prevent spurious
	// member change events from historical changes delivered on reconnect.
	if msg.IsStoredMessage {
		log.Debug().Str("uuid", msg.Uuid).Msg("Skipping stored participant change message")
		return
	}
	// Resolve the existing portal from the OLD participant list.
	oldPortalKey := c.makePortalKey(msg.Participants, msg.GroupName, msg.Sender, msg.SenderGuid)

	if len(msg.NewParticipants) == 0 {
		// No new participant list — fall back to a resync with current info.
		log.Warn().Msg("Participant change with empty NewParticipants, falling back to resync")
		c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventChatResync,
				PortalKey: oldPortalKey,
			},
			GetChatInfoFunc: c.GetChatInfo,
		})
		return
	}

	// Compute new portal ID from the NEW participant list using the same
	// normalization / dedup / sort logic as makePortalKey's group branch.
	deduped := c.buildCanonicalParticipantList(msg.NewParticipants)
	newPortalIDStr := strings.Join(deduped, ",")
	oldPortalIDStr := string(oldPortalKey.ID)

	// Refresh the persisted roster for a gid: portal immediately on an explicit
	// membership change, so the outbound recipient list drops a departed member
	// even if the portal is not re-keyed (gid: portals keep a stable ID; only
	// comma-separated portals re-key below). No-op for comma portals, which
	// derive recipients from the portal ID itself.
	c.persistGroupParticipantsIfChanged(oldPortalIDStr, msg.NewParticipants)

	// If the portal ID changed (member added/removed), re-key it in the DB.
	finalPortalKey := oldPortalKey
	if newPortalIDStr != oldPortalIDStr {
		ctx := context.Background()
		newPortalKey := networkid.PortalKey{
			ID:       networkid.PortalID(newPortalIDStr),
			Receiver: c.UserLogin.ID,
		}
		result, _, err := c.reIDPortalWithCacheUpdate(ctx, oldPortalKey, newPortalKey)
		if err != nil {
			log.Err(err).
				Str("old_portal_id", oldPortalIDStr).
				Str("new_portal_id", newPortalIDStr).
				Msg("Failed to ReID portal for participant change")
			return
		}
		log.Info().
			Str("old_portal_id", oldPortalIDStr).
			Str("new_portal_id", newPortalIDStr).
			Int("result", int(result)).
			Msg("ReID portal for participant change")
		finalPortalKey = newPortalKey
	}

	// Cache sender_guid and group_name under the (possibly new) portal ID.
	// For comma-based portals and gid: portals, cache the sender_guid so
	// future messages can be resolved to this portal. This is skipped for
	// other portal ID formats to avoid poisoning the cache with unrelated
	// sender_guids.
	if msg.SenderGuid != nil && *msg.SenderGuid != "" {
		portalIDStr := string(finalPortalKey.ID)
		isGidPortal := strings.HasPrefix(portalIDStr, "gid:")
		if strings.Contains(portalIDStr, ",") || isGidPortal {
			c.imGroupGuidsMu.Lock()
			c.imGroupGuids[portalIDStr] = *msg.SenderGuid
			c.imGroupGuidsMu.Unlock()
		}
	}
	if msg.GroupName != nil && *msg.GroupName != "" {
		c.imGroupNamesMu.Lock()
		c.imGroupNames[string(finalPortalKey.ID)] = *msg.GroupName
		c.imGroupNamesMu.Unlock()
	}

	// Build the full new member list for Matrix room sync.
	memberMap := make(map[networkid.UserID]bridgev2.ChatMember, len(msg.NewParticipants))
	for _, p := range msg.NewParticipants {
		normalized := normalizeIdentifierForPortalID(p)
		if normalized == "" {
			continue
		}
		userID := makeUserID(normalized)
		if c.isMyHandle(normalized) {
			memberMap[userID] = bridgev2.ChatMember{
				EventSender: bridgev2.EventSender{
					IsFromMe:    true,
					SenderLogin: c.UserLogin.ID,
					Sender:      userID,
				},
				Membership: event.MembershipJoin,
			}
		} else {
			memberMap[userID] = bridgev2.ChatMember{
				EventSender: bridgev2.EventSender{Sender: userID},
				Membership:  event.MembershipJoin,
			}
		}
	}

	// Queue a ChatInfoChange with the full member list so bridgev2 syncs
	// the Matrix room membership (invites new members, kicks removed ones).
	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.ChatInfoChange{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventChatInfoChange,
			PortalKey: finalPortalKey,
			Sender:    c.makeEventSender(msg.Sender),
			Timestamp: time.UnixMilli(int64(msg.TimestampMs)),
		},
		ChatInfoChange: &bridgev2.ChatInfoChange{
			MemberChanges: &bridgev2.ChatMemberList{
				IsFull:    true,
				MemberMap: memberMap,
			},
		},
	})
}

// safeCloudDownloadGroupPhoto wraps the FFI call with panic recovery and a
// 90-second timeout, matching the pattern used by safeCloudDownloadAttachment.
func safeCloudDownloadGroupPhoto(ctx context.Context, client *rustpushgo.Client, recordName string) ([]byte, error) {
	type dlResult struct {
		data []byte
		err  error
	}
	ch := make(chan dlResult, 1)
	go func() {
		var res dlResult
		defer func() {
			if r := recover(); r != nil {
				stack := string(debug.Stack())
				log.Error().Str("ffi_method", "CloudDownloadGroupPhoto").
					Str("record_name", recordName).
					Str("stack", stack).
					Msgf("FFI panic recovered: %v", r)
				res = dlResult{err: fmt.Errorf("FFI panic in CloudDownloadGroupPhoto: %v", r)}
			}
			ch <- res
		}()
		d, e := client.CloudDownloadGroupPhoto(recordName)
		res = dlResult{data: d, err: e}
	}()
	select {
	case res := <-ch:
		return res.data, res.err
	case <-time.After(90 * time.Second):
		log.Error().Str("ffi_method", "CloudDownloadGroupPhoto").
			Str("record_name", recordName).
			Msg("CloudDownloadGroupPhoto timed out after 90s — inner goroutine leaked until FFI unblocks")
		return nil, fmt.Errorf("CloudDownloadGroupPhoto timed out after 90s")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// fetchAndCacheGroupPhoto attempts to download the group photo from CloudKit
// using the record_name stored during chat sync. It tries the CloudKit "gp"
// (group photo) asset field on the chat record. Apple's iMessage clients may
// not always write to this field (preferring MMCS delivery via APNs IconChange
// messages), so failures are expected and logged at debug level rather than
// warning. On success, the bytes are persisted to group_photo_cache so
// subsequent GetChatInfo calls can serve the photo without a network round-trip.
// Returns (photoData, timestampMs); both are zero on any failure.
func (c *IMClient) fetchAndCacheGroupPhoto(ctx context.Context, log zerolog.Logger, portalID string) ([]byte, int64) {
	if c.cloudStore == nil {
		return nil, 0
	}
	_, recordName, err := c.cloudStore.getGroupPhotoByPortalID(ctx, portalID)
	if err != nil {
		log.Debug().Err(err).Msg("group_photo: failed to look up record_name for CloudKit download")
		return nil, 0
	}
	if recordName == "" {
		log.Debug().Msg("group_photo: no group_photo_guid in cloud_chat, skipping CloudKit download")
		return nil, 0
	}
	log.Debug().Str("record_name", recordName).Msg("group_photo: attempting CloudKit download")
	data, dlErr := safeCloudDownloadGroupPhoto(ctx, c.client, recordName)
	if dlErr != nil {
		log.Debug().Err(dlErr).Str("record_name", recordName).
			Msg("group_photo: CloudKit download failed (expected if Apple did not write gp asset)")
		return nil, 0
	}
	if len(data) == 0 {
		log.Debug().Str("record_name", recordName).Msg("group_photo: CloudKit download returned empty data")
		return nil, 0
	}
	ts := time.Now().UnixMilli()
	if saveErr := c.cloudStore.saveGroupPhoto(ctx, portalID, ts, data); saveErr != nil {
		log.Warn().Err(saveErr).Msg("group_photo: failed to cache downloaded photo")
	} else {
		log.Info().Str("record_name", recordName).Int("bytes", len(data)).
			Msg("group_photo: downloaded and cached from CloudKit")
	}
	return data, ts
}

// handleIconChange processes a group photo (icon) change from APNs.
// When a participant changes or clears the group photo from their iMessage
// client, Apple delivers an IconChange message with MMCS transfer data.
// The Rust layer downloads the photo inline; we apply it directly to the room.
//
// Stored (IsStoredMessage) icon changes are NOT skipped: when the bridge
// reconnects after being offline, Apple may deliver a queued IconChange with
// valid MMCS data, letting us catch up on changes that occurred while offline.
// If the MMCS URL has expired Rust returns nil bytes and we warn harmlessly.
func (c *IMClient) handleIconChange(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName, msg.Sender, msg.SenderGuid)
	portalID := string(portalKey.ID)
	log = log.With().Str("portal_id", portalID).Bool("stored", msg.IsStoredMessage).Logger()

	if msg.GroupPhotoCleared {
		// Photo was removed — clear the avatar and wipe the local cache so
		// GetChatInfo won't re-apply a stale photo after a restart.
		log.Info().Msg("Group photo cleared via APNs IconChange")
		if c.cloudStore != nil {
			if err := c.cloudStore.clearGroupPhoto(context.Background(), portalID); err != nil {
				log.Warn().Err(err).Msg("Failed to clear cached group photo in DB")
			}
		}
		c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.ChatInfoChange{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventChatInfoChange,
				PortalKey: portalKey,
				Sender:    c.makeEventSender(msg.Sender),
				Timestamp: time.UnixMilli(int64(msg.TimestampMs)),
			},
			ChatInfoChange: &bridgev2.ChatInfoChange{
				ChatInfo: &bridgev2.ChatInfo{
					Avatar: &bridgev2.Avatar{
						ID:  networkid.AvatarID(""),
						Get: func(ctx context.Context) ([]byte, error) { return nil, nil },
					},
				},
			},
		})
		return
	}

	// New photo set — Rust already downloaded the MMCS bytes inline.
	// Apple delivers group photos via MMCS in IconChange messages (not CloudKit).
	if msg.IconChangePhotoData == nil || len(*msg.IconChangePhotoData) == 0 {
		log.Warn().Msg("IconChange received but photo data is empty (MMCS download may have failed)")
		return
	}
	photoData := *msg.IconChangePhotoData
	// Timestamp-based avatar ID — changes when the photo changes, so bridgev2
	// always re-applies the new avatar even if the portal already has one.
	avatarID := networkid.AvatarID(fmt.Sprintf("icon-change:%d", msg.TimestampMs))
	log.Info().Int("size", len(photoData)).Msg("Group photo changed via APNs IconChange — applying MMCS photo")

	// Persist bytes to DB so GetChatInfo can apply the correct avatar after a
	// restart without a CloudKit round-trip.  Apple's native clients never write
	// to the CloudKit gp asset field — MMCS is the only delivery mechanism.
	if c.cloudStore != nil {
		if err := c.cloudStore.saveGroupPhoto(context.Background(), portalID, int64(msg.TimestampMs), photoData); err != nil {
			log.Warn().Err(err).Msg("Failed to persist group photo bytes to DB")
		}
	}

	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.ChatInfoChange{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventChatInfoChange,
			PortalKey: portalKey,
			Sender:    c.makeEventSender(msg.Sender),
			Timestamp: time.UnixMilli(int64(msg.TimestampMs)),
		},
		ChatInfoChange: &bridgev2.ChatInfoChange{
			ChatInfo: &bridgev2.ChatInfo{
				Avatar: &bridgev2.Avatar{
					ID:  avatarID,
					Get: func(ctx context.Context) ([]byte, error) { return photoData, nil },
				},
			},
		},
	})
}

const faceTimeRingMarker = "[[FACETIME_RING]]"
const faceTimeMissedMarker = "[[FACETIME_MISSED]]"
const faceTimeAnsweredElsewhereMarker = "[[FACETIME_ANSWERED_ELSEWHERE]]"

// handleNotifyAnyways posts a silent bot notice when a contact deliberately
// breaks through our Focus / Do Not Disturb by tapping "Notify Anyway" on their
// device. The notice is delivered to the portal that corresponds to this chat so
// the user can see who sent it. Stored messages are silently dropped.
func (c *IMClient) handleNotifyAnyways(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	if msg.IsStoredMessage {
		log.Debug().Msg("Skipping stored NotifyAnyways message")
		return
	}

	rawText := strings.TrimSpace(ptrStringOr(msg.Text, ""))
	if strings.HasPrefix(rawText, faceTimeRingMarker) ||
		strings.HasPrefix(rawText, faceTimeMissedMarker) ||
		strings.HasPrefix(rawText, faceTimeAnsweredElsewhereMarker) {
		if c.Main.Config.DisableFaceTime {
			return
		}
		switch {
		case strings.HasPrefix(rawText, faceTimeRingMarker):
			c.handleFaceTimeRingNotice(log, msg, rawText)
		case strings.HasPrefix(rawText, faceTimeMissedMarker):
			c.handleFaceTimeMissedNotice(log, msg)
		case strings.HasPrefix(rawText, faceTimeAnsweredElsewhereMarker):
			c.handleFaceTimeAnsweredElsewhereNotice(log, msg)
		}
		return
	}

	ctx := context.Background()
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName, msg.Sender, msg.SenderGuid)
	portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, portalKey)
	if err != nil || portal == nil || portal.MXID == "" {
		log.Debug().Err(err).Msg("NotifyAnyways: no portal found, skipping notice")
		return
	}

	// Resolve a friendly name from the ghost (same as StatusKit notices).
	senderHandle := ptrStringOr(msg.Sender, "")
	name := senderHandle
	if senderHandle != "" {
		ghost, ghostErr := c.Main.Bridge.GetGhostByID(ctx, makeUserID(normalizeIdentifierForPortalID(senderHandle)))
		if ghostErr == nil && ghost != nil && ghost.Name != "" {
			name = ghost.Name
		}
	}

	notice := "🔔 " + name + " sent a Notify Anyway (tapped through Focus / Do Not Disturb)."
	log.Info().
		Str("sender", senderHandle).
		Str("portal_mxid", string(portal.MXID)).
		Msg("NotifyAnyways: posting notice")

	_, sendErr := c.Main.Bridge.Bot.SendMessage(ctx, portal.MXID, event.EventMessage, &event.Content{
		Parsed: &event.MessageEventContent{
			MsgType:  event.MsgNotice,
			Body:     notice,
			Mentions: &event.Mentions{},
		},
	}, nil)
	if sendErr != nil {
		log.Warn().Err(sendErr).Msg("NotifyAnyways: failed to send notice")
	}
}

func (c *IMClient) handleFaceTimeRingNotice(log zerolog.Logger, msg rustpushgo.WrappedMessage, rawText string) {
	ctx := context.Background()
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName, msg.Sender, msg.SenderGuid)
	portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, portalKey)

	senderHandle := ptrStringOr(msg.Sender, "")
	name := stripIdentifierPrefix(senderHandle)
	if senderHandle != "" {
		ghost, ghostErr := c.Main.Bridge.GetGhostByID(ctx, makeUserID(normalizeIdentifierForPortalID(senderHandle)))
		if ghostErr == nil && ghost != nil && ghost.Name != "" {
			name = ghost.Name
		}
	}
	if name == "" {
		name = "someone"
	}

	link := firstFaceTimeLinkInText(rawText)
	if link == "" {
		// Native FaceTime ring — no link embedded. Use the persistent
		// bridge link + pin its session_link to this inbound session guid.
		//
		// Why not GetSessionLink: upstream's get_session_link calls
		// message_session, which sends a LinkCreated wire message to
		// every member of the session — including the peer whose call
		// is currently ringing our handle. Injecting a LinkCreated into
		// peer's active ring disrupts their FT UI and empirically
		// downgrades peer's display to audio-mode ("peer sees avatar,
		// hears audio, no video") even when the user answers natively
		// on a video-capable device. GetSessionLink only avoids this
		// when session.link is already set, which it isn't for native
		// FT calls (no web link embedded in peer's Invitation).
		//
		// Use the pre-minted "nextincomingcall" link slot (mirroring
		// OpenBubbles' rotateIncomingLink pattern at
		// rustpush_service.dart:2699-2702). The pseud has been on
		// Apple's FT server since startup (or since the last inbound
		// call's rotation), giving identity resolution time to fully
		// propagate the pseud↔handle binding before the webview joins.
		// The binding is pinned pre-rotation (while the link is still
		// in "nextincomingcall" slot); rotation below renames it to
		// "incomingcall" while preserving session_link.
		if ft, ftErr := c.client.GetFacetimeClient(); ftErr == nil {
			if generated, genErr := getFaceTimeLinkWithRecovery(ft, c.handle, ftLinkUsageNextIncomingCall); genErr == nil {
				link = generated
				if guid := extractFaceTimeGuid(rawText); guid != "" {
					if bindErr := ft.BindBridgeLinkToSession(c.handle, ftLinkUsageNextIncomingCall, guid); bindErr != nil {
						log.Warn().Err(bindErr).Str("guid", guid).Msg("FaceTimeRing: failed to pin bridge link to inbound session; web answer may route incorrectly")
					}
				}
				// Rotate asynchronously: nextincomingcall → incomingcall,
				// mint fresh nextincomingcall for the next inbound ring.
				// The rotation renames the bound link's slot to
				// "incomingcall" while preserving its session_link.
				go func() {
					_ = rotateIncomingLink(ft, c.handle)
				}()
				// Pre-fill the web page's display-name prompt with the
				// user's own handle so tapping Answer lands in the call
				// without the guest-name typing step (which otherwise
				// creates a second participant distinct from the pseud
				// bound to the user's handle).
				link = appendFaceTimeLinkName(link, c.resolveFaceTimeDisplayName(ctx))
			} else {
				log.Warn().Err(genErr).Msg("FaceTimeRing: failed to generate bridge FaceTime link")
			}
		}
	}

	// Build the notice as markdown so the join link renders as a tappable
	// anchor in the formatted_body. Plain-URL notices aren't autolinked by
	// every Matrix client; wrapping the URL in [text](url) guarantees an
	// <a> tag reaches the client.
	noticeMarkdown := "📞 **Incoming FaceTime call from " + name + ".**"
	if link != "" {
		noticeMarkdown += "\n\n[**Answer FaceTime call**](" + link + ")"
		noticeMarkdown += "\n\nRaw link (if the button above doesn't open): " + link
	}

	sendNotice := func(roomID id.RoomID) error {
		content := format.RenderMarkdown(noticeMarkdown, true, false)
		content.MsgType = event.MsgNotice
		content.Mentions = &event.Mentions{}
		_, sendErr := c.Main.Bridge.Bot.SendMessage(ctx, roomID, event.EventMessage, &event.Content{
			Parsed: &content,
		}, nil)
		return sendErr
	}

	if err == nil && portal != nil && portal.MXID != "" {
		if sendErr := sendNotice(portal.MXID); sendErr == nil {
			log.Info().
				Str("sender", senderHandle).
				Str("portal_mxid", string(portal.MXID)).
				Msg("FaceTimeRing: posted incoming call notice to portal")
			return
		} else {
			log.Warn().Err(sendErr).Msg("FaceTimeRing: failed to send portal notice")
		}
	}

	mgmtRoom, mgmtErr := c.UserLogin.User.GetManagementRoom(ctx)
	if mgmtErr != nil {
		log.Warn().Err(mgmtErr).Msg("FaceTimeRing: failed to get management room for fallback notice")
		return
	}
	if sendErr := sendNotice(mgmtRoom); sendErr != nil {
		log.Warn().Err(sendErr).Msg("FaceTimeRing: failed to send management room notice")
		return
	}
	log.Info().
		Str("sender", senderHandle).
		Str("management_room", string(mgmtRoom)).
		Msg("FaceTimeRing: posted incoming call notice to management room")
}

func (c *IMClient) handleFaceTimeMissedNotice(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	ctx := context.Background()
	senderHandle := ptrStringOr(msg.Sender, "")
	name := stripIdentifierPrefix(senderHandle)
	if senderHandle != "" {
		ghost, ghostErr := c.Main.Bridge.GetGhostByID(ctx, makeUserID(normalizeIdentifierForPortalID(senderHandle)))
		if ghostErr == nil && ghost != nil && ghost.Name != "" {
			name = ghost.Name
		}
	}
	if name == "" {
		name = "someone"
	}

	// Build a call-back button that uses the bridge's pending-ring flow,
	// same mechanism as the outbound `!im facetime` command. Tap → letmein
	// approve adds the user to a pre-armed session → JoinEvent fires
	// maybe_fire_pending_ring → ft.ring() against the original caller.
	// By the time their phone rings the user is already a live participant
	// so the callee's answer connects cleanly.
	//
	// No facetime:// fallback: that scheme only worked on native iOS/macOS
	// and provided no bridge integration. If the bridge-link arm fails we
	// still post the notice with no callback button; the user can always
	// `!im facetime` in the portal manually.
	noticeMarkdown := "📞 **Missed FaceTime call from " + name + ".**"
	if senderHandle != "" && c.handle != "" {
		if ft, ftErr := c.client.GetFacetimeClient(); ftErr == nil {
			if webLink, _, armErr := armBridgeFaceTimeCall(ft, c.handle, senderHandle, 3600, c.resolveFaceTimeDisplayName(ctx)); armErr == nil {
				noticeMarkdown += "\n\n[**📞 Call back " + name + "**](" + webLink + ")"
				noticeMarkdown += "\n\n⚠️ **Tapping this link will ring " + name + "'s phone.** The ring fires the moment you join — open the link when you're ready to be on camera. Works on iOS, macOS, Android, Windows, and Linux.\n\nRaw URL: " + webLink
			} else {
				log.Warn().Err(armErr).Str("caller", senderHandle).Msg("FaceTimeMissed: bridge-link callback arm failed; notice posted without callback button")
			}
		}
	}

	portalKey := c.makePortalKey(msg.Participants, msg.GroupName, msg.Sender, msg.SenderGuid)
	portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, portalKey)
	sendNotice := func(roomID id.RoomID) error {
		content := format.RenderMarkdown(noticeMarkdown, true, false)
		content.MsgType = event.MsgNotice
		content.Mentions = &event.Mentions{}
		_, sendErr := c.Main.Bridge.Bot.SendMessage(ctx, roomID, event.EventMessage, &event.Content{
			Parsed: &content,
		}, nil)
		return sendErr
	}
	if err == nil && portal != nil && portal.MXID != "" {
		if sendErr := sendNotice(portal.MXID); sendErr == nil {
			log.Info().Str("sender", logSafeHandle(senderHandle)).Str("portal_mxid", string(portal.MXID)).Bool("has_callback", senderHandle != "" && c.handle != "").Msg("FaceTimeMissed: posted missed call notice to portal")
			return
		}
	}
	mgmtRoom, mgmtErr := c.UserLogin.User.GetManagementRoom(ctx)
	if mgmtErr != nil {
		log.Warn().Err(mgmtErr).Msg("FaceTimeMissed: failed to get management room for fallback notice")
		return
	}
	if sendErr := sendNotice(mgmtRoom); sendErr != nil {
		log.Warn().Err(sendErr).Msg("FaceTimeMissed: failed to send management room notice")
		return
	}
	log.Info().Str("sender", logSafeHandle(senderHandle)).Str("management_room", string(mgmtRoom)).Bool("has_callback", senderHandle != "" && c.handle != "").Msg("FaceTimeMissed: posted missed call notice to management room")
}

func (c *IMClient) handleFaceTimeAnsweredElsewhereNotice(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	ctx := context.Background()
	notice := "📞 Incoming FaceTime call was answered on another device."
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName, msg.Sender, msg.SenderGuid)
	portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, portalKey)
	sendNotice := func(roomID id.RoomID) error {
		_, sendErr := c.Main.Bridge.Bot.SendMessage(ctx, roomID, event.EventMessage, &event.Content{
			Parsed: &event.MessageEventContent{
				MsgType:  event.MsgNotice,
				Body:     notice,
				Mentions: &event.Mentions{},
			},
		}, nil)
		return sendErr
	}
	senderHandle := ptrStringOr(msg.Sender, "")
	if err == nil && portal != nil && portal.MXID != "" {
		if sendErr := sendNotice(portal.MXID); sendErr == nil {
			log.Info().Str("sender", logSafeHandle(senderHandle)).Str("portal_mxid", string(portal.MXID)).Msg("FaceTimeAnsweredElsewhere: posted notice to portal")
			return
		}
	}
	mgmtRoom, mgmtErr := c.UserLogin.User.GetManagementRoom(ctx)
	if mgmtErr != nil {
		log.Warn().Err(mgmtErr).Msg("FaceTimeAnsweredElsewhere: failed to get management room for fallback notice")
		return
	}
	if sendErr := sendNotice(mgmtRoom); sendErr != nil {
		log.Warn().Err(sendErr).Msg("FaceTimeAnsweredElsewhere: failed to send management room notice")
		return
	}
	log.Info().Str("sender", logSafeHandle(senderHandle)).Str("management_room", string(mgmtRoom)).Msg("FaceTimeAnsweredElsewhere: posted notice to management room")
}

// makeDeletePortalKey constructs a PortalKey from the delete/recover-specific
// fields in a WrappedMessage. Delete and recover APNs messages populate
// DeleteChatGuid, DeleteChatGroupId, and DeleteChatParticipants instead of
// the regular Participants/GroupName/Sender/SenderGuid fields.
//
// Strategy: look up the portal_id from cloud_chat DB first (most reliable),
// then fall back to parsing the chat GUID or building from participants.
func (c *IMClient) makeDeletePortalKey(log zerolog.Logger, msg rustpushgo.WrappedMessage) networkid.PortalKey {
	ctx := context.Background()

	// Best path: look up portal_id from cloud_chat by chat GUID or group_id.
	// The cloud_chat table knows the correct portal_id for both DMs and groups.
	if c.cloudStore != nil {
		// Try chat GUID first (e.g. "iMessage;-;+1234567890" or "iMessage;+;chat123")
		if msg.DeleteChatGuid != nil && *msg.DeleteChatGuid != "" {
			if portalID, err := c.cloudStore.getChatPortalID(ctx, *msg.DeleteChatGuid); err == nil && portalID != "" {
				log.Debug().Str("portal_id", portalID).Str("chat_guid", *msg.DeleteChatGuid).Msg("Resolved delete portal from cloud_chat by chat GUID")
				return networkid.PortalKey{ID: networkid.PortalID(portalID), Receiver: c.UserLogin.ID}
			}
		}
		// Try group_id (UUID)
		if msg.DeleteChatGroupId != nil && *msg.DeleteChatGroupId != "" {
			if portalID, err := c.cloudStore.getChatPortalID(ctx, *msg.DeleteChatGroupId); err == nil && portalID != "" {
				log.Debug().Str("portal_id", portalID).Str("group_id", *msg.DeleteChatGroupId).Msg("Resolved delete portal from cloud_chat by group ID")
				return networkid.PortalKey{ID: networkid.PortalID(portalID), Receiver: c.UserLogin.ID}
			}
		}
	}

	// Fallback: parse the chat GUID to construct portal ID directly.
	if msg.DeleteChatGuid != nil && *msg.DeleteChatGuid != "" {
		parsed := imessage.ParseIdentifier(*msg.DeleteChatGuid)
		if parsed.LocalID != "" && !parsed.IsGroup {
			portalID := identifierToPortalID(parsed)
			log.Debug().Str("portal_id", string(portalID)).Msg("Resolved delete portal from chat GUID parsing")
			return networkid.PortalKey{ID: portalID, Receiver: c.UserLogin.ID}
		}
	}

	// Fallback: use group_id for groups
	if msg.DeleteChatGroupId != nil && *msg.DeleteChatGroupId != "" && len(msg.DeleteChatParticipants) > 1 {
		gidID := "gid:" + strings.ToLower(*msg.DeleteChatGroupId)
		portalKey := networkid.PortalKey{ID: networkid.PortalID(gidID), Receiver: c.UserLogin.ID}

		c.gidAliasesMu.RLock()
		aliasedID, hasAlias := c.gidAliases[gidID]
		c.gidAliasesMu.RUnlock()
		if hasAlias {
			if c.guidCacheMatchIsStale(aliasedID, msg.DeleteChatParticipants) {
				c.gidAliasesMu.Lock()
				// Compare-before-delete: another handler may have repaired
				// the alias between our RLock read and this write lock.
				if c.gidAliases[gidID] == aliasedID {
					delete(c.gidAliases, gidID)
				}
				c.gidAliasesMu.Unlock()
				log.Warn().
					Str("gid_id", gidID).
					Str("stale_alias", aliasedID).
					Msg("Cleared stale gid alias in handleDeleteChat: participant mismatch")
				// Fall through to direct gid: lookup / resolveExistingGroupByGid
			} else {
				portalKey.ID = networkid.PortalID(aliasedID)
				return portalKey
			}
		}
		if existing, _ := c.Main.Bridge.GetExistingPortalByKey(ctx, portalKey); existing != nil && existing.MXID != "" {
			return portalKey
		}
		if len(msg.DeleteChatParticipants) > 0 {
			resolved := c.resolveExistingGroupByGid(gidID, *msg.DeleteChatGroupId, msg.DeleteChatParticipants)
			return networkid.PortalKey{ID: resolved, Receiver: c.UserLogin.ID}
		}
		return portalKey
	}

	// Last resort: build from participants
	if len(msg.DeleteChatParticipants) == 1 {
		portalID := addIdentifierPrefix(msg.DeleteChatParticipants[0])
		return networkid.PortalKey{ID: networkid.PortalID(portalID), Receiver: c.UserLogin.ID}
	}
	if len(msg.DeleteChatParticipants) > 1 {
		members := make([]string, 0, len(msg.DeleteChatParticipants)+1)
		members = append(members, c.handle)
		for _, p := range msg.DeleteChatParticipants {
			members = append(members, addIdentifierPrefix(p))
		}
		sort.Strings(members)
		return networkid.PortalKey{
			ID:       networkid.PortalID(strings.Join(members, ",")),
			Receiver: c.UserLogin.ID,
		}
	}

	log.Warn().Msg("Delete/recover message has no delete-specific fields, falling back to regular makePortalKey")
	return c.makePortalKey(msg.Participants, msg.GroupName, msg.Sender, msg.SenderGuid)
}

func (c *IMClient) handleMessageDelete(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	deleteType := "MoveToRecycleBin"
	if msg.IsPermanentDelete {
		deleteType = "PermanentDelete"
	}

	log.Info().
		Str("delete_type", deleteType).
		Int("uuid_count", len(msg.DeleteMessageUuids)).
		Msg("Processing per-message delete")

	for _, targetUUID := range msg.DeleteMessageUuids {
		// Scrub-before-portal-resolve: Apple-side delete can arrive while
		// the targeted message is still sitting in pendingPortalMsgs for a
		// not-yet-created portal (CloudKit sync hadn't finished when APNs
		// delivered it), or before any cloud_message ingest has happened
		// at all. If we waited for portal resolution we'd skip the scrub
		// and the message would later bridge to Matrix with the body Apple
		// has already deleted on the user's devices. softDeleteMessageByGUID
		// also stub-inserts a deleted=TRUE row for guids not yet in
		// cloud_message so the future CloudKit upsert preserves the
		// deletion via its ON CONFLICT CASE.
		//
		// Fail-closed: skip the MessageRemove emit on scrub error so we
		// don't strand plaintext post-bridgev2-row-deletion.
		if c.cloudStore != nil {
			// 10s (> the SQLite busy_timeout of 5s) so a one-off lock
			// contention with the first-boot chunked scrubber doesn't cancel
			// the write early and strand this Apple-side delete unscrubbed.
			scrubCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := c.cloudStore.softDeleteMessageByGUID(scrubCtx, targetUUID)
			cancel()
			if err != nil {
				log.Error().Err(err).Str("target_uuid", targetUUID).
					Msg("Skipping MessageRemove emit because cloud_message scrub failed — preserving privacy at the cost of a stale Matrix event")
				continue
			}
		}

		portalKey := c.resolvePortalByTargetMessage(log, targetUUID)
		if portalKey.ID == "" {
			log.Debug().
				Str("target_uuid", targetUUID).
				Msg("Message UUID not found in bridge DB, scrub applied; skipping Matrix MessageRemove emit")
			continue
		}

		log.Info().
			Str("target_uuid", targetUUID).
			Str("portal_id", string(portalKey.ID)).
			Msg("Sending redaction for deleted message")

		c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.MessageRemove{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventMessageRemove,
				PortalKey: portalKey,
				Sender:    c.makeEventSender(msg.Sender),
				Timestamp: time.UnixMilli(int64(msg.TimestampMs)),
			},
			TargetMessage: makeMessageID(targetUUID),
		})
	}
}

func (c *IMClient) handleChatDelete(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	deleteType := "MoveToRecycleBin"
	if msg.IsPermanentDelete {
		deleteType = "PermanentDelete"
	}

	// Per-message delete: DeleteMessageUuids is populated, chat-level fields are empty.
	// Route to handleMessageDelete which does per-message redaction via the bridge DB.
	if len(msg.DeleteMessageUuids) > 0 {
		c.handleMessageDelete(log, msg)
		return
	}

	// chatdb backend: preserve master behavior — ignore Apple-initiated deletes.
	if !c.Main.Config.UseCloudKitBackfill() {
		log.Info().Str("delete_type", deleteType).Msg("Ignoring incoming Apple chat delete (chatdb backend)")
		return
	}

	portalKey := c.makeDeletePortalKey(log, msg)
	portalID := string(portalKey.ID)

	log.Info().
		Str("delete_type", deleteType).
		Str("portal_id", portalID).
		Str("msg_uuid", msg.Uuid).
		Msg("Processing incoming Apple chat delete")

	// Track as recently deleted so APNs echoes don't recreate it.
	c.trackDeletedChat(portalID)

	// Soft-delete local DB records (preserves UUIDs for echo detection).
	// Fail-closed: if soft-delete (which scrubs plaintext) fails, skip the
	// ChatDelete emit. Once bridgev2 wipes its message rows via
	// Message.DeleteInChunks, the scrubber's EXISTS predicate is forever
	// FALSE for those guids — a scrub failure here would leak the entire
	// chat's plaintext until next restart. Each DB op gets its own 5s
	// budget so a slow clearRestoreOverride can't starve the load-bearing
	// deleteLocalChatByPortalID into a spurious DeadlineExceeded.
	if c.cloudStore != nil {
		overrideCtx, overrideCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := c.cloudStore.clearRestoreOverride(overrideCtx, portalID); err != nil {
			log.Warn().Err(err).Str("portal_id", portalID).Msg("Failed to clear restore override for Apple-deleted chat")
		}
		overrideCancel()

		deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := c.cloudStore.deleteLocalChatByPortalID(deleteCtx, portalID)
		deleteCancel()
		if err != nil {
			log.Error().Err(err).Str("portal_id", portalID).
				Msg("Skipping ChatDelete emit because cloud_message scrub failed — preserving privacy at the cost of a stale Beeper portal")
			return
		}
	}

	// Queue bridge portal deletion if it exists.
	existing, _ := c.Main.Bridge.GetExistingPortalByKey(context.Background(), portalKey)
	if existing != nil && existing.MXID != "" {
		log.Info().
			Str("portal_id", portalID).
			Str("delete_type", deleteType).
			Msg("Deleting Beeper portal for Apple-deleted chat")
		c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.ChatDelete{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventChatDelete,
				PortalKey: portalKey,
				Timestamp: time.Now(),
				LogContext: func(lc zerolog.Context) zerolog.Context {
					return lc.Str("source", "apns_chat_delete").Str("delete_type", deleteType)
				},
			},
			OnlyForMe: true,
		})
	} else {
		log.Debug().
			Str("portal_id", portalID).
			Msg("No existing portal for Apple-deleted chat (already gone or never created)")
	}
}

func (c *IMClient) handleChatRecover(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	// chatdb backend: no-op. Recovery is CloudKit-only.
	if !c.Main.Config.UseCloudKitBackfill() {
		log.Debug().Msg("Ignoring RecoverChat (chatdb backend)")
		return
	}

	// Log all available fields from the recover APNs message for diagnostics.
	// Recovery failures are often caused by missing/empty delete-specific fields.
	recoverLog := log.With().
		Str("msg_uuid", msg.Uuid).
		Interface("delete_chat_guid", msg.DeleteChatGuid).
		Interface("delete_chat_group_id", msg.DeleteChatGroupId).
		Int("delete_participants_count", len(msg.DeleteChatParticipants)).
		Strs("delete_participants", msg.DeleteChatParticipants).
		Interface("sender_guid", msg.SenderGuid).
		Logger()

	portalKey := c.makeDeletePortalKey(recoverLog, msg)
	portalID := string(portalKey.ID)

	if portalID == "" {
		recoverLog.Warn().Msg("Chat recovery: could not resolve portal ID from recover message — skipping")
		return
	}

	// For group portals, cross-reference cloud_chat to find the canonical
	// portal_id. The APNs recover message may carry the chat_id UUID
	// (different from the group_id UUID), producing gid:<chat_uuid> while
	// the existing portal uses gid:<group_uuid>. Without this, we'd create
	// a duplicate portal for the same group.
	if strings.HasPrefix(portalID, "gid:") && c.cloudStore != nil {
		uuid := strings.TrimPrefix(portalID, "gid:")
		if altPortalIDs, err := c.cloudStore.findPortalIDsByGroupID(context.Background(), uuid); err == nil {
			for _, altID := range altPortalIDs {
				if altID != portalID {
					recoverLog.Info().
						Str("original_portal_id", portalID).
						Str("canonical_portal_id", altID).
						Msg("Resolved group recovery portal to canonical portal_id via cloud_chat")
					portalID = altID
					portalKey.ID = networkid.PortalID(altID)
					break
				}
			}
		}
	}

	// Always honor RecoverChat from APNs. Two scenarios:
	// 1. Echo of our own !restore-chat → portal already exists → ChatResync is a no-op.
	// 2. User recovered from iPhone/Mac → legitimate recovery → we should recreate the portal.
	// Previously this blocked non-tombstone entries, but that prevented legitimate
	// iPhone recoveries of chats deleted from Beeper.
	recoverLog.Info().
		Str("portal_id", portalID).
		Msg("Processing incoming Apple chat recovery from trash — queuing")

	// Cache participants from the recover message so resolveGroupMembers can find
	// them during ChatResync even before cloud_chat persistence settles.
	var seedParticipants []string
	for _, p := range msg.DeleteChatParticipants {
		if n := normalizeIdentifierForPortalID(p); n != "" {
			seedParticipants = append(seedParticipants, n)
		}
	}
	if strings.HasPrefix(portalID, "gid:") && len(msg.DeleteChatParticipants) > 0 {
		normalized := make([]string, 0, len(msg.DeleteChatParticipants)+1)
		normalized = append(normalized, c.handle)
		for _, p := range msg.DeleteChatParticipants {
			normalized = append(normalized, addIdentifierPrefix(p))
		}
		sort.Strings(normalized)
		c.imGroupParticipantsMu.Lock()
		c.imGroupParticipants[portalID] = normalized
		c.imGroupParticipantsMu.Unlock()
	}

	chatID := ""
	if msg.DeleteChatGuid != nil {
		chatID = *msg.DeleteChatGuid
	}
	groupID := ""
	if msg.DeleteChatGroupId != nil {
		groupID = *msg.DeleteChatGroupId
	}
	if err := c.startRestoreBackfillPipeline(restorePipelineOptions{
		PortalID:     portalID,
		PortalKey:    portalKey,
		Source:       "apns_chat_recover",
		Participants: seedParticipants,
		ChatID:       chatID,
		GroupID:      groupID,
		// APNs recover indicates Apple-side recovery already happened.
		RecoverOnApple: false,
	}); err != nil {
		recoverLog.Warn().Err(err).Str("portal_id", portalID).Msg("Failed to start restore pipeline for recovered chat")
	}
}

func (c *IMClient) startRestoreBackfillPipeline(opts restorePipelineOptions) error {
	portalID := strings.TrimSpace(opts.PortalID)
	if portalID == "" {
		return fmt.Errorf("restore portal ID is empty")
	}
	opts.PortalID = portalID
	if opts.PortalKey.ID == "" {
		opts.PortalKey.ID = networkid.PortalID(portalID)
	}
	if opts.PortalKey.Receiver == "" && c.UserLogin != nil {
		opts.PortalKey.Receiver = c.UserLogin.ID
	}
	if opts.Source == "" {
		opts.Source = "restore_pipeline"
	}

	c.restorePipelinesMu.Lock()
	if c.restorePipelines == nil {
		c.restorePipelines = make(map[string]bool)
	}
	if c.restorePipelines[portalID] {
		c.restorePipelinesMu.Unlock()
		if opts.Notify != nil {
			label := opts.DisplayName
			if label == "" {
				label = portalID
			}
			opts.Notify("Restore for **%s** is already in progress.", label)
		}
		return nil
	}
	c.restorePipelines[portalID] = true
	c.restorePipelinesMu.Unlock()

	go c.runRestoreBackfillPipeline(opts)
	return nil
}

func (c *IMClient) finishRestoreBackfillPipeline(portalID string) {
	c.restorePipelinesMu.Lock()
	delete(c.restorePipelines, portalID)
	c.restorePipelinesMu.Unlock()
}

// activeRestorePortalIDs returns a snapshot of portal IDs with an active
// restore pipeline. The privacy scrubber excludes these so a long backfill
// (large portal, many thousands of messages) can't be re-scrubbed mid-flight,
// which would interact with cloudRowToBackfillMessages' BodyScrubbed skip to
// silently drop the un-backfilled tail.
func (c *IMClient) activeRestorePortalIDs() []string {
	c.restorePipelinesMu.Lock()
	defer c.restorePipelinesMu.Unlock()
	if len(c.restorePipelines) == 0 {
		return nil
	}
	out := make([]string, 0, len(c.restorePipelines))
	for pid := range c.restorePipelines {
		out = append(out, pid)
	}
	return out
}

// runUnbridgedTailScrub clears plaintext from cloud_message rows older than the
// newest MaxInitialMessages of each portal — the history beyond the per-chat
// backfill cap that backward backfill can never deliver (FetchMessages
// short-circuits backward requests when the cap is set). Gated on a configured
// cap: with backfill uncapped (MaxInitialMessages == math.MaxInt32) backward
// backfill is live, so the tail is still reachable and must NOT be scrubbed.
//
// Runs both from post-sync housekeeping (to catch a freshly-synced backlog) and
// on the periodic scrub ticker. The ticker pass is essential, not redundant:
// rows synced just before the post-sync pass are still inside the grace window
// and get skipped there, so without a later periodic pass they would never be
// scrubbed. The per-portal indexed query is cheap and scrubs nothing once a
// portal has converged.
func (c *IMClient) runUnbridgedTailScrub(ctx context.Context, log zerolog.Logger) {
	if c.cloudStore == nil {
		return
	}
	keepN := c.Main.Bridge.Config.Backfill.MaxInitialMessages
	if keepN <= 0 || keepN >= math.MaxInt32 {
		return
	}
	scrubbed, err := c.cloudStore.scrubUnbridgedTail(ctx, keepN, bodyScrubGracePeriod, c.activeRestorePortalIDs())
	if err != nil {
		log.Warn().Err(err).Msg("Unbridged-tail scrub failed")
	} else if scrubbed > 0 {
		log.Info().Int64("scrubbed", scrubbed).Int("keep_per_portal", keepN).
			Msg("Scrubbed plaintext from unreachable backfill tail")
	}
}

func (c *IMClient) notifyRestoreStatus(opts restorePipelineOptions, format string, args ...any) {
	if opts.Notify == nil {
		return
	}
	opts.Notify(format, args...)
}

func restoreRetryDelay(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 15 * time.Second
	case 2:
		return 60 * time.Second
	case 3:
		return 3 * time.Minute
	default:
		return 10 * time.Minute
	}
}

func (c *IMClient) runRestoreBackfillPipeline(opts restorePipelineOptions) {
	defer c.finishRestoreBackfillPipeline(opts.PortalID)

	portalID := opts.PortalID
	portalKey := opts.PortalKey
	ctx := context.Background()

	displayName := strings.TrimSpace(opts.DisplayName)
	if displayName == "" {
		displayName = friendlyPortalName(ctx, c.Main.Bridge, c, portalKey, portalID)
	}
	if displayName == "" {
		displayName = portalID
	}

	log := c.UserLogin.Log.With().
		Str("portal_id", portalID).
		Str("source", opts.Source).
		Logger()

	if !c.Main.Config.UseCloudKitBackfill() || c.cloudStore == nil {
		log.Warn().Msg("Restore pipeline started without CloudKit backfill; queueing plain ChatResync")
		c.refreshRecoveredPortalAfterCloudSync(log, portalKey, opts.Source)
		c.notifyRestoreStatus(opts, "Restore of **%s** queued.", displayName)
		return
	}

	c.notifyRestoreStatus(opts, "Restoring **%s** — syncing iCloud history…", displayName)

	// Stage 1: restore prerequisites (undelete + metadata seeding).
	// Lock only covers DB mutations. Network I/O (metadata refresh,
	// recycle bin recovery, Apple APNs) runs after unlock to avoid
	// starving concurrent restores during slow CloudKit calls.
	c.restoreMu.Lock()
	if err := c.cloudStore.setRestoreOverride(ctx, portalID); err != nil {
		log.Warn().Err(err).Msg("Failed to persist restore override")
	}
	c.recentlyDeletedPortalsMu.Lock()
	delete(c.recentlyDeletedPortals, portalID)
	c.recentlyDeletedPortalsMu.Unlock()
	if len(opts.Participants) > 0 || opts.DisplayName != "" || opts.ChatID != "" || opts.GroupID != "" || opts.GroupPhotoGuid != "" {
		c.cloudStore.seedChatFromRecycleBin(
			ctx,
			portalID,
			opts.ChatID,
			opts.GroupID,
			opts.DisplayName,
			opts.GroupPhotoGuid,
			opts.Participants,
		)
	}
	// Clear the chat-level tombstone (deleted=TRUE) that seedDeletedChatsFromRecycleBin
	// may have set. Without this, the main CloudKit zone message sync's portalHasChat
	// check returns false and silently discards all messages for this portal.
	if err := c.cloudStore.undeleteCloudChatByPortalID(ctx, portalID); err != nil {
		log.Warn().Err(err).Msg("Failed to undelete cloud_chat tombstone for restore")
	} else {
		log.Info().Msg("Cleared cloud_chat tombstone for restore portal")
	}
	undeleted, err := c.cloudStore.undeleteCloudMessagesByPortalID(ctx, portalID)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to undelete cloud_message rows")
	} else {
		log.Info().Int("undeleted", undeleted).Msg("Undeleted cloud_message rows for restore")
	}
	// Clear body_scrubbed so the upsert from the upcoming CloudKit re-fetch
	// can repopulate text/subject/sender — privacy scrubbing will re-apply
	// on the next periodic tick after the restored messages are re-bridged.
	if cleared, err := c.cloudStore.clearBodyScrubByPortalID(ctx, portalID); err != nil {
		log.Warn().Err(err).Msg("Failed to clear body_scrubbed flag for restore")
	} else if cleared > 0 {
		log.Info().Int("cleared", cleared).Msg("Cleared body_scrubbed flag for restore (will re-scrub after re-bridge)")
	}
	needsRecoverMessages := false
	if hasMessages, err := c.cloudStore.hasPortalMessages(ctx, portalID); err != nil {
		log.Warn().Err(err).Msg("Failed to check portal messages during restore")
	} else if !hasMessages {
		needsRecoverMessages = true
	}
	c.restoreMu.Unlock()

	// Network I/O: refresh metadata and recover messages outside the mutex.
	// The restorePipelines map already prevents per-portal concurrency.
	if strings.HasPrefix(portalID, "gid:") || strings.Contains(portalID, ",") {
		c.refreshRecoveredChatMetadata(log, portalID, opts.Participants)
	}
	if needsRecoverMessages {
		c.recoverMessagesFromRecycleBin(log, portalID, opts.Participants)
	}
	if opts.RecoverOnApple {
		c.recoverChatOnApple(portalID)
	}

	// Stage 2: Attempt to import CloudKit message history.
	//
	// Retry up to maxRestoreAttempts times. CloudKit is eventually consistent —
	// after Apple recovers a chat, messages may still be propagating from the
	// recycle bin zone back to the main zone. Each attempt runs the full
	// targeted+unfiltered CloudFetchRecentMessages path.
	//
	// We do NOT do a forced full-zone re-sync here. Clearing global zone tokens
	// causes every message in every portal to be re-ingested through
	// resolveConversationID. When cloud_chat rows for other portals are
	// soft-deleted at that moment, from-me messages can be misrouted to the
	// self-chat portal, corrupting it.

	// Create a context that cancels on shutdown so CloudKit fetches
	// are bounded and interruptible.
	fetchCtx, fetchCancel := context.WithCancel(context.Background())
	defer fetchCancel()
	if c.stopChan != nil {
		stopCh := c.stopChan
		go func() {
			select {
			case <-stopCh:
				fetchCancel()
			case <-fetchCtx.Done():
			}
		}()
	}

	const maxRestoreAttempts = 4
	historyImported := false
	for attempt := range maxRestoreAttempts {
		if attempt > 0 {
			delay := restoreRetryDelay(attempt)
			c.notifyRestoreStatus(opts, "Restore for **%s** is still syncing. Retrying in %s.", displayName, delay.Round(time.Second))
			select {
			case <-time.After(delay):
			case <-fetchCtx.Done():
				log.Info().Int("attempt", attempt).Msg("Restore pipeline stopped during retry wait")
			}
			if fetchCtx.Err() != nil {
				break
			}
		}

		attemptCtx, attemptCancel := context.WithTimeout(fetchCtx, 2*time.Minute)
		imported, diag, importErr := c.fetchRecoveredMessagesFromCloudKit(attemptCtx, log.With().Int("attempt", attempt+1).Logger(), portalID)
		attemptCancel()

		if fetchCtx.Err() != nil {
			break
		}
		if importErr != nil {
			log.Warn().Err(importErr).Int("attempt", attempt+1).Msg("Targeted restore fetch failed")
		} else if imported > 0 {
			log.Info().
				Int("imported", imported).
				Int("attempt", attempt+1).
				Msg("Restore pipeline: imported messages from CloudKit")
			historyImported = true
			break
		} else {
			// Emit a diagnostic notification to help diagnose zero-message fetches.
			// Two cases:
			//   diag.UnfilteredTotal == 0: messages not yet in main CloudKit zone
			//     (likely still propagating from recycle bin after Apple recovery)
			//     → worth retrying later, CloudKit is eventually consistent
			//   diag.UnfilteredTotal > 0 but matched == 0: messages are in the
			//     zone but under a different chatId (e.g. phone number vs email).
			//     SampleChatIDs shows what chatIds ARE present.
			//     → retrying will never help; stop and report to the user
			if diag != nil && diag.UnfilteredTotal > 0 {
				log.Warn().Int("attempt", attempt+1).
					Int("zone_total", diag.UnfilteredTotal).
					Strs("sample_chat_ids", diag.SampleChatIDs).
					Msg("Restore pipeline: messages in zone but none matched portal — chatId format mismatch, stopping retries")
				c.notifyRestoreStatus(opts,
					"Restore of **%s** — scanned %d messages in recovery zone but none matched this chat. "+
						"The chat may be in the main iCloud zone (will appear once sync completes). "+
						"Sample IDs found: %s. Check logs for `Unfiltered scan complete`.",
					displayName, diag.UnfilteredTotal, strings.Join(diag.SampleChatIDs, ", "))
				// A chatId format mismatch won't resolve on its own — break out of
				// the retry loop immediately.
				break
			}
			// diag.UnfilteredTotal == 0 (or diag is nil): messages not yet visible
			// in the main zone — may still be in recycle bin. Worth retrying.
			if diag != nil {
				log.Warn().Int("attempt", attempt+1).
					Msg("Restore pipeline: CloudKit main zone empty (messages may still be in recycle bin)")
				if attempt == 0 {
					c.notifyRestoreStatus(opts, "Restore of **%s** — iCloud history not yet available (messages may still be moving from recycle bin to main zone). Retrying…", displayName)
				}
			} else {
				log.Warn().Int("attempt", attempt+1).Msg("Restore pipeline: fetch returned 0 messages")
			}
		}
	}

	// If shutdown was the reason we exited the retry loop, don't create
	// portals or send messages — just exit cleanly.
	if fetchCtx.Err() != nil {
		log.Info().Msg("Restore pipeline: shutdown detected, skipping portal recreation")
		return
	}

	// Always recreate the portal regardless of whether we found history.
	// The old code always did this — without it, a failed/delayed fetch means
	// the portal never comes back at all. The normal CloudKit sync will
	// backfill history when messages become available in the main zone.
	//
	// When no history was imported, send a bot notice into the restored room
	// so the user sees the chat in Beeper (empty rooms may be hidden) and
	// understands why there's no message history.
	var postCreate func(context.Context, *bridgev2.Portal)
	if !historyImported {
		postCreate = func(ctx context.Context, portal *bridgev2.Portal) {
			if portal == nil || portal.MXID == "" {
				return
			}
			notice := "Chat restored from iCloud. No message history was available — messages may have expired from Apple's recycle bin."
			_, err := c.Main.Bridge.Bot.SendMessage(ctx, portal.MXID, event.EventMessage, &event.Content{
				Parsed: &event.MessageEventContent{
					MsgType: event.MsgNotice,
					Body:    notice,
				},
			}, nil)
			if err != nil {
				log.Warn().Err(err).Str("portal_id", string(portal.ID)).
					Msg("Failed to send empty-restore notice to room")
			}
		}
	}
	c.queueRecoveredPortalResync(log, portalKey, opts.Source, postCreate)
	if historyImported {
		c.notifyRestoreStatus(opts, "Restore of **%s** complete — history backfill is running.", displayName)
	} else {
		log.Warn().Int("attempts", maxRestoreAttempts).Msg("Restore pipeline: no CloudKit history found after all attempts; portal recreated anyway")
		c.notifyRestoreStatus(opts, "Restore of **%s** — chat recreated. Message history may appear once iCloud finishes syncing.", displayName)
	}
}

// refreshRecoveredChatMetadata performs a targeted CloudKit chat scan and
// re-ingests matching chat records for a recovered portal. This is restore-flow
// specific and ensures recovered group portals pick up custom names/photos even
// when the local tombstone row had sparse metadata.
func (c *IMClient) refreshRecoveredChatMetadata(log zerolog.Logger, portalID string, knownParticipants ...[]string) {
	if c.client == nil || c.cloudStore == nil {
		return
	}
	// Guard the CloudKit FFI calls below (upstream cloudkit.rs has
	// reachable panic sites via type assertions). Missing one metadata
	// refresh is strictly safer than crashing the bridge.
	defer func() {
		if r := recover(); r != nil {
			log.Error().Interface("panic", r).Str("portal_id", portalID).
				Msg("refreshRecoveredChatMetadata panicked — skipped")
		}
	}()

	ctx := context.Background()
	targetGroupID := ""
	targetUUID := ""
	if strings.HasPrefix(portalID, "gid:") {
		targetUUID = strings.TrimPrefix(portalID, "gid:")
		targetGroupID = targetUUID
	}
	targetChatID := c.cloudStore.getChatIdentifierByPortalID(ctx, portalID)

	// Search the RECYCLE BIN for metadata, not the main chatManateeZone.
	// Deleted chats are moved to the recycle bin zone — they no longer
	// appear in the main zone. Paging chatManateeZone would never find
	// them, wasting hundreds of CloudKit API calls for nothing.
	recoverableChats, err := c.client.ListRecoverableChats()
	if err != nil {
		log.Warn().Err(err).Str("portal_id", portalID).
			Msg("Failed to list recoverable chats for metadata refresh")
		return
	}

	log.Info().
		Str("portal_id", portalID).
		Str("target_chat_id", targetChatID).
		Str("target_group_id", targetGroupID).
		Int("recycle_bin_count", len(recoverableChats)).
		Msg("refreshRecoveredChatMetadata: searching recycle bin")
	for i, chat := range recoverableChats {
		dn := ""
		if chat.DisplayName != nil {
			dn = *chat.DisplayName
		}
		log.Debug().
			Int("index", i).
			Str("cloud_chat_id", chat.CloudChatId).
			Str("group_id", chat.GroupId).
			Int64("style", chat.Style).
			Str("display_name", dn).
			Int("participants", len(chat.Participants)).
			Msg("refreshRecoveredChatMetadata: recycle bin entry")
	}

	var matched []rustpushgo.WrappedCloudSyncChat
	for _, chat := range recoverableChats {
		sameChatID := targetChatID != "" && normalizeUUID(chat.CloudChatId) == normalizeUUID(targetChatID)
		sameGroupID := targetGroupID != "" && normalizeUUID(chat.GroupId) == normalizeUUID(targetGroupID)
		sameChatUUID := targetUUID != "" && normalizeUUID(chat.CloudChatId) == normalizeUUID(targetUUID)
		if sameChatID || sameGroupID || sameChatUUID {
			matched = append(matched, chat)
		}
	}

	// Fallback: if UUID matching failed (e.g. portal ID is a per-participant
	// encryption UUID, not the real group_id), try matching by participants.
	// Per-participant encryption envelopes each get a unique UUID, so the
	// portal's gid:<uuid> won't match any recycle bin chat record. But the
	// chat record's participants WILL overlap with our known participants.
	if len(matched) == 0 && len(knownParticipants) > 0 && len(knownParticipants[0]) > 0 {
		knownSet := make(map[string]bool)
		for _, p := range knownParticipants[0] {
			norm := strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(p, "tel:"), "mailto:"))
			if norm != "" {
				knownSet[norm] = true
			}
		}
		for _, chat := range recoverableChats {
			if chat.Style != 43 || len(chat.Participants) == 0 {
				continue
			}
			overlapCount := 0
			for _, cp := range chat.Participants {
				norm := strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(normalizeIdentifierForPortalID(cp), "tel:"), "mailto:"))
				if norm != "" && knownSet[norm] {
					overlapCount++
				}
			}
			if overlapCount > 0 {
				matched = append(matched, chat)
			}
		}
		if len(matched) > 0 {
			log.Info().Str("portal_id", portalID).Int("matched", len(matched)).
				Msg("Matched recycle bin chat by participant overlap (per-participant UUID fallback)")
			// Seed directly with our portal ID rather than going through
			// ingestCloudChats, which would resolve to gid:<real-group-id>
			// (not our per-participant UUID portal ID).
			for _, chat := range matched {
				displayName := ""
				if chat.DisplayName != nil {
					displayName = *chat.DisplayName
				}
				var normParts []string
				for _, p := range chat.Participants {
					if n := normalizeIdentifierForPortalID(p); n != "" {
						normParts = append(normParts, n)
					}
				}
				photoGuid := ""
				if chat.GroupPhotoGuid != nil {
					photoGuid = *chat.GroupPhotoGuid
				}
				c.cloudStore.seedChatFromRecycleBin(ctx, portalID, chat.CloudChatId, chat.GroupId, displayName, photoGuid, normParts)
			}
			log.Info().Str("portal_id", portalID).Int("matched", len(matched)).
				Msg("Seeded chat metadata from recycle bin (participant fallback)")
			return
		}
	}

	if len(matched) == 0 {
		log.Debug().Str("portal_id", portalID).
			Msg("No match in recycle bin — scanning main CloudKit chat zone for group metadata")
		// Recycle bin is empty (Apple already recovered the chat back to the
		// main zone). Scan CloudSyncChats to find the group record with the
		// custom name and photo. Limit to 30 pages to avoid excessive API use;
		// recently-recovered chats should appear near the top of the feed.
		knownSet := make(map[string]bool)
		for _, slice := range knownParticipants {
			for _, p := range slice {
				norm := strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(p, "tel:"), "mailto:"))
				if norm != "" {
					knownSet[norm] = true
				}
			}
		}
		// Also build from cloud_chat participants if knownParticipants is empty.
		if len(knownSet) == 0 {
			dbParts, _ := c.cloudStore.getChatParticipantsByPortalID(ctx, portalID)
			for _, p := range dbParts {
				norm := strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(p, "tel:"), "mailto:"))
				if norm != "" {
					knownSet[norm] = true
				}
			}
		}
		if len(knownSet) > 0 {
			var token *string
			const maxChatScanPages = 30
			for page := 0; page < maxChatScanPages; page++ {
				chatsPage, pageErr := safeCloudSyncChats(c.client, token)
				if pageErr != nil {
					log.Warn().Err(pageErr).Int("page", page).
						Msg("CloudSyncChats page failed during main-zone name scan")
					break
				}
				for _, chat := range chatsPage.Chats {
					if chat.Style != 43 || len(chat.Participants) == 0 {
						continue
					}
					overlapCount := 0
					for _, cp := range chat.Participants {
						norm := strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(normalizeIdentifierForPortalID(cp), "tel:"), "mailto:"))
						if norm != "" && knownSet[norm] {
							overlapCount++
						}
					}
					if overlapCount > 0 {
						matched = append(matched, chat)
					}
				}
				if len(matched) > 0 {
					log.Info().Str("portal_id", portalID).Int("page", page).Int("matched", len(matched)).
						Msg("Found group chat in main CloudKit zone (participant match)")
					break
				}
				if chatsPage.ContinuationToken == nil {
					break
				}
				token = chatsPage.ContinuationToken
			}
		}
		if len(matched) == 0 {
			log.Debug().Str("portal_id", portalID).
				Msg("No matching chat metadata found in recycle bin or main zone")
			return
		}
		// Seed from main zone match using same logic as recycle bin path.
		for _, chat := range matched {
			displayName := ""
			if chat.DisplayName != nil {
				displayName = *chat.DisplayName
			}
			var normParts []string
			for _, p := range chat.Participants {
				if n := normalizeIdentifierForPortalID(p); n != "" {
					normParts = append(normParts, n)
				}
			}
			photoGuid := ""
			if chat.GroupPhotoGuid != nil {
				photoGuid = *chat.GroupPhotoGuid
			}
			c.cloudStore.seedChatFromRecycleBin(ctx, portalID, chat.CloudChatId, chat.GroupId, displayName, photoGuid, normParts)
		}
		log.Info().Str("portal_id", portalID).Int("matched", len(matched)).
			Msg("Seeded group chat metadata from main CloudKit zone")
		return
	}

	// Seed matched records directly with our portal ID rather than using
	// ingestCloudChats. ingestCloudChats calls resolvePortalIDForCloudChat
	// which can resolve to a DIFFERENT portal ID (e.g. gid:<real-group-id>
	// instead of our gid:<per-participant-uuid>), causing messages to be
	// routed to wrong portals (including the self-chat).
	for _, chat := range matched {
		displayName := ""
		if chat.DisplayName != nil {
			displayName = *chat.DisplayName
		}
		var normParts []string
		for _, p := range chat.Participants {
			if n := normalizeIdentifierForPortalID(p); n != "" {
				normParts = append(normParts, n)
			}
		}
		photoGuid := ""
		if chat.GroupPhotoGuid != nil {
			photoGuid = *chat.GroupPhotoGuid
		}
		c.cloudStore.seedChatFromRecycleBin(ctx, portalID, chat.CloudChatId, chat.GroupId, displayName, photoGuid, normParts)
	}

	log.Info().Str("portal_id", portalID).Int("matched", len(matched)).
		Msg("Seeded recovered chat metadata from recycle bin (UUID match)")
}

// recoverMessagesFromRecycleBin attempts to recover messages for a portal from
// Apple's recycle bin zone. When a chat is deleted, its messages are moved to
// recoverableMessageDeleteZone — NOT the main messageManateeZone. This function
// uses ListRecoverableMessageGuids to find those messages and cross-references
// them with the local cloud_message table to undelete matching rows.
//
// This replaces the old approach of paging ALL of messageManateeZone (which was
// both extremely expensive and incorrect — deleted messages aren't there).
func (c *IMClient) recoverMessagesFromRecycleBin(log zerolog.Logger, portalID string, knownParticipants ...[]string) {
	if c.client == nil || c.cloudStore == nil {
		return
	}
	// CloudKit FFI path — guard against upstream panics (cloudkit.rs type
	// assertions). Dropping one restore pass is safer than crashing.
	defer func() {
		if r := recover(); r != nil {
			log.Error().Interface("panic", r).Str("portal_id", portalID).
				Msg("recoverMessagesFromRecycleBin panicked — skipped")
		}
	}()

	ctx := context.Background()

	// Get recoverable message GUIDs from the recycle bin zone.
	entries, err := c.client.ListRecoverableMessageGuids()
	if err != nil {
		log.Warn().Err(err).Str("portal_id", portalID).
			Msg("Failed to list recoverable message GUIDs for restore")
		return
	}

	if len(entries) == 0 {
		log.Debug().Str("portal_id", portalID).Msg("No recoverable messages found in recycle bin")
		return
	}

	// Extract just the GUIDs from entries (format: "guid|metadata...").
	var allGUIDs []string
	for _, entry := range entries {
		guid := recoverableGUIDFromEntry(entry)
		if guid != "" {
			allGUIDs = append(allGUIDs, guid)
		}
	}

	if len(allGUIDs) == 0 {
		return
	}

	// Undelete cloud_message rows for this portal whose GUIDs appear in the
	// recycle bin. The rows were soft-deleted during CloudKit sync when their
	// parent chat was deleted.
	nowMS := time.Now().UnixMilli()
	restored := 0
	const chunkSize = 500
	for i := 0; i < len(allGUIDs); i += chunkSize {
		end := i + chunkSize
		if end > len(allGUIDs) {
			end = len(allGUIDs)
		}
		chunk := allGUIDs[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+3)
		args = append(args, c.cloudStore.loginID, portalID, nowMS)
		for j, guid := range chunk {
			placeholders[j] = fmt.Sprintf("$%d", j+4)
			args = append(args, guid)
		}
		query := fmt.Sprintf(
			`UPDATE cloud_message SET deleted=FALSE, updated_ts=$3 WHERE login_id=$1 AND portal_id=$2 AND guid IN (%s) AND deleted=TRUE`,
			strings.Join(placeholders, ","),
		)
		result, execErr := c.cloudStore.db.Exec(ctx, query, args...)
		if execErr != nil {
			log.Warn().Err(execErr).Msg("Failed to undelete recovered messages from recycle bin")
			continue
		}
		n, _ := result.RowsAffected()
		restored += int(n)
	}

	log.Info().Str("portal_id", portalID).Int("recycle_bin_guids", len(allGUIDs)).
		Int("restored", restored).Msg("Recovered messages from recycle bin")

	// If no existing rows were undeleted (cloud_message is empty), seed
	// placeholder rows from the recycle bin metadata. This makes
	// hasPortalMessages() return true so GetChatInfo.CanBackfill enables
	// backfill. The placeholder rows carry enough metadata (guid, sender,
	// timestamp, cloud_chat_id) for the bridge to page CloudKit messages.
	if restored == 0 {
		// Build a set of acceptable senders from known participants.
		// This prevents mixing messages from other deleted groups when
		// accepting per-participant encryption UUID gid: entries.
		knownSenders := make(map[string]bool)
		if len(knownParticipants) > 0 {
			for _, p := range knownParticipants[0] {
				norm := strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(p, "tel:"), "mailto:"))
				if norm != "" {
					knownSenders[norm] = true
				}
			}
		}
		isGidPortal := strings.HasPrefix(portalID, "gid:")

		seeded := 0
		for _, entry := range entries {
			metadata, ok := parseRecoverableMessageMetadata(entry)
			if !ok {
				continue
			}
			guid := recoverableGUIDFromEntry(entry)
			if guid == "" {
				continue
			}
			resolvedPortal := c.resolveConversationID(ctx, metadata.wrappedMessage(guid))
			if resolvedPortal == portalID {
				// Exact match — always accept
			} else if isGidPortal && strings.HasPrefix(resolvedPortal, "gid:") {
				// Per-participant encryption UUID. Accept if sender is a
				// known participant, or if we have no participant data.
				if len(knownSenders) > 0 {
					senderNorm := strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(
						normalizeIdentifierForPortalID(metadata.Sender), "tel:"), "mailto:"))
					if !knownSenders[senderNorm] && !metadata.IsFromMe {
						continue
					}
				}
				// Accept: same group, different per-participant UUID
			} else {
				continue
			}

			_, insertErr := c.cloudStore.db.Exec(ctx, `
				INSERT INTO cloud_message (login_id, guid, chat_id, portal_id, timestamp_ms, sender, is_from_me, service, deleted, created_ts, updated_ts, record_name, has_body)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, FALSE, $9, $9, $10, TRUE)
				ON CONFLICT (login_id, guid) DO UPDATE SET deleted=FALSE, portal_id=$4
			`, c.cloudStore.loginID, guid, metadata.CloudChatID, portalID, metadata.TimestampMS, metadata.Sender, metadata.IsFromMe, metadata.Service, nowMS, metadata.RecordName)
			if insertErr != nil {
				log.Debug().Err(insertErr).Str("guid", guid).Msg("Failed to seed cloud_message from recycle bin")
				continue
			}
			seeded++
		}
		if seeded > 0 {
			log.Info().Str("portal_id", portalID).Int("seeded", seeded).
				Msg("Seeded cloud_message from recycle bin metadata (cloud_message was empty)")
		}
	}
}

// fetchAndResyncRecoveredChat fetches messages from CloudKit for a recovered
// chat, imports them into the local cache, then queues ChatResync.
func (c *IMClient) fetchAndResyncRecoveredChat(log zerolog.Logger, portalKey networkid.PortalKey, portalID string) {
	imported, _, err := c.fetchRecoveredMessagesFromCloudKit(context.Background(), log, portalID)
	if err != nil {
		log.Warn().Err(err).Str("portal_id", portalID).Msg("Failed to import CloudKit messages for recovered chat")
	} else {
		log.Info().Int("imported", imported).Str("portal_id", portalID).
			Msg("Imported CloudKit messages for recovered chat")
	}
	c.refreshRecoveredPortalAfterCloudSync(log, portalKey, "apns_chat_recover_fetched")
}

// restoreFetchDiagnostic carries diagnostic info from fetchRecoveredMessagesFromCloudKit
// for user-visible status messages when the fetch returns 0 messages.
type restoreFetchDiagnostic struct {
	// UnfilteredTotal is the total number of non-deleted messages found in
	// CloudFetchRecentMessages with no chatId filter. If 0, messages are not
	// yet in the main CloudKit zone (likely still in the recycle bin zone
	// waiting for Apple-side propagation after chat recovery).
	UnfilteredTotal int
	// SampleChatIDs holds up to 20 chatId values from the unfiltered scan
	// that did NOT match the target portal — helps diagnose chatId format
	// mismatches (e.g. messages stored under a phone number instead of email).
	SampleChatIDs []string
}

// safeCloudFetchRecent wraps CloudFetchRecentMessages in a panic recovery.
// Rust CloudKit parsing panics (e.g. "Operation UUID has no response?") propagate
// through FFI as Go panics. Recovering here prevents a single bad CloudKit
// response from crashing the whole bridge.
func (c *IMClient) safeCloudFetchRecent(log zerolog.Logger, chatID *string, maxPages, maxMessages uint32) (msgs []rustpushgo.WrappedCloudSyncMessage, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("CloudFetchRecentMessages panicked: %v", r)
			log.Error().Err(err).Msg("Caught Rust panic in CloudFetchRecentMessages")
		}
	}()
	return c.client.CloudFetchRecentMessages(0, chatID, maxPages, maxMessages)
}

// fetchRecoveredMessagesFromCloudKit runs the targeted restore fetch path and
// imports matched rows into cloud_message for the given portal.
// Returns (importedCount, diagnostic, error). diagnostic is non-nil when the
// unfiltered fallback ran (all targeted fetches returned 0).
func (c *IMClient) fetchRecoveredMessagesFromCloudKit(ctx context.Context, log zerolog.Logger, portalID string) (int, *restoreFetchDiagnostic, error) {
	if c.cloudStore == nil {
		return 0, nil, fmt.Errorf("cloud store not initialized")
	}
	if c.client == nil {
		return 0, nil, fmt.Errorf("rustpush client not initialized")
	}

	// Resolve the CloudKit chat_id for this portal.
	cloudChatID := c.cloudStore.getChatIdentifierByPortalID(ctx, portalID)
	// Also try the stored group_id as a fallback chatId — it may differ from
	// the per-participant UUID in the portal_id (see per-participant UUID lore).
	groupIDForFetch := ""
	if strings.HasPrefix(portalID, "gid:") {
		groupIDForFetch = c.cloudStore.getGroupIDForPortalID(ctx, portalID)
	}
	if cloudChatID == "" {
		if strings.HasPrefix(portalID, "gid:") {
			uuid := strings.TrimPrefix(portalID, "gid:")
			cloudChatID = "any;+;" + uuid
		} else {
			identifier := strings.TrimPrefix(strings.TrimPrefix(portalID, "mailto:"), "tel:")
			cloudChatID = "iMessage;-;" + identifier
		}
	}

	log.Info().
		Str("portal_id", portalID).
		Str("cloud_chat_id", cloudChatID).
		Msg("Fetching messages from CloudKit for recovered chat with empty local cache")

	// Build the set of portal IDs that should be considered "this portal".
	acceptableIDs := map[string]bool{portalID: true}
	if !strings.Contains(portalID, ",") && !strings.HasPrefix(portalID, "gid:") {
		contactResolved := string(c.resolveContactPortalID(portalID))
		acceptableIDs[contactResolved] = true
		contact := c.lookupContact(portalID)
		if contact != nil {
			for _, altID := range contactPortalIDs(contact) {
				acceptableIDs[altID] = true
			}
		}
	}

	rawIdentifier := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(portalID, "tel:"), "mailto:"), "gid:")
	acceptableChatIDSuffixes := map[string]bool{
		strings.ToLower(rawIdentifier): true,
	}
	if !strings.Contains(portalID, ",") && !strings.HasPrefix(portalID, "gid:") {
		for id := range acceptableIDs {
			raw := strings.TrimPrefix(strings.TrimPrefix(id, "tel:"), "mailto:")
			acceptableChatIDSuffixes[strings.ToLower(raw)] = true
		}
	}
	if strings.HasPrefix(portalID, "gid:") {
		portalUUID := strings.TrimPrefix(portalID, "gid:")
		acceptableChatIDSuffixes[normalizeUUID(portalUUID)] = true
		acceptableChatIDSuffixes[strings.ToLower(portalUUID)] = true
		if chatID := c.cloudStore.getChatIdentifierByPortalID(ctx, portalID); chatID != "" {
			acceptableChatIDSuffixes[normalizeUUID(chatID)] = true
			acceptableChatIDSuffixes[strings.ToLower(chatID)] = true
		}
		if gid := c.cloudStore.getGroupIDForPortalID(ctx, portalID); gid != "" {
			acceptableChatIDSuffixes[normalizeUUID(gid)] = true
			acceptableChatIDSuffixes[strings.ToLower(gid)] = true
		}
	}

	var matched []rustpushgo.WrappedCloudSyncMessage
	tryTargetedFetch := func(chatID string) {
		if chatID == "" || ctx.Err() != nil {
			return
		}
		chatIDCopy := chatID
		targeted, fetchErr := c.safeCloudFetchRecent(log, &chatIDCopy, 50, 5000)
		if fetchErr != nil {
			log.Warn().Err(fetchErr).Str("cloud_chat_id", chatID).
				Msg("CloudFetchRecentMessages failed for recovered chat")
			return
		}
		log.Info().Int("count", len(targeted)).Str("portal_id", portalID).
			Str("cloud_chat_id", chatID).
			Msg("Targeted CloudKit fetch for recovered chat")
		for _, msg := range targeted {
			if !msg.Deleted {
				matched = append(matched, msg)
			}
		}
	}
	tryTargetedFetch(cloudChatID)
	if len(matched) == 0 && !strings.HasPrefix(portalID, "gid:") {
		suffix := ""
		if strings.HasPrefix(cloudChatID, "any;-;") {
			suffix = strings.TrimPrefix(cloudChatID, "any;-;")
			tryTargetedFetch("iMessage;-;" + suffix)
		} else if strings.HasPrefix(cloudChatID, "iMessage;-;") {
			suffix = strings.TrimPrefix(cloudChatID, "iMessage;-;")
			tryTargetedFetch("any;-;" + suffix)
		}
		if len(matched) == 0 && suffix != "" {
			tryTargetedFetch("SMS;-;" + suffix)
		}
	}
	if len(matched) == 0 && !strings.Contains(cloudChatID, ";") && cloudChatID != "" {
		if strings.HasPrefix(portalID, "gid:") {
			tryTargetedFetch("any;+;" + cloudChatID)
		} else {
			tryTargetedFetch("iMessage;-;" + cloudChatID)
			if len(matched) == 0 {
				tryTargetedFetch("any;-;" + cloudChatID)
			}
			if len(matched) == 0 {
				tryTargetedFetch("SMS;-;" + cloudChatID)
			}
		}
	}
	if len(matched) == 0 && groupIDForFetch != "" && groupIDForFetch != strings.TrimPrefix(cloudChatID, "any;+;") {
		tryTargetedFetch("any;+;" + groupIDForFetch)
		if len(matched) == 0 {
			tryTargetedFetch("iMessage;+;" + groupIDForFetch)
		}
	}

	var diag *restoreFetchDiagnostic
	if len(matched) == 0 && ctx.Err() != nil {
		return 0, nil, ctx.Err()
	}
	if len(matched) == 0 {
		log.Info().Str("portal_id", portalID).
			Msg("All targeted fetches returned 0 — falling back to unfiltered CloudFetchRecentMessages")
		// Use the same page/message limits as the targeted fetch (50 pages, 5000 msgs).
		// Larger limits (e.g. 500 pages) caused CloudKit to return malformed responses
		// that panicked the Rust code ("Operation UUID has no response?"), crashing the
		// bridge. 50 pages × ~200 msgs/page = ~10k messages — enough for diagnostics.
		const maxRecoveryPages = 50
		unfiltered, scanErr := c.safeCloudFetchRecent(log, nil, maxRecoveryPages, 10000)
		if scanErr != nil {
			return 0, nil, fmt.Errorf("unfiltered CloudFetchRecentMessages failed: %w", scanErr)
		}
		seenChatIDs := make(map[string]bool)
		for _, msg := range unfiltered {
			if msg.Deleted {
				continue
			}
			resolved := c.resolveConversationID(ctx, msg)
			if acceptableIDs[resolved] {
				matched = append(matched, msg)
				continue
			}
			if msg.CloudChatId == "" {
				continue
			}
			suffix := strings.ToLower(msg.CloudChatId)
			if parts := strings.SplitN(suffix, ";-;", 2); len(parts) == 2 {
				suffix = parts[1]
			} else if parts := strings.SplitN(suffix, ";+;", 2); len(parts) == 2 {
				suffix = parts[1]
			}
			if acceptableChatIDSuffixes[suffix] || acceptableChatIDSuffixes[normalizeUUID(suffix)] {
				matched = append(matched, msg)
			} else if !seenChatIDs[msg.CloudChatId] && len(seenChatIDs) < 20 {
				seenChatIDs[msg.CloudChatId] = true
			}
		}
		sampleIDs := make([]string, 0, len(seenChatIDs))
		for k := range seenChatIDs {
			sampleIDs = append(sampleIDs, k)
		}
		sort.Strings(sampleIDs)
		log.Info().Int("total", len(unfiltered)).Int("matched", len(matched)).
			Str("portal_id", portalID).
			Strs("sample_chat_ids", sampleIDs).
			Msg("Unfiltered scan complete")
		diag = &restoreFetchDiagnostic{
			UnfilteredTotal: len(unfiltered),
			SampleChatIDs:   sampleIDs,
		}
	}

	if len(matched) == 0 {
		return 0, diag, nil
	}

	rows := make([]cloudMessageRow, 0, len(matched))
	for _, msg := range matched {
		if msg.Guid == "" || msg.Deleted {
			continue
		}
		text := ""
		if msg.Text != nil {
			text = *msg.Text
		}
		subject := ""
		if msg.Subject != nil {
			subject = *msg.Subject
		}
		timestampMS := msg.TimestampMs
		if timestampMS <= 0 {
			timestampMS = time.Now().UnixMilli()
		}
		tapbackTargetGUID := ""
		if msg.TapbackTargetGuid != nil {
			tapbackTargetGUID = *msg.TapbackTargetGuid
		}
		tapbackEmoji := ""
		if msg.TapbackEmoji != nil {
			tapbackEmoji = *msg.TapbackEmoji
		}
		rows = append(rows, cloudMessageRow{
			GUID:              msg.Guid,
			RecordName:        msg.RecordName,
			CloudChatID:       msg.CloudChatId,
			PortalID:          portalID,
			TimestampMS:       timestampMS,
			Sender:            msg.Sender,
			IsFromMe:          msg.IsFromMe,
			Text:              text,
			Subject:           subject,
			Service:           msg.Service,
			Deleted:           false,
			TapbackType:       msg.TapbackType,
			TapbackTargetGUID: tapbackTargetGUID,
			TapbackEmoji:      tapbackEmoji,
			DateReadMS:        msg.DateReadMs,
			HasBody:           msg.HasBody,
		})
	}
	if len(rows) == 0 {
		return 0, diag, nil
	}
	if err := c.cloudStore.upsertMessageBatch(ctx, rows); err != nil {
		return 0, diag, fmt.Errorf("failed to import CloudKit messages for recovered chat: %w", err)
	}
	return len(rows), diag, nil
}

func (c *IMClient) queueRecoveredPortalResync(log zerolog.Logger, portalKey networkid.PortalKey, source string, postCreate func(context.Context, *bridgev2.Portal)) {
	portalID := string(portalKey.ID)

	// Use the newest BACKFILLABLE content timestamp. Placeholder rows from
	// recycle-bin seeding can have GUID/timestamp metadata but no message body;
	// treating those as "latest message" causes false-success resyncs that
	// still backfill zero messages.
	var latestMessageTS time.Time
	if c.cloudStore != nil {
		if newestTS, err := c.cloudStore.getNewestBackfillableMessageTimestamp(context.Background(), portalID, true); err == nil && newestTS > 0 {
			latestMessageTS = time.UnixMilli(newestTS)
		}
	}

	log.Info().
		Str("portal_id", portalID).
		Str("source", source).
		Time("latest_message_ts", latestMessageTS).
		Msg("Queueing ChatResync for recovered chat")

	// Always CreatePortal=true for recovery. The portal room may have been
	// deleted (com.beeper.delete_chat or ChatDelete) but the portal DB row
	// can linger with a stale MXID. Without CreatePortal=true, bridgev2
	// treats the resync as a metadata update and skips forward backfill.
	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.ChatResync{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventChatResync,
			PortalKey:    portalKey,
			CreatePortal: true,
			Timestamp:    time.Now(),
			LogContext: func(lc zerolog.Context) zerolog.Context {
				return lc.Str("source", source)
			},
			PostHandleFunc: postCreate,
		},
		GetChatInfoFunc: c.GetChatInfo,
		LatestMessageTS: latestMessageTS,
	})
}

func (c *IMClient) refreshRecoveredPortalAfterCloudSync(log zerolog.Logger, portalKey networkid.PortalKey, source string) {
	c.queueRecoveredPortalResync(log, portalKey, source, nil)
}

func (c *IMClient) handleReadReceipt(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	if msg.IsStoredMessage {
		log.Debug().Str("uuid", msg.Uuid).Msg("Skipping stored read receipt")
		return
	}
	// Drop read receipts while CloudKit sync is in progress or the APNs buffer
	// hasn't been flushed yet (forward backfills still pending, or the 10-second
	// safety net hasn't fired). Once flushedAt is stamped, backfill is done and
	// any arriving receipt is a genuine live event.
	flushedAt := atomic.LoadInt64(&c.apnsBufferFlushedAt)
	syncDone := c.isCloudSyncDone()
	if !syncDone || flushedAt == 0 {
		log.Debug().
			Str("uuid", msg.Uuid).
			Bool("sync_done", syncDone).
			Int64("flushed_at", flushedAt).
			Msg("Skipping read receipt during CloudKit sync/backfill")
		return
	}

	// Suppress duplicate receipts in the post-backfill grace window (60s).
	// After the APNs buffer flushes, stale receipts that arrived between the
	// 30s IsStoredMessage window and backfill completion can leak through.
	// If CloudKit already has a valid date_read_ms for this message, a
	// synthetic receipt with the correct historical timestamp was already
	// queued — drop the duplicate to prevent overwriting with time.Now().
	const postBackfillGraceMS = 60_000
	if c.cloudStore != nil && (time.Now().UnixMilli()-flushedAt) < postBackfillGraceMS {
		if hasReceipt, err := c.cloudStore.hasCloudReadReceipt(context.Background(), msg.Uuid); err == nil && hasReceipt {
			log.Debug().
				Str("uuid", msg.Uuid).
				Msg("Skipping duplicate read receipt — CloudKit already has date_read_ms (post-backfill grace)")
			return
		}
	}

	portalKey := c.makeReceiptPortalKey(msg.Participants, msg.GroupName, msg.Sender, msg.SenderGuid)
	ctx := context.Background()

	// Try sender_guid lookup — first check gid: portal ID, then cache
	if msg.SenderGuid != nil && *msg.SenderGuid != "" {
		gidPortalKey := networkid.PortalKey{ID: networkid.PortalID("gid:" + *msg.SenderGuid), Receiver: c.UserLogin.ID}
		if gidPortal, _ := c.Main.Bridge.GetExistingPortalByKey(ctx, gidPortalKey); gidPortal != nil && gidPortal.MXID != "" {
			portalKey = gidPortalKey
			log.Debug().
				Str("sender_guid", *msg.SenderGuid).
				Str("resolved_portal", string(portalKey.ID)).
				Msg("Resolved read receipt portal via gid: lookup")
			goto resolved
		}
		c.imGroupGuidsMu.RLock()
		for portalIDStr, guid := range c.imGroupGuids {
			if strings.EqualFold(guid, *msg.SenderGuid) {
				if c.guidCacheMatchIsStale(portalIDStr, msg.Participants) {
					continue
				}
				portalKey = networkid.PortalKey{ID: networkid.PortalID(portalIDStr), Receiver: c.UserLogin.ID}
				c.imGroupGuidsMu.RUnlock()
				log.Debug().
					Str("sender_guid", *msg.SenderGuid).
					Str("resolved_portal", string(portalKey.ID)).
					Msg("Resolved read receipt portal via sender_guid cache lookup")
				goto resolved
			}
		}
		c.imGroupGuidsMu.RUnlock()
	}

	// Fall back to group member tracking
	if msg.Sender != nil {
		if groupKey, ok := c.findGroupPortalForMember(*msg.Sender); ok {
			portalKey = groupKey
			log.Debug().
				Str("sender", *msg.Sender).
				Str("resolved_portal", string(portalKey.ID)).
				Msg("Resolved read receipt portal via group member lookup")
			goto resolved
		}
	}

	// Last resort: use the initial portal key if it resolves to a valid portal.
	{
		portal, _ := c.Main.Bridge.GetExistingPortalByKey(ctx, portalKey)
		if portal != nil && portal.MXID != "" {
			goto resolved
		}
	}
resolved:
	if msg.Uuid != "" {
		if msgPortal := c.resolvePortalByTargetMessage(log, msg.Uuid); msgPortal.ID != "" &&
			(msgPortal.ID != portalKey.ID || msgPortal.Receiver != portalKey.Receiver) {
			log.Debug().
				Str("uuid", msg.Uuid).
				Str("old_portal", string(portalKey.ID)).
				Str("resolved_portal", string(msgPortal.ID)).
				Msg("Resolved read receipt portal via target message UUID")
			portalKey = msgPortal
		}
	}

	readTime := time.UnixMilli(int64(msg.TimestampMs))
	sender := c.makeEventSender(msg.Sender)
	sender = c.canonicalizeDMSender(portalKey, sender)

	if !sender.IsFromMe {
		// Skip ghost receipts for messages that were backfilled from CloudKit.
		// APNs read receipts for group chats lack participants/senderGuid, so
		// the portal key often resolves to a DM rather than a gid: portal.
		// Check cloud_message regardless of portal type — any message that was
		// synced from CloudKit is a backfilled message whose ghost receipt
		// would incorrectly show "Seen by [person]" on outgoing messages.
		// Real-time messages (not in cloud_message) are unaffected.
		if c.cloudStore != nil {
			if backfilled, err := c.cloudStore.isCloudBackfilledMessage(context.Background(), msg.Uuid); err == nil && backfilled {
				log.Debug().
					Str("uuid", msg.Uuid).
					Str("portal_id", string(portalKey.ID)).
					Msg("Skipping ghost read receipt for backfilled message")
				return
			}
		}
		// Ghost receipt ("they read my message"): bypass the framework and call
		// SetBeeperInboxState directly. The standard framework path (QueueRemoteEvent
		// → MarkRead → SetReadMarkers) ignores BeeperReadExtra["ts"] for ghost
		// users, causing the homeserver to use server time instead of the actual
		// read time from the APNs receipt.
		c.sendGhostReadReceipt(&log, sender.Sender, portalKey, msg.Uuid, readTime)
	} else {
		// Self-receipt (unlikely for APNs read receipts, but handle gracefully).
		c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Receipt{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventReadReceipt,
				PortalKey: portalKey,
				Sender:    sender,
				Timestamp: readTime,
			},
			LastTarget: makeMessageID(msg.Uuid),
		})
	}
}

func (c *IMClient) handleDeliveryReceipt(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	// Don't gate on IsStoredMessage. Delivery receipts arrive within seconds
	// of a send — almost always inside the 30s post-reconnect window — and
	// SendMessageStatus is idempotent, so dropping them only erases the
	// "delivered" tick from genuinely live messages. Read receipts naturally
	// land later (after the user actually reads), which is why the same gate
	// on handleReadReceipt didn't have the same visible regression.
	ctx := context.Background()

	// Mirror handleReadReceipt's portal-resolution chain. Without these
	// fallbacks, any drift in makeReceiptPortalKey output (e.g. sender_guid
	// format churn) silently drops every delivery receipt while read receipts
	// keep working via their fallback chain — which manifests as "Beeper
	// shows read but never delivered" with zero log signal.
	portalKey := c.makeReceiptPortalKey(msg.Participants, msg.GroupName, msg.Sender, msg.SenderGuid)
	resolved := false

	if msg.SenderGuid != nil && *msg.SenderGuid != "" {
		gidPortalKey := networkid.PortalKey{ID: networkid.PortalID("gid:" + *msg.SenderGuid), Receiver: c.UserLogin.ID}
		if gidPortal, _ := c.Main.Bridge.GetExistingPortalByKey(ctx, gidPortalKey); gidPortal != nil && gidPortal.MXID != "" {
			portalKey = gidPortalKey
			log.Debug().
				Str("sender_guid", *msg.SenderGuid).
				Str("resolved_portal", string(portalKey.ID)).
				Msg("Resolved delivery receipt portal via gid: lookup")
			resolved = true
		}
		if !resolved {
			c.imGroupGuidsMu.RLock()
			for portalIDStr, guid := range c.imGroupGuids {
				if strings.EqualFold(guid, *msg.SenderGuid) {
					if c.guidCacheMatchIsStale(portalIDStr, msg.Participants) {
						continue
					}
					portalKey = networkid.PortalKey{ID: networkid.PortalID(portalIDStr), Receiver: c.UserLogin.ID}
					log.Debug().
						Str("sender_guid", *msg.SenderGuid).
						Str("resolved_portal", string(portalKey.ID)).
						Msg("Resolved delivery receipt portal via sender_guid cache lookup")
					resolved = true
					break
				}
			}
			c.imGroupGuidsMu.RUnlock()
		}
	}

	if !resolved && msg.Sender != nil {
		if groupKey, ok := c.findGroupPortalForMember(*msg.Sender); ok {
			portalKey = groupKey
			log.Debug().
				Str("sender", *msg.Sender).
				Str("resolved_portal", string(portalKey.ID)).
				Msg("Resolved delivery receipt portal via group member lookup")
			resolved = true
		}
	}

	if !resolved {
		if portal, _ := c.Main.Bridge.GetExistingPortalByKey(ctx, portalKey); portal != nil && portal.MXID != "" {
			resolved = true
		}
	}

	if msg.Uuid != "" {
		if msgPortal := c.resolvePortalByTargetMessage(log, msg.Uuid); msgPortal.ID != "" &&
			(msgPortal.ID != portalKey.ID || msgPortal.Receiver != portalKey.Receiver) {
			log.Debug().
				Str("uuid", msg.Uuid).
				Str("old_portal", string(portalKey.ID)).
				Str("resolved_portal", string(msgPortal.ID)).
				Msg("Resolved delivery receipt portal via target message UUID")
			portalKey = msgPortal
			resolved = true
		}
	}

	portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, portalKey)
	if err != nil || portal == nil || portal.MXID == "" {
		log.Debug().
			Err(err).
			Bool("resolved", resolved).
			Str("uuid", msg.Uuid).
			Str("portal_key", string(portalKey.ID)).
			Str("sender_guid", ptrStringOr(msg.SenderGuid, "")).
			Msg("Dropping delivery receipt: no portal found after fallback chain")
		return
	}

	// Race window: after we send an outgoing message, bridgev2's framework
	// inserts the Message row into the DB AFTER HandleMatrixMessage returns,
	// but on APNs-flap retry the stored-message echo can trigger this handler
	// within ~2ms of the FFI returning — before the framework has committed
	// the insert. Retry with a short backoff so the insert has time to land.
	// On the non-race path the first query succeeds immediately; overhead
	// only pays when the miss is real.
	msgID := makeMessageID(msg.Uuid)
	altUUID := strings.ToUpper(msg.Uuid)
	if altUUID == msg.Uuid {
		altUUID = strings.ToLower(msg.Uuid)
	}
	altMsgID := makeMessageID(altUUID)
	tryLookup := func() ([]*database.Message, bool, error) {
		dbM, err := c.Main.Bridge.DB.Message.GetAllPartsByID(ctx, portal.Receiver, msgID)
		if err == nil && len(dbM) > 0 {
			return dbM, false, nil
		}
		dbAlt, altErr := c.Main.Bridge.DB.Message.GetAllPartsByID(ctx, portal.Receiver, altMsgID)
		if altErr == nil && len(dbAlt) > 0 {
			return dbAlt, true, nil
		}
		// Propagate the case-sensitive error (primary path); alt is just a fallback.
		return nil, false, err
	}

	var dbMessages []*database.Message
	var lookupErr error
	var matchedAlt bool
	backoff := []time.Duration{0, 100 * time.Millisecond, 300 * time.Millisecond, 700 * time.Millisecond, 1500 * time.Millisecond}
	for _, d := range backoff {
		if d > 0 {
			time.Sleep(d)
		}
		dbMessages, matchedAlt, lookupErr = tryLookup()
		if lookupErr == nil && len(dbMessages) > 0 {
			break
		}
	}

	if lookupErr != nil || len(dbMessages) == 0 {
		log.Debug().
			Err(lookupErr).
			Str("uuid", msg.Uuid).
			Str("portal_id", string(portalKey.ID)).
			Str("receiver", string(portal.Receiver)).
			Msg("Dropping delivery receipt: target message not in bridge DB")
		return
	}

	if matchedAlt {
		log.Info().
			Str("uuid", msg.Uuid).
			Str("alt_uuid", altUUID).
			Str("portal_id", string(portalKey.ID)).
			Msg("Delivery receipt matched via case-insensitive UUID fallback")
	}

	normalizedSender := normalizeIdentifierForPortalID(ptrStringOr(msg.Sender, ""))
	// For DM portals, use the portal ID as the sender identity so the ghost
	// matches the canonical handle (avoids phantom ghost from alternate handles).
	portalID := string(portalKey.ID)
	if !strings.Contains(portalID, ",") && !strings.HasPrefix(portalID, "gid:") {
		normalizedSender = portalID
	}
	senderUserID := makeUserID(normalizedSender)
	ghost, err := c.Main.Bridge.GetGhostByID(ctx, senderUserID)
	if err != nil || ghost == nil {
		log.Debug().
			Err(err).
			Str("uuid", msg.Uuid).
			Str("portal_id", portalID).
			Str("sender_user_id", string(senderUserID)).
			Msg("Dropping delivery receipt: ghost not found for sender")
		return
	}

	for _, dbMsg := range dbMessages {
		c.Main.Bridge.Matrix.SendMessageStatus(ctx, &bridgev2.MessageStatus{
			Status:      event.MessageStatusSuccess,
			DeliveredTo: []id.UserID{ghost.Intent.GetMXID()},
		}, &bridgev2.MessageStatusEventInfo{
			RoomID:        portal.MXID,
			SourceEventID: dbMsg.MXID,
			Sender:        dbMsg.SenderMXID,
		})
	}
}

func (c *IMClient) handleTyping(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName, msg.Sender, msg.SenderGuid)

	// For group typing indicators, iMessage may only include [sender, target]
	// without the full participant list. If the portal key resolves to a
	// non-existent portal (DM-style key), try sender_guid lookup first.
	ctx := context.Background()
	portal, _ := c.Main.Bridge.GetExistingPortalByKey(ctx, portalKey)
	if (portal == nil || portal.MXID == "") && msg.SenderGuid != nil && *msg.SenderGuid != "" {
		// Try gid: portal ID first
		gidKey := networkid.PortalKey{ID: networkid.PortalID("gid:" + *msg.SenderGuid), Receiver: c.UserLogin.ID}
		if gidPortal, _ := c.Main.Bridge.GetExistingPortalByKey(ctx, gidKey); gidPortal != nil && gidPortal.MXID != "" {
			portalKey = gidKey
			log.Debug().
				Str("sender_guid", *msg.SenderGuid).
				Str("resolved_portal", string(portalKey.ID)).
				Msg("Resolved typing portal via gid: lookup")
			goto found
		}
		// Fall back to cache lookup for legacy portals
		c.imGroupGuidsMu.RLock()
		for portalIDStr, guid := range c.imGroupGuids {
			if strings.EqualFold(guid, *msg.SenderGuid) {
				if c.guidCacheMatchIsStale(portalIDStr, msg.Participants) {
					continue
				}
				portalKey = networkid.PortalKey{ID: networkid.PortalID(portalIDStr), Receiver: c.UserLogin.ID}
				c.imGroupGuidsMu.RUnlock()
				log.Debug().
					Str("sender_guid", *msg.SenderGuid).
					Str("resolved_portal", string(portalKey.ID)).
					Msg("Resolved typing portal via sender_guid cache lookup")
				goto found
			}
		}
		c.imGroupGuidsMu.RUnlock()
	}
	// Fall back to member tracking
	if (portal == nil || portal.MXID == "") && msg.Sender != nil {
		if groupKey, ok := c.findGroupPortalForMember(*msg.Sender); ok {
			portalKey = groupKey
			log.Debug().
				Str("sender", *msg.Sender).
				Str("resolved_portal", string(portalKey.ID)).
				Msg("Resolved typing portal via group member lookup")
		}
	}
found:

	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Typing{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventTyping,
			PortalKey: portalKey,
			Sender:    c.canonicalizeDMSender(portalKey, c.makeEventSender(msg.Sender)),
			Timestamp: time.UnixMilli(int64(msg.TimestampMs)),
		},
		Timeout: 60 * time.Second,
	})
}

// ============================================================================
// Matrix → iMessage
// ============================================================================

// convertURLPreviewToIMessage encodes a Beeper link preview (or auto-detected
// URL) into the sideband text prefix that Rust parses for rich link sending.
// Follows the pattern from mautrix-whatsapp's urlpreview.go.
func (c *IMClient) convertURLPreviewToIMessage(ctx context.Context, content *event.MessageEventContent) string {
	log := zerolog.Ctx(ctx)
	body := content.Body

	// Note: we intentionally do NOT treat empty BeeperLinkPreviews ([]) as
	// "explicitly disabled." Beeper sends [] when it can't generate a preview
	// itself (e.g. bare domains like "x.com"), but our bridge has its own
	// og: scraper that can often succeed. So we fall through to auto-detection.

	// Priority 1: Explicit BeeperLinkPreviews from Matrix
	if len(content.BeeperLinkPreviews) > 0 {
		lp := content.BeeperLinkPreviews[0]
		canonical := lp.CanonicalURL
		if canonical == "" {
			canonical = lp.MatchedURL
		}
		log.Debug().
			Str("matched_url_host", logSafeURL(lp.MatchedURL)).
			Str("canonical_url_host", logSafeURL(canonical)).
			Bool("has_title", lp.Title != "").
			Msg("Encoding Beeper link preview for iMessage")
		return "\x00RL\x01" + lp.MatchedURL + "\x01" + canonical + "\x01" + lp.Title + "\x01" + lp.Description + "\x00" + body
	}

	// Priority 2: Auto-detect URL and fetch preview via homeserver or og: scraping
	if detectedURL := urlRegex.FindString(body); detectedURL != "" && isLikelyURL(detectedURL) {
		fetchURL := normalizeURL(detectedURL)
		log.Debug().Str("detected_url_host", logSafeURL(detectedURL)).Msg("Auto-detected URL in outbound message, fetching preview")
		title, desc := "", ""
		// Try homeserver preview first
		if mc, ok := c.Main.Bridge.Matrix.(bridgev2.MatrixConnectorWithURLPreviews); ok {
			if lp, err := mc.GetURLPreview(ctx, fetchURL); err == nil && lp != nil {
				title = lp.Title
				desc = lp.Description
				log.Debug().Bool("has_title", title != "").Bool("has_description", desc != "").Msg("Got URL preview from homeserver for outbound")
			} else if err != nil {
				log.Debug().Err(sanitizeURLError(err, fetchURL, detectedURL)).Msg("Failed to fetch URL preview from homeserver for outbound")
			}
		}
		// Fall back to our own og: scraping if homeserver didn't provide metadata
		if title == "" && desc == "" {
			ogData := fetchPageMetadata(ctx, fetchURL)
			title = ogData["title"]
			desc = ogData["description"]
			if title != "" || desc != "" {
				log.Debug().Bool("has_title", title != "").Bool("has_description", desc != "").Msg("Got URL preview from og: scraping for outbound")
			}
		}
		return "\x00RL\x01" + detectedURL + "\x01" + fetchURL + "\x01" + title + "\x01" + desc + "\x00" + body
	}

	return body
}

// retrySendOnAPNsFlap retries an APNs-dependent send up to three times across
// 3s total when a transient APNs flap manifests as "Send timeout; try again".
// APNs reconnect grace is 30s on our side, so a short retry almost always
// lands on the restored connection.
func retrySendOnAPNsFlap(op func() error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = op()
		if err == nil {
			return nil
		}
		if msg := strings.ToLower(err.Error()); !strings.Contains(msg, "send timeout; try again") && !strings.Contains(msg, "sendtimedout") {
			return err
		}
		time.Sleep(time.Duration(1+attempt) * time.Second)
	}
	return err
}

func (c *IMClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	if c.client == nil {
		return nil, bridgev2.ErrNotLoggedIn
	}

	conv := c.portalToConversation(msg.Portal)

	// File/image messages
	if msg.Content.URL != "" || msg.Content.File != nil {
		return c.handleMatrixFile(ctx, msg, conv)
	}

	textToSend := c.convertURLPreviewToIMessage(ctx, msg.Content)

	replyGuid, replyPart := extractReplyInfo(msg.ReplyTo)
	// Rust-side send_with_flap_retry handles SendTimedOut retry with a stable
	// UUID (lib.rs:~7373). No Go-side retry here — a retry would generate a
	// fresh MessageInst and orphan delivery receipts for the first attempt.
	uuid, err := c.client.SendMessage(conv, textToSend, nil, c.handle, replyGuid, replyPart, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to send iMessage: %w", err)
	}
	zerolog.Ctx(ctx).Info().
		Str("uuid", uuid).
		Str("portal_id", string(msg.Portal.ID)).
		Bool("is_sms", c.isPortalSMS(string(msg.Portal.ID))).
		Msg("Message sent, storing UUID in bridge DB")
	// Persist UUID immediately so echo detection works even if the portal
	// is deleted before the APNs echo arrives.
	if c.cloudStore != nil {
		if err := c.cloudStore.persistMessageUUID(ctx, uuid, string(msg.Portal.ID), time.Now().UnixMilli(), true); err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Str("uuid", uuid).Msg("Failed to persist sent message UUID; echo may be delivered as duplicate")
		}
	}

	// If the outbound message has a URL and the client didn't provide previews,
	// add com.beeper.linkpreviews so Beeper renders it. Fire the double puppet
	// edit for both nil (field omitted) and empty slice (client couldn't preview)
	// since the bridge has its own og: scraper that can often succeed where
	// the client couldn't.
	if len(msg.Content.BeeperLinkPreviews) == 0 {
		if detectedURL := urlRegex.FindString(msg.Content.Body); detectedURL != "" && isLikelyURL(detectedURL) {
			go c.addOutboundURLPreview(msg.Event.ID, msg.Portal.MXID, msg.Content.Body, msg.Content.MsgType, detectedURL)
		}
	}

	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:        makeMessageID(uuid),
			SenderID:  makeUserID(c.handle),
			Timestamp: time.Now(),
			Metadata:  &MessageMetadata{},
		},
	}, nil
}

// addOutboundURLPreview edits an outbound Matrix event to add com.beeper.linkpreviews
// so Beeper displays a URL preview for messages sent from the client.
func (c *IMClient) addOutboundURLPreview(eventID id.EventID, roomID id.RoomID, body string, msgType event.MessageType, detectedURL string) {
	log := c.UserLogin.Log.With().
		Str("component", "url_preview").
		Stringer("event_id", eventID).
		Str("detected_url_host", logSafeURL(detectedURL)).
		Logger()
	ctx := log.WithContext(context.Background())

	intent := c.UserLogin.User.DoublePuppet(ctx)
	if intent == nil {
		log.Debug().Msg("No double puppet available, skipping outbound URL preview edit")
		return
	}

	preview := fetchURLPreview(ctx, c.Main.Bridge, intent, roomID, detectedURL)

	editContent := &event.MessageEventContent{
		MsgType:            msgType,
		Body:               body,
		BeeperLinkPreviews: []*event.BeeperLinkPreview{preview},
	}
	editContent.SetEdit(eventID)

	wrappedContent := &event.Content{Parsed: editContent}
	_, err := intent.SendMessage(ctx, roomID, event.EventMessage, wrappedContent, nil)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to send outbound URL preview edit")
	} else {
		log.Debug().Bool("has_title", preview.Title != "").Msg("Sent outbound URL preview edit")
	}
}

// fixOutboundImage re-uploads a corrected image to Matrix and edits the
// original event so all Beeper clients (desktop, Android, etc.) see the
// image with the right format, MIME type, and dimensions.
func (c *IMClient) fixOutboundImage(msg *bridgev2.MatrixMessage, data []byte, mimeType, fileName string, width, height int) {
	log := c.UserLogin.Log.With().
		Str("component", "image_fix").
		Stringer("event_id", msg.Event.ID).
		Logger()
	ctx := log.WithContext(context.Background())

	intent := c.UserLogin.User.DoublePuppet(ctx)
	if intent == nil {
		log.Debug().Msg("No double puppet available, skipping outbound image fix")
		return
	}

	url, encFile, err := intent.UploadMedia(ctx, "", data, fileName, mimeType)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to upload corrected image")
		return
	}

	editContent := &event.MessageEventContent{
		MsgType: event.MsgImage,
		Body:    fileName,
		Info: &event.FileInfo{
			MimeType: mimeType,
			Size:     len(data),
			Width:    width,
			Height:   height,
		},
	}
	if encFile != nil {
		editContent.File = encFile
	} else {
		editContent.URL = url
	}
	editContent.SetEdit(msg.Event.ID)

	wrappedContent := &event.Content{Parsed: editContent}
	_, err = intent.SendMessage(ctx, msg.Portal.MXID, event.EventMessage, wrappedContent, nil)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to edit outbound image event")
	} else {
		log.Debug().Str("mime", mimeType).Int("size", len(data)).Msg("Fixed outbound image on Matrix")
	}
}

func (c *IMClient) handleMatrixFile(ctx context.Context, msg *bridgev2.MatrixMessage, conv rustpushgo.WrappedConversation) (*bridgev2.MatrixMessageResponse, error) {
	var data []byte
	var err error
	if msg.Content.File != nil {
		data, err = c.Main.Bridge.Bot.DownloadMedia(ctx, msg.Content.File.URL, msg.Content.File)
	} else {
		data, err = c.Main.Bridge.Bot.DownloadMedia(ctx, msg.Content.URL, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to download media: %w", err)
	}

	fileName := msg.Content.FileName
	if fileName == "" {
		fileName = msg.Content.Body
	}
	if fileName == "" {
		fileName = "file"
	}

	mimeType := "application/octet-stream"
	if msg.Content.Info != nil && msg.Content.Info.MimeType != "" {
		mimeType = msg.Content.Info.MimeType
	}

	// Convert OGG Opus voice recordings to CAF Opus for native iMessage playback
	data, mimeType, fileName = convertAudioForIMessage(data, mimeType, fileName)

	// Process outbound images: detect actual format, convert non-JPEG to JPEG,
	// correct MIME type, and edit the Matrix event so all clients see it right.
	var matrixEdited bool
	if looksLikeImage(data) {
		origMime := mimeType
		if mimeType == "image/gif" {
			// GIFs are fine as-is, just detect correct MIME
			if detected := detectImageMIME(data); detected != "" && detected != mimeType {
				mimeType = detected
				fileName = strings.TrimSuffix(fileName, filepath.Ext(fileName)) + ".gif"
			}
		} else if img, _, isJPEG := decodeImageData(data); img != nil {
			if !isJPEG {
				var buf bytes.Buffer
				if encErr := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); encErr == nil {
					data = buf.Bytes()
					mimeType = "image/jpeg"
					fileName = strings.TrimSuffix(fileName, filepath.Ext(fileName)) + ".jpg"
				}
			} else if detected := detectImageMIME(data); detected != "" && detected != mimeType {
				mimeType = detected
				fileName = strings.TrimSuffix(fileName, filepath.Ext(fileName)) + ".jpg"
			}
			// Edit the Matrix event with corrected image so other Beeper clients see it right
			if mimeType != origMime {
				b := img.Bounds()
				go c.fixOutboundImage(msg, data, mimeType, fileName, b.Dx(), b.Dy())
				matrixEdited = true
			}
		} else {
			// Can't decode but fix MIME type at least
			if detected := detectImageMIME(data); detected != "" && detected != mimeType {
				mimeType = detected
				ext := ".bin"
				switch detected {
				case "image/jpeg":
					ext = ".jpg"
				case "image/png":
					ext = ".png"
				case "image/tiff":
					ext = ".tiff"
				}
				fileName = strings.TrimSuffix(fileName, filepath.Ext(fileName)) + ext
			}
		}
	}
	_ = matrixEdited

	replyGuid, replyPart := extractReplyInfo(msg.ReplyTo)

	// Rust-side send_with_flap_retry handles SendTimedOut retry with a stable
	// UUID — no Go-side retry here (would orphan delivery receipts).
	uuid, err := c.client.SendAttachment(conv, data, mimeType, mimeToUTI(mimeType), fileName, c.handle, replyGuid, replyPart, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to send attachment: %w", err)
	}
	// Persist UUID immediately so echo detection works even if the portal
	// is deleted before the APNs echo arrives.
	if c.cloudStore != nil {
		if err := c.cloudStore.persistMessageUUID(ctx, uuid, string(msg.Portal.ID), time.Now().UnixMilli(), true); err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Str("uuid", uuid).Msg("Failed to persist sent attachment UUID; echo may be delivered as duplicate")
		}
	}

	// If the Matrix event has body text alongside the attachment, send it as a
	// separate iMessage so the text is editable, reactable, etc. — like every
	// other chat app. Skip when body duplicates the filename (no real caption).
	textUUID := ""
	textMXID := id.EventID("")
	siblingUUID := ""
	if msg.Content.FileName != "" && msg.Content.Body != "" && msg.Content.Body != msg.Content.FileName {
		tUUID, textErr := c.client.SendMessage(conv, msg.Content.Body, nil, c.handle, nil, nil, nil)
		if textErr != nil {
			zerolog.Ctx(ctx).Warn().Err(textErr).Str("attachment_uuid", uuid).Msg("Failed to send caption as follow-up text; attachment was delivered")
		} else {
			textUUID = tUUID
			siblingUUID = uuid
			if c.cloudStore != nil {
				if err := c.cloudStore.persistMessageUUID(ctx, tUUID, string(msg.Portal.ID), time.Now().UnixMilli(), true); err != nil {
					zerolog.Ctx(ctx).Warn().Err(err).Str("uuid", tUUID).Msg("Failed to persist sent text UUID; echo may be delivered as duplicate")
				}
			}

			// Surface the caption as a standalone Matrix m.text event (via the
			// user's double puppet) so the user's Matrix client treats it like
			// any other text message — editable, reactable, etc. Also edit the
			// original m.image to drop the caption body so the text doesn't
			// appear twice in the room.
			if dp := c.UserLogin.User.DoublePuppet(ctx); dp != nil {
				textContent := &event.MessageEventContent{
					MsgType: event.MsgText,
					Body:    msg.Content.Body,
				}
				resp, err := dp.SendMessage(ctx, msg.Portal.MXID, event.EventMessage, &event.Content{Parsed: textContent}, nil)
				if err != nil {
					zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to bridge caption as standalone Matrix text event")
				} else if resp != nil {
					textMXID = resp.EventID
					// Edit the original m.image to drop the caption body so it doesn't render twice.
					editContent := &event.MessageEventContent{
						MsgType:  event.MsgImage,
						Body:     msg.Content.FileName,
						FileName: msg.Content.FileName,
						Info:     msg.Content.Info,
						URL:      msg.Content.URL,
						File:     msg.Content.File,
					}
					editContent.SetEdit(msg.Event.ID)
					if _, err := dp.SendMessage(ctx, msg.Portal.MXID, event.EventMessage, &event.Content{Parsed: editContent}, nil); err != nil {
						zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to clear caption from original m.image event")
					}
				}
			}
		}
	}

	// If we bridged the caption as its own m.text Matrix event, the attachment
	// and the text live on separate Matrix events. Each gets its own DB row so
	// edits/redacts/reactions on either route to the right iMessage.
	if textUUID != "" && textMXID != "" {
		bridgeRef := c.Main.Bridge
		userLogin := c.UserLogin
		portalKey := msg.Portal.PortalKey
		senderID := makeUserID(c.handle)
		now := time.Now()
		return &bridgev2.MatrixMessageResponse{
			DB: &database.Message{
				ID:        makeMessageID(uuid),
				SenderID:  senderID,
				Timestamp: now,
				Metadata:  &MessageMetadata{HasAttachments: true},
			},
			PostSave: func(ctx context.Context, primary *database.Message) {
				textRow := &database.Message{
					Room:       portalKey,
					ID:         makeMessageID(textUUID),
					MXID:       textMXID,
					SenderID:   senderID,
					SenderMXID: userLogin.UserMXID,
					Timestamp:  now,
					Metadata:   &MessageMetadata{},
				}
				if err := bridgeRef.DB.Message.Insert(ctx, textRow); err != nil {
					zerolog.Ctx(ctx).Warn().Err(err).Str("text_uuid", textUUID).Msg("Failed to insert DB row for bridged caption text")
				}
			},
		}, nil
	}

	// Fallback when the double puppet isn't available: keep the attachment
	// as the primary Matrix-side event and rely on sibling-UUID metadata so
	// at least redact unsends both halves on iMessage.
	finalUUID := uuid
	hasAttachments := true
	if textUUID != "" {
		finalUUID = textUUID
		hasAttachments = false
	}
	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:        makeMessageID(finalUUID),
			SenderID:  makeUserID(c.handle),
			Timestamp: time.Now(),
			Metadata:  &MessageMetadata{HasAttachments: hasAttachments, SiblingUUID: siblingUUID},
		},
	}, nil
}

func (c *IMClient) HandleMatrixTyping(ctx context.Context, msg *bridgev2.MatrixTyping) error {
	if c.client == nil || !c.Main.Config.TypingNotifications {
		return nil
	}
	conv := c.portalToConversation(msg.Portal)
	if conv.IsSms {
		return nil
	}
	return retrySendOnAPNsFlap(func() error {
		return c.client.SendTyping(conv, msg.IsTyping, c.handle)
	})
}

func (c *IMClient) HandleMatrixReadReceipt(ctx context.Context, receipt *bridgev2.MatrixReadReceipt) error {
	if c.client == nil || !c.Main.Config.ReadReceipts {
		return nil
	}
	conv := c.portalToConversation(receipt.Portal)
	if conv.IsSms {
		return nil
	}
	var forUuid *string
	if receipt.ExactMessage != nil {
		uuid := string(receipt.ExactMessage.ID)
		// Strip attachment suffixes like _att0, _att1 — Rust expects a pure UUID
		if idx := strings.Index(uuid, "_att"); idx > 0 {
			uuid = uuid[:idx]
		}
		forUuid = &uuid
	}
	err := retrySendOnAPNsFlap(func() error {
		return c.client.SendReadReceipt(conv, c.handle, forUuid)
	})
	if err != nil {
		errStr := err.Error()
		// Suppress non-actionable failures: IDS lookup errors (6001) for
		// urn:biz: business chat portals and edge cases between restart and SMS
		// state restoration; NoValidTargets for contacts with no reachable handle.
		// All other errors propagate normally so real network/auth failures surface.
		if strings.Contains(errStr, "6001") || strings.Contains(errStr, "NoValidTargets") {
			zerolog.Ctx(ctx).Warn().Err(err).
				Str("portal_id", string(receipt.Portal.ID)).
				Msg("Read receipt failed (non-actionable, suppressed)")
			return nil
		}
		return err
	}
	return nil
}

func (c *IMClient) HandleMatrixEdit(ctx context.Context, msg *bridgev2.MatrixEdit) error {
	if c.client == nil {
		return bridgev2.ErrNotLoggedIn
	}

	conv := c.portalToConversation(msg.Portal)
	if conv.IsSms {
		return fmt.Errorf("edits are not supported for SMS conversations")
	}
	targetGUID := string(msg.EditTarget.ID)

	// Rust-side retry handles SendTimedOut with stable UUID.
	_, err := c.client.SendEdit(conv, targetGUID, 0, msg.Content.Body, c.handle)
	if err == nil {
		// Work around mautrix-go bridgev2 not incrementing EditCount before saving.
		msg.EditTarget.EditCount++
	}
	return err
}

func (c *IMClient) HandleMatrixMessageRemove(ctx context.Context, msg *bridgev2.MatrixMessageRemove) error {
	if c.client == nil {
		return bridgev2.ErrNotLoggedIn
	}

	conv := c.portalToConversation(msg.Portal)
	if conv.IsSms {
		return fmt.Errorf("message retraction is not supported for SMS conversations")
	}

	// If this Matrix event was bridged as two iMessages (attachment + caption text),
	// unsend the sibling first. The attachment was sent before the text on the wire,
	// so unsend it first to keep the same wire ordering — some peers ignore unsends
	// that arrive out of timeline order for an attachment.
	var siblingUUID string
	if meta, ok := msg.TargetMessage.Metadata.(*MessageMetadata); ok && meta != nil {
		siblingUUID = meta.SiblingUUID
	}

	// Track scrub failures so we can fail-closed at the end after both
	// Apple-side unsends have been attempted. We can't bail mid-flight
	// because we've already committed to one Apple-side unsend by the
	// time we get to the second.
	var scrubErrs []error

	if siblingUUID != "" {
		c.trackOutboundUnsend(siblingUUID)
		if _, sibErr := c.client.SendUnsend(conv, siblingUUID, 0, c.handle); sibErr != nil {
			zerolog.Ctx(ctx).Warn().Err(sibErr).
				Str("sibling_uuid", siblingUUID).
				Msg("Failed to unsend sibling iMessage on split image+caption redact")
		} else if c.cloudStore != nil {
			if scrubErr := c.cloudStore.softDeleteMessageByGUID(ctx, siblingUUID); scrubErr != nil {
				zerolog.Ctx(ctx).Error().Err(scrubErr).Str("sibling_uuid", siblingUUID).
					Msg("Failed to soft-delete sibling cloud_message on split image+caption redact")
				scrubErrs = append(scrubErrs, scrubErr)
			}
		}
	}

	// Track outbound unsend so we can suppress the APNs echo.
	c.trackOutboundUnsend(string(msg.TargetMessage.ID))
	// Rust-side retry handles SendTimedOut with stable UUID.
	_, err := c.client.SendUnsend(conv, string(msg.TargetMessage.ID), 0, c.handle)

	// Soft-delete the message in local DB so it doesn't re-bridge on backfill,
	// while preserving the UUID for echo detection.
	if c.cloudStore != nil {
		if scrubErr := c.cloudStore.softDeleteMessageByGUID(ctx, string(msg.TargetMessage.ID)); scrubErr != nil {
			zerolog.Ctx(ctx).Error().Err(scrubErr).Str("target_uuid", string(msg.TargetMessage.ID)).
				Msg("Failed to soft-delete cloud_message after outbound unsend")
			scrubErrs = append(scrubErrs, scrubErr)
		}
	}

	// Fail-closed: report scrub failure to bridgev2 so it doesn't proceed
	// with future row deletion (currently a `// TODO delete msg/reaction
	// db row` in bridgev2/portal.go:2442 — not implemented today but
	// forward-compatible). When bridgev2 does start deleting on success,
	// the EXISTS gate in scrubBridgedBodies goes permanently FALSE for
	// scrubbed-fail rows, stranding plaintext.
	if err != nil {
		return err
	}
	if len(scrubErrs) > 0 {
		return fmt.Errorf("cloud_message scrub failed after Matrix-initiated unsend (privacy fail-closed): %v", scrubErrs[0])
	}
	return nil
}

func (c *IMClient) PreHandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (bridgev2.MatrixReactionPreResponse, error) {
	return bridgev2.MatrixReactionPreResponse{
		SenderID: makeUserID(c.handle),
		Emoji:    msg.Content.RelatesTo.Key,
	}, nil
}

func (c *IMClient) HandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (*database.Reaction, error) {
	if c.client == nil {
		return nil, bridgev2.ErrNotLoggedIn
	}

	conv := c.portalToConversation(msg.Portal)
	reaction, emoji := emojiToTapbackType(msg.Content.RelatesTo.Key)

	if conv.IsSms {
		// For SMS/RCS portals, send the tapback as an SMS text message via the
		// iPhone relay format instead of as an iMessage tapback (Message::React).
		//
		// SendTapback() generates Message::React, which causes prepare_send() in
		// Rust to assign a new random sender_guid when the conversation has none
		// (as SMS/RCS groups always do). The iPhone relay treats any unrecognised
		// sender_guid as a new iMessage group conversation rather than routing the
		// tapback to the existing SMS/RCS thread — producing a phantom new thread.
		//
		// Sending via SendMessage() with the reaction text uses RawSmsOutgoingMessage
		// format, which the iPhone routes by participants without needing a stable
		// sender_guid, correctly delivering to the existing SMS/RCS thread.
		// Strip _attN suffix: SMS doesn't support part-targeting, and the
		// suffixed ID would fail lookups and relay routing.
		targetGUID, _ := extractTapbackTarget(string(msg.TargetMessage.ID))
		reactionText := formatSMSReactionText(reaction, emoji, false)
		if c.cloudStore != nil {
			if origText, err := c.cloudStore.getMessageTextByGUID(ctx, targetGUID); err == nil && origText != "" {
				reactionText = formatSMSReactionTextWithBody(reaction, emoji, origText, false)
			}
		}
		// Rust-side retry handles SendTimedOut with stable UUID.
		uuid, err := c.client.SendMessage(conv, reactionText, nil, c.handle, &targetGUID, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to send SMS reaction: %w", err)
		}
		c.trackSmsReactionEcho(uuid)
		// Return nil: SMS reactions are sent as plain text, not as structured
		// tapbacks, so there is no database.Reaction to store. The remove path
		// (HandleMatrixReactionRemove) reads the target from the Matrix event
		// directly and does not need a stored Reaction record.
		return nil, nil
	}

	targetUUID, targetPart := extractTapbackTarget(string(msg.TargetMessage.ID))
	// Rust-side retry handles SendTimedOut with stable UUID.
	_, err := c.client.SendTapback(conv, targetUUID, targetPart, reaction, emoji, false, c.handle)
	if err != nil {
		return nil, fmt.Errorf("failed to send tapback: %w", err)
	}

	return &database.Reaction{
		MessageID: msg.TargetMessage.ID,
		SenderID:  makeUserID(c.handle),
		Emoji:     msg.Content.RelatesTo.Key,
		Metadata:  &MessageMetadata{},
		MXID:      msg.Event.ID,
	}, nil
}

func (c *IMClient) HandleMatrixReactionRemove(ctx context.Context, msg *bridgev2.MatrixReactionRemove) error {
	if c.client == nil {
		return bridgev2.ErrNotLoggedIn
	}

	conv := c.portalToConversation(msg.Portal)
	reaction, emoji := emojiToTapbackType(msg.TargetReaction.Emoji)

	if conv.IsSms {
		// Same SMS routing fix as HandleMatrixReaction: use SendMessage instead
		// of SendTapback to avoid the phantom new-thread creation.
		// Strip _attN suffix: SMS doesn't support part-targeting, and the
		// suffixed ID would fail lookups and relay routing.
		targetGUID, _ := extractTapbackTarget(string(msg.TargetReaction.MessageID))
		reactionText := formatSMSReactionText(reaction, emoji, true)
		if c.cloudStore != nil {
			if origText, err := c.cloudStore.getMessageTextByGUID(ctx, targetGUID); err == nil && origText != "" {
				reactionText = formatSMSReactionTextWithBody(reaction, emoji, origText, true)
			}
		}
		// Rust-side retry handles SendTimedOut with stable UUID.
		uuid, err := c.client.SendMessage(conv, reactionText, nil, c.handle, &targetGUID, nil, nil)
		if err != nil {
			return err
		}
		c.trackSmsReactionEcho(uuid)
		return nil
	}

	targetUUID, targetPart := extractTapbackTarget(string(msg.TargetReaction.MessageID))
	// Rust-side retry handles SendTimedOut with stable UUID.
	_, err := c.client.SendTapback(conv, targetUUID, targetPart, reaction, emoji, true, c.handle)
	return err
}

// HandleMatrixDeleteChat is called when the user deletes a chat in Matrix/Beeper.
// It cleans up local state (echo detection, local DB) but does NOT touch Apple:
// no MoveToRecycleBin APNs message, no CloudKit record deletion. The chat stays
// on the user's Apple devices; only the Beeper portal is removed.
func (c *IMClient) HandleMatrixDeleteChat(ctx context.Context, msg *bridgev2.MatrixDeleteChat) error {
	if c.client == nil {
		return bridgev2.ErrNotLoggedIn
	}

	// Flush the APNs reorder buffer before processing the delete.
	if c.msgBuffer != nil {
		c.msgBuffer.flush()
	}
	c.flushPendingPortalMsgs()

	log := zerolog.Ctx(ctx)
	portalID := string(msg.Portal.ID)

	conv := c.portalToConversation(msg.Portal)
	chatGuid := c.portalToChatGUID(portalID)

	log.Info().Str("chat_guid", chatGuid).Str("portal_id", portalID).Msg("Deleting chat from Apple")

	// Track as recently deleted so stale APNs echoes don't recreate it.
	c.trackDeletedChat(portalID)

	// Clean up local DB FIRST (fast) — prevents portal resurrection on restart
	// even if the CloudKit calls below hang or fail.
	var chatRecordNames, msgRecordNames []string
	if c.cloudStore != nil {
		if err := c.cloudStore.clearRestoreOverride(ctx, portalID); err != nil {
			log.Warn().Err(err).Str("portal_id", portalID).Msg("Failed to clear restore override for locally deleted chat")
		}
		chatRecordNames, _ = c.cloudStore.getCloudRecordNamesByPortalID(ctx, portalID)
		msgRecordNames, _ = c.cloudStore.getMessageRecordNamesByPortalID(ctx, portalID)

		// Fallback: if no record_names by portal_id, look up by group_id.
		groupID := ""
		if strings.HasPrefix(portalID, "gid:") {
			groupID = strings.TrimPrefix(portalID, "gid:")
		}
		if len(chatRecordNames) == 0 && groupID != "" {
			chatRecordNames, _ = c.cloudStore.getCloudRecordNamesByGroupID(ctx, groupID)
		}
		if len(msgRecordNames) == 0 && groupID != "" {
			msgRecordNames, _ = c.cloudStore.getMessageRecordNamesByGroupID(ctx, groupID)
		}

		// Soft-delete local records for both portal_id and group_id.
		// Fail-closed: same pattern as handleChatDelete — scrub failure must
		// not be followed by Matrix-side deletion that would forever strand
		// the cloud_message plaintext via the EXISTS predicate going FALSE.
		if err := c.cloudStore.deleteLocalChatByPortalID(ctx, portalID); err != nil {
			log.Error().Err(err).Str("portal_id", portalID).
				Msg("Aborting Matrix-initiated chat delete because cloud_message scrub failed — preserving privacy")
			return fmt.Errorf("failed to scrub cloud_message for chat delete: %w", err)
		}
		log.Info().Str("portal_id", portalID).Msg("Soft-deleted local cloud_chat and cloud_message records")
		if groupID != "" {
			if err := c.cloudStore.deleteLocalChatByGroupID(ctx, groupID); err != nil {
				log.Warn().Err(err).Str("group_id", groupID).Msg("Failed to soft-delete local cloud records by group_id")
			}
		}
	}

	// Delete from Apple — CloudKit backend only. chatdb users should not have
	// Apple-side chats deleted when they remove a portal from Beeper.
	if c.Main.Config.UseCloudKitBackfill() {
		c.deleteFromApple(portalID, conv, chatGuid, chatRecordNames, msgRecordNames)
	}

	return nil
}

// portalToChatGUID constructs the iMessage chat GUID for a portal, used for
// MoveToRecycleBin messages. Tries the cloud_chat DB first (has the exact
// chat_identifier from CloudKit), then falls back to constructing it from the
// portal ID.
func (c *IMClient) portalToChatGUID(portalID string) string {
	service := "iMessage"
	if c.isPortalSMS(portalID) {
		service = "SMS"
	}

	// Try cloud store first — it has the authoritative chat_identifier.
	// CloudKit stores just the identifier part (e.g. "rollingonchrome@proton.me"),
	// so we need to add the service prefix to form a full chat GUID.
	if c.cloudStore != nil {
		if chatID := c.cloudStore.getChatIdentifierByPortalID(context.Background(), portalID); chatID != "" {
			// If it already has a service prefix (e.g. "iMessage;-;..."), return as-is.
			if strings.Contains(chatID, ";") {
				return chatID
			}
			// Add the service prefix for bare identifiers.
			sep := ";-;"
			if strings.HasPrefix(portalID, "gid:") {
				sep = ";+;"
			}
			return service + sep + chatID
		}
	}
	// Fallback: construct from portal ID.
	if strings.HasPrefix(portalID, "gid:") {
		return service + ";+;" + strings.TrimPrefix(portalID, "gid:")
	}
	cleanID := strings.TrimPrefix(strings.TrimPrefix(portalID, "tel:"), "mailto:")
	// Strip legacy (sms...) suffix from pre-fix portal IDs so the chat GUID
	// passed to Rust is well-formed (e.g., "SMS;-;+12155167207" not "SMS;-;+12155167207(smsft)").
	cleanID = stripSmsSuffix(cleanID)
	return service + ";-;" + cleanID
}

// deleteFromApple sends MoveToRecycleBin via APNs and deletes known CloudKit
// records synchronously, then kicks off a background scan to catch any records
// not in the local DB. Local DB is already cleaned before this is called, so
// restarts are safe even if the background scan is interrupted.
func (c *IMClient) deleteFromApple(portalID string, conv rustpushgo.WrappedConversation, chatGuid string, chatRecordNames, msgRecordNames []string) {
	log := c.Main.Bridge.Log.With().Str("portal_id", portalID).Str("chat_guid", chatGuid).Logger()

	// Send MoveToRecycleBin via APNs — notifies other Apple devices.
	if err := c.client.SendMoveToRecycleBin(conv, c.handle, chatGuid); err != nil {
		log.Warn().Err(err).Msg("Failed to send MoveToRecycleBin via APNs")
	} else {
		log.Info().Msg("Sent MoveToRecycleBin via APNs")
	}

	// Delete known records from CloudKit synchronously (fast — no full table scan).
	if len(chatRecordNames) > 0 {
		if err := c.client.DeleteCloudChats(chatRecordNames); err != nil {
			log.Warn().Err(err).Strs("record_names", chatRecordNames).Msg("Failed to delete chats from CloudKit")
		} else {
			log.Info().Strs("record_names", chatRecordNames).Msg("Deleted chat records from CloudKit")
		}
	}
	// NOTE: We intentionally do NOT call DeleteCloudMessages here.
	// DeleteCloudMessages is a hard CloudKit delete from messageManateeZone — if
	// called before iPhone has a chance to process the MoveToRecycleBin APNs
	// command and move records to recoverableMessageDeleteZone, the messages are
	// permanently destroyed and can never be recovered.
	// Instead we rely on:
	//   1. SendMoveToRecycleBin (APNs) → iPhone moves messages to recycle bin in CloudKit
	//   2. Local soft-delete via deleteLocalChatByPortalID prevents portal resurrection
	//      even while messages are still visible in messageManateeZone pending iPhone processing

	// Background: scan CloudKit chat records if we don't have record names locally.
	// Only scans the chat zone (small) — NOT the message zone (can be 100k+ records).
	// Message records are handled by MoveToRecycleBin moving them to Apple's trash;
	// local soft-delete of cloud_message rows prevents portal resurrection.
	if len(chatRecordNames) == 0 && chatGuid != "" {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error().Interface("panic", r).Msg("Panic in background CloudKit chat deletion scan")
				}
			}()
			c.findAndDeleteCloudChatByIdentifier(log, chatGuid)
		}()
	}

}

// recoverChatOnApple sends a RecoverChat APNs message (command 182) and
// re-uploads the chat record to CloudKit. This is the inverse of deleteFromApple
// and makes the chat reappear on other Apple devices after a Beeper-side restore.
func (c *IMClient) recoverChatOnApple(portalID string) {
	if c.client == nil {
		return
	}

	log := c.Main.Bridge.Log.With().Str("portal_id", portalID).Logger()
	chatGuid := c.portalToChatGUID(portalID)

	// Build a minimal conversation for the APNs recover message.
	isGroup := strings.HasPrefix(portalID, "gid:") || strings.Contains(portalID, ",")
	isSms := c.isPortalSMS(portalID)
	var conv rustpushgo.WrappedConversation

	if isGroup {
		var participants []string
		if c.cloudStore != nil {
			if parts, err := c.cloudStore.getChatParticipantsByPortalID(context.Background(), portalID); err == nil && len(parts) > 0 {
				participants = parts
			}
		}
		if len(participants) == 0 && strings.Contains(portalID, ",") {
			participants = strings.Split(portalID, ",")
		}
		var senderGuid *string
		if strings.HasPrefix(portalID, "gid:") {
			guid := strings.TrimPrefix(portalID, "gid:")
			c.imGroupGuidsMu.RLock()
			if cached := c.imGroupGuids[portalID]; cached != "" {
				guid = cached
			}
			c.imGroupGuidsMu.RUnlock()
			senderGuid = &guid
		}
		conv = rustpushgo.WrappedConversation{
			Participants: participants,
			SenderGuid:   senderGuid,
			IsSms:        isSms,
		}
	} else {
		sendTo := c.resolveSendTarget(portalID)
		participants := []string{c.handle, sendTo}
		if c.isMyHandle(sendTo) {
			participants = []string{sendTo}
		}
		conv = rustpushgo.WrappedConversation{
			Participants: participants,
			IsSms:        isSms,
		}
	}

	// Send RecoverChat via APNs (command 182) — notifies other Apple devices.
	if err := c.client.SendRecoverChat(conv, c.handle, chatGuid); err != nil {
		log.Warn().Err(err).Str("chat_guid", chatGuid).Msg("Failed to send RecoverChat via APNs")
	} else {
		log.Info().Str("chat_guid", chatGuid).Msg("Sent RecoverChat via APNs — chat will reappear on Apple devices")
	}

	// Re-upload the chat record to CloudKit so it reappears in chatManateeZone.
	if c.cloudStore != nil {
		rec, err := c.cloudStore.getCloudChatRecordByPortalID(context.Background(), portalID)
		if err == nil && rec != nil {
			if err := c.client.RestoreCloudChat(
				rec.RecordName, rec.ChatIdentifier, rec.GroupID,
				rec.Style, rec.Service, rec.DisplayName, rec.Participants,
			); err != nil {
				log.Warn().Err(err).Msg("Failed to restore chat record in CloudKit")
			} else {
				log.Info().Str("record_name", rec.RecordName).Msg("Restored chat record in CloudKit")
			}
		} else if err != nil {
			log.Debug().Err(err).Msg("No cloud_chat record found for CloudKit restore (may be a new chat)")
		}
	}
}

// findAndDeleteCloudChatByIdentifier syncs all chat records from CloudKit,
// finds ones matching the given chat_identifier (e.g. "iMessage;-;user@example.com"),
// and deletes them. Used as a fallback when local DB doesn't have record_names.
//
// Note: This uses the same CloudSyncChats API starting from nil token (full scan).
// CloudKit change tokens are client-side state — this does NOT advance the main
// sync controller's server-side watermark or cause it to miss changes.
func (c *IMClient) findAndDeleteCloudChatByIdentifier(log zerolog.Logger, chatIdentifier string) {
	log.Info().Str("chat_identifier", chatIdentifier).Msg("Querying CloudKit for chat records to delete")

	var matchingRecordNames []string
	var token *string

	for page := 0; page < 256; page++ {
		resp, err := safeCloudSyncChats(c.client, token)
		if err != nil {
			log.Warn().Err(err).Msg("Failed to sync chats from CloudKit for delete lookup")
			return
		}
		for _, chat := range resp.Chats {
			if chat.CloudChatId == chatIdentifier && chat.RecordName != "" {
				matchingRecordNames = append(matchingRecordNames, chat.RecordName)
			}
		}
		prev := ptrStringOr(token, "")
		token = resp.ContinuationToken
		if resp.Done || (page > 0 && prev == ptrStringOr(token, "")) {
			break
		}
	}

	if len(matchingRecordNames) == 0 {
		log.Info().Str("chat_identifier", chatIdentifier).Msg("No matching chat records found in CloudKit")
	} else if err := c.client.DeleteCloudChats(matchingRecordNames); err != nil {
		log.Warn().Err(err).Strs("record_names", matchingRecordNames).Msg("Failed to delete chat records found via CloudKit query")
	} else {
		log.Info().Int("count", len(matchingRecordNames)).Str("chat_identifier", chatIdentifier).Msg("Deleted chat records found via CloudKit query")
	}
}

const (
	// plLockLevel is the power level required for destructive or bridge-owned
	// room actions. It sits just below the bridge bot's 9001 so the bot can
	// still perform them, but no human-reachable level (Matrix tops out at 100
	// in practice) can. The homeserver enforces this, so a user's attempt to
	// tombstone the room, toggle encryption, set server ACLs, invite/kick/ban,
	// or redact someone else's message is rejected outright. Power-level edits
	// themselves are intentionally NOT locked here — they're allowed through and
	// then rubberbanded by HandleMatrixPowerLevels.
	plLockLevel = 9000

	// botPowerLevel mirrors the level bridgev2 assigns the bridge bot in every
	// portal at creation. It must stay above plLockLevel so the bot can always
	// re-assert the hardened power levels.
	botPowerLevel = 9001
)

// hardenedPowerLevels returns the power-level overrides applied to every portal
// — DMs and group chats, on both the CloudKit (GetChatInfo) and chat.db
// (chatDBInfoToBridgev2) paths — so Matrix users can't clobber bridge-managed
// room state. See docs/postmortem-style notes: a user raising their power level
// can otherwise tombstone the room, silently kill incoming messages by raising
// events_default, pull unbridged users into a group, or redact other people's
// messages.
//
// Two layers of defense:
//
//  1. Homeserver-enforced lock. The destructive/bridge-owned actions are pinned
//     to plLockLevel, so the homeserver itself rejects any human user trying
//     them. Everyday messaging is deliberately left alone: events_default stays
//     0 so both ghosts and the user can always send, and a user can still
//     redact (unsend) their OWN messages because self-redaction is gated by
//     events_default, not the redact level.
//
//  2. Reset on apply (+ notice). The Custom hook (resetEscalatedUsers) runs on
//     every power-level apply — room creation, every member resync, and the
//     HandleMatrixPowerLevels rubberband. If a non-bot user sits above the room
//     baseline (users_default) it resets them and posts a single rate-limited
//     m.notice. Running on resync (not just on user-sent edits) is what catches
//     the escalation vectors that don't arrive as a user event — `set-pl` and
//     the homeserver admin API both write power levels as the bot, which the
//     framework filters out before HandleMatrixPowerLevels would ever see them.
//     This is safe against notice spam because the bridge never grants normal
//     users an explicit entry above users_default, so the reset only fires on a
//     genuine escalation, not on every resync.
//
// ctx/portal may be nil (the chat.db path has neither in scope); the reset still
// happens, only the m.notice is skipped.
func (c *IMClient) hardenedPowerLevels(ctx context.Context, portal *bridgev2.Portal) *bridgev2.PowerLevelOverrides {
	return &bridgev2.PowerLevelOverrides{
		EventsDefault: ptr.Ptr(0), // ghosts + users can always send messages
		Events: map[event.Type]int{
			// NOTE: m.room.power_levels is deliberately NOT locked. On homeservers
			// where a user can edit it, we let the change land (so it's visible in
			// the room), then snap it back. On platforms where the user can't (e.g.
			// Beeper users sit at 0 and power_levels needs 100), the escalation can
			// only come via set-pl/admin-API, which the resync reset below catches.
			event.StateTombstone:  plLockLevel, // no user may upgrade/replace the room
			event.StateEncryption: plLockLevel, // encryption is bridge-managed
			event.StateServerACL:  plLockLevel,
		},
		Invite: ptr.Ptr(plLockLevel), // membership is bridge-managed
		Kick:   ptr.Ptr(plLockLevel),
		Ban:    ptr.Ptr(plLockLevel),
		Redact: ptr.Ptr(plLockLevel), // can't redact others; self-unsend still works
		Custom: func(pl *event.PowerLevelsEventContent) bool {
			return c.resetEscalatedUsers(ctx, portal, pl)
		},
	}
}

// powerLevelsForMemberSync returns the power-level overrides to attach to a
// member list, choosing by whether the Matrix room already exists.
//
// On room CREATION (portal.MXID == ""), bridgev2 bakes these overrides into the
// CreateRoom request's PowerLevelOverride, and on Beeper (auto_join_invites) the
// server adds the initial roster via that same create — silently — only if the
// create's power levels don't get in the way. Baking the full hardened lockdown
// (Invite/Kick/Ban/Redact = plLockLevel, EventsDefault, the destructive-event
// locks and the Custom reset hook) into the CREATE request breaks that silent
// initial-member add, so the whole roster re-surfaces as "<name> joined the
// chat" on a fresh-DB backfill. Pre-d78e666 we created groups with only a light
// Invite=95 and DMs with no override, and creation was silent.
//
// So: at creation use those pre-lockdown levels; on every later resync
// (portal.MXID != "") apply the full hardened lockdown. The room therefore ends
// up — and stays — locked, because the immediate post-creation ChatResync runs
// this with portal.MXID set. Steady-state protection is unchanged; only the
// one create event is permissive.
func (c *IMClient) powerLevelsForMemberSync(ctx context.Context, portal *bridgev2.Portal, isGroup bool) *bridgev2.PowerLevelOverrides {
	if portal == nil || portal.MXID == "" {
		if isGroup {
			// Bridge still owns membership for groups, just at a human-reachable
			// lock so the silent initial-member add isn't rejected at creation.
			return &bridgev2.PowerLevelOverrides{Invite: ptr.Ptr(95)}
		}
		return nil
	}
	return c.hardenedPowerLevels(ctx, portal)
}

// resetEscalatedUsers is the Custom power-level hook from hardenedPowerLevels.
// It demotes any non-bot user whose level was raised above the room baseline
// (users_default) back down to that baseline, and — when a room is known and the
// per-room+user rate limit allows — posts one m.notice. The baseline is also
// exactly the level the user had before they escalated, since the bridge never
// grants normal users an explicit power-level entry. Returns true if it modified
// the power-level content, so the framework re-sends the event.
func (c *IMClient) resetEscalatedUsers(ctx context.Context, portal *bridgev2.Portal, pl *event.PowerLevelsEventContent) bool {
	if pl == nil {
		return false
	}
	botMXID := c.Main.Bridge.Bot.GetMXID()
	baseline := pl.UsersDefault

	// Snapshot the user IDs so we don't mutate the map mid-range.
	var userIDs []id.UserID
	for userID := range pl.Users {
		userIDs = append(userIDs, userID)
	}

	var demoted []id.UserID
	for _, userID := range userIDs {
		if userID == botMXID {
			continue
		}
		if pl.GetUserLevel(userID) <= baseline {
			continue
		}
		// The bot sits at 9001, so this reset always succeeds against a user.
		if pl.EnsureUserLevelAs(botMXID, userID, baseline) {
			demoted = append(demoted, userID)
		}
	}
	if len(demoted) == 0 {
		return false
	}

	if portal != nil && portal.MXID != "" {
		roomID := portal.MXID
		notifyCtx := context.Background()
		if ctx != nil {
			notifyCtx = context.WithoutCancel(ctx)
		}
		body := fmt.Sprintf("⚠️ Power-level changes are disabled in bridge chats. Your power level remains at %d.", baseline)
		for _, userID := range demoted {
			c.UserLogin.Log.Info().
				Stringer("user_id", userID).
				Stringer("room_id", roomID).
				Int("baseline", baseline).
				Msg("Reset escalated Matrix power level back to room default")
			go func() {
				if _, err := c.Main.Bridge.Bot.SendMessage(notifyCtx, roomID, event.EventMessage, &event.Content{
					Parsed: &event.MessageEventContent{MsgType: event.MsgNotice, Body: body},
				}, nil); err != nil {
					c.UserLogin.Log.Warn().Err(err).Stringer("room_id", roomID).Msg("Failed to post power-level reset notice")
				}
			}()
		}
	}
	return true
}

// rubberbandSetPowerLevel is the instant path for the `set-pl` command. The
// framework sends set-pl's change as the bot and filters bot-sent events before
// HandleMatrixPowerLevels would see them, so the connector's own set-pl command
// (cmdSetPowerLevel) calls this directly: apply the requested level just long
// enough to be visible, then snap the room back to the hardened policy —
// resetting the just-granted level and posting the m.notice via the Custom hook.
func (c *IMClient) rubberbandSetPowerLevel(ctx context.Context, portal *bridgev2.Portal, target id.UserID, level int) error {
	if portal == nil || portal.MXID == "" {
		return fmt.Errorf("no portal room")
	}
	pl, err := c.Main.Bridge.Matrix.GetPowerLevels(ctx, portal.MXID)
	if err != nil {
		return fmt.Errorf("failed to get power levels: %w", err)
	}
	botMXID := c.Main.Bridge.Bot.GetMXID()
	pl.EnsureUserLevel(botMXID, botPowerLevel)

	// Apply the requested level first so the change is briefly visible.
	if pl.EnsureUserLevelAs(botMXID, target, level) {
		if _, err := c.Main.Bridge.Bot.SendState(ctx, portal.MXID, event.StatePowerLevels, "", &event.Content{Parsed: pl}, time.Time{}); err != nil {
			return fmt.Errorf("failed to set power level: %w", err)
		}
	}
	// Snap back to the hardened policy (Custom hook resets + posts the notice).
	if c.hardenedPowerLevels(ctx, portal).Apply(botMXID, pl) {
		if _, err := c.Main.Bridge.Bot.SendState(ctx, portal.MXID, event.StatePowerLevels, "", &event.Content{Parsed: pl}, time.Time{}); err != nil {
			return fmt.Errorf("failed to re-assert hardened power levels: %w", err)
		}
	}
	return nil
}

// HandleMatrixPowerLevels implements bridgev2.PowerLevelHandlingNetworkAPI.
// iMessage has no power-level concept, so the change is never bridged to the
// network. Instead this callback is the immediate "rubberband" for homeservers
// where a user CAN edit power levels: the instant they do, re-apply the hardened
// overrides — reset any escalation back to the room default, re-assert
// events_default=0 and the destructive-action locks — and, if that changed
// anything, re-send the corrected power-levels event as the bot. The user's edit
// lands first (so it's visible in the timeline), then snaps back. The m.notice
// is posted by resetEscalatedUsers (the Custom hook inside Apply), which also
// covers the set-pl/admin-API vectors that never reach this callback.
func (c *IMClient) HandleMatrixPowerLevels(ctx context.Context, msg *bridgev2.MatrixPowerLevelChange) (bool, error) {
	portal := msg.Portal
	pl := msg.Content
	if portal == nil || portal.MXID == "" || pl == nil {
		return false, nil
	}
	// The homeserver already guarantees the bot's entry survives (a user can't
	// modify a level above their own), but assert it defensively so the bot is
	// always a high-enough actor to apply the overrides below.
	botMXID := c.Main.Bridge.Bot.GetMXID()
	pl.EnsureUserLevel(botMXID, botPowerLevel)

	// Apply runs the Custom hook (resetEscalatedUsers), which resets escalations
	// and posts the notice. If it changed nothing, the edit was already within
	// policy — leave it alone.
	if !c.hardenedPowerLevels(ctx, portal).Apply(botMXID, pl) {
		return false, nil
	}
	// Re-send the corrected power levels as the bot (the snap-back).
	if _, err := c.Main.Bridge.Bot.SendState(ctx, portal.MXID, event.StatePowerLevels, "", &event.Content{Parsed: pl}, time.Time{}); err != nil {
		return false, fmt.Errorf("failed to re-assert hardened power levels: %w", err)
	}
	// Return false: nothing was bridged to the iMessage network.
	return false, nil
}

// ============================================================================
// Chat & user info
// ============================================================================

func (c *IMClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	portalID := string(portal.ID)
	// Groups use "gid:<UUID>" portal IDs, or legacy comma-separated participant IDs
	isGroup := strings.HasPrefix(portalID, "gid:") || strings.Contains(portalID, ",")

	canBackfill := false
	if c.Main.Config.UseChatDBBackfill() {
		canBackfill = c.chatDB != nil
	} else if c.cloudStore != nil {
		if hasMessages, err := c.cloudStore.hasPortalMessages(ctx, portalID); err == nil {
			canBackfill = hasMessages
		}
		// Force backfill for portals with a restore override (from
		// !restore-chat or APNs RecoverChat). Even when cloud_message is
		// empty, the restore is legitimate and forward backfill should run
		// to page fresh messages from CloudKit.
		if !canBackfill {
			if overridden, err := c.cloudStore.hasRestoreOverride(ctx, portalID); err == nil && overridden {
				canBackfill = true
			}
		}
	}

	chatInfo := &bridgev2.ChatInfo{
		CanBackfill: canBackfill,
		// Suppress bridge bot timeline messages for name/member changes
		// during ChatResync. Real-time renames go through handleRename
		// which sends its own ChatInfoChange event (with timeline visibility).
		ExcludeChangesFromTimeline: true,
	}

	if isGroup {
		chatInfo.Type = ptr.Ptr(database.RoomTypeDefault)

		// For gid: portals, look up members from cloud_chat table;
		// for legacy comma-separated IDs, parse from the portal ID.
		memberList := c.resolveGroupMembers(ctx, portalID)

		memberMap := make(map[networkid.UserID]bridgev2.ChatMember)
		for _, member := range memberList {
			userID := makeUserID(member)
			if c.isMyHandle(member) {
				memberMap[userID] = bridgev2.ChatMember{
					EventSender: bridgev2.EventSender{
						IsFromMe:    true,
						SenderLogin: c.UserLogin.ID,
						Sender:      userID,
					},
					Membership: event.MembershipJoin,
				}
			} else {
				memberMap[userID] = bridgev2.ChatMember{
					EventSender: bridgev2.EventSender{Sender: userID},
					Membership:  event.MembershipJoin,
				}
			}
		}

		// CloudKit doesn't include the owner in the participant list (it's
		// implied). Always ensure we're in the member map so Beeper knows
		// we belong to this conversation.
		myUserID := makeUserID(c.handle)
		if _, hasSelf := memberMap[myUserID]; !hasSelf {
			memberMap[myUserID] = bridgev2.ChatMember{
				EventSender: bridgev2.EventSender{
					IsFromMe:    true,
					SenderLogin: c.UserLogin.ID,
					Sender:      myUserID,
				},
				Membership: event.MembershipJoin,
			}
		}
		chatInfo.Members = &bridgev2.ChatMemberList{
			IsFull:    true,
			MemberMap: memberMap,
			// Permissive at creation (silent initial-member add), full lockdown
			// on resync. See powerLevelsForMemberSync.
			PowerLevels: c.powerLevelsForMemberSync(ctx, portal, true),
		}

		// Only set the group name for NEW portals (no Matrix room yet).
		// For existing portals, skip — the name is managed by handleRename
		// (explicit renames) and makePortalKey's envelope-change detection.
		// Setting the name here for existing portals would revert correct
		// names to stale values from CloudKit or metadata, and produce
		// unwanted bridge bot "name changed" events.
		if portal.MXID == "" || portal.Name == "" {
			groupName, _ := c.resolveGroupName(ctx, portalID)
			chatInfo.Name = &groupName
		}

		// Set group photo from cache or CloudKit.
		//
		// Primary path: MMCS bytes previously downloaded via an APNs IconChange
		// message and persisted to group_photo_cache by handleIconChange.
		//
		// Fallback path: when the cache is empty (e.g. fresh bridge, or first
		// sync before any IconChange has arrived), attempt a CloudKit download
		// using the record_name stored during chat sync. Apple's iMessage
		// clients may not write the CloudKit "gp" asset field (preferring MMCS
		// delivery), so this fallback often returns MissingGroupPhoto — logged
		// at debug level and silently ignored. When it does succeed the bytes
		// are cached for future calls.
		if c.cloudStore != nil {
			photoLog := c.Main.Bridge.Log.With().Str("portal_id", portalID).Logger()
			photoTS, photoData, gpErr := c.cloudStore.getGroupPhoto(ctx, portalID)
			if gpErr != nil {
				photoLog.Warn().Err(gpErr).Msg("group_photo: DB lookup error")
			} else if len(photoData) == 0 {
				photoLog.Debug().Msg("group_photo: no cached photo in DB, trying CloudKit")
				photoData, photoTS = c.fetchAndCacheGroupPhoto(ctx, photoLog, portalID)
			}
			if len(photoData) > 0 {
				avatarID := networkid.AvatarID(fmt.Sprintf("icon-change:%d", photoTS))
				photoLog.Info().Int64("ts", photoTS).Int("bytes", len(photoData)).Msg("group_photo: setting avatar from cache")
				cachedData := photoData
				chatInfo.Avatar = &bridgev2.Avatar{
					ID:  avatarID,
					Get: func(ctx context.Context) ([]byte, error) { return cachedData, nil },
				}
			}
		}

		// Persist sender_guid to portal metadata for gid: portals.
		// Only persist iMessage protocol-level group names (cv_name) to
		// metadata — NOT auto-generated contact-resolved names — since
		// the metadata GroupName is used for outbound message routing.
		if strings.HasPrefix(portalID, "gid:") {
			// Prefer original-case sender_guid from cache (populated by
			// makePortalKey synchronously before GetChatInfo runs).
			gid := strings.TrimPrefix(portalID, "gid:")
			c.imGroupGuidsMu.RLock()
			if cached := c.imGroupGuids[portalID]; cached != "" {
				gid = cached
			}
			c.imGroupGuidsMu.RUnlock()
			c.imGroupNamesMu.RLock()
			protocolName := c.imGroupNames[portalID]
			c.imGroupNamesMu.RUnlock()
			isSms := c.isPortalSMS(portalID)
			chatInfo.ExtraUpdates = func(ctx context.Context, p *bridgev2.Portal) bool {
				meta, ok := p.Metadata.(*PortalMetadata)
				if !ok {
					meta = &PortalMetadata{}
				}
				changed := false
				if meta.SenderGuid != gid {
					meta.SenderGuid = gid
					changed = true
				}
				if protocolName != "" && meta.GroupName != protocolName {
					meta.GroupName = protocolName
					changed = true
				}
				if meta.IsSms != isSms {
					meta.IsSms = isSms
					changed = true
				}
				if changed {
					p.Metadata = meta
				}
				return changed
			}
		}
	} else {
		chatInfo.Type = ptr.Ptr(database.RoomTypeDM)
		otherUser := makeUserID(portalID)
		isSelfChat := c.isMyHandle(portalID)

		memberMap := map[networkid.UserID]bridgev2.ChatMember{
			makeUserID(c.handle): {
				EventSender: bridgev2.EventSender{
					IsFromMe:    true,
					SenderLogin: c.UserLogin.ID,
					Sender:      makeUserID(c.handle),
				},
				Membership: event.MembershipJoin,
			},
		}
		// Only add the other user if it's not a self-chat, to avoid
		// overwriting the IsFromMe entry with a duplicate map key.
		if !isSelfChat {
			memberMap[otherUser] = bridgev2.ChatMember{
				EventSender: bridgev2.EventSender{Sender: otherUser},
				Membership:  event.MembershipJoin,
			}
		}

		members := &bridgev2.ChatMemberList{
			IsFull:      true,
			OtherUserID: otherUser,
			MemberMap:   memberMap,
			// Permissive at creation (silent initial-member add), full lockdown
			// on resync. See powerLevelsForMemberSync.
			PowerLevels: c.powerLevelsForMemberSync(ctx, portal, false),
		}

		// For self-chats, set an explicit name and avatar from contacts since
		// the framework can't derive them from the ghost when the "other user"
		// is the logged-in user. Setting Name causes NameIsCustom=true in the
		// framework, which blocks UpdateInfoFromGhost (it returns early when
		// NameIsCustom is set), so we must also set the avatar explicitly here.
		if isSelfChat {
			selfName := c.resolveContactDisplayname(portalID)
			chatInfo.Name = &selfName

			// Pull contact photo for self-chat room avatar.
			localID := stripIdentifierPrefix(portalID)
			if c.contacts != nil {
				if contact, _ := c.contacts.GetContactInfo(localID); contact != nil && len(contact.Avatar) > 0 {
					avatarHash := sha256.Sum256(contact.Avatar)
					avatarData := contact.Avatar
					chatInfo.Avatar = &bridgev2.Avatar{
						ID: networkid.AvatarID(fmt.Sprintf("contact:%s:%s", portalID, hex.EncodeToString(avatarHash[:8]))),
						Get: func(ctx context.Context) ([]byte, error) {
							return avatarData, nil
						},
					}
				}
			}
		}
		// For regular DMs, don't set an explicit room name. With
		// private_chat_portal_meta, the framework derives it from the ghost's
		// display name, which auto-updates when contacts are edited.
		chatInfo.Members = members

		// New-portal-creation StatusKit invite hook. When bridgev2 materializes
		// a fresh DM portal mid-uptime (incoming iMessage from a contact whose
		// 1:1 portal didn't exist yet), fire a single-handle invite immediately
		// so the peer gets our key without waiting up to 4h for the periodic
		// tick. subscribeAfterInit only catches portals that existed at connect
		// time; the post-backfill hook (onForwardBackfillDone) only fires once
		// at initial bootstrap. Without this hook, mid-uptime new peers had no
		// automatic invite path between sweeps.
		//
		// Mirrors OB's setActiveChat trigger (chat_manager.dart:64-78). OB
		// invites on chat-open; the bridge's non-UI analog is "portal
		// materialized for the first time." MXID empty distinguishes a brand
		// new portal from a refresh of an existing one. Self-chat skipped
		// (peer == self). Helper applies the same latch / known-keys / spacing
		// guards as the periodic sweep.
		if !isSelfChat && portal.MXID == "" {
			statuskitLog := c.Main.Bridge.Log.With().Str("hook", "new-portal-statuskit").Str("portal_id", portalID).Logger()
			go c.inviteSingleHandleToStatusSharing(statuskitLog, portalID)
		}

		// Persist IsSms so CloudKit-created portals (no suffix, no live APNs
		// message yet) survive restarts. Mirrors the group ExtraUpdates pattern.
		isSms := c.isPortalSMS(portalID)
		chatInfo.ExtraUpdates = func(ctx context.Context, p *bridgev2.Portal) bool {
			meta, ok := p.Metadata.(*PortalMetadata)
			if !ok {
				meta = &PortalMetadata{}
			}
			if meta.IsSms != isSms {
				meta.IsSms = isSms
				p.Metadata = meta
				return true
			}
			return false
		}
	}

	return chatInfo, nil
}

// resolveContactDisplayname returns a contact-resolved display name for the
// given identifier (e.g. "tel:+1234567890"). Falls back to formatting the
// raw identifier if no contact is found.
func (c *IMClient) resolveContactDisplayname(identifier string) string {
	localID := stripIdentifierPrefix(identifier)
	if c.contacts != nil {
		contact, contactErr := c.contacts.GetContactInfo(localID)
		if contactErr != nil {
			c.Main.Bridge.Log.Debug().Err(contactErr).Str("id", localID).Msg("Failed to resolve contact info")
		}
		if contact != nil && contact.HasName() {
			return c.Main.Config.FormatDisplayname(DisplaynameParams{
				FirstName: contact.FirstName,
				LastName:  contact.LastName,
				Nickname:  contact.Nickname,
				ID:        localID,
			})
		}
	}
	return c.Main.Config.FormatDisplayname(identifierToDisplaynameParams(identifier))
}

func (c *IMClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	identifier := string(ghost.ID)
	if identifier == "" {
		return nil, nil
	}

	isBot := false
	ui := &bridgev2.UserInfo{
		IsBot:       &isBot,
		Identifiers: []string{identifier},
	}

	// Try contact info from cloud contacts (iCloud CardDAV)
	localID := stripIdentifierPrefix(identifier)
	var contact *imessage.Contact
	if c.contacts != nil {
		var contactErr error
		contact, contactErr = c.contacts.GetContactInfo(localID)
		if contactErr != nil {
			zerolog.Ctx(ctx).Debug().Err(contactErr).Str("id", localID).Msg("Failed to resolve contact info")
		}
	}

	// User-provided contacts (CardDAV / iCloud) always win. Any match from
	// c.contacts fully overrides the shared iMessage profile, regardless of
	// whether the contact has a name or photo: the user adding an entry is
	// itself the signal that they want their own data used. Shared profiles
	// only fill the gap when the identifier is unknown to the address book.
	if contact != nil {
		if contact.HasName() {
			name := c.Main.Config.FormatDisplayname(DisplaynameParams{
				FirstName: contact.FirstName,
				LastName:  contact.LastName,
				Nickname:  contact.Nickname,
				ID:        localID,
			})
			ui.Name = &name
		} else {
			name := c.Main.Config.FormatDisplayname(identifierToDisplaynameParams(identifier))
			ui.Name = &name
		}
		// Emit normalized, deduped identifiers. The raw vCard phone strings
		// carry display formatting straight from the address book (e.g.
		// "(XXX) XXX-XXXX"); concatenating "tel:"+phone ships a malformed
		// "tel:(XXX) XXX-XXXX" URI (spaces + parens) that the client can't
		// parse as a phone number, so the contact renders with no number.
		// contactPortalIDs() already produces clean E.164 tel: / lowercased
		// mailto: IDs and dedupes them — reuse it instead of re-deriving here.
		seen := map[string]bool{identifier: true}
		for _, id := range contactPortalIDs(contact) {
			if seen[id] {
				continue
			}
			seen[id] = true
			ui.Identifiers = append(ui.Identifiers, id)
		}
		if len(contact.Avatar) > 0 {
			avatarHash := sha256.Sum256(contact.Avatar)
			avatarData := contact.Avatar // capture for closure
			ui.Avatar = &bridgev2.Avatar{
				ID: networkid.AvatarID(fmt.Sprintf("contact:%s:%s", identifier, hex.EncodeToString(avatarHash[:8]))),
				Get: func(ctx context.Context) ([]byte, error) {
					return avatarData, nil
				},
			}
		}
		return ui, nil
	}

	// No user-provided contact — fall back to a shared iMessage profile
	// (Name & Photo Sharing / Me card) if one was received and cached.
	if profile := c.lookupSharedProfile(identifier); profile != nil {
		if profile.FirstName != "" || profile.LastName != "" || profile.DisplayName != "" {
			name := c.Main.Config.FormatDisplayname(DisplaynameParams{
				FirstName: profile.FirstName,
				LastName:  profile.LastName,
				ID:        localID,
			})
			ui.Name = &name
		}
		if profile.Avatar != nil && len(*profile.Avatar) > 0 {
			avatarData := *profile.Avatar
			avatarHash := sha256.Sum256(avatarData)
			ui.Avatar = &bridgev2.Avatar{
				ID: networkid.AvatarID(fmt.Sprintf("improfile:%s:%s", identifier, hex.EncodeToString(avatarHash[:8]))),
				Get: func(ctx context.Context) ([]byte, error) {
					return avatarData, nil
				},
			}
		}
		if ui.Name != nil {
			return ui, nil
		}
	}

	// Final fallback: format from identifier
	name := c.Main.Config.FormatDisplayname(identifierToDisplaynameParams(identifier))
	ui.Name = &name
	return ui, nil
}

func (c *IMClient) ResolveIdentifier(ctx context.Context, identifier string, createChat bool) (resp *bridgev2.ResolveIdentifierResponse, err error) {
	if c.client == nil {
		return nil, bridgev2.ErrNotLoggedIn
	}
	// ValidateTargets crosses into the identity-manager FFI path, which has
	// reachable panic sites upstream (identity_manager.rs:249/335/542/555).
	// This is a user-triggered call (start-chat flow), so a panic here
	// would crash the bridge on a normal user action. Convert to error.
	defer func() {
		if r := recover(); r != nil {
			c.UserLogin.Log.Error().Interface("panic", r).Str("identifier", identifier).
				Msg("ResolveIdentifier panicked in FFI path")
			resp = nil
			err = fmt.Errorf("identity lookup panicked: %v", r)
		}
	}()

	valid := c.client.ValidateTargets([]string{identifier}, c.handle)
	if len(valid) == 0 {
		return nil, fmt.Errorf("user not found on iMessage: %s", identifier)
	}

	userID := makeUserID(identifier)
	portalID := networkid.PortalKey{
		ID:       networkid.PortalID(identifier),
		Receiver: c.UserLogin.ID,
	}

	ghost, err := c.Main.Bridge.GetGhostByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get ghost: %w", err)
	}
	portal, err := c.Main.Bridge.GetPortalByKey(ctx, portalID)
	if err != nil {
		return nil, fmt.Errorf("failed to get portal: %w", err)
	}
	ghostInfo, err := c.GetUserInfo(ctx, ghost)
	if err != nil {
		return nil, err
	}

	return &bridgev2.ResolveIdentifierResponse{
		Ghost:    ghost,
		UserID:   userID,
		UserInfo: ghostInfo,
		Chat: &bridgev2.CreateChatResponse{
			Portal:    portal,
			PortalKey: portalID,
		},
	}, nil
}

// ============================================================================
// Backfill (CloudKit cache-backed)
// ============================================================================

// GetBackfillMaxBatchCount returns -1 (unlimited) so backward backfill
// processes all available messages in cloud_message without a batch cap.
// When the user caps max_initial_messages, FetchMessages short-circuits
// backward requests to return empty immediately.
func (c *IMClient) GetBackfillMaxBatchCount(_ context.Context, _ *bridgev2.Portal, _ *database.BackfillTask) int {
	return -1
}

type cloudBackfillCursor struct {
	TimestampMS int64  `json:"ts"`
	GUID        string `json:"g"`
}

func (c *IMClient) FetchMessages(ctx context.Context, params bridgev2.FetchMessagesParams) (*bridgev2.FetchMessagesResponse, error) {
	fetchStart := time.Now()
	log := zerolog.Ctx(ctx)

	// For forward backfill calls: ensure the bootstrap pending counter is
	// decremented on every return path. The normal path (with messages) sets
	// forwardDone=true and uses CompleteCallback to decrement AFTER bridgev2
	// delivers the batch to Matrix. All other paths (early return, empty
	// result, error) decrement here via defer — there is nothing to wait for.
	var forwardDone bool
	if params.Forward {
		defer func() {
			if !forwardDone {
				c.onForwardBackfillDone()
			}
		}()
	}

	// When the user has capped max_initial_messages, skip backward backfill
	// entirely. Forward backfill already delivered the capped N messages;
	// returning empty here marks the backward task as done immediately.
	// Applies to both chat.db and CloudKit paths.
	if !params.Forward && c.Main.Bridge.Config.Backfill.MaxInitialMessages < math.MaxInt32 {
		return &bridgev2.FetchMessagesResponse{HasMore: false, Forward: false}, nil
	}

	// Chat.db backfill path
	if c.Main.Config.UseChatDBBackfill() && c.chatDB != nil {
		return c.chatDB.FetchMessages(ctx, params, c)
	}

	if !c.Main.Config.UseCloudKitBackfill() || c.cloudStore == nil {
		log.Debug().Bool("forward", params.Forward).Bool("backfill_enabled", c.Main.Config.CloudKitBackfill).
			Msg("FetchMessages: backfill disabled or no cloud store, returning empty")
		return &bridgev2.FetchMessagesResponse{HasMore: false, Forward: params.Forward}, nil
	}

	count := params.Count
	if count <= 0 {
		count = 50
	}

	if params.Portal == nil || params.ThreadRoot != "" {
		log.Debug().Bool("forward", params.Forward).Msg("FetchMessages: nil portal or thread root, returning empty")
		return &bridgev2.FetchMessagesResponse{HasMore: false, Forward: params.Forward}, nil
	}
	portalID := string(params.Portal.ID)
	if portalID == "" {
		log.Debug().Bool("forward", params.Forward).Msg("FetchMessages: empty portal ID, returning empty")
		return &bridgev2.FetchMessagesResponse{HasMore: false, Forward: params.Forward}, nil
	}

	// Guard: if this portal was recently deleted, only backfill if there are
	// live (non-deleted) messages. Prevents backfilling old messages into
	// portals recreated by a genuinely new message.
	c.recentlyDeletedPortalsMu.RLock()
	_, isDeletedPortal := c.recentlyDeletedPortals[portalID]
	c.recentlyDeletedPortalsMu.RUnlock()
	if isDeletedPortal && c.cloudStore != nil {
		rows, err := c.cloudStore.listLatestMessages(context.Background(), portalID, 1)
		if err == nil && len(rows) == 0 {
			log.Info().Str("portal_id", portalID).Msg("FetchMessages: deleted portal with no live messages, returning empty")
			return &bridgev2.FetchMessagesResponse{HasMore: false, Forward: params.Forward}, nil
		}
	}

	// Look up the group display name for system message filtering.
	// Group rename system messages have text == display name but no attributedBody;
	// this lets cloudRowToBackfillMessages filter them with an AND condition.
	var groupDisplayName string
	if c.cloudStore != nil {
		groupDisplayName, _ = c.cloudStore.getDisplayNameByPortalID(ctx, portalID)
	}

	// Forward backfill: return messages for this portal in chronological order.
	// bridgev2 doForwardBackfill calls FetchMessages exactly once (no external
	// pagination), so we loop internally — fetching and converting in chunks of
	// forwardChunkSize to bound per-iteration memory. All messages are
	// accumulated and returned in a single response.
	const forwardChunkSize = 5000
	if params.Forward {
		// Acquire semaphore to limit concurrent forward backfills.
		// This prevents overwhelming CloudKit/Matrix with simultaneous
		// attachment downloads and uploads across many portals.
		// Use a select with ctx.Done() so we don't block the portal event
		// loop indefinitely when all slots are taken — that causes "Portal
		// event channel is still full" errors and dropped events.
		select {
		case c.forwardBackfillSem <- struct{}{}:
		case <-ctx.Done():
			log.Warn().Str("portal_id", portalID).Msg("Forward backfill: context cancelled while waiting for semaphore")
			return &bridgev2.FetchMessagesResponse{HasMore: false, Forward: true}, nil
		}
		defer func() { <-c.forwardBackfillSem }()
		log.Info().
			Str("portal_id", portalID).
			Int("count", count).
			Int("chunk_size", forwardChunkSize).
			Str("trigger", "portal_creation").
			Bool("has_anchor", params.AnchorMessage != nil).
			Msg("Forward backfill START")

		// Determine the starting anchor for the internal pagination loop.
		var cursorTS int64
		var cursorGUID string
		hasCursor := false
		if params.AnchorMessage != nil {
			cursorTS = params.AnchorMessage.Timestamp.UnixMilli()
			cursorGUID = string(params.AnchorMessage.ID)
			hasCursor = true
			log.Debug().
				Str("portal_id", portalID).
				Int64("anchor_ts", cursorTS).
				Str("anchor_guid", cursorGUID).
				Msg("Forward backfill: using anchor — fetching only newer messages")
		}

		var allRows []cloudMessageRow
		totalRows := 0
		chunk := 0
		remaining := count

		if !hasCursor {
			// No anchor: fetch the N most recent messages. listLatestMessages
			// selects the newest N (crash resilience — if interrupted, the
			// delivered messages are recent, not ancient). We reverse to
			// chronological (ASC) order before sending so the Matrix timeline
			// displays correctly.
			queryStart := time.Now()
			rows, queryErr := c.cloudStore.listLatestMessages(ctx, portalID, count)
			if queryErr != nil {
				log.Err(queryErr).Str("portal_id", portalID).Msg("Forward backfill: listLatestMessages FAILED")
				return nil, queryErr
			}
			for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
				rows[i], rows[j] = rows[j], rows[i]
			}
			c.preUploadChunkAttachments(ctx, rows, *log)
			allRows = rows
			totalRows = len(rows)
			chunk = 1
			log.Debug().
				Str("portal_id", portalID).
				Int("rows", totalRows).
				Dur("query_ms", time.Since(queryStart)).
				Msg("Forward backfill: fetched most recent messages")
		} else {
			// Anchor path (catchup): page forward from the anchor message.
			for remaining > 0 {
				chunkLimit := forwardChunkSize
				if chunkLimit > remaining {
					chunkLimit = remaining
				}

				queryStart := time.Now()
				rows, queryErr := c.cloudStore.listForwardMessages(ctx, portalID, cursorTS, cursorGUID, chunkLimit)
				if queryErr != nil {
					log.Err(queryErr).Str("portal_id", portalID).Int("chunk", chunk).Msg("Forward backfill: query FAILED")
					return nil, queryErr
				}
				if len(rows) == 0 {
					break
				}

				// Pre-upload any uncached attachments in this chunk in parallel
				// before conversion. This prevents the sequential conversion loop
				// from doing live CloudKit downloads (90s timeout each), which
				// would hang the portal event loop for hours on large portals.
				c.preUploadChunkAttachments(ctx, rows, *log)

				allRows = append(allRows, rows...)
				totalRows += len(rows)
				chunk++

				log.Debug().
					Str("portal_id", portalID).
					Int("chunk", chunk).
					Int("chunk_rows", len(rows)).
					Int("total_rows", totalRows).
					Dur("chunk_query_ms", time.Since(queryStart)).
					Msg("Forward backfill: chunk fetched")

				// Advance cursor to the last row in this chunk for the next iteration.
				lastRow := rows[len(rows)-1]
				cursorTS = lastRow.TimestampMS
				cursorGUID = lastRow.GUID
				hasCursor = true
				remaining -= len(rows)

				// If we got fewer rows than requested, there are no more.
				if len(rows) < chunkLimit {
					break
				}
			}
		}

		// Convert all rows with two-pass tapback resolution: reactions
		// targeting messages in this batch go into BackfillMessage.Reactions
		// (correct DAG ordering) instead of QueueRemoteEvent (end of DAG).
		allMessages := c.cloudRowsToBackfillMessages(ctx, allRows, groupDisplayName)

		if len(allMessages) == 0 {
			log.Debug().Str("portal_id", portalID).Msg("Forward backfill: no rows to process")
			// Use context.Background() — if the bridge is shutting down, ctx
			// may be cancelled but we still need to persist the flag.
			c.cloudStore.markForwardBackfillDone(context.Background(), portalID)
			return &bridgev2.FetchMessagesResponse{HasMore: false, Forward: true}, nil
		}

		log.Info().
			Str("portal_id", portalID).
			Int("db_rows", totalRows).
			Int("backfill_msgs", len(allMessages)).
			Int("chunks", chunk).
			Dur("total_ms", time.Since(fetchStart)).
			Msg("Forward backfill COMPLETE — all messages returned for portal creation")

		// Mark done so the outer defer doesn't double-decrement; the
		// CompleteCallback will call onForwardBackfillDone after bridgev2
		// delivers the messages to Matrix (preserving ordering: CloudKit
		// backfill is fully in Matrix before APNs buffer is flushed).
		forwardDone = true
		cloudStoreDone := c.cloudStore
		portalKey := params.Portal.PortalKey

		// Capture latest backfilled message for Receipt 2 target.
		// This ensures we mark the conversation as read up to the very
		// latest message, not just the most recently read outgoing message.
		lastMsg := allMessages[len(allMessages)-1]
		lastMsgID := lastMsg.ID
		lastMsgTS := lastMsg.Timestamp

		return &bridgev2.FetchMessagesResponse{
			Messages: allMessages,
			HasMore:  false,
			Forward:  true,
			// Don't set MarkRead here — MarkReadBy on the batch send creates
			// a homeserver-side read receipt with server time (≈now), which
			// Beeper clients display as "Read at [now]". Instead, rely on
			// the synthetic receipts below for correct timestamps.
			CompleteCallback: func() {
				// Compute read state BEFORE markForwardBackfillDone, which may
				// insert a synthetic cloud_chat row with is_filtered=0 default
				// that would defeat the "no chat row → unread" safeguard.
				readByMe, readErr := cloudStoreDone.getConversationReadByMe(context.Background(), portalID)

				cloudStoreDone.markForwardBackfillDone(context.Background(), portalID)

				// NOTE: Receipt 1 (ghost "they read my message") is intentionally
				// NOT sent during backfill. CloudKit's date_read_ms is unreliable:
				// real iPhone users have no data (shows "Sent" correctly), but
				// bridge users get wrong timestamps (shows "Seen at [wrong time]").
				// Real-time read receipts via APNs still work after backfill.

				// --- Receipt 2: Double puppet receipt ("I read their message") ---
				// Marks all non-filtered CloudKit conversations as read, since
				// they exist on the user's Apple devices and the user receives
				// push notifications for them. Filtered chats and portals
				// without cloud_chat metadata are left unread.
				// Targets the latest backfilled message to mark the entire
				// conversation as read, overwriting forceMarkRead's server-time
				// receipt via SetBeeperInboxState with correct BeeperReadExtra["ts"].
				if readErr == nil && readByMe {
					c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Receipt{
						EventMeta: simplevent.EventMeta{
							Type:      bridgev2.RemoteEventReadReceipt,
							PortalKey: portalKey,
							Sender: bridgev2.EventSender{
								IsFromMe:    true,
								SenderLogin: c.UserLogin.ID,
								Sender:      makeUserID(c.handle),
							},
							Timestamp: lastMsgTS,
						},
						LastTarget: lastMsgID,
						ReadUpTo:   lastMsgTS,
					})
					log.Info().
						Str("portal_id", portalID).
						Str("last_msg_id", string(lastMsgID)).
						Str("last_msg_ts", lastMsgTS.UTC().Format(time.RFC3339)).
						Msg("Queued double puppet read receipt (I read their message)")
				} else if readErr != nil {
					log.Warn().Err(readErr).Str("portal_id", portalID).Msg("Failed to check if conversation is read by me")
				}

				c.onForwardBackfillDone()
			},
		}, nil
	}

	// Backward backfill: triggered by the mautrix bridgev2 backfill queue
	// for portals with CanBackfill=true (set in GetChatInfo when cloud store
	// has messages for this portal). Paginates through older messages.
	cursorDesc := "none (initial page)"
	fetchCount := count + 1
	beforeTS := int64(0)
	beforeGUID := ""
	if params.Cursor != "" {
		cursor, err := decodeCloudBackfillCursor(params.Cursor)
		if err != nil {
			return nil, fmt.Errorf("invalid backfill cursor: %w", err)
		}
		beforeTS = cursor.TimestampMS
		beforeGUID = cursor.GUID
		cursorDesc = fmt.Sprintf("before ts=%d guid=%s", beforeTS, beforeGUID)
	} else if params.AnchorMessage != nil {
		beforeTS = params.AnchorMessage.Timestamp.UnixMilli()
		beforeGUID = string(params.AnchorMessage.ID)
		cursorDesc = fmt.Sprintf("anchor ts=%d id=%s", beforeTS, beforeGUID)
	}

	if beforeTS == 0 && beforeGUID == "" {
		// If forward backfill hasn't completed yet, don't permanently mark backward
		// as done — the anchor will appear once sendBatch finishes.
		// But if the portal has no messages at all, stop waiting — there's
		// nothing for forward backfill to anchor against, and deferring
		// creates an infinite retry loop.
		if !c.cloudStore.isForwardBackfillDone(ctx, portalID) {
			hasMessages, _ := c.cloudStore.hasPortalMessages(ctx, portalID)
			if hasMessages {
				log.Info().Str("portal_id", portalID).
					Msg("Backward backfill: no anchor yet, forward backfill still in progress — deferring")
				// Sleep before returning HasMore=true so the bridgev2 backfill
				// queue doesn't tight-loop on this task and steal scheduler
				// time from forward backfill (which we're waiting on). Each
				// tight-loop iteration was ~1s of pure no-op — with 30+
				// deferred portals, that's 30+ CPU-seconds per second burned
				// waiting for a state change that takes minutes to happen.
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(30 * time.Second):
				}
				return &bridgev2.FetchMessagesResponse{HasMore: true, Forward: false}, nil
			}
			log.Info().Str("portal_id", portalID).
				Msg("Backward backfill: no anchor and no messages — stopping (nothing to backfill)")
			return &bridgev2.FetchMessagesResponse{HasMore: false, Forward: false}, nil
		}
		// No anchor but forward backfill is done — this happens for recovered
		// portals where the room already exists but has no messages (e.g. chat
		// was deleted and recovered). Fetch the latest messages forward-style
		// so the portal gets populated.
		if c.cloudStore != nil {
			if hasMessages, _ := c.cloudStore.hasPortalMessages(ctx, portalID); hasMessages {
				log.Info().Str("portal_id", portalID).
					Msg("Backward backfill: no anchor but portal has messages — doing recovery backfill")
				rows, queryErr := c.cloudStore.listLatestMessages(ctx, portalID, count)
				if queryErr != nil {
					return nil, queryErr
				}
				if len(rows) > 0 {
					// Reverse to chronological order
					for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
						rows[i], rows[j] = rows[j], rows[i]
					}
					allMessages := c.cloudRowsToBackfillMessages(ctx, rows, groupDisplayName)
					log.Info().
						Str("portal_id", portalID).
						Int("db_rows", len(rows)).
						Int("backfill_msgs", len(allMessages)).
						Msg("Recovery backfill COMPLETE")
					return &bridgev2.FetchMessagesResponse{
						Messages: allMessages,
						HasMore:  false,
						Forward:  false,
					}, nil
				}
			}
		}
		log.Debug().Str("portal_id", portalID).
			Msg("Backward backfill: no anchor or cursor, nothing to paginate from")
		return &bridgev2.FetchMessagesResponse{HasMore: false, Forward: false}, nil
	}

	log.Info().
		Str("portal_id", portalID).
		Int("count", count).
		Str("cursor", cursorDesc).
		Str("trigger", "backfill_queue").
		Msg("Backward backfill START — paginating older messages")

	queryStart := time.Now()
	rows, err := c.cloudStore.listBackwardMessages(ctx, portalID, beforeTS, beforeGUID, fetchCount)
	if err != nil {
		log.Err(err).Str("portal_id", portalID).Dur("query_ms", time.Since(queryStart)).Msg("Backward backfill: query FAILED")
		return nil, err
	}
	queryElapsed := time.Since(queryStart)

	hasMore := false
	if len(rows) > count {
		hasMore = true
		rows = rows[:count]
	}
	reverseCloudMessageRows(rows)

	convertStart := time.Now()
	messages := c.cloudRowsToBackfillMessages(ctx, rows, groupDisplayName)
	convertElapsed := time.Since(convertStart)

	var nextCursor networkid.PaginationCursor
	if hasMore && len(rows) > 0 {
		cursor, cursorErr := encodeCloudBackfillCursor(cloudBackfillCursor{
			TimestampMS: rows[0].TimestampMS,
			GUID:        rows[0].GUID,
		})
		if cursorErr != nil {
			return nil, cursorErr
		}
		nextCursor = cursor
	}

	log.Info().
		Str("portal_id", portalID).
		Int("db_rows", len(rows)).
		Int("backfill_msgs", len(messages)).
		Bool("has_more", hasMore).
		Dur("query_ms", queryElapsed).
		Dur("convert_ms", convertElapsed).
		Dur("total_ms", time.Since(fetchStart)).
		Msg("Backward backfill COMPLETE — older messages returned")

	return &bridgev2.FetchMessagesResponse{
		Messages: messages,
		Cursor:   nextCursor,
		HasMore:  hasMore,
		Forward:  false,
	}, nil
}

// cloudRowsToBackfillMessages converts a batch of CloudKit rows into backfill
// messages, attaching tapback reactions to their target messages when possible.
// This two-pass approach ensures reactions appear in BackfillMessage.Reactions
// (correct DAG ordering) instead of being queued via QueueRemoteEvent (which
// places them at the end of the DAG, making the sidebar show old reactions).
// Tapback removes and tapbacks targeting messages outside this batch still
// fall back to QueueRemoteEvent.
func (c *IMClient) cloudRowsToBackfillMessages(ctx context.Context, rows []cloudMessageRow, groupDisplayName string) []*bridgev2.BackfillMessage {
	// Pass 1: convert regular messages, defer tapback rows.
	var messages []*bridgev2.BackfillMessage
	var tapbackRows []cloudMessageRow
	messageByGUID := make(map[string]*bridgev2.BackfillMessage)

	for _, row := range rows {
		if row.TapbackType != nil && *row.TapbackType >= 2000 {
			tapbackRows = append(tapbackRows, row)
			continue
		}
		converted := c.cloudRowToBackfillMessages(ctx, row, groupDisplayName)
		messages = append(messages, converted...)
		// Key by row GUID so tapbacks can find their target. A row may
		// produce multiple BackfillMessages (text + attachments); attach
		// the reaction to the first one (text, or first attachment).
		if len(converted) > 0 {
			messageByGUID[row.GUID] = converted[0]
		}
	}

	// Pass 2: resolve tapbacks — attach to target if in this batch,
	// otherwise fall back to QueueRemoteEvent.
	for _, row := range tapbackRows {
		sender := c.makeCloudSender(row)
		if sender.Sender == "" && !sender.IsFromMe {
			continue
		}
		sender = c.canonicalizeDMSender(networkid.PortalKey{ID: networkid.PortalID(row.PortalID)}, sender)

		tapbackType := *row.TapbackType
		isRemove := tapbackType >= 3000

		// Parse target GUID and balloon-part index from "p:N/GUID" format.
		targetGUID := row.TapbackTargetGUID
		bp := 0
		if parts := strings.SplitN(targetGUID, "/", 2); len(parts) == 2 {
			bp = parseBalloonPart(parts[0], "p:%d")
			targetGUID = parts[1]
		}
		if targetGUID == "" {
			continue
		}

		// Removes can't use BackfillReaction (framework only supports add).
		// Tapbacks targeting messages outside this batch also fall back.
		targetMsg, inBatch := messageByGUID[targetGUID]
		if !isRemove && inBatch {
			ts := time.UnixMilli(row.TimestampMS)
			idx := tapbackType - 2000
			emoji := tapbackTypeToEmoji(&idx, &row.TapbackEmoji)
			// Map balloon-part index to bridge part ID:
			// bp 0 = text body (nil TargetPart → first part),
			// bp >= 1 = attachment (att0, att1, …).
			var targetPart *networkid.PartID
			if bp >= 1 {
				p := networkid.PartID(fmt.Sprintf("att%d", bp-1))
				targetPart = &p
			}
			targetMsg.Reactions = append(targetMsg.Reactions, &bridgev2.BackfillReaction{
				Sender:     sender,
				Emoji:      emoji,
				Timestamp:  ts,
				TargetPart: targetPart,
			})
		} else {
			// Fall back to QueueRemoteEvent for removes and out-of-batch targets.
			c.cloudTapbackToBackfill(row, sender, time.UnixMilli(row.TimestampMS))
		}
	}

	return messages
}

// cloudReactionMinType is the lowest tapback_type that denotes a real reaction.
// Regular messages are stored with tapback_type NULL or 0; reactions occupy
// 2000-2006 (add) and 3000-3006 (remove). Every "is this a reaction?" decision
// goes through isCloudReactionRow so the NULL-vs-0 representation can't be
// mistaken for a reaction (which previously let the privacy scrubber skip every
// type-0 message — i.e. nearly all of them).
const cloudReactionMinType uint32 = 2000

func isCloudReactionRow(tapbackType *uint32) bool {
	return tapbackType != nil && *tapbackType >= cloudReactionMinType
}

func (c *IMClient) cloudRowToBackfillMessages(ctx context.Context, row cloudMessageRow, groupDisplayName string) []*bridgev2.BackfillMessage {
	// Privacy: a body_scrubbed regular message was already bridged to Matrix
	// (the scrubber requires a bridgev2 `message` row for the same GUID before
	// clearing text/subject/sender). Emitting here would produce empty-body
	// events. Skip — bridgev2 already has the message and would dedup anyway
	// under aggressive-dedup, plus this avoids the empty-body cliff for any
	// backfill path that lands on a scrubbed row whose bridgev2 counterpart got
	// purged. Restore-chat clears body_scrubbed before re-fetching from
	// CloudKit, so the user-initiated restore path is unaffected.
	//
	// Exception: scrubbed reactions (tapback_type >= 2000) fall through to
	// cloudTapbackToBackfill and render identically to master. scrubReactionText
	// (the only scrub touching a live reaction) clears text/subject only — it
	// preserves tapback_emoji — so custom-emoji reactions (type 6) keep their
	// emoji. The paths that DO null tapback_emoji all also set deleted=TRUE, and
	// every backfill reader filters deleted=FALSE, so an emoji-nulled reaction is
	// never re-rendered. Net: reaction rendering is unchanged from master.
	if row.BodyScrubbed && !isCloudReactionRow(row.TapbackType) {
		return nil
	}

	// Defense in depth for the restore-chat orphan case: a previously-
	// scrubbed row whose flag got cleared (clearBodyScrubByPortalID) but
	// whose CloudKit re-fetch returned no data (purged from CloudKit,
	// network failure) is now body_scrubbed=FALSE with text/attachments
	// still NULL. Without this skip cloudRowToBackfillMessages would
	// silently emit no events for it, bridgev2 would treat the empty
	// result as "no more history" and mark forward backfill done — losing
	// the row permanently from Matrix. Detected as: real-message shape
	// (HasBody=TRUE, not a reaction) but empty body+attachments after a
	// supposed restore re-population.
	if row.HasBody && !isCloudReactionRow(row.TapbackType) &&
		strings.TrimSpace(row.Text) == "" && row.Subject == "" && row.AttachmentsJSON == "" {
		return nil
	}

	sender := c.makeCloudSender(row)
	ts := time.UnixMilli(row.TimestampMS)

	// Skip messages with no resolvable sender. These are typically iMessage
	// system/notification records (group renames, participant changes, etc.)
	// stored in CloudKit without a sender field. Without this check, bridgev2
	// falls back to the bridge bot as the sender, producing spurious bot
	// messages in the backfilled timeline.
	if sender.Sender == "" && !sender.IsFromMe {
		return nil
	}
	sender = c.canonicalizeDMSender(networkid.PortalKey{ID: networkid.PortalID(row.PortalID)}, sender)

	// Skip system/service messages (group renames, participant changes, etc.).
	// Two complementary signals:
	//   !HasBody with no text/attachments: genuine system rows often have no
	//     attributedBody, no attachments, and an empty body. Restored Apple
	//     messages can legitimately arrive with has_body=FALSE but still carry
	//     text, so don't drop those.
	//   text==groupDisplayName: for older rows whose has_body defaulted to
	//     TRUE, the rename-notification text exactly matches the group name.
	// The startup DB cleanup (ensureSchema) hard-deletes matching rows so
	// this filter is a second line of defence for any that slipped through.
	isSystemByHasBody := !row.HasBody && strings.TrimSpace(row.Text) == "" && row.AttachmentsJSON == "" && row.TapbackType == nil
	isSystemByName := row.Text != "" && groupDisplayName != "" && row.Text == groupDisplayName && row.AttachmentsJSON == "" && row.TapbackType == nil
	if isSystemByHasBody || isSystemByName {
		return nil
	}

	// Tapback/reaction: return as a reaction event, not a text message.
	if row.TapbackType != nil && *row.TapbackType >= 2000 {
		return c.cloudTapbackToBackfill(row, sender, ts)
	}

	var messages []*bridgev2.BackfillMessage

	// Text message — trim OBJ placeholders before building body.
	body := strings.Trim(row.Text, "\ufffc \n")
	var formattedBody string
	if row.Subject != "" {
		if body != "" {
			formattedBody = fmt.Sprintf("<strong>%s</strong><br>%s", html.EscapeString(row.Subject), html.EscapeString(body))
			body = fmt.Sprintf("**%s**\n%s", row.Subject, body)
		} else {
			body = row.Subject
		}
	}
	hasText := strings.TrimSpace(body) != ""
	if hasText {
		textContent := &event.MessageEventContent{
			MsgType: event.MsgText,
			Body:    body,
		}
		if formattedBody != "" {
			textContent.Format = event.FormatHTML
			textContent.FormattedBody = formattedBody
		}
		if c.Main.Config.URLPreviewsInBackfill {
			if detectedURL := urlRegex.FindString(row.Text); detectedURL != "" {
				textContent.BeeperLinkPreviews = []*event.BeeperLinkPreview{
					fetchURLPreview(ctx, c.Main.Bridge, c.Main.Bridge.Bot, "", detectedURL),
				}
			}
		}
		messages = append(messages, &bridgev2.BackfillMessage{
			Sender:    sender,
			ID:        makeMessageID(row.GUID),
			Timestamp: ts,
			ConvertedMessage: &bridgev2.ConvertedMessage{
				Parts: []*bridgev2.ConvertedMessagePart{{
					Type:    event.EventMessage,
					Content: textContent,
				}},
			},
		})
	}

	// Attachments: downloadAndUploadAttachment checks attachmentContentCache first,
	// so this is a cheap cache lookup when preUploadCloudAttachments has run.
	attMessages := c.cloudAttachmentsToBackfill(ctx, row, sender, ts, hasText)
	messages = append(messages, attMessages...)

	// Attachment-only message where all downloads failed: produce a notice
	// placeholder so the message isn't silently dropped. Without this, the
	// message never enters the Matrix `message` table but stays in
	// cloud_message, so it's permanently lost — retries hit the same failure
	// and the user never knows a message existed.
	if len(messages) == 0 && row.AttachmentsJSON != "" {
		messages = append(messages, &bridgev2.BackfillMessage{
			Sender:    sender,
			ID:        makeMessageID(row.GUID),
			Timestamp: ts,
			ConvertedMessage: &bridgev2.ConvertedMessage{
				Parts: []*bridgev2.ConvertedMessagePart{{
					Type: event.EventMessage,
					Content: &event.MessageEventContent{
						MsgType: event.MsgNotice,
						Body:    "Attachment could not be downloaded from iCloud.",
					},
				}},
			},
		})
	}

	// Reply: apply the same target to every part this row produced (text,
	// attachment(s), notice placeholder), mirroring the live convertMessage
	// path. Reactions/tapbacks already returned above, so they're unaffected.
	// bridgev2 resolves the target via deterministic event IDs during backfill,
	// so a same-batch reply target renders even before its DB row is inserted.
	if row.ReplyToGUID != "" {
		replyTo := chatDBReplyTarget(row.ReplyToGUID, parseBalloonPart(row.ReplyToPart, "%d:"))
		for _, m := range messages {
			if m.ConvertedMessage != nil {
				m.ConvertedMessage.ReplyTo = replyTo
			}
		}
	}

	return messages
}

func (c *IMClient) makeCloudSender(row cloudMessageRow) bridgev2.EventSender {
	if row.IsFromMe {
		return bridgev2.EventSender{
			IsFromMe:    true,
			SenderLogin: c.UserLogin.ID,
			Sender:      makeUserID(c.handle),
		}
	}
	normalizedSender := normalizeIdentifierForPortalID(row.Sender)
	if normalizedSender == "" {
		portalID := strings.TrimSpace(row.PortalID)
		if portalID != "" && !strings.HasPrefix(portalID, "gid:") && !strings.Contains(portalID, ",") {
			normalizedSender = portalID
		}
	}
	if normalizedSender == "" {
		normalizedSender = row.Sender
	}
	return bridgev2.EventSender{Sender: makeUserID(normalizedSender)}
}

// cloudTapbackToBackfill converts a CloudKit reaction record to a backfill reaction event.
func (c *IMClient) cloudTapbackToBackfill(row cloudMessageRow, sender bridgev2.EventSender, ts time.Time) []*bridgev2.BackfillMessage {
	tapbackType := *row.TapbackType
	isRemove := tapbackType >= 3000
	idx := tapbackType - 2000
	if isRemove {
		idx = tapbackType - 3000
	}
	emoji := tapbackTypeToEmoji(&idx, &row.TapbackEmoji)

	// Parse target GUID from "p:N/GUID" format, preserving the part index.
	targetGUID := row.TapbackTargetGUID
	bp := 0
	if parts := strings.SplitN(targetGUID, "/", 2); len(parts) == 2 {
		bp = parseBalloonPart(parts[0], "p:%d")
		targetGUID = parts[1]
	}
	if targetGUID == "" {
		return nil
	}
	targetMsgID := c.resolveTapbackTargetID(targetGUID, bp)

	evtType := bridgev2.RemoteEventReaction
	if isRemove {
		evtType = bridgev2.RemoteEventReactionRemove
	}

	// Pre-filter: skip reactions already in the bridge DB to avoid flooding
	// the portal event channel with no-op duplicates on every restart.
	// CloudKit re-imports all reactions on bootstrap, but the bridge framework's
	// dedup (handleRemoteReaction) processes them sequentially through the
	// portal event channel — 2000+ duplicate reactions can block real messages
	// for minutes. Checking the reaction table here is a cheap PK lookup that
	// prevents the queue from filling with known duplicates.
	if !isRemove {
		existing, err := c.Main.Bridge.DB.Reaction.GetByIDWithoutMessagePart(
			context.Background(), c.UserLogin.ID, targetMsgID, sender.Sender, "",
		)
		if err == nil && existing != nil {
			return nil
		}
	}

	// Reactions are sent as remote events, not backfill messages.
	// We queue them so the bridge handles dedup and target resolution.
	portalKey := networkid.PortalKey{
		ID:       networkid.PortalID(row.PortalID),
		Receiver: c.UserLogin.ID,
	}
	c.UserLogin.QueueRemoteEvent(&simplevent.Reaction{
		EventMeta: simplevent.EventMeta{
			Type:      evtType,
			PortalKey: portalKey,
			Sender:    sender,
			Timestamp: ts,
		},
		TargetMessage: targetMsgID,
		Emoji:         emoji,
	})
	return nil
}

// isPluginPayloadAttachment reports whether the row is a rich-link plugin
// payload sideband (the binary plist Apple stores alongside a URL-bubble
// message). Filename-based because that's the only signal CloudKit reliably
// preserves; the plist always uses the .pluginPayloadAttachment extension.
func isPluginPayloadAttachment(att cloudAttachmentRow) bool {
	return strings.HasSuffix(att.Filename, ".pluginPayloadAttachment")
}

// cloudAttachmentResult holds the result of a concurrent attachment download+upload.
type cloudAttachmentResult struct {
	Index   int
	Message *bridgev2.BackfillMessage
}

// cloudAttachmentsToBackfill downloads CloudKit attachments, uploads them to
// the Matrix media repo, and returns backfill messages with media URLs set.
// Downloads and uploads run concurrently (up to 4 at a time) for speed.
func (c *IMClient) cloudAttachmentsToBackfill(ctx context.Context, row cloudMessageRow, sender bridgev2.EventSender, ts time.Time, hasText bool) []*bridgev2.BackfillMessage {
	if row.AttachmentsJSON == "" {
		return nil
	}
	var atts []cloudAttachmentRow
	if err := json.Unmarshal([]byte(row.AttachmentsJSON), &atts); err != nil {
		c.Main.Bridge.Log.Warn().Err(err).Str("guid", row.GUID).
			Msg("Failed to unmarshal attachment JSON, skipping attachments for this message")
		return nil
	}

	// Filter to downloadable attachments.
	type indexedAtt struct {
		index int
		att   cloudAttachmentRow
	}
	var downloadable []indexedAtt
	for i, att := range atts {
		if att.RecordName == "" {
			continue
		}
		// Skip rich-link plugin payload sidebands. The URL itself is in the
		// message text and the preview card renders from it; bridging the
		// plist as a file is noise. We deliberately do NOT filter on
		// HideAttachment broadly because Live Photo MOV companions also
		// carry that flag and we intentionally bridge them.
		if isPluginPayloadAttachment(att) {
			continue
		}
		downloadable = append(downloadable, indexedAtt{index: i, att: att})
	}

	// Live Photo handling: when HasAvid is true on an attachment, the download
	// step will fetch both the lqa (HEIC still) and avid (video) from the same
	// CloudKit record and return two BackfillMessages.
	if len(downloadable) == 0 {
		return nil
	}
	// For a single attachment, skip goroutine overhead.
	if len(downloadable) == 1 {
		return c.downloadAndUploadAttachment(ctx, row, sender, ts, hasText, downloadable[0].index, downloadable[0].att)
	}

	// Concurrent download+upload with bounded parallelism.
	const maxParallel = 4
	sem := make(chan struct{}, maxParallel)
	results := make(chan cloudAttachmentResult, len(downloadable))
	var wg sync.WaitGroup

	for _, da := range downloadable {
		wg.Add(1)
		go func(idx int, att cloudAttachmentRow) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					log.Error().Any("panic", r).Str("stack", string(debug.Stack())).Msg("Recovered panic in attachment download goroutine")
				}
			}()
			sem <- struct{}{}
			defer func() { <-sem }()
			// Bound resident attachment bytes against the shared process-wide
			// budget (see preUploadCloudAttachments / attachmentByteSem).
			weight := clampAttachmentWeight(att.FileSize)
			if err := attachmentByteSem.Acquire(ctx, weight); err != nil {
				return // ctx cancelled
			}
			defer attachmentByteSem.Release(weight)
			msgs := c.downloadAndUploadAttachment(ctx, row, sender, ts, hasText, idx, att)
			for _, m := range msgs {
				results <- cloudAttachmentResult{Index: idx, Message: m}
			}
		}(da.index, da.att)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results and sort by original index for deterministic ordering.
	var collected []cloudAttachmentResult
	for r := range results {
		collected = append(collected, r)
	}
	sort.Slice(collected, func(i, j int) bool {
		return collected[i].Index < collected[j].Index
	})

	messages := make([]*bridgev2.BackfillMessage, 0, len(collected))
	for _, r := range collected {
		messages = append(messages, r.Message)
	}
	return messages
}

// safeCloudDownloadAttachment wraps the FFI call with panic recovery and a
// 90-second timeout. When Rust stalls on a network hang or an unrecognised
// attachment format the goroutine would otherwise block forever; the timeout
// frees the semaphore slot so other downloads can proceed. The inner goroutine
// is leaked until Rust eventually unblocks, but that is bounded by
// attachmentMaxParallelDownloads and is temporary.
func safeCloudDownloadAttachment(client *rustpushgo.Client, recordName string) ([]byte, error) {
	type dlResult struct {
		data []byte
		err  error
	}
	ch := make(chan dlResult, 1)
	go func() {
		var res dlResult
		defer func() {
			if r := recover(); r != nil {
				stack := string(debug.Stack())
				log.Error().Str("ffi_method", "CloudDownloadAttachment").
					Str("record_name", recordName).
					Str("stack", stack).
					Msgf("FFI panic recovered: %v", r)
				res = dlResult{err: fmt.Errorf("FFI panic in CloudDownloadAttachment: %v", r)}
			}
			ch <- res
		}()
		d, e := client.CloudDownloadAttachment(recordName)
		res = dlResult{data: d, err: e}
	}()
	select {
	case res := <-ch:
		return res.data, res.err
	case <-time.After(90 * time.Second):
		log.Error().Str("ffi_method", "CloudDownloadAttachment").
			Str("record_name", recordName).
			Msg("CloudDownloadAttachment timed out after 90s — inner goroutine leaked until FFI unblocks")
		return nil, fmt.Errorf("CloudDownloadAttachment timed out after 90s")
	}
}

// safeCloudDownloadAttachmentAvid wraps the avid FFI call with the same
// panic recovery and timeout as safeCloudDownloadAttachment.
func safeCloudDownloadAttachmentAvid(client *rustpushgo.Client, recordName string) ([]byte, error) {
	type dlResult struct {
		data []byte
		err  error
	}
	ch := make(chan dlResult, 1)
	go func() {
		var res dlResult
		defer func() {
			if r := recover(); r != nil {
				stack := string(debug.Stack())
				log.Error().Str("ffi_method", "CloudDownloadAttachmentAvid").
					Str("record_name", recordName).
					Str("stack", stack).
					Msgf("FFI panic recovered: %v", r)
				res = dlResult{err: fmt.Errorf("FFI panic in CloudDownloadAttachmentAvid: %v", r)}
			}
			ch <- res
		}()
		d, e := client.CloudDownloadAttachmentAvid(recordName)
		res = dlResult{data: d, err: e}
	}()
	select {
	case res := <-ch:
		return res.data, res.err
	case <-time.After(90 * time.Second):
		log.Error().Str("ffi_method", "CloudDownloadAttachmentAvid").
			Str("record_name", recordName).
			Msg("CloudDownloadAttachmentAvid timed out after 90s")
		return nil, fmt.Errorf("CloudDownloadAttachmentAvid timed out after 90s")
	}
}

// defaultMaxAttachmentSizeMB is the fallback ceiling when max_attachment_size_mb
// is unset or invalid. 100 MB matches Beeper's upload limit and is safe on small
// hosts. See the config doc (IMConfig.MaxAttachmentSizeMB) for the full rationale.
const defaultMaxAttachmentSizeMB = 100

// maxAttachmentBytes is the configured attachment-size ceiling in bytes.
// Attachments larger than this are skipped outright — never downloaded,
// transcoded, or uploaded. The homeserver rejects anything over its upload cap,
// so bridging it just burns a download buffer (in Rust, outside the Go heap), a
// doomed transcode, and disk for a guaranteed rejection — and a multi-GB blob
// can wedge a small host. Operators with a homeserver that accepts larger
// uploads (and the RAM to spare) can raise max_attachment_size_mb.
func (c *IMClient) maxAttachmentBytes() int64 {
	mb := c.Main.Config.MaxAttachmentSizeMB
	if mb <= 0 {
		mb = defaultMaxAttachmentSizeMB
	}
	return int64(mb) * 1024 * 1024
}

// recoverAttachmentSize returns an attachment's real byte size from its
// reported file_size. CloudKit stores file_size as a wrapped i32, so a blob in
// [2 GiB, 4 GiB) surfaces as a NEGATIVE value; add 2^32 to recover it. This lets
// the size gate compare correctly against any configured limit, including ones
// raised above 2 GiB.
func recoverAttachmentSize(fileSize int64) int64 {
	if fileSize < 0 {
		return fileSize + (1 << 32)
	}
	return fileSize
}

// oversizedDownloadTimeout bounds the to-file download of a multi-GB blob.
// These legitimately take minutes over CloudKit, so it's far longer than the
// in-memory path's 90s.
const oversizedDownloadTimeout = 10 * time.Minute

// minFreeDiskBytes is the free-space floor for downloading attachments to disk.
// Below this, downloadAttachmentToTempFile defers the attachment (records a
// retriable failure) instead of writing — so a burst of large concurrent
// downloads can never fill the disk; it paces itself as in-flight temp files
// finish uploading and get cleaned up. Sized to comfortably hold one worst-case
// download plus its transcode output (input + MP4) with room to spare.
const minFreeDiskBytes int64 = 6 << 30 // 6 GiB

// safeCloudDownloadAttachmentToFile wraps the to-file FFI download with the same
// panic recovery as safeCloudDownloadAttachment and a generous timeout. rustpush
// writes the blob to destPath (never returning it over the i32-limited FFI) and
// returns the byte count.
func safeCloudDownloadAttachmentToFile(client *rustpushgo.Client, recordName, destPath string) (uint64, error) {
	type dlResult struct {
		n   uint64
		err error
	}
	ch := make(chan dlResult, 1)
	go func() {
		var res dlResult
		defer func() {
			if r := recover(); r != nil {
				stack := string(debug.Stack())
				log.Error().Str("ffi_method", "CloudDownloadAttachmentToFile").
					Str("record_name", recordName).
					Str("stack", stack).
					Msgf("FFI panic recovered: %v", r)
				res = dlResult{err: fmt.Errorf("FFI panic in CloudDownloadAttachmentToFile: %v", r)}
			}
			ch <- res
		}()
		n, e := client.CloudDownloadAttachmentToFile(recordName, destPath)
		res = dlResult{n: n, err: e}
	}()
	select {
	case res := <-ch:
		return res.n, res.err
	case <-time.After(oversizedDownloadTimeout):
		log.Error().Str("ffi_method", "CloudDownloadAttachmentToFile").
			Str("record_name", recordName).
			Msg("CloudDownloadAttachmentToFile timed out")
		return 0, fmt.Errorf("CloudDownloadAttachmentToFile timed out after %s", oversizedDownloadTimeout)
	}
}

// downloadAttachmentToTempFile downloads an attachment to a fresh temp file via
// the to-file FFI. This is the single, uniform download path: it never lowers a
// Vec over uniffi's i32-capped RustBuffer (so no "buffer too large" panic), it
// doesn't rely on the unreliable wrapped-i32 file_size to predict size, and peak
// memory is one copy rather than the Rust Vec and a Go []byte at once. Returns
// the path and real byte count; the caller owns the file and must remove it.
// Defers (returns an error) when free disk is below minFreeDiskBytes so a burst
// of concurrent large downloads can't fill the disk — it paces itself as
// in-flight temp files finish uploading and get cleaned up.
func (c *IMClient) downloadAttachmentToTempFile(recordName string) (string, int64, error) {
	if free, ok := freeDiskBytes(os.TempDir()); ok && free < uint64(minFreeDiskBytes) {
		return "", 0, fmt.Errorf("low disk space (%d MiB free < %d MiB floor), deferring attachment",
			free>>20, minFreeDiskBytes>>20)
	}
	tmp, err := os.CreateTemp("", "mautrix-cloud-att-*")
	if err != nil {
		return "", 0, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	n, err := safeCloudDownloadAttachmentToFile(c.client, recordName, tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", 0, err
	}
	return tmpPath, int64(n), nil
}

// downloadAndUploadAttachment handles a single attachment: download from CloudKit,
// upload to Matrix, return as a backfill message.
func (c *IMClient) downloadAndUploadAttachment(
	ctx context.Context,
	row cloudMessageRow,
	sender bridgev2.EventSender,
	ts time.Time,
	hasText bool,
	i int,
	att cloudAttachmentRow,
) []*bridgev2.BackfillMessage {
	log := c.Main.Bridge.Log.With().Str("component", "cloud_backfill").Logger()
	intent := c.Main.Bridge.Bot

	attID := row.GUID
	if i > 0 || hasText {
		attID = fmt.Sprintf("%s_att%d", row.GUID, i)
	}

	// Skip over-limit attachments before doing ANY work. A blob bigger than the
	// configured ceiling (max_attachment_size_mb) can't be uploaded (the
	// homeserver rejects it), so downloading + transcoding it is pure waste —
	// and a multi-GB download wedges small hosts. file_size is a wrapped i32, so
	// recoverAttachmentSize un-wraps the negative ones before comparing. Not
	// recorded as a retriable failure — it's permanently too large.
	maxAttBytes := c.maxAttachmentBytes()
	if recoverAttachmentSize(att.FileSize) > maxAttBytes {
		log.Info().
			Str("guid", row.GUID).
			Str("att_guid", att.GUID).
			Str("record_name", att.RecordName).
			Int64("file_size", att.FileSize).
			Int64("max_bytes", maxAttBytes).
			Msg("Skipping over-limit attachment — too large to bridge (not downloaded)")
		return nil
	}

	// Permanently tombstoned: CloudKit no longer serves this asset (Apple aged
	// out the MMCS blob). Skip without touching CloudKit — re-attempting it just
	// re-fails. Not recorded as a retriable failure; it's gone for good.
	if c.isDeadAttachment(att.RecordName) {
		log.Debug().
			Str("guid", row.GUID).
			Str("att_guid", att.GUID).
			Str("record_name", att.RecordName).
			Msg("Skipping tombstoned attachment — CloudKit no longer serves it")
		return nil
	}

	// Cache hit: preUploadCloudAttachments already downloaded and uploaded this
	// attachment in the cloud sync goroutine. Return immediately without touching
	// CloudKit, keeping the portal event loop unblocked.
	// NOTE: Skip cache for non-MP4 videos that need transcoding, and for
	// Live Photo HasAvid records (old cache entries only have one part, not
	// both still+video). Regular videos with HasAvid use the cache normally
	// since we only bridge the lqa (the video itself), not the avid duplicate.
	isLivePhoto := att.HasAvid && !strings.HasPrefix(att.MimeType, "video/")
	if cached, ok := c.attachmentContentCache.Load(att.RecordName); ok && !isLivePhoto {
		cachedContent := cached.(*event.MessageEventContent)
		if cachedContent.Info != nil && cachedContent.Info.MimeType == "video/quicktime" {
			// Stale cache entry — video needs transcoding. Fall through to re-download.
			c.attachmentContentCache.Delete(att.RecordName)
		} else {
			return []*bridgev2.BackfillMessage{{
				Sender:    sender,
				ID:        makeMessageID(attID),
				Timestamp: ts,
				ConvertedMessage: &bridgev2.ConvertedMessage{
					Parts: []*bridgev2.ConvertedMessagePart{{
						Type:    event.EventMessage,
						Content: cachedContent,
					}},
				},
			}}
		}
	}

	// Always download the lqa to a temp file rather than returning the bytes
	// over the FFI. One uniform path: it never lowers a Vec across uniffi's
	// i32-capped RustBuffer (no "buffer too large" panic), it doesn't depend on
	// the unreliable wrapped-i32 file_size to guess size up front, and it's
	// disk-space guarded. We learn the real size from the file, then decide.
	tmpPath, n, err := c.downloadAttachmentToTempFile(att.RecordName)
	if err != nil {
		fe := c.recordAttachmentFailure(att.RecordName, err.Error())
		log.Warn().Err(err).
			Str("guid", row.GUID).
			Str("att_guid", att.GUID).
			Str("record_name", att.RecordName).
			Int("attempt", fe.retries).
			Msg("Failed to download CloudKit attachment, skipping")
		return nil
	}
	defer func() { _ = os.Remove(tmpPath) }()
	if n == 0 {
		c.recordAttachmentFailure(att.RecordName, "empty data")
		log.Debug().Str("guid", row.GUID).Str("record_name", att.RecordName).
			Msg("CloudKit attachment returned empty data")
		return nil
	}

	// Backstop the size gate: file_size is a wrapped i32, so a blob in the
	// [4 GiB, ~4.4 GiB) window wraps to a small POSITIVE value that slips the
	// pre-download check. Now that we know the real size, drop anything over the
	// configured ceiling before reading it into memory or transcoding it — it
	// can't be uploaded and would just spike RAM for a guaranteed rejection.
	if n > maxAttBytes {
		log.Info().
			Str("guid", row.GUID).
			Str("att_guid", att.GUID).
			Str("record_name", att.RecordName).
			Int64("bytes", n).
			Int64("max_bytes", maxAttBytes).
			Msg("Skipping over-limit attachment — too large to bridge (downloaded size)")
		return nil
	}

	// Small enough: read it back for the normal in-memory pipeline.
	data, err := os.ReadFile(tmpPath)
	if err != nil {
		fe := c.recordAttachmentFailure(att.RecordName, err.Error())
		log.Warn().Err(err).Str("guid", row.GUID).Str("record_name", att.RecordName).
			Int("attempt", fe.retries).
			Msg("Failed to read downloaded attachment, skipping")
		return nil
	}
	if len(data) == 0 {
		fe := c.recordAttachmentFailure(att.RecordName, "empty data")
		log.Debug().Str("guid", row.GUID).Str("record_name", att.RecordName).
			Int("attempt", fe.retries).
			Msg("CloudKit attachment returned empty data")
		return nil
	}

	mimeType := att.MimeType
	fileName := att.Filename
	if mimeType == "" {
		mimeType = utiToMIME(att.UTIType)
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	if fileName == "" {
		fileName = "attachment"
	}

	// Convert CAF Opus voice messages to OGG Opus for Matrix clients
	var durationMs int
	if att.UTIType == "com.apple.coreaudio-format" || mimeType == "audio/x-caf" {
		data, mimeType, fileName, durationMs = convertAudioForMatrix(data, mimeType, fileName)
	}

	// Remux/transcode non-MP4 videos to MP4 for broad Matrix client compatibility.
	// Goes through transcodeToMP4 so this CloudKit-backfill path shares the same
	// process-wide serialization and retry as the live and chat.db paths — the
	// pre-upload runs up to attachmentMaxParallelDownloads of these goroutines at
	// once, so without the shared semaphore a low-memory host could have several
	// ffmpeg re-encodes running simultaneously and OOM.
	if c.Main.Config.VideoTranscoding && ffmpeg.Supported() && strings.HasPrefix(mimeType, "video/") && mimeType != "video/mp4" {
		origMime := mimeType
		origSize := len(data)
		converted, method, convertErr := transcodeToMP4(ctx, &log, data, mimeType)
		if convertErr != nil {
			log.Warn().Err(convertErr).Str("guid", row.GUID).Str("original_mime", origMime).
				Msg("FFmpeg video conversion failed after retries, uploading original")
		} else {
			log.Info().Str("guid", row.GUID).Str("original_mime", origMime).
				Str("method", method).Int("original_bytes", origSize).Int("converted_bytes", len(converted)).
				Msg("Video transcoded to MP4")
			data = converted
			mimeType = "video/mp4"
			fileName = strings.TrimSuffix(fileName, filepath.Ext(fileName)) + ".mp4"
		}
	}

	// Convert HEIC/HEIF images to JPEG since most Matrix clients can't display HEIC.
	var heicImg image.Image
	data, mimeType, fileName, heicImg = maybeConvertHEIC(&log, data, mimeType, fileName, c.Main.Config.HEICJPEGQuality, c.Main.Config.HEICConversion)

	// Convert non-JPEG images to JPEG and extract dimensions/thumbnail
	var imgWidth, imgHeight int
	var thumbData []byte
	var thumbW, thumbH int
	if heicImg != nil {
		// Use the already-decoded image from HEIC conversion
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

	msgType := mimeToMsgType(mimeType)
	content := &event.MessageEventContent{
		MsgType: msgType,
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

	url, encFile, uploadErr := intent.UploadMedia(ctx, "", data, fileName, mimeType)
	if uploadErr != nil {
		fe := c.recordAttachmentFailure(att.RecordName, uploadErr.Error())
		log.Warn().Err(uploadErr).
			Str("guid", row.GUID).
			Str("att_guid", att.GUID).
			Int("attempt", fe.retries).
			Msg("Failed to upload attachment to Matrix, skipping")
		return nil
	}
	if encFile != nil {
		content.File = encFile
	} else {
		content.URL = url
	}

	if thumbData != nil {
		thumbURL, thumbEnc, thumbErr := intent.UploadMedia(ctx, "", thumbData, "thumbnail.jpg", "image/jpeg")
		if thumbErr != nil {
			log.Warn().Err(thumbErr).Str("record_name", att.RecordName).
				Msg("Failed to upload attachment thumbnail")
		} else {
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

	// Populate the in-memory cache so any future backfill call for the same
	// attachment returns instantly without re-downloading from CloudKit.
	c.attachmentContentCache.Store(att.RecordName, content)
	// Clear any prior failure tracking — this attachment succeeded.
	c.failedAttachments.Delete(att.RecordName)
	// Persist the mxc URI to SQLite so the cache survives bridge restarts.
	// Future pre-upload passes load this at startup and skip re-downloading.
	if c.cloudStore != nil {
		if jsonBytes, err := json.Marshal(content); err == nil {
			c.cloudStore.saveAttachmentCacheEntry(ctx, att.RecordName, jsonBytes)
		}
	}

	parts := make([]*bridgev2.ConvertedMessagePart, 0, 2)
	if isVCardAttachment(mimeType, fileName, att.UTIType) {
		if vcardPreview := makeVCardPreviewContent(data); vcardPreview != nil {
			parts = append(parts, &bridgev2.ConvertedMessagePart{
				Type:    event.EventMessage,
				Content: vcardPreview,
			})
		}
	}
	parts = append(parts, &bridgev2.ConvertedMessagePart{
		Type:    event.EventMessage,
		Content: content,
	})

	messages := []*bridgev2.BackfillMessage{{
		Sender:    sender,
		ID:        makeMessageID(attID),
		Timestamp: ts,
		ConvertedMessage: &bridgev2.ConvertedMessage{
			Parts: parts,
		},
	}}

	// Live Photo: if this attachment has an avid (video) asset, also download
	// and bridge the video so recipients see both the still and the motion.
	// Skip when the lqa itself is already a video — that means this is a regular
	// video attachment (not a Live Photo), and CloudKit stores the same video in
	// both lqa and avid fields. Bridging both would produce duplicates.
	if att.HasAvid && !strings.HasPrefix(mimeType, "video/") {
		avidData, avidErr := safeCloudDownloadAttachmentAvid(c.client, att.RecordName)
		if avidErr != nil || len(avidData) == 0 {
			log.Warn().Err(avidErr).Str("guid", row.GUID).Str("record_name", att.RecordName).
				Msg("Live Photo avid download failed, bridging still only")
			return messages
		}
		avidMime := "video/quicktime"
		avidFileName := "livephoto.MOV"
		if fileName != "" {
			base := filenameBase(fileName)
			if base != "" {
				avidFileName = base + ".MOV"
			}
		}

		// Remux/transcode the avid video if enabled. Shares transcodeToMP4's
		// serialization + retry with the other paths (see the lqa branch above).
		if c.Main.Config.VideoTranscoding && ffmpeg.Supported() {
			origSize := len(avidData)
			converted, method, convertErr := transcodeToMP4(ctx, &log, avidData, avidMime)
			if convertErr != nil {
				log.Warn().Err(convertErr).Str("guid", row.GUID).
					Msg("Live Photo avid ffmpeg conversion failed after retries, uploading original")
			} else {
				log.Info().Str("guid", row.GUID).
					Str("method", method).Int("original_bytes", origSize).Int("converted_bytes", len(converted)).
					Msg("Live Photo avid transcoded to MP4")
				avidData = converted
				avidMime = "video/mp4"
				avidFileName = strings.TrimSuffix(avidFileName, filepath.Ext(avidFileName)) + ".mp4"
			}
		}

		avidMsgType := mimeToMsgType(avidMime)
		avidContent := &event.MessageEventContent{
			MsgType: avidMsgType,
			Body:    avidFileName,
			Info: &event.FileInfo{
				MimeType: avidMime,
				Size:     len(avidData),
			},
		}
		avidURL, avidEnc, avidUploadErr := intent.UploadMedia(ctx, "", avidData, avidFileName, avidMime)
		if avidUploadErr != nil {
			log.Warn().Err(avidUploadErr).Str("guid", row.GUID).
				Msg("Live Photo avid upload failed, bridging still only")
			return messages
		}
		if avidEnc != nil {
			avidContent.File = avidEnc
		} else {
			avidContent.URL = avidURL
		}

		log.Info().Str("guid", row.GUID).Str("record_name", att.RecordName).
			Int("bytes", len(avidData)).
			Msg("Live Photo: bridging both HEIC still and avid video")

		messages = append(messages, &bridgev2.BackfillMessage{
			Sender:    sender,
			ID:        makeMessageID(attID + "_avid"),
			Timestamp: ts,
			ConvertedMessage: &bridgev2.ConvertedMessage{
				Parts: []*bridgev2.ConvertedMessagePart{{
					Type:    event.EventMessage,
					Content: avidContent,
				}},
			},
		})
	}

	return messages
}

const (
	// attachmentMaxParallelDownloads caps the number of concurrent attachment
	// pipelines during pre-upload — each goroutine runs a full CloudKit
	// download → transcode → Matrix upload, so this bounds the upload fan-out
	// against the homeserver as well as sockets/fds/goroutines. Kept low (down
	// from 32, then 8) so a backfill doesn't hammer the Matrix media repo with
	// simultaneous uploads and — now that downloads land on disk — can't pile up
	// many large temp files at once. On a single-core host the downloads are
	// I/O-bound, so fewer in flight costs little speed and is meaningfully safer
	// on memory and disk. Per-item memory/disk is bounded separately by
	// attachmentByteSem and the minFreeDiskBytes floor.
	attachmentMaxParallelDownloads = 4
	// attachmentMaxInFlightBytes caps the total attachment payload resident in
	// memory at once across the pre-upload fan-out. Each in-flight download
	// holds its blob (and briefly its transcoded copy ≈ 2× this budget) in RAM,
	// so on a small host (the 1GB Docker target) a burst of large videos at
	// full count parallelism could blow the memory budget. Weighting each
	// download by its FileSize against this budget keeps a pure-photo backfill
	// fully parallel while throttling large-video bursts to whatever fits —
	// bounding peak RAM without slowing the common case. The ffmpeg encode peak
	// is bounded separately by transcodeSem (one re-encode at a time), which is
	// the single largest consumer; this value leaves it headroom under 1GB.
	// iMessage caps attachments at ~100 MB, so this sits just above the largest
	// possible single item: one max-size video downloads near-alone (the right
	// bound) while smaller attachments still pack in alongside it. Tune up if
	// the host has more RAM (faster large-video backfill) or down if it's still
	// tight.
	attachmentMaxInFlightBytes int64 = 128 << 20 // 128 MiB
)

// attachmentByteSem bounds the total attachment payload resident in memory at
// once across ALL CloudKit download fan-outs — the startup pre-upload, the
// per-chunk forward backfill, and the per-message multi-attachment download. It
// is process-wide (one shared budget) so overlapping phases can't each spend a
// full budget and collectively blow past it. Acquired weighted by FileSize and
// held for the lifetime of each download+transcode+upload.
var attachmentByteSem = semaphore.NewWeighted(attachmentMaxInFlightBytes)

// clampAttachmentWeight turns an attachment's reported FileSize into a valid
// weight for the in-flight-bytes semaphore: at least 1 and at most the whole
// budget.
//
// CRITICAL: an unknown or invalid size (0, or a NEGATIVE value from an
// overflowed/corrupt CloudKit file_size — real records carry sizes like
// -1,676,737,818, i.e. a ~2.6 GB blob wrapped past int32) must be treated as
// WORST CASE, not as free. The old code returned weight 1 for fileSize < 1,
// which defeated the budget entirely: a multi-GB download with a bogus size
// cost one byte of budget, so a full fan-out of them ran concurrently and OOM-killed the
// host (resident set blew past 5 GB before the kernel reaped the process).
// Charging the full budget makes an unknown-size blob acquire the whole
// semaphore and run ALONE, bounding peak resident bytes to a single download
// instead of the fan-out's worth.
//
// The upper clamp does the same for a known-oversized item (> budget): it
// runs alone rather than deadlocking on budget it could never acquire.
func clampAttachmentWeight(fileSize int64) int64 {
	if fileSize < 1 || fileSize > attachmentMaxInFlightBytes {
		return attachmentMaxInFlightBytes
	}
	return fileSize
}

// preUploadCloudAttachments downloads every CloudKit attachment recorded in the
// cloud message store and uploads it to Matrix, caching the resulting
// *event.MessageEventContent in attachmentContentCache keyed by record_name.
//
// Call this in the cloud sync goroutine BEFORE createPortalsFromCloudSync.
// When bridgev2 subsequently calls FetchMessages inside the portal event loop,
// every downloadAndUploadAttachment invocation becomes an instant cache lookup
// instead of a multi-second CloudKit download — eliminating the 30+ minute
// portal event loop stall caused by image-heavy conversations (e.g. 486 pics).
func (c *IMClient) preUploadCloudAttachments(ctx context.Context) {
	if c.cloudStore == nil {
		return
	}
	// When the user has capped max_initial_messages, skip the bulk startup
	// pre-upload. The per-chunk preUploadChunkAttachments in FetchMessages
	// already handles attachments for the rows actually being backfilled.
	// The startup pre-upload is an optimization for the unlimited case where
	// thousands of attachments need warming before portal creation.
	if c.Main.Bridge.Config.Backfill.MaxInitialMessages < math.MaxInt32 {
		return
	}
	log := c.Main.Bridge.Log.With().Str("component", "cloud_preupload").Logger()

	rows, err := c.cloudStore.listAllAttachmentMessages(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Pre-upload: failed to list attachment messages, skipping")
		return
	}

	// Load previously persisted mxc URIs into the in-memory cache.
	// This means attachments uploaded in any prior run are instant cache hits
	// in the pending-list filter below — zero CloudKit downloads needed.
	if cachedJSON, err := c.cloudStore.loadAttachmentCacheJSON(ctx); err != nil {
		log.Warn().Err(err).Msg("Pre-upload: failed to load persistent attachment cache, will re-download")
	} else {
		loaded := 0
		for recordName, jsonBytes := range cachedJSON {
			var content event.MessageEventContent
			if err := json.Unmarshal(jsonBytes, &content); err != nil {
				log.Debug().Err(err).Str("record_name", recordName).
					Msg("Pre-upload: skipping corrupted attachment cache entry")
			} else {
				// Skip stale cache entries for non-MP4 videos that need transcoding
				if content.Info != nil && content.Info.MimeType == "video/quicktime" {
					continue
				}
				c.attachmentContentCache.Store(recordName, &content)
				loaded++
			}
		}
		if loaded > 0 {
			log.Debug().Int("loaded", loaded).Msg("Pre-upload: restored attachment cache from SQLite")
		}
	}

	// Build the set of portal IDs whose forward FetchMessages has already
	// completed successfully. These portals don't need pre-upload on restart:
	// - Normal restart: all portals are done → skip everything (no re-upload storm)
	// - Interrupted backfill: that portal is NOT in the done set → still pre-uploads
	// Using fwd_backfill_done (set by FetchMessages via CompleteCallback) rather than
	// MXID-existence avoids the interrupted-backfill edge case.
	donePortals, err := c.cloudStore.getForwardBackfillDonePortals(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Pre-upload: failed to query fwd_backfill_done, skipping pre-upload entirely")
		return
	}

	// Build the list of attachments that still need to be uploaded.
	type pendingUpload struct {
		row     cloudMessageRow
		idx     int
		att     cloudAttachmentRow
		sender  bridgev2.EventSender
		ts      time.Time
		hasText bool
	}
	var pending []pendingUpload
	for _, row := range rows {
		portalDone := donePortals[row.PortalID]
		var atts []cloudAttachmentRow
		if err := json.Unmarshal([]byte(row.AttachmentsJSON), &atts); err != nil {
			log.Warn().Err(err).Str("guid", row.GUID).
				Msg("Pre-upload: failed to unmarshal attachment JSON, skipping message")
			continue
		}
		sender := c.makeCloudSender(row)
		ts := time.UnixMilli(row.TimestampMS)
		hasText := strings.TrimSpace(strings.Trim(row.Text, "\ufffc \n")) != ""
		for i, att := range atts {
			if att.RecordName == "" {
				continue
			}
			// Mirror the filter in cloudAttachmentsToBackfill — don't waste a
			// CloudKit download on rich-link plugin payloads we'll never bridge.
			if isPluginPayloadAttachment(att) {
				continue
			}
			if _, ok := c.attachmentContentCache.Load(att.RecordName); ok {
				continue // already cached from a previous pass
			}
			// Permanently tombstoned (CloudKit no longer serves it). Skip
			// without re-attempting — this is what stops the dead old
			// attachments from being re-downloaded on every sync sweep.
			if c.isDeadAttachment(att.RecordName) {
				continue
			}
			// Check if this attachment has failed before and whether
			// it has exceeded the retry limit.
			prev, isFailed := c.failedAttachments.Load(att.RecordName)
			if isFailed {
				fe := prev.(*failedAttachmentEntry)
				if fe.abandoned || fe.retries >= maxAttachmentRetries {
					// Don't delete the entry — keep it so this attachment stays
					// skipped for the rest of the session. Deleting it reset the
					// retry counter and made the same dead attachment re-download
					// on the next sweep (the infinite-loop bug).
					log.Debug().
						Str("record_name", att.RecordName).
						Str("portal_id", row.PortalID).
						Str("last_error", fe.lastError).
						Int("retries", fe.retries).
						Msg("Pre-upload: skipping attachment, retry budget exhausted")
					continue
				}
			}
			// For done portals, only retry attachments that previously
			// failed (transient). New uncached attachments in done portals
			// were already handled by FetchMessages — no re-upload needed.
			if portalDone {
				if !isFailed {
					continue
				}
				fe := prev.(*failedAttachmentEntry)
				log.Info().
					Str("record_name", att.RecordName).
					Str("portal_id", row.PortalID).
					Int("attempt", fe.retries+1).
					Msg("Pre-upload: retrying previously failed attachment for completed portal")
			}
			pending = append(pending, pendingUpload{
				row: row, idx: i, att: att,
				sender: sender, ts: ts, hasText: hasText,
			})
		}
	}

	if len(pending) == 0 {
		log.Debug().Int("message_rows", len(rows)).Msg("Pre-upload: all attachments already cached")
		return
	}

	log.Info().
		Int("attachments", len(pending)).
		Int("message_rows", len(rows)).
		Msg("Pre-upload: starting CloudKit→Matrix attachment pre-upload before portal creation")

	start := time.Now()
	sem := make(chan struct{}, attachmentMaxParallelDownloads)
	byteSem := attachmentByteSem
	var wg sync.WaitGroup
	var uploaded atomic.Int64

	for _, p := range pending {
		wg.Add(1)
		go func(p pendingUpload) {
			defer wg.Done()
			// Check context before acquiring semaphore so shutdown doesn't
			// queue up behind the in-flight downloads (each up to 90s).
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			// Bound resident attachment bytes: hold budget for this blob's
			// size for the lifetime of the download+transcode+upload.
			weight := clampAttachmentWeight(p.att.FileSize)
			if err := byteSem.Acquire(ctx, weight); err != nil {
				return // ctx cancelled
			}
			defer byteSem.Release(weight)
			defer func() {
				if r := recover(); r != nil {
					log.Error().Any("panic", r).
						Str("record_name", p.att.RecordName).
						Msg("Pre-upload: recovered panic in attachment goroutine")
				}
			}()
			// downloadAndUploadAttachment stores the result in attachmentContentCache
			// as a side effect; we discard the returned BackfillMessage here.
			result := c.downloadAndUploadAttachment(ctx, p.row, p.sender, p.ts, p.hasText, p.idx, p.att)
			if result != nil {
				uploaded.Add(1)
			}
		}(p)
	}

	// Wait for all uploads to finish. No overall timeout here: the 90s
	// per-download cap in safeCloudDownloadAttachment already bounds the
	// worst case, and the persistent SQLite cache means this only runs for
	// genuinely new/uncached attachments — so it completes fully every time
	// and FetchMessages always gets 100% cache hits.
	wg.Wait()
	failed := int64(len(pending)) - uploaded.Load()
	log.Info().
		Int64("uploaded", uploaded.Load()).
		Int64("failed", failed).
		Int("total", len(pending)).
		Dur("elapsed", time.Since(start)).
		Msg("Pre-upload: CloudKit→Matrix attachment pre-upload complete")
}

// preUploadChunkAttachments downloads and uploads any uncached attachments in
// the given rows in parallel (up to attachmentMaxParallelDownloads concurrent). Called inline during
// forward backfill before the sequential conversion loop, so that
// downloadAndUploadAttachment gets instant cache hits instead of doing
// sequential 90s CloudKit downloads that hang the portal event loop.
func (c *IMClient) preUploadChunkAttachments(ctx context.Context, rows []cloudMessageRow, log zerolog.Logger) {
	type pendingAtt struct {
		row     cloudMessageRow
		idx     int
		att     cloudAttachmentRow
		sender  bridgev2.EventSender
		ts      time.Time
		hasText bool
	}
	var pending []pendingAtt
	for _, row := range rows {
		if row.AttachmentsJSON == "" {
			continue
		}
		var atts []cloudAttachmentRow
		if err := json.Unmarshal([]byte(row.AttachmentsJSON), &atts); err != nil {
			log.Warn().Err(err).Str("guid", row.GUID).
				Msg("Forward backfill: failed to unmarshal attachment JSON, skipping message")
			continue
		}
		sender := c.makeCloudSender(row)
		ts := time.UnixMilli(row.TimestampMS)
		hasText := strings.TrimSpace(strings.Trim(row.Text, "\ufffc \n")) != ""
		for i, att := range atts {
			if att.RecordName == "" {
				continue
			}
			if _, ok := c.attachmentContentCache.Load(att.RecordName); ok {
				continue
			}
			pending = append(pending, pendingAtt{
				row: row, idx: i, att: att,
				sender: sender, ts: ts, hasText: hasText,
			})
		}
	}
	if len(pending) == 0 {
		return
	}
	log.Info().Int("uncached", len(pending)).Msg("Forward backfill: pre-uploading uncached attachments in parallel")
	sem := make(chan struct{}, attachmentMaxParallelDownloads)
	byteSem := attachmentByteSem
	var wg sync.WaitGroup
	for _, p := range pending {
		wg.Add(1)
		go func(p pendingAtt) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			// Bound resident attachment bytes (see preUploadCloudAttachments).
			weight := clampAttachmentWeight(p.att.FileSize)
			if err := byteSem.Acquire(ctx, weight); err != nil {
				return // ctx cancelled
			}
			defer byteSem.Release(weight)
			defer func() {
				if r := recover(); r != nil {
					log.Error().Any("panic", r).
						Str("record_name", p.att.RecordName).
						Msg("Forward backfill pre-upload: recovered panic")
				}
			}()
			c.downloadAndUploadAttachment(ctx, p.row, p.sender, p.ts, p.hasText, p.idx, p.att)
		}(p)
	}
	wg.Wait()
	log.Info().Int("processed", len(pending)).Msg("Forward backfill: pre-upload complete")
}

func reverseCloudMessageRows(rows []cloudMessageRow) {
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
}

func encodeCloudBackfillCursor(cursor cloudBackfillCursor) (networkid.PaginationCursor, error) {
	data, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(data)
	return networkid.PaginationCursor(encoded), nil
}

func decodeCloudBackfillCursor(cursor networkid.PaginationCursor) (*cloudBackfillCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(string(cursor))
	if err != nil {
		return nil, err
	}
	var parsed cloudBackfillCursor
	if err = json.Unmarshal(decoded, &parsed); err != nil {
		return nil, err
	}
	if parsed.GUID == "" {
		return nil, fmt.Errorf("empty guid in cursor")
	}
	return &parsed, nil
}

// ============================================================================
// State persistence
// ============================================================================

func (c *IMClient) persistState(log zerolog.Logger) {
	// Guard against panics crossing the FFI boundary from any of the four
	// rustpushgo calls below. A panic here would otherwise kill the bridge
	// on a non-essential periodic persist; skipping one cycle is strictly
	// safer than crashing the process.
	defer func() {
		if r := recover(); r != nil {
			log.Warn().Interface("panic", r).Msg("persistState panicked — skipped this cycle")
		}
	}()
	meta := c.UserLogin.Metadata.(*UserLoginMetadata)
	if c.connection != nil {
		meta.APSState = c.connection.State().ToString()
	}
	if c.users != nil {
		meta.IDSUsers = c.users.ToString()
	}
	if c.identity != nil {
		meta.IDSIdentity = c.identity.ToString()
	}
	if c.config != nil {
		meta.DeviceID = c.config.GetDeviceId()
	}
	if err := c.UserLogin.Save(context.Background()); err != nil {
		log.Err(err).Msg("Failed to persist state")
	}
}

func (c *IMClient) periodicStateSave(log zerolog.Logger) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.persistState(log)
			log.Debug().Msg("Periodic state save completed")
		case <-c.stopChan:
			c.persistState(log)
			log.Debug().Msg("Final state save on disconnect")
			return
		}
	}
}

// petRefreshMinInterval throttles startup/on-demand PET refreshes to protect
// against restart-loop-induced Apple rate limiting. login_email_pass is
// per-user-expected but bridges in a crash-restart loop could stack several
// per minute, which is the one failure mode that reliably trips 429s or a
// temporary account lock. 5 min gives a comfortable margin (max 12/hour vs
// the ~30/hour threshold where Apple's fraud systems start reacting) while
// still allowing legitimate manual restarts close together during debugging.
const petRefreshMinInterval = 5 * time.Minute

// petRefreshKVKey is the KV key used to persist the last PET refresh time.
// Persistent (not in-memory) so the throttle survives the exact failure mode
// it's protecting against — a crash-restart loop that wipes in-memory state
// between each attempt.
const petRefreshKVKey = database.Key("gsa.pet_last_refresh")

// safeRefreshPetTokenThrottled is safeRefreshPetToken with a persistent
// timestamp guard. Returns an error without hitting GSA if a refresh
// completed within petRefreshMinInterval. Callers on hot startup paths
// (share_status prime, announce, command handlers) should use this variant;
// the 12h periodicPetRefresh is already self-throttled and uses the raw
// function.
func (c *IMClient) safeRefreshPetTokenThrottled() error {
	if c.tokenProvider == nil || *c.tokenProvider == nil {
		return fmt.Errorf("no token provider")
	}
	ctx := context.Background()
	now := time.Now()
	if raw := c.Main.Bridge.DB.KV.Get(ctx, petRefreshKVKey); raw != "" {
		if ts, parseErr := strconv.ParseInt(raw, 10, 64); parseErr == nil {
			if elapsed := now.Sub(time.Unix(ts, 0)); elapsed < petRefreshMinInterval {
				return fmt.Errorf("PET refresh throttled: last refresh %s ago (min %s)",
					elapsed.Round(time.Second), petRefreshMinInterval)
			}
		}
	}
	if err := safeRefreshPetToken(*c.tokenProvider); err != nil {
		return err
	}
	c.Main.Bridge.DB.KV.Set(ctx, petRefreshKVKey, strconv.FormatInt(now.Unix(), 10))
	return nil
}

// safeRefreshPetToken wraps RefreshPetToken with panic recovery and a 60s
// timeout. Uniffi turns Rust panics into Go panics; without recovery a single
// anisette or network hiccup could crash the bridge from a best-effort
// background refresh. The timeout prevents an anisette-poisoned tokio mutex
// (see project memory) from blocking the goroutine indefinitely.
//
// For hot startup paths (announce, share_status prime, etc.) prefer
// safeRefreshPetTokenThrottled to avoid stacking GSA logins across
// restart loops. This raw variant is appropriate only for self-throttled
// paths like the 12h periodic refresh.
func safeRefreshPetToken(tp *rustpushgo.WrappedTokenProvider) error {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("RefreshPetToken panicked: %v", r)
			}
		}()
		done <- tp.RefreshPetToken()
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(60 * time.Second):
		return fmt.Errorf("RefreshPetToken timed out after 60s")
	}
}

// periodicPetRefresh proactively re-runs login_email_pass every 12h so the
// GSA/PET session never ages past Apple's ~24h lifetime. Closes the gap that
// restore_token_provider only covers at startup: a bridge running continuously
// for >24h would otherwise rely on upstream's on-demand auto-refresh, which
// fails with NeedsDevice2FA once Apple has aged out the session.
func (c *IMClient) periodicPetRefresh(log zerolog.Logger) {
	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if c.tokenProvider == nil || *c.tokenProvider == nil {
				continue
			}
			if err := safeRefreshPetToken(*c.tokenProvider); err != nil {
				log.Warn().Err(err).Msg("Periodic PET refresh failed")
			} else {
				log.Debug().Msg("Periodic PET refresh completed")
			}
		case <-c.stopChan:
			return
		}
	}
}

// periodicStatusSharingReinvite re-runs the pending-only StatusKit invite
// sweep every 4h. Peers who've already keyed us are skipped inside the
// sweep. The periodic path sets respectSpacing=true so per-handle KV
// timestamps bound worst-case re-invite rate for unresponsive peers.
// Startup and post-backfill paths pass respectSpacing=false: a restart
// is intentional and should always re-initiate invites regardless of
// KV state from the prior session.
func (c *IMClient) periodicStatusSharingReinvite(log zerolog.Logger) {
	ticker := time.NewTicker(4 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.inviteContactsToStatusSharingOpts(log, true, false)
		case <-c.stopChan:
			return
		}
	}
}

// periodicCloudContactSync re-fetches contacts from iCloud CardDAV every
// 15 minutes. Also re-runs the shared iMessage profile re-fetch on the
// same tick so we keep one ticker but don't gate the share-profile path
// behind CardDAV success — refreshAllSharedProfiles only needs CloudKit
// (ProfilesClient) and runs independently even if SyncContacts errors.
func (c *IMClient) periodicCloudContactSync(log zerolog.Logger) {
	const (
		base = 15 * time.Minute
		max  = 6 * time.Hour
	)
	interval := base
	failures := 0
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			if err := c.contacts.SyncContacts(log); err != nil {
				failures++
				interval = contactSyncBackoff(base, max, failures)
				ev := log.Warn().Err(err).Int("failures", failures).Dur("next_in", interval)
				if errors.Is(err, errICloudContactsThrottled) {
					ev = ev.Bool("throttled", true)
				}
				ev.Msg("Periodic CardDAV sync failed — backing off to avoid hammering iCloud")
			} else {
				if failures > 0 {
					log.Info().Msg("CardDAV sync recovered — resetting to base interval")
				}
				failures = 0
				interval = base
				c.setContactsReady(log)
				c.persistMmeDelegate(log)
			}
			c.refreshAllSharedProfiles(log)
			timer.Reset(interval)
		case <-c.stopChan:
			return
		}
	}
}

// contactSyncBackoff returns base doubled per consecutive failure, capped at
// max: 15m → 30m → 1h → 2h → 4h → 6h. Keeps a throttled (403) contacts sync
// from retrying at its fixed cadence and compounding the rate limit.
func contactSyncBackoff(base, max time.Duration, failures int) time.Duration {
	if failures <= 1 {
		return base
	}
	d := base
	for i := 1; i < failures && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}

// persistMmeDelegate saves the current MobileMe delegate to user_login metadata
// so it can be seeded on restore without needing a fresh PET-based auth.
func (c *IMClient) persistMmeDelegate(log zerolog.Logger) {
	if c.tokenProvider == nil || *c.tokenProvider == nil {
		return
	}
	tp := *c.tokenProvider
	delegateJSON, err := tp.GetMmeDelegateJson()
	if err != nil {
		log.Warn().Err(err).Msg("Failed to get MobileMe delegate for persistence")
		return
	}
	if delegateJSON == nil || *delegateJSON == "" {
		return
	}
	meta := c.UserLogin.Metadata.(*UserLoginMetadata)
	if meta.MmeDelegateJSON == *delegateJSON {
		return // unchanged
	}
	meta.MmeDelegateJSON = *delegateJSON
	if err = c.UserLogin.Save(context.Background()); err != nil {
		log.Warn().Err(err).Msg("Failed to persist MobileMe delegate")
	} else {
		log.Info().Msg("Persisted MobileMe delegate to user_login metadata")
	}
}

// retryCloudContacts retries the cloud contacts initialization periodically
// when it fails on startup (e.g., expired MobileMe delegate). Once contacts
// succeed, the readiness gate opens and cloud sync begins.
func (c *IMClient) retryCloudContacts(log zerolog.Logger) {
	const (
		base = 2 * time.Minute
		max  = 1 * time.Hour
	)
	interval := base
	failures := 0
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			log.Info().Msg("Retrying cloud contacts initialization...")
			c.contacts = newCloudContactsClient(c.client, log)
			if c.contacts != nil {
				if syncErr := c.contacts.SyncContacts(log); syncErr != nil {
					failures++
					interval = contactSyncBackoff(base, max, failures)
					ev := log.Warn().Err(syncErr).Int("failures", failures).Dur("next_in", interval)
					if errors.Is(syncErr, errICloudContactsThrottled) {
						ev = ev.Bool("throttled", true)
					}
					ev.Msg("Cloud contacts retry: sync failed — backing off to avoid hammering iCloud")
				} else {
					c.setContactsReady(log)
					c.persistMmeDelegate(log)
					log.Info().Msg("Cloud contacts retry succeeded, starting periodic sync")
					go c.periodicCloudContactSync(log)
					return
				}
			} else {
				failures++
				interval = contactSyncBackoff(base, max, failures)
				log.Warn().Int("failures", failures).Dur("next_in", interval).Msg("Cloud contacts retry: still unavailable — backing off")
			}
			timer.Reset(interval)
		case <-c.stopChan:
			return
		}
	}
}

// ============================================================================
// Contact change watcher
// ============================================================================

// refreshAllGhosts re-resolves contact info for every known ghost and pushes
// any changes (name, avatar, identifiers) to Matrix.
func (c *IMClient) refreshAllGhosts(log zerolog.Logger) {
	ctx := log.WithContext(context.Background())

	// Query all ghost IDs from the bridge database.
	rows, err := c.Main.Bridge.DB.Database.Query(ctx,
		"SELECT id FROM ghost WHERE bridge_id=$1",
		c.Main.Bridge.ID,
	)
	if err != nil {
		log.Err(err).Msg("Contact refresh: failed to query ghost IDs")
		return
	}
	defer rows.Close()
	var ghostIDs []networkid.UserID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			log.Err(err).Msg("Contact refresh: failed to scan ghost ID")
			continue
		}
		ghostIDs = append(ghostIDs, networkid.UserID(id))
	}
	if err := rows.Err(); err != nil {
		log.Err(err).Msg("Contact refresh: row iteration error")
	}

	updated := 0
	for _, ghostID := range ghostIDs {
		ghost, err := c.Main.Bridge.GetGhostByID(ctx, ghostID)
		if err != nil {
			log.Warn().Err(err).Str("ghost_id", string(ghostID)).Msg("Contact refresh: failed to load ghost")
			continue
		}
		info, err := c.GetUserInfo(ctx, ghost)
		if err != nil || info == nil {
			continue
		}
		ghost.UpdateInfo(ctx, info)
		updated++
	}

	log.Info().Int("ghosts_checked", len(ghostIDs)).Int("updated", updated).
		Msg("Contact change detected — refreshed ghost profiles")
}

// ============================================================================
// Helpers
// ============================================================================

func (c *IMClient) isMyHandle(handle string) bool {
	normalizedHandle := normalizeIdentifierForPortalID(handle)
	for _, h := range c.allHandles {
		if normalizedHandle == normalizeIdentifierForPortalID(h) {
			return true
		}
	}
	return false
}

// normalizeIdentifierForPortalID canonicalizes user/chat identifiers so portal
// routing is stable across formatting variants (notably SMS numbers with and
// without leading "+1").
func normalizeIdentifierForPortalID(identifier string) string {
	id := strings.TrimSpace(identifier)
	if id == "" {
		return ""
	}
	// Strip Apple SMS service suffixes: "+12155167207(smsft)" → "+12155167207",
	// "787473(smsft)" → "787473". Must happen before any other processing so
	// the suffix never reaches isNumeric / normalizePhone checks.
	id = stripSmsSuffix(id)

	if strings.HasPrefix(id, "mailto:") {
		return "mailto:" + strings.ToLower(strings.TrimPrefix(id, "mailto:"))
	}
	if strings.Contains(id, "@") && !strings.HasPrefix(id, "tel:") {
		return "mailto:" + strings.ToLower(strings.TrimPrefix(id, "mailto:"))
	}

	if strings.HasPrefix(id, "tel:") || strings.HasPrefix(id, "+") || isNumeric(id) {
		local := stripIdentifierPrefix(id)
		normalized := normalizePhoneIdentifierForPortalID(local)
		if normalized != "" {
			return "tel:" + normalized
		}
		return addIdentifierPrefix(local)
	}

	return id
}

// buildCanonicalParticipantList normalizes, deduplicates, and sorts a
// participant list into the canonical form used for comma-based portal IDs.
// Any self handles are filtered out and replaced with the single canonical
// c.handle. Accepts both raw and pre-normalized inputs (normalization is
// idempotent).
func (c *IMClient) buildCanonicalParticipantList(participants []string) []string {
	sorted := make([]string, 0, len(participants))
	for _, p := range participants {
		normalized := normalizeIdentifierForPortalID(p)
		if normalized == "" || c.isMyHandle(normalized) {
			continue
		}
		sorted = append(sorted, normalized)
	}
	sorted = append(sorted, normalizeIdentifierForPortalID(c.handle))
	sort.Strings(sorted)
	deduped := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			deduped = append(deduped, s)
		}
	}
	return deduped
}

// normalizePhoneIdentifierForPortalID canonicalizes phone-like identifiers while
// preserving short-code semantics (e.g. "242733" stays "242733", not "+242733").
func normalizePhoneIdentifierForPortalID(local string) string {
	cleaned := normalizePhone(local)
	if cleaned == "" {
		return ""
	}
	if strings.HasPrefix(cleaned, "+") {
		return cleaned
	}
	if len(cleaned) == 10 {
		return "+1" + cleaned
	}
	if len(cleaned) == 11 && cleaned[0] == '1' {
		return "+" + cleaned
	}
	if len(cleaned) >= 11 {
		return "+" + cleaned
	}
	return cleaned
}

func (c *IMClient) makeEventSender(sender *string) bridgev2.EventSender {
	if sender == nil || *sender == "" || c.isMyHandle(*sender) {
		c.ensureDoublePuppet()
		return bridgev2.EventSender{
			IsFromMe:    true,
			SenderLogin: c.UserLogin.ID,
			Sender:      makeUserID(c.handle),
		}
	}
	normalizedSender := normalizeIdentifierForPortalID(*sender)
	return bridgev2.EventSender{
		IsFromMe: false,
		Sender:   makeUserID(normalizedSender),
	}
}

// ensureDoublePuppet retries double puppet setup if it previously failed.
//
// The mautrix bridgev2 framework permanently caches a nil DoublePuppet() on
// first failure (user.go sets doublePuppetInitialized=true BEFORE calling
// NewUserIntent). On macOS Ventura, transient IDS registration issues can
// cause the initial setup to fail, and without a retry the nil is cached
// forever — making all IsFromMe messages fall through to the ghost intent,
// which flips their direction (sent appears as received).
//
// This workaround detects the cached nil and re-attempts login using the
// saved access token, which succeeds once IDS registration stabilizes.
func (c *IMClient) ensureDoublePuppet() {
	ctx := context.Background()
	user := c.UserLogin.User
	if user.DoublePuppet(ctx) != nil {
		return // already working
	}
	token := user.AccessToken
	if token == "" {
		return // no token to retry with
	}
	user.LogoutDoublePuppet(ctx)
	if err := user.LoginDoublePuppet(ctx, token); err != nil {
		c.UserLogin.Log.Warn().Err(err).Msg("Failed to re-establish double puppet")
	} else {
		c.UserLogin.Log.Info().Msg("Re-established double puppet after previous failure")
	}
}

// resolveExistingDMPortalID prefers an already-created DM portal key variant
// (e.g. legacy tel:1415... vs canonical tel:+1415...) to avoid splitting rooms
// when normalization rules change. For mailto: identifiers, it also tries
// the contact's phone-based portal IDs (since StatusKit may report the email
// handle while the DM portal was created under the phone handle).
func (c *IMClient) resolveExistingDMPortalID(identifier string) networkid.PortalID {
	defaultID := networkid.PortalID(identifier)
	if identifier == "" || strings.Contains(identifier, ",") {
		return defaultID
	}

	// For mailto: identifiers, try the contact's other handles (phone numbers)
	// since the DM portal may have been created under a tel: handle.
	if strings.HasPrefix(identifier, "mailto:") {
		contact := c.lookupContact(identifier)
		if contact != nil {
			ctx := context.Background()
			for _, altID := range contactPortalIDs(contact) {
				if altID == identifier {
					continue
				}
				portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, networkid.PortalKey{
					ID:       networkid.PortalID(altID),
					Receiver: c.UserLogin.ID,
				})
				if err == nil && portal != nil && portal.MXID != "" {
					c.UserLogin.Log.Debug().
						Str("original", identifier).
						Str("resolved", altID).
						Msg("Resolved mailto: DM portal to existing contact portal")
					return networkid.PortalID(altID)
				}
			}
		}
		return defaultID
	}

	if !strings.HasPrefix(identifier, "tel:") {
		return defaultID
	}

	local := strings.TrimPrefix(identifier, "tel:")
	candidates := make([]string, 0, 3)
	seen := map[string]bool{identifier: true}
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		candidates = append(candidates, id)
	}

	if strings.HasPrefix(local, "+") {
		withoutPlus := strings.TrimPrefix(local, "+")
		add("tel:" + withoutPlus)
		if strings.HasPrefix(local, "+1") && len(local) == 12 {
			add("tel:" + strings.TrimPrefix(local, "+1"))
		}
	} else if isNumeric(local) {
		if len(local) == 10 {
			add("tel:1" + local)
		}
		if len(local) == 11 && strings.HasPrefix(local, "1") {
			add("tel:" + local[1:])
		}
	}

	ctx := context.Background()
	for _, candidate := range candidates {
		portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, networkid.PortalKey{
			ID:       networkid.PortalID(candidate),
			Receiver: c.UserLogin.ID,
		})
		if err == nil && portal != nil && portal.MXID != "" {
			c.UserLogin.Log.Debug().
				Str("normalized", identifier).
				Str("resolved", candidate).
				Msg("Resolved DM portal to existing legacy identifier")
			return networkid.PortalID(candidate)
		}
	}

	return defaultID
}

// ensureGroupPortalIndex lazily loads all existing group portals from the DB
// and builds an in-memory index mapping each member to its group portal IDs.
func (c *IMClient) ensureGroupPortalIndex() {
	c.groupPortalMu.Lock()
	defer c.groupPortalMu.Unlock()
	if c.groupPortalIndex != nil {
		return // already loaded
	}

	idx := make(map[string]map[string]bool)
	ctx := context.Background()
	portals, err := c.Main.Bridge.DB.Portal.GetAllWithMXID(ctx)
	if err != nil {
		c.UserLogin.Log.Err(err).Msg("Failed to load portals for group index")
		return // leave c.groupPortalIndex nil so next call retries
	}
	for _, p := range portals {
		portalID := string(p.ID)
		if !strings.Contains(portalID, ",") {
			continue // skip DMs
		}
		if p.Receiver != c.UserLogin.ID {
			continue // skip other users' portals
		}
		for _, member := range strings.Split(portalID, ",") {
			if idx[member] == nil {
				idx[member] = make(map[string]bool)
			}
			idx[member][portalID] = true
		}
	}
	c.groupPortalIndex = idx
	c.UserLogin.Log.Debug().
		Int("portals_indexed", len(c.groupPortalIndex)).
		Msg("Built group portal fuzzy-match index")
}

// indexGroupPortalLocked adds a group portal ID to the in-memory index.
// Caller must hold groupPortalMu write lock.
// Safe to call before ensureGroupPortalIndex — returns early when the index
// has not been built yet (nil map), since the full rebuild will pick it up.
func (c *IMClient) indexGroupPortalLocked(portalID string) {
	if c.groupPortalIndex == nil {
		return
	}
	for _, member := range strings.Split(portalID, ",") {
		if c.groupPortalIndex[member] == nil {
			c.groupPortalIndex[member] = make(map[string]bool)
		}
		c.groupPortalIndex[member][portalID] = true
	}
}

// registerGroupPortal thread-safely indexes a new group portal.
func (c *IMClient) registerGroupPortal(portalID string) {
	c.groupPortalMu.Lock()
	defer c.groupPortalMu.Unlock()
	c.indexGroupPortalLocked(portalID)
}

// reIDPortalWithCacheUpdate atomically re-keys a portal in the DB and updates
// all in-memory caches. Holding all group cache write locks during the entire
// operation prevents concurrent handlers (read receipts, typing indicators)
// from observing a state where the DB key changed but caches still reference
// the old portal ID.
func (c *IMClient) reIDPortalWithCacheUpdate(ctx context.Context, oldKey, newKey networkid.PortalKey) (bridgev2.ReIDResult, *bridgev2.Portal, error) {
	oldID := string(oldKey.ID)
	newID := string(newKey.ID)

	c.imGroupNamesMu.Lock()
	c.imGroupGuidsMu.Lock()
	c.imGroupParticipantsMu.Lock()
	c.groupPortalMu.Lock()
	c.lastGroupForMemberMu.Lock()
	c.gidAliasesMu.Lock()
	c.smsPortalsLock.Lock()
	defer c.smsPortalsLock.Unlock()
	defer c.gidAliasesMu.Unlock()
	defer c.lastGroupForMemberMu.Unlock()
	defer c.groupPortalMu.Unlock()
	defer c.imGroupParticipantsMu.Unlock()
	defer c.imGroupGuidsMu.Unlock()
	defer c.imGroupNamesMu.Unlock()

	result, portal, err := c.Main.Bridge.ReIDPortal(ctx, oldKey, newKey)
	if err != nil {
		return result, portal, err
	}

	// Move group name cache
	if name, ok := c.imGroupNames[oldID]; ok {
		c.imGroupNames[newID] = name
		delete(c.imGroupNames, oldID)
	}
	// Move group guid cache
	if guid, ok := c.imGroupGuids[oldID]; ok {
		c.imGroupGuids[newID] = guid
		delete(c.imGroupGuids, oldID)
	}
	// Move group participants cache
	if parts, ok := c.imGroupParticipants[oldID]; ok {
		c.imGroupParticipants[newID] = parts
		delete(c.imGroupParticipants, oldID)
	}
	// Update group portal index: remove old members, add new
	for _, member := range strings.Split(oldID, ",") {
		if portals, ok := c.groupPortalIndex[member]; ok {
			delete(portals, oldID)
			if len(portals) == 0 {
				delete(c.groupPortalIndex, member)
			}
		}
	}
	c.indexGroupPortalLocked(newID)
	// Update lastGroupForMember entries pointing to old portal
	for member, key := range c.lastGroupForMember {
		if key == oldKey {
			c.lastGroupForMember[member] = newKey
		}
	}
	// Update gidAliases entries pointing to old portal
	for alias, target := range c.gidAliases {
		if target == oldID {
			c.gidAliases[alias] = newID
		}
	}
	// Move SMS portal flag
	if isSms, ok := c.smsPortals[oldID]; ok {
		c.smsPortals[newID] = isSms
		delete(c.smsPortals, oldID)
	}

	return result, portal, nil
}

// resolveExistingGroupPortalID checks whether an existing group portal matches
// the computed portal ID via fuzzy matching (differs by at most 1 member).
// If senderGuid is provided, fuzzy matches are validated against the cached
// sender_guid — a mismatch means a different group even if members overlap.
// If a match is found, returns the existing portal ID; otherwise registers the
// new ID and returns it as-is.
func (c *IMClient) resolveExistingGroupPortalID(computedID string, senderGuid *string) networkid.PortalID {
	c.ensureGroupPortalIndex()

	// Fast path: exact match in DB
	ctx := context.Background()
	portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, networkid.PortalKey{
		ID:       networkid.PortalID(computedID),
		Receiver: c.UserLogin.ID,
	})
	if err == nil && portal != nil && portal.MXID != "" {
		return networkid.PortalID(computedID)
	}

	// Fuzzy match: find existing portals that share members with the candidate.
	candidateMembers := strings.Split(computedID, ",")
	candidateSize := len(candidateMembers)

	// Count how many members each existing portal shares with the candidate.
	overlap := make(map[string]int) // existing portal ID -> shared member count
	c.groupPortalMu.RLock()
	for _, member := range candidateMembers {
		for existingID := range c.groupPortalIndex[member] {
			overlap[existingID]++
		}
	}
	c.groupPortalMu.RUnlock()

	for existingID, sharedCount := range overlap {
		existingMembers := strings.Split(existingID, ",")
		existingSize := len(existingMembers)
		diff := (candidateSize - sharedCount) + (existingSize - sharedCount)
		if diff > 1 {
			continue
		}
		if diff == 1 && !participantSetsMatch(candidateMembers, existingMembers, c.isMyHandle) {
			continue // diff=1 for a non-self member → different group
		}

		// If we have a sender_guid, reject fuzzy matches with a different
		// sender_guid — they are genuinely different group conversations
		// that happen to share most members.
		if senderGuid != nil && *senderGuid != "" {
			c.imGroupGuidsMu.RLock()
			existingGuid := c.imGroupGuids[existingID]
			c.imGroupGuidsMu.RUnlock()
			if existingGuid != "" && existingGuid != *senderGuid {
				continue
			}
		}

		// Verify the match actually exists in DB with a Matrix room.
		existing, err := c.Main.Bridge.GetExistingPortalByKey(ctx, networkid.PortalKey{
			ID:       networkid.PortalID(existingID),
			Receiver: c.UserLogin.ID,
		})
		if err != nil || existing == nil || existing.MXID == "" {
			continue
		}

		c.UserLogin.Log.Info().
			Str("computed", computedID).
			Str("resolved", existingID).
			Int("diff", diff).
			Msg("Fuzzy-matched group portal to existing room")
		return networkid.PortalID(existingID)
	}

	// No match — register this as a new group portal.
	c.registerGroupPortal(computedID)
	return networkid.PortalID(computedID)
}

// findGroupPortalForMember returns the most likely group portal for a member.
// Prefers the group where the member last sent a message; falls back to the
// sole group containing them. Used when typing/read receipts lack full
// participant lists.
func isComputedDMPortalID(id networkid.PortalID) bool {
	s := string(id)
	return !strings.HasPrefix(s, "gid:") && !strings.Contains(s, ",")
}

func (c *IMClient) resolveSMSGroupRedirectPortal(msg rustpushgo.WrappedMessage) (networkid.PortalKey, bool) {
	if len(msg.Participants) != 0 || msg.Sender == nil || *msg.Sender == "" {
		return networkid.PortalKey{}, false
	}

	candidates := make(map[string]struct{}, 3)
	addCandidate := func(portalID string) {
		if portalID == "" {
			return
		}
		candidates[portalID] = struct{}{}
	}

	if msg.SenderGuid != nil && *msg.SenderGuid != "" {
		// Apple group GUIDs are stable across messages; this bridge stores them
		// as portal IDs prefixed with "gid:".
		addCandidate("gid:" + strings.ToLower(*msg.SenderGuid))

		c.imGroupGuidsMu.RLock()
		matches := make([]string, 0, 1)
		for portalID, guid := range c.imGroupGuids {
			if strings.EqualFold(guid, *msg.SenderGuid) {
				matches = append(matches, portalID)
			}
		}
		c.imGroupGuidsMu.RUnlock()
		if len(matches) == 1 {
			addCandidate(matches[0])
		}
	}

	// Fallback: unique group membership match only (no "last active" heuristic).
	normalized := normalizeIdentifierForPortalID(*msg.Sender)
	if normalized != "" {
		c.ensureGroupPortalIndex()
		c.groupPortalMu.RLock()
		portals := c.groupPortalIndex[normalized]
		c.groupPortalMu.RUnlock()
		if len(portals) == 1 {
			for portalID := range portals {
				addCandidate(portalID)
			}
		}
	}

	if len(candidates) != 1 {
		return networkid.PortalKey{}, false
	}

	ctx := context.Background()
	for portalID := range candidates {
		groupKey := networkid.PortalKey{ID: networkid.PortalID(portalID), Receiver: c.UserLogin.ID}
		if existing, _ := c.Main.Bridge.GetExistingPortalByKey(ctx, groupKey); existing != nil && existing.MXID != "" {
			return groupKey, true
		}
	}
	return networkid.PortalKey{}, false
}

func (c *IMClient) findGroupPortalForMember(member string) (networkid.PortalKey, bool) {
	normalized := normalizeIdentifierForPortalID(member)
	if normalized == "" {
		return networkid.PortalKey{}, false
	}

	// Prefer last active group for this member.
	c.lastGroupForMemberMu.RLock()
	lastGroup, ok := c.lastGroupForMember[normalized]
	c.lastGroupForMemberMu.RUnlock()
	if ok {
		return lastGroup, true
	}

	// Fall back to group portal index — works if they're in exactly one group.
	c.ensureGroupPortalIndex()
	c.groupPortalMu.RLock()
	portals := c.groupPortalIndex[normalized]
	c.groupPortalMu.RUnlock()

	if len(portals) != 1 {
		return networkid.PortalKey{}, false
	}

	for portalID := range portals {
		return networkid.PortalKey{
			ID:       networkid.PortalID(portalID),
			Receiver: c.UserLogin.ID,
		}, true
	}
	return networkid.PortalKey{}, false
}

// guidCacheMatchIsStale returns true if a guid cache entry for a comma-based
// portal is provably stale — the incoming participants contain members not
// present in the cached portal. Returns false (not provably stale) for
// non-comma portals, when no participant info is available, or when all
// incoming participants are present in the portal's member set.
//
// This intentionally uses a subset check rather than a symmetric match:
// typing and read-receipt payloads may only include [sender, target] without
// the full group roster, so missing portal members are expected and do not
// indicate staleness.
func (c *IMClient) guidCacheMatchIsStale(portalIDStr string, rawParticipants []string) bool {
	if len(rawParticipants) == 0 {
		return false
	}
	// For gid: portal IDs (no comma), resolve participants from the cache
	// so gid->gid aliases can be validated and self-heal when stale.
	var portalParts []string
	if !strings.Contains(portalIDStr, ",") {
		c.imGroupParticipantsMu.RLock()
		cached := c.imGroupParticipants[portalIDStr]
		c.imGroupParticipantsMu.RUnlock()
		if len(cached) == 0 {
			return false // no participant info to compare against
		}
		portalParts = cached
	} else {
		portalParts = strings.Split(portalIDStr, ",")
	}
	portalSet := make(map[string]bool, len(portalParts))
	for _, p := range portalParts {
		portalSet[p] = true
	}
	// Canonicalize incoming participants (collapses alternate self handles
	// to c.handle, deduplicates, sorts) so all callsites get consistent
	// behavior regardless of whether they pre-canonicalize.
	canonical := c.buildCanonicalParticipantList(rawParticipants)
	if len(canonical) == 0 {
		return false
	}
	// Only flag as stale if incoming participants contain members NOT in
	// the portal's set (and not self handles). This correctly handles
	// partial payloads where the incoming list is a subset of the portal.
	for _, p := range canonical {
		if !portalSet[p] && !c.isMyHandle(p) {
			return true
		}
	}
	return false
}

// resolveExistingGroupByGid tries to find an existing group portal that matches
// the incoming message when the gid (sender_guid) doesn't match any known portal.
// This handles the case where another rustpush client (like OpenBubbles) uses a
// different UUID for the same group conversation.
//
// Resolution order:
//  1. imGroupGuids cache — any existing portal with a matching guid value
//  2. imGroupParticipants cache — portals with overlapping participant sets
//  3. groupPortalIndex — comma-based portals via fuzzy participant matching
//  4. cloud_chat DB — participant matching against all persisted groups
func (c *IMClient) resolveExistingGroupByGid(gidPortalID string, senderGuid string, participants []string) networkid.PortalID {
	ctx := context.Background()
	normalizedGuid := strings.ToLower(senderGuid)

	// 1. Check imGroupGuids cache: does any existing portal already have
	//    this guid cached? (covers comma-based portals that previously
	//    received messages with this same guid)
	c.imGroupGuidsMu.RLock()
	for portalIDStr, guid := range c.imGroupGuids {
		if strings.ToLower(guid) == normalizedGuid {
			c.imGroupGuidsMu.RUnlock()
			if c.guidCacheMatchIsStale(portalIDStr, participants) {
				c.UserLogin.Log.Warn().
					Str("gid_portal_id", gidPortalID).
					Str("candidate_portal", portalIDStr).
					Str("stale_guid", guid).
					Msg("Skipping stale guid cache entry: participant mismatch — clearing")
				// Self-heal: remove from in-memory cache
				c.imGroupGuidsMu.Lock()
				if c.imGroupGuids[portalIDStr] == guid {
					delete(c.imGroupGuids, portalIDStr)
				}
				c.imGroupGuidsMu.Unlock()
				// Self-heal: clear stale SenderGuid from DB metadata.
				// MUST be synchronous — portalToConversation (Site E) lazily
				// loads metadata into cache and would re-poison it.
				staleKey := networkid.PortalKey{ID: networkid.PortalID(portalIDStr), Receiver: c.UserLogin.ID}
				if stalePortal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, staleKey); err != nil {
					c.UserLogin.Log.Warn().Err(err).
						Str("portal_id", portalIDStr).
						Msg("Failed to look up portal for stale guid self-heal")
				} else if stalePortal != nil {
					if meta, ok := stalePortal.Metadata.(*PortalMetadata); ok && meta.SenderGuid == guid {
						meta.SenderGuid = ""
						stalePortal.Metadata = meta
						if err := stalePortal.Save(ctx); err != nil {
							c.UserLogin.Log.Warn().Err(err).
								Str("portal_id", portalIDStr).
								Msg("Failed to clear stale SenderGuid from DB metadata")
						} else {
							c.UserLogin.Log.Info().
								Str("portal_id", portalIDStr).
								Str("cleared_guid", guid).
								Msg("Cleared stale SenderGuid from DB metadata")
						}
					}
				}
				c.imGroupGuidsMu.RLock()
				continue
			}
			key := networkid.PortalKey{ID: networkid.PortalID(portalIDStr), Receiver: c.UserLogin.ID}
			if p, _ := c.Main.Bridge.GetExistingPortalByKey(ctx, key); p != nil && p.MXID != "" {
				c.UserLogin.Log.Info().
					Str("gid_portal_id", gidPortalID).
					Str("resolved_portal", portalIDStr).
					Msg("Resolved unknown gid to existing portal via guid cache")
				return networkid.PortalID(portalIDStr)
			}
			c.imGroupGuidsMu.RLock()
		}
	}
	c.imGroupGuidsMu.RUnlock()

	// Build normalized participant set for matching.
	if len(participants) == 0 {
		return networkid.PortalID(gidPortalID)
	}
	normalizedParts := make([]string, 0, len(participants))
	for _, p := range participants {
		n := normalizeIdentifierForPortalID(p)
		if n != "" {
			normalizedParts = append(normalizedParts, n)
		}
	}
	if len(normalizedParts) == 0 {
		return networkid.PortalID(gidPortalID)
	}

	// 2. Check imGroupParticipants cache: find gid: portals with matching
	//    participant sets. Comma-based portals are intentionally excluded here
	//    because step 3 (groupPortalIndex) handles them using the authoritative
	//    portal ID rather than the potentially-stale in-memory cache.
	c.imGroupParticipantsMu.RLock()
	for portalIDStr, parts := range c.imGroupParticipants {
		if portalIDStr == gidPortalID {
			continue
		}
		if !strings.HasPrefix(portalIDStr, "gid:") {
			continue
		}
		if participantSetsMatch(parts, normalizedParts, c.isMyHandle) {
			c.imGroupParticipantsMu.RUnlock()
			key := networkid.PortalKey{ID: networkid.PortalID(portalIDStr), Receiver: c.UserLogin.ID}
			if p, _ := c.Main.Bridge.GetExistingPortalByKey(ctx, key); p != nil && p.MXID != "" {
				c.UserLogin.Log.Info().
					Str("gid_portal_id", gidPortalID).
					Str("resolved_portal", portalIDStr).
					Msg("Resolved unknown gid to existing portal via participant cache")
				return networkid.PortalID(portalIDStr)
			}
			c.imGroupParticipantsMu.RLock()
		}
	}
	c.imGroupParticipantsMu.RUnlock()

	// 3. Check comma-based portals via groupPortalIndex fuzzy matching.
	//    We intentionally skip the guid mismatch rejection here because the
	//    whole point is to find portals where the guid differs (another client
	//    using a different gid for the same group).
	c.ensureGroupPortalIndex()
	deduped := c.buildCanonicalParticipantList(normalizedParts)

	overlap := make(map[string]int)
	c.groupPortalMu.RLock()
	for _, member := range deduped {
		for existingID := range c.groupPortalIndex[member] {
			overlap[existingID]++
		}
	}
	c.groupPortalMu.RUnlock()

	candidateSize := len(deduped)
	var exactMatch, fuzzyMatch string
	for existingID, sharedCount := range overlap {
		existingMembers := strings.Split(existingID, ",")
		existingSize := len(existingMembers)
		diff := (candidateSize - sharedCount) + (existingSize - sharedCount)
		if diff > 1 {
			continue
		}
		if diff == 1 && !participantSetsMatch(deduped, existingMembers, c.isMyHandle) {
			continue // diff=1 for a non-self member → different group
		}
		key := networkid.PortalKey{ID: networkid.PortalID(existingID), Receiver: c.UserLogin.ID}
		if p, _ := c.Main.Bridge.GetExistingPortalByKey(ctx, key); p != nil && p.MXID != "" {
			if diff == 0 {
				exactMatch = existingID
				break // Can't do better than exact
			}
			if fuzzyMatch == "" {
				fuzzyMatch = existingID
			}
		}
	}
	if exactMatch != "" {
		c.UserLogin.Log.Info().
			Str("gid_portal_id", gidPortalID).
			Str("resolved_portal", exactMatch).
			Int("participant_diff", 0).
			Msg("Resolved unknown gid to existing comma-based portal via fuzzy match")
		return networkid.PortalID(exactMatch)
	}
	if fuzzyMatch != "" {
		c.UserLogin.Log.Info().
			Str("gid_portal_id", gidPortalID).
			Str("resolved_portal", fuzzyMatch).
			Int("participant_diff", 1).
			Msg("Resolved unknown gid to existing comma-based portal via fuzzy match")
		return networkid.PortalID(fuzzyMatch)
	}

	// 4. Fall back to cloud_chat DB for portals not in memory caches
	//    (e.g., gid: portals from CloudKit sync that haven't received
	//    live messages yet, so imGroupParticipants is empty).
	//    Only consider group portals (gid: or comma-based). DM portals
	//    can accidentally match via ±1 participant tolerance because a
	//    DM's [self, A] is one member short of a group's [self, A, B].
	if c.cloudStore != nil {
		matches, err := c.cloudStore.findPortalIDsByParticipants(ctx, normalizedParts, c.isMyHandle)
		if err == nil {
			for _, matchPortalID := range matches {
				if matchPortalID == gidPortalID {
					continue
				}
				// Skip DM portals — they can't be the same group.
				if !strings.HasPrefix(matchPortalID, "gid:") && !strings.Contains(matchPortalID, ",") {
					continue
				}
				key := networkid.PortalKey{ID: networkid.PortalID(matchPortalID), Receiver: c.UserLogin.ID}
				if p, _ := c.Main.Bridge.GetExistingPortalByKey(ctx, key); p != nil && p.MXID != "" {
					c.UserLogin.Log.Info().
						Str("gid_portal_id", gidPortalID).
						Str("resolved_portal", matchPortalID).
						Msg("Resolved unknown gid to existing portal via cloud_chat DB")
					return networkid.PortalID(matchPortalID)
				}
			}
		}
	}

	// No existing portal found — this is genuinely a new group.
	return networkid.PortalID(gidPortalID)
}

func (c *IMClient) makePortalKey(participants []string, groupName *string, sender *string, senderGuid *string) networkid.PortalKey {
	isGroup := c.getUniqueParticipantCount(participants) > 2 || (groupName != nil && *groupName != "")

	if isGroup {
		// When a persistent group UUID (sender_guid / gid) is available,
		// use "gid:<UUID>" as the stable portal ID. This avoids the
		// fragility of participant-based IDs that break when membership
		// changes or participants normalize differently.
		var portalID networkid.PortalID
		if senderGuid != nil && *senderGuid != "" {
			gidID := "gid:" + strings.ToLower(*senderGuid)

			// Fast path: check if we've previously resolved this gid to
			// a different portal (cached from a prior resolution).
			c.gidAliasesMu.RLock()
			aliasedID, hasAlias := c.gidAliases[gidID]
			c.gidAliasesMu.RUnlock()
			if hasAlias {
				// Canonicalize participants (collapses alternate self handles to
				// c.handle) before staleness check so a valid alias is never
				// evicted just because APNs reported self with a different identifier.
				// Guard on raw participants first: buildCanonicalParticipantList always
				// injects c.handle, so canonical is never empty even when the message
				// carried no participant info — a [self]-only list must not trigger eviction.
				canonical := c.buildCanonicalParticipantList(participants)
				if len(participants) > 0 && len(canonical) > 0 && c.guidCacheMatchIsStale(aliasedID, canonical) {
					c.gidAliasesMu.Lock()
					// Compare-before-delete: another handler may have repaired
					// the alias between our RLock read and this write lock.
					if c.gidAliases[gidID] == aliasedID {
						delete(c.gidAliases, gidID)
					}
					c.gidAliasesMu.Unlock()
					c.UserLogin.Log.Warn().
						Str("gid_id", gidID).
						Str("stale_alias", aliasedID).
						Msg("Cleared stale gid alias: participant mismatch")
					// Fall through to normal resolution below
				} else {
					portalID = networkid.PortalID(aliasedID)
				}
			}
			if portalID == "" {
				// Check if a portal with this exact gid already exists.
				ctx := context.Background()
				gidKey := networkid.PortalKey{ID: networkid.PortalID(gidID), Receiver: c.UserLogin.ID}
				if existing, _ := c.Main.Bridge.GetExistingPortalByKey(ctx, gidKey); existing != nil && existing.MXID != "" {
					portalID = networkid.PortalID(gidID)
				} else {
					// This gid doesn't match any existing portal. Another
					// rustpush client (like OpenBubbles) may use a different
					// gid for the same group. Try to resolve by participants.
					portalID = c.resolveExistingGroupByGid(gidID, *senderGuid, participants)
					// Cache the alias so subsequent messages with this gid
					// resolve instantly without repeated participant matching.
					if string(portalID) != gidID {
						c.gidAliasesMu.Lock()
						c.gidAliases[gidID] = string(portalID)
						c.gidAliasesMu.Unlock()
					}
				}
			}
		} else {
			// Fallback: build a participant-based ID for groups without a UUID.
			deduped := c.buildCanonicalParticipantList(participants)
			computedID := strings.Join(deduped, ",")
			portalID = c.resolveExistingGroupPortalID(computedID, senderGuid)
		}
		// Cache the actual iMessage group name (cv_name) so outbound
		// messages can route to the correct conversation. Also push a
		// room name update when the envelope name differs from what's
		// cached OR on the first message after restart (old is empty).
		// After restart, imGroupNames is empty and the portal may have
		// a stale name from CloudKit. Pushing on first message ensures
		// the correct cv_name from APNs overrides any stale data.
		// bridgev2's updateName deduplicates — no state event if the
		// name is already correct.
		if groupName != nil && *groupName != "" {
			c.imGroupNamesMu.Lock()
			old := c.imGroupNames[string(portalID)]
			c.imGroupNames[string(portalID)] = *groupName
			c.imGroupNamesMu.Unlock()
			if old != *groupName {
				newName := *groupName
				pid := string(portalID)
				go func() {
					c.UserLogin.QueueRemoteEvent(&simplevent.ChatInfoChange{
						EventMeta: simplevent.EventMeta{
							Type: bridgev2.RemoteEventChatInfoChange,
							PortalKey: networkid.PortalKey{
								ID:       portalID,
								Receiver: c.UserLogin.ID,
							},
							LogContext: func(lc zerolog.Context) zerolog.Context {
								return lc.Str("portal_id", pid).Str("source", "envelope_name_change")
							},
						},
						ChatInfoChange: &bridgev2.ChatInfoChange{
							ChatInfo: &bridgev2.ChatInfo{
								Name:                       &newName,
								ExcludeChangesFromTimeline: true,
							},
						},
					})
				}()
			}
		}
		// Cache normalized participants in memory so resolveGroupMembers
		// can find them immediately. The cloud_chat DB write happens async
		// and may not complete before GetChatInfo is called during portal
		// creation, which would cause the group name to resolve as "Group Chat".
		if len(participants) > 0 {
			normalized := make([]string, 0, len(participants))
			for _, p := range participants {
				n := normalizeIdentifierForPortalID(p)
				if n != "" {
					normalized = append(normalized, n)
				}
			}
			if len(normalized) > 0 {
				c.imGroupParticipantsMu.Lock()
				c.imGroupParticipants[string(portalID)] = normalized
				c.imGroupParticipantsMu.Unlock()
			}
		}
		portalKey := networkid.PortalKey{ID: portalID, Receiver: c.UserLogin.ID}

		// Cache the original-case sender_guid so outbound messages reuse the
		// same UUID casing and Apple Messages recipients match them to the
		// existing group thread (Apple matches case-sensitively).
		if senderGuid != nil && *senderGuid != "" {
			// Cache for legacy comma portals and for gid: portals where the
			// portal ID directly corresponds to this sender_guid. Skip aliased
			// portals (where resolveExistingGroupByGid mapped this gid to a
			// different existing portal) to avoid overwriting the original
			// portal's sender_guid.
			isOwnGidPortal := string(portalID) == "gid:"+strings.ToLower(*senderGuid)
			if strings.Contains(string(portalID), ",") || isOwnGidPortal {
				c.imGroupGuidsMu.Lock()
				c.imGroupGuids[string(portalID)] = *senderGuid
				c.imGroupGuidsMu.Unlock()
			}
		}

		// Persist sender_guid and group name to database so they survive restarts
		persistGuid := ""
		if senderGuid != nil {
			persistGuid = *senderGuid
		}
		persistName := ""
		if groupName != nil {
			persistName = *groupName
		}
		// Persist participants to cloud_chat so portalToConversation can
		// find them for outbound messages (even if CloudKit never synced this group).
		if strings.HasPrefix(string(portalID), "gid:") && len(participants) > 0 {
			go func(pk networkid.PortalKey, parts []string, guid string) {
				if c.cloudStore == nil {
					return
				}
				ctx := context.Background()
				// Only insert if no cloud_chat record exists yet
				existing, err := c.cloudStore.getChatParticipantsByPortalID(ctx, string(pk.ID))
				if err == nil && len(existing) > 0 {
					return // already have participants
				}
				if upsertErr := c.cloudStore.upsertChat(ctx, guid, "", guid, string(pk.ID), "iMessage", nil, nil, parts, 0); upsertErr != nil {
					c.Main.Bridge.Log.Warn().Err(upsertErr).Str("portal_id", string(pk.ID)).Msg("Failed to persist real-time group participants")
				} else {
					c.Main.Bridge.Log.Info().Str("portal_id", string(pk.ID)).Int("participants", len(parts)).Msg("Persisted real-time group participants to cloud_chat")
				}
			}(portalKey, participants, persistGuid)
		}

		if persistGuid != "" || persistName != "" {
			go func(pk networkid.PortalKey, guid, gname string) {
				ctx := context.Background()
				portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, pk)
				if err == nil && portal != nil {
					meta := &PortalMetadata{}
					if existing, ok := portal.Metadata.(*PortalMetadata); ok {
						*meta = *existing
					}
					changed := false
					if guid != "" && meta.SenderGuid != guid {
						meta.SenderGuid = guid
						changed = true
					}
					if gname != "" && meta.GroupName != gname {
						meta.GroupName = gname
						changed = true
					}
					if changed {
						portal.Metadata = meta
						_ = portal.Save(ctx)
					}
				}
			}(portalKey, persistGuid, persistName)
		}
		// Track which group each member last sent a message in, so typing
		// indicators (which lack full participant lists) can be routed.
		if sender != nil && *sender != "" {
			normalized := normalizeIdentifierForPortalID(*sender)
			if normalized != "" && !c.isMyHandle(normalized) {
				c.lastGroupForMemberMu.Lock()
				c.lastGroupForMember[normalized] = portalKey
				c.lastGroupForMemberMu.Unlock()
			}
		}
		return portalKey
	}

	for _, p := range participants {
		normalized := normalizeIdentifierForPortalID(p)
		if normalized != "" && !c.isMyHandle(normalized) {
			// Resolve to an existing portal if the contact has multiple phone numbers.
			// This ensures messages from any of a contact's numbers land in one room.
			portalID := c.resolveContactPortalID(normalized)
			portalID = c.resolveExistingDMPortalID(string(portalID))
			return networkid.PortalKey{
				ID:       portalID,
				Receiver: c.UserLogin.ID,
			}
		}
	}

	// SMS edge case: some payloads include only the local forwarding number in
	// participants. When that happens, use sender as the DM portal identifier.
	if sender != nil && *sender != "" {
		normalizedSender := normalizeIdentifierForPortalID(*sender)
		if normalizedSender != "" && !c.isMyHandle(normalizedSender) {
			portalID := c.resolveContactPortalID(normalizedSender)
			portalID = c.resolveExistingDMPortalID(string(portalID))
			return networkid.PortalKey{
				ID:       portalID,
				Receiver: c.UserLogin.ID,
			}
		}
	}

	if len(participants) > 0 {
		normalized := normalizeIdentifierForPortalID(participants[0])
		if normalized == "" {
			normalized = participants[0]
		}
		portalID := c.resolveExistingDMPortalID(normalized)
		return networkid.PortalKey{
			ID:       portalID,
			Receiver: c.UserLogin.ID,
		}
	}

	return networkid.PortalKey{ID: "unknown", Receiver: c.UserLogin.ID}
}

// makeReceiptPortalKey handles receipt messages where participants may be empty.
// When participants is empty (rustpush sets conversation: None for receipts),
// use the sender field to identify the DM portal.
func (c *IMClient) makeReceiptPortalKey(participants []string, groupName *string, sender *string, senderGuid *string) networkid.PortalKey {
	if len(participants) > 0 {
		return c.makePortalKey(participants, groupName, sender, senderGuid)
	}
	if sender != nil && *sender != "" {
		// Resolve to existing portal for contacts with multiple numbers
		normalizedSender := normalizeIdentifierForPortalID(*sender)
		if normalizedSender == "" {
			return networkid.PortalKey{ID: "unknown", Receiver: c.UserLogin.ID}
		}
		portalID := c.resolveContactPortalID(normalizedSender)
		portalID = c.resolveExistingDMPortalID(string(portalID))
		return networkid.PortalKey{
			ID:       portalID,
			Receiver: c.UserLogin.ID,
		}
	}
	return networkid.PortalKey{ID: "unknown", Receiver: c.UserLogin.ID}
}

func (c *IMClient) makeConversation(participants []string, groupName *string) rustpushgo.WrappedConversation {
	return rustpushgo.WrappedConversation{
		Participants: participants,
		GroupName:    groupName,
	}
}

func (c *IMClient) portalToConversation(portal *bridgev2.Portal) rustpushgo.WrappedConversation {
	portalID := string(portal.ID)
	isSms := c.isPortalSMS(portalID)

	isGroup := strings.HasPrefix(portalID, "gid:") || strings.Contains(portalID, ",")
	if isGroup {
		var participants []string
		var guid string
		if strings.HasPrefix(portalID, "gid:") {
			// Prefer original-case sender_guid from cache (populated by
			// makePortalKey on incoming messages and loadSenderGuidsFromDB
			// on restart). Falls back to metadata, then portal ID (lossy).
			c.imGroupGuidsMu.RLock()
			guid = c.imGroupGuids[portalID]
			c.imGroupGuidsMu.RUnlock()
			if guid == "" {
				if meta, ok := portal.Metadata.(*PortalMetadata); ok && meta.SenderGuid != "" {
					guid = meta.SenderGuid
				}
			}
			if guid == "" {
				// Last resort: extract from portal ID (lowercase, lossy)
				guid = strings.TrimPrefix(portalID, "gid:")
			}
			// Look up participants from cloud store
			if c.cloudStore != nil {
				ctx := context.Background()
				if parts, err := c.cloudStore.getChatParticipantsByPortalID(ctx, portalID); err == nil && len(parts) > 0 {
					participants = parts
				}
			}
		} else {
			participants = strings.Split(portalID, ",")
		}

		// Use the actual iMessage group name (cv_name) from the protocol,
		// NOT the bridge-generated display name (portal.Name). Using the
		// bridge display name causes Messages.app to split conversations.
		c.imGroupNamesMu.RLock()
		name := c.imGroupNames[portalID]
		c.imGroupNamesMu.RUnlock()
		if name == "" {
			// Not in memory cache - try loading from portal metadata
			if meta, ok := portal.Metadata.(*PortalMetadata); ok && meta.GroupName != "" {
				name = meta.GroupName
				c.imGroupNamesMu.Lock()
				c.imGroupNames[portalID] = name
				c.imGroupNamesMu.Unlock()
			}
		}
		var groupName *string
		if name != "" {
			groupName = &name
		}

		// For gid: portals, guid is already set above (cache/metadata/portal ID).
		// For legacy comma-separated portals, look up from cache/metadata.
		if guid == "" {
			c.imGroupGuidsMu.RLock()
			guid = c.imGroupGuids[portalID]
			c.imGroupGuidsMu.RUnlock()
			if guid == "" {
				if meta, ok := portal.Metadata.(*PortalMetadata); ok && meta.SenderGuid != "" {
					guid = meta.SenderGuid
					c.imGroupGuidsMu.Lock()
					c.imGroupGuids[portalID] = guid
					c.imGroupGuidsMu.Unlock()
				}
			}
		}
		var senderGuid *string
		if guid != "" {
			senderGuid = &guid
		}
		return rustpushgo.WrappedConversation{
			Participants: participants,
			GroupName:    groupName,
			SenderGuid:   senderGuid,
			IsSms:        isSms,
		}
	}

	// For DMs, resolve the best sendable identifier. For merged contacts,
	// the portal ID might be an inactive number that rustpush can't send to.
	// Strip any legacy (sms...) suffix before resolution — resolveSendTarget
	// calls lookupContact → stripIdentifierPrefix, which would pass the suffix
	// through to contact lookup and break alternate-handle resolution.
	cleanPortalID := stripSmsSuffix(portalID)
	sendTo := c.resolveSendTarget(cleanPortalID)

	// For self-chats, only include one participant. Duplicating our own
	// handle (e.g. [self, self]) causes rustpush to reject the message
	// with NoValidTargets because all targets belong to the sender.
	participants := []string{c.handle, sendTo}
	if c.isMyHandle(sendTo) {
		participants = []string{sendTo}
	}

	return rustpushgo.WrappedConversation{
		Participants: participants,
		IsSms:        isSms,
	}
}

// persistGroupParticipantsIfChanged keeps cloud_chat.participants_json — the
// source of truth for a gid: group's OUTBOUND recipient list (see
// portalToConversation) — in sync with the freshest authoritative roster.
//
// The real-time persist in makePortalKey is insert-once, so without this a
// member who leaves (or is added) is never dropped from / added to the send
// list: the bridge keeps encrypting every outgoing message to the stale
// roster, so the departed member keeps receiving messages. handleMessage calls
// this on every live group message, which also self-heals a membership change
// that arrived while the bridge was offline (delivered later as a stored
// message and skipped by handleParticipantChange).
//
// gid-only: comma-separated portal IDs encode their members in the ID itself
// and re-key via ReIDPortal, so their participants_json is not the send source
// and must not be touched here.
func (c *IMClient) persistGroupParticipantsIfChanged(portalID string, participants []string) {
	if c.cloudStore == nil || !strings.HasPrefix(portalID, "gid:") {
		return
	}
	// A real group roster always has at least two members. Guard against a
	// fluke partial/control-message list shrinking the stored roster.
	normalized := make([]string, 0, len(participants))
	for _, p := range participants {
		if n := normalizeIdentifierForPortalID(p); n != "" {
			normalized = append(normalized, n)
		}
	}
	if len(normalized) < 2 {
		return
	}
	go func(pid string, parts []string) {
		oldCount, updated, err := syncStoredGroupRoster(context.Background(), c.cloudStore, pid, parts, c.isMyHandle)
		if err != nil {
			c.Main.Bridge.Log.Warn().Err(err).Str("portal_id", pid).
				Msg("group roster sync: failed to refresh stored participants")
			return
		}
		if updated {
			c.Main.Bridge.Log.Info().
				Str("portal_id", pid).
				Int("old_count", oldCount).
				Int("new_count", len(parts)).
				Msg("group roster sync: refreshed stored participants (membership changed)")
		}
	}(portalID, normalized)
}

// resolveGroupMembers returns the participant list for a group portal.
// For gid: portals it checks the in-memory cache first (populated synchronously
// by makePortalKey), then the cloud store DB; for legacy comma-separated
// portal IDs it splits the ID string.
func (c *IMClient) resolveGroupMembers(ctx context.Context, portalID string) []string {
	if strings.HasPrefix(portalID, "gid:") {
		// 1) Check cloud store DB (persisted from CloudKit sync or previous messages)
		if c.cloudStore != nil {
			if participants, err := c.cloudStore.getChatParticipantsByPortalID(ctx, portalID); err == nil && len(participants) > 0 {
				return participants
			}
		}
		// 2) Fallback to in-memory cache (populated synchronously by makePortalKey).
		// The cloud_chat DB write is async and may not have completed yet when
		// GetChatInfo is called during portal creation from a real-time message.
		c.imGroupParticipantsMu.RLock()
		cached := c.imGroupParticipants[portalID]
		c.imGroupParticipantsMu.RUnlock()
		if len(cached) > 0 {
			return cached
		}
		return nil
	}
	return strings.Split(portalID, ",")
}

// resolveGroupName determines the best display name for a group portal.
// Returns the name and whether it came from an authoritative source
// (imGroupNames cache or CloudKit display_name). When authoritative is
// false, the name was generated from contact-resolved participant names.
// Priority: 1) in-memory cache (user-set iMessage group name from real-time
//
//	   protocol cv_name, e.g. when someone explicitly renames a group)
//	2) CloudKit display_name (user-set group name persisted to iCloud,
//	   the "name" field on CKChatRecord = cv_name from chat.db)
//	3) contact-resolved member names via buildGroupName (non-authoritative)
func (c *IMClient) resolveGroupName(ctx context.Context, portalID string) (name string, authoritative bool) {
	// 1) In-memory cache (populated from real-time iMessage rename messages)
	c.imGroupNamesMu.RLock()
	cached := c.imGroupNames[portalID]
	c.imGroupNamesMu.RUnlock()
	if cached != "" {
		return cached, true
	}

	// 2) CloudKit display_name (user-set group name from iCloud).
	if c.cloudStore != nil {
		if dn, err := c.cloudStore.getDisplayNameByPortalID(ctx, portalID); err == nil && dn != "" {
			return dn, true
		}
	}

	// 3) Build from contact-resolved member names (fallback, non-authoritative)
	members := c.resolveGroupMembers(ctx, portalID)
	if len(members) == 0 {
		return "Group Chat", false
	}
	return c.buildGroupName(members), false
}

// buildGroupName creates a human-readable group name from member identifiers
// by resolving contact names where possible, falling back to phone/email.
func (c *IMClient) buildGroupName(members []string) string {
	var names []string
	for _, memberID := range members {
		if c.isMyHandle(memberID) {
			continue // skip self
		}
		// Strip tel:/mailto: prefix for contact lookup
		lookupID := stripIdentifierPrefix(memberID)
		name := ""
		var contact *imessage.Contact
		if c.contacts != nil {
			var contactErr error
			contact, contactErr = c.contacts.GetContactInfo(lookupID)
			if contactErr != nil {
				c.Main.Bridge.Log.Debug().Err(contactErr).Str("id", lookupID).Msg("Failed to resolve contact info")
			}
		}

		if contact != nil && contact.HasName() {
			name = c.Main.Config.FormatDisplayname(DisplaynameParams{
				FirstName: contact.FirstName,
				LastName:  contact.LastName,
				Nickname:  contact.Nickname,
				ID:        lookupID,
			})
		}
		if name == "" {
			name = lookupID // raw phone/email without prefix
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return "Group Chat"
	}
	if len(names) <= 4 {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, %s, %s +%d more", names[0], names[1], names[2], len(names)-3)
}

// ============================================================================
// Message conversion
// ============================================================================

type attachmentMessage struct {
	*rustpushgo.WrappedMessage
	Attachment *rustpushgo.WrappedAttachment
	Index      int
}

// stickerTapbackData carries the image bytes for a sticker placed on a
// message bubble (iMessage sticker tapback, type 7). Bridged as an image
// message replying to the target since Matrix reactions are text-only.
type stickerTapbackData struct {
	ImageData []byte
	MimeType  string
	TargetID  string // UUID of the message the sticker was placed on
}

func convertStickerTapback(ctx context.Context, intent bridgev2.MatrixAPI, data *stickerTapbackData) (*bridgev2.ConvertedMessage, error) {
	content := &event.MessageEventContent{
		MsgType: event.MsgImage,
		Body:    "sticker.png",
		Info: &event.FileInfo{
			MimeType: data.MimeType,
			Size:     len(data.ImageData),
		},
	}
	if intent != nil {
		url, encFile, err := intent.UploadMedia(ctx, "", data.ImageData, "sticker.png", data.MimeType)
		if err != nil {
			return nil, fmt.Errorf("failed to upload sticker: %w", err)
		}
		if encFile != nil {
			content.File = encFile
		} else {
			content.URL = url
		}
	}
	cm := &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{{
			Type:    event.EventMessage,
			Content: content,
		}},
	}
	if data.TargetID != "" {
		cm.ReplyTo = &networkid.MessageOptionalPartID{MessageID: makeMessageID(data.TargetID)}
	}
	return cm, nil
}

// convertURLPreviewToBeeper parses rich link sideband attachments from an
// inbound iMessage and returns Beeper link previews. Follows the pattern
// from mautrix-whatsapp's urlpreview.go.
func convertURLPreviewToBeeper(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, msg *rustpushgo.WrappedMessage, bodyText string) []*event.BeeperLinkPreview {
	log := zerolog.Ctx(ctx)

	// Find sideband attachments encoded by Rust
	var rlMeta, rlImage *rustpushgo.WrappedAttachment
	for i := range msg.Attachments {
		switch msg.Attachments[i].MimeType {
		case "x-richlink/meta":
			rlMeta = &msg.Attachments[i]
		case "x-richlink/image":
			rlImage = &msg.Attachments[i]
		}
	}

	if rlMeta != nil && rlMeta.InlineData != nil {
		fields := bytes.SplitN(*rlMeta.InlineData, []byte{0x01}, 5)
		originalURL := string(fields[0])
		canonicalURL := originalURL
		if len(fields) > 1 && len(fields[1]) > 0 {
			canonicalURL = string(fields[1])
		}
		title := ""
		if len(fields) > 2 && len(fields[2]) > 0 {
			title = string(fields[2])
		}
		description := ""
		if len(fields) > 3 && len(fields[3]) > 0 {
			description = string(fields[3])
		}
		imageMime := ""
		if len(fields) > 4 && len(fields[4]) > 0 {
			imageMime = string(fields[4])
		}

		log.Debug().
			Str("original_url_host", logSafeURL(originalURL)).
			Str("canonical_url_host", logSafeURL(canonicalURL)).
			Bool("has_title", title != "").
			Bool("has_description", description != "").
			Str("image_mime", imageMime).
			Msg("Parsed rich link sideband data from iMessage")

		// MatchedURL must exactly match a URL in the body text so Beeper
		// can associate the preview with the inline URL. Use regex to find
		// the URL in the body rather than trusting the NSURL-converted value.
		matchedURL := originalURL
		if bodyURL := urlRegex.FindString(bodyText); bodyURL != "" {
			matchedURL = bodyURL
		}

		preview := &event.BeeperLinkPreview{
			MatchedURL: matchedURL,
			LinkPreview: event.LinkPreview{
				CanonicalURL: canonicalURL,
				Title:        title,
				Description:  description,
			},
		}

		// Upload preview image if available
		if rlImage != nil && rlImage.InlineData != nil && intent != nil {
			if imageMime == "" {
				imageMime = "image/jpeg"
			}
			log.Debug().Int("image_bytes", len(*rlImage.InlineData)).Str("mime", imageMime).Msg("Uploading rich link preview image")
			url, encFile, err := intent.UploadMedia(ctx, portal.MXID, *rlImage.InlineData, "preview", imageMime)
			if err == nil {
				if encFile != nil {
					preview.ImageEncryption = encFile
					preview.ImageURL = encFile.URL
				} else {
					preview.ImageURL = url
				}
				preview.ImageType = imageMime
			} else {
				log.Warn().Err(err).Msg("Failed to upload rich link preview image")
			}
		}

		log.Debug().Str("matched_url_host", logSafeURL(matchedURL)).Bool("has_title", title != "").Msg("Inbound rich link preview ready")
		return []*event.BeeperLinkPreview{preview}
	}

	// No rich link from iMessage — auto-detect URL and fetch og: metadata + image
	if detectedURL := urlRegex.FindString(bodyText); detectedURL != "" {
		log.Debug().Str("detected_url_host", logSafeURL(detectedURL)).Msg("No iMessage rich link, fetching URL preview")
		return []*event.BeeperLinkPreview{fetchURLPreview(ctx, portal.Bridge, intent, portal.MXID, detectedURL)}
	}

	return nil
}

func convertMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, msg *rustpushgo.WrappedMessage) (*bridgev2.ConvertedMessage, error) {
	text := strings.TrimSpace(strings.ReplaceAll(ptrStringOr(msg.Text, ""), "\uFFFC", ""))
	content := &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    text,
	}
	if msg.Subject != nil && *msg.Subject != "" {
		if text != "" {
			content.Body = fmt.Sprintf("**%s**\n%s", *msg.Subject, text)
			content.Format = event.FormatHTML
			content.FormattedBody = fmt.Sprintf("<strong>%s</strong><br/>%s", *msg.Subject, text)
		} else {
			content.Body = *msg.Subject
		}
	}

	content.BeeperLinkPreviews = convertURLPreviewToBeeper(ctx, portal, intent, msg, text)

	cm := &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{{
			Type:    event.EventMessage,
			Content: content,
		}},
	}

	if msg.ReplyGuid != nil && *msg.ReplyGuid != "" {
		bp := 0
		if msg.ReplyPart != nil {
			bp = parseBalloonPart(*msg.ReplyPart, "%d:")
		}
		cm.ReplyTo = chatDBReplyTarget(*msg.ReplyGuid, bp)
	}

	return cm, nil
}

func isVCardAttachment(mimeType, fileName, utiType string) bool {
	if strings.EqualFold(utiType, "public.vcard") {
		return true
	}
	if strings.EqualFold(mimeType, "text/vcard") ||
		strings.EqualFold(mimeType, "text/x-vcard") ||
		strings.EqualFold(mimeType, "text/directory") {
		return true
	}
	return strings.HasSuffix(strings.ToLower(fileName), ".vcf")
}

func makeVCardPreviewContent(data []byte) *event.MessageEventContent {
	contact := parseVCard(string(data))
	if contact == nil {
		return nil
	}
	name := strings.TrimSpace(contact.Name())
	if name == "" && len(contact.Phones) == 0 && len(contact.Emails) == 0 {
		return nil
	}

	bodyLines := []string{"Shared contact"}
	htmlLines := []string{"<strong>Shared contact</strong>"}
	if name != "" {
		bodyLines = append(bodyLines, name)
		htmlLines = append(htmlLines, html.EscapeString(name))
	}
	for i, phone := range contact.Phones {
		if i >= 3 {
			break
		}
		phone = strings.TrimSpace(phone)
		if phone == "" {
			continue
		}
		bodyLines = append(bodyLines, "Phone: "+phone)
		htmlLines = append(htmlLines, "Phone: "+html.EscapeString(phone))
	}
	for i, email := range contact.Emails {
		if i >= 3 {
			break
		}
		email = strings.TrimSpace(email)
		if email == "" {
			continue
		}
		bodyLines = append(bodyLines, "Email: "+email)
		htmlLines = append(htmlLines, "Email: "+html.EscapeString(email))
	}

	return &event.MessageEventContent{
		MsgType:       event.MsgNotice,
		Body:          strings.Join(bodyLines, "\n"),
		Format:        event.FormatHTML,
		FormattedBody: strings.Join(htmlLines, "<br/>"),
		Mentions:      &event.Mentions{},
	}
}

const (
	// transcodeBackoffInitial is the first wait between transcode retries; it
	// doubles after each failure.
	transcodeBackoffInitial = 3 * time.Second
	// transcodeRetryCadence caps the INTERVAL between retries (not their
	// count): an uncapped exponential interval would double to hours, so it
	// settles at this steady cadence.
	transcodeRetryCadence = 60 * time.Second
	// transcodeMaxAttempts bounds the TOTAL transcode attempts before we give
	// up and ship the original. Transient memory-pressure failures (the ffmpeg
	// subprocess OOM-killed) are worth retrying — by the next attempt other
	// work has often freed memory — but NOT forever. Transcodes are serialized,
	// so a re-encode that is OOM-killed while running alone means this host
	// simply can't fit this video's encode in memory and it will be killed on
	// every attempt; retrying that forever would pin the attachment's blob and
	// its backfill download slot and stall preUploadCloudAttachments' wg.Wait()
	// indefinitely — backfill would never complete, which under Docker looks
	// exactly like a memory leak. A generous cap rides out genuine transient
	// spikes, then degrades gracefully to the original file (~9 min at the
	// cadence above).
	transcodeMaxAttempts = 12
	// transcodeMaxDeterministicFailures gives up much sooner for a
	// DETERMINISTIC failure — ffmpeg ran to completion and exited non-zero
	// (unsupported codec, corrupt blob, or a build missing the libx264/aac
	// encoder). Waiting for memory can't fix those. A small allowance absorbs
	// the rare transient that surfaces as an exit code instead of a kill.
	transcodeMaxDeterministicFailures = 3
)

// transcodeSem serializes ffmpeg conversions process-wide. The dominant cause
// of OOM on low-memory hosts (Raspberry Pi, small VPS, low-memory Mac) is many
// backfill attachments transcoding at once; capping concurrency to a single
// ffmpeg run bounds peak memory. The slot is held only for the duration of each
// ffmpeg invocation and released during the backoff wait, so a struggling job
// never starves the others.
var transcodeSem = make(chan struct{}, 1)

// transcodeFailureIsDeterministic reports whether an ffmpeg failure will never
// be fixed by retrying. ffmpeg.ConvertBytes shells out to a subprocess, so:
//   - terminated by a signal (ExitCode() == -1, the OOM-killer's SIGKILL) →
//     transient: the box was briefly out of memory, retry will likely succeed.
//   - exited with a non-zero status (ExitCode() >= 0) → deterministic: ffmpeg
//     ran to completion and rejected the input (bad codec, corrupt data, or a
//     build missing the encoder). Retrying changes nothing.
//
// Anything that isn't an *exec.ExitError (e.g. fork/exec itself failed because
// the host couldn't spawn the subprocess under memory pressure) is treated as
// transient so we keep trying.
func transcodeFailureIsDeterministic(err error) bool {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode() >= 0
	}
	return false
}

// transcodeToMP4 remuxes (cheap) or, failing that, re-encodes data to MP4. It
// is serialized against other transcodes (one ffmpeg at a time → bounded peak
// memory) and retries with exponential backoff, then degrades to shipping the
// original. Failures fall in two buckets:
//
//   - Transient (the ffmpeg subprocess was OOM-killed by a signal): worth
//     retrying, since other work may free memory before the next attempt — but
//     only up to transcodeMaxAttempts. Because transcodes are serialized, an
//     encode OOM-killed while running alone means the host can't fit it in
//     memory and will kill it every time; retrying forever would pin the blob
//     and its backfill download slot and stall the backfill's wg.Wait()
//     indefinitely (indistinguishable from a memory leak under Docker).
//   - Deterministic (ffmpeg ran and exited non-zero — unsupported codec,
//     corrupt data, or a build missing the encoder): capped much sooner, since
//     waiting for memory can't fix it.
//
// The ffmpeg run is detached from the parent context's deadline so a short
// inbound deadline can't abort it mid-encode; bridge shutdown (ctx cancelled)
// still exits promptly. On any give-up path the caller ships the original.
func transcodeToMP4(ctx context.Context, log *zerolog.Logger, data []byte, mimeType string) (converted []byte, method string, err error) {
	base := context.WithoutCancel(ctx)
	// runOnce acquires the process-wide slot, runs ffmpeg, and releases the slot
	// even if ffmpeg panics — a bare release could leak the only slot and
	// deadlock all transcoding process-wide.
	runOnce := func() ([]byte, string, error) {
		select {
		case transcodeSem <- struct{}{}:
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
		defer func() { <-transcodeSem }()
		out, e := ffmpeg.ConvertBytes(base, data, ".mp4", nil,
			[]string{"-c", "copy", "-movflags", "+faststart"},
			mimeType)
		if e != nil {
			// Remux failed (often an incompatible codec) — try a full re-encode.
			out, e = ffmpeg.ConvertBytes(base, data, ".mp4", nil,
				[]string{"-c:v", "libx264", "-preset", "fast", "-crf", "23",
					"-c:a", "aac", "-movflags", "+faststart"},
				mimeType)
			return out, "re-encode", e
		}
		return out, "remux", e
	}
	backoff := transcodeBackoffInitial
	deterministicFailures := 0
	for attempt := 1; attempt <= transcodeMaxAttempts; attempt++ {
		converted, method, err = runOnce()
		if err == nil {
			return converted, method, nil
		}
		// Shutdown (or a cancelled acquire) — give up, caller ships original.
		if ctx.Err() != nil {
			return nil, method, ctx.Err()
		}
		if transcodeFailureIsDeterministic(err) {
			deterministicFailures++
			if deterministicFailures >= transcodeMaxDeterministicFailures {
				log.Warn().Err(err).Int("attempt", attempt).Int("bytes", len(data)).
					Msg("Video transcode failing deterministically (unsupported codec, corrupt input, or ffmpeg missing the encoder); giving up and shipping original")
				return nil, method, err
			}
		} else {
			// Transient (memory pressure) — reset the deterministic streak so a
			// kill never counts toward the deterministic give-up.
			deterministicFailures = 0
		}
		if attempt == transcodeMaxAttempts {
			break // don't sleep after the final attempt
		}
		log.Warn().Err(err).Int("attempt", attempt).Int("max_attempts", transcodeMaxAttempts).
			Dur("backoff", backoff).Int("bytes", len(data)).Bool("deterministic", deterministicFailures > 0).
			Msg("Video transcode failed; backing off before retry")
		select {
		case <-ctx.Done():
			return nil, method, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > transcodeRetryCadence {
			backoff = transcodeRetryCadence
		}
	}
	log.Warn().Err(err).Int("attempts", transcodeMaxAttempts).Int("bytes", len(data)).
		Msg("Video transcode still failing after max attempts (host likely can't fit this encode in memory); giving up and shipping original")
	return nil, method, err
}

func convertAttachment(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, attMsg *attachmentMessage, videoTranscoding, heicConversion bool, heicQuality int) (*bridgev2.ConvertedMessage, error) {
	att := attMsg.Attachment
	mimeType := att.MimeType
	fileName := att.Filename
	var durationMs int

	// Convert CAF Opus voice messages to OGG Opus for Matrix clients
	var inlineData []byte
	zerolog.Ctx(ctx).Debug().Bool("is_inline", att.IsInline).Bool("has_data", att.InlineData != nil).Str("mime", mimeType).Str("file", fileName).Uint64("size", att.Size).Msg("convertAttachment called")
	if att.IsInline && att.InlineData != nil {
		inlineData = *att.InlineData
		if att.UtiType == "com.apple.coreaudio-format" || mimeType == "audio/x-caf" {
			inlineData, mimeType, fileName, durationMs = convertAudioForMatrix(inlineData, mimeType, fileName)
		}
	}

	// Guard: no payload means the MMCS download failed upstream after the
	// rustpushgo retry exhausted (download_one_mmcs_attachment at
	// pkg/rustpushgo/src/lib.rs logs the specific error). Without this guard
	// we'd fall through to build an m.image/m.video/m.file event with no
	// url/file field, and clients render that as "Unsupported attachment
	// type: IMAGE (<body>)". Emit an m.notice placeholder instead — mirrors
	// the CloudKit backfill fallback a few thousand lines up in this file.
	if inlineData == nil {
		zerolog.Ctx(ctx).Warn().
			Str("file", fileName).
			Str("mime", mimeType).
			Uint64("size", att.Size).
			Msg("Attachment has no payload — MMCS download failed; emitting notice placeholder")
		return &bridgev2.ConvertedMessage{
			Parts: []*bridgev2.ConvertedMessagePart{{
				ID:   networkid.PartID(fmt.Sprintf("att%d", attMsg.Index)),
				Type: event.EventMessage,
				Content: &event.MessageEventContent{
					MsgType: event.MsgNotice,
					Body:    fmt.Sprintf("Attachment could not be downloaded (%s).", fileName),
				},
			}},
		}, nil
	}

	var vcardPreview *event.MessageEventContent
	if inlineData != nil && isVCardAttachment(mimeType, fileName, att.UtiType) {
		vcardPreview = makeVCardPreviewContent(inlineData)
	}

	// Remux/transcode non-MP4 videos to MP4 for broad Matrix client compatibility.
	if inlineData != nil && videoTranscoding && ffmpeg.Supported() && strings.HasPrefix(mimeType, "video/") && mimeType != "video/mp4" {
		log := zerolog.Ctx(ctx)
		origMime := mimeType
		origSize := len(inlineData)
		converted, method, convertErr := transcodeToMP4(ctx, log, inlineData, mimeType)
		if convertErr != nil {
			log.Warn().Err(convertErr).Str("original_mime", origMime).
				Msg("FFmpeg video conversion failed after retries, uploading original")
		} else {
			log.Info().Str("original_mime", origMime).
				Str("method", method).Int("original_bytes", origSize).Int("converted_bytes", len(converted)).
				Msg("Video transcoded to MP4")
			inlineData = converted
			mimeType = "video/mp4"
			fileName = strings.TrimSuffix(fileName, filepath.Ext(fileName)) + ".mp4"
		}
	}

	// Convert HEIC/HEIF images to JPEG since most Matrix clients can't display HEIC.
	var heicImg image.Image
	if inlineData != nil {
		log := zerolog.Ctx(ctx)
		inlineData, mimeType, fileName, heicImg = maybeConvertHEIC(log, inlineData, mimeType, fileName, heicQuality, heicConversion)
	}

	// Process images: extract dimensions, convert non-JPEG to JPEG, generate thumbnail
	var imgWidth, imgHeight int
	var thumbData []byte
	var thumbW, thumbH int
	if heicImg != nil {
		// Use the already-decoded image from HEIC conversion
		b := heicImg.Bounds()
		imgWidth, imgHeight = b.Dx(), b.Dy()
		if imgWidth > 800 || imgHeight > 800 {
			thumbData, thumbW, thumbH = scaleAndEncodeThumb(heicImg, imgWidth, imgHeight)
		}
	} else if inlineData != nil && (strings.HasPrefix(mimeType, "image/") || looksLikeImage(inlineData)) {
		log := zerolog.Ctx(ctx)
		log.Debug().Str("mime_type", mimeType).Str("file_name", fileName).Int("data_len", len(inlineData)).Msg("Processing image attachment")
		if mimeType == "image/gif" {
			cfg, _, err := image.DecodeConfig(bytes.NewReader(inlineData))
			if err == nil {
				imgWidth, imgHeight = cfg.Width, cfg.Height
			}
		} else if img, fmtName, _ := decodeImageData(inlineData); img != nil {
			b := img.Bounds()
			imgWidth, imgHeight = b.Dx(), b.Dy()
			log.Debug().Str("decoded_format", fmtName).Int("width", imgWidth).Int("height", imgHeight).Msg("Image decoded successfully")
			// Re-encode TIFF as JPEG for compatibility (PNG is fine as-is)
			if fmtName == "tiff" {
				var buf bytes.Buffer
				if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err == nil {
					inlineData = buf.Bytes()
					mimeType = "image/jpeg"
					fileName = strings.TrimSuffix(fileName, filepath.Ext(fileName)) + ".jpg"
					log.Debug().Int("jpeg_size", len(inlineData)).Msg("Re-encoded TIFF as JPEG")
				} else {
					log.Warn().Err(err).Msg("Failed to re-encode TIFF as JPEG")
				}
			}
			if imgWidth > 800 || imgHeight > 800 {
				thumbData, thumbW, thumbH = scaleAndEncodeThumb(img, imgWidth, imgHeight)
			}
		} else {
			log.Warn().Str("mime_type", mimeType).Msg("Failed to decode image data")
			// Log first few bytes for debugging
			if len(inlineData) >= 4 {
				log.Debug().Hex("magic_bytes", inlineData[:4]).Msg("Image magic bytes")
			}
		}
	}

	msgType := mimeToMsgType(mimeType)

	fileSize := int(att.Size)
	if inlineData != nil {
		fileSize = len(inlineData)
	}
	content := &event.MessageEventContent{
		MsgType: msgType,
		Body:    fileName,
		Info: &event.FileInfo{
			MimeType: mimeType,
			Size:     fileSize,
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
		content.Info.Size = len(inlineData)
	}

	if inlineData != nil && intent != nil {
		url, encFile, err := intent.UploadMedia(ctx, "", inlineData, fileName, mimeType)
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
			thumbURL, thumbEnc, err := intent.UploadMedia(ctx, "", thumbData, "thumbnail.jpg", "image/jpeg")
			if err == nil {
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
			} else {
				zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to upload image thumbnail")
			}
		}
	}

	parts := make([]*bridgev2.ConvertedMessagePart, 0, 2)
	if vcardPreview != nil {
		parts = append(parts, &bridgev2.ConvertedMessagePart{
			ID:      networkid.PartID(fmt.Sprintf("att%d-preview", attMsg.Index)),
			Type:    event.EventMessage,
			Content: vcardPreview,
		})
	}
	parts = append(parts, &bridgev2.ConvertedMessagePart{
		ID:      networkid.PartID(fmt.Sprintf("att%d", attMsg.Index)),
		Type:    event.EventMessage,
		Content: content,
	})

	cm := &bridgev2.ConvertedMessage{
		Parts: parts,
	}

	if attMsg.WrappedMessage.ReplyGuid != nil && *attMsg.WrappedMessage.ReplyGuid != "" {
		bp := 0
		if attMsg.WrappedMessage.ReplyPart != nil {
			bp = parseBalloonPart(*attMsg.WrappedMessage.ReplyPart, "%d:")
		}
		cm.ReplyTo = chatDBReplyTarget(*attMsg.WrappedMessage.ReplyGuid, bp)
	}

	return cm, nil
}

// resolveTapbackTargetID constructs the message ID for a tapback target,
// handling backward compatibility with messages stored before part-targeting
// was introduced. For bp >= 1 it tries the suffixed ID (e.g. "uuid_att0")
// first; if that message doesn't exist in the bridge DB it falls back to the
// bare UUID, which is how pre-existing messages were stored.
func (c *IMClient) resolveTapbackTargetID(targetGUID string, bp int) networkid.MessageID {
	if bp >= 1 {
		suffixed := fmt.Sprintf("%s_att%d", targetGUID, bp-1)
		suffixedID := makeMessageID(suffixed)
		msg, err := c.Main.Bridge.DB.Message.GetFirstPartByID(
			context.Background(), c.UserLogin.ID, suffixedID,
		)
		if err == nil && msg != nil {
			return suffixedID
		}
		// Suffixed ID not found — fall back to bare UUID for old messages.
	}
	return makeMessageID(targetGUID)
}

// ============================================================================
// Static helpers
// ============================================================================

// parseBalloonPart extracts an integer balloon-part index from s using the
// given fmt.Sscanf format string. Returns 0 if s is empty or parsing fails.
func parseBalloonPart(s, format string) int {
	var bp int
	fmt.Sscanf(s, format, &bp)
	return bp
}

// extractTapbackTarget splits a message ID that may contain an _attN suffix into
// the bare UUID and the iMessage tapback part index (0 = text body, ≥1 = attachment).
// The _attN index is 0-based (att0 = first attachment = part 1 in iMessage).
func extractTapbackTarget(messageID string) (string, uint64) {
	if idx := strings.Index(messageID, "_att"); idx > 0 {
		attIndex := parseBalloonPart(messageID[idx+4:], "%d")
		return messageID[:idx], uint64(attIndex) + 1
	}
	return messageID, 0
}

// extractReplyInfo converts a bridgev2 reply-to database message into the
// iMessage reply_guid and reply_part strings expected by rustpush.
// reply_guid is the message UUID; reply_part uses the iMessage format "bp:type:length".
// We don't have the original text length, so we use 0 as a placeholder.
func extractReplyInfo(replyTo *database.Message) (*string, *string) {
	if replyTo == nil {
		return nil, nil
	}
	guid := string(replyTo.ID)
	// Strip attachment suffixes like _att0, _att1 — iMessage expects a pure UUID,
	// but we derive the balloon-part index from the suffix to set reply_part correctly.
	bp := 0
	if idx := strings.Index(guid, "_att"); idx > 0 {
		bp = parseBalloonPart(guid[idx+4:], "%d") + 1
		guid = guid[:idx]
	}
	// iMessage thread_originator_part format is "bp:type:length" where:
	//   bp = balloon part index (0 for text body, ≥1 for attachments)
	//   type = part type (0 for text)
	//   length = character count of the original message text
	// We use 0 as the length since we don't have the original text available.
	part := fmt.Sprintf("%d:0:0", bp)
	return &guid, &part
}

// scaleAndEncodeThumb generates a JPEG thumbnail capped at 800px on the
// longest side using nearest-neighbor scaling (no external dependencies).
func scaleAndEncodeThumb(img image.Image, origW, origH int) ([]byte, int, int) {
	scale := min(800.0/float64(origW), 800.0/float64(origH))
	thumbW := int(float64(origW) * scale)
	thumbH := int(float64(origH) * scale)
	if thumbW < 1 {
		thumbW = 1
	}
	if thumbH < 1 {
		thumbH = 1
	}

	srcBounds := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, thumbW, thumbH))
	for y := range thumbH {
		srcY := srcBounds.Min.Y + y*srcBounds.Dy()/thumbH
		for x := range thumbW {
			srcX := srcBounds.Min.X + x*srcBounds.Dx()/thumbW
			dst.Set(x, y, img.At(srcX, srcY))
		}
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 75}); err != nil {
		return nil, 0, 0
	}
	return buf.Bytes(), thumbW, thumbH
}

// detectImageMIME returns the correct MIME type based on magic bytes.
func detectImageMIME(data []byte) string {
	if len(data) < 8 {
		return ""
	}
	if data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return "image/jpeg"
	}
	if data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47 {
		return "image/png"
	}
	if string(data[:4]) == "GIF8" {
		return "image/gif"
	}
	if (data[0] == 'I' && data[1] == 'I' && data[2] == 0x2a && data[3] == 0x00) ||
		(data[0] == 'M' && data[1] == 'M' && data[2] == 0x00 && data[3] == 0x2a) {
		return "image/tiff"
	}
	return ""
}

// looksLikeImage checks magic bytes to detect images even when MIME type is wrong.
func looksLikeImage(data []byte) bool {
	if len(data) < 8 {
		return false
	}
	// JPEG: FF D8 FF
	if data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return true
	}
	// PNG: 89 50 4E 47
	if data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47 {
		return true
	}
	// GIF: GIF8
	if string(data[:4]) == "GIF8" {
		return true
	}
	// TIFF: II*\0 or MM\0*
	if (data[0] == 'I' && data[1] == 'I' && data[2] == 0x2a && data[3] == 0x00) ||
		(data[0] == 'M' && data[1] == 'M' && data[2] == 0x00 && data[3] == 0x2a) {
		return true
	}
	return false
}

// decodeImageData tries to decode image bytes using stdlib decoders (PNG,
// JPEG, GIF) and falls back to a minimal TIFF parser. Returns the decoded
// image, detected format name, and whether the data is already JPEG (so
// callers can skip re-encoding).
func decodeImageData(data []byte) (image.Image, string, bool) {
	// Handles PNG, JPEG, GIF (stdlib) and TIFF (golang.org/x/image/tiff)
	if img, fmtName, err := image.Decode(bytes.NewReader(data)); err == nil {
		return img, fmtName, fmtName == "jpeg"
	}
	return nil, "", false
}

func tapbackTypeToEmoji(tapbackType *uint32, tapbackEmoji *string) string {
	if tapbackType == nil {
		return "❤️"
	}
	switch *tapbackType {
	case 0:
		return "❤️"
	case 1:
		return "👍"
	case 2:
		return "👎"
	case 3:
		return "😂"
	case 4:
		return "‼️"
	case 5:
		return "❓"
	case 6:
		if tapbackEmoji != nil {
			return *tapbackEmoji
		}
		return "👍"
	default:
		return "❤️"
	}
}

func emojiToTapbackType(emoji string) (uint32, *string) {
	switch emoji {
	case "❤️", "♥️":
		return 0, nil
	case "👍":
		return 1, nil
	case "👎":
		return 2, nil
	case "😂":
		return 3, nil
	case "❗", "‼️":
		return 4, nil
	case "❓":
		return 5, nil
	default:
		return 6, &emoji
	}
}

// formatSMSReactionText returns the SMS/RCS reaction text for a tapback with an
// empty quoted body (when the original message text is not available). The result
// matches Apple's SMS relay format exactly so the iPhone can thread it correctly.
func formatSMSReactionText(tapbackType uint32, customEmoji *string, isRemove bool) string {
	return formatSMSReactionTextWithBody(tapbackType, customEmoji, "", isRemove)
}

// formatSMSReactionTextWithBody returns the SMS/RCS reaction text with the
// original message body quoted inside the curly-quote delimiters.
// Mirrors the Rust ReactMessageType::get_text() output so SMS contacts see the
// same reaction text they would receive from a native iMessage client.
func formatSMSReactionTextWithBody(tapbackType uint32, customEmoji *string, body string, isRemove bool) string {
	var word string
	if isRemove {
		switch tapbackType {
		case 0:
			word = "Removed a heart from"
		case 1:
			word = "Removed a like from"
		case 2:
			word = "Removed a dislike from"
		case 3:
			word = "Removed a laugh from"
		case 4:
			word = "Removed an exclamation from"
		case 5:
			word = "Removed a question mark from"
		case 6:
			if customEmoji != nil {
				word = "Removed a " + *customEmoji + " from"
			} else {
				word = "Removed a like from"
			}
		default:
			word = "Removed a heart from"
		}
		return word + " \u201c" + body + "\u201d"
	}
	switch tapbackType {
	case 0:
		word = "Loved"
	case 1:
		word = "Liked"
	case 2:
		word = "Disliked"
	case 3:
		word = "Laughed at"
	case 4:
		word = "Emphasized"
	case 5:
		word = "Questioned"
	case 6:
		if customEmoji != nil {
			return "Reacted " + *customEmoji + " to \u201c" + body + "\u201d"
		}
		word = "Liked"
	default:
		word = "Loved"
	}
	return word + " \u201c" + body + "\u201d"
}

func mimeToUTI(mime string) string {
	switch {
	case mime == "image/jpeg":
		return "public.jpeg"
	case mime == "image/png":
		return "public.png"
	case mime == "image/gif":
		return "com.compuserve.gif"
	case mime == "image/heic":
		return "public.heic"
	case mime == "video/mp4":
		return "public.mpeg-4"
	case mime == "video/quicktime":
		return "com.apple.quicktime-movie"
	case mime == "audio/mpeg", mime == "audio/mp3":
		return "public.mp3"
	case mime == "audio/aac", mime == "audio/mp4":
		return "public.aac-audio"
	case mime == "audio/x-caf":
		return "com.apple.coreaudio-format"
	case mime == "text/vcard", mime == "text/x-vcard", mime == "text/directory":
		return "public.vcard"
	case strings.HasPrefix(mime, "image/"):
		return "public.image"
	case strings.HasPrefix(mime, "video/"):
		return "public.movie"
	case strings.HasPrefix(mime, "audio/"):
		return "public.audio"
	default:
		return "public.data"
	}
}

// utiToMIME converts an Apple UTI type to its MIME equivalent.
// Used as a fallback when CloudKit attachment records have a UTI but no MIME type.
func utiToMIME(uti string) string {
	switch uti {
	case "public.jpeg":
		return "image/jpeg"
	case "public.png":
		return "image/png"
	case "com.compuserve.gif":
		return "image/gif"
	case "public.tiff":
		return "image/tiff"
	case "public.heic":
		return "image/heic"
	case "public.heif":
		return "image/heif"
	case "public.webp":
		return "image/webp"
	case "public.mpeg-4":
		return "video/mp4"
	case "com.apple.quicktime-movie":
		return "video/quicktime"
	case "public.mp3":
		return "audio/mpeg"
	case "public.aac-audio":
		return "audio/aac"
	case "com.apple.coreaudio-format":
		return "audio/x-caf"
	case "public.vcard":
		return "text/vcard"
	default:
		return ""
	}
}

func mimeToMsgType(mime string) event.MessageType {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return event.MsgImage
	case strings.HasPrefix(mime, "video/"):
		return event.MsgVideo
	case strings.HasPrefix(mime, "audio/"):
		return event.MsgAudio
	default:
		return event.MsgFile
	}
}

func (c *IMClient) updatePortalSMS(portalID string, isSms bool) bool {
	c.smsPortalsLock.Lock()
	defer c.smsPortalsLock.Unlock()
	prev, existed := c.smsPortals[portalID]
	c.smsPortals[portalID] = isSms
	return !existed || prev != isSms
}

func (c *IMClient) isPortalSMS(portalID string) bool {
	c.smsPortalsLock.RLock()
	defer c.smsPortalsLock.RUnlock()
	if val, ok := c.smsPortals[portalID]; ok {
		return val
	}
	// Fallback for legacy portals that still have the raw suffix in their ID
	// (pre-fix DB entries that survive without a full reset). Such portals can
	// never transition to iMessage — their IDs are malformed by definition.
	return portalID != stripSmsSuffix(portalID)
}

func (c *IMClient) trackUnsend(uuid string) {
	c.recentUnsendsLock.Lock()
	defer c.recentUnsendsLock.Unlock()
	c.recentUnsends[uuid] = time.Now()
	for k, t := range c.recentUnsends {
		if time.Since(t) > 5*time.Minute {
			delete(c.recentUnsends, k)
		}
	}
}

func (c *IMClient) wasUnsent(uuid string) bool {
	c.recentUnsendsLock.Lock()
	defer c.recentUnsendsLock.Unlock()
	if t, ok := c.recentUnsends[uuid]; ok {
		return time.Since(t) < 5*time.Minute
	}
	return false
}

func (c *IMClient) trackSmsReactionEcho(uuid string) {
	if uuid == "" {
		return
	}
	c.recentSmsReactionEchoesLock.Lock()
	defer c.recentSmsReactionEchoesLock.Unlock()
	c.recentSmsReactionEchoes[strings.ToUpper(uuid)] = time.Now()
	for k, t := range c.recentSmsReactionEchoes {
		if time.Since(t) > 5*time.Minute {
			delete(c.recentSmsReactionEchoes, k)
		}
	}
}

func (c *IMClient) wasSmsReactionEcho(uuid string) bool {
	if uuid == "" {
		return false
	}
	c.recentSmsReactionEchoesLock.Lock()
	defer c.recentSmsReactionEchoesLock.Unlock()
	key := strings.ToUpper(uuid)
	if t, ok := c.recentSmsReactionEchoes[key]; ok {
		delete(c.recentSmsReactionEchoes, key)
		return time.Since(t) < 5*time.Minute
	}
	return false
}

// resolvePortalByTargetMessage looks up a message by UUID in the bridge database
// and returns the portal key it belongs to. This is critical for unsends and edits
// that arrive as self-reflections from the user's other Apple devices: the APNs
// envelope has participants=[self, self], so makePortalKey can't determine the
// correct DM portal. Returns an empty PortalKey if not found.
func (c *IMClient) resolvePortalByTargetMessage(log zerolog.Logger, targetUUID string) networkid.PortalKey {
	if targetUUID == "" {
		return networkid.PortalKey{}
	}
	msgID := makeMessageID(targetUUID)
	dbMessages, err := c.Main.Bridge.DB.Message.GetAllPartsByID(
		context.Background(), c.UserLogin.ID, msgID)
	if err != nil || len(dbMessages) == 0 {
		return networkid.PortalKey{}
	}
	log.Debug().
		Str("target_uuid", targetUUID).
		Str("resolved_portal", string(dbMessages[0].Room.ID)).
		Msg("Resolved portal via target message UUID lookup")
	return dbMessages[0].Room
}

func (c *IMClient) trackOutboundUnsend(uuid string) {
	c.recentOutboundUnsendsLock.Lock()
	defer c.recentOutboundUnsendsLock.Unlock()
	c.recentOutboundUnsends[uuid] = time.Now()
	for k, t := range c.recentOutboundUnsends {
		if time.Since(t) > 5*time.Minute {
			delete(c.recentOutboundUnsends, k)
		}
	}
}

func (c *IMClient) wasOutboundUnsend(uuid string) bool {
	c.recentOutboundUnsendsLock.Lock()
	defer c.recentOutboundUnsendsLock.Unlock()
	if t, ok := c.recentOutboundUnsends[uuid]; ok {
		if time.Since(t) < 5*time.Minute {
			delete(c.recentOutboundUnsends, uuid)
			return true
		}
		delete(c.recentOutboundUnsends, uuid)
	}
	return false
}

// urlRegex matches URLs in message text for rich link matching.
// Matches explicit schemes (https://...) and bare domains (example.com, example.com/path).
var urlRegex = regexp.MustCompile(`(?:https?://\S+|(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}(?:/\S*)?)`)

// isLikelyURL validates that a regex-matched string is actually a URL and not
// a filename or other dotted identifier. Uses url.Parse for validation,
// mirroring mautrix-whatsapp's approach. Bare domains (google.com, www.example.com)
// are accepted as long as they parse as valid URLs with a real host.
func isLikelyURL(s string) bool {
	normalized := s
	if !strings.HasPrefix(normalized, "http://") && !strings.HasPrefix(normalized, "https://") {
		normalized = "https://" + normalized
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	if host == "" {
		return false
	}
	// Must have at least one dot (rules out bare words).
	dot := strings.LastIndex(host, ".")
	if dot < 0 {
		return false
	}
	tld := strings.ToLower(host[dot+1:])
	if tld == "" {
		return false
	}
	// Reject common file extensions that the greedy bare-domain regex matches
	// (e.g. "config.yaml", "main.go", "notes.txt"). Only applied when the
	// original string had no scheme — explicit http(s):// URLs are always trusted.
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") && parsed.Path == "" {
		switch tld {
		case "go", "rs", "py", "rb", "js", "ts", "tsx", "jsx", "sh", "bash", "zsh",
			"c", "h", "cc", "cpp", "hpp", "cs", "java", "kt", "scala", "swift",
			"m", "mm", "pl", "pm", "r", "lua", "zig", "v", "d", "ex", "exs",
			"md", "txt", "log", "csv", "tsv", "diff", "patch",
			"json", "yaml", "yml", "toml", "xml", "ini", "cfg", "conf",
			"html", "css", "scss", "less", "sass",
			"png", "jpg", "jpeg", "gif", "bmp", "svg", "webp", "ico", "tiff",
			"mp3", "mp4", "wav", "flac", "ogg", "avi", "mkv", "mov", "webm",
			"zip", "tar", "gz", "bz2", "xz", "rar", "7z",
			"pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx",
			"lock", "sum", "mod", "bak", "tmp", "swp", "o", "so", "dylib", "a",
			"wasm", "bin", "exe", "dll", "dmg", "iso", "img", "apk", "ipa":
			return false
		}
	}
	return true
}

// normalizeURL ensures a URL has a scheme for HTTP fetching.
func normalizeURL(u string) string {
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return "https://" + u
	}
	return u
}

func ptrStringOr(s *string, def string) string {
	if s != nil {
		return *s
	}
	return def
}

func ptrUint64Or(v *uint64, def uint64) uint64 {
	if v != nil {
		return *v
	}
	return def
}

// ============================================================================
// Chat.db initial sync
// ============================================================================

// runChatDBInitialSync creates portals and backfills messages for all recent
// chats found in chat.db. Runs once on first login, then marks ChatsSynced.
func (c *IMClient) runChatDBInitialSync(log zerolog.Logger) {
	ctx := log.WithContext(context.Background())
	meta := c.UserLogin.Metadata.(*UserLoginMetadata)
	if meta.ChatsSynced {
		log.Info().Msg("Initial sync already completed, skipping")
		return
	}

	chats, err := c.chatDB.api.GetChatsWithMessagesAfter(time.Time{})
	if err != nil {
		log.Err(err).Msg("Failed to get chat list for initial sync")
		return
	}

	type chatEntry struct {
		chatGUID  string
		portalKey networkid.PortalKey
		info      *imessage.ChatInfo
		isSms     bool
	}
	var entries []chatEntry
	for _, chat := range chats {
		info, err := c.chatDB.api.GetChatInfo(chat.ChatGUID, chat.ThreadID)
		if err != nil || info == nil {
			continue
		}
		parsed := imessage.ParseIdentifier(chat.ChatGUID)
		var portalKey networkid.PortalKey
		isSms := parsed.Service == "SMS"
		if parsed.IsGroup {
			members := make([]string, 0, len(info.Members)+1)
			members = append(members, addIdentifierPrefix(c.handle))
			for _, m := range info.Members {
				members = append(members, addIdentifierPrefix(stripSmsSuffix(m)))
			}
			sort.Strings(members)
			portalKey = networkid.PortalKey{
				ID:       networkid.PortalID(strings.Join(members, ",")),
				Receiver: c.UserLogin.ID,
			}
		} else {
			portalKey = networkid.PortalKey{
				ID:       identifierToPortalID(parsed),
				Receiver: c.UserLogin.ID,
			}
		}
		if isSms {
			c.updatePortalSMS(string(portalKey.ID), true)
		}
		entries = append(entries, chatEntry{
			chatGUID:  chat.ChatGUID,
			portalKey: portalKey,
			info:      info,
			isSms:     isSms,
		})
	}

	// Deduplicate DM entries for contacts with multiple phone numbers.
	{
		type contactGroup struct {
			indices []int
		}
		groups := make(map[string]*contactGroup)
		for i, entry := range entries {
			portalID := string(entry.portalKey.ID)
			if strings.Contains(portalID, ",") {
				continue
			}
			contact := c.lookupContact(portalID)
			key := contactKeyFromContact(contact)
			if key == "" {
				continue
			}
			if g, ok := groups[key]; ok {
				g.indices = append(g.indices, i)
			} else {
				groups[key] = &contactGroup{indices: []int{i}}
			}
		}

		skip := make(map[int]bool)
		for _, group := range groups {
			if len(group.indices) <= 1 {
				continue
			}
			primaryIdx := group.indices[0]
			for _, idx := range group.indices[1:] {
				skip[idx] = true
				log.Info().
					Str("skip_portal", string(entries[idx].portalKey.ID)).
					Str("primary_portal", string(entries[primaryIdx].portalKey.ID)).
					Msg("Merging DM portal for contact with multiple phone numbers")
			}
		}

		if len(skip) > 0 {
			var merged []chatEntry
			for i, entry := range entries {
				if !skip[i] {
					merged = append(merged, entry)
				}
			}
			log.Info().Int("before", len(entries)).Int("after", len(merged)).Msg("Deduplicated DM entries by contact")
			entries = merged
		}
	}

	// Reverse to process oldest-activity first, so the most recent chat
	// gets the highest stream_ordering.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}

	log.Info().
		Int("chat_count", len(entries)).
		Msg("Initial sync: processing chats sequentially (oldest activity first)")

	synced := 0
	for _, entry := range entries {
		done := make(chan struct{})
		chatInfo := c.chatDBInfoToBridgev2(entry.info)
		chatGUID := entry.chatGUID
		isSms := entry.isSms
		c.UserLogin.QueueRemoteEvent(&simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventChatResync,
				PortalKey:    entry.portalKey,
				CreatePortal: true,
				PostHandleFunc: func(ctx context.Context, portal *bridgev2.Portal) {
					if isSms {
						meta := &PortalMetadata{}
						if existing, ok := portal.Metadata.(*PortalMetadata); ok {
							*meta = *existing
						}
						if !meta.IsSms {
							meta.IsSms = true
							portal.Metadata = meta
							if err := portal.Save(ctx); err != nil {
								zerolog.Ctx(ctx).Warn().Err(err).
									Str("portal_id", string(portal.ID)).
									Msg("Failed to persist IsSms metadata during initial sync")
							}
						}
					}
					close(done)
				},
				LogContext: func(lc zerolog.Context) zerolog.Context {
					return lc.Str("chat_guid", chatGUID).Str("source", "initial_sync")
				},
			},
			ChatInfo:        chatInfo,
			LatestMessageTS: time.Now(),
		})

		select {
		case <-done:
			synced++
			if synced%10 == 0 || synced == len(entries) {
				log.Info().
					Int("progress", synced).
					Int("total", len(entries)).
					Msg("Initial sync progress")
			}
		case <-time.After(30 * time.Minute):
			synced++
			log.Warn().
				Str("chat_guid", entry.chatGUID).
				Msg("Initial sync: timeout waiting for chat, continuing")
		case <-c.stopChan:
			log.Info().Msg("Initial sync stopped")
			return
		}
	}

	meta.ChatsSynced = true
	if err := c.UserLogin.Save(ctx); err != nil {
		log.Err(err).Msg("Failed to save metadata after initial sync")
	}
	log.Info().
		Int("synced_chats", synced).
		Int("total_chats", len(entries)).
		Msg("Initial sync complete")
}

// chatDBInfoToBridgev2 converts a chat.db ChatInfo to a bridgev2 ChatInfo.
func (c *IMClient) chatDBInfoToBridgev2(info *imessage.ChatInfo) *bridgev2.ChatInfo {
	parsed := imessage.ParseIdentifier(info.JSONChatGUID)
	if parsed.LocalID == "" {
		parsed = info.Identifier
	}

	chatInfo := &bridgev2.ChatInfo{
		CanBackfill: true,
	}

	if parsed.IsGroup {
		displayName := info.DisplayName
		if displayName == "" {
			displayName = c.buildGroupName(info.Members)
		}
		chatInfo.Name = &displayName
	}

	if parsed.IsGroup {
		chatInfo.Type = ptr.Ptr(database.RoomTypeDefault)
		members := &bridgev2.ChatMemberList{
			IsFull:      true,
			MemberMap:   make(map[networkid.UserID]bridgev2.ChatMember),
			PowerLevels: c.hardenedPowerLevels(context.Background(), nil),
		}
		members.MemberMap[makeUserID(c.handle)] = bridgev2.ChatMember{
			EventSender: bridgev2.EventSender{
				IsFromMe:    true,
				SenderLogin: c.UserLogin.ID,
				Sender:      makeUserID(c.handle),
			},
			Membership: event.MembershipJoin,
		}
		for _, memberID := range info.Members {
			userID := makeUserID(addIdentifierPrefix(stripSmsSuffix(memberID)))
			members.MemberMap[userID] = bridgev2.ChatMember{
				EventSender: bridgev2.EventSender{Sender: userID},
				Membership:  event.MembershipJoin,
			}
		}
		chatInfo.Members = members
	} else {
		chatInfo.Type = ptr.Ptr(database.RoomTypeDM)
		portalID := addIdentifierPrefix(stripSmsSuffix(parsed.LocalID))
		otherUser := makeUserID(portalID)
		isSelfChat := c.isMyHandle(portalID)

		memberMap := map[networkid.UserID]bridgev2.ChatMember{
			makeUserID(c.handle): {
				EventSender: bridgev2.EventSender{
					IsFromMe:    true,
					SenderLogin: c.UserLogin.ID,
					Sender:      makeUserID(c.handle),
				},
				Membership: event.MembershipJoin,
			},
		}
		// Only add the other user if it's not a self-chat, to avoid
		// overwriting the IsFromMe entry with a duplicate map key.
		if !isSelfChat {
			memberMap[otherUser] = bridgev2.ChatMember{
				EventSender: bridgev2.EventSender{Sender: otherUser},
				Membership:  event.MembershipJoin,
			}
		}

		members := &bridgev2.ChatMemberList{
			IsFull:      true,
			OtherUserID: otherUser,
			MemberMap:   memberMap,
			PowerLevels: c.hardenedPowerLevels(context.Background(), nil),
		}

		// For self-chats, set an explicit name and avatar from contacts since
		// the framework can't derive them from the ghost when the "other user"
		// is the logged-in user. Setting Name causes NameIsCustom=true in the
		// framework, which blocks UpdateInfoFromGhost (it returns early when
		// NameIsCustom is set), so we must also set the avatar explicitly here.
		if isSelfChat {
			selfName := c.resolveContactDisplayname(portalID)
			chatInfo.Name = &selfName

			// Pull contact photo for self-chat room avatar.
			localID := stripIdentifierPrefix(portalID)
			if c.contacts != nil {
				if contact, _ := c.contacts.GetContactInfo(localID); contact != nil && len(contact.Avatar) > 0 {
					avatarHash := sha256.Sum256(contact.Avatar)
					avatarData := contact.Avatar
					chatInfo.Avatar = &bridgev2.Avatar{
						ID: networkid.AvatarID(fmt.Sprintf("contact:%s:%s", portalID, hex.EncodeToString(avatarHash[:8]))),
						Get: func(ctx context.Context) ([]byte, error) {
							return avatarData, nil
						},
					}
				}
			}
		}

		chatInfo.Members = members
	}

	return chatInfo
}
