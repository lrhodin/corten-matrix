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
			"Service/Restart":     "always",
			"Service/LimitNOFILE": "65536",
		} {
			if got := f[k]; got != want {
				t.Errorf("%s = %q, want %q (the install scripts set it)", k, got, want)
			}
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

	owner, xdg := existingUnitIdentity(path)
	if owner != "david" {
		t.Errorf("owner = %q, want david — a re-install under `su -` would otherwise rewrite this to root", owner)
	}
	if xdg != "/home/david/.local/share" {
		t.Errorf("xdg = %q, want /home/david/.local/share", xdg)
	}

	// Absent file: derive rather than blow up.
	owner, xdg = existingUnitIdentity(filepath.Join(dir, "nope.service"))
	if owner != "" || xdg != "" {
		t.Errorf("missing unit returned (%q, %q), want empty so the caller derives", owner, xdg)
	}

	// A unit without the fields must also report empty, not a partial parse.
	bare := filepath.Join(dir, "bare.service")
	if err := os.WriteFile(bare, []byte("[Service]\nExecStart=/bin/true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if owner, xdg = existingUnitIdentity(bare); owner != "" || xdg != "" {
		t.Errorf("bare unit returned (%q, %q), want empty", owner, xdg)
	}
}

// TestUnitBodyRoundTripsThroughItsOwnParser is the property that matters for an
// overwrite: rendering a unit and reading it back must return the same identity,
// so repeated `install-service` runs converge instead of drifting.
func TestUnitBodyRoundTripsThroughItsOwnParser(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corten-matrix.service")
	const owner, xdg = "david", "/home/david/.local/share"

	body := systemdUnitBody(true, owner, xdg, "/opt/corten/corten-matrix", xdg+"/corten-matrix", "multi-user.target")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gotOwner, gotXDG := existingUnitIdentity(path)
	if gotOwner != owner || gotXDG != xdg {
		t.Fatalf("round trip = (%q, %q), want (%q, %q)", gotOwner, gotXDG, owner, xdg)
	}

	// Second pass must be byte-identical — an overwrite that drifts is how a
	// working install decays across re-installs.
	again := systemdUnitBody(true, gotOwner, gotXDG, "/opt/corten/corten-matrix", xdg+"/corten-matrix", "multi-user.target")
	if again != body {
		t.Error("second render differs from the first; repeated install-service runs would drift")
	}
}
