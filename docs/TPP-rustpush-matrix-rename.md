# TPP: Rename `mautrix-imessage` → `rustpush-matrix` *(working name)*

## Status
- **Phase**: Scope/design (no code changes yet, no implementation in this branch)
- **Branch**: `rustpush-matrix`
- **Working name**: `rustpush-matrix` is provisional. The branch and this TPP use it as a placeholder so we can scope the work; the final brand can be substituted with a single global change before merge. Every "to" cell in the tables below and every literal in the migration scripts is one find-and-replace away from a different name. **Pick the final name before implementation lands**, not after.
- **Goal**: Drop the `mautrix` brand from every artifact users touch (binary, app bundle, data directory, service unit) without breaking existing installs. Migrate consenting users in-place; leave non-consenting users on the old install untouched.

---

## Decision: should we do this at all?

**This document is also a sanity check on whether the rename is worth doing.** Read this section first and decide what scope you're committing to before reading the implementation details below.

### Cost
- **~207 string occurrences across 49 files** (full breakdown in the Surface Area table below). Most are mechanical, but every install script and every Go path constant has to be reviewed individually.
- **A consent-prompted in-place migration** for every existing install (Linux systemd unit move, macOS launchd plist swap, data-dir rename, DB filename rename, shell-rc alias marker cleanup, sed-platform divergence between Linux and macOS).
- **A bridge-side safety net** (a small but load-bearing Go check that exits fatally if a user's binary upgraded out-of-band but their data dir didn't move). Without it, an out-of-band upgrade silently re-pairs with Beeper and looks like data loss.
- **macOS TCC re-prompt** for Contacts (and Full Disk Access if used) on the new bundle ID — unavoidable, has to be in release notes.
- **Name approval risk.** `rustpush-matrix` is derived from `rustpush` (the OB/upstream Rust library that powers this bridge). Using it for our binary may be confusing (suggests the rustpush authors maintain a Matrix bridge under that name) or stepping on toes. Worth confirming with the rustpush maintainers before committing — and that's true for any name derived from `rustpush`. A name that's clearly *ours* (not derived from a vendored dep) sidesteps this entirely.
- **All of the above is one-time per existing install** — every dogfooder hits the prompt once, then never again. Fresh installs see nothing.

### Benefit
- "mautrix" brand disappears from binary name, data dir, systemd/launchd unit, app bundle.
- These are **sysadmin-visible** surfaces — not user-visible. A regular user sees `DisplayName: "iMessage"` in their Matrix client (which is already not "mautrix").
- The mautrix-imessage upstream project is not the same as this fork, so the name is technically misleading. That's the substantive case for renaming.

### Off-ramps (cheapest to most invasive)

1. **Do nothing.** `DisplayName` is already "iMessage". The "mautrix" brand only appears in places only sysadmins ever look (binary name, data dir, systemd unit). Cost: zero. Benefit: zero brand cleanup, but accept that the naming is a non-issue for the 99% of users who never invoke `systemctl status` or look in `~/.local/share`.

2. **Update README and user-visible docs only** to clarify that this fork's relationship to upstream is "based on / forked from / divergent from" mautrix-imessage. Costs ~1 file change. The binary keeps the upstream-aligned name (which is honest — it's a fork) and no migration is needed.

3. **Rename only the cosmetic surfaces** — README, the Go file-header boilerplate (~38 files via one sed), the OGG vendor string. Skips binary, data dir, install scripts, all migration logic. Cost: ~1 PR, no migration. Benefit: brand cleanup in source-tree-visible places without disturbing any installed user.

4. **Full rename (this TPP).** Everything in the table below. Cost: this whole document. Benefit: clean break from the upstream brand at every layer.

**Recommendation:** if dropping "mautrix" is more aspirational than urgent, **option 2 or 3 is probably the right scope.** Option 4 is correct only if there's a stronger driver than brand-cleanliness (e.g. a clear product identity decision, distribution under a new name, etc.) AND the final name has been approved (see name-approval risk above). The migration is doable and self-extinguishing — but the cost-vs-benefit ratio gets thin once you tally the install-script work and the macOS TCC re-prompt.

---

## Surface area (where the old name appears)

Total: **~207 string occurrences across 49 files**, of which ~38 are cosmetic Go file-header comments.

