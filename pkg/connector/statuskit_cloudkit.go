package connector

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/lrhodin/corten-matrix/pkg/rustpushgo"
)

// StatusKit-CloudKit pull (bridge-side orchestration).
//
// The Rust side does the actual zone discovery, record fetch, and state
// injection (see pkg/rustpushgo/src/statuskit_cloudkit.rs). This file:
//   1. Reads the cached discovered-zone-name and continuation-token from
//      the existing cloud_sync_state table.
//   2. Calls the FFI cloud_sync_statuskit_peers.
//   3. Persists the resolved zone name and the next continuation token.
//   4. If any peers were injected, fires subscribeToContactPresence so the
//      bridge subscribes to the new peers' StatusKit channels immediately.
//
// All logging is at log.Info() level so production output can be inspected
// without raising the log level. Phase 1 of the feature is intentionally
// diagnostic-heavy: the Rust side dumps every record's schema so the user
// can identify the StatusKit zone and field names from logs and feed them
// back to refine the decoder.

// Reserved cloud_sync_state row names. All three reuse the existing table so
// no migration is required.
//   - statusKitCloudZoneRow stores the resolved zone name in
//     continuation_token.
//   - statusKitCloudTokenRow stores the FFI continuation token (base64) in
//     continuation_token.
//   - statusKitCloudPassMetaRow stores "<last_attempt_unix_ms>:<consec_err>"
//     in continuation_token. Used for the inter-pass backoff gate so the
//     bridge doesn't spam Apple's CKKS — empirically, an unpaced burst of
//     LimitedPeersAllowed-zone fetches gets the device kicked out of the
//     iCloud trust circle ("Not in clique" responses on every CloudKit op).
const (
	statusKitCloudZoneRow     = "_statuskit_zone"
	statusKitCloudTokenRow    = "_statuskit_token"
	statusKitCloudPassMetaRow = "_statuskit_pass_meta"
)

// statusKitFailureBackoffBase is the base delay used when the previous
// pass failed. The actual delay scales exponentially with consecutive
// errors (15m → 30m → 1h → 2h capped).
const statusKitFailureBackoffBase = 15 * time.Minute

// statusKitSuccessFloor is the steady-state minimum interval between
// successful passes. New peer keys arrive at human-event rates (a
// contact opening Focus sharing for the first time, a peer rotating
// devices) — typically a handful per week across a contact list. The
// outer cloud-sync orchestrator fires multiple times per hour on a
// chatty cluster; without this floor we'd pull ~100 records of CKKS
// churn per cycle to discover changes that statistically aren't there.
// 12h is well above the natural event rate while still picking up new
// peers within a day, and an order-of-magnitude reduction in CKKS
// volume on the LimitedPeersAllowed view (the surface that produced
// the clique-kick during early testing).
//
// This is the STEADY-STATE floor, applied only once peer keys are
// established and have gone quiet. Before keys are established (or while
// they're actively arriving) the much shorter statusKitFreshFloor applies
// — see nextAllowedAt.
//
// Lowered 12h → 4h: a peer ROTATING their StatusKit key (re-registration /
// fresh reshare) makes our stored key stale, and if that key only lands in
// CloudKit (not delivered to the bridge's device via APNs key-sharing) the old
// 12h floor left presence undecryptable — and the moon stuck — for up to half a
// day. 4h picks up rotations far sooner while staying ~1.25 passes/hr, well
// under the ~15–20/hr the clique-kick postmortem documents as tolerated (the
// kick was wrong-class-unwrap/zone-probing bursts, not steady pull rate). The
// moon-TTL in isPortalSilenced is the backstop for keys that never re-pull.
const statusKitSuccessFloor = 4 * time.Hour

// statusKitFreshFloor is the (gentle) minimum interval between successful
// passes while the bridge has NOT yet pulled any peer keys, or while keys are
// still actively arriving. A fresh or large account (James: 1,480 chats, 0
// peer keys at first — presence can't decrypt anything) has its contacts'
// reshares land in CloudKit gradually after the bridge publishes
// share_status. With only the 12h floor, the single bootstrap pass runs
// before those reshares arrive and nothing re-pulls them for 12h. This floor
// keeps pulling every half hour until keys show up, then nextAllowedAt backs
// off to statusKitSuccessFloor once keys are established and the stream is
// quiet. At ~4–5 Apple round-trips per pass that's ~10/hour — within the
// ~15–20/hour the clique-kick postmortem documents as tolerated, and far from
// the wrong-class-unwrap / zone-probing bursts that actually caused the kick.
// See docs/postmortem-statuskit-clique-kick-2026-05-09.md.
const statusKitFreshFloor = 30 * time.Minute

