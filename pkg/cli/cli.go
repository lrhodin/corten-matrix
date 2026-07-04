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
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/rs/zerolog"

	"github.com/lrhodin/corten-matrix/pkg/bbctl"
	"github.com/lrhodin/corten-matrix/scripts"
)

const cortenBundleID = "com.lrhodin.corten-matrix"

// effectiveHome resolves the REAL target user's home even when the binary is
// invoked via sudo. Under sudo, $HOME / os.UserHomeDir() point at root's home
// (/root), so config, session, and anisette state would land there while the
// service — which runs as the real login user — looks for them under that user's
// home. That split is exactly the "logged in fine under /home/<user>, then a
// later run provisioned fresh under /root and failed" symptom. When running as
// root with a real $SUDO_USER, use that user's home; otherwise (a normal user,
// or true root in an LXC container) keep the existing $HOME behaviour.
func effectiveHome() string {
	if os.Geteuid() == 0 {
		if su := os.Getenv("SUDO_USER"); su != "" && su != "root" {
			if u, err := user.Lookup(su); err == nil && u.HomeDir != "" {
				return u.HomeDir
			}
		}
	}
	home, _ := os.UserHomeDir()
	return home
}

func cortenDataDir() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "corten-matrix")
	}
	return filepath.Join(effectiveHome(), ".local", "share", "corten-matrix")
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

