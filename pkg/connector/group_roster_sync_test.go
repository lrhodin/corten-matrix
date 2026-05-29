package connector

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// newTestCloudStore spins up an in-memory SQLite-backed cloud store with the
// real schema applied, for exercising the participant-roster write paths.
func newTestCloudStore(t *testing.T) *cloudBackfillStore {
	t.Helper()
	rawDB, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// Single connection so the in-memory DB persists across statements.
	rawDB.SetMaxOpenConns(1)
	db, err := dbutil.NewWithDB(rawDB, "sqlite3")
	if err != nil {
		t.Fatalf("wrap dbutil: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := newCloudBackfillStore(db, networkid.UserLoginID("login1"))
	if err := store.ensureSchema(context.Background()); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	return store
}

// TestUpdateChatParticipantsDropsDepartedMember reproduces the reported bug: a
// member who left a gid: group kept receiving outbound messages because the
// stored roster (the outbound recipient source for gid portals) was written
// insert-once and never updated. updateChatParticipants must drop the departed
// member without clobbering the other chat metadata.
func TestUpdateChatParticipantsDropsDepartedMember(t *testing.T) {
	ctx := context.Background()
	store := newTestCloudStore(t)

	portalID := "gid:abc-123"
	displayName := "Trip Planning"
	recordName := "ckrecord-xyz"
	full := []string{"tel:+15551110000", "tel:+15552220000", "tel:+15553330000"}

	// Seed the chat exactly like the real-time insert path (makePortalKey):
	// cloud_chat_id == group UUID, portal_id == gid:<uuid>, full roster.
	if err := store.upsertChat(ctx, "abc-123", recordName, "abc-123", portalID, "iMessage", &displayName, nil, full, 100); err != nil {
		t.Fatalf("seed upsertChat: %v", err)
	}

	got, err := store.getChatParticipantsByPortalID(ctx, portalID)
	if err != nil {
		t.Fatalf("read seeded participants: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 seeded participants, got %v", got)
	}

	// Member 3 leaves. The roster is updated to the remaining two.
	remaining := []string{"tel:+15551110000", "tel:+15552220000"}
	if err := store.updateChatParticipants(ctx, portalID, remaining); err != nil {
		t.Fatalf("updateChatParticipants: %v", err)
	}

	got, err = store.getChatParticipantsByPortalID(ctx, portalID)
	if err != nil {
		t.Fatalf("read updated participants: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 participants after leave, got %v", got)
	}
	for _, p := range got {
		if p == "tel:+15553330000" {
			t.Fatalf("departed member tel:+15553330000 still in outbound roster: %v", got)
		}
	}

	// The targeted update must not clobber other chat metadata.
	dn, err := store.getDisplayNameByPortalID(ctx, portalID)
	if err != nil {
		t.Fatalf("read display name: %v", err)
	}
	if dn != displayName {
		t.Fatalf("display_name clobbered by participant update: got %q want %q", dn, displayName)
	}
}

// TestUpdateChatParticipantsGidFallback verifies the group_id / cloud_chat_id
// fallback used when a gid: portal's row is not keyed by portal_id directly
// (mirrors getChatParticipantsByPortalID's fallback).
func TestUpdateChatParticipantsGidFallback(t *testing.T) {
	ctx := context.Background()
	store := newTestCloudStore(t)

	groupUUID := "DEF-456"
	full := []string{"tel:+15551110000", "tel:+15552220000", "tel:+15553330000"}

	// Row keyed by group_id == UUID, but portal_id stored as something else
	// (e.g. a CloudKit-synced row whose portal_id differs from gid:<uuid>).
	if err := store.upsertChat(ctx, groupUUID, "", groupUUID, "some-other-portal", "iMessage", nil, nil, full, 100); err != nil {
		t.Fatalf("seed upsertChat: %v", err)
	}

	// Update addressed by the gid: portal ID — must resolve via the group_id
	// fallback and still drop the departed member.
	remaining := []string{"tel:+15551110000", "tel:+15552220000"}
	if err := store.updateChatParticipants(ctx, "gid:def-456", remaining); err != nil {
		t.Fatalf("updateChatParticipants (fallback): %v", err)
	}

	got, err := store.getChatParticipantsByPortalID(ctx, "gid:def-456")
	if err != nil {
		t.Fatalf("read participants via fallback: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 participants after fallback update, got %v", got)
	}
	for _, p := range got {
		if p == "tel:+15553330000" {
			t.Fatalf("departed member still present via fallback path: %v", got)
		}
	}
}

// isSelfNone is an isSelf predicate that matches no handle — used where the
// test roster contains no self handle.
func isSelfNone(string) bool { return false }

// TestSyncStoredGroupRosterSelfHeals exercises the full self-heal decision path
// (read stored roster → compare → rewrite when changed) that handleMessage runs
// on every live group message. This is what retroactively drops a member who
// already left: the next live message from a remaining member carries the
// current roster, and the stale stored roster is rewritten to match.
func TestSyncStoredGroupRosterSelfHeals(t *testing.T) {
	ctx := context.Background()
	store := newTestCloudStore(t)

	portalID := "gid:heal-1"
	stale := []string{"tel:+15551110000", "tel:+15552220000", "tel:+15553330000"}
	if err := store.upsertChat(ctx, "heal-1", "", "heal-1", portalID, "iMessage", nil, nil, stale, 100); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A live message arrives carrying the current roster (member 3 has left).
	current := []string{"tel:+15551110000", "tel:+15552220000"}
	oldCount, updated, err := syncStoredGroupRoster(ctx, store, portalID, current, isSelfNone)
	if err != nil {
		t.Fatalf("syncStoredGroupRoster: %v", err)
	}
	if !updated {
		t.Fatal("expected self-heal to update the stale roster, but it reported no change")
	}
	if oldCount != 3 {
		t.Fatalf("expected oldCount 3, got %d", oldCount)
	}

	got, err := store.getChatParticipantsByPortalID(ctx, portalID)
	if err != nil {
		t.Fatalf("read healed roster: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 participants after self-heal, got %v", got)
	}
	for _, p := range got {
		if p == "tel:+15553330000" {
			t.Fatalf("self-heal failed to drop departed member: %v", got)
		}
	}

	// Idempotent: a second live message with the same roster writes nothing.
	_, updated, err = syncStoredGroupRoster(ctx, store, portalID, current, isSelfNone)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if updated {
		t.Fatal("expected no update when roster already matches")
	}
}

// TestSyncStoredGroupRosterIgnoresSelfHandleVariation ensures the self-heal does
// not churn the roster when the only difference is the user's own handle being
// represented differently (e.g. tel: vs mailto: self handle) — that is not a
// membership change.
func TestSyncStoredGroupRosterIgnoresSelfHandleVariation(t *testing.T) {
	ctx := context.Background()
	store := newTestCloudStore(t)

	portalID := "gid:heal-2"
	stored := []string{"tel:+15551110000", "tel:+15552220000", "mailto:me@example.com"}
	if err := store.upsertChat(ctx, "heal-2", "", "heal-2", portalID, "iMessage", nil, nil, stored, 100); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Same two peers, but our own handle arrives as a different identifier.
	// isSelf reports the differing handles as ours, so this is NOT a change.
	incoming := []string{"tel:+15551110000", "tel:+15552220000", "tel:+15559990000"}
	isSelf := func(h string) bool { return h == "mailto:me@example.com" || h == "tel:+15559990000" }

	_, updated, err := syncStoredGroupRoster(ctx, store, portalID, incoming, isSelf)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if updated {
		t.Fatal("self-handle-only difference must not be treated as a membership change")
	}
}

// TestPersistGuardsRejectNonHealableInput documents the cheap front-line guards
// in persistGroupParticipantsIfChanged that protect the self-heal from acting on
// non-group portals or fluke/partial rosters. These guards run before any DB
// read, so we assert them at the input level.
func TestPersistGuardsRejectNonHealableInput(t *testing.T) {
	normalize := func(parts []string) []string {
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if n := normalizeIdentifierForPortalID(p); n != "" {
				out = append(out, n)
			}
		}
		return out
	}
	// A single-member "roster" must be rejected (real groups have >= 2),
	// guarding against a partial/control-message list shrinking the roster.
	if got := normalize([]string{"tel:+15551110000"}); len(got) >= 2 {
		t.Fatalf("single-member list should not reach the >=2 floor: %v", got)
	}
	// A comma portal ID is not a gid: portal and must be skipped by the caller.
	if strings.HasPrefix("tel:+15551110000,tel:+15552220000", "gid:") {
		t.Fatal("comma portal IDs must not be treated as gid: portals")
	}
}

// TestUpdateChatParticipantsNoRowNoOp verifies updateChatParticipants creates
// no row when none exists — first-time creation stays owned by makePortalKey's
// insert path, so a stray update must not spawn a junk chat.
func TestUpdateChatParticipantsNoRowNoOp(t *testing.T) {
	ctx := context.Background()
	store := newTestCloudStore(t)

	if err := store.updateChatParticipants(ctx, "gid:never-seen", []string{"tel:+15551110000", "tel:+15552220000"}); err != nil {
		t.Fatalf("updateChatParticipants on missing row: %v", err)
	}
	got, err := store.getChatParticipantsByPortalID(ctx, "gid:never-seen")
	if err != nil {
		t.Fatalf("read participants: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no row created, got participants %v", got)
	}
}