| File | Occurrences | Nature |
|---|---:|---|
| **Build / packaging** | | |
| `Makefile` | 4 | `APP_NAME`, `CMD_PKG`, `BUNDLE_ID`, `DATA_DIR` defaults |
| `Info.plist` | 4 | `CFBundleIdentifier`, `CFBundleName`, `CFBundleExecutable`, `NSContactsUsageDescription` |
| `.github/workflows/release.yml` | 8 | tarball names, artifact paths |
| `cmd/mautrix-imessage/` (directory) | 1 | rename to `cmd/rustpush-matrix/` |
| **Runtime — Go (path-bearing, NOT cosmetic)** | | |
| `pkg/connector/identity_store.go` | 8 | 3 `filepath.Join(dataDir, "mautrix-imessage", ...)` calls + 4 doc-comment paths + 1 file header |
| `pkg/connector/carddav_crypto.go` | 2 | 1 `filepath.Join` call + 1 file header |
| `pkg/connector/audioconvert.go` | 2 | OGG `vendor` string (cosmetic) + 1 file header |
| `pkg/connector/example-config.yaml` | 1 | binary-name in a setup comment |
| **Tests** | | |
| `pkg/connector/identity_store` (covered above) | — | |
| `pkg/connector/audioconvert_test.go` | 2 | vendor-string assertion |
| `pkg/connector/carddav_crypto_test.go` | 5 | path-segment assertions (lines 64, 113, 165) |
| **Install scripts** | | |
| `scripts/install.sh` (macOS, self-hosted) | 8 | paths, plist, bundle id |
| `scripts/install-linux.sh` (Linux, self-hosted) | 36 | paths, systemd unit, db filename, marker comments |
| `scripts/install-beeper.sh` (macOS, Beeper) | 9 | paths, plist, bundle id |
| `scripts/install-beeper-linux.sh` (Linux, Beeper) | 51 | paths, systemd unit, db filename, marker comments |
| `scripts/reset-bridge.sh` | 6 | state dir, bundle id, systemd unit, pgrep pattern, journalctl unit |
| **Docs** | | |
| `README.md` | 23 | title, all path/command/launchctl examples |
| **Go file headers** *(cosmetic, bulk-sed)* | | |
| ~38 additional `.go` files | 38 | `// mautrix-imessage - A Matrix-iMessage puppeting bridge.` boilerplate at top of file |

The four install scripts dominate the line count (104 occurrences of 207). Most of those are repeated path/unit references — once the script is parameterized via `OLD_*`/`NEW_*` variables in the migration block, the actual diff is much smaller than the raw count suggests.

**Counts include `mautrix-imessage`, `mautrix-imessage-v2`, and `com.lrhodin.mautrix-imessage` together** (any string containing `mautrix-imessage`).

---

## Decisions (load-bearing — read before changing the plan)

### What changes
| Surface | From | To |
|---|---|---|
| Binary name | `mautrix-imessage-v2` | `rustpush-matrix` |
| Cmd directory | `cmd/mautrix-imessage/` | `cmd/rustpush-matrix/` |
| macOS bundle ID | `com.lrhodin.mautrix-imessage` | `com.lrhodin.rustpush-matrix` |
| macOS app bundle | `mautrix-imessage-v2.app` | `rustpush-matrix.app` |
| macOS launchd plist | `~/Library/LaunchAgents/com.lrhodin.mautrix-imessage.plist` | `…/com.lrhodin.rustpush-matrix.plist` |
| Linux systemd unit | `mautrix-imessage.service` | `rustpush-matrix.service` |
| Data dir | `~/.local/share/mautrix-imessage/` | `~/.local/share/rustpush-matrix/` |
| SQLite filename | `mautrix-imessage.db` | `rustpush-matrix.db` |
| Release tarballs | `mautrix-imessage-v2-{os}-{arch}.tar.gz` | `rustpush-matrix-{os}-{arch}.tar.gz` |

### What does NOT change (and why)
| Identifier | Why it stays |
|---|---|
| `BeeperBridgeType: "imessagego"` | Beeper-side identifier. Changing orphans every Beeper-hosted bridge. |
| `NetworkID: "imessage"` | Used as portal/ghost ID prefix. Changing rewrites every room and breaks every existing portal mapping. |
| `DisplayName: "iMessage"` | Already the user-visible network name. Has nothing to do with the `mautrix` brand. |
| Go module path `github.com/lrhodin/imessage` | Not the binary name. Repo rename (separate effort) handles import-path updates if it ever happens. |
| OGG `vendor` string in `audioconvert.go` | Embedded in audio file metadata for outbound voice notes. Cosmetic; can be updated for consistency but not blocking. |

