package main

import (
	"fmt"
	"os"

	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"

	"github.com/lrhodin/corten-matrix/pkg/dbowner"
)

// migrateDatabaseOwner repairs the database_owner row left by the pre-rename
// binary so dbutil's ownership check passes instead of exiting 15 in a restart
// loop. See pkg/dbowner for why this is needed and what it refuses to touch.
//
// Runs between PreInit (config loaded) and Init (database opened), alongside
// the other pre-DB fixups. Errors are ignored on purpose: this is a best-effort
// repair running before the bridge's logger exists, and the real database open
// a moment later reports the real problem with much better context.
func migrateDatabaseOwner(br *mxmain.BridgeMain) {
	if br.Config == nil {
		return
	}
	migrated, err := dbowner.Migrate(br.Config.Database.Type, br.Config.Database.URI)
	if err != nil || !migrated {
		return
	}
	// br.Log isn't set up yet — stderr, matching the other pre-Init notices.
	// Silence would be worse than noise: the user is most likely staring at a
	// crash loop and needs to know what changed under them.
	fmt.Fprintf(os.Stderr, "[db] migrated database owner %q → %q (bridge renamed in bc2101c7)\n",
		dbowner.Old, dbowner.New)
}
