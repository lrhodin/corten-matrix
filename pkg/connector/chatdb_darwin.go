//go:build darwin

package connector

// Register the macOS chat.db platform (contacts, backfill, sleep detection).
// This side-effect import is Darwin-only because imessage/mac links against
// macOS frameworks (IOKit, Foundation, Contacts).
import _ "github.com/lrhodin/corten-matrix/imessage/mac"