// statusKitDrainResumeFloor is the (short) interval before resuming a drain
// that hasn't finished walking the zone yet. The 12h success floor — and even
// the 30m fresh floor — must NOT apply while the continuation token is still
// outstanding: a large account (James: 1,480 chats, many pages of
// CD_ReceivedInvitation / CD_Channel records) can't be fully walked if the
// gate parks the pass on a long floor mid-drain, and the un-walked tail of
// peer keys then never hydrates so those contacts' presence stays dark. The
// post-backfill restart trigger runs under context.Background (no timeout) and
// drains the whole zone in one pass, so this floor only governs the rare case
// where a bounded-ctx periodic pass is cancelled mid-walk — resume soon and
// finish. See nextAllowedAt.
const statusKitDrainResumeFloor = 2 * time.Minute

// statusKitPullTickInterval is how often the background loop (runStatusKitPullLoop)
// offers the pass a chance to run. The pass's own floor gate decides whether to
// actually contact Apple — once keys are established most ticks are a cheap
// in-memory skip. Shorter than statusKitFreshFloor so a fresh account's keys
// are pulled promptly after the floor elapses.
const statusKitPullTickInterval = 15 * time.Minute

// statusKitPassMaxBackoff caps the exponential backoff after consecutive
// errors. Two hours is long enough that a transient CKKS rate-limit window
// (which typically resets in tens of minutes) clears between attempts, and
// short enough that recovery after a self-healing trust event is automatic
// rather than requiring a bridge restart.
const statusKitPassMaxBackoff = 2 * time.Hour

// statuskitPassRetryAfterRegex extracts an Apple-provided "retry_after_seconds"
// hint from FFI error strings. Same shape as the chat/message backfill path
// uses (see cloudKitRetryAfterRegex in sync_controller.go) — we honor the
// larger of the hint or our exponential fallback, capped to
// statusKitPassMaxBackoff.
var statuskitPassRetryAfterRegex = cloudKitRetryAfterRegex

// statusKitPassMeta is the parsed form of statusKitCloudPassMetaRow. Stored
// flat in the existing TEXT column so no schema change is needed. Old 2-field
// rows ("<ms>:<errs>") parse with Established/LastPulledKeys defaulting false,
// which just means a not-yet-established account starts on the fresh floor —
// self-correcting on the first key-bearing pass.
type statusKitPassMeta struct {
	LastAttempt     time.Time
	ConsecutiveErrs int
	// Established is sticky: true once any pass has pulled at least one peer
	// key. Until then the bridge pulls on the gentle fresh floor.
	Established bool
	// LastPulledKeys records whether the most recent successful pass pulled
	// new keys. While true the bridge keeps pulling on the fresh floor to
	// catch the rest of an in-flight reshare stream; once a pass comes up
	// empty it backs off to the 12h steady-state floor.
	LastPulledKeys bool
	// DrainComplete records whether the most recent successful pass walked the
	// zone all the way to the end (continuation token exhausted) rather than
	// stopping early (a cancelled bounded-ctx pass, or the page backstop). The
	// 12h success floor and the 30m fresh floor are BOTH gated on this: while a
	// drain is incomplete the gate returns the short statusKitDrainResumeFloor
	// so the pass resumes and finishes instead of stranding the un-walked tail
	// of peer keys for hours. Defaults false for old 2/4-field rows, which is
	// exactly right — a pre-existing account is treated as mid-drain on its
	// first post-upgrade pass and gets one full walk.
	DrainComplete bool
	// RecoveryRepullDone records that we've already forced a one-shot full
	// re-walk to recover from the stranded-keys trap: a cached continuation
	// token parked past CD_ReceivedInvitation records that were never decoded
	// into peer keys, so incremental fetches return 0 forever and Established
	// never flips. Sticky once set so a genuinely peer-less account doesn't
	// re-walk the whole zone on every pass. See the recovery block in
	// syncCloudStatusKitPeers.
	RecoveryRepullDone bool
}

func parseStatusKitPassMeta(raw *string) statusKitPassMeta {
	if raw == nil || *raw == "" {
		return statusKitPassMeta{}
	}
	parts := strings.Split(*raw, ":")
	if len(parts) < 2 {
		return statusKitPassMeta{}
	}
	tsMS, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || tsMS <= 0 {
		return statusKitPassMeta{}
	}
	errs, err := strconv.Atoi(parts[1])
	if err != nil || errs < 0 {
		errs = 0
	}
	m := statusKitPassMeta{
		LastAttempt:     time.UnixMilli(tsMS),
		ConsecutiveErrs: errs,
	}
	if len(parts) >= 4 {
		m.Established = parts[2] == "1"
		m.LastPulledKeys = parts[3] == "1"
	}
	if len(parts) >= 5 {
		m.DrainComplete = parts[4] == "1"
	}
	if len(parts) >= 6 {
		m.RecoveryRepullDone = parts[5] == "1"
	}
	return m
}

func (m statusKitPassMeta) encode() string {
	b2i := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d:%d", m.LastAttempt.UnixMilli(), m.ConsecutiveErrs, b2i(m.Established), b2i(m.LastPulledKeys), b2i(m.DrainComplete), b2i(m.RecoveryRepullDone))
}

