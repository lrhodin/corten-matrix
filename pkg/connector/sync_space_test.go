package connector

import (
	"strings"
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

// TestSyncSpaceHandlerShape checks the handler is shaped the way BridgeCommands
// expects, so dropping `cmdSyncSpace` into that list can't be the thing that
// breaks. RequiresLogin is what lets fnSyncSpace assume GetDefaultLogin works.
func TestSyncSpaceHandlerShape(t *testing.T) {
	registered := []*commands.FullHandler{cmdSyncSpace}
	cmd := registered[0]
	if cmd.Name != "sync-space" {
		t.Errorf("unexpected command name %q", cmd.Name)
	}
	if cmd.Func == nil {
		t.Error("command has no handler func")
	}
	if !cmd.RequiresLogin {
		t.Error("sync-space operates on a login's portals, so it must require a login")
	}
	if cmd.Help.Section != commands.HelpSectionChats {
		t.Errorf("unexpected help section %+v", cmd.Help.Section)
	}
	if cmd.Help.Description == "" {
		t.Error("command needs a help description")
	}
}

func syncSpaceTestPortal(portalID string, receiver networkid.UserLoginID, mxid id.RoomID) *bridgev2.Portal {
	return &bridgev2.Portal{
		Portal: &database.Portal{
			PortalKey: networkid.PortalKey{
				ID:       networkid.PortalID(portalID),
				Receiver: receiver,
			},
			MXID: mxid,
		},
	}
}

func TestSyncSpacePortalsForLogin(t *testing.T) {
	const mine = networkid.UserLoginID("login-mine")
	const theirs = networkid.UserLoginID("login-theirs")

	portals := []*bridgev2.Portal{
		syncSpaceTestPortal("tel:+15551234567", mine, "!a:example.org"),
		// Another account on the same bridge — must not be swept.
		syncSpaceTestPortal("tel:+15557654321", theirs, "!b:example.org"),
		// Known chat with no Matrix room yet; nothing to join or file.
		syncSpaceTestPortal("mailto:nobody@example.com", mine, ""),
		syncSpaceTestPortal("gid:1234", mine, "!c:example.org"),
		nil,
		// Guards the embedded-pointer dereference in the filter.
		{},
	}

	got := syncSpacePortalsForLogin(portals, mine)
	if len(got) != 2 {
		t.Fatalf("expected 2 portals for %s, got %d", mine, len(got))
	}
	if got[0].MXID != "!a:example.org" || got[1].MXID != "!c:example.org" {
		t.Errorf("unexpected portals selected: %s, %s", got[0].MXID, got[1].MXID)
	}
}

func TestSyncSpacePortalsForLoginEmpty(t *testing.T) {
	if got := syncSpacePortalsForLogin(nil, "login"); len(got) != 0 {
		t.Errorf("expected no portals from a nil slice, got %d", len(got))
	}
}

func TestSyncSpaceCountsSummary(t *testing.T) {
	tests := []struct {
		name        string
		counts      syncSpaceCounts
		wantSubstrs []string
		notWant     string
	}{
		{
			name:        "clean run",
			counts:      syncSpaceCounts{joined: 12, addedToSpace: 12},
			wantSubstrs: []string{"joined 12", "added to your space 12", "no failures"},
			notWant:     "failures:",
		},
		{
			// The re-run shape: everything already joined and already filed.
			name:        "idempotent re-run",
			counts:      syncSpaceCounts{joined: 400, alreadyInSpace: 400},
			wantSubstrs: []string{"joined 400", "added to your space 0", "already there 400", "no failures"},
			notWant:     "failures:",
		},
		{
			name:        "partial failures",
			counts:      syncSpaceCounts{joined: 8, joinFailed: 2, addedToSpace: 7, spaceFailed: 3},
			wantSubstrs: []string{"joined 8", "failures: 2 join(s) and 3 space add(s)", "bridge log"},
			notWant:     "no failures",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.counts.summary()
			for _, want := range tt.wantSubstrs {
				if !strings.Contains(got, want) {
					t.Errorf("summary %q missing %q", got, want)
				}
			}
			if strings.Contains(got, tt.notWant) {
				t.Errorf("summary %q should not contain %q", got, tt.notWant)
			}
		})
	}
}

func TestSyncSpaceInFlightGuard(t *testing.T) {
	const a = networkid.UserLoginID("login-a")
	const b = networkid.UserLoginID("login-b")
	t.Cleanup(func() {
		syncSpaceRelease(a)
		syncSpaceRelease(b)
	})

	if !syncSpaceAcquire(a) {
		t.Fatal("first acquire should succeed")
	}
	if syncSpaceAcquire(a) {
		t.Error("second acquire for the same login should be refused")
	}
	// A concurrent sweep for a different login is fine — the pacing is per-login
	// and each account has its own homeserver work to do.
	if !syncSpaceAcquire(b) {
		t.Error("acquire for a different login should succeed")
	}
	syncSpaceRelease(a)
	if !syncSpaceAcquire(a) {
		t.Error("acquire should succeed again after release")
	}
}
