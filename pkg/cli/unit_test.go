package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The unit body is the one thing in this package that cannot be exercised
// without a systemd machine, and it is the one whose mistakes are silent: a
// unit that fails to find its config still returns 0 from `enable --now`,
// because Type=simple reports success as soon as the fork succeeds. So it is
// factored into a pure function and pinned here.
//
// Every field asserted below has been wrong in a shipped version of this code
// at least once. Treat a failure as "the overwrite will break someone's
// install," not as "the string changed."

// fields parses a rendered unit into "Section/Key" -> value.
func fields(t *testing.T, body string) map[string]string {
	t.Helper()
	out := map[string]string{}
	section := ""
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = strings.Trim(line, "[]")
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("unparseable unit line %q", line)
		}
		// Environment= is keyed by its variable so two Environment lines
		// would not silently collapse.
		if k == "Environment" {
			ek, ev, _ := strings.Cut(v, "=")
			out[section+"/Environment:"+ek] = ev
			continue
		}
		out[section+"/"+k] = v
	}
	return out
}

// TestSystemdUnitBodyMatchesInstallScripts pins the fields against what
// install_systemd_user / install_systemd_system write in scripts/install-linux.sh.
// `install-service` overwrites units those scripts authored, so any field they
// set and this does not is silently dropped from a working install.
func TestSystemdUnitBodyMatchesInstallScripts(t *testing.T) {
	t.Run("system scope pins identity and data dir", func(t *testing.T) {
		f := fields(t, systemdUnitBody(true, "david", "/home/david/.local/share",
			"/opt/corten/corten-matrix", "/home/david/.local/share/corten-matrix", "multi-user.target"))

		if got := f["Service/User"]; got != "david" {
			t.Errorf("User = %q, want david — without it a system unit runs as root and cannot find config.yaml", got)
		}
		if got := f["Service/Environment:XDG_DATA_HOME"]; got != "/home/david/.local/share" {
			t.Errorf("XDG_DATA_HOME = %q, want /home/david/.local/share", got)
		}
		if got := f["Install/WantedBy"]; got != "multi-user.target" {
			t.Errorf("WantedBy = %q, want multi-user.target", got)
		}
		for k, want := range map[string]string{
			"Service/Restart":          "always",
			"Service/RestartSec":       "5",
			"Service/LimitNOFILE":      "65536",
			"Service/WorkingDirectory": "/home/david/.local/share/corten-matrix",
			"Service/ExecStart":        "/opt/corten/corten-matrix bridge-all",
			"Unit/Description":         "corten-matrix iMessage bridge",
		} {
			if got := f[k]; got != want {
				t.Errorf("%s = %q, want %q (the install scripts set it)", k, got, want)
			}
		}
	})

	// WorkingDirectory has to agree with User= and XDG_DATA_HOME=. systemd
	// chdir()s AFTER switching credentials, and a directory the unit's user
	// cannot traverse is a fatal 200/CHDIR — Restart=always then just loops to
	// the start limit. This field had no assertion at all, which is how a
	// version shipped that preserved the identity and derived the directory.
	t.Run("working directory agrees with the pinned data dir", func(t *testing.T) {
		const xdg = "/home/corten/.local/share"
		f := fields(t, systemdUnitBody(true, "corten", xdg,
			"/opt/corten/corten-matrix", filepath.Join(xdg, "corten-matrix"), "multi-user.target"))

		wd := f["Service/WorkingDirectory"]
		env := f["Service/Environment:XDG_DATA_HOME"]
		if filepath.Dir(wd) != env {
			t.Errorf("WorkingDirectory %q is not inside XDG_DATA_HOME %q — the unit's user "+
				"will fail to chdir into another user's home (fatal 200/CHDIR)", wd, env)
		}
		if got := f["Service/User"]; got != "corten" {
			t.Errorf("User = %q, want corten", got)
		}
	})

	// The regression that shipped: Environment= was emitted only for system
	// scope, but the scripts set it on the USER unit too. Overwriting a user
	// unit therefore dropped a custom XDG_DATA_HOME and pointed the bridge at
	// a directory with no config.yaml.
	t.Run("user scope still pins the data dir", func(t *testing.T) {
		f := fields(t, systemdUnitBody(false, "david", "/data/xdg",
			"/opt/corten/corten-matrix", "/data/xdg/corten-matrix", "default.target"))

		if got, ok := f["Service/Environment:XDG_DATA_HOME"]; !ok || got != "/data/xdg" {
			t.Errorf("XDG_DATA_HOME = %q (present=%v), want /data/xdg — the scripts set this on user units too", got, ok)
		}
		if _, ok := f["Service/User"]; ok {
			t.Error("User= must not appear in a user unit; systemd rejects it")
		}
		if got := f["Install/WantedBy"]; got != "default.target" {
			t.Errorf("WantedBy = %q, want default.target", got)
		}
	})

	// Omitting User= is NOT the same as User=root: per systemd.exec's
	// SetLoginEnvironment=, $HOME/$LOGNAME/$SHELL are set by default only when
	// User= is present. With $HOME unset, os.UserConfigDir() errors and
	// pkg/bbctl discards that into a relative path.
	t.Run("root is written explicitly, not omitted", func(t *testing.T) {
		f := fields(t, systemdUnitBody(true, "root", "/root/.local/share",
			"/opt/corten/corten-matrix", "/root/.local/share/corten-matrix", "multi-user.target"))
		if got, ok := f["Service/User"]; !ok || got != "root" {
			t.Errorf("User = %q (present=%v), want an explicit root", got, ok)
		}
	})
}

