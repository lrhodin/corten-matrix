// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package connector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

// cmdSyncSpace re-runs, for every bridged room of the invoking login, the two
// things bridgev2 normally does once per portal in UserLogin.MarkInPortal
// (space.go:23-50): join the room with the user's double puppet, and file it
// into the user's personal filtering space.
//
// It exists because MarkInPortal only does either of those once. It joins the
// double puppet when there is one and otherwise merely invites the user, and it
// short-circuits on an in-memory inPortalCache the first time it runs for a
// portal. So the common "backfill first with double puppeting off so the
// timestamps land correctly, then turn double puppeting on" sequence leaves the
// whole backlog invited-but-not-joined, and enabling double puppeting later
// never re-sweeps it — the cache says the portal is already handled. This
// command goes straight to EnsureJoined + AddPortalToSpace for each portal
// instead, so the cache is not in the way.
//
// The sweep is idempotent: EnsureJoined is a no-op once the double puppet is in
// the room, and rooms the bridge has already filed are skipped outright.
var cmdSyncSpace = &commands.FullHandler{
	Name: "sync-space",
	Func: fnSyncSpace,
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Join every bridged room with your double puppet and file it into your personal space. Use after enabling double puppeting on a bridge that has already backfilled — the pending invites aren't swept automatically.",
		Args:        "",
	},
	RequiresLogin: true,
}

const (
	// syncSpacePacing is the gap left between rooms. A login with hundreds of
	// chats otherwise turns a sweep into a burst of joins and state events that
	// the homeserver will rate-limit (and, on a small server, queue behind
	// everything else the bridge is trying to do).
	syncSpacePacing = 40 * time.Millisecond
	// syncSpaceRoomTimeout bounds the homeserver calls for a single room.
	// Pacing bounds the rate but not the duration: without a deadline one
	// wedged request stalls the rest of the sweep behind it forever.
	syncSpaceRoomTimeout = 30 * time.Second
	// syncSpaceProgressEvery is how many rooms pass between progress replies.
	// At the pacing above that is roughly one update every few seconds, which
	// is often enough to show a long sweep is alive without spamming the room.
	syncSpaceProgressEvery = 100
)

// syncSpaceInFlight holds the UserLoginIDs that currently have a sweep running.
// The house pattern for a long background command is a guard map on IMClient
// (see restorePipelines in client.go), but a sweep only needs the login's
// identity, so this stays package-level: nothing to add to the client, and the
// guard survives a reconnect swapping the IMClient out mid-sweep. The command
// runs for minutes, so an impatient second invocation is easy; letting it
// through would double the homeserver load and defeat the pacing.
var syncSpaceInFlight sync.Map

// syncSpaceAcquire claims the sweep slot for a login, returning false if a
// sweep is already running for it.
func syncSpaceAcquire(loginID networkid.UserLoginID) bool {
	_, running := syncSpaceInFlight.LoadOrStore(loginID, struct{}{})
	return !running
}

func syncSpaceRelease(loginID networkid.UserLoginID) {
	syncSpaceInFlight.Delete(loginID)
}

// syncSpaceCounts tallies one sweep.
type syncSpaceCounts struct {
	joined         int
	joinFailed     int
	addedToSpace   int
	alreadyInSpace int
	spaceFailed    int
}

// summary renders the tally for a progress or completion reply.
//
// Joins are a single number because EnsureJoined cannot tell us whether it
// actually joined or found the double puppet already in the room. The space
// side can be split, because the bridge keeps its own record of what it filed.
// Reporting "already there" separately matters: filing a room into the space
// uses the bridge bot, not the double puppet, so after a double-puppeting-off
// backfill most rooms are already filed and it is the joins that were missed. A
// summary that only said "added to your space 0" would read like a failure.
func (c syncSpaceCounts) summary() string {
	base := fmt.Sprintf("joined %d (rooms already joined counted here too), added to your space %d, already there %d",
		c.joined, c.addedToSpace, c.alreadyInSpace)
	if c.joinFailed == 0 && c.spaceFailed == 0 {
		return base + ", no failures"
	}
	return fmt.Sprintf("%s, failures: %d join(s) and %d space add(s) — see the bridge log for details",
		base, c.joinFailed, c.spaceFailed)
}

// syncSpacePortalsForLogin picks out the bridged rooms belonging to one login.
// Every portal key this bridge mints carries the login as its receiver (see the
// networkid.PortalKey constructions throughout client.go), so the receiver is
// the whole ownership test. The MXID check drops portals that exist as DB rows
// but were never given a Matrix room.
func syncSpacePortalsForLogin(portals []*bridgev2.Portal, loginID networkid.UserLoginID) []*bridgev2.Portal {
	out := make([]*bridgev2.Portal, 0, len(portals))
	for _, portal := range portals {
		if portal == nil || portal.Portal == nil || portal.MXID == "" || portal.Receiver != loginID {
			continue
		}
		out = append(out, portal)
	}
	return out
}

