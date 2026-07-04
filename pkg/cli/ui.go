// corten-matrix - terminal UI helpers shared by the management CLI.
package cli

import (
	"fmt"
	"os"
)

// ANSI styling for CLI output. Cleared to empty strings on a non-TTY or when
// NO_COLOR is set (see init), so piped/redirected output stays clean.
var (
	cAccent = "\x1b[38;5;73m" // corten/teal accent
	cGreen  = "\x1b[32m"
	cRed    = "\x1b[31m"
	cDim    = "\x1b[2m"
	cBold   = "\x1b[1m"
	cReset  = "\x1b[0m"
)

func init() {
	if fi, _ := os.Stdout.Stat(); fi == nil || (fi.Mode()&os.ModeCharDevice) == 0 || os.Getenv("NO_COLOR") != "" {
		cAccent, cGreen, cRed, cDim, cBold, cReset = "", "", "", "", "", ""
	}
}

// die prints a formatted error and exits non-zero.
func die(f string, a ...any) {
	fmt.Printf("\n  %s✗%s  "+f+"\n\n", append([]any{cRed, cReset}, a...)...)
	os.Exit(1)
}
