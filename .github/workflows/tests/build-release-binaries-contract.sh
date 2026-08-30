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

PUBLIC_COMMIT_GUARD="$ROOT/.githooks/pre-commit"
[ -x "$PUBLIC_COMMIT_GUARD" ] || fail 'missing executable public private-path commit guard'
require "$PUBLIC_COMMIT_GUARD" '^opencider/'
require "$PUBLIC_COMMIT_GUARD" '^private-go/'
require "$PUBLIC_COMMIT_GUARD" '^rustpush/apple-private-apis/'
require "$PUBLIC_COMMIT_GUARD" '^third_party/rustpush-upstream/'
PUBLIC_IGNORE="$ROOT/.gitignore"
require "$PUBLIC_IGNORE" '/opencider/'
require "$PUBLIC_IGNORE" '/private-go/'
require "$PUBLIC_IGNORE" '/rustpush/apple-private-apis/'

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
import re
import sys
workflow = Path(sys.argv[1]).read_text()
trigger_block = workflow.split('on:\n', 1)[1].split('\npermissions:', 1)[0]
triggers = re.findall(r'^  ([A-Za-z_][A-Za-z0-9_]*):', trigger_block, re.MULTILINE)
if triggers != ['workflow_dispatch']:
    raise SystemExit(f'workflow_dispatch must be the sole trigger, found {triggers!r}')
if workflow.index('Draft release title is required for All platforms.') > workflow.index('LATEST_TAG="$(gh api'):
    raise SystemExit('release title must be validated before resolving/reserving a release tag')
PY
require "$WORKFLOW" '  verify-rustpush-patches:'
require "$WORKFLOW" 'needs: resolve-release-version'
require "$WORKFLOW" 'opencider_ref_token: ${{ steps.opencider.outputs.ref_token }}'
require "$WORKFLOW" '[[ "$sha" =~ ^[0-9a-f]{40}$ ]]'
if grep -Eq -- 'opencider_sha|OPENCIDER_REF:' "$WORKFLOW"; then
  fail 'plaintext private OpenCider revision metadata must not enter rendered workflow fields'
fi
require "$WORKFLOW" '(( index <= total ))'
if grep -Fq -- 'repository: mackid1993/OpenCider' "$WORKFLOW"; then
  fail 'actions/checkout must not print private revision metadata in public logs'
fi
if grep -Fq -- 'ssh-key: ${{ secrets.OPENCIDER_READ_DEPLOY_KEY }}' "$WORKFLOW"; then
  fail 'the deploy key must be confined to the source-safe checkout helper'
fi
PRIVATE_CHECKOUT="$ROOT/.github/scripts/checkout-opencider.sh"
[ -x "$PRIVATE_CHECKOUT" ] || fail 'missing executable source-safe private checkout helper'
RESTORE_EXECUTABLES="$ROOT/.github/scripts/restore-downloaded-executables.sh"
[ -x "$RESTORE_EXECUTABLES" ] || fail 'missing executable downloaded-artifact mode restoration helper'
MAC_BUILD_MONITOR="$ROOT/.github/scripts/run-private-macos-build.sh"
[ -x "$MAC_BUILD_MONITOR" ] || fail 'missing executable source-safe macOS build monitor'
require "$WORKFLOW" '../.github/scripts/run-private-macos-build.sh amd64'
require "$WORKFLOW" '../.github/scripts/run-private-macos-build.sh arm64'
mac_timeout_count="$(grep -Fc -- 'timeout-minutes: 60' "$WORKFLOW")"
[ "$mac_timeout_count" -ge 3 ] || fail "both Mac slices and finalizer require 60-minute job caps (found $mac_timeout_count)"
require "$RESTORE_EXECUTABLES" 'chmod 0755'
require "$WORKFLOW" './.github/scripts/restore-downloaded-executables.sh "$restore_scope" opencider/dist'
require "$PRIVATE_CHECKOUT" 'github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl'
require "$PRIVATE_CHECKOUT" 'fetch --quiet --no-tags origin refs/heads/master'
if grep -Eq -- '--filter=|--depth=' "$PRIVATE_CHECKOUT"; then
  fail 'private checkout must include complete history and blobs for OpenAbsinthe artifact fingerprinting'
