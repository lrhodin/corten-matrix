#!/usr/bin/env bash
# Static safety contract for the public release workflow.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
WORKFLOW="$ROOT/.github/workflows/build-release-binaries.yml"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

require() {
  grep -Fq -- "$2" "$1" || fail "missing workflow safety contract: $2"
}

require "$WORKFLOW" 'Lipo, harden, and sign macOS binary'
require "$WORKFLOW" './build-macos-universal.sh > "$RUNNER_TEMP/opencider-build.log" 2>&1'
require "$WORKFLOW" 'Private build failed. Detailed compiler output was intentionally withheld from public Actions logs.'
require "$WORKFLOW" 'BUILD_SAFE_STAGE_FILE: /runner-temp/opencider-linux-amd64-stage'
require "$WORKFLOW" 'BUILD_SAFE_STAGE_FILE: /runner-temp/opencider-linux-arm64-stage'
require "$WORKFLOW" '--volume "$RUNNER_TEMP:/runner-temp"'
require "$WORKFLOW" 'Private build failed at source-safe stage:'

ruby -e 'require "yaml"; YAML.load_file(ARGV.fetch(0))' "$WORKFLOW" \
  || fail 'workflow YAML does not parse'
printf 'PASS: public release workflow safety contract\n'
