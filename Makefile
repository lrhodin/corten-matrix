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
# apple-private-apis (icloud-auth, omnisette, clearadi) is a rustpush submodule.
# Upstream moved it from <root>/apple-private-apis to <root>/third_party/
# apple-private-apis at 0e0f13c; the submodule commit itself is unchanged. Keep
# the location in one variable so a future move is a one-line edit rather than a
# hunt through the patch block — where a stale path silently no-ops every guard.
APA_DIR      := $(RUSTPUSH_DIR)/third_party/apple-private-apis
# Pinned OpenBubbles/rustpush commit. Edit third_party/rustpush-upstream.sha to
# bump, then test locally before committing. The Makefile reads the SHA on build
# and checks out that exact commit — no auto-bump, no branch drift.
RUSTPUSH_PIN_FILE := third_party/rustpush-upstream.sha
RUSTPUSH_PIN      := $(shell cat $(RUSTPUSH_PIN_FILE) 2>/dev/null)
RUSTPUSH_SRC:= $(shell find $(RUSTPUSH_DIR)/src $(APA_DIR) $(RUSTPUSH_DIR)/open-absinthe/src -name '*.rs' -o -name '*.s' 2>/dev/null) $(wildcard $(RUSTPUSH_DIR)/open-absinthe/build.rs)
# Patches applied to the pinned tree by ensure-rustpush-source. They and this
# Makefile are prerequisites of the Rust archive because make stats the tree's
# .rs files before that recipe patches them: a patch that lands during the run
# would otherwise not rebuild the archive until the next invocation.
PATCH_FILES := $(shell find third_party/patches -name '*.patch' 2>/dev/null)
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

# Homebrew prefix differs by architecture: /opt/homebrew on Apple Silicon,
# /usr/local on Intel. Detect an installed brew first; if this is a fresh
# install, select the architecture's normal target so check-deps installs and
# later CGO compilation use the same location. Override with
# `make BREW_PREFIX=...` (or an exported BREW_PREFIX) for non-standard installs.
#
# Keep the detected value simply-expanded so this shell probe runs once per make
# invocation, while still preserving both environment and command-line overrides.
ifeq ($(origin BREW_PREFIX),undefined)
BREW_PREFIX := $(shell \
	if [ -x /opt/homebrew/bin/brew ]; then echo /opt/homebrew; \
	elif [ -x /usr/local/bin/brew ]; then echo /usr/local; \
	elif command -v brew >/dev/null 2>&1; then brew --prefix; \
	elif [ "$$(uname -m)" = x86_64 ]; then echo /usr/local; \
	else echo /opt/homebrew; fi)
endif

export PATH := $(BREW_PREFIX)/bin:$(BREW_PREFIX)/sbin:$(PATH)
BINARY      := $(APP_NAME)
CGO_CFLAGS  := -I$(BREW_PREFIX)/include
CGO_LDFLAGS := -L$(BREW_PREFIX)/lib -L$(CURDIR)
CARGO_ENV   := MACOSX_DEPLOYMENT_TARGET=13.0

# ===========================================================================
# Dependency checks
# ===========================================================================

# Auto-install missing build deps via Homebrew.
check-deps:
	@if ! command -v brew >/dev/null 2>&1; then \
		echo "Installing Homebrew..."; \
		NONINTERACTIVE=1 /bin/bash -c "$$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"; \
		eval "$$("$(BREW_PREFIX)/bin/brew" shellenv)"; \
	fi; \
	missing=""; \
	command -v go >/dev/null 2>&1    || missing="$$missing go"; \
	command -v cargo >/dev/null 2>&1 || missing="$$missing rust"; \
	command -v protoc >/dev/null 2>&1|| missing="$$missing protobuf"; \
	command -v tmux >/dev/null 2>&1  || missing="$$missing tmux"; \
	[ -f "$(BREW_PREFIX)/include/olm/olm.h" ] || missing="$$missing libolm"; \
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