fi
require "$PRIVATE_CHECKOUT" 'checkout --quiet --detach'
require "$PRIVATE_CHECKOUT" '> "$checkout_log" 2>&1'
require "$PRIVATE_CHECKOUT" 'rm -f "$key_file" "$known_hosts" "$checkout_log" "$revisions_file"'
require "$PRIVATE_CHECKOUT" 'unset OPENCIDER_READ_DEPLOY_KEY'
require "$PRIVATE_CHECKOUT" 'opencider-ref-v1:'
require "$PRIVATE_CHECKOUT" 'hmac.compare_digest'
require "$PRIVATE_CHECKOUT" 'Private checkout failed. Detailed Git output was intentionally withheld from public Actions logs.'
checkout_tmp="$(mktemp -d)"
trap 'rm -rf "$checkout_tmp"' EXIT
mkdir -p "$checkout_tmp/bin" "$checkout_tmp/runner"

mode_mac="$checkout_tmp/mode-mac"
mkdir -p "$mode_mac"
printf 'mac-amd64\n' > "$mode_mac/corten-matrix-macos.amd64"
printf 'mac-arm64\n' > "$mode_mac/corten-matrix-macos.arm64"
chmod 0644 "$mode_mac"/*
"$RESTORE_EXECUTABLES" mac "$mode_mac" >/dev/null 2>&1 \
  || fail 'mode helper rejected exact Mac-only downloaded artifacts'
for binary in "$mode_mac/corten-matrix-macos.amd64" "$mode_mac/corten-matrix-macos.arm64"; do
  [ -x "$binary" ] || fail 'mode helper did not restore a Mac slice to executable'
done
printf 'unexpected\n' > "$mode_mac/unexpected-private-output"
if "$RESTORE_EXECUTABLES" mac "$mode_mac" >/dev/null 2>&1; then
  fail 'mode helper accepted an unexpected artifact-boundary file'
fi
rm -f "$mode_mac/unexpected-private-output"
rm -f "$mode_mac/corten-matrix-macos.arm64"
ln -s "$mode_mac/corten-matrix-macos.amd64" "$mode_mac/corten-matrix-macos.arm64"
if "$RESTORE_EXECUTABLES" mac "$mode_mac" >/dev/null 2>&1; then
  fail 'mode helper accepted a symlinked artifact-boundary file'
fi

mode_all="$checkout_tmp/mode-all"
mkdir -p "$mode_all"
for name in corten-matrix-macos.amd64 corten-matrix-macos.arm64 \
  corten-matrix-linux-amd64 corten-matrix-linux-arm64; do
  printf '%s\n' "$name" > "$mode_all/$name"
done
chmod 0644 "$mode_all"/*
"$RESTORE_EXECUTABLES" all "$mode_all" >/dev/null 2>&1 \
  || fail 'mode helper rejected exact all-platform downloaded artifacts'
for binary in "$mode_all"/*; do
  [ -x "$binary" ] || fail 'mode helper did not restore an all-platform artifact to executable'
done

monitor_dir="$checkout_tmp/mac-monitor"
monitor_runner="$checkout_tmp/mac-monitor-runner"
mkdir -p "$monitor_dir" "$monitor_runner"
cat > "$monitor_dir/build-macos-slice.sh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' cargo-build > "$BUILD_SAFE_STAGE_FILE"
printf '%s\n' 'PRIVATE COMPILER OUTPUT MUST STAY HIDDEN' >&2
sleep 2
printf '%s\n' complete > "$BUILD_SAFE_STAGE_FILE"
EOF
chmod +x "$monitor_dir/build-macos-slice.sh"
monitor_output="$checkout_tmp/mac-monitor-output"
(
  cd "$monitor_dir"
  RUNNER_TEMP="$monitor_runner" MAC_BUILD_HEARTBEAT_SECONDS=1 \
    MAC_BUILD_HEARTBEAT_EVERY=1 "$MAC_BUILD_MONITOR" arm64
) > "$monitor_output" 2>&1 || fail 'macOS build monitor rejected a successful private build'
require "$monitor_output" 'macOS arm64 build stage: cargo-build'
require "$monitor_output" 'macOS arm64 build stage: complete'
if grep -Fq 'PRIVATE COMPILER OUTPUT' "$monitor_output"; then
  fail 'macOS build monitor exposed private build output on success'
fi
if compgen -G "$monitor_runner/opencider-macos-*" >/dev/null; then
  fail 'macOS build monitor retained a private transcript or status file after success'
fi
cat > "$monitor_dir/build-macos-slice.sh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' go-build > "$BUILD_SAFE_STAGE_FILE"
printf '%s\n' 'PRIVATE FAILURE OUTPUT MUST STAY HIDDEN' >&2
exit 7
EOF
chmod +x "$monitor_dir/build-macos-slice.sh"
if (
  cd "$monitor_dir"
  RUNNER_TEMP="$monitor_runner" MAC_BUILD_HEARTBEAT_SECONDS=1 \
    MAC_BUILD_HEARTBEAT_EVERY=1 "$MAC_BUILD_MONITOR" amd64
) > "$monitor_output" 2>&1; then
  fail 'macOS build monitor accepted a failed private build'
fi
require "$monitor_output" 'Private macOS build failed at source-safe stage: go-build.'
if grep -Fq 'PRIVATE FAILURE OUTPUT' "$monitor_output"; then
  fail 'macOS build monitor exposed private build output on failure'
fi
if compgen -G "$monitor_runner/opencider-macos-*" >/dev/null; then
  fail 'macOS build monitor retained a private transcript or status file after failure'
fi
cat > "$monitor_dir/build-macos-slice.sh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$$" > "$FAKE_CHILD_PID_FILE"
printf '%s\n' cargo-build > "$BUILD_SAFE_STAGE_FILE"
printf '%s\n' 'PRIVATE CANCELLATION OUTPUT MUST STAY HIDDEN' >&2
trap 'exit 0' INT TERM
while :; do sleep 1; done
EOF
chmod +x "$monitor_dir/build-macos-slice.sh"
child_pid_file="$checkout_tmp/mac-monitor-child-pid"
monitor_cwd="$PWD"
cd "$monitor_dir"
RUNNER_TEMP="$monitor_runner" MAC_BUILD_HEARTBEAT_SECONDS=1 \
  MAC_BUILD_HEARTBEAT_EVERY=1 FAKE_CHILD_PID_FILE="$child_pid_file" \
  "$MAC_BUILD_MONITOR" arm64 > "$monitor_output" 2>&1 &
monitor_pid=$!
cd "$monitor_cwd"
for _ in 1 2 3 4 5 6 7 8 9 10; do
  [ -s "$child_pid_file" ] && break
  sleep 1
done
[ -s "$child_pid_file" ] || fail 'cancellation fixture did not start its private child'
child_pid="$(<"$child_pid_file")"
kill -TERM "$monitor_pid"
set +e
wait "$monitor_pid"
monitor_status=$?
set -e
[ "$monitor_status" -ne 0 ] || fail 'cancelled macOS build monitor reported success'
for _ in 1 2 3 4 5; do
  kill -0 "$child_pid" 2>/dev/null || break
  sleep 1
done
if kill -0 "$child_pid" 2>/dev/null; then
  kill -TERM "$child_pid" 2>/dev/null || true
  fail 'cancelled macOS build monitor left its private child running'
fi
sleep 1
if compgen -G "$monitor_runner/opencider-macos-*" >/dev/null; then
  fail 'cancelled macOS build recreated or retained a private transcript/status file'
fi
if grep -Fq 'PRIVATE CANCELLATION OUTPUT' "$monitor_output"; then
  fail 'macOS build monitor exposed private build output on cancellation'
fi

guard_repo="$checkout_tmp/public-guard-repo"
git init -q "$guard_repo"
git -C "$guard_repo" config user.email 'test@example.invalid'
git -C "$guard_repo" config user.name 'Public guard test'
printf 'allowed\n' > "$guard_repo/allowed.txt"
git -C "$guard_repo" add allowed.txt
( cd "$guard_repo" && "$PUBLIC_COMMIT_GUARD" ) >/dev/null 2>&1 \
  || fail 'public commit guard rejected an ordinary staged file'
mkdir -p "$guard_repo/rustpush/open-absinthe/src"
printf 'public fixture\n' > "$guard_repo/rustpush/open-absinthe/src/nac.rs"
git -C "$guard_repo" add rustpush/open-absinthe/src/nac.rs
( cd "$guard_repo" && "$PUBLIC_COMMIT_GUARD" ) >/dev/null 2>&1 \
  || fail 'public commit guard rejected the intentionally public open-absinthe implementation'
mkdir -p "$guard_repo/opencider"
printf 'private fixture\n' > "$guard_repo/opencider/private.rs"
git -C "$guard_repo" add -f opencider/private.rs
if ( cd "$guard_repo" && "$PUBLIC_COMMIT_GUARD" ) >/dev/null 2>&1; then
  fail 'public commit guard accepted the private OpenCider checkout path'
fi

cat > "$checkout_tmp/bin/git" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' 'PRIVATE COMMIT SUBJECT MUST STAY HIDDEN' >&2
case " $* " in
  *' rev-parse HEAD '*) printf '%040d\n' 0 ;;
  *' rev-list --all '*) printf '%040d\n' 0 ;;
  *' fetch '* ) [ "${FAKE_GIT_FETCH_FAIL:-0}" != 1 ] ;;
esac
EOF
chmod +x "$checkout_tmp/bin/git"
checkout_output="$checkout_tmp/public-output"
( cd "$checkout_tmp" && \
  OPENCIDER_READ_DEPLOY_KEY='fixture-key' RUNNER_TEMP="$checkout_tmp/runner" \
  PATH="$checkout_tmp/bin:$PATH" "$PRIVATE_CHECKOUT" success master ) \
  > "$checkout_output" 2>&1 \
  || fail 'source-safe private checkout helper rejected a clean synthetic checkout'
if grep -Fq 'PRIVATE COMMIT SUBJECT' "$checkout_output"; then
  fail 'source-safe private checkout helper exposed Git output on success'
fi
if compgen -G "$checkout_tmp/runner/opencider-*" >/dev/null; then
  fail 'source-safe private checkout helper retained a key, host-key file, or private Git transcript after success'
fi
fixture_sha="$(printf '%040d' 0)"
fixture_token="$(FIXTURE_KEY='fixture-key' FIXTURE_SHA="$fixture_sha" python3 -c \
  'import hashlib,hmac,os; key=os.environ["FIXTURE_KEY"].encode().rstrip(b"\r\n")+b"\n"; message=b"opencider-ref-v1:"+os.environ["FIXTURE_SHA"].encode(); print(hmac.new(key,message,hashlib.sha256).hexdigest())')"
[[ "$fixture_token" =~ ^[0-9a-f]{64}$ ]] || fail 'opaque fixture revision token generation failed'
( cd "$checkout_tmp" && \
  OPENCIDER_READ_DEPLOY_KEY='fixture-key' RUNNER_TEMP="$checkout_tmp/runner" \
  PATH="$checkout_tmp/bin:$PATH" "$PRIVATE_CHECKOUT" success-token "$fixture_token" ) \
  > "$checkout_output" 2>&1 \
  || fail 'source-safe private checkout helper rejected a valid opaque revision token'
if grep -Fq 'PRIVATE COMMIT SUBJECT' "$checkout_output" \
   || grep -Fq "$fixture_sha" "$checkout_output"; then
  fail 'source-safe private checkout helper exposed private revision metadata'
fi
if compgen -G "$checkout_tmp/runner/opencider-*" >/dev/null; then
  fail 'opaque checkout retained a key, host-key file, revision list, or private Git transcript'
fi
if ( cd "$checkout_tmp" && \
  OPENCIDER_READ_DEPLOY_KEY='fixture-key' RUNNER_TEMP="$checkout_tmp/runner" \
  FAKE_GIT_FETCH_FAIL=1 PATH="$checkout_tmp/bin:$PATH" \
  "$PRIVATE_CHECKOUT" failure master ) > "$checkout_output" 2>&1; then
  fail 'source-safe private checkout helper accepted a failed fetch'
fi
require "$checkout_output" 'Private checkout failed. Detailed Git output was intentionally withheld from public Actions logs.'
if grep -Fq 'PRIVATE COMMIT SUBJECT' "$checkout_output"; then
  fail 'source-safe private checkout helper exposed Git output on failure'
fi
if compgen -G "$checkout_tmp/runner/opencider-*" >/dev/null; then
  fail 'source-safe private checkout helper retained a key, host-key file, or private Git transcript after failure'
fi
private_checkout_count="$(grep -Fc -- './.github/scripts/checkout-opencider.sh opencider "$OPENCIDER_REF_TOKEN"' "$WORKFLOW")"
[ "$private_checkout_count" -eq 6 ] || fail "all six private checkouts must use the source-safe helper (found $private_checkout_count)"
verified_ref_count="$(grep -Fc -- 'OPENCIDER_REF_TOKEN: ${{ needs.verify-rustpush-patches.outputs.opencider_ref_token }}' "$WORKFLOW")"
[ "$verified_ref_count" -eq 5 ] || fail "all four platform jobs and finalizer must consume the opaque verified OpenCider token (found $verified_ref_count)"
require "$WORKFLOW" 'needs: [resolve-release-version, verify-rustpush-patches, macos-amd64, macos-arm64, linux-amd64, linux-arm64]'
require "$WORKFLOW" 'Apply and verify all portable RustPush patches'
require "$WORKFLOW" 'RustPush patches applied and verified for private and public build flavors.'
require "$WORKFLOW" 'PATCH_SAFE_STATUS_FILE: ${{ runner.temp }}/opencider-patch-status'
require "$WORKFLOW" './verify-rustpush-patches.sh > "$RUNNER_TEMP/opencider-patch-verify.log" 2>&1'
require "$WORKFLOW" 'Portable RustPush patch verification failed at source-safe entry:'
require "$WORKFLOW" 'Detailed patch/source output was intentionally withheld from public Actions logs.'
require "$WORKFLOW" 'needs: [resolve-release-version, verify-rustpush-patches]'
count="$(grep -Fc -- 'needs: [resolve-release-version, verify-rustpush-patches]' "$WORKFLOW")"
[ "$count" -eq 4 ] || fail "all four platform jobs must depend on patch verification (found $count)"
readelf_guard_count="$(grep -Fc -- 'readelf --dyn-syms --wide' "$WORKFLOW" || true)"
[ "$readelf_guard_count" -eq 0 ] || fail "private artifact inspection belongs in the redacted verifier, not public workflow text"
artifact_verifier_count="$(grep -Fc -- './opencider/tools/verify-linux-artifact.sh "$binary"' "$WORKFLOW")"
[ "$artifact_verifier_count" -eq 2 ] || fail "both Linux artifacts must pass the redacted private verifier (found $artifact_verifier_count)"
mac_artifact_verifier_count="$(grep -Fc -- './opencider/tools/verify-macos-artifact.sh "$binary"' "$WORKFLOW" || true)"
[ "$mac_artifact_verifier_count" -eq 3 ] || fail "both macOS slices and the universal binary must pass the redacted private verifier (found $mac_artifact_verifier_count)"
require "$WORKFLOW" 'Private Linux artifact safety verification failed. Detailed inspection output was intentionally withheld from public Actions logs.'
require "$WORKFLOW" 'Private macOS artifact safety verification failed. Detailed inspection output was intentionally withheld from public Actions logs.'
regular_file_guard_count="$(grep -Fc -- 'test ! -L "$binary"' "$WORKFLOW")"
[ "$regular_file_guard_count" -ge 5 ] || fail "every uploaded artifact must reject symlinks (found $regular_file_guard_count guards)"
mach_execute_guard_count="$(grep -Fc -- 'otool -hv "$binary" | grep -qw EXECUTE' "$WORKFLOW")"
[ "$mach_execute_guard_count" -ge 3 ] || fail "both macOS slices and the universal binary must be Mach-O executables (found $mach_execute_guard_count guards)"
python3 - "$WORKFLOW" "$MAC_BUILD_MONITOR" <<'PY'
from pathlib import Path
import re
import sys

workflow = Path(sys.argv[1]).read_text()
mac_monitor = Path(sys.argv[2]).read_text()
action_uses = re.findall(r'^\s+uses:\s+([^@\s]+)@([^\s]+)', workflow, re.MULTILINE)
if not action_uses:
    raise SystemExit('workflow has no pinned Actions to validate')
mutable_actions = [f'{name}@{ref}' for name, ref in action_uses if not re.fullmatch(r'[0-9a-f]{40}', ref)]
if mutable_actions:
    raise SystemExit(f'Actions must use immutable commit SHAs: {mutable_actions!r}')
private_commands = re.findall(
    r'^\s+(?:if ! )?\./(?:verify-rustpush-patches|build-macos-slice|build-linux|build-macos-universal|opencider/tools/verify-linux-artifact|opencider/tools/verify-macos-artifact)\.sh.*$',
    workflow,
    re.MULTILINE,
)
if len(private_commands) != 11:
    raise SystemExit(f'expected 11 directly redacted private-script invocations, found {len(private_commands)}')
unsafe_commands = [
    command for command in private_commands
    if not re.search(r'> "\$RUNNER_TEMP/[^"]+\.log" 2>&1', command)
]
if unsafe_commands:
    raise SystemExit('private OpenCider script can write directly to public Actions logs')
if '["./build-macos-slice.sh", arch]' not in mac_monitor or 'start_new_session=True' not in mac_monitor:
    raise SystemExit('macOS monitor must launch exactly one fixed private slice build in an isolated process group')
if 'os.killpg(child.pid, signum)' not in mac_monitor:
    raise SystemExit('macOS monitor must forward cancellation to the complete private build process group')
if "trap cleanup EXIT" not in mac_monitor or 'rm -f "$status_file" "$build_log"' not in mac_monitor:
    raise SystemExit('macOS monitor must delete its source-safe status and private transcript')

upload_paths = re.findall(
    r'uses: actions/upload-artifact@[0-9a-f]{40}.*?\n\s+with:\n.*?\n\s+path: ([^\n]+)',
    workflow,
    re.DOTALL,
)
expected_upload_paths = [
    'opencider/dist/corten-matrix-macos.amd64',
    'opencider/dist/corten-matrix-macos.arm64',
    'opencider/dist/corten-matrix-linux-amd64',
    'opencider/dist/corten-matrix-linux-arm64',
    'opencider/dist/corten-matrix-macos',
]
if upload_paths != expected_upload_paths:
    raise SystemExit(f'artifact upload allowlist changed: {upload_paths!r}')
for forbidden in ('dist/**', 'build/**', 'target/**', '.a\n', 'opencider-build.log'):
    if forbidden in '\n'.join(upload_paths):
        raise SystemExit(f'private or broad artifact path is forbidden: {forbidden}')
step_starts = list(re.finditer(r'^      - name: ', workflow, re.MULTILINE))
step_blocks = [
    workflow[match.start():(step_starts[index + 1].start() if index + 1 < len(step_starts) else len(workflow))]
    for index, match in enumerate(step_starts)
]
cache_blocks = [block for block in step_blocks if re.search(r'^        uses: actions/cache@[0-9a-f]{40}', block, re.MULTILINE)]
if len(cache_blocks) != 6:
    raise SystemExit(f'exactly six public-input cache steps are required, found {len(cache_blocks)}')

def cache_spec(block):
    name_match = re.search(r'^      - name: (.+)$', block, re.MULTILINE)
    path_match = re.search(r'^          path: (.+)$', block, re.MULTILINE)
    key_match = re.search(r'^          key: (.+)$', block, re.MULTILINE)
    if not (name_match and path_match and key_match):
        raise SystemExit('cache step is missing a name, path, or exact key')
    if path_match.group(1) == '|':
        path_lines = block[path_match.end():].splitlines()
        paths = []
        for line in path_lines:
            if line.startswith('            '):
                paths.append(line.strip())
            elif line.strip():
                break
    else:
        paths = [path_match.group(1)]
    return name_match.group(1), tuple(paths), key_match.group(1)

cargo_key = "opencider-public-cargo-${{ runner.os }}-${{ runner.arch }}-${{ hashFiles('pkg/rustpushgo/Cargo.lock', 'nac-validation/Cargo.lock', 'third_party/rustpush-upstream.sha') }}"
expected_cache_specs = [
    ('Cache public Cargo registry inputs', ('~/.cargo/registry/cache', '~/.cargo/registry/index'), cargo_key),
    ('Cache public Cargo registry inputs', ('~/.cargo/registry/cache', '~/.cargo/registry/index'), cargo_key),
    ('Cache public Cargo registry inputs', (
        '${{ runner.temp }}/opencider-public-cargo-registry/cache',
        '${{ runner.temp }}/opencider-public-cargo-registry/index',
    ), cargo_key),
    ('Cache public protoc release archive', (
        '${{ runner.temp }}/opencider-public-protoc/protoc-27.5-linux-x86_64.zip',
    ), 'opencider-public-protoc-${{ runner.os }}-${{ runner.arch }}-27.5-33abede6330dd22fb9af47c2d9804ead3d86c82e025b9eef361a9ac2e955ecce'),
    ('Cache public Cargo registry inputs', (
        '${{ runner.temp }}/opencider-public-cargo-registry/cache',
        '${{ runner.temp }}/opencider-public-cargo-registry/index',
    ), cargo_key),
    ('Cache public protoc release archive', (
        '${{ runner.temp }}/opencider-public-protoc/protoc-27.5-linux-aarch_64.zip',
    ), 'opencider-public-protoc-${{ runner.os }}-${{ runner.arch }}-27.5-9fdb165737b6dc8138473fa6a18e8a9e21a85f567102f3ab20d4e70442eef6db'),
]
actual_cache_specs = [cache_spec(block) for block in cache_blocks]
if actual_cache_specs != expected_cache_specs:
    raise SystemExit(f'public cache allowlist changed: {actual_cache_specs!r}')
if any('restore-keys:' in block for block in cache_blocks):
    raise SystemExit('cache restore prefixes are forbidden; cache keys must match exactly')
for forbidden_cache_path in (
    '/target', 'cargo-target', 'registry/src', '/git', 'go-build',
    'opencider/dist', 'opencider/build', 'opencider/rustpush', 'private-go', '.log',
):
    if any(forbidden_cache_path in block for block in cache_blocks):
        raise SystemExit(f'private, extracted-source, or compiled cache path is forbidden: {forbidden_cache_path}')
protoc_mount = '--volume "$RUNNER_TEMP/opencider-public-protoc:/public-protoc-cache"'
cargo_mount = '--volume "$RUNNER_TEMP/opencider-public-cargo-registry:/root/.cargo/registry"'
if workflow.count(protoc_mount) != 4:
    raise SystemExit('both Linux preflight and build containers must mount the protoc archive cache')
if workflow.count(cargo_mount) != 2:
    raise SystemExit('only the two Linux build containers may mount the public Cargo registry cache')

def one_step(name):
    matches = [block for block in step_blocks if block.startswith(f'      - name: {name}\n')]
    if len(matches) != 1:
        raise SystemExit(f'expected exactly one workflow step named {name!r}, found {len(matches)}')
    return matches[0]

for arch in ('amd64', 'arm64'):
    preflight = one_step(f'Preflight native {arch} Linux toolchain')
    build = one_step(f'Build native {arch} Linux binary')
    if preflight.count(protoc_mount) != 1 or build.count(protoc_mount) != 1:
        raise SystemExit(f'Linux {arch} preflight and build must each mount the protoc ZIP cache')
    if cargo_mount in preflight or build.count(cargo_mount) != 1:
        raise SystemExit(f'only the Linux {arch} build may mount the Cargo registry cache')
mac_slice_invocations = re.findall(
    r'^\s+run: (\.\./\.github/scripts/run-private-macos-build\.sh (?:amd64|arm64))$',
    workflow,
    re.MULTILINE,
)
if mac_slice_invocations != ['../.github/scripts/run-private-macos-build.sh amd64', '../.github/scripts/run-private-macos-build.sh arm64']:
    raise SystemExit(f'macOS jobs must use the source-safe public-OpenAbsinthe build monitor: {mac_slice_invocations!r}')
if re.search(r'^\s+(?:if ! )?\./build\.sh\b', workflow, re.MULTILINE):
    raise SystemExit('public workflow must not bypass the macOS public-OpenAbsinthe slice contract')
setup_go_blocks = [block for block in step_blocks if re.search(r'^        uses: actions/setup-go@[0-9a-f]{40}', block, re.MULTILINE)]
if len(setup_go_blocks) != 2 or any(block.count('          cache: false\n') != 1 for block in setup_go_blocks):
    raise SystemExit('both and only the Linux setup-go steps must keep caching disabled')
if workflow.count("trap 'rm -f \"$RUNNER_TEMP/opencider") != 11:
    raise SystemExit('every directly invoked private build/verification transcript must be deleted when its step exits')
for forbidden in ('set -x', 'set -o xtrace', 'tee ', 'printenv'):
    if forbidden in workflow:
        raise SystemExit(f'public workflow contains a log- or cache-leak primitive: {forbidden}')
secret_names = re.findall(r'secrets\.([A-Za-z0-9_]+)', workflow)
if set(secret_names) != {'OPENCIDER_READ_DEPLOY_KEY'} or len(secret_names) != 6:
    raise SystemExit(f'unexpected workflow secret surface: {secret_names!r}')
if workflow.count('contents: write') != 1:
    raise SystemExit('only the finalizer may receive contents: write')
PY
require "$WORKFLOW" './build-macos-universal.sh > "$RUNNER_TEMP/opencider-build.log" 2>&1'
require "$WORKFLOW" 'Private build failed. Detailed compiler output was intentionally withheld from public Actions logs.'
require "$WORKFLOW" 'BUILD_SAFE_STAGE_FILE: /runner-temp/opencider-linux-amd64-stage'
require "$WORKFLOW" 'BUILD_SAFE_STAGE_FILE: /runner-temp/opencider-linux-arm64-stage'
require "$WORKFLOW" '--volume "$RUNNER_TEMP:/runner-temp"'
require "$WORKFLOW" '--volume "$RUNNER_TEMP/opencider-public-cargo-registry:/root/.cargo/registry"'
require "$WORKFLOW" '--volume "$RUNNER_TEMP/opencider-public-protoc:/public-protoc-cache"'
require "$WORKFLOW" 'PROTOC_DOWNLOAD_CACHE_DIR: /public-protoc-cache'
require "$WORKFLOW" 'Private build failed at source-safe stage:'
require "$WORKFLOW" 'sync-public-bridge|clone-public-rustpush|checkout-public-rustpush|fairplay-stubs|fetch-public-submodules|validate-public-submodules|overlay-private-rust|apply-rustpush-patches'

ruby -e 'require "yaml"; YAML.load_file(ARGV.fetch(0))' "$WORKFLOW" \
  || fail 'workflow YAML does not parse'
printf 'PASS: public release workflow safety contract\n'
