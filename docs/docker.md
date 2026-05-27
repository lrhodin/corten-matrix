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

One line:

```bash
curl -fsSL https://raw.githubusercontent.com/lrhodin/imessage/master/scripts/install-imessage.sh | sudo bash
```

The installer downloads the `imessage` script, drops it at `/usr/local/bin/imessage` (on `$PATH` by default on every standard Linux distro), verifies it's actually executable, and prints the next step. Safe to re-run any time to update to the latest version.

Verify:

```bash
imessage help
```

If you get `command not found`, open a new terminal — your current shell may have cached the old PATH. If it still doesn't work, the installer will have already printed the fix (typically adding `/usr/local/bin` to your shell rc on a stripped-down distro).

To uninstall: `sudo rm /usr/local/bin/imessage`.

> Prefer to inspect the installer before running it? Download and read first:
> ```bash
> curl -fsSL https://raw.githubusercontent.com/lrhodin/imessage/master/scripts/install-imessage.sh -o install-imessage.sh
> less install-imessage.sh
> sudo bash install-imessage.sh
> ```

### Step 2 — Drop in a compose file

Pick a directory you'll keep `docker-compose.yml` in — anywhere is fine (`~/docker/imessage/`, `/srv/imessage/`, your home dir, etc.). From inside that directory:

```bash
curl -L https://raw.githubusercontent.com/lrhodin/imessage/master/docker-compose.example.yml \
    -o docker-compose.yml
```

### Step 3 — Edit two things in `docker-compose.yml`

Open it in your editor:

1. **`BEEPER` env var** — set to `"true"` for a Beeper deploy or `"false"` for a self-hosted homeserver.
2. **The bind mounts** under `volumes:`. There are two by default:

   ```yaml
   volumes:
     - type: bind
       source: ${HOME}/.local/share/mautrix-imessage
       target: /data
     - type: bind
       source: ${HOME}/.config/bbctl
       target: /home/bridge/.config/bbctl
   ```

   - `/data` holds bridge state (`config.yaml`, `mautrix-imessage.db`, `session.json`, `trustedpeers.plist`, …). Path matches bare-Linux's `~/.local/share/mautrix-imessage/`, so migrating in either direction is a no-copy operation.
   - `/home/bridge/.config/bbctl` holds `bbctl`'s Beeper auth — same path bare-Linux puts it at (`~/.config/bbctl/`). Kept separate from bridge state on purpose: a bare-Linux user migrating their existing `~/.config/bbctl/` keeps their Beeper login, and `imessage bbctl logout` doesn't touch bridge state.

   For non-default platforms, change the left side of each line:

   | Platform | Bridge state path | bbctl path |
   |---|---|---|
   | Standard Linux | `~/.local/share/mautrix-imessage` | `~/.config/bbctl` |
   | UNRAID | `/mnt/user/appdata/Rustpush-Matrix/data` | `/mnt/user/appdata/Rustpush-Matrix/bbctl` |
   | Synology | `/volume1/docker/Rustpush-Matrix/data` | `/volume1/docker/Rustpush-Matrix/bbctl` |
   | TrueNAS / ZFS | dataset of your choice | dataset of your choice |