// nextAllowedAt returns the earliest time at which a new pass may run.
// On a successful previous pass with no Apple retry-after hint, applies
// the statusKitSuccessFloor (12h) — new peer keys are rare and polling
// tighter wastes CKKS budget. On failure, applies exponential backoff
// (15m → 30m → 1h → 2h capped) honoring any larger retry-after hint.
func (m statusKitPassMeta) nextAllowedAt(retryAfterHint time.Duration) time.Time {
	if m.LastAttempt.IsZero() {
		return time.Time{}
	}
	if m.ConsecutiveErrs == 0 && retryAfterHint == 0 {
		// The longer floors only apply once the zone has been walked to the
		// end. While a drain is still incomplete — the continuation token is
		// outstanding because a bounded-ctx pass was cancelled mid-walk, or a
		// large account spans more pages than one pass finished — resume on the
		// short drain floor so the pass picks up where it left off and
		// completes. Parking on a 30m/12h floor here would strand the
		// un-walked tail of peer keys and presence for those contacts never
		// hydrates. This is David's invariant: the 12h back-off only kicks in
		// once the drain is complete.
		if !m.DrainComplete {
			return m.LastAttempt.Add(statusKitDrainResumeFloor)
		}
		// Drain complete. Pull on the gentle fresh floor while keys haven't
		// been established yet (fresh/large account whose reshares are still
		// landing) or while keys are actively arriving; back off to the 12h
		// steady-state only once we have keys AND the stream went quiet.
		if !m.Established || m.LastPulledKeys {
			return m.LastAttempt.Add(statusKitFreshFloor)
		}
		return m.LastAttempt.Add(statusKitSuccessFloor)
	}
	delay := statusKitFailureBackoffBase
	if m.ConsecutiveErrs > 0 {
		// 15m * 2^(n-1), capped. n=1 → 15m, n=2 → 30m, n=3 → 1h, n=4 → 2h.
		shift := m.ConsecutiveErrs - 1
		if shift > 6 {
			shift = 6
		}
		delay = statusKitFailureBackoffBase << shift
		if delay > statusKitPassMaxBackoff {
			delay = statusKitPassMaxBackoff
		}
	}
	if retryAfterHint > delay {
		delay = retryAfterHint
		if delay > statusKitPassMaxBackoff {
			delay = statusKitPassMaxBackoff
		}
	}
	return m.LastAttempt.Add(delay)
}

// extractRetryAfterHint parses Apple's `retry_after_seconds` from an FFI
// error string, mirroring sync_controller.go's chat/message path. Returns
// zero if no hint present.
func extractRetryAfterHint(err error) time.Duration {
	if err == nil {
		return 0
	}
	m := statuskitPassRetryAfterRegex.FindStringSubmatch(err.Error())
	if len(m) < 2 {
		return 0
	}
	secs, parseErr := strconv.Atoi(m[1])
	if parseErr != nil || secs <= 0 {
		return 0
	}
	hint := time.Duration(secs) * time.Second
	if hint > statusKitPassMaxBackoff {
		hint = statusKitPassMaxBackoff
	}
	return hint
}

// runStatusKitPullLoop periodically offers the StatusKit-CloudKit peer-key
// pull a chance to run. Without it the pass only fires at startup (the
// bootstrap sync plus three one-shot delayed re-syncs) and never again until
// the next bridge restart — so peer keys that reshare into CloudKit after
// those early passes (the common case on a fresh or large account, where
// contacts reshare gradually after the bridge publishes share_status) sit
// un-pulled and presence can't decrypt them.
//
// The pass's own floor gate (nextAllowedAt) decides whether each tick actually
// contacts Apple: gentle 30-min pulls while keys haven't been established or
// are still arriving, backing off to 12h once established and quiet. Most ticks
// in steady state are a cheap in-memory skip with no Apple traffic.
func (c *IMClient) runStatusKitPullLoop(log zerolog.Logger, stopChan <-chan struct{}) {
	if c.cloudStore == nil {
		return
	}
	ticker := time.NewTicker(statusKitPullTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stopChan:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			if err := c.syncCloudStatusKitPeers(ctx, log); err != nil {
				log.Info().Err(err).Msg("StatusKit-CloudKit periodic pull: errored (continuing)")
			}
			cancel()
		}
	}
}

// syncCloudStatusKitPeers runs one StatusKit-CloudKit pull pass. Called as
// the fourth phase of runCloudSyncOnce, after the existing chats / messages
// / attachments backfill completes.
//
// Behavior:
//   - Reads cached zone name + continuation token from cloud_sync_state.
//   - Defers (no-op) if a StatusKit invite sweep is currently running, so
//     we don't fight the rust-side StatusKit mutex.
//   - Refreshes the PET token (matches the startup-share pattern).
//   - Calls the FFI; the Rust side discovers the zone on first call.
//   - Persists the resolved zone name + next token. If the FFI returns
//     resolved_zone=None alongside an explicit summary, that signals
//     "clear cached zone so we re-discover next pass".
//   - If injected_handles > 0, fires subscribeToContactPresence so the
//     newly-keyed peers get APNs subscriptions immediately rather than
//     waiting for the next periodic loop.
func (c *IMClient) syncCloudStatusKitPeers(ctx context.Context, log zerolog.Logger) error {
	return c.syncCloudStatusKitPeersForce(ctx, log, false)
}

