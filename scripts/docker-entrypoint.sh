#!/bin/bash
# ============================================================================
# docker-entrypoint.sh — dispatcher for the mautrix-imessage-v2 container.
#
# Subcommands:
#   run       (default) — exec the bridge if /data/config.yaml exists,
#                         otherwise print a hint and exit non-zero.
#   setup     — interactively ask Beeper vs self-hosted, then exec the
#               corresponding existing install script with IN_DOCKER=1.
#               Same as bare-Linux setup, just inside the container.
#   login     — re-run only the iMessage login flow.
#   aliases   — when EMIT_ALIASES=true, print a host-side alias block
#               (imessage-setup, imessage-logs) that the user can append
#               to ~/.bashrc / ~/.zshrc. No-op otherwise.
#
# All setup scripts read IN_DOCKER to skip apt-install, systemd, and
# host-bashrc-aliases sections that don't make sense in the container.
#
# Privilege model: the container's CMD is invoked as root so the
# entrypoint can fix /data ownership when Docker auto-creates the bind
# mount source as root (happens on first `docker compose up` if the
# host path didn't exist yet). After chown, we re-exec ourselves via
# gosu as the bridge user. Every subcommand below runs as bridge — the
# long-lived bridge process is never root.
# ============================================================================
set -euo pipefail

# ── Privilege bootstrap ──────────────────────────────────────────────────────
# Runs only on the initial root invocation. After gosu re-exec we come back
# in as the bridge user and skip this block entirely.
if [ "$(id -u)" = "0" ]; then
    PUID="${PUID:-1000}"
    PGID="${PGID:-1000}"

    # Align the bridge user/group to the requested UID/GID if different
    # from the image default. Cheap; only fires when the user actually
    # overrides via PUID/PGID in compose.
    if [ "$(id -u bridge)" != "$PUID" ]; then
        usermod -o -u "$PUID" bridge >/dev/null 2>&1 || true
    fi
    if [ "$(id -g bridge)" != "$PGID" ]; then
        groupmod -o -g "$PGID" bridge >/dev/null 2>&1 || true
    fi

    # Make sure /data is writable by the bridge user. Only chown when
    # ownership doesn't already match — preserves existing UIDs on bind
    # mounts from bare-Linux installs (migration path stays clean).
    mkdir -p /data
    if [ "$(stat -c '%u:%g' /data 2>/dev/null)" != "${PUID}:${PGID}" ]; then
        chown "${PUID}:${PGID}" /data
    fi

    # Drop privileges. gosu replaces this shell with the same script
    # running as the bridge user — no fork, no extra process, signals
    # propagate cleanly to PID 1.
    exec gosu bridge "$0" "$@"
fi
# ── From here on we are the bridge user. ─────────────────────────────────────

BIN=/usr/local/bin/mautrix-imessage-v2
DATA_DIR=/data
CONFIG="$DATA_DIR/config.yaml"
BBCTL=/usr/local/bin/bbctl
SCRIPTS=/opt/imessage/scripts
MIGRATION_DONE="$DATA_DIR/.docker-migration-done"
SETUP_LOCK="$DATA_DIR/.setup-in-progress"

CMD="${1:-run}"
shift || true

usage() {
    cat <<'EOF'
usage: /entrypoint.sh <subcommand>

  run        Start the bridge (requires /data/config.yaml). Default.
  setup      Run the interactive setup wizard (BEEPER env var picks Beeper vs self-hosted).
  login      Re-run only the iMessage login flow.
  migrate    Walk the user through migrating from a bare-Linux install.
             Auto-runs on first start when MIGRATE=true is set in compose.
  aliases    Print the host-side alias block (when EMIT_ALIASES=true).
EOF
}

require_tty() {
    if [ ! -t 0 ] || [ ! -t 1 ]; then
        echo "error: '$1' is interactive — re-run with 'docker exec -it' or 'docker compose run' so a TTY is attached." >&2
        exit 2
    fi
}

