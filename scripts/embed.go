// Package scripts embeds the install/management shell scripts so the
// corten-matrix binary runs them in-process without shipping loose files.
// These live at the repo's top-level /scripts (single source of truth);
// pkg/cli reads them through this embed.FS.
package scripts

import "embed"

//go:embed install.sh install-linux.sh install-beeper.sh install-beeper-linux.sh reset-bridge.sh
var Files embed.FS
