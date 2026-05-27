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
# Privilege model:
#   The image's USER is root so PID 1 enters this script with full
#   privileges. The root prelude below — gated on `id -u = 0` and a
#   needs-fix check — chowns bind mounts to PUID:PGID and creates a
#   host-source → /data symlink so absolute paths baked into
#   config.yaml by the bare-Linux installer (e.g. file:/root/.local/
#   share/mautrix-imessage/mautrix-imessage.db) resolve inside the
#   container. Then setpriv drops to PUID:PGID and re-execs this
#   script. The second pass skips the prelude and runs the bridge as
#   the configured user.
#
#   `docker exec` invocations from the host wrapper run as root by
#   default (the container's USER), so they go through
#   /usr/local/bin/as-bridge which applies the same setpriv drop.
# ============================================================================
set -euo pipefail

# ── Root prelude ────────────────────────────────────────────────────────────
# Only runs on the first pass (real container start, before setpriv). After
# the re-exec we're running as PUID:PGID and the `id -u = 0` check skips
# this whole block.
#
# Every fix below is conditional — chown only when find spots a mismatched
# file; symlink only when readlink doesn't already match. Repeat invocations
# (`docker restart`, `compose up -d` against an unchanged container) cost a
# single find -quit each and exit silently.
if [ "$(id -u)" = "0" ]; then
    PUID="${PUID:-1000}"
    PGID="${PGID:-1000}"

    if ! printf '%s' "$PUID" | grep -qE '^[0-9]+$' || ! printf '%s' "$PGID" | grep -qE '^[0-9]+$'; then
        echo "[entrypoint] PUID/PGID must be numeric (got PUID=$PUID PGID=$PGID)" >&2
        exit 1
    fi

    # find -quit returns the first path that fails ANY of the four criteria
    # (wrong uid, wrong gid, dir without 0777, file without 0666). If
    # nothing fails, the find returns empty and we skip the recursive
    # chown/chmod entirely — the common steady-state case.
    needs_fix() {
        local dir="$1" uid="$2" gid="$3"
        [ -d "$dir" ] || return 1
        local bad
        bad=$(find "$dir" \( \
            ! -user "$uid" -o \
            ! -group "$gid" -o \
            \( -type d ! -perm -0777 \) -o \
            \( ! -type d ! -perm -0666 \) \
        \) -print -quit 2>/dev/null || true)
        [ -n "$bad" ]
    }

    for dir in /data /home/bridge/.config/bbctl; do
        if needs_fix "$dir" "$PUID" "$PGID"; then
            echo "[entrypoint] fixing perms: $dir → $PUID:$PGID"
            chown -R "$PUID:$PGID" "$dir"
            chmod -R a+rwX "$dir"
        fi
    done

    # Mirror the host bind-mount source path inside the container, pointing
    # at /data. Lets absolute paths from a bare-Linux install (e.g. config
    # has `uri: file:/root/.local/share/mautrix-imessage/mautrix-imessage.db`)
    # resolve when the bridge runs in Docker. /proc/self/mountinfo field 4
    # is the host-side source for bind mounts; field 5 is the mount point.
    host_src=$(awk '$5 == "/data" { print $4; exit }' /proc/self/mountinfo)
    if [ -n "$host_src" ] && [ "$host_src" != "/data" ]; then
        case "$host_src" in
            /*)
                current=$(readlink "$host_src" 2>/dev/null || true)
                if [ "$current" != "/data" ]; then
                    if [ -e "$host_src" ] && [ ! -L "$host_src" ]; then
                        echo "[entrypoint] WARN: $host_src exists and is not a symlink; skipping host-path link" >&2
                    else
                        echo "[entrypoint] linking $host_src → /data"
                        mkdir -p "$(dirname "$host_src")"
                        ln -sfn /data "$host_src"
                    fi
                fi
                ;;
        esac
    fi

    # --clear-groups (not --init-groups): --init-groups looks PUID up in
    # /etc/passwd via getpwuid, which fails for arbitrary UIDs without an
    # /etc/passwd entry (UNRAID 99:100, TrueNAS 568:568, etc.). --clear-groups
    # drops all supplementary groups; the bridge doesn't need any inside
    # this container.
    export HOME=/home/bridge
    exec setpriv --reuid "$PUID" --regid "$PGID" --clear-groups -- "$0" "$@"
fi

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
