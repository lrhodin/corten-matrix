// corten-matrix - host-side management CLI.
//
// These subcommands give bridge users the same familiar operations as the old
// Makefile / Docker `imessage` wrapper, but baked into the binary: setup,
// setup-beeper, start, stop, restart, status, logs, bbctl, reset, uninstall.
//
// Design:
//   - setup / setup-beeper / reset run the project's existing shell scripts,
//     embedded into the binary (so behaviour matches what users know today).
//   - start / stop / restart / status / logs / bbctl are handled natively
//     (launchd on macOS, systemd --user on Linux).
//   - Docker-aware: inside a container, host-lifecycle commands no-op because
//     Docker Compose + the `imessage` host wrapper drive the lifecycle there;
//     the bridge daemon itself still runs via the container entrypoint.

// Package cli is the shared, CGO-free management/install CLI used by both the
// pure-Go installer bundle (cmd/corten-installer) and the post-install bridge
// binary (cmd/corten-matrix).
package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/lrhodin/corten-matrix/pkg/bbctl"
	"github.com/lrhodin/corten-matrix/scripts"
)

const cortenBundleID = "com.lrhodin.corten-matrix"

func cortenDataDir() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "corten-matrix")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "corten-matrix")
}

// DataDir returns the bridge data directory (~/.local/share/corten-matrix, or
// $XDG_DATA_HOME/corten-matrix) where config.yaml lives. Exported so the login
// subcommand resolves the same path as the rest of the CLI.
func DataDir() string { return cortenDataDir() }

// secondDataDir is the data dir of the optional second account — a sibling of
// the first account's dir (…/corten-matrix-1). Setup creates it only if the
// user opts into a second account.
func secondDataDir() string {
	return filepath.Join(filepath.Dir(cortenDataDir()), "corten-matrix-1")
}

// hasSecondAccount reports whether a second account has been set up.
func hasSecondAccount() bool {
	fi, err := os.Stat(secondDataDir())
	return err == nil && fi.IsDir()
}

// serviceLabel returns the launchd label (macOS) / systemd unit base name
// (Linux) for account idx (0 = primary, 1 = second). These mirror the names the
// install scripts use (BUNDLE_ID / SERVICE_NAME).
func serviceLabel(idx int) string {
	base := cortenBundleID // com.lrhodin.corten-matrix
	if runtime.GOOS != "darwin" {
		base = "corten-matrix"
	}
	if idx == 0 {
		return base
	}
	return base + "-1"
}


// selfPath returns the absolute path of this binary (resolving symlinks).
func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	if rp, err := filepath.EvalSymlinks(p); err == nil {
		return rp
	}
	return p
}