cmd_run() {
    # Reconcile alias state against EMIT_ALIASES every start. Cheap when
    # nothing changes (idempotent), guarantees the host rc files match
    # the compose state without the user having to remember a setup step.
    manage_aliases || true

    # Auto-trigger migration on first start when MIGRATE=true is set in
    # compose and we've never migrated this data dir before. The user
    # only sees the migration once — sentinel keeps subsequent restarts
    # from re-prompting.
    case "${MIGRATE:-}" in
        true|TRUE|1|yes|YES)
            if [ ! -f "$MIGRATION_DONE" ] && [ -f "$CONFIG" ]; then
                cmd_migrate
            fi
            ;;
    esac

    # Wait for setup. Keeps PID 1 alive so `docker exec -it bridge
    # imessage-setup` can be run against a "running" container. As soon
    # as setup writes /data/config.yaml AND finishes (no setup lock),
    # the next poll picks it up and exec's the bridge.
    #
    # The lock check matters because install-beeper-linux.sh writes
    # config.yaml partway through (during `bbctl config`), then keeps
    # going with the iMessage login. If we started the bridge as soon as
    # config appeared, the bridge would grab the DB lock mid-login and
    # the login step would fail. cmd_setup writes SETUP_LOCK before
    # invoking the install script and removes it after.
    local warned=0
    while [ ! -f "$CONFIG" ] || [ -f "$SETUP_LOCK" ]; do
        if [ "$warned" -eq 0 ]; then
            echo "no /data/config.yaml yet — run 'docker exec -it bridge imessage-setup' from the host to configure the bridge."
            warned=1
        fi
        sleep 30
    done
    # cd into /data so anisette's relative state/anisette/ lands on the
    # volume rather than wherever PID 1 happened to start. WORKDIR in
    # the Dockerfile already does this; the cd is defensive.
    cd "$DATA_DIR"
    exec "$BIN" -c "$CONFIG"
}

# ── Alias-block helpers ──────────────────────────────────────────────────────
# Marker format matches the bare-Linux install scripts so the same block
# is recognized cross-runtime — a user who switches between bare-Linux
# and Docker doesn't end up with duplicate blocks.
ALIAS_MARKER_START="# >>> mautrix-imessage shortcuts (managed) >>>"
ALIAS_MARKER_END="# <<< mautrix-imessage shortcuts (managed) <<<"

# alias_block_contents — print the Docker-flavored alias body (no markers).
# Kept as a function so the same source feeds the install path AND the
# manual `aliases` subcommand.
alias_block_contents() {
    cat <<'EOF'
alias imessage-setup='docker exec -it bridge imessage-setup'
alias imessage-logs='docker logs -f bridge'
EOF
}

# strip_alias_block — remove the managed-alias block from one rc file.
# Returns 0 if anything was stripped, 1 otherwise. Idempotent.
strip_alias_block() {
    local rc="$1"
    if [ -f "$rc" ] && grep -qF "$ALIAS_MARKER_START" "$rc"; then
        awk -v s="$ALIAS_MARKER_START" -v e="$ALIAS_MARKER_END" '
            $0 == s { skip = 1; next }
            $0 == e { skip = 0; next }
            !skip   { print }
        ' "$rc" > "$rc.tmp" && mv "$rc.tmp" "$rc"
        return 0
    fi
    return 1
}

# install_alias_block — write the managed-alias block to one rc file.
# Strips first so re-runs replace the block in place (matches the
# bare-Linux install scripts' idempotent re-run pattern). Returns 0
# on write, 1 if the rc file doesn't exist as a bind mount.
install_alias_block() {
    local rc="$1"
    [ -e "$rc" ] || return 1
    strip_alias_block "$rc" || true
    {
        echo "$ALIAS_MARKER_START"
        alias_block_contents
        echo "$ALIAS_MARKER_END"
    } >> "$rc"
    return 0
}

