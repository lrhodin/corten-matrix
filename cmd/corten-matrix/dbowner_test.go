package main

import (
	"testing"

	"github.com/lrhodin/corten-matrix/pkg/dbowner"
)

// TestDatabaseOwnerTracksBridgeName is the guard that would have caught
// bc2101c7. mxmain opens the database as "megabridge/"+BridgeMain.Name
// (mxmain/main.go), so renaming the bridge orphans every existing database
// unless pkg/dbowner is updated in the same commit — every pre-rename install
// then crash-loops on ErrNotOwned with a message about "different programs".
//
// If this fails, someone renamed the bridge again. Add the current value of
// dbowner.New to the migration's accepted-old list BEFORE changing it.
func TestDatabaseOwnerTracksBridgeName(t *testing.T) {
	if want := "megabridge/" + m.Name; dbowner.New != want {
		t.Errorf("dbowner.New = %q, but mxmain opens the database as %q.\n"+
			"The bridge was renamed without updating pkg/dbowner; every "+
			"pre-rename install will crash-loop on startup.", dbowner.New, want)
	}
}
