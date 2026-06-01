#!/bin/sh
# ============================================================================
# healthcheck — Docker HEALTHCHECK probe for the mautrix-imessage-v2 bridge.
#
# cmd_run (docker-entrypoint.sh) supervises the bridge as a child, so it isn't
# necessarily PID 1. We scan every PID for the bridge binary (via /proc exe and
# comm — see the loop below) rather than probing a listening port, because the
# Beeper path uses appservice-websocket (no inbound port) while self-hosted
# listens on 29325 — the process check works for both and needs no extra tooling.
#
#   * PID 1 is the bridge binary            -> healthy (exit 0)
#   * setup in progress (.setup-in-progress) -> healthy AND silent: `imessage
#                                              setup` legitimately holds PID 1
#                                              off the bridge. See the race
#                                              note below — this probe runs as
#                                              root and must not touch the log
#                                              file setup's PUID process owns.
#   * config.yaml not written yet           -> unhealthy, but QUIET: the
#                                              container is legitimately idle
#                                              waiting for `imessage setup`, so
#                                              don't clutter the log.
#   * config exists but PID 1 is the shell  -> unhealthy AND log it: the bridge
#                                              should be running but isn't (e.g.
#                                              the root-prelude privilege-drop
#                                              loop when PUID=0). We append a
#                                              line to the same file `imessage
#                                              logs` tails, so the failure is
#                                              visible instead of the container
#                                              sitting at "Up" with a silent log.
# ============================================================================
set -eu

LOG=/data/logs/bridge.log
CONFIG=/data/config.yaml
SETUP_LOCK=/data/.setup-in-progress

# Healthy: the bridge binary is running. cmd_run supervises it as a CHILD (so a
# fast-failing bridge can't crash-loop the container out of `docker exec` reach),
# so it is usually not PID 1 — match the binary across all PIDs.
#
# Two probes, because each covers a gap the other leaves:
#   * /proc/<pid>/exe is precise (full path, no truncation), BUT following that
#     symlink for a process owned by another uid needs ptrace access. Docker
#     drops CAP_SYS_PTRACE by default, and the entrypoint setpriv-drops the
#     bridge to PUID (default 1000) while this healthcheck runs as root — so a
#     root readlink() of the PUID bridge's exe returns EACCES and the match
#     silently fails. That is the "bridge not running but it is" false negative
#     seen after migrating from baremetal to Docker.
#   * /proc/<pid>/comm is the process name read straight from the task struct;
#     it is readable across uids WITHOUT ptrace, so it works for any PUID. The
#     kernel truncates comm to 15 chars: mautrix-imessage-v2 -> mautrix-imessag.
# comm can't false-positive on an argument either (it is the process name, not
# the cmdline), so checking both is strictly safer than exe alone.
for d in /proc/[0-9]*; do
    case "$(readlink "$d/exe" 2>/dev/null)" in
        */mautrix-imessage-v2) exit 0 ;;
    esac
    case "$(cat "$d/comm" 2>/dev/null || true)" in
        mautrix-imessag*) exit 0 ;;
    esac
done

# A genuine setup is running ONLY if the lock is held AND its owning process is
# still alive (cmd_setup records its PID; setup runs as a `docker exec` child in
# PID 1's namespace, so it's visible via /proc). During a genuine setup the
# bridge is legitimately not PID 1, and that PUID process owns and rotates
# /data/logs/bridge.log on init-db/login — this probe runs as ROOT, so writing
# the log line below would race it (a root-created/rotated bridge.log can't be
# written by the PUID bridge: EACCES). So while a live setup holds the lock,
# report healthy and touch NOTHING (this also keeps a multi-minute interactive
# setup from flapping to "unhealthy"). A lock whose owner is DEAD is stale (an
# interrupted setup) — fall through so the wedge is surfaced in the log and
# health status rather than masked; cmd_run clears the stale lock and starts up.
if [ -f "$SETUP_LOCK" ]; then
    pid=$(cat "$SETUP_LOCK" 2>/dev/null || true)
    if [ -n "$pid" ] && [ -d "/proc/$pid" ]; then
        exit 0
    fi
fi

# Not running yet and never configured — nothing is wrong, stay quiet.
[ -f "$CONFIG" ] || exit 1

# Configured but the bridge isn't the running process. Surface it in the log
# (JSON line matching the bridge's file writer so log parsers don't choke) —
# but only APPEND to an already-existing file. NEVER create it: a root-created
# bridge.log can't be written/rotated by the PUID bridge (EACCES). During the
# bridge's own startup log-rotation the file is briefly absent; skipping that
# sub-second window just defers this line to the next 30s probe.
if [ -f "$LOG" ]; then
    printf '{"time":"%s","level":"error","logger":"healthcheck","message":"bridge process is not running (PID 1 is not mautrix-imessage-v2) — container is Up but the bridge never started; check PUID/PGID and the entrypoint privilege-drop loop"}\n' \
        "$(date -Is 2>/dev/null || date)" >> "$LOG" 2>/dev/null || true
fi
exit 1