// TestSystemdUnitBodyIsWellFormed checks the rendered file actually parses as a
// unit in every branch — the identity block is spliced in ahead of ExecStart,
// so a missing newline would silently produce "User=davidExecStart=...".
func TestSystemdUnitBodyIsWellFormed(t *testing.T) {
	for _, tc := range []struct {
		name         string
		system       bool
		owner, xdg   string
		wantSections []string
	}{
		{"system with identity", true, "david", "/home/david/.local/share", []string{"Unit", "Service", "Install"}},
		{"user with xdg only", false, "david", "/data/xdg", []string{"Unit", "Service", "Install"}},
		{"nothing to pin", false, "", "", []string{"Unit", "Service", "Install"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := systemdUnitBody(tc.system, tc.owner, tc.xdg, "/bin/corten", "/data", "default.target")
			var seen []string
			for _, line := range strings.Split(body, "\n") {
				line = strings.TrimSpace(line)
				switch {
				case line == "":
				case strings.HasPrefix(line, "["):
					seen = append(seen, strings.Trim(line, "[]"))
				case !strings.Contains(line, "="):
					t.Errorf("line is neither a section nor key=value: %q", line)
				case strings.HasPrefix(line, "="):
					t.Errorf("line has an empty key: %q", line)
				}
			}
			if strings.Join(seen, ",") != strings.Join(tc.wantSections, ",") {
				t.Errorf("sections = %v, want %v", seen, tc.wantSections)
			}
			if !strings.Contains(body, "\nExecStart=") {
				t.Error("ExecStart is not at the start of its own line — the identity splice ate the newline")
			}
			if !strings.HasSuffix(body, "\n") {
				t.Error("unit file must end with a newline")
			}
		})
	}
}

