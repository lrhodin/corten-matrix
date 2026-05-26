#!/bin/bash
# ============================================================================
# docker-entrypoint.sh — dispatcher for the mautrix-imessage-v2 container.
#
# Subcommands:
#   run     (default) — wait for /data/config.yaml, then exec the bridge.
#                       Polls every 30s so `docker exec -it
#                       Rustpush-Matrix imessage-setup` can populate
#                       config against a running container.
#   setup   — invoke the existing install-beeper-linux.sh (or
#             install-linux.sh) inside the container. `BEEPER` env var
#             in compose selects which.
#   login   — re-run only the iMessage login flow.
#
# Host-side concerns (shell aliases, bare-Linux migration) live in the
# `imessage` host CLI script in this repo (scripts/imessage). The
# container only does container things.
#
# Privilege model: the CMD is invoked as root so the entrypoint can fix
# /data ownership when Docker auto-creates the bind-mount source as
# root (happens on first `docker compose up` if the host path didn't
# exist yet). After chown, we re-exec via gosu as the bridge user. The
# long-lived bridge process is never root.
# ============================================================================
set -euo pipefail

# ── Privilege bootstrap ──────────────────────────────────────────────────────
# Runs only on the initial root invocation. After gosu re-exec we come
# back in as the bridge user and skip this block.
if [ "$(id -u)" = "0" ]; then
    PUID="${PUID:-1000}"
    PGID="${PGID:-1000}"

    if [ "$(id -u bridge)" != "$PUID" ]; then
        usermod -o -u "$PUID" bridge >/dev/null 2>&1 || true
    fi
    if [ "$(id -g bridge)" != "$PGID" ]; then
        groupmod -o -g "$PGID" bridge >/dev/null 2>&1 || true
    fi

    # /data ownership reconciliation. If the top-level dir doesn't match
    # PUID:PGID we run a recursive chown — not just /data itself —
    # because a partial mismatch (top dir correct, files inside owned by
    # a stranger UID) shows up as "permission denied" deep in the bridge
    # startup ("unable to open database file"), nowhere near the actual
    # cause. Recursive chown on a small state dir is fast.
    mkdir -p /data
    if [ "$(stat -c '%u:%g' /data 2>/dev/null)" != "${PUID}:${PGID}" ]; then
        echo "[entrypoint] chowning /data tree to ${PUID}:${PGID} (was $(stat -c '%u:%g' /data))" >&2
        chown -R "${PUID}:${PGID}" /data
    fi

    exec gosu bridge "$0" "$@"
fi
# ── From here on we are the bridge user. ─────────────────────────────────────

BIN=/usr/local/bin/mautrix-imessage-v2
DATA_DIR=/data
CONFIG="$DATA_DIR/config.yaml"
BBCTL=/usr/local/bin/bbctl
SCRIPTS=/opt/imessage/scripts
SETUP_LOCK="$DATA_DIR/.setup-in-progress"

CMD="${1:-run}"
shift || true

usage() {
    cat <<'EOF'
usage: /entrypoint.sh <subcommand>

  run        Start the bridge (waits for /data/config.yaml). Default.
  setup      Run the interactive setup wizard. BEEPER env var picks
             Beeper (true) vs self-hosted homeserver (false).
  login      Re-run only the iMessage login flow.

For host-side commands (logs, restart, update, migrate, aliases) use
the `imessage` host CLI shipped alongside this image.
EOF
}

require_tty() {
    if [ ! -t 0 ] || [ ! -t 1 ]; then
        echo "error: '$1' is interactive — re-run with 'docker exec -it' so a TTY is attached." >&2
        exit 2
    fi
}

cmd_run() {
    # Wait for setup. Keeps PID 1 alive so `docker exec -it
    # Rustpush-Matrix imessage-setup` works against a "running"
    # container. The lock
    # check prevents the bridge from grabbing the DB mid-login —
    # install-beeper-linux.sh writes config.yaml partway through (during
    # `bbctl config`) and then continues with the iMessage login.
    local warned=0
    while [ ! -f "$CONFIG" ] || [ -f "$SETUP_LOCK" ]; do
        if [ "$warned" -eq 0 ]; then
            echo "no /data/config.yaml yet — run 'imessage setup' from the host to configure the bridge."
            warned=1
        fi
        sleep 30
    done
    # cd into /data so anisette's relative state/anisette/ lands on the
    # volume. WORKDIR /data in the Dockerfile already does this; the
    # cd is defensive.
    cd "$DATA_DIR"
    exec "$BIN" -c "$CONFIG"
}

cmd_setup() {
    require_tty "setup"

    local script=""
    local script_args=()
    case "${BEEPER:-}" in
        true|TRUE|1|yes|YES)
            script="$SCRIPTS/install-beeper-linux.sh"
            script_args=("$BIN" "$DATA_DIR" "$BBCTL")
            ;;
        false|FALSE|0|no|NO)
            script="$SCRIPTS/install-linux.sh"
            script_args=("$BIN" "$DATA_DIR")
            ;;
        "")
            cat >&2 <<EOF
error: BEEPER env var is not set.

Set BEEPER in your docker-compose.yml under the Rustpush-Matrix service:

  environment:
    BEEPER: "true"     # for Beeper deploys
    BEEPER: "false"    # for self-hosted homeservers

Then re-run: imessage setup
EOF
            exit 2
            ;;
        *)
            echo "error: BEEPER must be 'true' or 'false' (got: '${BEEPER}')" >&2
            exit 2
            ;;
    esac

    # Hold the setup lock so cmd_run (PID 1) doesn't race-start the
    # bridge when the install script writes config.yaml partway through.
    : > "$SETUP_LOCK"
    trap 'rm -f "$SETUP_LOCK"' EXIT

    export IN_DOCKER=1
    cd "$DATA_DIR"
    local rc=0
    if ! "$script" "${script_args[@]}"; then
        rc=$?
    fi
    exit "$rc"
}

cmd_login() {
    require_tty "login"
    if [ ! -f "$CONFIG" ]; then
        echo "error: no /data/config.yaml found. Run 'imessage setup' first." >&2
        exit 1
    fi
    cd "$DATA_DIR"
    exec "$BIN" login -c "$CONFIG"
}

case "$CMD" in
    run)      cmd_run "$@" ;;
    setup)    cmd_setup "$@" ;;
    login)    cmd_login "$@" ;;
    help|-h|--help) usage ;;
    *)
        # Fall through: allow `docker run image <arbitrary command>` for
        # diagnostics (e.g. `docker run --rm image bash`). Standard
        # entrypoint behavior — bypass the dispatcher when the first
        # arg isn't one of our subcommands.
        exec "$CMD" "$@"
        ;;
esac
