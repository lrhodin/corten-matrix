// corten-matrix - A Matrix-iMessage puppeting bridge.

package connector

// HandleHostCommand lets the build configuration's host-command extensions
// claim a management subcommand before the binary's normal dispatch. args is
// os.Args[1:]; version/goos/goarch describe the running build. The base build
// registers no extensions, so this always declines (returns false) and normal
// dispatch proceeds. An extension that handles a command acts on it — usually
// terminating the process itself — and returns true.
func HandleHostCommand(args []string, version, goos, goarch string) bool { return false }

// ExtraHostHelp returns extra {command, description} rows for the `help`
// listing. The base build contributes none.
func ExtraHostHelp() [][2]string { return nil }