func fnSyncSpace(ce *commands.Event) {
	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("No active login found.")
		return
	}
	dp := ce.User.DoublePuppet(ce.Ctx)
	if dp == nil {
		ce.Reply("Double puppeting isn't set up, so the bridge can't join rooms as you. Run `$cmdprefix login-matrix` first, then try again.")
		return
	}

	// Resolve the space once, up front, and branch on the room ID rather than on
	// the personal_filtering_spaces config flag. The flag being off doesn't mean
	// there is no space — a bridge that used to have it on still has the room,
	// and AddPortalToSpace still works there — and the flag being on doesn't mean
	// a space exists yet. With the flag on and no space, GetSpaceRoom creates
	// one, which is the same side effect ordinary portal processing already has.
	// Resolving here also keeps the loop off UserLogin.spaceCreateLock.
	spaceRoom, err := login.GetSpaceRoom(ce.Ctx)
	if err != nil {
		ce.Reply("Failed to resolve your personal space: %v", err)
		return
	}
	if spaceRoom == "" {
		ce.Reply("You don't have a personal filtering space, so there's nothing to file your rooms into — `personal_filtering_spaces` is disabled in the bridge config. Nothing to do.")
		return
	}

	portals, err := ce.Bridge.GetAllPortalsWithMXID(ce.Ctx)
	if err != nil {
		ce.Reply("Failed to list bridged rooms: %v", err)
		return
	}
	mine := syncSpacePortalsForLogin(portals, login.ID)
	if len(mine) == 0 {
		ce.Reply("No bridged rooms found for this login.")
		return
	}

	if !syncSpaceAcquire(login.ID) {
		ce.Reply("A sync-space sweep is already running for this login — wait for it to finish before starting another.")
		return
	}
	ce.Reply("Sweeping %d room(s) — joining each with your double puppet and filing it into your space, paced at about %d rooms a second so the homeserver isn't flooded. This runs in the background; I'll report progress here.",
		len(mine), int(time.Second/syncSpacePacing))

	// The sweep outlives this handler, so detach from the command's request
	// context while keeping its log fields. The copied event carries the same
	// detached context, so progress replies can't be canceled mid-sweep either.
	bg := *ce
	bg.Ctx = context.WithoutCancel(ce.Ctx)
	go runSyncSpaceSweep(&bg, login, dp, spaceRoom, mine)
}

func runSyncSpaceSweep(ce *commands.Event, login *bridgev2.UserLogin, dp bridgev2.MatrixAPI, spaceRoom id.RoomID, portals []*bridgev2.Portal) {
	defer syncSpaceRelease(login.ID)

	// Join the space itself before filing anything into it. GetSpaceRoom only
	// joins the double puppet to the space at creation time (space.go:200-205),
	// so a space made back when double puppeting was off leaves the user sitting
	// on an invite — the same failure mode as the rooms, and filing rooms into a
	// space the user hasn't joined would look like nothing happened. Inside the
	// sweep so the in-flight guard covers it; non-fatal, because the per-room
	// work is still worth doing if this fails.
	if err := dp.EnsureJoined(ce.Ctx, spaceRoom); err != nil {
		ce.Log.Warn().Err(err).Str("action", "sync-space").Stringer("room_id", spaceRoom).
			Msg("Failed to join personal space with double puppet")
		ce.Reply("Couldn't join your personal space (%v) — carrying on with the rooms anyway; you may need to accept the space invite yourself.", err)
	}

	var counts syncSpaceCounts
	for i, portal := range portals {
		if i > 0 {
			time.Sleep(syncSpacePacing)
		}
		syncSpaceOneRoom(ce, login, dp, portal, &counts)
		if done := i + 1; done%syncSpaceProgressEvery == 0 && done < len(portals) {
			ce.Reply("sync-space progress: %d/%d — %s", done, len(portals), counts.summary())
		}
	}
	ce.Reply("sync-space finished: %d room(s) — %s", len(portals), counts.summary())
}

// syncSpaceOneRoom does the join and the space filing for a single portal. Every
// failure is counted and logged rather than returned: one unreachable or
// half-broken room must not take the rest of the sweep down with it. The
// per-room context lives here so its cancel runs at the end of each room
// instead of piling up until the whole sweep finishes.
func syncSpaceOneRoom(ce *commands.Event, login *bridgev2.UserLogin, dp bridgev2.MatrixAPI, portal *bridgev2.Portal, counts *syncSpaceCounts) {
	ctx, cancel := context.WithTimeout(ce.Ctx, syncSpaceRoomTimeout)
	defer cancel()

	log := ce.Log.With().
		Str("action", "sync-space").
		Str("portal_id", string(portal.ID)).
		Stringer("room_id", portal.MXID).
		Logger()

	// EnsureJoined short-circuits on the appservice state store when the double
	// puppet is already in the room, so a re-run costs no requests here.
	if err := dp.EnsureJoined(ctx, portal.MXID); err != nil {
		counts.joinFailed++
		log.Warn().Err(err).Msg("Failed to join bridged room with double puppet")
	} else {
		counts.joined++
	}

	up, err := ce.Bridge.DB.UserPortal.GetOrCreate(ctx, login.UserLogin, portal.PortalKey)
	if err != nil {
		counts.spaceFailed++
		log.Warn().Err(err).Msg("Failed to load user portal row for space filing")
		return
	}
	if up.InSpace != nil && *up.InSpace {
		// The bridge's own record that it already sent the m.space.child event.
		// Trusting it is what makes a re-run cheap; the alternative is resending
		// an identical state event and hoping the homeserver deduplicates it,
		// which is not something every homeserver does.
		counts.alreadyInSpace++
		return
	}
	// CopyWithoutValues: AddPortalToSpace writes the row back, and a sweep of
	// hundreds of rooms runs long enough that passing the live row could roll
	// back a last_read or preferred update that landed while we were working.
	if err := login.AddPortalToSpace(ctx, portal, up.CopyWithoutValues()); err != nil {
		counts.spaceFailed++
		log.Warn().Err(err).Msg("Failed to add bridged room to personal space")
		return
	}
	counts.addedToSpace++
}
