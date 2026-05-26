# Docker — Step-by-Step Guide

> **Linux only.** Docker Desktop on Mac runs the daemon inside a slow VM, gives you flaky bind mounts, and burns power for no reason when there's a native build that runs the bridge directly on the same machine. **Mac users always use `make install` or `make install-beeper` — never Docker.** The hardware key still gets extracted from a Mac, and the Apple Silicon NAC relay still runs on a Mac, but those are external machines from the Linux Docker host's perspective.

The bridge runs in a Docker container on a Linux host. The container itself is opaque — you drive it from the host with a small CLI called `imessage`, which is just a thin wrapper around `docker exec` / `docker compose` so you don't have to memorize either.

State lives on a bind mount you control. The image lives at `ghcr.io/lrhodin/imessage` (published manually via GitHub Actions, not auto-built per commit).

---

## What you need before starting

- A Linux host with Docker Engine 20.10+ and `docker compose` v2 (`docker compose version`, not `docker-compose`).
- A Beeper account or your own Matrix homeserver.
- A hardware key extracted once from a Mac (see [Quick Start (Linux) → Step 1](../README.md#step-1-extract-hardware-key-one-time-on-your-mac) in the main README — same extraction path whether the bridge later runs bare-Linux or in Docker).
- ~3 GB of disk for the image and ~1 GB for state, give or take.

---

## Fresh install

### Step 1 — Install the host CLI

```bash
curl -L https://raw.githubusercontent.com/lrhodin/imessage/master/scripts/imessage \
    | sudo install /dev/stdin /usr/local/bin/imessage
```

That puts `imessage` on `$PATH` for every user on the host. To verify:

```bash
imessage help
```

To update the CLI later, re-run the same `curl … install` line. To uninstall: `sudo rm /usr/local/bin/imessage`.

### Step 2 — Drop in a compose file

Pick a directory you'll keep `docker-compose.yml` in — anywhere is fine (`~/docker/imessage/`, `/srv/imessage/`, your home dir, etc.). From inside that directory:

```bash
curl -L https://raw.githubusercontent.com/lrhodin/imessage/master/docker-compose.example.yml \
    -o docker-compose.yml
```

### Step 3 — Edit two things in `docker-compose.yml`

Open it in your editor:

1. **`BEEPER` env var** — set to `"true"` for a Beeper deploy or `"false"` for a self-hosted homeserver.
2. **The bind mount** under `volumes:`. The default is `~/.local/share/mautrix-imessage:/data`, which matches the bare-Linux install path so migrating to or from Docker is trivial. Change the left side to whatever fits your platform:

   | Platform | Typical host path |
   |---|---|
   | Standard Linux | `~/.local/share/mautrix-imessage` |
   | UNRAID | `/mnt/user/appdata/mautrix-imessage` |
   | Synology | `/volume1/docker/mautrix-imessage` |
   | TrueNAS / ZFS | dataset of your choice |

   You don't need to `mkdir` the path yourself — Docker creates it on first start and the entrypoint chowns it for you.

If your host appdata is owned by a UID other than `1000` (UNRAID is often `99:100`, for example), also uncomment and set `PUID` / `PGID` to match. Find your host UID with `id -u` and `id -g`.

### Step 4 — Start the container

From the directory containing `docker-compose.yml`:

```bash
imessage start
```

The container pulls the image (first time only, ~250 MB), comes up, and sits idle waiting for setup. You can confirm with:

```bash
imessage status
imessage logs       # Ctrl-C to detach
```

The logs should show `no /data/config.yaml yet — run 'imessage setup' from the host to configure the bridge.` every 30 seconds — that's the entrypoint waiting on step 5.

### Step 5 — Run the setup wizard

```bash
imessage setup
```

This invokes the same install script the bare-Linux path uses, inside the container. For Beeper that means: bbctl login → bbctl config → iMessage login (paste the hardware key, Apple ID, password, 2FA). For self-hosted that means: homeserver URL / domain / Matrix ID / DB choice → iMessage login.

Then you'll be asked about CardDAV (if applicable), preferred handle, and the optional toggles for FaceTime, StatusKit, HEIC conversion, video transcoding.

When the wizard finishes, the container detects the new `config.yaml` and starts the bridge automatically. `imessage logs` should now show the bridge running. Go send yourself an iMessage to confirm the round trip.

### Step 6 (optional) — Install shell aliases

If you'd prefer typing `start-imessage` instead of `imessage start`, the CLI can drop the same alias block the bare-Linux installer uses into your shell rc file:

```bash
imessage install-aliases
```

This auto-detects bash vs zsh from `$SHELL` and writes to `~/.bashrc` or `~/.zshrc`. Open a new terminal (or `source` the rc file) to pick the aliases up. To remove them: `imessage uninstall-aliases`.

---

## Day-to-day operations

| Want to… | Run |
|---|---|
| Tail bridge logs | `imessage logs` |
| Check if it's running | `imessage status` |
| Restart the bridge | `imessage restart` |
| Stop it | `imessage stop` |
| Start it again | `imessage start` |
| Pull a new image + restart | `imessage update` |
| Re-run the iMessage login flow | `imessage login` |
| Re-run setup (flip a toggle) | `imessage setup` |
| Open a debug shell inside | `imessage shell` |

If you keep your compose file somewhere other than the current directory, set `IMESSAGE_COMPOSE_FILE`:

```bash
export IMESSAGE_COMPOSE_FILE=~/docker/imessage/docker-compose.yml
imessage update
```

---

## Migrating from a bare-Linux install

If you already have a bare-Linux install (the systemd-unit + `~/.local/share/mautrix-imessage` setup), you can move to Docker without losing state, login, or the iCloud Keychain trust circle:

### Step 1 — Install the host CLI

Same as the fresh-install Step 1:

```bash
curl -L https://raw.githubusercontent.com/lrhodin/imessage/master/scripts/imessage \
    | sudo install /dev/stdin /usr/local/bin/imessage
```

### Step 2 — Run the migration helper

```bash
imessage migrate
```

This:
1. Strips the bare-Linux systemd-based shell aliases from `~/.bashrc` / `~/.zshrc` (they shell out to `systemctl` and would no longer work).
2. Stops, disables, and removes the `mautrix-imessage` systemd unit (user and system scopes — uses `sudo` only for the system scope).
3. Confirms your state at `~/.local/share/mautrix-imessage` is intact.

It's idempotent. Safe to re-run if something didn't complete the first time.

### Step 3 — Drop in the compose file

```bash
curl -L https://raw.githubusercontent.com/lrhodin/imessage/master/docker-compose.example.yml \
    -o docker-compose.yml
```

The default bind mount (`~/.local/share/mautrix-imessage`) already points at your existing state, so no edits to volumes are needed unless your host UID isn't 1000 (set `PUID`/`PGID`) or your bare-Linux install used a non-default state directory.

### Step 4 — Start

```bash
imessage start
```

The bridge resumes against your existing config / session / trustedpeers / sqlite as-is. No re-login, no re-setup. `imessage logs` should show the bridge picking up where it left off within a few seconds.

If you want to flip toggles later: `imessage setup`. If you want to re-login: `imessage login`.

---

## How the privilege model works

The container's CMD is invoked as root, but only briefly. The entrypoint:

1. Reads `PUID` / `PGID` from compose (default 1000:1000).
2. Aligns the bundled `bridge` user to that UID/GID.
3. Creates `/data` if Docker didn't (happens when the host bind-mount path didn't exist on first start).
4. `chown`s `/data` to `PUID:PGID` only if it doesn't already match.
5. Re-execs itself via `gosu` as the bridge user.

After step 5, the long-lived bridge process is non-root. The whole root window is a few milliseconds at container start.

---

## Apple Silicon NAC relay

If your hardware key was extracted from an Apple Silicon Mac, the bridge fetches NAC validation data from a relay running on that Mac. The relay URL, bearer token, and TLS fingerprint are all embedded in the base64 key blob — there's nothing to configure in compose.

The Mac running the relay is a separate machine from the Docker host. The key needs to have been extracted with a hostname/IP that's routable from your Docker host (LAN, VPN, or port-forwarded WAN). Intel hardware keys don't need a relay at all — the x86_64 unicorn emulator runs entirely in-process inside the container.

---

## Troubleshooting

**`imessage start` says "Cannot connect to the Docker daemon"** — make sure Docker is running and your user is in the `docker` group. Log out and back in after adding yourself.

**`imessage logs` keeps showing `no /data/config.yaml yet`** — you never finished `imessage setup`, or the wizard exited mid-flow. Run it again; it's idempotent.

**Container restarts in a loop** — `imessage logs` shows the actual error. Most common cause: UID mismatch between the host appdata dir and the container's `PUID`. Check `stat -c '%u:%g' <your host path>` and set `PUID`/`PGID` in compose to match.

**Bridge starts but Beeper doesn't see it** — `imessage logs` should show the appservice connecting. If it doesn't, the registration / login didn't complete. Re-run `imessage setup`.

**"403 Forbidden" or "401 Unauthorized" from Apple IDS** — your hardware key may have aged out or been invalidated. Re-extract the key on the Mac and re-run `imessage login`.

**Need a shell inside the container** — `imessage shell` (drops you into bash as the bridge user).

---

## Out of scope

- **`backfill_source: chatdb`** doesn't work in Docker (macOS-only, Full Disk Access required). Use CloudKit backfill.
- **macOS Contacts framework** — same reason. Use the external CardDAV path for non-iCloud contacts.
- **`extract-key` / NAC relay GUIs** — those run on the user's Mac to mint the hardware key, not inside the bridge container.

---

## Updating

```bash
imessage update
```

This pulls the latest image tag your compose file points at (defaults to `:latest`) and recreates the container. State on the bind mount is preserved.

To pin to a specific version, change the `image:` line in `docker-compose.yml` to `ghcr.io/lrhodin/imessage:sha-<7chars>` or a tagged release.
