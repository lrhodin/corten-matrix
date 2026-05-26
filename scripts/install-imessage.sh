#!/usr/bin/env bash
# ============================================================================
# install-imessage.sh — bootstrap installer for the `imessage` host CLI.
#
# Run this once on the host to put `imessage` on $PATH. It's idempotent —
# re-run any time to refresh to the latest version.
#
# Usage (safe to pipe through bash; fails fast on any error):
#
#     curl -fsSL https://raw.githubusercontent.com/lrhodin/imessage/master/scripts/install-imessage.sh \
#         | sudo bash
#
# Or download and inspect first:
#
#     curl -fsSL https://raw.githubusercontent.com/lrhodin/imessage/master/scripts/install-imessage.sh -o install-imessage.sh
#     less install-imessage.sh
#     sudo bash install-imessage.sh
# ============================================================================
set -euo pipefail

REPO_RAW="https://raw.githubusercontent.com/lrhodin/imessage/master"
CLI_URL="$REPO_RAW/scripts/imessage"
INSTALL_PATH="/usr/local/bin/imessage"

# ─── pretty output helpers ──────────────────────────────────────────────────
if [ -t 1 ]; then
    R='\033[1;31m'; G='\033[1;32m'; Y='\033[1;33m'; B='\033[1;34m'; N='\033[0m'
else
    R=''; G=''; Y=''; B=''; N=''
fi
ok()    { printf "${G}✓${N} %s\n" "$*"; }
warn()  { printf "${Y}⚠${N} %s\n" "$*"; }
err()   { printf "${R}✗${N} %s\n" "$*" >&2; }
info()  { printf "${B}→${N} %s\n" "$*"; }

# ─── preflight ──────────────────────────────────────────────────────────────
info "Installing the imessage host CLI..."
echo ""

# Must run as root so we can write /usr/local/bin (and the file ends up
# readable+executable by every user on the host, not just the installer).
if [ "$(id -u)" -ne 0 ]; then
    err "This script must run as root."
    echo "  Pipe through sudo:"
    echo "    curl -fsSL $REPO_RAW/scripts/install-imessage.sh | sudo bash"
    exit 1
fi

for tool in curl install; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        err "Missing required tool: $tool"
        case "$tool" in
            curl)    echo "  Install with: apt-get install curl  (or dnf/pacman/apk equivalent)" ;;
            install) echo "  Install with: apt-get install coreutils  (or your package manager's equivalent)" ;;
        esac
        exit 1
    fi
done
ok "Preflight: running as root, curl + install both present"

# ─── download + place ───────────────────────────────────────────────────────
TMP=$(mktemp)
trap 'rm -f "$TMP"' EXIT

info "Downloading $CLI_URL"
if ! curl -fsSL "$CLI_URL" -o "$TMP"; then
    err "Download failed."
    echo "  Check your network and that the URL is reachable:"
    echo "    curl -I $CLI_URL"
    exit 1
fi

# Sanity check: the file should be a bash script, not an HTML error page.
if ! head -1 "$TMP" | grep -qE '^#!/(usr/bin/env )?(bash|sh)'; then
    err "Downloaded file doesn't look like a shell script."
    echo "  Check the URL by hand and re-run:"
    echo "    less $TMP"
    exit 1
fi
ok "Download verified ($(wc -l < "$TMP") lines)"

info "Installing to $INSTALL_PATH"
install -m 0755 "$TMP" "$INSTALL_PATH"
ok "Installed (mode 0755, owner root:root)"

# ─── PATH verification ──────────────────────────────────────────────────────
# /usr/local/bin is on $PATH by default on every standard Linux distro
# (Debian/Ubuntu /etc/profile, Fedora /etc/login.defs, Arch /etc/profile,
# Alpine /etc/profile). The check is here for the edge cases where it's
# been removed.
if ! echo "$PATH" | tr ':' '\n' | grep -qx /usr/local/bin; then
    warn "/usr/local/bin is NOT on your current PATH."
    echo "  Add it to your shell rc:"
    echo "      echo 'export PATH=\"/usr/local/bin:\$PATH\"' >> ~/.bashrc"
    echo "      echo 'export PATH=\"/usr/local/bin:\$PATH\"' >> ~/.zshrc"
    echo "  Then open a new terminal."
fi

# ─── self-test ──────────────────────────────────────────────────────────────
if ! "$INSTALL_PATH" help >/dev/null 2>&1; then
    err "Self-test failed — $INSTALL_PATH help did not run cleanly."
    echo "  Inspect manually: $INSTALL_PATH help"
    exit 1
fi
ok "Self-test passed — 'imessage help' runs"

echo ""
echo "═══════════════════════════════════════════════════════════"
ok "Done. The imessage CLI is installed."
echo "═══════════════════════════════════════════════════════════"
echo ""
echo "Next steps:"
echo "  1. Open a new terminal (so PATH is fresh) and run:  imessage help"
echo "  2. Drop in a docker-compose.yml:"
echo "       curl -fsSL $REPO_RAW/docker-compose.example.yml -o docker-compose.yml"
echo "  3. Edit it (BEEPER + bind-mount path), then start:"
echo "       imessage start"
echo "       imessage setup"
echo ""
echo "Full guide: https://github.com/lrhodin/imessage/blob/master/docs/docker.md"