// TestExistingUnitIdentityIsPreserved covers the reason overwrite stopped
// re-deriving: the process running `install-service` is not necessarily the one
// the unit runs as (`su -` being the clearest case), so an overwrite that
// guesses from the ambient environment rewrites a working unit to point at the
// wrong user and the wrong data directory.
func TestExistingUnitIdentityIsPreserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corten-matrix.service")
	if err := os.WriteFile(path, []byte(`[Unit]
Description=corten-matrix iMessage bridge

[Service]
User=david
Environment=XDG_DATA_HOME=/home/david/.local/share
ExecStart=/opt/corten/corten-matrix bridge-all

[Install]
WantedBy=multi-user.target
`), 0o644); err != nil {
		t.Fatal(err)
	}

	owner, xdg, found := existingUnitIdentity(path)
	if !found {
		t.Fatal("found = false for a unit that exists")
	}
	if owner != "david" {
		t.Errorf("owner = %q, want david — a re-install under `su -` would otherwise rewrite this to root", owner)
	}
	if xdg != "/home/david/.local/share" {
		t.Errorf("xdg = %q, want /home/david/.local/share", xdg)
	}

	// Absent file: `found` false, so the caller derives.
	if owner, xdg, found = existingUnitIdentity(filepath.Join(dir, "nope.service")); found {
		t.Errorf("missing unit reported found=true (%q, %q)", owner, xdg)
	}

	// A unit that EXISTS but sets no User= must report found=true with an empty
	// owner. That absence is meaningful — a system unit without User= runs as
	// root deliberately — and deriving one would silently re-point the service
	// at whoever ran install-service.
	bare := filepath.Join(dir, "bare.service")
	if err := os.WriteFile(bare, []byte("[Service]\nExecStart=/bin/true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	owner, xdg, found = existingUnitIdentity(bare)
	if !found {
		t.Error("found = false for a unit that exists but sets no identity fields")
	}
	if owner != "" || xdg != "" {
		t.Errorf("bare unit returned (%q, %q), want empty", owner, xdg)
	}
}

// TestExistingUnitIdentityToleratesRealUnitSpellings covers forms systemd
// accepts and this parser previously returned "" for — each of which used to
// mean "derive and overwrite", i.e. silently repoint a working service.
func TestExistingUnitIdentityToleratesRealUnitSpellings(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantOwner, wantXDG string
	}{
		{"spaces around =", "[Service]\nUser = david\nEnvironment = XDG_DATA_HOME=/srv\n", "david", "/srv"},
		{"double-quoted environment", "[Service]\nUser=david\nEnvironment=\"XDG_DATA_HOME=/srv\"\n", "david", "/srv"},
		{"single-quoted environment", "[Service]\nUser=david\nEnvironment='XDG_DATA_HOME=/srv'\n", "david", "/srv"},
		{"tab indented", "[Service]\n\tUser=david\n\tEnvironment=XDG_DATA_HOME=/srv\n", "david", "/srv"},
		{"comments ignored", "[Service]\n#User=root\n;User=nobody\nUser=david\n", "david", ""},
		{"last one wins", "[Service]\nUser=first\nUser=david\n", "david", ""},
		{"crlf", "[Service]\r\nUser=david\r\nEnvironment=XDG_DATA_HOME=/srv\r\n", "david", "/srv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "u.service")
			if err := os.WriteFile(p, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			owner, xdg, found := existingUnitIdentity(p)
			if !found {
				t.Fatal("found = false")
			}
			if owner != tc.wantOwner {
				t.Errorf("owner = %q, want %q", owner, tc.wantOwner)
			}
			if xdg != tc.wantXDG {
				t.Errorf("xdg = %q, want %q", xdg, tc.wantXDG)
			}
		})
	}
}

// TestResolveUnitIdentity covers the DECISION rather than the rendering. Every
// unit-body defect so far has lived here, and mutation-testing showed the
// renderer's tests could not see it: they were handed an already-consistent
// data dir, so removing the line that makes it consistent changed nothing.
func TestResolveUnitIdentity(t *testing.T) {
	const fallback = "/home/david/.local/share"

	t.Run("preserves an existing unit's identity", func(t *testing.T) {
		owner, xdg, data := resolveUnitIdentity(true, "corten", "/home/corten/.local/share", true,
			"david", "david", fallback)
		if owner != "corten" {
			t.Errorf("owner = %q, want corten (preserved, not the caller)", owner)
		}
		if xdg != "/home/corten/.local/share" {
			t.Errorf("xdg = %q, want the preserved value", xdg)
		}
		if data != "/home/corten/.local/share/corten-matrix" {
			t.Errorf("data = %q — it must follow the preserved xdg, or the unit's user cannot chdir into it", data)
		}
	})

	// The `su -` case named in the commit message: caller resolves to root,
	// the unit belongs to david. Deriving would repoint it; worse, deriving
	// only the data dir would leave User=david chdir'ing into /root.
	t.Run("su - does not repoint an existing unit", func(t *testing.T) {
		owner, xdg, data := resolveUnitIdentity(true, "david", fallback, true,
			"", "root", "/root/.local/share")
		if owner != "david" || xdg != fallback {
			t.Fatalf("got (%q, %q), want (david, %q)", owner, xdg, fallback)
		}
		if data != fallback+"/corten-matrix" {
			t.Errorf("data = %q, want it under %q — /root is 0700, chdir would fail fatally", data, fallback)
		}
	})

	// A system unit that sets no User= runs as root on purpose. `found` is what
	// decides preserve-vs-derive; keying on "owner is empty" would silently
	// hand the service to whoever ran install-service.
	t.Run("absent User= is preserved, not derived", func(t *testing.T) {
		owner, _, _ := resolveUnitIdentity(true, "", "/var/lib/corten", true, "david", "david", fallback)
		if owner != "" {
			t.Errorf("owner = %q, want empty — the unit deliberately has no User=", owner)
		}
	})

	t.Run("fresh install derives from sudo caller", func(t *testing.T) {
		owner, xdg, data := resolveUnitIdentity(true, "", "", false, "david", "root", fallback)
		if owner != "david" {
			t.Errorf("owner = %q, want the sudo caller", owner)
		}
		if xdg != fallback || data != fallback+"/corten-matrix" {
			t.Errorf("got (%q, %q), want (%q, %q/corten-matrix)", xdg, data, fallback, fallback)
		}
	})

	// user.Current() fails for a uid with no passwd entry. Emitting no User=
	// at all is not the same as root: per systemd.exec's SetLoginEnvironment=,
	// $HOME is then unset.
	t.Run("system fresh install never yields an empty owner", func(t *testing.T) {
		if owner, _, _ := resolveUnitIdentity(true, "", "", false, "", "", fallback); owner != "root" {
			t.Errorf("owner = %q, want an explicit root", owner)
		}
	})

	// The invariant that ties the three together, across every combination.
	t.Run("data dir is always inside xdg", func(t *testing.T) {
		for _, tc := range []struct {
			system                        bool
			pOwner, pXDG                  string
			found                         bool
			sudoUser, current, fallbackIn string
		}{
			{true, "corten", "/srv/corten", true, "david", "david", fallback},
			{true, "", "", true, "david", "david", fallback},
			{false, "", "/data/xdg", true, "", "david", fallback},
			{true, "", "", false, "", "", fallback},
			{false, "", "", false, "david", "david", fallback},
		} {
			_, xdg, data := resolveUnitIdentity(tc.system, tc.pOwner, tc.pXDG, tc.found,
				tc.sudoUser, tc.current, tc.fallbackIn)
			if filepath.Dir(data) != xdg {
				t.Errorf("data %q is not inside xdg %q (case %+v)", data, xdg, tc)
			}
			if xdg == "" {
				t.Errorf("xdg is empty (case %+v) — the unit would pin nothing", tc)
			}
		}
	})
}