---

## Migration UX

Single consent prompt, near the top of every install script, before any destructive action:

```
═══════════════════════════════════════════════════════════════
  This bridge has been renamed from mautrix-imessage to
  rustpush-matrix.

  Your installation will be migrated:
    • Service unit:  mautrix-imessage  →  rustpush-matrix
    • Data dir:      ~/.local/share/mautrix-imessage/
                  →  ~/.local/share/rustpush-matrix/
    • Database file: mautrix-imessage.db → rustpush-matrix.db
    • Old binary and app bundle will be removed.

  Your messages, login session, and configuration are preserved.

  Continue? [y/N]
═══════════════════════════════════════════════════════════════
```

- **Default = N.** Hitting enter aborts.
- **No** → `exit 0`, print "Upgrade cancelled. Your existing install is unchanged. Re-run when ready." Existing install keeps running.
- **Yes** → run the migration block, then proceed with normal install of the new-named version.
- **Non-TTY** (`[ ! -t 0 ]`, e.g. piped from curl, CI) → do **not** auto-migrate. Print the rename notice and the manual migration commands. `exit 1`.

The block is **self-extinguishing**: every conditional is "does the old thing exist?" Once migrated, all checks fail, the block becomes a no-op for that user. Fresh installers never see the prompt.

---

## Migration logic

### Linux (`install-beeper-linux.sh`, `install-linux.sh`)

```bash
OLD_DATA_DIR="$HOME/.local/share/mautrix-imessage"
NEW_DATA_DIR="$HOME/.local/share/rustpush-matrix"
OLD_UNIT="mautrix-imessage"
NEW_UNIT="rustpush-matrix"

needs_migration=false
[ -d "$OLD_DATA_DIR" ] && needs_migration=true
systemctl --user list-unit-files "${OLD_UNIT}.service" 2>/dev/null \
    | grep -q "$OLD_UNIT" && needs_migration=true
systemctl list-unit-files "${OLD_UNIT}.service" 2>/dev/null \
    | grep -q "$OLD_UNIT" && needs_migration=true

if $needs_migration; then
    if [ ! -t 0 ]; then
        cat <<EOF
This installation needs to be migrated from mautrix-imessage to rustpush-matrix.
Migration requires interactive consent. Re-run this script in a terminal,
or perform the migration manually:

  systemctl --user stop mautrix-imessage
  systemctl --user disable mautrix-imessage
  rm -f ~/.config/systemd/user/mautrix-imessage.service
  mv $OLD_DATA_DIR $NEW_DATA_DIR
  mv $NEW_DATA_DIR/mautrix-imessage.db    $NEW_DATA_DIR/rustpush-matrix.db
  mv $NEW_DATA_DIR/mautrix-imessage.db-wal $NEW_DATA_DIR/rustpush-matrix.db-wal 2>/dev/null
  mv $NEW_DATA_DIR/mautrix-imessage.db-shm $NEW_DATA_DIR/rustpush-matrix.db-shm 2>/dev/null
  sed -i 's|mautrix-imessage|rustpush-matrix|g' $NEW_DATA_DIR/config.yaml
EOF
        exit 1
    fi

    show_migration_banner
    read -p "Continue? [y/N]: " RESP
    case "$RESP" in
        [yY]|[yY][eE][sS]) ;;
        *) echo "Upgrade cancelled. Existing install is unchanged."; exit 0 ;;
    esac

    # Stop + disable + remove old unit (user scope first)
    if systemctl --user is-active "$OLD_UNIT" >/dev/null 2>&1; then
        systemctl --user stop "$OLD_UNIT"
        systemctl --user disable "$OLD_UNIT" 2>/dev/null || true
    fi
    rm -f "$HOME/.config/systemd/user/${OLD_UNIT}.service"
    systemctl --user daemon-reload 2>/dev/null || true

    # System-scope unit needs sudo
    if systemctl is-active "$OLD_UNIT" >/dev/null 2>&1; then
        echo "Old system-scope unit detected. The following commands need sudo:"
        echo "  sudo systemctl stop $OLD_UNIT"
        echo "  sudo systemctl disable $OLD_UNIT"
        echo "  sudo rm /etc/systemd/system/${OLD_UNIT}.service"
        echo "  sudo systemctl daemon-reload"
        read -p "Run them now? [y/N]: " SUDO_RESP
        case "$SUDO_RESP" in
            [yY]|[yY][eE][sS])
                sudo systemctl stop "$OLD_UNIT"
                sudo systemctl disable "$OLD_UNIT"
                sudo rm -f "/etc/systemd/system/${OLD_UNIT}.service"
                sudo systemctl daemon-reload
                ;;
            *) echo "Migration aborted. Run them yourself, then re-run this script."; exit 1 ;;
        esac
    fi

    # Move data dir (refuse to overwrite if NEW already exists)
    if [ -d "$OLD_DATA_DIR" ] && [ ! -d "$NEW_DATA_DIR" ]; then
        mv "$OLD_DATA_DIR" "$NEW_DATA_DIR"
        for ext in "" "-wal" "-shm"; do
            [ -f "$NEW_DATA_DIR/mautrix-imessage.db$ext" ] && \
                mv "$NEW_DATA_DIR/mautrix-imessage.db$ext" "$NEW_DATA_DIR/rustpush-matrix.db$ext"
        done
        # NARROW patterns only — global mautrix-imessage→rustpush-matrix
        # would clobber comments and any incidental occurrence in the user's config.
        if [ -f "$NEW_DATA_DIR/config.yaml" ]; then
            sed -i 's|share/mautrix-imessage|share/rustpush-matrix|g' "$NEW_DATA_DIR/config.yaml"
            sed -i 's|mautrix-imessage\.db|rustpush-matrix.db|g'      "$NEW_DATA_DIR/config.yaml"
        fi
    elif [ -d "$OLD_DATA_DIR" ] && [ -d "$NEW_DATA_DIR" ]; then
        echo "Both old and new data dirs exist. Refusing to overwrite."
        echo "  Old: $OLD_DATA_DIR"
        echo "  New: $NEW_DATA_DIR"
        echo "Resolve manually, then re-run."
        exit 1
    fi

    # Old binary cleanup (best-effort)
    rm -f "$HOME/.local/bin/mautrix-imessage-v2" 2>/dev/null || true

    # Strip OLD shell-rc alias marker block. The new install writes
    # markers under a new name (rustpush-matrix shortcuts), so the OLD
    # marked block would be orphaned with stale launchctl/systemctl
    # commands pointing at a service that no longer exists.
    for rc in "$HOME/.zshrc" "$HOME/.bashrc"; do
        [ -f "$rc" ] || continue
        if grep -q "# >>> mautrix-imessage shortcuts (managed) >>>" "$rc"; then
            # Delete from start marker through end marker, inclusive.
            sed -i '/# >>> mautrix-imessage shortcuts (managed) >>>/,/# <<< mautrix-imessage shortcuts (managed) <<</d' "$rc"
        fi
    done

    echo "✓ Migration complete. Continuing with install of rustpush-matrix..."
fi
```

