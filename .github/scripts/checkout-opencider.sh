#!/usr/bin/env bash
# Copyright (c) 2026 Ludvig Rhodin. All rights reserved.
# Quietly fetch one verified OpenCider revision without exposing private Git data.
set -euo pipefail

fail() {
  printf '::error::Private checkout failed. Detailed Git output was intentionally withheld from public Actions logs.\n' >&2
  exit 1
}

[ "$#" -eq 2 ] || fail
destination="$1"
requested_ref_token="$2"
case "$destination" in
  ''|/*|*'..'*) fail ;;
esac
case "$requested_ref_token" in
  master) ;;
  *) [[ "$requested_ref_token" =~ ^[0-9a-f]{64}$ ]] || fail ;;
esac
[ -n "${OPENCIDER_READ_DEPLOY_KEY:-}" ] || fail
[ -n "${RUNNER_TEMP:-}" ] || fail
[ ! -e "$destination" ] || fail
for command in git ssh python3 mktemp rm; do
  command -v "$command" >/dev/null 2>&1 || fail
done

umask 077
key_file="$(mktemp "$RUNNER_TEMP/opencider-key.XXXXXX")"
known_hosts="$(mktemp "$RUNNER_TEMP/opencider-known-hosts.XXXXXX")"
checkout_log="$(mktemp "$RUNNER_TEMP/opencider-checkout.log.XXXXXX")"
revisions_file="$(mktemp "$RUNNER_TEMP/opencider-revisions.XXXXXX")"
cleanup() {
  rm -f "$key_file" "$known_hosts" "$checkout_log" "$revisions_file"
}
trap cleanup EXIT

printf '%s\n' "$OPENCIDER_READ_DEPLOY_KEY" > "$key_file"
unset OPENCIDER_READ_DEPLOY_KEY
printf '%s\n' \
  'github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl' \
  > "$known_hosts"
export GIT_SSH_COMMAND="ssh -i $key_file -o IdentitiesOnly=yes -o HostKeyAlgorithms=ssh-ed25519 -o StrictHostKeyChecking=yes -o UserKnownHostsFile=$known_hosts"

perform_checkout() {
  local selected_ref actual_ref
  mkdir -p "$destination" || return
  git -C "$destination" init -q || return
  git -C "$destination" remote add origin git@github.com:mackid1993/OpenCider.git || return
  git -C "$destination" fetch --quiet --no-tags origin refs/heads/master || return
  if [ "$requested_ref_token" = master ]; then
    selected_ref=FETCH_HEAD
  else
    git -C "$destination" rev-list --all > "$revisions_file" 2>/dev/null || return
    selected_ref="$(python3 - "$key_file" "$revisions_file" "$requested_ref_token" <<'PY'
import hashlib
import hmac
from pathlib import Path
import re
import sys

key = Path(sys.argv[1]).read_bytes().rstrip(b"\r\n") + b"\n"
revisions = Path(sys.argv[2]).read_bytes().splitlines()
token = sys.argv[3]
matches = []
for revision in revisions:
    if not re.fullmatch(rb"[0-9a-f]{40}", revision):
        raise SystemExit(1)
    message = b"opencider-ref-v1:" + revision
    candidate = hmac.new(key, message, hashlib.sha256).hexdigest()
    if hmac.compare_digest(candidate, token):
        matches.append(revision.decode("ascii"))
if len(matches) != 1:
    raise SystemExit(1)
sys.stdout.write(matches[0])
PY
)" || return
    [[ "$selected_ref" =~ ^[0-9a-f]{40}$ ]] || return
  fi
  git -C "$destination" checkout --quiet --detach "$selected_ref" || return
  actual_ref="$(git -C "$destination" rev-parse HEAD)" || return
  [[ "$actual_ref" =~ ^[0-9a-f]{40}$ ]] || return
  [ "$requested_ref_token" = master ] || [ "$actual_ref" = "$selected_ref" ] || return
}

if ! perform_checkout > "$checkout_log" 2>&1; then
  fail
fi
