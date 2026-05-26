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
#   /root/.cargo/registry   - downloaded crate sources
#   /root/.cargo/git        - git-backed crate sources
#   /src/pkg/rustpushgo/target  - cargo build artifacts (the big one)
#   /src/third_party/rustpush-upstream/target  - same, for rustpush itself
#   /root/go                - GOPATH (Go module cache)
#   /root/.cache/go-build   - Go build cache
#
# `make build` runs ensure-rustpush-source → clones rustpush at the
# pinned SHA, overlays open-absinthe, applies every sed patch — then
# cargo build, then go build. Network is required during this step
# (BuildKit cache mounts don't replace network access for git clones).
RUN --mount=type=cache,target=/root/.cargo/registry,sharing=locked \
    --mount=type=cache,target=/root/.cargo/git,sharing=locked \
    --mount=type=cache,target=/src/pkg/rustpushgo/target,sharing=locked \
    --mount=type=cache,target=/src/third_party/rustpush-upstream/target,sharing=locked \
    --mount=type=cache,target=/root/go,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    make build

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
    gosu \
    && rm -rf /var/lib/apt/lists/*

# Apple Root CA — same source the bootstrap-linux.sh script fetches.
# Without this, identity.ess.apple.com fails TLS verification.
RUN wget -qO /tmp/AppleRootCA.cer https://www.apple.com/appleca/AppleIncRootCertificate.cer \
    && openssl x509 -inform DER -in /tmp/AppleRootCA.cer \
        -out /usr/local/share/ca-certificates/AppleRootCA.crt \
    && update-ca-certificates --fresh >/dev/null 2>&1 \
    && rm -f /tmp/AppleRootCA.cer

# Non-root user with a stable UID/GID. Matches the typical first Linux
# user (1000:1000), so a bind mount to ~/.local/share/mautrix-imessage on
# the host works without chown gymnastics. Users on hosts with a
# different UID can override via `user:` in compose.
#
# HOME is /data on purpose — bbctl persists its Beeper auth state under
# $HOME, and the bridge user's "home" needs to be on the bind-mounted
# volume so credentials survive container restarts. This is the same
# directory the bare-Linux install puts state in
# (~/.local/share/mautrix-imessage), so paths match cross-runtime.
RUN groupadd --system --gid 1000 bridge \
    && useradd --system --uid 1000 --gid 1000 --home-dir /data --shell /bin/bash bridge

# Copy the built binaries and the existing install scripts. The scripts
# stay as-is; the entrypoint dispatches to them. Three sections inside
# each script are gated on IN_DOCKER (no apt-install, no systemd, no
# host-bashrc aliases) — that gating is set by the entrypoint before exec.
COPY --from=builder /src/mautrix-imessage-v2 /usr/local/bin/mautrix-imessage-v2
COPY --from=builder /src/bbctl              /usr/local/bin/bbctl
COPY --from=builder /src/scripts/install-linux.sh        /opt/imessage/scripts/install-linux.sh
COPY --from=builder /src/scripts/install-beeper-linux.sh /opt/imessage/scripts/install-beeper-linux.sh

# Entrypoint + the imessage-setup convenience wrapper.
COPY scripts/docker-entrypoint.sh /entrypoint.sh
COPY scripts/imessage-setup.sh    /usr/local/bin/imessage-setup
RUN chmod +x /entrypoint.sh /usr/local/bin/imessage-setup /opt/imessage/scripts/*.sh

# State directory. WORKDIR /data is load-bearing: Rust hardcodes
# `state/anisette/` as a relative path (pkg/rustpushgo/src/lib.rs).
# Don't change it.
RUN mkdir -p /data && chown bridge:bridge /data /opt/imessage /usr/local/bin/bbctl /usr/local/bin/mautrix-imessage-v2 || true

WORKDIR /data
VOLUME ["/data"]
EXPOSE 29325

# Entrypoint starts as root (no USER directive). It chowns /data to
# the bridge user when needed — handles the case where Docker
# auto-created the bind-mount source as root because the host path
# didn't exist on first `docker compose up` — then drops privileges
# via gosu. The long-lived bridge process always runs as bridge (UID
# 1000 by default; override with PUID / PGID env vars in compose for
# hosts where appdata isn't owned by 1000, e.g. UNRAID, Synology).
ENTRYPOINT ["/entrypoint.sh"]
CMD ["run"]