### macOS (`install.sh`, `install-beeper.sh`)

Same structure, but launchd instead of systemd. **Two macOS-specific gotchas the Linux block doesn't have**:

1. **`sed -i` is platform-divergent.** GNU sed: `sed -i 's|...|...|g' file`. BSD sed (macOS default): `sed -i '' 's|...|...|g' file`. Use the portable form: `sed -i.bak 's|...|...|g' file && rm file.bak`. The existing Beeper-mac script (`install-beeper.sh:201`) uses `sed -i ''`; mirror that pattern within the macOS block, OR switch all migration `sed`s to the portable `-i.bak` form.

2. **macOS `.app` install location is not `/Applications`.** `make install` puts the app in the build directory; the install scripts don't move it to `/Applications`. **Verify-before-implementing**: where does each install script (`install.sh`, `install-beeper.sh`) actually drop the `.app`? The migration's old-app cleanup must point at the *real* path, not a guess. Likely candidates: build dir relative to the install script, `$HOME/Applications`, or wherever the tarball was extracted.

```bash
OLD_BUNDLE_ID="com.lrhodin.mautrix-imessage"
NEW_BUNDLE_ID="com.lrhodin.rustpush-matrix"
OLD_PLIST="$HOME/Library/LaunchAgents/${OLD_BUNDLE_ID}.plist"
OLD_DATA_DIR="$HOME/.local/share/mautrix-imessage"
NEW_DATA_DIR="$HOME/.local/share/rustpush-matrix"
# OLD_APP_PATH=??? — TBD: verify where the install actually places the .app

needs_migration=false
[ -d "$OLD_DATA_DIR" ] && needs_migration=true
[ -f "$OLD_PLIST" ] && needs_migration=true

if $needs_migration; then
    # ... same prompt + non-TTY guard as Linux block ...

    # Stop + remove old launchd service
    launchctl bootout "gui/$(id -u)/$OLD_BUNDLE_ID" 2>/dev/null || true
    launchctl unload "$OLD_PLIST" 2>/dev/null || true
    rm -f "$OLD_PLIST"

    # Move data dir + rename DB files (same shape as Linux). For sed:
    if [ -f "$NEW_DATA_DIR/config.yaml" ]; then
        sed -i '' 's|share/mautrix-imessage|share/rustpush-matrix|g' "$NEW_DATA_DIR/config.yaml"
        sed -i '' 's|mautrix-imessage\.db|rustpush-matrix.db|g'      "$NEW_DATA_DIR/config.yaml"
    fi

    # Strip OLD shell-rc alias marker block — same as Linux but BSD sed:
    for rc in "$HOME/.zshrc" "$HOME/.bashrc"; do
        [ -f "$rc" ] || continue
        if grep -q "# >>> mautrix-imessage shortcuts (managed) >>>" "$rc"; then
            sed -i '' '/# >>> mautrix-imessage shortcuts (managed) >>>/,/# <<< mautrix-imessage shortcuts (managed) <<</d' "$rc"
        fi
    done

    # Remove old .app bundle once OLD_APP_PATH is verified
    # [ -d "$OLD_APP_PATH" ] && rm -rf "$OLD_APP_PATH"

    echo "⚠  macOS will prompt for Contacts (and Full Disk Access if used)"
    echo "   the first time the new binary launches. This is expected — the"
    echo "   new bundle ID is treated as a new application."
fi
```