# manage_aliases — called from cmd_run before the bridge starts. Reads
# EMIT_ALIASES and reconciles the alias block in any bind-mounted rc
# files. Shell detection is by presence of the mount, not by $SHELL —
# the container's $SHELL is the container's (always /bin/bash), not
# the host's. If the user has both bash and zsh mounted we manage both.
manage_aliases() {
    case "${EMIT_ALIASES:-}" in
        true|TRUE|1|yes|YES)    local want=install ;;
        false|FALSE|0|no|NO)    local want=remove ;;
        "")                     return 0 ;;
        *)                      echo "EMIT_ALIASES has unknown value '${EMIT_ALIASES}' — skipping alias management" >&2; return 0 ;;
    esac

    local touched=0 mounted=0
    for rc in /host/.bashrc /host/.zshrc; do
        [ -e "$rc" ] || continue
        mounted=1
        if [ "$want" = install ]; then
            if install_alias_block "$rc"; then
                echo "✓ Installed mautrix-imessage shortcuts in $(echo "$rc" | sed 's|^/host|~|')"
                touched=1
            fi
        else
            if strip_alias_block "$rc"; then
                echo "✓ Removed mautrix-imessage shortcuts from $(echo "$rc" | sed 's|^/host|~|')"
                touched=1
            fi
        fi
    done

    if [ "$mounted" -eq 0 ]; then
        cat >&2 <<'EOF'
EMIT_ALIASES is set, but neither ~/.bashrc nor ~/.zshrc is bind-mounted
into the container. To let the container install/remove the shortcut
aliases automatically, add to docker-compose.yml under the bridge
service's volumes:

    - ~/.bashrc:/host/.bashrc
    - ~/.zshrc:/host/.zshrc

Open a new terminal (or `source` the rc file) after EMIT_ALIASES
changes to pick up the new state.
EOF
    elif [ "$touched" -eq 1 ]; then
        echo "  (Open a new terminal or 'source' the rc file to pick up the change.)"
    fi
}

cmd_migrate() {
    echo ""
    echo "═══════════════════════════════════════════════"
    echo "  Migrating from bare-Linux install"
    echo "═══════════════════════════════════════════════"
    echo ""
    echo "Detected existing state at /data (bind-mounted from your host's"
    echo "~/.local/share/mautrix-imessage). The bridge will resume against"
    echo "it — no re-login, no re-setup, no state copy."
    echo ""

    # ── 1. Bare-Linux shell aliases (systemctl-based) ───────────────────────
    # If the user bind-mounted their rc files into /host/, we strip the
    # marker block automatically. Otherwise we print the awk command for
    # them to run on the host. Detect both shells — bash and zsh — and
    # handle whichever rc files exist. No HOST_SHELL env var needed:
    # presence of the rc file in /host/ IS the signal.
    local stripped=0
    local rc_unmounted=0
    for shell_rc in /host/.bashrc /host/.zshrc; do
        if [ -e "$shell_rc" ]; then
            if strip_alias_block "$shell_rc"; then
                echo "✓ Stripped managed-alias block from $(echo "$shell_rc" | sed 's|^/host|~|')"
                stripped=1
            fi
        fi
    done

    if [ "$stripped" -eq 0 ] && [ ! -e /host/.bashrc ] && [ ! -e /host/.zshrc ]; then
        rc_unmounted=1
        cat <<'EOF'
Note: ~/.bashrc and ~/.zshrc are not bind-mounted into the container.
To let this script strip the old systemd-based aliases automatically,
add to your docker-compose.yml volumes (only needed during migration):

    - ~/.bashrc:/host/.bashrc
    - ~/.zshrc:/host/.zshrc

Otherwise, run this on the host yourself to remove them:

    for rc in ~/.bashrc ~/.zshrc; do
        [ -f "$rc" ] || continue
        awk -v s='# >>> mautrix-imessage shortcuts (managed) >>>' \
            -v e='# <<< mautrix-imessage shortcuts (managed) <<<' '
            $0 == s { skip = 1; next }
            $0 == e { skip = 0; next }
            !skip   { print }
        ' "$rc" > "$rc.tmp" && mv "$rc.tmp" "$rc"
    done

EOF
    fi

    # ── 2. systemd unit teardown (host-side, can't reach from container) ────
    cat <<'EOF'
Stop and remove the bare-Linux systemd service on the HOST (the
container can't reach the host's systemd from inside). Run on the host:

    # systemd --user (most setups)
    systemctl --user stop mautrix-imessage 2>/dev/null
    systemctl --user disable mautrix-imessage 2>/dev/null
    rm -f ~/.config/systemd/user/mautrix-imessage.service
    systemctl --user daemon-reload

    # systemd system (only if you installed as root)
    sudo systemctl stop mautrix-imessage 2>/dev/null
    sudo systemctl disable mautrix-imessage 2>/dev/null
    sudo rm -f /etc/systemd/system/mautrix-imessage.service
    sudo systemctl daemon-reload

EOF

    if [ -t 0 ]; then
        # Interactive — pause so the user can do host-side cleanup before
        # the bridge starts (otherwise the old systemd unit could race for
        # the same DB lock + appservice port).
        read -r -p "Press Enter once host-side cleanup is done (or Ctrl-C to abort): "
    else
        echo "(non-interactive run — proceeding without pause; do the host-side cleanup at your earliest convenience)"
    fi

    # ── 3. Mark migration done so subsequent restarts skip this ─────────────
    : > "$MIGRATION_DONE"
    echo ""
    echo "✓ Migration complete. Starting bridge..."
    echo ""

    if [ "$rc_unmounted" -eq 1 ]; then
        echo "Reminder: rc-file mounts can stay commented now that migration's done."
        echo ""
    fi
}

