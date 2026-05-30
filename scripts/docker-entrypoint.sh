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
# Only runs on the first pass (real container start, before setpriv). The
# ENTRYPOINT_PRIV_DROPPED sentinel — exported just before the re-exec and
# preserved across it by setpriv — marks that the drop already happened, so
# this block is skipped on the second pass. We can't rely on `id -u != 0` for
# that: with PUID=0 the re-exec stays uid 0, so an `id -u = 0` guard alone
# would re-enter the prelude forever (infinite setpriv loop, container never
# reaches the dispatcher).
#
# Every fix below is conditional — chown only when find spots a mismatched
# file; symlink only when readlink doesn't already match. Repeat invocations
# (`docker restart`, `compose up -d` against an unchanged container) cost a
# single find -quit each and exit silently.
if [ "$(id -u)" = "0" ] && [ -z "${ENTRYPOINT_PRIV_DROPPED:-}" ]; then
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

    # Pre-create the log dir/file owned by PUID. The Docker HEALTHCHECK runs as
    # root and appends here whenever config exists but the bridge isn't PID 1
    # (e.g. during `imessage setup`). If it CREATES a root-owned bridge.log
    # first, the PUID bridge/init-db can't rotate it on startup (rename ->
    # EACCES) and setup fails until a retry happens to win the race.
    mkdir -p /data/logs
    [ -e /data/logs/bridge.log ] || : > /data/logs/bridge.log
    chown "$PUID:$PGID" /data/logs /data/logs/bridge.log 2>/dev/null || true

    # A setup lock present at container start is ALWAYS stale: `imessage setup`
    # runs as a `docker exec` child, never as part of PID 1, so a lock can't
    # have survived a (re)start. Clear it here so cmd_run isn't wedged waiting
    # on a lock whose setup process is long gone (the EXIT-trap cleanup in
    # cmd_setup doesn't fire on Ctrl-C / closed exec session / SIGKILL).
    rm -f /data/.setup-in-progress 2>/dev/null || true

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

                # Open up search (o+x) on each ancestor dir of the symlink so
                # PUID can traverse them. /root ships at mode 0700 in the base
                # image, so without this chmod the kernel denies the path walk
                # before SQLite ever calls open() on the DB. Idempotent.
                parent="$(dirname "$host_src")"
                while [ -n "$parent" ] && [ "$parent" != "/" ]; do
                    chmod o+x "$parent" 2>/dev/null || true
                    parent="$(dirname "$parent")"
                done
                ;;
        esac
    fi

    # --clear-groups (not --init-groups): --init-groups looks PUID up in
    # /etc/passwd via getpwuid, which fails for arbitrary UIDs without an
    # /etc/passwd entry (UNRAID 99:100, TrueNAS 568:568, etc.). --clear-groups
    # drops all supplementary groups; the bridge doesn't need any inside
    # this container.
    export HOME=/home/bridge
    export ENTRYPOINT_PRIV_DROPPED=1
    exec setpriv --reuid "$PUID" --regid "$PGID" --clear-groups -- "$0" "$@"
fi

BIN=/usr/local/bin/mautrix-imessage-v2
DATA_DIR=/data
CONFIG="$DATA_DIR/config.yaml"
BBCTL=/usr/local/bin/bbctl
SCRIPTS=/opt/imessage/scripts
SETUP_LOCK="$DATA_DIR/.setup-in-progress"
# session.json (the iMessage identity/APNs state, written by every successful
# login) marks that setup has actually completed a login. /home/bridge/.local/
# share/mautrix-imessage is symlinked to /data, so the bridge writes it here.
SESSION_FILE="$DATA_DIR/session.json"
# A bridge that exits in under this many seconds is treated as a startup
# failure (e.g. a rejected as_token) rather than a real run — see cmd_run.
BRIDGE_MIN_UPTIME="${BRIDGE_MIN_UPTIME:-30}"

