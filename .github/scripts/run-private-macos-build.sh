#!/usr/bin/env bash
# Copyright (c) 2026 Ludvig Rhodin. All rights reserved.
# Run one private macOS build while exposing only fixed source-safe stage names.
set -euo pipefail

fail() {
  printf '%s\n' '::error::Source-safe macOS build monitor failed.' >&2
  exit 1
}

[ "$#" -eq 1 ] || fail
arch="$1"
case "$arch" in arm64|amd64) ;; *) fail ;; esac
[ -n "${RUNNER_TEMP:-}" ] || fail
command -v python3 >/dev/null 2>&1 || fail
interval="${MAC_BUILD_HEARTBEAT_SECONDS:-30}"
every="${MAC_BUILD_HEARTBEAT_EVERY:-10}"
case "$interval" in [1-9]|[1-9][0-9]*) ;; *) fail ;; esac
case "$every" in [1-9]|[1-9][0-9]*) ;; *) fail ;; esac

status_file="$RUNNER_TEMP/opencider-macos-$arch-stage"
build_log="$RUNNER_TEMP/opencider-macos-$arch-build.log"
launcher_pid=''
cleanup() {
  rm -f "$status_file" "$build_log"
}
cancel_build() {
  trap - INT TERM HUP
  if [ -n "$launcher_pid" ] && kill -0 "$launcher_pid" 2>/dev/null; then
    kill -TERM "$launcher_pid" 2>/dev/null || true
    wait "$launcher_pid" 2>/dev/null || true
  fi
  cleanup
  exit 143
}
trap cleanup EXIT
trap cancel_build INT TERM HUP
rm -f "$status_file" "$build_log"
export BUILD_SAFE_STAGE_FILE="$status_file"

safe_status() {
  local status=unknown
  if [ -f "$status_file" ]; then
    status="$(<"$status_file")"
  fi
  case "$status" in
    bootstrap|glibc|sync-public-bridge|clone-public-rustpush|checkout-public-rustpush|fairplay-stubs|fetch-public-submodules|validate-public-submodules|overlay-private-rust|apply-rustpush-patches|cargo-build|overlay-private-go|go-build|harden|notices|complete)
      printf '%s\n' "$status"
      ;;
    *)
      printf '%s\n' unknown
      ;;
  esac
}

python3 - "$arch" "$build_log" <<'PY' &
import os
from pathlib import Path
import signal
import subprocess
import sys

arch = sys.argv[1]
log_path = Path(sys.argv[2])
with log_path.open("wb") as log:
    child = subprocess.Popen(
        ["./build-macos-slice.sh", arch],
        stdout=log,
        stderr=subprocess.STDOUT,
        start_new_session=True,
    )

    def forward(signum, _frame):
        try:
            os.killpg(child.pid, signum)
        except ProcessLookupError:
            pass

    for handled in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        signal.signal(handled, forward)
    raise SystemExit(child.wait())
PY
launcher_pid=$!
last_status=''
ticks=0
while kill -0 "$launcher_pid" 2>/dev/null; do
  status="$(safe_status)"
  if [ "$status" != "$last_status" ] || [ $((ticks % every)) -eq 0 ]; then
    printf 'macOS %s build stage: %s\n' "$arch" "$status"
    last_status="$status"
  fi
  sleep "$interval"
  ticks=$((ticks + 1))
done

set +e
wait "$launcher_pid"
build_status=$?
set -e
launcher_pid=''
status="$(safe_status)"
if [ "$status" != "$last_status" ]; then
  printf 'macOS %s build stage: %s\n' "$arch" "$status"
fi
if [ "$build_status" -ne 0 ]; then
  printf '::error::Private macOS build failed at source-safe stage: %s. Detailed compiler output was intentionally withheld from public Actions logs.\n' "$status" >&2
  exit 1
fi
[ "$status" = complete ] || fail