# ---------------------------------------------------------------------------
# source-build patches — every one of them PROVEN to have landed.
# ---------------------------------------------------------------------------
# Each patch below goes through RP_PATCH, whose fail-closed semantics mirror the
# build.sh helper. Shared vendored-tree patches should remain aligned where both
# build lanes use them; public source-build compatibility patches may be present
# only here and must be verified against this repository's exact pinned tree.
#
#   rp_patch <label> <file> <perl-expr> <verify-ere>
#
# It verifies the DESIRED END STATE, never "did perl fire", so:
#   target file missing    -> FAIL  (upstream moved something; fix the path)
#   verify already matches -> skip  (upstream fixed it themselves; not an error)
#   still no match after   -> FAIL  (upstream reworded the anchor; fix the expr)
#
# The old form was `if grep -q <pre-patch-anchor>; then sed -i.bak ...; fi`:
# a moved file or a reworded anchor was not an error, it was a silent skip. When
# upstream relocated apple-private-apis under third_party/ that turned four
# patches into no-ops with no output at all. Anything that does not land now
# stops the build and names the patch.
#
# Changes too large for a substitution live as unified diffs under
# third_party/patches/ and go through rp_apply, which proves the same end state
# with the same three outcomes, using the complete patch as the test:
#
#   rp_apply <label> <apply-root> <patch-file> <verify-file-relative-to-root> <verify-ere>
#
#   patch reverse-applies and marker present -> skip  (already adopted upstream)
#   marker present but patch does not reverse-apply -> FAIL (partial tree; reset it)
#   otherwise `git apply --check`, apply, marker must appear -> else FAIL
RP_PATCH = rp_patch() { \
	  rp_label="$$1"; rp_file="$$2"; rp_expr="$$3"; rp_verify="$$4"; \
	  if [ ! -f "$$rp_file" ]; then \
	    echo "error: rustpush patch '$$rp_label': target file missing: $$rp_file (did upstream move it?)" >&2; \
	    return 1; \
	  fi; \
	  if grep -qE "$$rp_verify" "$$rp_file"; then \
	    echo "  patch $$rp_label (already satisfied upstream)"; \
	    return 0; \
	  fi; \
	  perl -i -pe "$$rp_expr" "$$rp_file" \
	    || { echo "error: rustpush patch '$$rp_label': perl failed on $$rp_file" >&2; return 1; }; \
	  if ! grep -qE "$$rp_verify" "$$rp_file"; then \
	    echo "error: rustpush patch '$$rp_label' did not apply to $$rp_file — the anchor no longer matches upstream" >&2; \
	    return 1; \
	  fi; \
	  echo "  patch $$rp_label"; \
	}; \
	rp_apply() { \
	  rp_label="$$1"; rp_root="$$2"; rp_file="$$3"; rp_target="$$2/$$4"; rp_verify="$$5"; \
	  case "$$rp_file" in /*) ;; *) rp_file="$$PWD/$$rp_file" ;; esac; \
	  if [ ! -f "$$rp_file" ]; then \
	    echo "error: rustpush patch '$$rp_label': patch file missing: $$rp_file" >&2; \
	    return 1; \
	  fi; \
	  if [ ! -f "$$rp_target" ]; then \
	    echo "error: rustpush patch '$$rp_label': target file missing: $$rp_target (did upstream move it?)" >&2; \
	    return 1; \
	  fi; \
	  if git -C "$$rp_root" apply --reverse --unidiff-zero --whitespace=fix --check "$$rp_file" >/dev/null 2>&1; then \
	    if ! grep -qE "$$rp_verify" "$$rp_target"; then \
	      echo "error: rustpush patch '$$rp_label' is fully present but its end-state marker is absent from $$rp_target" >&2; \
	      return 1; \
	    fi; \
	    echo "  patch $$rp_label (already satisfied upstream)"; \
	    return 0; \
	  fi; \
	  if grep -qE "$$rp_verify" "$$rp_target"; then \
	    echo "error: rustpush patch '$$rp_label' is only partly present in $$rp_root — delete $(RUSTPUSH_DIR) and rebuild" >&2; \
	    return 1; \
	  fi; \
	  git -C "$$rp_root" apply --unidiff-zero --whitespace=fix --check "$$rp_file" \
	    || { echo "error: rustpush patch '$$rp_label' does not apply to the pinned tree — refresh $$rp_file" >&2; return 1; }; \
	  git -C "$$rp_root" apply --unidiff-zero --whitespace=fix "$$rp_file" \
	    || { echo "error: rustpush patch '$$rp_label' failed after a successful preflight" >&2; return 1; }; \
	  if ! grep -qE "$$rp_verify" "$$rp_target"; then \
	    echo "error: rustpush patch '$$rp_label' applied but did not produce its end state in $$rp_target" >&2; \
	    return 1; \
	  fi; \
	  echo "  patch $$rp_label"; \
	}; \
	[ "$(RUSTPUSH_DIR)" = "third_party/rustpush-upstream" ] || exit 0;

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
			git -C third_party/rustpush-upstream clean -ffd || exit 1; \
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
	fi
# The rustpushgo anisette-aoskit / anisette-remote-v3 features reference
# omnisette/prefer-* features that upstream omnisette doesn't define. cargo
# validates the whole [features] table even though this build never selects
# them, so define them here as harmless no-ops.
	@$(RP_PATCH) rp_patch "omnisette prefer-* no-op features" $(APA_DIR)/omnisette/Cargo.toml \
	  's/^(remote-clearadi = .*)$$/$$1\nprefer-aoskit = []\nprefer-remote-anisette-v3 = []/' \
	  '^prefer-aoskit'
# Upstream gates the proto decode on msgType 1..=2 and throws the payload away
# for anything else. Real user messages arrive from CloudKit as msgType 0 (see
# sync_controller.go:3369) — without this they decode empty, land with
# has_body=0, and ensureSchema's cleanup DELETEs them: silent history loss.
	@$(RP_PATCH) rp_patch "decode msgType=0 as a message" $(RUSTPUSH_DIR)/src/imessage/cloud_messages.rs \
	  's/^                1\.\.=2 => CloudMessageType::Message \{/                0..=2 => CloudMessageType::Message {/' \
	  '^                0\.\.=2 => CloudMessageType::Message \{'
# ids_guard + the IDS query call sites use rustpush::ids::*.
	@$(RP_PATCH) rp_patch "pub mod ids" $(RUSTPUSH_DIR)/src/lib.rs \
	  's/^mod ids;$$/pub mod ids;/' \
	  '^pub mod ids;'
# FetchedToken's fields are private upstream; we construct one to replay the
# persisted PET on session restore (breaks the daily-2FA loop). The type itself
# has been re-exported from icloud-auth since the 7725a32 pin (apple-private-apis
# b8598d2), so only the field visibility still needs patching here; the private
# build lane retired its re-export patch at the same bump.
	@$(RP_PATCH) rp_patch "FetchedToken.token pub" $(APA_DIR)/icloud-auth/src/client.rs \
	  's/^    token: String,$$/    pub token: String,/' \
	  '^    pub token: String,'
	@$(RP_PATCH) rp_patch "FetchedToken.expiration pub" $(APA_DIR)/icloud-auth/src/client.rs \
	  's/^    expiration: SystemTime,$$/    pub expiration: SystemTime,/' \
	  '^    pub expiration: SystemTime,'
# Upstream unwraps the base64, the UTF-8, the token field, and the expiry of
# every X-Apple-GS/HB/PE-Token header, and reads the HB expiry as now plus a
# raw epoch-millisecond value. One fallible parser turns a malformed header
# into Error::BadTokenHeader instead of a panic that unwinds through the Go
# host. Too large for a substitution; the release build lane carries the same
# diff. Applied after the two field patches above, which it does not touch.
	@$(RP_PATCH) rp_apply "fallible GSA token header parsing" $(APA_DIR) \
	  third_party/patches/apple-private-apis/fallible-gsa-token-header-parsing.patch \
	  icloud-auth/src/client.rs '^fn parse_token_header\('
# Ignore self-exclusion in fast_forward_trust (Clique self-eviction fix; ports 9f29ff1).
	@$(RP_PATCH) rp_patch "keychain self-exclusion" $(RUSTPUSH_DIR)/src/icloud/keychain.rs \
	  's/^            for excluded in &trust\.excludeds \{$$/            let my_id = &state.user_identity.as_ref().unwrap().identifier;\n            for excluded in &trust.excludeds {\n                if excluded == my_id {\n                    warn!(\n                        "Ignoring exclusion of ourselves ({}) from peer {}",\n                        excluded,\n                        peer.0.hash.as_ref().unwrap()\n                    );\n                    continue;\n                }/' \
	  'Ignoring exclusion of ourselves'
# Default passcode_generation when the escrow record omits it (deserialize
# compat; ports OpenBubbles/rustpush#23).
	@$(RP_PATCH) rp_patch "keychain passcode_generation default" $(RUSTPUSH_DIR)/src/icloud/keychain.rs \
	  's/^    pub passcode_generation: u32,$$/    #[serde(default)] pub passcode_generation: u32,/' \
	  '#\[serde\(default\)\] pub passcode_generation: u32,'
# Env-gated REGISTER body XML dump (StatusKit reliability diagnostic; ports
# d77b1ac4). Off unless RUSTPUSH_LOG_REGISTER_BODY is set.
	@$(RP_PATCH) rp_patch "REGISTER body XML dump" $(RUSTPUSH_DIR)/src/ids/user.rs \
	  's/^    let mut request = SignedRequest::new\("id-register", Method::POST\)$$/    if std::env::var("RUSTPUSH_LOG_REGISTER_BODY").is_ok() { info!("REGISTER body XML: {}", plist_to_string(&body).unwrap_or_default()); } let mut request = SignedRequest::new("id-register", Method::POST)/' \
	  'RUSTPUSH_LOG_REGISTER_BODY'
# Soften statuskit.rs:119 panic to warn + default APSChannel (StatusKit reliability).
	@$(RP_PATCH) rp_patch "statuskit no-saved-channel panic" $(RUSTPUSH_DIR)/src/statuskit.rs \
	  's/            panic!\("No saved channel for identifier!"\)$$/            warn!("StatusKit: no saved channel for identifier — using last_msg_ns=0 (will replay)"); return APSChannel { identifier: channel.clone(), last_msg_ns: 0, subscribe: join };/' \
	  'using last_msg_ns=0 \(will replay\)'
# Soften statuskit.rs:736 panic to warn + Ok(None) (presence-before-keysharing race).
	@$(RP_PATCH) rp_patch "statuskit channel-not-found panic" $(RUSTPUSH_DIR)/src/statuskit.rs \
	  's/let Some\(referenced_channel\) = state\.keys\.get_mut\(&base64_encode\(&channel\.id\)\) else \{ panic!\("Channel not found!"\) \};/let Some(referenced_channel) = state.keys.get_mut(&base64_encode(&channel.id)) else { warn!("StatusKit: presence msg arrived before keysharing for channel={} — dropping", encode_hex(&channel.id)); return Ok(None); };/' \
	  'presence msg arrived before keysharing'

# The pinned public omnisette exposes the two-argument default_provider in all
# feature shapes available to this source build. Keep the wrapper's device_id
# parameter for its callers, but compile the fetched public tree against the
# API it actually provides.
	@$(RP_PATCH) rp_patch "public omnisette provider comment" pkg/rustpushgo/src/lib.rs \
	  's#^    // The omnisette provider selected with `cleanroom-registration` takes a$$#    // The pinned public upstream provider accepts login info and its state#; s#^    // `device_id`; upstream OpenBubbles omnisette.s `default_provider` is 2-arg\.$$#    // path in every feature shape used by this source build.#; s#^    // Gate on the feature so both signatures build: with cleanroom-registration$$#    // Keep device_id at the wrapper boundary for callers while using that#; s#^    // the 3-arg call is used; without it \(this from-source build\) the 2-arg$$#    // two-argument API.#; s#^    // upstream signature is used\.$$#    // Both cfg arms intentionally use the same public signature.#' \
	  '^    // Both cfg arms intentionally use the same public signature\.$$'
	@$(RP_PATCH) rp_patch "public omnisette default_provider is 2-arg" pkg/rustpushgo/src/lib.rs \
	  's/^        omnisette::default_provider\(info, path, device_id\)$$/        let _ = device_id; \/\/ pinned public provider is two-argument\n        omnisette::default_provider(info, path)/' \
	  'pinned public provider is two-argument'

# The public contacts-auth dependency can be unavailable. Add a source-build
# opt-out that leaves external CardDAV and local ChatDB Contacts ahead of the
# disabled iCloud branch, and suppresses remote shared-profile refreshes while
# preserving cached profile rendering, messages, and history backfill.
	@$(RP_PATCH) rp_patch "disable iCloud contacts config field" pkg/connector/config.go \
	  's/^(	CardDAV CardDAVConfig `yaml:"carddav"`\n)/$$1\n	\/\/ DisableICloudContacts disables iCloud CardDAV and remote shared-profile refresh.\n	DisableICloudContacts bool `yaml:"disable_icloud_contacts"`\n/' \
	  'DisableICloudContacts bool `yaml:"disable_icloud_contacts"`'
	@$(RP_PATCH) rp_patch "disable iCloud contacts config upgrade" pkg/connector/config.go \
	  's/^(	helper\.Copy\(up\.Bool, "typing_notifications"\)\n)/$$1	helper.Copy(up.Bool, "disable_icloud_contacts")\n/' \
	  'helper\.Copy\(up\.Bool, "disable_icloud_contacts"\)'
	@$(RP_PATCH) rp_patch "disable iCloud contacts example config" pkg/imconfig/example-config.yaml \
	  's/^(# External CardDAV server for contact name resolution\.\n)/# Disable iCloud CardDAV and remote shared-profile refresh when the public\n# contacts-auth path is unavailable. Messages and history backfill continue.\ndisable_icloud_contacts: false\n\n$$1/' \
	  '^disable_icloud_contacts: false$$'
# These anchors are inside the final iCloud branch, after external CardDAV and
# local ChatDB Contacts have already been selected. They therefore cannot
# pre-empt either higher-priority source. The constructor is also gated so the
# disabled path never enters the public contacts-auth implementation.
	@$(RP_PATCH) rp_patch "skip iCloud CardDAV client construction when disabled" pkg/connector/client.go \
	  's/^		cloudContacts := newCloudContactsClient\(c\.client, log\)$$/		var cloudContacts *cloudContactsClient\n		if !c.Main.Config.DisableICloudContacts {\n			cloudContacts = newCloudContactsClient(c.client, log)\n		}/' \
	  '^		if !c\.Main\.Config\.DisableICloudContacts \{$$'
	@$(RP_PATCH) rp_patch "skip iCloud CardDAV setup when disabled" pkg/connector/client.go \
	  's/^		if cloudContacts != nil \{$$/		if c.Main.Config.DisableICloudContacts {\n			log.Info().Msg("iCloud contacts disabled before CardDAV setup")\n		} else if cloudContacts != nil {/' \
	  'iCloud contacts disabled before CardDAV setup'
# refreshAllSharedProfiles is shared by startup and periodic refresh paths. A
# central guard suppresses both network paths while the startup caller still
# applies cached profiles before reaching this function.
	@$(RP_PATCH) rp_patch "skip remote shared-profile refresh when iCloud contacts disabled" pkg/connector/shared_profile.go \
	  's/^	if c\.sharedProfileStore == nil \|\| c\.client == nil \{$$/	if c.Main.Config.DisableICloudContacts || c.sharedProfileStore == nil || c.client == nil {/' \
	  '^	if c\.Main\.Config\.DisableICloudContacts \|\| c\.sharedProfileStore == nil \|\| c\.client == nil \{$$'

# `ensure-rustpush-source` is an order-only prereq (the `|` separator):
# it runs before the recipe when needed, but its phony "always-dirty"
# timestamp doesn't force $(RUST_LIB) to rebuild on every `make` invocation.
# Only actual Rust source changes / Cargo.toml changes should trigger a
# rebuild; the pinned SHA + submodule setup is idempotent once done.
$(RUST_LIB): $(RUST_SRC) $(RUSTPUSH_SRC) $(CARGO_FILES) $(PATCH_FILES) Makefile | ensure-rustpush-source
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