// decryptFailureRepullCooldown bounds how often a presence decrypt failure (a
// stale peer key) can force an out-of-cycle CloudKit re-pull. A peer who rotated
// their key emits a stream of undecodable presence updates; without this cooldown
// each one would force a pull, hammering CKKS and re-triggering the clique-kick.
// One forced pull per window is enough to fetch the rotated key.
const decryptFailureRepullCooldown = 15 * time.Minute

// onPresenceDecryptFailed forces a rate-limited CloudKit peer-key re-pull in
// response to a presence update we couldn't decode (OnStatusDecryptFailed). The
// re-pull bypasses the steady-state floor (force=true) so the fresh key is
// fetched right away, but is gated by decryptFailureRepullCooldown and inherits
// every other safety guard (single-flight, sweep, backfill-settle). Runs the pull
// in a goroutine so the Rust recv-loop callback returns immediately.
func (c *IMClient) onPresenceDecryptFailed(log zerolog.Logger) {
	last := atomic.LoadInt64(&c.lastDecryptFailureRepull)
	if last != 0 && time.Since(time.UnixMilli(last)) < decryptFailureRepullCooldown {
		return
	}
	if !atomic.CompareAndSwapInt64(&c.lastDecryptFailureRepull, last, time.Now().UnixMilli()) {
		// Another decrypt-failure goroutine claimed the slot — it'll run the pull.
		return
	}
	go func() {
		if err := c.syncCloudStatusKitPeersForce(context.Background(), log, true); err != nil {
			log.Info().Err(err).Msg("StatusKit: decrypt-failure-forced CloudKit re-pull errored")
		}
	}()
}