# A setup lock is honored only while the setup process that created it is still
# alive. cmd_setup records its PID in the lock; `imessage setup` runs as a
# `docker exec` child in PID 1's namespace, so cmd_run can see it via /proc. An
# interrupted setup (Ctrl-C, closed exec session, SIGKILL) never runs its
# EXIT-trap cleanup, leaving the lock behind — without this check that stale
# lock wedges cmd_run forever and the bridge never starts. A PID-less lock
# (pre-upgrade format, or one left by an older image) is likewise treated as
# stale and cleared.
setup_in_progress() {
    [ -f "$SETUP_LOCK" ] || return 1
    local pid
    pid=$(cat "$SETUP_LOCK" 2>/dev/null || true)
    if [ -n "$pid" ] && [ -d "/proc/$pid" ]; then
        return 0
    fi
    rm -f "$SETUP_LOCK"
    echo "[entrypoint] cleared stale setup lock (setup PID '${pid:-none}' not running)"
    return 1
}

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
    local warned start elapsed rc
    while true; do
        # Wait until setup has produced a runnable state. Three gates:
        #   * config.yaml exists        — something to run against.
        #   * no LIVE setup in progress  — install-*.sh writes config partway
        #                                  through, then does the login; don't
        #                                  race it (a stale lock is auto-cleared).
        #   * session.json exists        — a login has actually completed.
        # The session.json gate is load-bearing: /data persists across rebuilds,
        # so config.yaml is usually present at boot; without it PID 1 would start
        # a login-less bridge before `imessage setup` ever runs.
        warned=0
        while [ ! -f "$CONFIG" ] || setup_in_progress || [ ! -f "$SESSION_FILE" ]; do
            if [ "$warned" -eq 0 ]; then
                echo "waiting for setup — run 'imessage setup' from the host (no config.yaml or no completed iMessage login yet)."
                warned=1
            fi
            sleep 30
        done

        # SUPERVISE the bridge as a child instead of exec-ing it as PID 1.
        #
        # A bridge that dies almost immediately is a startup failure — most
        # commonly a rejected as_token after `bbctl delete` ("M_UNKNOWN_TOKEN",
        # log.Fatal). If the bridge were PID 1, that fatal would exit the
        # container, Docker's restart policy would relaunch it, and it would
        # fatal again — parking the container in "restarting", which BLOCKS
        # `docker exec`. But `docker exec` is exactly how `imessage setup`
        # attaches to regenerate config.yaml — so a crash-looping container
        # fights the only tool that can fix it. (On bare metal this never
        # happens: setup stops the systemd unit first, so no bridge races it.)
        #
        # Supervising keeps PID 1 as this shell: the container is always
        # "running" and exec-able. On a fast bridge exit we hold here, exec-able,
        # for setup to fix config; once it rewrites config.yaml the next attempt
        # succeeds on its own. cd into /data so anisette's relative
        # state/anisette/ resolves the same as WORKDIR.
        cd "$DATA_DIR"
        echo "[entrypoint] starting bridge"
        STOPPING=0
        "$BIN" -c "$CONFIG" &
        BRIDGE_CHILD=$!
        # `docker stop` sends SIGTERM to PID 1 (this shell), not the child —
        # forward it so the bridge shuts down cleanly. wait returns early when a
        # trapped signal fires, so reap until the child is actually gone.
        trap 'STOPPING=1; kill -TERM "$BRIDGE_CHILD" 2>/dev/null' TERM INT
        start=$(date +%s)
        rc=0
        while kill -0 "$BRIDGE_CHILD" 2>/dev/null; do
            if wait "$BRIDGE_CHILD"; then rc=0; else rc=$?; fi
        done
        trap - TERM INT
        elapsed=$(( $(date +%s) - start ))

        if [ "$STOPPING" = "1" ]; then
            # Being stopped — propagate and let the container go down.
            exit "$rc"
        fi
        if [ "$elapsed" -ge "$BRIDGE_MIN_UPTIME" ]; then
            # Ran long enough to be a real session (a healthy bridge only exits
            # on crash or signal). Exit so Docker's restart policy restarts it.
            echo "[entrypoint] bridge exited after ${elapsed}s (rc=$rc); container will restart."
            exit "$rc"
        fi
        # Fast exit = startup failure. Do NOT crash-loop the container out of
        # `docker exec` reach. Stay up and exec-able so `imessage setup` can fix
        # config.yaml, then retry.
        echo "[entrypoint] bridge exited after only ${elapsed}s (rc=$rc) — likely a bad/stale config (e.g. a rejected as_token after 'bbctl delete'). NOT crash-looping: the container stays up and exec-able so 'imessage setup' can regenerate config.yaml. Retrying in 30s."
        sleep 30
    done
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

    # Hold the setup lock so cmd_run (PID 1) doesn't race-start the bridge when
    # the install script writes config.yaml partway through. Record our PID so
    # cmd_run / the healthcheck can tell a live setup from a stale lock left by
    # an interrupted run (the EXIT trap below only fires on a clean exit).
    echo $$ > "$SETUP_LOCK"
    # Remove the lock on a clean exit AND on the common interrupts — Ctrl-C
    # (INT), `docker stop` (TERM), a disconnected exec session (HUP) — by
    # turning each signal into a normal exit so the EXIT handler runs. SIGKILL
    # can't be trapped, but cmd_run's PID-liveness check and the boot-time clear
    # reclaim a lock left behind that way, so a leaked lock can never wedge the
    # bridge regardless of how setup dies.
    trap 'rm -f "$SETUP_LOCK"' EXIT
    trap 'exit 130' INT TERM HUP

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