### `reset-bridge.sh`

Updated to know **both** names (so it cleans up regardless of which version the user is on). Probe both old and new paths/units; remove what's present. Specific updates:

- `STATE_DIR` probe: check both `~/.local/share/mautrix-imessage` and `~/.local/share/rustpush-matrix`.
- Default `BUNDLE_ID`: keep `com.lrhodin.mautrix-imessage` as a probe alongside `com.lrhodin.rustpush-matrix`.
- `systemctl --user stop`: try both unit names.
- `pgrep -f` (line 31): probe both `mautrix-imessage-v2` AND `rustpush-matrix` so a partial-state user (new binary running, old reset script invoked, or vice-versa) doesn't fail to detect the running process.
- `journalctl --unit=`: rotate and vacuum both unit names.

---

## Bridge-side safety net (Go)

**Behavior contract** (no code stub — implementation must follow `identity_store.go`'s actual platform-aware path resolution, not a hardcoded `~/.local/share`):

`pkg/connector/connector.go` `Start()` adds a startup check **before** any DB or session load:

- Resolve the data directory via the **same path-resolution logic** that `identity_store.go` uses (currently `os.UserConfigDir()` / XDG-aware semantics; verify the exact platform behavior before implementing — Linux likely uses `$XDG_DATA_HOME` / `~/.local/share`, macOS may use `~/Library/Application Support`).
- Check whether the OLD-name subdirectory exists at that resolved location AND the NEW-name subdirectory does NOT.
- If yes → return a fatal error with a message naming both paths and pointing the user at the install script. Bridge logs FATAL and exits non-zero.
- Otherwise → no-op.

Catches users who:
- Replace the binary out-of-band without running the install script.
- Have a custom systemd unit that points at the new binary path but the old data dir.
- Get the new binary via package manager updates that don't run our migration script.

Without this, the new binary would silently fall back to "fresh install" behavior, re-pair with Beeper, and the user thinks they lost everything.

**Verify-before-implementing**: confirm exactly where `identity_store.go`'s `dataDir` resolves on each platform (Linux + macOS). Mirror that resolution in the safety-net check, do not duplicate by assuming `~/.local/share`.

---

## Go-side hardcoded path changes

These references are NOT cosmetic — they determine where the bridge reads/writes data:

- `pkg/connector/identity_store.go:67,81,95` — `filepath.Join(dataDir, "mautrix-imessage", ...)` × 3 (session.json, identity.plist, trustedpeers.plist)
- `pkg/connector/carddav_crypto.go:28` — `filepath.Join(dir, "mautrix-imessage")`

Replace the `"mautrix-imessage"` literal with `"rustpush-matrix"`. Update accompanying doc comments in the same files.

The `vendor` string in `pkg/connector/audioconvert.go:405` (embedded in OGG voice-note metadata) is **cosmetic** — change for consistency, but no migration concerns.

File-header comments (`// mautrix-imessage - A Matrix-iMessage puppeting bridge.`) appear at the top of ~30 Go files. Bulk-update via sed in one commit.

---

## Files touched (complete list)

**Build/packaging:**
- `Makefile` (APP_NAME, CMD_PKG, BUNDLE_ID, DATA_DIR, clean targets)
- `cmd/mautrix-imessage/` → `cmd/rustpush-matrix/` (directory rename)
- `Info.plist` (CFBundleIdentifier, CFBundleName, CFBundleExecutable, NSContactsUsageDescription)
- `.github/workflows/release.yml` (tarball names, paths)

**Runtime (Go):**
- `pkg/connector/connector.go` (startup safety-net check)
- `pkg/connector/identity_store.go` (3 hardcoded paths)
- `pkg/connector/carddav_crypto.go` (1 hardcoded path)
- `pkg/connector/audioconvert.go` (OGG vendor string — optional, cosmetic)
- `pkg/connector/example-config.yaml` (binary-name comment, default DB URI)
- All Go file headers (~30 files, sed)

**Install scripts (4):**
- `scripts/install.sh` — macOS, self-hosted
- `scripts/install-linux.sh` — Linux, self-hosted
- `scripts/install-beeper.sh` — macOS, Beeper
- `scripts/install-beeper-linux.sh` — Linux, Beeper

Each gets the migration block prelude + path/unit/bundle-id renames throughout.

**Maintenance scripts:**
- `scripts/reset-bridge.sh` (cleanup probes for both old + new names)

**Docs:**
- `README.md` (title, all path/command references — many)

**Tests:**
- `pkg/connector/audioconvert_test.go` (vendor assertion if vendor string changes)
- `pkg/connector/carddav_crypto_test.go` (path-segment assertion at lines 64, 113, 165)

---

## Rollout

1. Land this PR. Branch contains: TPP + all renames + migration scripts + Go safety-net.
2. Internal/dogfood phase: existing dogfooders hit the prompt on next install-script run. Watch for friction.
3. Release notes call out:
   - macOS: TCC re-prompt for Contacts (and Full Disk Access if enabled) — this is normal, the new bundle ID is a new application as far as macOS is concerned.
   - All platforms: one-time consent prompt. Saying "no" leaves the existing install fully functional.
4. Migration block stays in the install script indefinitely. It's idempotent and self-extinguishing — no cleanup needed.
5. Bridge-side safety-net check stays for at least one full release cycle, ideally indefinitely (~30 lines, no maintenance burden).

---

## Open decisions

1. **Should we even do the full rename?** See "Decision: should we do this at all?" above. Confirm scope (do nothing / docs only / cosmetic only / full) before committing engineering time.
2. **Name approval.** If proceeding with `rustpush-matrix` (or any name derived from `rustpush`), confirm with the rustpush / OpenBubbles maintainers that they're OK with us using the name. A name that's clearly distinct from any vendored dep avoids this question.
3. **Final binary name.** Working assumption: `rustpush-matrix`. Alternatives: `rpmatrix`, `rpm` (collides with Red Hat package manager), `rpx`. The branch is named `rustpush-matrix` so this is the path of least resistance, but a fully-original name (no `rustpush` prefix) sidesteps approval risk.
4. **Bundle ID prefix.** Working assumption: `com.lrhodin.rustpush-matrix`. Could be `com.beeper.rustpush-matrix` or other.
5. **OGG vendor string change.** Cosmetic; embedded in outbound voice notes. Recommendation: change for consistency.
6. **GitHub repo rename.** Out of scope for this PR — easy follow-up (`gh repo rename`, GitHub auto-redirects old URLs and clones).

---

## Edge cases (covered in the migration block)

- Old data dir AND new data dir both exist → refuse, ask user to resolve.
- System-scope systemd unit (`/etc/systemd/system/`) → needs sudo; print commands and prompt.
- Custom `BBCTL_DIR` env var → already overridable via existing `${BBCTL_DIR:-...}` pattern; respected.
- Config.yaml has absolute paths to old dir → `sed` updates.
- User says "no", runs script again later → prompt re-fires (state-detection is idempotent).
- User has both old and new binary on `$PATH` → install script removes old binary in a known location; can't catch every install method.
- macOS keychain items keyed by old bundle ID → verify whether **anything** is stored there. `session.json` lives in the data dir, not keychain, so likely none. If any are found, document loss in release notes.
