#!/usr/bin/env bash
#
# Full bridge reset: delete Beeper registration, wipe ALL local state.
# You will need to re-login (2FA) after reset.
#
# Usage: corten-matrix reset   (run interactively — prompts for confirmation)
#
set -euo pipefail

STATE_DIR="$HOME/.local/share/corten-matrix"
BRIDGE_NAME="sh-imessage"
UNAME_S=$(uname -s)
BBCTL="$STATE_DIR/bridge-manager/bbctl"

# ── Preflight checks ────────────────────────────────────────
if [ ! -x "$BBCTL" ]; then
    echo "ERROR: bbctl not found at $BBCTL"
    exit 1
fi

# ── Stop the bridge ──────────────────────────────────────────
echo "Stopping bridge..."
if [ "$UNAME_S" = "Darwin" ]; then
    BUNDLE_ID="${1:-com.lrhodin.corten-matrix}"
    launchctl unload "$HOME/Library/LaunchAgents/$BUNDLE_ID.plist" 2>/dev/null || true
else
    systemctl --user stop corten-matrix 2>/dev/null || true
fi

sleep 1
if pgrep -f corten-matrix >/dev/null 2>&1; then
    echo "ERROR: bridge process still running after stop"
    exit 1
fi

# ── Delete server-side registration (cleans up Matrix rooms) ──
# Check whoami first: a registration that the server has already dropped can
# linger in bbctl whoami, and `bbctl delete` then fails with M_NOT_FOUND
# (HTTP 404). Under set -e that aborts the reset with a confusing error, even
# though there's nothing left to delete. Skip the delete when nothing is
# registered, and don't let a not-found delete abort the local wipe.
echo ""
if "$BBCTL" whoami 2>/dev/null | grep -q "^[[:space:]]*$BRIDGE_NAME "; then
    echo "Deleting bridge registration from Beeper..."
    echo "(Answer the confirmation prompt below)"
    echo ""
    "$BBCTL" delete "$BRIDGE_NAME" || \
        echo "⚠  Registration already absent on server — continuing with local wipe."
else
    echo "✓ No '$BRIDGE_NAME' registration on server — skipping delete."
fi

# ── Clear journal logs ───────────────────────────────────────
echo ""
echo "Clearing bridge journal logs..."
if [ "$UNAME_S" != "Darwin" ]; then
    journalctl --user --unit=corten-matrix --rotate 2>/dev/null || true
    journalctl --user --unit=corten-matrix --vacuum-time=1s 2>/dev/null || true
    echo "✓ Logs cleared"
else
    echo "  (macOS — logs managed by launchd, skipping)"
fi

# ── Wipe EVERYTHING ─────────────────────────────────────────
echo ""
echo "Wiping all state in $STATE_DIR/ ..."
find "$STATE_DIR" -maxdepth 1 -not -name bridge-manager -not -path "$STATE_DIR" -exec rm -rf {} +

# Verify
REMAINING=$(find "$STATE_DIR" -maxdepth 1 -not -name bridge-manager -not -path "$STATE_DIR" | wc -l)
if [ "$REMAINING" -ne 0 ]; then
    echo "ERROR: state directory not fully cleaned:"
    ls -la "$STATE_DIR/"
    exit 1
fi

echo ""
echo "✓ Bridge fully reset."
echo "  All state wiped — you will need to re-login (2FA)."
echo ""
echo "  Run 'corten-matrix setup-beeper' to re-register, login, and start the bridge."