If your host bind-mount sources are owned by a UID/GID other than `1000:1000` — or if you want the bridge to *write* as a different UID — edit the `PUID:` and `PGID:` env vars in compose. See [Finding your UID and GID](#finding-your-uid-and-gid) for how to look them up. The container's root prelude chowns the bind mounts to whatever you set on first start (and only then), so you don't have to chown anything by hand.

### Step 4 — Start the container

**`imessage start` must be run from the directory that contains your `docker-compose.yml`.** Same rule as raw `docker compose up` — compose-driven subcommands (`start`, `stop`, `restart`, `pull`, `update`) need to find the file, and they look in the current directory by default.

```bash
imessage start
```

`imessage start` is just `docker compose up -d`. On first start (or any start where state needs adjusting), the container's root prelude:

1. Chowns each bind-mount target to `PUID:PGID` — but only if `find` spots a file that doesn't already match. Steady state is a single `find -quit` and a no-op.
2. Reads `/proc/self/mountinfo` to discover the host path of the `/data` bind mount and creates a symlink at that path inside the container pointing to `/data`. Lets absolute paths baked into `config.yaml` by the bare-Linux installer (e.g. `uri: file:/root/.local/share/mautrix-imessage/mautrix-imessage.db`) resolve unchanged. Skipped if the symlink already points where it should.
3. `setpriv`s to `PUID:PGID` and re-execs the entrypoint to run the bridge.

The container pulls the image (first time only, ~250 MB), comes up, and sits idle waiting for setup. You can confirm with:

```bash
imessage status
imessage logs       # Ctrl-C to detach
```

If you'd rather run lifecycle commands from anywhere, set `IMESSAGE_COMPOSE_FILE` to the absolute path of your compose file — see [Day-to-day operations](#day-to-day-operations) below.

The logs should show `no /data/config.yaml yet — run 'imessage setup' from the host to configure the bridge.` every 30 seconds — that's the entrypoint waiting on step 5.

### Step 5 — Run the setup wizard

```bash
imessage setup
```

This invokes the same install script the bare-Linux path uses, inside the container. For Beeper that means: bbctl login → bbctl config → iMessage login (paste the hardware key, Apple ID, password, 2FA). For self-hosted that means: homeserver URL / domain / Matrix ID / DB choice → iMessage login.

Then you'll be asked about CardDAV (if applicable), preferred handle, and the optional toggles for FaceTime, StatusKit, HEIC conversion, video transcoding.

When the wizard finishes, the container detects the new `config.yaml` and starts the bridge automatically. `imessage logs` should now show the bridge running. Go send yourself an iMessage to confirm the round trip.

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
| Run `bbctl` (Beeper bridge-manager) | `imessage bbctl <args>` |

### What each command does under the hood

The `imessage` CLI is a thin wrapper — every subcommand maps to a small number of raw `docker` / `docker compose` invocations. Useful when debugging, when teaching someone else, or when you want to do the same thing manually:

| `imessage …` | Equivalent raw command |
|---|---|
| `setup` | `docker exec -it Rustpush-Matrix as-bridge imessage-setup` |
| `login` | `docker exec -it Rustpush-Matrix as-bridge /entrypoint.sh login` |
| `logs` | `tail -F <host bind-mount>/logs/bridge.log` (read from host directly; works even when the container is stopped or restart-looping) |
| `status` | `docker ps --filter name=^Rustpush-Matrix$` |
| `shell` | `docker exec -it Rustpush-Matrix as-bridge bash` |
| `bbctl <args>` | `docker exec -it Rustpush-Matrix as-bridge bbctl <args>` |
| `start` | `docker compose up -d`. Bind-mount perms + host-path symlinks are handled by the container's root prelude, conditionally on detection. |
| `stop` | `docker compose stop Rustpush-Matrix` |
| `restart` | `docker compose restart Rustpush-Matrix` |
| `pull` | `docker compose pull` |
| `update` | `docker compose pull && docker compose up -d` |
| `help` | Prints the subcommand list. |

`as-bridge` is a tiny in-container wrapper that re-applies the entrypoint's setpriv drop to `PUID:PGID`. `docker exec` inherits the container's USER (root, so the entrypoint can chown bind mounts at PID 1), so without `as-bridge` every host-side `imessage bbctl …` would run as root inside the container.

When `docker compose` is invoked above, the CLI inserts `-f "$IMESSAGE_COMPOSE_FILE"` if that env var is set, otherwise compose uses its default search behavior (current working directory). Container name comes from `${IMESSAGE_CONTAINER:-Rustpush-Matrix}`.

### Where you need to run these

| Subcommand group | Runs from anywhere? | Why |
|---|---|---|
| `setup`, `login`, `logs`, `status`, `shell`, `bbctl` | ✓ yes | Found by container name (`Rustpush-Matrix`, unique per host) |
| `start`, `stop`, `restart`, `pull`, `update` | ✗ only from the `docker-compose.yml` dir (or set `IMESSAGE_COMPOSE_FILE`) | Lifecycle commands invoke `docker compose`, which needs to find the file |

If you'd rather run lifecycle commands from anywhere, point at the compose file once in your shell rc:

```bash
echo 'export IMESSAGE_COMPOSE_FILE=~/docker/imessage/docker-compose.yml' >> ~/.bashrc
# or ~/.zshrc — open a new terminal to pick it up
imessage update                                # now works from anywhere
```

---

## How the privilege model works

The image's `USER` is unset, so PID 1 enters the entrypoint as root. The entrypoint runs a small prelude that:

1. **Chowns the bind-mount targets to `PUID:PGID`** — but only if `find` spots a file that doesn't already match. The check is a single `find -quit`, so on subsequent starts where everything's already right, it's a no-op.
2. **Creates a host-path symlink** at the bind mount's host source path inside the container, pointing to `/data`. Lets absolute paths from a bare-Linux install (e.g. `file:/root/.local/share/mautrix-imessage/mautrix-imessage.db` baked into `config.yaml`) resolve when the bridge runs in Docker. Skipped if the symlink already points where it should. Skipped (with a warning) if a non-symlink already exists at that path.
3. **`setpriv`s to `PUID:PGID`** and re-execs itself. From that point on the bridge runs as the configured non-root user.

To pick a different UID/GID, edit `PUID:` and `PGID:` in `docker-compose.yml`:

```yaml
environment:
  PUID: "99"      # UNRAID nobody
  PGID: "100"     # UNRAID users
```

`PUID: "568"`/`PGID: "568"` for TrueNAS Scale. `PUID: "0"`/`PGID: "0"` for root, discouraged unless required. Numeric only — names like `nobody:users` don't translate.

Changing `PUID`/`PGID` and running `imessage start` is enough — the entrypoint re-chowns the bind mounts on next boot.

Host-side `docker exec` calls (`imessage setup`, `imessage bbctl …`, etc.) go through `/usr/local/bin/as-bridge` inside the container, which applies the same `setpriv` drop so commands run as `PUID:PGID` rather than as root.

---

## Finding your UID and GID

Run on the **host** (not inside the container). Three approaches, pick whichever is easiest:

**Your own UID/GID** — the user you're currently logged in as:

```bash
id           # prints: uid=1000(yourname) gid=1000(yourname) groups=...
id -u        # just the UID, e.g. 1000
id -g        # just the GID, e.g. 1000
```

**The UID/GID that owns an existing directory** — most useful before starting the container, to make sure your bind-mount source paths line up with the container's user:

```bash
stat -c '%u:%g' ~/.local/share/mautrix-imessage           # numeric:  1000:1000
stat -c '%U:%G' ~/.local/share/mautrix-imessage           # by name:  david:david
ls -ldn         ~/.local/share/mautrix-imessage           # numeric, with perms
```

If those don't match what `PUID:PGID` is set to in compose, you have two options:

- **Easiest:** just run `imessage start`. The container's root prelude will chown the bind mounts to `PUID:PGID` automatically on first boot. No host-side sudo needed.
- **Or change `PUID`/`PGID`** in compose to match the existing ownership of your bind-mount sources, then `imessage restart`.

**For another user** (e.g. you'll run Docker as a separate service account):

```bash
id beepuser
```

**Platform quick reference** (a starting point; `id` / `stat` are still authoritative):

| Platform | Typical UID:GID |
|---|---|
| Standard Linux (first user) | `1000:1000` |
| UNRAID (`nobody:users`) | `99:100` |
| Synology DSM (`admin`) | `1024:100` (varies; verify with `id`) |
| TrueNAS Scale (`apps`) | `568:568` |

### Running Docker as root

If you're logged into the host as `root` (`id -u` returns `0`), you have two clean options. Pick one:

**Option A — leave the bridge at its default 1000:1000 (recommended)**

`PUID: "1000"` / `PGID: "1000"` is the default. Just run `imessage start`. The container's root prelude chowns your bind-mount sources to `1000:1000` on first boot. Your root login on the host still has full read access (root bypasses owner checks).

**Option B — run the bridge as root via `PUID: "0"`**

```yaml
environment:
  PUID: "0"
  PGID: "0"
```

The bridge then runs everything as root. Functionally fine, but you lose the non-root containment the image gives you by default. Use Option A unless you specifically can't.

Either way, `imessage` host commands work — they call `docker` directly without caring what UID the bridge ultimately runs as.

You don't need to `mkdir` the bind-mount source paths manually. If the host paths don't exist, Docker creates them on first `imessage start`, and the container's root prelude chowns them on the first start that needs it.

---

## Apple Silicon NAC relay

If your hardware key was extracted from an Apple Silicon Mac, the bridge fetches NAC validation data from a relay running on that Mac. The relay URL, bearer token, and TLS fingerprint are all embedded in the base64 key blob — there's nothing to configure in compose.

The Mac running the relay is a separate machine from the Docker host. The key needs to have been extracted with a hostname/IP that's routable from your Docker host (LAN, VPN, or port-forwarded WAN). Intel hardware keys don't need a relay at all — the x86_64 unicorn emulator runs entirely in-process inside the container.

---

## Troubleshooting

**`imessage start` says "Cannot connect to the Docker daemon"** — make sure Docker is running and your user is in the `docker` group. Log out and back in after adding yourself.

**`imessage logs` keeps showing `no /data/config.yaml yet`** — you never finished `imessage setup`, or the wizard exited mid-flow. Run it again; it's safe to re-run.

**Container restarts in a loop** — `imessage logs` shows the actual error. The container's root prelude chowns bind mounts to `PUID:PGID` and creates the host-path symlink on every start where they're not already right, so persistent permission errors usually mean either (a) the bind mount itself isn't writable by root (read-only filesystem, immutable bits, SELinux/AppArmor denials) or (b) the bridge is failing for unrelated reasons. Read the actual error from `imessage logs`.

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
