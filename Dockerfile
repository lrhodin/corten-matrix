# syntax=docker/dockerfile:1.6
# ============================================================================
# mautrix-imessage-v2 — Docker image
#
# Built and published by CI (.github/workflows/docker.yml) to
# ghcr.io/lrhodin/imessage. End users pull from GHCR; local builds are
# not a supported user path.
#
# Stage 1 (builder) runs `make build`, which triggers ensure-rustpush-source
# in the Makefile. That clones OpenBubbles/rustpush at the pinned SHA
# (third_party/rustpush-upstream.sha) and applies every overlay + sed patch.
# Result: bridge binary contains the same patched rustpush as bare-Linux.
#
# Stage 2 (runtime) is bookworm-slim + the runtime closures (libolm3 etc.)
# + the Apple Root CA + the two existing install scripts.
# ============================================================================

# ─── Stage 1: builder ────────────────────────────────────────────────────────
FROM golang:1.25-bookworm AS builder

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends \
    build-essential \
    cmake \
    pkg-config \
    git \
    curl \
    wget \
    ca-certificates \
    openssl \
    sqlite3 \
    libolm-dev \
    libclang-dev \
    libssl-dev \
    libunicorn-dev \
    libheif-dev \
    libsqlite3-dev \
    zlib1g-dev \
    protobuf-compiler \
    && rm -rf /var/lib/apt/lists/*

# Rustup (stable). Matches bootstrap-linux.sh's minimum (1.88+).
RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
    | sh -s -- -y --default-toolchain stable --profile minimal
ENV PATH=/root/.cargo/bin:$PATH

# Apple Root CA — bootstrap-linux.sh (invoked by `make check-deps`) tries to
# install this via `sudo`, which doesn't exist in this builder. Install it
# here ahead of time so bootstrap-linux.sh's openssl check passes and it
# skips the sudo path. Same source/destination the runtime stage uses.
RUN wget -qO /tmp/AppleRootCA.cer https://www.apple.com/appleca/AppleIncRootCertificate.cer \
    && openssl x509 -inform DER -in /tmp/AppleRootCA.cer \
        -out /usr/local/share/ca-certificates/AppleRootCA.crt \
    && update-ca-certificates --fresh >/dev/null 2>&1 \
    && rm -f /tmp/AppleRootCA.cer

WORKDIR /src
COPY . /src

# Stub out bootstrap-linux.sh inside the builder. Every dep it would
# install is already in the apt layer above, but its dedup leaves
# APT_PACKAGES=" " (single space) when nothing's missing, so the
# `[ -n "$APT_PACKAGES" ]` guard fires and the script tries to `sudo
# apt-get install` an empty list. sudo isn't in the builder image
# either. Bypassing the whole script is cleaner than installing sudo +
# fixing the dedup bug upstream.
RUN echo '#!/bin/bash' > scripts/bootstrap-linux.sh \
    && chmod +x scripts/bootstrap-linux.sh

# Build with BuildKit cache mounts. CI exports these as part of
# `cache-to: type=gha,mode=max` so subsequent runs reuse compiled
# crates and Go modules instead of rebuilding rustpush from scratch
# (~5 min saved per run once the cache is warm).
#
# Caches mounted:
#   /root/.cargo/registry         - downloaded crate sources
#   /root/.cargo/git              - git-backed crate sources
#   /src/pkg/rustpushgo/target    - cargo build artifacts (the big one;
#                                   rustpush builds here as a workspace
#                                   dep, no separate cache needed)
#   /root/go                      - GOPATH (Go module cache)
#   /root/.cache/go-build         - Go build cache
#
# We can't cache /src/third_party/rustpush-upstream/ — BuildKit would
# auto-create the parent on mount, and `ensure-rustpush-source` then
# refuses to git clone into the non-empty dir. Cargo handles those
# artifacts in pkg/rustpushgo/target anyway.
#
# `make build` runs ensure-rustpush-source → clones rustpush at the
# pinned SHA, overlays open-absinthe, applies every sed patch — then
# cargo build, then go build. Network is required during this step
# (cache mounts don't replace network access for git clones).
RUN --mount=type=cache,target=/root/.cargo/registry,sharing=locked \
    --mount=type=cache,target=/root/.cargo/git,sharing=locked \
    --mount=type=cache,target=/src/pkg/rustpushgo/target,sharing=locked \
    --mount=type=cache,target=/root/go,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    make build

# Verify every rustpush patch landed in the cloned source tree. Mirrors
# the `verify-rustpush-patches` job in .github/workflows/ci.yml exactly
# so the Docker image can't ship a binary that's missing one of these
# patches (which would happen silently if upstream reworded a guarded
# line and the sed in ensure-rustpush-source quietly skipped it).
#
# Prints a one-line-per-patch status + a summary line at the end. Fails
# the build if ANY patch marker is missing — the only safe failure mode
# for a binary that depends on every one of these.
RUN set -e; \
    BASE=third_party/rustpush-upstream; \
    PASS=0; FAIL=0; TOTAL=0; \
    check() { \
        local desc="$1" file="$2" marker="$3"; \
        TOTAL=$((TOTAL+1)); \
        if grep -Fq "$marker" "$file" 2>/dev/null; then \
            echo "  ✓ $desc"; \
            PASS=$((PASS+1)); \
        else \
            echo "  ✗ $desc — marker not found in $file"; \
            FAIL=$((FAIL+1)); \
        fi; \
    }; \
    echo ""; \
    echo "═══════════════════════════════════════════════════════════"; \
    echo "  Verifying rustpush patches are applied to built binary"; \
    echo "═══════════════════════════════════════════════════════════"; \
    check "activation pub"                       "$BASE/src/lib.rs"                                   "pub mod activation;"; \
    check "ids pub"                              "$BASE/src/lib.rs"                                   "pub mod ids;"; \
    check "FetchedToken.token pub"               "$BASE/apple-private-apis/icloud-auth/src/client.rs" "pub token: String,"; \
    check "FetchedToken.expiration pub"          "$BASE/apple-private-apis/icloud-auth/src/client.rs" "pub expiration: SystemTime,"; \
    check "FetchedToken re-export"               "$BASE/apple-private-apis/icloud-auth/src/lib.rs"    "pub use client::{AppleAccount, FetchedToken,"; \
    check "keychain self-exclusion fix"          "$BASE/src/icloud/keychain.rs"                       "Ignoring exclusion of ourselves"; \
    check "register XML dump env-gate"           "$BASE/src/ids/user.rs"                              "RUSTPUSH_LOG_REGISTER_BODY"; \
    check "statuskit no-saved-channel softened"  "$BASE/src/statuskit.rs"                             "no saved channel for identifier"; \
    check "statuskit channel-not-found softened" "$BASE/src/statuskit.rs"                             "presence msg arrived before keysharing"; \
    echo "═══════════════════════════════════════════════════════════"; \
    echo "  Patch status: $PASS / $TOTAL applied"; \
    if [ "$FAIL" -ne 0 ]; then \
        echo "  ✗ FAIL — $FAIL patch(es) missing. Image will NOT be pushed."; \
        echo "═══════════════════════════════════════════════════════════"; \
        exit 1; \
    fi; \
    echo "  ✓ OK — all rustpush patches applied to the shipped binary."; \
    echo "═══════════════════════════════════════════════════════════"; \
    echo ""

# ─── Stage 2: runtime ────────────────────────────────────────────────────────
FROM debian:bookworm-slim AS runtime

ENV DEBIAN_FRONTEND=noninteractive

# Runtime closures. libunicorn2 + libheif1 + libolm3 + libssl3 are the
# dynamic deps the bridge binary links against; ffmpeg is needed at
# runtime when video_transcoding is enabled in config; ca-certificates +
# the Apple Root CA below give IDS endpoints a valid trust path.
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends \
    libolm3 \
    libunicorn2 \
    libssl3 \
    libheif1 \
    ca-certificates \
    ffmpeg \
    wget \
    bash \
    coreutils \
    sed \
    grep \
    python3 \
    sqlite3 \
    openssl \
    && rm -rf /var/lib/apt/lists/*

# Apple Root CA — same source the bootstrap-linux.sh script fetches.
# Without this, identity.ess.apple.com fails TLS verification.
RUN wget -qO /tmp/AppleRootCA.cer https://www.apple.com/appleca/AppleIncRootCertificate.cer \
    && openssl x509 -inform DER -in /tmp/AppleRootCA.cer \
        -out /usr/local/share/ca-certificates/AppleRootCA.crt \
    && update-ca-certificates --fresh >/dev/null 2>&1 \
    && rm -f /tmp/AppleRootCA.cer

# Non-root user defined at a stable UID/GID. The container's PID 1 starts
# as root so the entrypoint can chown bind mounts and create host-path
# symlinks; it then setpriv-drops to PUID:PGID (defaults 1000:1000, from
# env vars in compose) and re-execs to run the bridge.
#
# Home-directory layout mirrors bare-Linux exactly so install scripts
# that build $HOME/.local/share/mautrix-imessage/... paths land in
# the same on-disk locations they would on bare-Linux:
#
#   $HOME                                       = /home/bridge
#   /home/bridge/.local/share/mautrix-imessage  → symlink to /data
#   /home/bridge/.config                        ← parent for user config
#
RUN groupadd --system --gid 1000 bridge \
    && useradd --system --uid 1000 --gid 1000 --create-home --shell /bin/bash bridge \
    && mkdir -p /home/bridge/.local/share /home/bridge/.config \
    && ln -sf /data /home/bridge/.local/share/mautrix-imessage \
    && chown -R bridge:bridge /home/bridge \
    && mkdir -p /data

# Copy the built binaries and the existing install scripts. The scripts
# stay as-is; the entrypoint dispatches to them. Three sections inside
# each script are gated on IN_DOCKER (no apt-install, no systemd, no
# host-bashrc aliases) — that gating is set by the entrypoint before exec.
COPY --from=builder /src/mautrix-imessage-v2 /usr/local/bin/mautrix-imessage-v2
COPY --from=builder /src/bbctl              /usr/local/bin/bbctl
COPY --from=builder /src/scripts/install-linux.sh        /opt/imessage/scripts/install-linux.sh
COPY --from=builder /src/scripts/install-beeper-linux.sh /opt/imessage/scripts/install-beeper-linux.sh

# Entrypoint + the imessage-setup convenience wrapper + as-bridge.
#
# as-bridge re-applies the entrypoint's setpriv drop for any command
# invoked via `docker exec`. exec inherits the container's USER (root
# here, because the entrypoint needs root for chown/symlink at PID 1),
# so without this wrapper a host-side `imessage bbctl …` would run
# bbctl as root and write root-owned files into the bbctl bind mount.
COPY scripts/docker-entrypoint.sh /entrypoint.sh
COPY scripts/imessage-setup.sh    /usr/local/bin/imessage-setup
COPY scripts/as-bridge.sh         /usr/local/bin/as-bridge
RUN chmod +x /entrypoint.sh /usr/local/bin/imessage-setup /usr/local/bin/as-bridge /opt/imessage/scripts/*.sh

# State directory. WORKDIR /data is load-bearing: Rust hardcodes
# `state/anisette/` as a relative path (pkg/rustpushgo/src/lib.rs).
# Don't change it. The entrypoint chowns this to PUID:PGID on first
# start (and only when find spots a mismatched file thereafter).
RUN mkdir -p /data

WORKDIR /data
VOLUME ["/data"]
EXPOSE 29325

# XDG env vars left unset on purpose. User config persistence is defined by
# docker-compose.yml bind mounts, not by image-level path overrides.

# No USER directive — PID 1 enters /entrypoint.sh as root so it can chown
# bind mounts to PUID:PGID and create the host-source → /data symlink.
# The prelude then `setpriv`s to PUID:PGID (defaults 1000:1000) and
# re-execs the script for the actual bridge run. Override the target
# UID/GID via env vars in docker-compose.yml:
#
#   environment:
#     PUID: "99"       # UNRAID nobody
#     PGID: "100"      # UNRAID users
#
# Both fixes (perms + symlink) are conditional — they no-op when the
# bind mount is already in the right shape, so the prelude is a quiet
# `find -quit` on subsequent starts.

ENTRYPOINT ["/entrypoint.sh"]
CMD ["run"]
