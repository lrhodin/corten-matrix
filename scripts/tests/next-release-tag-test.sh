#!/usr/bin/env bash
# Behavioral tests for semantic release-tag increments.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HELPER="$ROOT/scripts/next-release-tag.sh"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

expect() {
  local version="$1" bump="$2" expected="$3" actual
  actual="$("$HELPER" "$version" "$bump")" || fail "$version $bump returned failure"
  [ "$actual" = "$expected" ] || fail "$version $bump: got $actual, expected $expected"
}

[ -x "$HELPER" ] || fail 'missing executable version-resolution helper'
expect 1.2.2 patch 1.2.3
expect v1.2.2 patch 1.2.3
expect 1.2.2 minor 1.3.0
expect 1.2.2 major 2.0.0
expect 0.9.999 patch 0.9.1000
expect 0.999.999 minor 0.1000.0
expect 999.999.999 major 1000.0.0
expect 9223372036854775807.0.0 major 9223372036854775808.0.0

if "$HELPER" 1.2 invalid >/dev/null 2>&1; then
  fail 'malformed version must fail'
fi
if "$HELPER" 1.02.3 patch >/dev/null 2>&1; then
  fail 'leading-zero version must fail'
fi
if "$HELPER" 1.2.3 prerelease >/dev/null 2>&1; then
  fail 'unsupported bump must fail'
fi

printf 'PASS: semantic release-tag resolution\n'