// serviceCtl runs a lifecycle action on THE bridge service. There is ONE service
// for both accounts — its ExecStart is `corten-matrix bridge-all`, which runs
// every configured account's bridge (see RunAllBridges) — so start/stop/restart/
// status act on a single unit, not one-per-account. (`logs N` is still per-account.)
func serviceCtl(action string) {
	if err := serviceCtlOne(action, serviceLabel(0)); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// linuxSystemctl picks the systemctl mode: --user when a user session bus is
// reachable (a normal login), else system. Containers (LXC) and SSH-without-
// lingering have no user bus — `systemctl --user` there dies with "Failed to
// connect to bus", and the install scripts fall back to a SYSTEM unit
// (SYSTEMD_MODE=system) in that case, which this drives instead.
func linuxSystemctl() (base []string, system bool) {
	if exec.Command("systemctl", "--user", "show-environment").Run() == nil {
		return []string{"systemctl", "--user"}, false
	}
	if os.Geteuid() == 0 {
		return []string{"systemctl"}, true // root in a container: system bus directly
	}
	return []string{"sudo", "systemctl"}, true
}

// sysctl streams a systemctl command in whichever mode (user|system) has a bus.
func sysctl(args ...string) error {
	b, _ := linuxSystemctl()
	return streamRun(b[0], append(append([]string{}, b[1:]...), args...)...)
}

// serviceCtlOne runs one action against a single account's service label
// (launchd label on macOS, systemd unit base name on Linux).
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
	// Linux: drive the systemd unit the install scripts created, in whichever mode
	// has a bus — user normally, system in LXC/containers (see linuxSystemctl).
	unit := label + ".service"
	switch action {
	case "start", "stop", "restart", "status":
		return sysctl(action, unit)
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
	_ = os.Chmod(tmp.Name(), 0o755) // 0o755 so a dropped-to user can read/exec it

	bashArgs := append([]string{tmp.Name()}, args...)
	var c *exec.Cmd
	// When launched via sudo (root, but a real $SUDO_USER), run the script — and
	// the bridge `login` it spawns — AS that user via `sudo -u`, so config,
	// session, and anisette are owned by the login user (not root) and land in
	// their home. The script then sudo-elevates only the systemd bits itself
	// ($SUDO). Without this, a sudo-launched setup writes root-owned state into
	// /home/<user> (or into /root) that the service, running as the login user,
	// can't read — the "logged in fine once, then it stopped working" symptom.
	// True root with no SUDO_USER (e.g. LXC) runs directly, unchanged.
	if os.Geteuid() == 0 {
		if su := os.Getenv("SUDO_USER"); su != "" && su != "root" {
			// `env KEY=VAL …` re-injects the orchestration vars (sudo would
			// otherwise scrub them); -H sets HOME to the target user's home.
			sudoArgs := []string{"-u", su, "-H", "env"}
			sudoArgs = append(sudoArgs, extraEnv...)
			sudoArgs = append(sudoArgs, "/bin/bash")
			c = exec.Command("sudo", append(sudoArgs, bashArgs...)...)
		}
	}
	if c == nil {
		c = exec.Command("/bin/bash", bashArgs...)
		c.Env = append(os.Environ(), extraEnv...)
	}
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// setupAccount runs the install script for account idx (0 = primary, 1 = the
// optional second account) in the given mode (Beeper vs self-hosted).
func setupAccount(beeper bool, idx int) error {
	self := selfPath()
	bbctlPath := filepath.Join(filepath.Dir(self), "bbctl")
	dataDir := cortenDataDir()
	bundleID := cortenBundleID
	// The orchestrator (runSetup) starts every account together AFTER the optional
	// second account is configured, so each script only INSTALLS its service here.
	env := []string{"CORTEN_DEFER_START=1"}
	if idx == 1 {
		dataDir = secondDataDir()
		bundleID = cortenBundleID + "-1"
		env = append(env,
			"BRIDGE_NAME=sh-imessage1",     // Beeper appservice name for the 2nd account
			"SERVICE_NAME=corten-matrix-1", // 2nd account's stop/identity name
			"XDG_DATA_HOME="+dataDir,       // 2nd account's own login/session dir
			"CORTEN_SKIP_SERVICE=1",        // ONE service runs both bridges — don't install a 2nd
		)
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
	// No mid-setup "add a second account?" prompt — a second bridge is added
	// explicitly later with `setup 1` / `setup-beeper 1` (see reconfigureSecond).
	startAfterSetup()
	os.Exit(0)
}

// reconfigureSecond runs setup for ONLY the second account (`setup 1` /
// `setup-beeper 1`). It both ADDS a second bridge later (if none exists yet) and
// RECONFIGURES an existing one (e.g. to flip a toggle like CloudKit backfill),
// then restarts the single service so bridge-all (re)loads both bridges together.
func reconfigureSecond(beeper bool) {
	fmt.Printf("%s═══ Second account (add / reconfigure) ═══%s\n", cAccent, cReset)
	if err := setupAccount(beeper, 1); err != nil {
		os.Exit(exitCodeOf(err))
	}
	_ = serviceCtlOne("restart", serviceLabel(0)) // one service → both bridges (re)load
	fmt.Printf("\n%s✓%s Second bridge ready.\n", cGreen, cReset)
	os.Exit(0)
}

// startAfterSetup prompts once (default Yes) and starts every configured account.
// The install scripts only install each service; starting is centralized here so
// the "add a second account?" prompt is never preceded by a "bridge started".
func startAfterSetup() {
	if isInteractive() {
		fmt.Print("\nStart the bridge now (and automatically at login)? [Y/n]: ")
		var ans string
		fmt.Scanln(&ans)
		switch ans {
		case "n", "N", "no", "No":
			fmt.Printf("\n%s✓%s Installed. Start any time with: corten-matrix start\n", cGreen, cReset)
			return
		}
	}
	startOneNow(0) // one service runs every configured account (bridge-all)
	fmt.Printf("\n%s✓%s Bridge started — view logs with: corten-matrix logs\n", cGreen, cReset)
}

// RunAllBridges is the ExecStart of the ONE service: it runs every configured
// account's bridge — account 0 always, plus the optional second account — each
// logging to its own data dir. If any bridge exits, the rest are signalled and we
// exit non-zero so systemd/launchd restarts the whole service (both) together.
func RunAllBridges() {
	self := selfPath()
	dirs := []string{cortenDataDir()}
	if hasSecondAccount() {
		dirs = append(dirs, secondDataDir())
	}
	var cmds []*exec.Cmd
	for i, d := range dirs {
		cfg := filepath.Join(d, "config.yaml")
		if _, err := os.Stat(cfg); err != nil {
			continue // account not configured yet — skip
		}
		// Beeper accounts run via their generated start.sh (permission-fix + the
		// right flags); a self-hosted account runs the binary against its config.
		var c *exec.Cmd
		if _, err := os.Stat(filepath.Join(d, "start.sh")); err == nil {
			c = exec.Command("/bin/bash", filepath.Join(d, "start.sh"))
		} else {
			c = exec.Command(self, "-c", cfg)
		}
		c.Env = os.Environ()
		if i > 0 {
			c.Env = setEnv(c.Env, "XDG_DATA_HOME", d) // 2nd account's own session dir
		}
		// Run each bridge in its own data dir. The generated config's only
		// relative path — the JSON file writer's ./logs/bridge.log — then
		// resolves to THIS account's logs dir; with bridge-all's inherited cwd
		// (the primary data dir) the 2nd account's JSON log landed in the
		// PRIMARY's bridge.log. DB URIs are absolutized by the install
		// scripts, so nothing else is cwd-sensitive.
		c.Dir = d
		_ = os.MkdirAll(filepath.Join(d, "logs"), 0o755)
		// Capture the child's stdout/stderr — the config's pretty-colored
		// terminal writer plus any crash output — in a separate file, NOT in
		// bridge.log: that file is the JSON file writer's target, and dumping
		// stdout there too duplicated every line and filled the log that
		// `corten-matrix logs` tails with ANSI color codes. Truncate the
		// capture once it gets large; bridge.log is the rotated real log.
		soPath := filepath.Join(d, "logs", "bridge.stdout.log")
		soMode := os.O_CREATE | os.O_WRONLY | os.O_APPEND
		if st, err := os.Stat(soPath); err == nil && st.Size() > 50<<20 {
			soMode |= os.O_TRUNC
		}
		if f, err := os.OpenFile(soPath, soMode, 0o644); err == nil {
			c.Stdout, c.Stderr = f, f
		} else {
			c.Stdout, c.Stderr = os.Stdout, os.Stderr
		}
		if err := c.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "corten-matrix: start account %d: %v\n", i, err)
			continue
		}
		cmds = append(cmds, c)
	}
	if len(cmds) == 0 {
		fmt.Fprintln(os.Stderr, "corten-matrix: no configured account to run (run setup first)")
		os.Exit(1)
	}
	exited := make(chan struct{}, len(cmds))
	for _, c := range cmds {
		go func(c *exec.Cmd) { _ = c.Wait(); exited <- struct{}{} }(c)
	}
	<-exited // first bridge to stop takes the service down → restart as a unit
	for _, c := range cmds {
		if c.Process != nil {
			_ = c.Process.Signal(syscall.SIGTERM)
		}
	}
	os.Exit(1)
}

// setEnv returns env with key set to val (replacing any existing entry).
func setEnv(env []string, key, val string) []string {
	var out []string
	for _, e := range env {
		if !strings.HasPrefix(e, key+"=") {
			out = append(out, e)
		}
	}
	return append(out, key+"="+val)
}

// startOneNow loads (and force-starts) one account's already-installed service.
func startOneNow(idx int) {
	label := serviceLabel(idx)
	if runtime.GOOS == "darwin" {
		uid := strconv.Itoa(os.Getuid())
		plist := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", label+".plist")
		if exec.Command("launchctl", "bootstrap", "gui/"+uid, plist).Run() != nil {
			_ = exec.Command("launchctl", "load", "-w", plist).Run()
		}
		_ = exec.Command("launchctl", "kickstart", "-k", "gui/"+uid+"/"+label).Run()
		return
	}
	_ = sysctl("start", label+".service")
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
  <key>ProgramArguments</key><array><string>%s</string><string>bridge-all</string></array>
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
	// Linux: systemd unit — a user unit normally, a system unit in LXC/containers
	// (no user bus), matching the install scripts' SYSTEMD_MODE fallback.
	_, system := linuxSystemctl()
	dir := filepath.Join(os.Getenv("HOME"), ".config", "systemd", "user")
	wantedBy := "default.target"
	if system {
		dir = "/etc/systemd/system"
		wantedBy = "multi-user.target"
	}
	unit := filepath.Join(dir, "corten-matrix.service")
	body := fmt.Sprintf(`[Unit]
Description=corten-matrix iMessage bridge
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s bridge-all
WorkingDirectory=%s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=%s
`, self, data, wantedBy)
	if system && os.Geteuid() != 0 {
		// system unit needs root: write it via sudo tee.
		_ = exec.Command("sudo", "mkdir", "-p", dir).Run()
		w := exec.Command("sudo", "tee", unit)
		w.Stdin = strings.NewReader(body)
		w.Stderr = os.Stderr
		if err := w.Run(); err != nil {
			die("write systemd unit (sudo): %v", err)
		}
	} else {
		_ = os.MkdirAll(dir, 0o755)
		if err := os.WriteFile(unit, []byte(body), 0o644); err != nil {
			die("write systemd unit: %v", err)
		}
	}
	_ = sysctl("daemon-reload")
	if err := sysctl("enable", "--now", "corten-matrix.service"); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
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
	_, system := linuxSystemctl()
	_ = sysctl("disable", "--now", "corten-matrix.service")
	if system {
		unit := "/etc/systemd/system/corten-matrix.service"
		if os.Geteuid() != 0 {
			_ = exec.Command("sudo", "rm", "-f", unit).Run()
		} else {
			_ = os.Remove(unit)
		}
	} else {
		_ = os.Remove(filepath.Join(os.Getenv("HOME"), ".config", "systemd", "user", "corten-matrix.service"))
	}
	_ = sysctl("daemon-reload")
	fmt.Println("corten-matrix service removed.")
	os.Exit(0)
}

// tailLogs tails a bridge log. `logs` → primary account; `logs 1` → 2nd account
// (the corten-matrix-1 account — matching `setup 1`).
func tailLogs(args []string) {
	dir := cortenDataDir()
	if len(args) > 0 && args[0] == "1" {
		dir = secondDataDir()
	}
	c := exec.Command("tail", "-F", filepath.Join(dir, "logs", "bridge.log"))
	c.Stdin = os.Stdin
	c.Stderr = os.Stderr
	pipe, err := c.StdoutPipe()
	if err != nil {
		die("tail: %v", err)
	}
	if err := c.Start(); err != nil {
		die("tail: %v", err)
	}
	prettyTail(pipe, os.Stdout, cReset == "") // cReset empty ⇒ non-TTY/NO_COLOR
	if err := c.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		os.Exit(1)
	}
	os.Exit(0)
}

