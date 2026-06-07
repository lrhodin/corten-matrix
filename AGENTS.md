# Dev notes

## FFI boundary (Go ↔ Rust)

**Never hand-edit** `pkg/rustpushgo/rustpushgo.go` or `rustpushgo.h`. Always regenerate.

### Linux / macOS

```bash
make bindings   # requires uniffi-bindgen-go on PATH
make build
```

Install `uniffi-bindgen-go` (must match UniFFI 0.25.0 as pinned in `pkg/rustpushgo/Cargo.toml`):
```bash
cargo install uniffi-bindgen-go --git https://github.com/NordSecurity/uniffi-bindgen-go --tag v0.2.2+v0.25.0
```

### Windows

The Makefile's `bindings` target hardcodes UNIX static-lib naming
(`librustpushgo.a`). MSVC Rust emits `rustpushgo.lib` instead, so `make
bindings` on Windows can't find the artifact. Use the Windows-native helper
scripts under `dev/` instead:

```cmd
dev\windows-dev-env.bat && dev\windows-bindings.bat
```

- `dev\windows-dev-env.bat` — one-shot environment bootstrap: vcvarsall,
  LIB/INCLUDE, PATH (cargo, Strawberry Perl, CMake, Python, protoc, LLVM),
  LIBCLANG_PATH, `python3` shim, and a first-run `cargo install` for
  `uniffi-bindgen-go`. Idempotent — safe to re-run.
- `dev\windows-bindings.bat` — mirrors `make bindings`: builds the crate if
  the `.lib` is missing, runs `uniffi-bindgen-go` against `rustpushgo.lib`,
  then runs `python3 scripts/patch_bindings.py`. Respects `CARGO_TARGET_DIR`
  if set (so users with cloud-sync folders can redirect cargo output
  elsewhere to avoid file-lock races on openssl-sys intermediates).

The one-time winget installs needed for the dev-env script to work are
documented at the top of `dev\windows-dev-env.bat`.

## Config

### Network config (the `network:` / iMessage settings)

**Source of truth: `pkg/imconfig/example-config.yaml`** — the documented
defaults and comments users see. It is embedded **verbatim** via `//go:embed`
into `NetworkExampleConfig` (`pkg/imconfig/imconfig.go`); it is **not** generated
from Go, and there is no separate template. Both `pkg/connector` and `cmd/bbctl`
consume this one embedded copy, so **never re-add a hardcoded config string**
elsewhere — the two generation paths drifted once, which is exactly why this was
unified into `pkg/imconfig`.

The matching Go struct is `IMConfig` in `pkg/connector/config.go`, with
`upgradeConfig` handling migrations.

To add or change a network option, edit all three in lockstep:
1. `pkg/imconfig/example-config.yaml` — the YAML key, default value, and the
   user-facing comment.
2. `pkg/connector/config.go` — the `IMConfig` struct field (+ `yaml:` tag) and
   its doc comment. Keep this comment's wording in sync with the YAML comment.
3. `pkg/connector/config.go` `upgradeConfig` — add a `helper.Copy(...)` line so
   the option survives a config upgrade.

`pkg/imconfig/imconfig_test.go` guards that the embedded YAML stays valid YAML.

### Bridge (base/appservice) config — the `bridge:` section

This is the **bridgev2 framework's** config (`bridgeconfig.BridgeConfig`), not
ours. To change a framework default, override it in `IMConnector.Start()`
(config YAML is loaded by then) — see the existing overrides there for
`phone_numbers_in_profile` (must be true or bridgev2 strips `tel:` from contact
profiles → no call button), `unknown_error_auto_reconnect`, and
`unknown_error_max_auto_reconnects`.
