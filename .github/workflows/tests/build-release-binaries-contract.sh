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

require "$WORKFLOW" 'build_scope:'
require "$WORKFLOW" 'description: Build selection'
require "$WORKFLOW" 'default: All platforms'
require "$WORKFLOW" '      - Mac only'
require "$WORKFLOW" '      - Linux AMD64 only'
require "$WORKFLOW" '      - Linux ARM64 only'
require "$WORKFLOW" '      - Linux both'
require "$WORKFLOW" '      - All platforms'
require "$WORKFLOW" "inputs.build_scope == 'Mac only'"
require "$WORKFLOW" "inputs.build_scope == 'Linux AMD64 only'"
require "$WORKFLOW" "inputs.build_scope == 'Linux ARM64 only'"
require "$WORKFLOW" "inputs.build_scope == 'Linux both'"
require "$WORKFLOW" "inputs.build_scope == 'All platforms'"
require "$WORKFLOW" 'BUILD_SCOPE: ${{ inputs.build_scope }}'
require "$WORKFLOW" "if [ \"\$BUILD_SCOPE\" != 'All platforms' ]; then"
require "$WORKFLOW" 'No release tag will be reserved and no draft release will be created.'
require "$WORKFLOW" 'Upload universal macOS binary'
require "$WORKFLOW" 'RELEASE_TITLE: ${{ inputs.release_title }}'
require "$WORKFLOW" "Draft release title is required for All platforms."
python3 - "$WORKFLOW" <<'PY'
from pathlib import Path
import sys
workflow = Path(sys.argv[1]).read_text()
if workflow.index('Draft release title is required for All platforms.') > workflow.index('LATEST_TAG="$(gh api'):
    raise SystemExit('release title must be validated before resolving/reserving a release tag')
PY
require "$WORKFLOW" './build-macos-universal.sh > "$RUNNER_TEMP/opencider-build.log" 2>&1'
require "$WORKFLOW" 'Private build failed. Detailed compiler output was intentionally withheld from public Actions logs.'
require "$WORKFLOW" 'BUILD_SAFE_STAGE_FILE: /runner-temp/opencider-linux-amd64-stage'
require "$WORKFLOW" 'BUILD_SAFE_STAGE_FILE: /runner-temp/opencider-linux-arm64-stage'
require "$WORKFLOW" '--volume "$RUNNER_TEMP:/runner-temp"'
require "$WORKFLOW" 'Private build failed at source-safe stage:'
require "$WORKFLOW" 'sync-public-bridge|clone-public-rustpush|checkout-public-rustpush|fairplay-stubs|fetch-public-submodules|validate-public-submodules|overlay-private-rust|apply-rustpush-patches'

ruby -e 'require "yaml"; YAML.load_file(ARGV.fetch(0))' "$WORKFLOW" \
  || fail 'workflow YAML does not parse'
printf 'PASS: public release workflow safety contract\n'