// prettyTail renders a bridge log stream for the terminal: JSON log lines
// (bridge.log is the config's JSON file writer's target) are rendered through
// zerolog's console writer — the same pretty format a foreground bridge run
// prints — and anything else (older pretty/colored lines written before
// RunAllBridges kept stdout out of bridge.log) passes through unchanged.
func prettyTail(r io.Reader, w io.Writer, noColor bool) {
	cw := zerolog.ConsoleWriter{
		Out:     w,
		NoColor: noColor,
		// zeroconfig's default pretty timestamp format, so the output matches
		// the bridge's own pretty writer.
		TimeFormat: "2006-01-02T15:04:05.999Z07:00",
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) > 0 && line[0] == '{' {
			if _, err := cw.Write(line); err == nil {
				continue
			}
		}
		_, _ = w.Write(append(line, '\n'))
	}
	// Scanner stopped early (line over 1MB, read error): raw passthrough.
	_, _ = io.Copy(w, r)
}

func runBbctl(args []string) {
	// bbctl is compiled into this binary (pkg/bbctl) — run it in-process,
	// no separate bbctl binary to ship or locate.
	bbctl.Run(append([]string{"bbctl"}, args...))
	os.Exit(0)
}

// ExtraHelpRows holds extra {command, description} rows contributed by the
// build configuration's host-command extensions (none in the base build). Set
// from main before any help is printed; appended to the listing by PrintHelp.
var ExtraHelpRows [][2]string