// TestScriptWrittenUnitSurvivesAnOverwrite is the end-to-end property: take the
// unit an install script actually writes, run it through preserve-then-render
// the way serviceInstall does, and confirm nothing the script pinned is lost or
// changed. This is the check that would have caught every unit-body defect so
// far — dropped Environment=, omitted User=root, missing LimitNOFILE, and a
// WorkingDirectory that disagreed with the preserved identity.
//
// The previous version of this test round-tripped the Go renderer through the
// Go parser and asserted a pure function returns the same thing twice, which is
// true by construction and constrained nothing.
func TestScriptWrittenUnitSurvivesAnOverwrite(t *testing.T) {
	// Verbatim shape of install_systemd_system in scripts/install-linux.sh,
	// for a box whose bridge runs as a dedicated account.
	const scriptUnit = `[Unit]
Description=corten-matrix iMessage bridge
After=network.target

[Service]
Type=simple
User=corten
WorkingDirectory=/opt/corten
Environment=XDG_DATA_HOME=/home/corten/.local/share
ExecStart=/opt/corten/corten-matrix bridge-all
Restart=always
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
`
	path := filepath.Join(t.TempDir(), "corten-matrix.service")
	if err := os.WriteFile(path, []byte(scriptUnit), 0o644); err != nil {
		t.Fatal(err)
	}

	owner, xdg, found := existingUnitIdentity(path)
	if !found {
		t.Fatal("script-written unit not detected; the overwrite would derive and repoint it")
	}
	// serviceInstall derives the data dir from the resolved xdg precisely so it
	// cannot disagree with the preserved User=.
	data := filepath.Join(xdg, "corten-matrix")
	got := fields(t, systemdUnitBody(true, owner, xdg, "/opt/corten/corten-matrix", data, "multi-user.target"))
	want := fields(t, scriptUnit)

	for _, k := range []string{
		"Service/User",
		"Service/Environment:XDG_DATA_HOME",
		"Service/ExecStart",
		"Service/Restart",
		"Service/RestartSec",
		"Service/LimitNOFILE",
		"Install/WantedBy",
	} {
		if got[k] != want[k] {
			t.Errorf("overwrite changed %s: %q -> %q", k, want[k], got[k])
		}
	}
	// WorkingDirectory legitimately differs (the script pins the binary dir,
	// this pins the data dir) — but it must remain traversable by the preserved
	// user, i.e. inside their XDG_DATA_HOME.
	if filepath.Dir(got["Service/WorkingDirectory"]) != got["Service/Environment:XDG_DATA_HOME"] {
		t.Errorf("WorkingDirectory %q is outside the preserved XDG_DATA_HOME %q — user %q cannot chdir there",
			got["Service/WorkingDirectory"], got["Service/Environment:XDG_DATA_HOME"], got["Service/User"])
	}
}
