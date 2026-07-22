#!/usr/bin/env bash
#
# Full bridge reset: delete Beeper registration, wipe ALL local state.
# You will need to re-login (2FA) after reset.
#
# Usage: corten-matrix reset [--yes]
#   Prompts for confirmation unless --yes is passed. Refuses to run
#   non-interactively without --yes, since it is not undoable.
#
# Invoked by pkg/cli RunManagement as:
#   reset-bridge.sh <corten-binary> <bundle-id> [user args...]
#
set -euo pipefail

BINARY="${1:-}"
[ $# -gt 0 ] && shift
BUNDLE_ID="${1:-com.lrhodin.corten-matrix}"
[ $# -gt 0 ] && shift

ASSUME_YES=0
for arg in "$@"; do
    case "$arg" in
        -y|--yes) ASSUME_YES=1 ;;
    esac
done

STATE_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/corten-matrix"
BRIDGE_NAME="${BRIDGE_NAME:-sh-imessage}"
SERVICE_NAME="${SERVICE_NAME:-corten-matrix}"
UNAME_S=$(uname -s)

# ── Confirm BEFORE touching anything ─────────────────────────
# This deletes the Beeper registration (tearing down the Matrix rooms) and
# wipes every local file including the login. There is no undo.
if [ "$ASSUME_YES" -ne 1 ]; then
    if [ ! -t 0 ]; then
        echo "ERROR: refusing to reset non-interactively. Re-run with --yes if you mean it." >&2
        exit 1
    fi
    echo "This will:"
    echo "  • stop the bridge"
    echo "  • delete the '$BRIDGE_NAME' registration from Beeper (removes the Matrix rooms)"
    echo "  • wipe all local state in $STATE_DIR"
    echo ""
    echo "You will need to re-login (2FA) afterwards. This cannot be undone."
    echo ""
    read -r -p "Type 'reset' to confirm: " reply || reply=""
    # Tolerate a trailing CR (CRLF terminals, pty wrappers) and stray spaces;
    # anything else still has to be exactly "reset".
    reply=$(printf '%s' "$reply" | tr -d '[:space:]')
    if [ "$reply" != "reset" ]; then
        echo "Aborted — nothing was changed."
        exit 1
    fi
fi

# ── Stop the bridge ──────────────────────────────────────────
echo "Stopping bridge..."
if [ "$UNAME_S" = "Darwin" ]; then
    launchctl unload "$HOME/Library/LaunchAgents/$BUNDLE_ID.plist" 2>/dev/null || true
else
    systemctl --user stop "$SERVICE_NAME" 2>/dev/null || true
fi

sleep 1
# Match the SERVICE process specifically (`corten-matrix bridge-all`), not any
# process whose command line contains "corten-matrix" — this script's own
# parent is `corten-matrix reset`, so a broad match would always see itself
# still running and abort the reset.
if pgrep -f "corten-matrix bridge-all" >/dev/null 2>&1; then
    echo "ERROR: bridge process still running after stop" >&2
    exit 1
fi

# ── Delete server-side registration (cleans up Matrix rooms) ──
# bbctl is compiled into the corten-matrix binary (pkg/bbctl) and is invoked
# as `corten-matrix bbctl ...` — there is no standalone bbctl to locate.
# If the binary isn't usable, skip deregistration rather than aborting: the
# local wipe is still worth doing, and a self-hosted install has no Beeper
# registration to delete in the first place.
echo ""
if [ -n "$BINARY" ] && [ -x "$BINARY" ]; then
    # Check whoami first: a registration the server has already dropped can
    # linger in bbctl whoami, and `bbctl delete` then fails with M_NOT_FOUND
    # (HTTP 404). Under set -e that aborts the reset with a confusing error,
    # even though there's nothing left to delete.
    if "$BINARY" bbctl whoami 2>/dev/null | grep -q "^[[:space:]]*$BRIDGE_NAME "; then
        echo "Deleting bridge registration from Beeper..."
        echo "(Answer the confirmation prompt below)"
        echo ""
        "$BINARY" bbctl delete "$BRIDGE_NAME" || \
            echo "⚠  Registration already absent on server — continuing with local wipe."
    else
        echo "✓ No '$BRIDGE_NAME' registration on server — skipping delete."
    fi
else
    echo "⚠  corten-matrix binary not available — skipping Beeper deregistration."
fi

# ── Clear journal logs ───────────────────────────────────────
echo ""
echo "Clearing bridge journal logs..."
if [ "$UNAME_S" != "Darwin" ]; then
    journalctl --user --unit="$SERVICE_NAME" --rotate 2>/dev/null || true
    journalctl --user --unit="$SERVICE_NAME" --vacuum-time=1s 2>/dev/null || true
    echo "✓ Logs cleared"
else
    echo "  (macOS — logs managed by launchd, skipping)"
fi

# ── Wipe EVERYTHING ─────────────────────────────────────────
# bridge-manager/ is excluded for compatibility with pre-2026-06 installs that
# still have a standalone bbctl (and its Beeper credentials) there; current
# installs never create it.
echo ""
echo "Wiping all state in $STATE_DIR/ ..."
if [ -d "$STATE_DIR" ]; then
    find "$STATE_DIR" -maxdepth 1 -not -name bridge-manager -not -path "$STATE_DIR" -exec rm -rf {} +

    # Verify
    REMAINING=$(find "$STATE_DIR" -maxdepth 1 -not -name bridge-manager -not -path "$STATE_DIR" | wc -l)
    if [ "$REMAINING" -ne 0 ]; then
        echo "ERROR: state directory not fully cleaned:" >&2
        ls -la "$STATE_DIR/"
        exit 1
    fi
else
    echo "  (no state directory at $STATE_DIR — nothing to wipe)"
fi

echo ""
echo "✓ Bridge fully reset."
echo "  All state wiped — you will need to re-login (2FA)."
echo ""
echo "  Run 'corten-matrix setup-beeper' to re-register, login, and start the bridge."