// printHelp shows the user-facing command list (clean, accent-colored).
func PrintHelp() {
	hdr := cBold + cAccent + "◆ corten-matrix" + cReset
	fmt.Println()
	fmt.Printf("  %s  %sMatrix ↔ iMessage bridge%s\n\n", hdr, cDim, cReset)
	fmt.Printf("  %sUsage:%s corten-matrix <command>\n\n", cDim, cReset)
	rows := [][2]string{
		{"setup", "configure & start (re-run to flip a toggle, e.g. backfill)"},
		{"setup-beeper", "configure for Beeper (re-run to reconfigure)"},
		{"setup 1", "add / reconfigure the SECOND account"},
		{"setup-beeper 1", "add / reconfigure the SECOND account (Beeper)"},
		{"start", "start the bridge"},
		{"stop", "stop the bridge"},
		{"restart", "restart the bridge"},
		{"status", "show service status"},
		{"logs 1", "tail a bridge log (1 = second account)"},
		{"install-service", "install + start the background service"},
		{"uninstall-service", "stop + remove the background service"},
		{"reset", "reset bridge state"},
		{"uninstall", "remove the service"},
		{"login", "re-run the iMessage login flow"},
		{"bbctl <args>", "Beeper bridge-manager CLI"},
		{"help", "show this help"},
	}
	rows = append(rows, ExtraHelpRows...)
	for _, r := range rows {
		fmt.Printf("    %s%-14s%s %s%s%s\n", cAccent, r[0], cReset, cDim, r[1], cReset)
	}
	fmt.Println()
	fmt.Printf("  %sOne service runs every configured account. Re-run a setup command to flip a%s\n", cDim, cReset)
	fmt.Printf("  %stoggle (backfill, contacts…); add '1' to reconfigure the second account.%s\n\n", cDim, cReset)
	if hasSecondAccount() {
		fmt.Printf("  %sTwo accounts configured — start/stop/restart act on the one service; 'logs 1' = second.%s\n\n", cDim, cReset)
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
		if len(args) > 0 && args[0] == "1" {
			reconfigureSecond(false)
		}
		runSetup(false)
	case "setup-beeper":
		if len(args) > 0 && args[0] == "1" {
			reconfigureSecond(true)
		}
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
