APP_NAME    := corten-matrix
CMD_PKG     := corten-matrix
VERSION     := 0.1.0
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
UNAME_S     := $(shell uname -s)

RUST_LIB    := librustpushgo.a
RUST_SRC    := $(shell find pkg/rustpushgo/src -name '*.rs' -o -name '*.m' -o -name '*.h' 2>/dev/null) \
               pkg/rustpushgo/build.rs \
               $(shell find nac-validation/src -name '*.rs' -o -name '*.m' -o -name '*.h' 2>/dev/null) \
               nac-validation/build.rs
RUSTPUSH_DIR := third_party/rustpush-upstream
# Pinned OpenBubbles/rustpush commit. Edit third_party/rustpush-upstream.sha to
# bump, then test locally before committing. The Makefile reads the SHA on build
# and checks out that exact commit — no auto-bump, no branch drift.
RUSTPUSH_PIN_FILE := third_party/rustpush-upstream.sha
RUSTPUSH_PIN      := $(shell cat $(RUSTPUSH_PIN_FILE) 2>/dev/null)
RUSTPUSH_SRC:= $(shell find $(RUSTPUSH_DIR)/src $(RUSTPUSH_DIR)/apple-private-apis $(RUSTPUSH_DIR)/open-absinthe/src -name '*.rs' -o -name '*.s' 2>/dev/null) $(wildcard $(RUSTPUSH_DIR)/open-absinthe/build.rs)
CARGO_FILES := $(shell find . -name 'Cargo.toml' -o -name 'Cargo.lock' 2>/dev/null | grep -v target)
GO_SRC      := $(shell find pkg/ cmd/ -name '*.go' 2>/dev/null)

LDFLAGS     := -X main.Tag=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildTime=$(BUILD_TIME)

# Track the git commit so ldflags changes trigger a Go rebuild.
# .build-commit is updated whenever HEAD changes.
COMMIT_FILE := .build-commit
PREV_COMMIT := $(shell cat $(COMMIT_FILE) 2>/dev/null)
ifneq ($(COMMIT),$(PREV_COMMIT))
  $(shell echo $(COMMIT) > $(COMMIT_FILE))
endif

# Plain `make` builds the corten-matrix binary.
.DEFAULT_GOAL := build
.PHONY: build clean rust bindings check-deps

# ===========================================================================
# Path validation – spaces in the working directory break CGO linker flags
# and #cgo LDFLAGS ${SRCDIR} expansion. Detect early with a clear message.
# ===========================================================================
ifneq ($(word 2,$(CURDIR)),)
  $(error The project path "$(CURDIR)" contains spaces. CGO and the linker cannot handle spaces in library paths. Please move the project to a path without spaces, e.g.: $(HOME)/corten-matrix)
endif