cmd_setup() {
    require_tty "setup"
    # Path selection lives in docker-compose.yml as BEEPER=true|false.
    # `imessage-setup` always re-runs whichever script that env var picks,
    # so re-config on a Beeper deploy stays on the Beeper script and
    # re-config on a self-hosted deploy stays on the self-hosted script —
    # no choose-your-own-adventure inside the container, no state file.

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

Set BEEPER in your docker-compose.yml under the bridge service:

  environment:
    BEEPER: "true"     # for Beeper deploys
    BEEPER: "false"    # for self-hosted homeservers

Then 'docker compose up -d' and re-run 'docker exec -it bridge imessage-setup'.
EOF
            exit 2
            ;;
        *)
            echo "error: BEEPER must be 'true' or 'false' (got: '${BEEPER}')" >&2
            exit 2
            ;;
    esac

    # Take the setup lock so cmd_run (PID 1) doesn't race-start the bridge
    # when bbctl writes config.yaml partway through the script. The trap
    # clears the lock no matter how we exit (clean, error, or signal).
    : > "$SETUP_LOCK"
    trap 'rm -f "$SETUP_LOCK"' EXIT

    export IN_DOCKER=1
    cd "$DATA_DIR"
    # `if` form so `set -e` doesn't kill us before we can capture rc.
    local rc=0
    if ! "$script" "${script_args[@]}"; then
        rc=$?
    fi

    # On clean completion, also mark migration as "done" so that a stray
    # MIGRATE=true left in compose on a Docker-native install doesn't
    # trigger a spurious migration prompt on next restart. Migration is
    # only meaningful when state existed before Docker owned this volume.
    if [ "$rc" -eq 0 ]; then
        : > "$MIGRATION_DONE"
    fi

    exit "$rc"
}

cmd_login() {
    require_tty "login"
    if [ ! -f "$CONFIG" ]; then
        echo "error: no /data/config.yaml found. Run setup first." >&2
        exit 1
    fi
    cd "$DATA_DIR"
    exec "$BIN" login -c "$CONFIG"
}

cmd_aliases() {
    # Print the marker-wrapped block to stdout for manual paste. Useful
    # when the user doesn't want to bind-mount their rc files into the
    # container — they can pipe the output into their rc file once and
    # manage it themselves. Same block content the startup-side
    # install_alias_block writes, so the two paths can't drift.
    echo "$ALIAS_MARKER_START"
    alias_block_contents
    echo "$ALIAS_MARKER_END"
}

case "$CMD" in
    run)      cmd_run "$@" ;;
    setup)    cmd_setup "$@" ;;
    login)    cmd_login "$@" ;;
    migrate)  cmd_migrate "$@" ;;
    aliases)  cmd_aliases "$@" ;;
    help|-h|--help) usage ;;
    *)
        # Fall through: allow `docker run image <arbitrary command>` to work
        # for diagnostics (e.g. `docker run --rm image bash`). This is
        # standard entrypoint behavior — bypass the dispatcher when the
        # first arg isn't one of our subcommands.
        exec "$CMD" "$@"
        ;;
esac
