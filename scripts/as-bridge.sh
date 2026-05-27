#!/bin/sh
# ============================================================================
# as-bridge — drop to PUID:PGID and exec the requested command.
#
# Used by `docker exec` paths from the host wrapper (`imessage setup`,
# `imessage login`, `imessage shell`, `imessage bbctl …`). `docker exec`
# inherits the container's USER, which is root for this image because
# PID 1 enters the entrypoint root-privileged to chown bind mounts and
# create host-path symlinks before setpriv-dropping to PUID:PGID.
#
# Without this wrapper, every host-side exec would run as root inside
# the container and silently write root-owned files into the bind mounts.
# ============================================================================
set -e

PUID="${PUID:-1000}"
PGID="${PGID:-1000}"
export HOME=/home/bridge

# --clear-groups (not --init-groups): the latter fails for UIDs that don't
# have an /etc/passwd entry (UNRAID, TrueNAS). Mirrors the entrypoint's drop.
exec setpriv --reuid "$PUID" --regid "$PGID" --clear-groups -- "$@"