# ===========================================================================
# macOS only — NAC validation data comes from Apple's native AAAbsintheContext
# framework, which exists only on macOS. Build and run the bridge on a Mac.
# ===========================================================================
ifneq ($(UNAME_S),Darwin)
  $(error This bridge builds on macOS only: NAC uses Apple's native AAAbsintheContext framework. Build and run it on a Mac.)
endif

# Plain binary (no .app bundle; host ops live in the corten-matrix subcommands).
export PATH := /opt/homebrew/bin:/opt/homebrew/sbin:$(PATH)
BINARY      := $(APP_NAME)
CGO_CFLAGS  := -I/opt/homebrew/include
CGO_LDFLAGS := -L/opt/homebrew/lib -L$(CURDIR)
CARGO_ENV   := MACOSX_DEPLOYMENT_TARGET=13.0

# ===========================================================================
# Dependency checks
# ===========================================================================

# Auto-install missing build deps via Homebrew.
check-deps:
	@if ! command -v brew >/dev/null 2>&1; then \
		echo "Installing Homebrew..."; \
		NONINTERACTIVE=1 /bin/bash -c "$$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"; \
		eval "$$(/opt/homebrew/bin/brew shellenv)"; \
	fi; \
	missing=""; \
	command -v go >/dev/null 2>&1    || missing="$$missing go"; \
	command -v cargo >/dev/null 2>&1 || missing="$$missing rust"; \
	command -v protoc >/dev/null 2>&1|| missing="$$missing protobuf"; \
	command -v tmux >/dev/null 2>&1  || missing="$$missing tmux"; \
	[ -f /opt/homebrew/include/olm/olm.h ] || [ -f /usr/local/include/olm/olm.h ] || missing="$$missing libolm"; \
	pkg-config --exists libheif 2>/dev/null || missing="$$missing libheif"; \
	if [ -n "$$missing" ]; then \
		echo "Installing dependencies:$$missing"; \
		brew install $$missing; \
	fi

# ===========================================================================
# Rust static library
# ===========================================================================

# From-source build feature selection.
# ---------------------------------------------------------------------------
# The rustpushgo crate's default feature, `cleanroom-registration`, enables
# `open-absinthe/native-nac-rust` + `remote-clearadi`, which are NOT provided by
# the upstream OpenBubbles crates this from-source build vendors. So this build
# opts out of the default and selects the native AAAbsintheContext NAC path
# (`nac-apple-framework`). omnisette's built-in macOS AOSKit anisette compiles
# automatically — no provider feature, no unicorn, no cmake. The rustpushgo
# `anisette-*` features are skipped because they pull `prefer-*` omnisette
# features that upstream omnisette doesn't define.
#
# Override on the command line to build a different feature shape, e.g.:
#   make CARGO_FEATURES="--features cleanroom-registration"
CARGO_FEATURES := --no-default-features --features nac-apple-framework

UPSTREAM_REPO := https://github.com/OpenBubbles/rustpush.git
FAIRPLAY_CERTS := 4056631661436364584235346952193 \
                  4056631661436364584235346952194 \
                  4056631661436364584235346952195 \
                  4056631661436364584235346952196 \
                  4056631661436364584235346952197 \
                  4056631661436364584235346952198 \
                  4056631661436364584235346952199 \
                  4056631661436364584235346952200 \
                  4056631661436364584235346952201 \
                  4056631661436364584235346952208

.PHONY: ensure-rustpush-source

# Prepare rustpush sources the same way upstream CI does: checkout with
# submodules present and fake FairPlay certs available for build-time signing.
ensure-rustpush-source:
	@if [ "$(RUSTPUSH_DIR)" = "third_party/rustpush-upstream" ]; then \
		if [ -z "$(RUSTPUSH_PIN)" ]; then \
			echo "error: $(RUSTPUSH_PIN_FILE) is missing or empty — required to pin rustpush SHA" >&2; exit 1; \
		fi; \
		export GIT_CONFIG_COUNT=1; \
		export GIT_CONFIG_KEY_0="url.https://github.com/.insteadOf"; \
		export GIT_CONFIG_VALUE_0="git@github.com:"; \
		if [ ! -d third_party/rustpush-upstream/.git ]; then \
			echo "Cloning OpenBubbles/rustpush at pinned SHA $(RUSTPUSH_PIN)..."; \
			mkdir -p third_party; \
			git clone $(UPSTREAM_REPO) third_party/rustpush-upstream || exit 1; \
			git -C third_party/rustpush-upstream checkout $(RUSTPUSH_PIN) || exit 1; \
			git -C third_party/rustpush-upstream submodule sync --recursive || exit 1; \
			git -C third_party/rustpush-upstream submodule update --init --recursive || exit 1; \
		fi; \
		current=$$(git -C third_party/rustpush-upstream rev-parse HEAD 2>/dev/null || echo none); \
		if [ "$$current" != "$(RUSTPUSH_PIN)" ]; then \
			echo "Checking out pinned rustpush SHA $(RUSTPUSH_PIN) (was $$current)..."; \
			git -C third_party/rustpush-upstream remote set-url origin $(UPSTREAM_REPO); \
			echo "Discarding any local mods to third_party/rustpush-upstream before checkout..."; \
			git -C third_party/rustpush-upstream reset --hard HEAD || exit 1; \
			git -C third_party/rustpush-upstream clean -fd || exit 1; \
			git -C third_party/rustpush-upstream fetch --all --tags --prune || exit 1; \
			git -C third_party/rustpush-upstream checkout $(RUSTPUSH_PIN) || { echo "error: failed to checkout pinned SHA $(RUSTPUSH_PIN)" >&2; exit 1; }; \
			git -C third_party/rustpush-upstream submodule sync --recursive || exit 1; \
			git -C third_party/rustpush-upstream submodule update --init --recursive || exit 1; \
		fi; \
		if [ ! -d third_party/rustpush-upstream/certs/fairplay ]; then \
			echo "Generating FairPlay cert stubs..."; \
			mkdir -p third_party/rustpush-upstream/certs/fairplay; \
			for name in $(FAIRPLAY_CERTS); do \
				cp third_party/rustpush-upstream/certs/legacy-fairplay/fairplay.crt \
				   third_party/rustpush-upstream/certs/fairplay/$$name.crt; \
				cp third_party/rustpush-upstream/certs/legacy-fairplay/fairplay.pem \
				   third_party/rustpush-upstream/certs/fairplay/$$name.pem; \
			done; \
		fi; \
		if [ -d rustpush/open-absinthe ] && [ -f rustpush/open-absinthe/Cargo.toml ]; then \
			if ! diff -rq rustpush/open-absinthe third_party/rustpush-upstream/open-absinthe >/dev/null 2>&1; then \
				echo "Overlaying our open-absinthe (native NAC wiring) onto $(RUSTPUSH_DIR)/open-absinthe..."; \
				rm -rf third_party/rustpush-upstream/open-absinthe; \
				cp -Rp rustpush/open-absinthe third_party/rustpush-upstream/open-absinthe; \
			fi; \
		fi; \
		if [ -f $(RUSTPUSH_DIR)/apple-private-apis/omnisette/Cargo.toml ] && ! grep -q 'prefer-aoskit' $(RUSTPUSH_DIR)/apple-private-apis/omnisette/Cargo.toml 2>/dev/null; then \
			echo "Adding no-op prefer-* anisette features to upstream omnisette (the rustpushgo anisette-aoskit/anisette-remote-v3 features reference omnisette/prefer-* features that upstream omnisette doesn't define; cargo validates the whole [features] table even though this build never selects them, so define them here as harmless no-ops)..."; \
			perl -i -pe 's/^(remote-clearadi = .*)$$/$$1\nprefer-aoskit = []\nprefer-remote-anisette-v3 = []/' $(RUSTPUSH_DIR)/apple-private-apis/omnisette/Cargo.toml; \
		fi; \
		if grep -q '^mod activation;' $(RUSTPUSH_DIR)/src/lib.rs 2>/dev/null; then \
			echo "Making rustpush activation module public (used by OSConfig overlays)..."; \
			sed -i.bak 's/^mod activation;/pub mod activation;/' $(RUSTPUSH_DIR)/src/lib.rs && rm -f $(RUSTPUSH_DIR)/src/lib.rs.bak; \
		fi; \
		if grep -q '^mod ids;' $(RUSTPUSH_DIR)/src/lib.rs 2>/dev/null; then \
			echo "Making rustpush ids module public (needed by FT RespondedElsewhere overlay)..."; \
			sed -i.bak 's/^mod ids;/pub mod ids;/' $(RUSTPUSH_DIR)/src/lib.rs && rm -f $(RUSTPUSH_DIR)/src/lib.rs.bak; \
		fi; \
		if grep -q '^    token: String,$$' $(RUSTPUSH_DIR)/apple-private-apis/icloud-auth/src/client.rs 2>/dev/null; then \
			echo "Making FetchedToken.token pub (needed to replay persisted PET on session restore)..."; \
			sed -i.bak 's/^    token: String,$$/    pub token: String,/' $(RUSTPUSH_DIR)/apple-private-apis/icloud-auth/src/client.rs && rm -f $(RUSTPUSH_DIR)/apple-private-apis/icloud-auth/src/client.rs.bak; \
		fi; \
		if grep -q '^    expiration: SystemTime,$$' $(RUSTPUSH_DIR)/apple-private-apis/icloud-auth/src/client.rs 2>/dev/null; then \
			echo "Making FetchedToken.expiration pub..."; \
			sed -i.bak 's/^    expiration: SystemTime,$$/    pub expiration: SystemTime,/' $(RUSTPUSH_DIR)/apple-private-apis/icloud-auth/src/client.rs && rm -f $(RUSTPUSH_DIR)/apple-private-apis/icloud-auth/src/client.rs.bak; \
		fi; \
		if grep -q '^pub use client::{AppleAccount, LoginState,' $(RUSTPUSH_DIR)/apple-private-apis/icloud-auth/src/lib.rs 2>/dev/null; then \
			echo "Re-exporting FetchedToken from icloud_auth crate root..."; \
			sed -i.bak 's/^pub use client::{AppleAccount, LoginState,/pub use client::{AppleAccount, FetchedToken, LoginState,/' $(RUSTPUSH_DIR)/apple-private-apis/icloud-auth/src/lib.rs && rm -f $(RUSTPUSH_DIR)/apple-private-apis/icloud-auth/src/lib.rs.bak; \
		fi; \
		if grep -q '^            for excluded in &trust.excludeds {$$' $(RUSTPUSH_DIR)/src/icloud/keychain.rs 2>/dev/null && \
		   ! grep -q 'Ignoring exclusion of ourselves' $(RUSTPUSH_DIR)/src/icloud/keychain.rs 2>/dev/null; then \
			echo "Patching keychain.rs to ignore self-exclusion in fast_forward_trust (Clique self-eviction fix; ports 9f29ff1)..."; \
			KEYCHAIN_REPL=$$(printf '            let my_id = \\&state.user_identity.as_ref().unwrap().identifier;\\\n&\\\n                if excluded == my_id {\\\n                    warn!(\\\n                        "Ignoring exclusion of ourselves ({}) from peer {}",\\\n                        excluded,\\\n                        peer.0.hash.as_ref().unwrap()\\\n                    );\\\n                    continue;\\\n                }') && \
			sed -i.bak "s|^            for excluded in \&trust\.excludeds {\$$|$${KEYCHAIN_REPL}|" $(RUSTPUSH_DIR)/src/icloud/keychain.rs && rm -f $(RUSTPUSH_DIR)/src/icloud/keychain.rs.bak; \
		fi; \
		if grep -q '^    let mut request = SignedRequest::new("id-register", Method::POST)$$' $(RUSTPUSH_DIR)/src/ids/user.rs 2>/dev/null && \
		   ! grep -q 'RUSTPUSH_LOG_REGISTER_BODY' $(RUSTPUSH_DIR)/src/ids/user.rs 2>/dev/null; then \
			echo "Patching user.rs to add env-gated REGISTER body XML dump (StatusKit reliability diagnostic; ports d77b1ac4)..."; \
			sed -i.bak 's|^    let mut request = SignedRequest::new("id-register", Method::POST)$$|    if std::env::var("RUSTPUSH_LOG_REGISTER_BODY").is_ok() { info!("REGISTER body XML: {}", plist_to_string(\&body).unwrap_or_default()); } let mut request = SignedRequest::new("id-register", Method::POST)|' $(RUSTPUSH_DIR)/src/ids/user.rs && rm -f $(RUSTPUSH_DIR)/src/ids/user.rs.bak; \
		fi; \
		if grep -q 'panic!("No saved channel for identifier!")' $(RUSTPUSH_DIR)/src/statuskit.rs 2>/dev/null; then \
			echo "Softening statuskit.rs:119 panic to warn+default APSChannel (StatusKit reliability)..."; \
			sed -i.bak 's|            panic!("No saved channel for identifier!")$$|            warn!("StatusKit: no saved channel for identifier — using last_msg_ns=0 (will replay)"); return APSChannel { identifier: channel.clone(), last_msg_ns: 0, subscribe: join };|' $(RUSTPUSH_DIR)/src/statuskit.rs && rm -f $(RUSTPUSH_DIR)/src/statuskit.rs.bak; \
		fi; \
		if grep -q 'else { panic!("Channel not found!") };' $(RUSTPUSH_DIR)/src/statuskit.rs 2>/dev/null; then \
			echo "Softening statuskit.rs:736 panic to warn+Ok(None) (StatusKit reliability — presence-before-keysharing race)..."; \
			sed -i.bak 's|let Some(referenced_channel) = state.keys.get_mut(&base64_encode(&channel.id)) else { panic!("Channel not found!") };|let Some(referenced_channel) = state.keys.get_mut(\&base64_encode(\&channel.id)) else { warn!("StatusKit: presence msg arrived before keysharing for channel={} — dropping", encode_hex(\&channel.id)); return Ok(None); };|' $(RUSTPUSH_DIR)/src/statuskit.rs && rm -f $(RUSTPUSH_DIR)/src/statuskit.rs.bak; \
		fi; \
	fi

# `ensure-rustpush-source` is an order-only prereq (the `|` separator):
# it runs before the recipe when needed, but its phony "always-dirty"
# timestamp doesn't force $(RUST_LIB) to rebuild on every `make` invocation.
# Only actual Rust source changes / Cargo.toml changes should trigger a
# rebuild; the pinned SHA + submodule setup is idempotent once done.
$(RUST_LIB): $(RUST_SRC) $(RUSTPUSH_SRC) $(CARGO_FILES) | ensure-rustpush-source
	cd pkg/rustpushgo && $(CARGO_ENV) cargo build --release $(CARGO_FEATURES)
	cp pkg/rustpushgo/target/release/librustpushgo.a .

rust: $(RUST_LIB)

# ===========================================================================
# Go bindings
# ===========================================================================

bindings: $(RUST_LIB)
	cd pkg/rustpushgo && uniffi-bindgen-go target/release/librustpushgo.a --library --out-dir ..
	python3 scripts/patch_bindings.py

# ===========================================================================
# Build
# ===========================================================================

# A single self-contained `corten-matrix` binary. The host-side ops
# (setup/start/stop/status/reset/install-service/…) are built into the binary as
# subcommands (see pkg/cli) — run `./corten-matrix help`. No .app bundle, no
# separate bbctl, no install scripts to wire up here.
build: check-deps $(RUST_LIB) $(BINARY)
	@echo "Built $(BINARY) ($(VERSION)-$(COMMIT)) — run './$(BINARY) help' for setup/ops"

$(BINARY): $(GO_SRC) $(shell find . -name '*.m' -o -name '*.h' 2>/dev/null | grep -v target) go.mod go.sum $(RUST_LIB) $(COMMIT_FILE)
	CGO_CFLAGS="$(CGO_CFLAGS)" CGO_LDFLAGS="$(CGO_LDFLAGS)" \
		go build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/$(CMD_PKG)/
	@# Sign with a STABLE identifier so macOS/TCC can track this binary across
	@# rebuilds (the arm64 linker otherwise leaves it as 'a.out', untrackable) —
	@# needed for the Full Disk Access probe to register it for chat.db backfill.
	codesign --force --sign - --identifier com.lrhodin.corten-matrix $(BINARY)

clean:
	rm -f $(APP_NAME) $(RUST_LIB)
	cd pkg/rustpushgo && cargo clean 2>/dev/null || true