// syncCloudStatusKitPeersForce is syncCloudStatusKitPeers with an explicit force
// flag. When force is true the inter-pass backoff/floor gate is bypassed (the
// caller has a concrete reason to pull NOW — e.g. a presence decrypt failure that
// means a peer key is stale), but every OTHER guard still applies: single-flight,
// the invite-sweep deferral, and the backfill-settle window. Forced callers are
// responsible for their own cooldown so they can't defeat the clique-kick rate
// limit (see onPresenceDecryptFailed).
func (c *IMClient) syncCloudStatusKitPeersForce(ctx context.Context, log zerolog.Logger, force bool) error {
	if c.client == nil {
		log.Info().Msg("StatusKit-CloudKit pass: skipped (rust client not ready)")
		return nil
	}
	if c.UserLogin == nil {
		log.Info().Msg("StatusKit-CloudKit pass: skipped (no UserLogin)")
		return nil
	}

	// Single-flight: the periodic pull loop, the post-backfill trigger, and the
	// cloud-sync phases can all call this. Two concurrent passes would race on
	// the continuation-token / pass-meta rows and double the CKKS round-trips.
	if !c.statusKitPassInFlight.CompareAndSwap(false, true) {
		log.Info().Msg("StatusKit-CloudKit pass: skipped (another pass already in flight)")
		return nil
	}
	defer c.statusKitPassInFlight.Store(false)

	// Avoid running concurrently with an invite sweep — both touch the
	// rust-side StatusKit state mutex. Sweep wins; we'll pick up next pass.
	if c.statusKitSweepRunning.Load() {
		log.Info().Msg("StatusKit-CloudKit pass: deferred (StatusKit invite sweep is running)")
		return nil
	}

	// Don't run during initial forward backfill or for a settling window
	// after it completes. On a cold bootstrap, presence broadcasts that
	// arrive while messages are still being inserted into the bridge DB
	// hit the OnStatusUpdate `lastMsg=nil` fallback (now-1ms) and bump
	// chats to the top of the room list in random presence-arrival order.
	// A warm restart works fine because the DB already has prior-session
	// messages to anchor against. apnsBufferFlushedAt is set the moment
	// the last forward-backfill batch's CompleteCallback fires (in
	// onForwardBackfillDone at counter==0); we add a settling window on
	// top of that to let any straggler DB writes commit.
	const statusKitBackfillSettleWindow = 60 * time.Second
	flushedAt := atomic.LoadInt64(&c.apnsBufferFlushedAt)
	if flushedAt == 0 {
		log.Info().Msg("StatusKit-CloudKit pass: skipped (initial forward backfill not yet complete)")
		return nil
	}
	if elapsed := time.Since(time.UnixMilli(flushedAt)); elapsed < statusKitBackfillSettleWindow {
		log.Info().
			Dur("elapsed_since_backfill", elapsed.Round(time.Second)).
			Dur("settle_window", statusKitBackfillSettleWindow).
			Msg("StatusKit-CloudKit pass: skipped (waiting for backfill DB writes to settle)")
		return nil
	}

	// Inter-pass backoff gate. The outer cloud-sync orchestrator can fire
	// runCloudSyncOnce several times in the first minutes after a restart
	// (bootstrap + delayed-resync schedule + !resync) and on every APNs
	// nudge thereafter. Hammering Apple's CKKS for the LimitedPeersAllowed
	// view at that cadence is what got us kicked out of the trust circle
	// during early testing. Two gates apply here: a 12h success floor
	// (statusKitSuccessFloor) for steady-state, since new peer keys
	// arrive at human-event rates rather than per-cycle; and exponential
	// failure backoff (15m → 30m → 1h → 2h capped) honoring Apple's
	// `retry_after_seconds` hint when larger.
	//
	// Skipping the FFI here intentionally does NOT touch the meta row —
	// the gate is read-only when it blocks. Bumping last_attempt on a
	// blocked attempt would push the deadline forward forever and the
	// pass would never fire.
	metaPtr, err := c.cloudStore.getSyncState(ctx, statusKitCloudPassMetaRow)
	if err != nil {
		log.Info().Err(err).Msg("StatusKit-CloudKit pass: cloud_sync_state read failed for meta-row (continuing — treats as fresh state)")
		metaPtr = nil
	}
	meta := parseStatusKitPassMeta(metaPtr)
	// First pass after process startup bypasses the gate so a bridge restart
	// is a natural retry signal — the user may have fixed trust externally
	// and is rebooting to recover. CompareAndSwap ensures only one caller
	// per process gets the bypass; subsequent calls fall through to the
	// gate normally.
	bypassForFirstCall := c.statusKitCloudPassFirstCallDone.CompareAndSwap(false, true)
	if !bypassForFirstCall && !force && !meta.LastAttempt.IsZero() {
		nextAllowed := meta.nextAllowedAt(0)
		if time.Now().Before(nextAllowed) {
			log.Info().
				Time("last_attempt", meta.LastAttempt).
				Int("consecutive_errors", meta.ConsecutiveErrs).
				Time("next_allowed", nextAllowed).
				Dur("wait", time.Until(nextAllowed).Round(time.Second)).
				Msg("StatusKit-CloudKit pass: skipped by inter-pass backoff (Apple CKKS rate-limit guard)")
			return nil
		}
	} else if bypassForFirstCall && meta.ConsecutiveErrs > 0 {
		log.Info().
			Int("prior_consecutive_errors", meta.ConsecutiveErrs).
			Time("prior_last_attempt", meta.LastAttempt).
			Msg("StatusKit-CloudKit pass: bypassing prior backoff for first pass after restart (one chance to recover)")
	}

	// Refresh PET so the CloudKit container init has a fresh delegate.
	// Matches the startup-share path pattern in client.go.
	if err := c.safeRefreshPetTokenThrottled(); err != nil {
		log.Info().Err(err).Msg("StatusKit-CloudKit pass: PET refresh skipped (continuing — refresh is best-effort)")
	}

	// Read cached zone name (if any). The discovery row stores the
	// resolved zone name in the continuation_token column.
	cachedZonePtr, err := c.cloudStore.getSyncState(ctx, statusKitCloudZoneRow)
	if err != nil {
		log.Info().Err(err).Msg("StatusKit-CloudKit pass: cloud_sync_state read failed for zone-row (continuing without cache)")
		cachedZonePtr = nil
	}
	var cachedZone *string
	if cachedZonePtr != nil && *cachedZonePtr != "" {
		cachedZone = cachedZonePtr
	}

	// Read continuation token. The token is opaque bytes; we base64-encode
	// it for storage in the TEXT column.
	cachedTokenPtr, err := c.cloudStore.getSyncState(ctx, statusKitCloudTokenRow)
	if err != nil {
		log.Info().Err(err).Msg("StatusKit-CloudKit pass: cloud_sync_state read failed for token-row (continuing without token)")
		cachedTokenPtr = nil
	}
	var sinceToken *[]byte
	if cachedTokenPtr != nil && *cachedTokenPtr != "" {
		decoded, decErr := base64.StdEncoding.DecodeString(*cachedTokenPtr)
		if decErr != nil {
			log.Info().Err(decErr).Str("raw", *cachedTokenPtr).Msg("StatusKit-CloudKit pass: stored token failed base64 decode — clearing and continuing without token")
			if clrErr := c.cloudStore.clearZoneToken(ctx, statusKitCloudTokenRow); clrErr != nil {
				log.Info().Err(clrErr).Msg("StatusKit-CloudKit pass: failed to clear malformed token row")
			}
		} else {
			sinceToken = &decoded
		}
	}

	// Stranded-keys recovery guard. If we hold a continuation token but have
	// never pulled a single peer key (Established=false), the token is parked
	// past CD_ReceivedInvitation records that were already in CloudKit before
	// we could decode them — incremental fetches return 0 forever, Established
	// never flips, and presence never hydrates. A bridge can land here after
	// running a build with the old 30-page cap on a large account (the tail
	// stranded behind the token), or any time a full walk happened to insert 0
	// keys. The Rust channel-map-empty guard does NOT catch this: the channel
	// map can be fully populated from CD_Channel records while zero invitations
	// were ever decoded into keys.
	//
	// Force ONE full re-walk by dropping the token. Bounded by
	// RecoveryRepullDone (sticky) so a genuinely peer-less account doesn't
	// re-walk the whole zone every pass: once attempted, later passes resume
	// forward from the persisted mid-walk token via the normal drainComplete
	// machinery instead of restarting. If the re-walk finds keys, Established
	// flips true and this guard never fires again; if it finds none, the latch
	// stops further forced walks and a future invitation arrives via the
	// incremental token as a normal change.
	//
	// DrainComplete is part of the trigger so we only ever recover from a walk
	// that FINISHED (token exhausted) and still produced zero keys — the real
	// stuck state. A drain that's merely in progress (a large account still
	// walking, or a bounded-ctx pass cancelled mid-walk before reaching an
	// invitation page) has DrainComplete=false; it must NOT be disrupted —
	// dropping its mid-walk token would throw away progress and restart from
	// scratch. The normal resume floor finishes those instead. A pre-existing
	// row that predates DrainComplete parses false, so it just takes one extra
	// (cheap, fetched=0) pass to set DrainComplete before recovery fires.
	forcedRecoveryRepull := false
	if sinceToken != nil && !meta.Established && meta.DrainComplete && !meta.RecoveryRepullDone {
		log.Info().
			Time("parked_token_last_attempt", meta.LastAttempt).
			Msg("StatusKit-CloudKit pass: completed walk left a cached token but no peer keys ever pulled — forcing a one-shot full re-walk to recover stranded keys")
		sinceToken = nil
		if clrErr := c.cloudStore.clearZoneToken(ctx, statusKitCloudTokenRow); clrErr != nil {
			log.Info().Err(clrErr).Msg("StatusKit-CloudKit pass: failed to clear token row for recovery re-walk (continuing — FFI still gets a nil token)")
		}
		forcedRecoveryRepull = true
	}

	log.Info().
		Bool("has_cached_zone", cachedZone != nil).
		Interface("cached_zone", cachedZone).
		Bool("has_cached_token", sinceToken != nil).
		Bool("forced_recovery_repull", forcedRecoveryRepull).
		Msg("StatusKit-CloudKit pass: invoking FFI")

	// Wrap in safeFFICall to match the panic-isolation pattern used by
	// the other cloud-sync FFIs (sync_controller.go safeCloudSyncMessages etc).
	// UniFFI deserialization panics on malformed buffers; without this wrap
	// a bad page would crash the bridge instead of just failing the pass.
	//
	// We commit a meta-row update either way (success: errors=0, failure:
	// errors+1 + retry-after extension). Done via defer so a panic that
	// escapes safeFFICall still leaves the gate in a recoverable state
	// rather than stranding the bridge in "always run, no backoff".
	// Drain accumulators, declared before the defer so the meta-row update can
	// record whether THIS pass actually pulled keys — that drives the
	// fresh-floor-vs-12h-floor decision in nextAllowedAt.
	var (
		totalFetched      uint32
		totalInserted     uint32
		totalAlreadyKnown uint32
		totalDecodeFailed uint32
		totalRecordsSeen  uint32
		injectedHandles   []string
		// drainComplete latches true only when the drain loop walks the zone to
		// the end (token exhausted / empty fetch). Stays false if the pass is
		// cancelled mid-walk or hits the page backstop, so the gate resumes it.
		drainComplete bool
	)

	attemptStart := time.Now()
	var ffiErr error
	defer func() {
		newMeta := statusKitPassMeta{
			LastAttempt:    attemptStart,
			Established:    meta.Established,    // sticky — never un-establish
			LastPulledKeys: meta.LastPulledKeys, // carried over unless a success updates it below
			// Sticky once we've forced a recovery re-walk, so a peer-less account
			// doesn't restart the walk from scratch every pass — but ONLY latch on
			// a successful pass. If the FFI errored AND the token-row clear also
			// failed (DB write error), the parked token is still present; latching
			// here would block the guard from ever re-firing and leave the account
			// permanently re-stranded. Leaving the latch false on error lets the
			// next pass re-attempt recovery. (On error where the clear succeeded,
			// the now-nil token drives a full walk anyway and the guard's
			// sinceToken!=nil check stops a redundant re-fire.)
			RecoveryRepullDone: meta.RecoveryRepullDone || (forcedRecoveryRepull && ffiErr == nil),
		}
		if ffiErr != nil {
			newMeta.ConsecutiveErrs = meta.ConsecutiveErrs + 1
		} else {
			newMeta.ConsecutiveErrs = 0
			// A pass that inserted new peer keys keeps us on the gentle fresh
			// floor (more reshares may still be in flight) and marks the
			// account established. An empty success lets the floor back off to
			// the 12h steady-state.
			pulled := totalInserted > 0
			newMeta.LastPulledKeys = pulled
			if pulled {
				newMeta.Established = true
			}
			// Record whether the zone was walked to the end. The gate uses this
			// to decide between the short resume floor (drain still in progress)
			// and the fresh/12h floors (drain finished). A pass cancelled
			// mid-walk leaves this false so the next pass resumes the drain.
			newMeta.DrainComplete = drainComplete
		}
		encoded := newMeta.encode()
		if persistErr := c.cloudStore.setSyncStateSuccess(ctx, statusKitCloudPassMetaRow, &encoded); persistErr != nil {
			log.Info().Err(persistErr).Msg("StatusKit-CloudKit pass: failed to persist pass-meta row (next pass may not honor backoff)")
		}
	}()

	// Drain all pages within this single pass. Each FFI call returns one
	// page with at most one Apple-side fetch round-trip; the continuation
	// token in the response tells us whether more pages exist. Looping
	// here means a single bootstrap pass ingests every available record
	// instead of stranding pages behind the success floor (the
	// chat/message backfill paths get hit hundreds of times per session
	// so they drain naturally; StatusKit doesn't, and architecturally
	// pretending it does was the bug).
	//
	// Safety cap of 30 pages with a 1s pause between iterations keeps
	// the per-pass CKKS round-trip count bounded even if Apple keeps
	// returning continuation tokens — far below the per-pass budget that
	// produced the early-testing clique-kick.
	// Walk every page to completion, the same way the message and chat CloudKit
	// drains do (syncCloudMessages pages back-to-back up to maxCloudSyncPages
	// with no artificial per-pass cap). The old 30-page cap is what stranded a
	// large account like James's: his zone spans far more than 30 pages, so the
	// pass stopped with the continuation token still outstanding and the
	// inter-pass gate then parked it on the 12h floor before the tail ever
	// drained — those peers' keys never hydrated and their presence stayed
	// dark. drainComplete (below) latches only when the token is exhausted, and
	// the gate keeps the pass on the short resume floor until then. The
	// post-backfill restart trigger runs under context.Background (no timeout),
	// so it can drain the whole zone in a single pass.
	//
	// Pacing is deliberately slow-and-steady at 5s/page — gentler than the
	// message/chat drains (which page with no delay and don't trip Apple's
	// rate limits in production) so a long initial walk of the rate-sensitive
	// LimitedPeersAllowed view stays well clear of any throttle.
	const (
		maxPagesPerPass = maxCloudSyncPages
		interPageDelay  = 5 * time.Second
	)
	currentZone := cachedZone
	currentToken := sinceToken
	prevTokenStr := ""
	if currentToken != nil {
		prevTokenStr = base64.StdEncoding.EncodeToString(*currentToken)
	}

	for pageNum := 1; pageNum <= maxPagesPerPass; pageNum++ {
		page, err := safeFFICall("CloudSyncStatuskitPeers", func() (rustpushgo.CloudSyncStatusKitPage, error) {
			return c.client.CloudSyncStatuskitPeers(currentZone, currentToken)
		})
		if err != nil {
			ffiErr = err
			hint := extractRetryAfterHint(err)
			nextErrs := meta.ConsecutiveErrs + 1
			nextMeta := statusKitPassMeta{LastAttempt: attemptStart, ConsecutiveErrs: nextErrs}
			log.Info().
				Err(err).
				Int("page", pageNum).
				Int("consecutive_errors", nextErrs).
				Dur("retry_after_hint", hint).
				Time("next_allowed", nextMeta.nextAllowedAt(hint)).
				Msg("StatusKit-CloudKit pass: FFI returned error — applying inter-pass backoff")
			return fmt.Errorf("CloudSyncStatuskitPeers (page %d): %w", pageNum, err)
		}

		totalFetched += page.Fetched
		totalInserted += page.Inserted
		totalAlreadyKnown += page.AlreadyKnown
		totalDecodeFailed += page.DecodeFailed
		totalRecordsSeen += page.RecordsSeen
		if len(page.InjectedHandles) > 0 {
			injectedHandles = append(injectedHandles, page.InjectedHandles...)
		}

		// Feed every successfully-decoded (channel_id, sender_handle) pair
		// from this page into the persistent alias-cluster store. Catches
		// peers whose only reshare arrived while the bridge was offline —
		// those bypass the live OnReshareSender callback and would otherwise
		// be invisible to alias correlation.
		for _, obs := range page.ClusterObservations {
			c.recordReshareObservation(ctx, log, obs.ChannelId, obs.SenderHandle)
		}

		pageLog := log.Info().
			Int("page", pageNum).
			Uint32("fetched", page.Fetched).
			Uint32("inserted", page.Inserted).
			Uint32("already_known", page.AlreadyKnown).
			Uint32("decode_failed", page.DecodeFailed).
			Uint32("records_seen", page.RecordsSeen).
			Int("injected_handles_count", len(page.InjectedHandles))
		if page.ResolvedZone != nil {
			pageLog = pageLog.Str("resolved_zone", *page.ResolvedZone)
		}
		if page.DiscoverySummary != nil {
			pageLog = pageLog.Str("discovery_summary", *page.DiscoverySummary)
		}
		if len(page.InjectedHandles) > 0 {
			pageLog = pageLog.Strs("injected_handles", page.InjectedHandles)
		}
		pageLog.Msg("StatusKit-CloudKit pass: page returned")

		// Persist resolved zone name. If FFI returned ResolvedZone=nil,
		// treat that as a directive to clear the cached zone (discovery
		// found nothing, or the cached zone is stale and fetch failed).
		if page.ResolvedZone == nil {
			if currentZone != nil {
				if err := c.cloudStore.clearZoneToken(ctx, statusKitCloudZoneRow); err != nil {
					log.Info().Err(err).Msg("StatusKit-CloudKit pass: failed to clear cached zone row")
				} else {
					log.Info().Msg("StatusKit-CloudKit pass: cleared cached zone (FFI returned ResolvedZone=nil)")
				}
				currentZone = nil
			}
		} else if currentZone == nil || *currentZone != *page.ResolvedZone {
			zoneStr := *page.ResolvedZone
			if err := c.cloudStore.setSyncStateSuccess(ctx, statusKitCloudZoneRow, &zoneStr); err != nil {
				log.Info().Err(err).Msg("StatusKit-CloudKit pass: failed to persist resolved zone name")
			} else {
				log.Info().Str("zone", zoneStr).Msg("StatusKit-CloudKit pass: cached resolved zone name for future passes")
			}
			currentZone = &zoneStr
		}

		// Persist (or clear) the continuation token after every page so a
		// crash mid-drain doesn't lose progress — the next pass resumes
		// from the persisted token.
		if page.NextToken != nil && len(*page.NextToken) > 0 {
			encoded := base64.StdEncoding.EncodeToString(*page.NextToken)
			if err := c.cloudStore.setSyncStateSuccess(ctx, statusKitCloudTokenRow, &encoded); err != nil {
				log.Info().Err(err).Msg("StatusKit-CloudKit pass: failed to persist next continuation token")
			}
		} else if page.ResolvedZone == nil && currentToken != nil {
			// FFI signalled re-discovery. Drop the stale token so the
			// next pass starts fresh.
			if err := c.cloudStore.clearZoneToken(ctx, statusKitCloudTokenRow); err != nil {
				log.Info().Err(err).Msg("StatusKit-CloudKit pass: failed to clear stale continuation token")
			}
		}

		// Done detection mirrors syncCloudMessages: stop when the API signals
		// no more pages (missing/empty continuation token or an empty fetch),
		// or when the token stops advancing (defensive against a server that
		// keeps echoing the same token). Any of these means the zone is fully
		// walked, so latch drainComplete — the ONLY condition that lets the
		// inter-pass gate fall through from the short resume floor to the
		// fresh/12h floors.
		nextTokenStr := ""
		if page.NextToken != nil {
			nextTokenStr = base64.StdEncoding.EncodeToString(*page.NextToken)
		}
		if page.NextToken == nil || len(*page.NextToken) == 0 || page.Fetched == 0 || (pageNum > 1 && nextTokenStr == prevTokenStr) {
			drainComplete = true
			break
		}
		prevTokenStr = nextTokenStr
		currentToken = page.NextToken

		// Pace the next page fetch — slow and steady. On the bounded-ctx
		// periodic path a cancel here returns mid-walk with drainComplete still
		// false, so the next pass resumes from the persisted token (the gate
		// uses the short resume floor). The post-backfill restart path runs
		// under context.Background and never cancels, so it drains the whole
		// zone in one pass.
		select {
		case <-time.After(interPageDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	log.Info().
		Uint32("total_fetched", totalFetched).
		Uint32("total_inserted", totalInserted).
		Uint32("total_already_known", totalAlreadyKnown).
		Uint32("total_decode_failed", totalDecodeFailed).
		Uint32("total_records_seen", totalRecordsSeen).
		Int("total_injected_handles", len(injectedHandles)).
		Bool("drain_complete", drainComplete).
		Msg("StatusKit-CloudKit pass: drain complete")

	// If we injected any new peer keys (across all drained pages), fire a
	// presence-resubscribe so APNs interest tokens cover the new channels
	// immediately rather than waiting for the next subscribeAfterInit cycle.
	if totalInserted > 0 {
		log.Info().Uint32("total_inserted", totalInserted).Msg("StatusKit-CloudKit pass: triggering presence resubscribe for newly-injected peers")
		c.subscribeToContactPresence(log)
	}

	// Per-cycle alias-link confirmation. Walks every peer handle in
	// state.keys (now freshly populated by the CloudKit drain), uses local
	// data first (alias_portal cache, cluster store, contacts), and only
	// hits Apple for handles still unresolved after that — in a single
	// batched IDS call regardless of count. This is the 12h-cadence path
	// the bridge-start hook also drives once at boot.
	c.batchLinkStatusKitAliases(ctx, log)

	return nil
}