func streamRun(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// exitWith runs a command, streaming stdio, and exits with its status code.
func exitWith(name string, args ...string) {
	if err := streamRun(name, args...); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "corten-matrix: %s: %v\n", name, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// runEmbeddedScript extracts an embedded management script to a temp file and
// execs it with args, streaming stdio and propagating its exit code.
func runEmbeddedScript(name string, args ...string) {
	data, err := scripts.Files.ReadFile(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "corten-matrix: embedded script %q missing: %v\n", name, err)
		os.Exit(1)
	}
	tmp, err := os.CreateTemp("", "corten-*.sh")
	if err != nil {
		fmt.Fprintf(os.Stderr, "corten-matrix: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		fmt.Fprintf(os.Stderr, "corten-matrix: %v\n", err)
		os.Exit(1)
	}
	tmp.Close()
	_ = os.Chmod(tmp.Name(), 0o755)
	exitWith("/bin/bash", append([]string{tmp.Name()}, args...)...)
}

// serviceCtl runs a lifecycle action on EVERY configured account's service:
// start/stop/restart/status act on both accounts in one shot. (Only `logs` is
// per-account — see tailLogs.) Exits non-zero if any account's action failed.
func serviceCtl(action string) {
	labels := []string{serviceLabel(0)}
	if hasSecondAccount() {
		labels = append(labels, serviceLabel(1))
	}
	failed := false
	for _, label := range labels {
		if err := serviceCtlOne(action, label); err != nil {
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
	os.Exit(0)
}

// serviceCtlOne runs one action against a single account's service label
// (launchd label on macOS, systemd --user unit base name on Linux).
func serviceCtlOne(action, label string) error {
	if runtime.GOOS == "darwin" {
		plist := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", label+".plist")
		switch action {
		case "start":
			return streamRun("launchctl", "load", "-w", plist)
		case "stop":
			return streamRun("launchctl", "unload", "-w", plist)
		case "restart":
			return streamRun("launchctl", "kickstart", "-k", "gui/"+strconv.Itoa(os.Getuid())+"/"+label)
		case "status":
			return streamRun("launchctl", "list", label)
		}
		return nil
	}
	// Linux: systemd --user unit installed by the install scripts.
	unit := label + ".service"
	switch action {
	case "start":
		return streamRun("systemctl", "--user", "start", unit)
	case "stop":
		return streamRun("systemctl", "--user", "stop", unit)
	case "restart":
		return streamRun("systemctl", "--user", "restart", unit)
	case "status":
		return streamRun("systemctl", "--user", "status", unit)
	}
	return nil
}

// ── Setup orchestration: account 1, then an optional second account ──────────
// Max two accounts. A second account is a different Apple ID on the same
// machine, run as its own bridge (own appservice name, data dir, login, and
// service) so the two never share login state. The lifecycle commands act on
// both; only logs are per-account.

// isInteractive reports whether stdin is a terminal (so we can prompt).
func isInteractive() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

// configBackfillSource returns an account's `backfill_source:` value
// ("cloudkit" / "chatdb"), or "" if unset/unreadable.
func configBackfillSource(dataDir string) string {
	data, err := os.ReadFile(filepath.Join(dataDir, "config.yaml"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "backfill_source:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "backfill_source:"))
		}
	}
	return ""
}

// canOfferSecondAccount reports whether to offer a second account after the
// first one's setup. Interactive only; never more than two. On macOS a second
// account is only possible with CloudKit — the local chat.db serves only the
// one signed-in Apple ID — so it is silently not offered for a chat.db setup.
func canOfferSecondAccount() bool {
	if !isInteractive() || hasSecondAccount() {
		return false
	}
	if runtime.GOOS == "darwin" {
		return configBackfillSource(cortenDataDir()) == "cloudkit"
	}
	return true
}

// runSetupScript runs an embedded setup script with extra env, streaming stdio,
// and returns its exit error (does NOT exit the process).
func runSetupScript(extraEnv []string, name string, args ...string) error {
	data, err := scripts.Files.ReadFile(name)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "corten-*.sh")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	tmp.Close()
	_ = os.Chmod(tmp.Name(), 0o755)
	c := exec.Command("/bin/bash", append([]string{tmp.Name()}, args...)...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	c.Env = append(os.Environ(), extraEnv...)
	return c.Run()
}

// setupAccount runs the install script for account idx (0 = primary, 1 = the
// optional second account) in the given mode (Beeper vs self-hosted).
func setupAccount(beeper bool, idx int) error {
	self := selfPath()
	bbctlPath := filepath.Join(filepath.Dir(self), "bbctl")
	dataDir := cortenDataDir()
	bundleID := cortenBundleID
	var env []string
	if idx == 1 {
		dataDir = secondDataDir()
		bundleID = cortenBundleID + "-1"
		env = []string{
			"BRIDGE_NAME=sh-imessage1",     // Beeper appservice name for the 2nd account
			"SERVICE_NAME=corten-matrix-1", // local systemd unit / launchd suffix
			"XDG_DATA_HOME=" + dataDir,     // 2nd account's own login/session dir
		}
	}
	var name string
	var args []string
	switch {
	case beeper && runtime.GOOS == "darwin":
		name, args = "install-beeper.sh", []string{self, dataDir, bundleID, bbctlPath}
	case beeper:
		name, args = "install-beeper-linux.sh", []string{self, dataDir, bbctlPath}
	case runtime.GOOS == "darwin":
		name, args = "install.sh", []string{self, dataDir, bundleID}
	default:
		name, args = "install-linux.sh", []string{self, dataDir}
	}
	return runSetupScript(env, name, args...)
}

func exitCodeOf(err error) int {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return 1
}

// runSetup runs the primary account's setup, then offers ONE optional second
// account (Beeper appservice sh-imessage1 / local corten-matrix-1).
func runSetup(beeper bool) {
	if err := setupAccount(beeper, 0); err != nil {
		os.Exit(exitCodeOf(err))
	}
	if canOfferSecondAccount() {
		fmt.Print("\nAdd a second iMessage account (different Apple ID)? [y/N]: ")
		var ans string
		fmt.Scanln(&ans)
		switch ans {
		case "y", "Y", "yes", "Yes":
			fmt.Printf("\n%s═══ Setting up the second account ═══%s\n", cAccent, cReset)
			if err := setupAccount(beeper, 1); err != nil {
				fmt.Fprintf(os.Stderr, "second account setup failed: %v\n", err)
				os.Exit(exitCodeOf(err))
			}
		}
	}
	os.Exit(0)
}

// offerAddToPath offers to symlink the binary into /usr/local/bin so `corten-matrix`
// works as a bare command. No shell-rc edits — just a symlink in a standard PATH dir.
func offerAddToPath(self string) {
	const target = "/usr/local/bin/corten-matrix"
	if self == target {
		return
	}
	if p, err := exec.LookPath("corten-matrix"); err == nil && p != "" {
		return // already on PATH
	}
	fmt.Printf("  Add 'corten-matrix' to your PATH (symlink %s)? [Y/n]: ", target)
	var ans string
	fmt.Scanln(&ans)
	if ans == "n" || ans == "N" || ans == "no" || ans == "No" {
		return
	}
	_ = os.Remove(target)
	if os.Symlink(self, target) == nil {
		fmt.Printf("  %s✓%s corten-matrix is on your PATH\n", cGreen, cReset)
		return
	}
	c := exec.Command("sudo", "ln", "-sf", self, target)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if c.Run() == nil {
		fmt.Printf("  %s✓%s corten-matrix is on your PATH\n", cGreen, cReset)
		return
	}
	fmt.Printf("  run manually: sudo ln -sf %s %s\n", self, target)
}

// serviceInstall installs (and starts) the bridge as a user service pointing at
// this binary. setup/setup-beeper call this; it's also exposed as a manual command.
func serviceInstall() {
	self := selfPath()
	offerAddToPath(self)
	data := cortenDataDir()
	_ = os.MkdirAll(filepath.Join(data, "logs"), 0o755)
	if runtime.GOOS == "darwin" {
		dir := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents")
		_ = os.MkdirAll(dir, 0o755)
		plist := filepath.Join(dir, cortenBundleID+".plist")
		body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key><array><string>%s</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>WorkingDirectory</key><string>%s</string>
  <key>StandardOutPath</key><string>%s/logs/bridge.log</string>
  <key>StandardErrorPath</key><string>%s/logs/bridge.log</string>
</dict></plist>
`, cortenBundleID, self, data, data, data)
		if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
			die("write launchd plist: %v", err)
		}
		_ = exec.Command("launchctl", "unload", plist).Run()
		exitWith("launchctl", "load", "-w", plist)
	}
	// Linux: systemd --user unit.
	dir := filepath.Join(os.Getenv("HOME"), ".config", "systemd", "user")
	_ = os.MkdirAll(dir, 0o755)
	unit := filepath.Join(dir, "corten-matrix.service")
	body := fmt.Sprintf(`[Unit]
Description=corten-matrix iMessage bridge
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s
WorkingDirectory=%s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, self, data)
	if err := os.WriteFile(unit, []byte(body), 0o644); err != nil {
		die("write systemd unit: %v", err)
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	exitWith("systemctl", "--user", "enable", "--now", "corten-matrix.service")
}

// serviceUninstall stops and removes the bridge service unit.
func serviceUninstall() {
	if runtime.GOOS == "darwin" {
		plist := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", cortenBundleID+".plist")
		_ = exec.Command("launchctl", "unload", "-w", plist).Run()
		_ = os.Remove(plist)
		fmt.Println("corten-matrix service removed.")
		os.Exit(0)
	}
	_ = exec.Command("systemctl", "--user", "disable", "--now", "corten-matrix.service").Run()
	_ = os.Remove(filepath.Join(os.Getenv("HOME"), ".config", "systemd", "user", "corten-matrix.service"))
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	fmt.Println("corten-matrix service removed.")
	os.Exit(0)
}

// tailLogs tails a bridge log. `logs` → primary account; `logs 2` → 2nd account.
func tailLogs(args []string) {
	dir := cortenDataDir()
	if len(args) > 0 && args[0] == "2" {
		dir = secondDataDir()
	}
	exitWith("tail", "-F", filepath.Join(dir, "logs", "bridge.log"))
}

func runBbctl(args []string) {
	// bbctl is compiled into this binary (pkg/bbctl) — run it in-process,
	// no separate bbctl binary to ship or locate.
	bbctl.Run(append([]string{"bbctl"}, args...))
	os.Exit(0)
}

// printHelp shows the user-facing command list (clean, accent-colored).
func PrintHelp() {
	hdr := cBold + cAccent + "◆ corten-matrix" + cReset
	fmt.Println()
	fmt.Printf("  %s  %sMatrix ↔ iMessage bridge%s\n\n", hdr, cDim, cReset)
	fmt.Printf("  %sUsage:%s corten-matrix <command>\n\n", cDim, cReset)
	rows := [][2]string{
		{"setup", "configure & start the bridge"},
		{"setup-beeper", "configure for Beeper"},
		{"start", "start the bridge"},
		{"stop", "stop the bridge"},
		{"restart", "restart the bridge"},
		{"status", "show service status"},
		{"logs [2]", "tail a bridge log (2 = second account)"},
		{"install-service", "install + start the background service"},
		{"uninstall-service", "stop + remove the background service"},
		{"reset", "reset bridge state"},
		{"uninstall", "remove the service"},
		{"login", "re-run the iMessage login flow"},
		{"bbctl <args>", "Beeper bridge-manager CLI"},
		{"help", "show this help"},
	}
	for _, r := range rows {
		fmt.Printf("    %s%-14s%s %s%s%s\n", cAccent, r[0], cReset, cDim, r[1], cReset)
	}
	fmt.Println()
	if hasSecondAccount() {
		fmt.Printf("  %sTwo accounts configured — start/stop/restart act on both; use 'logs 2' for the second.%s\n\n", cDim, cReset)
	}
}

// isManagementCommand reports whether cmd is a host-management subcommand.
func IsManagementCommand(cmd string) bool {
	switch cmd {
	case "setup", "setup-beeper", "start", "stop", "restart",
		"status", "logs", "bbctl", "reset", "uninstall",
		"install-service", "uninstall-service":
		return true
	}
	return false
}

// runManagementCommand dispatches a host-management subcommand. It always
// terminates the process (os.Exit) — it is only called for known commands.
func RunManagement(cmd string, args []string) {
	switch cmd {
	case "setup":
		runSetup(false)
	case "setup-beeper":
		runSetup(true)
	case "reset":
		if runtime.GOOS == "darwin" {
			runEmbeddedScript("reset-bridge.sh", cortenBundleID)
		}
		runEmbeddedScript("reset-bridge.sh")
	case "install-service":
		serviceInstall()
	case "uninstall-service", "uninstall":
		serviceUninstall()
	case "start", "stop", "restart", "status":
		serviceCtl(cmd)
	case "logs":
		tailLogs(args)
	case "bbctl":
		runBbctl(args)
	}
	os.Exit(0)
}
