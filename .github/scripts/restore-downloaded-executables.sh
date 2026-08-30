#!/usr/bin/env bash
# Copyright (c) 2026 Ludvig Rhodin. All rights reserved.
# Restore executable modes that actions/download-artifact normalizes to 0644.
set -euo pipefail

fail() {
  printf '%s\n' 'downloaded artifact validation failed' >&2
  exit 1
}

[ "$#" -eq 2 ] || fail
scope="$1"
dist="$2"
case "$scope" in
  mac|all) ;;
  *) fail ;;
esac
[ -d "$dist" ] || fail
[ ! -L "$dist" ] || fail

expected=(
  corten-matrix-macos.amd64
  corten-matrix-macos.arm64
)
if [ "$scope" = all ]; then
  expected+=(
    corten-matrix-linux-amd64
    corten-matrix-linux-arm64
  )
fi

shopt -s nullglob dotglob
entries=("$dist"/*)
[ "${#entries[@]}" -eq "${#expected[@]}" ] || fail

files=()
for name in "${expected[@]}"; do
  file="$dist/$name"
  [ -f "$file" ] || fail
  [ ! -L "$file" ] || fail
  files+=("$file")
done
for entry in "${entries[@]}"; do
  name="${entry##*/}"
  allowed=0
  for expected_name in "${expected[@]}"; do
    if [ "$name" = "$expected_name" ]; then
      allowed=1
      break
    fi
  done
  [ "$allowed" -eq 1 ] || fail
done

chmod 0755 "${files[@]}" || fail
for file in "${files[@]}"; do
  [ -x "$file" ] || fail
done
