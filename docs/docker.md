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
     - ~/.local/share/mautrix-imessage:/data
     - ~/.config/bridge-manager:/home/bridge/.config/bridge-manager
   ```

   - `/data` holds bridge state (`config.yaml`, `mautrix-imessage.db`, `session.json`, `trustedpeers.plist`, …). Path matches bare-Linux's `~/.local/share/mautrix-imessage/`, so migrating in either direction is a no-copy operation.
   - `/home/bridge/.config/bridge-manager` holds `bbctl`'s Beeper auth — same path bare-Linux puts it at (`~/.config/bridge-manager/`). Kept separate from bridge state on purpose: a bare-Linux user migrating their existing `~/.config/bridge-manager/` keeps their Beeper login, and `imessage bbctl logout` doesn't touch bridge state.

   For non-default platforms, change the left side of each line:

   | Platform | Bridge state path | bbctl path |
   |---|---|---|
   | Standard Linux | `~/.local/share/mautrix-imessage` | `~/.config/bridge-manager` |
   | UNRAID | `/mnt/user/appdata/Rustpush-Matrix/data` | `/mnt/user/appdata/Rustpush-Matrix/bbctl` |
   | Synology | `/volume1/docker/Rustpush-Matrix/data` | `/volume1/docker/Rustpush-Matrix/bbctl` |
   | TrueNAS / ZFS | dataset of your choice | dataset of your choice |

   Run `imessage fix-perms` after editing to chown both paths to the container's UID/GID in one step.

If your host appdata is owned by a UID other than `1000` (UNRAID is often `99:100`, for example), also uncomment and set the `user:` directive in compose to match — and run `imessage fix-perms` afterward to chown the bind-mount sources. See [Finding your UID and GID](#finding-your-uid-and-gid) below for how to look them up.

### Step 4 — Start the container

**`imessage start` must be run from the directory that contains your `docker-compose.yml`.** Same rule as raw `docker compose up` — compose-driven subcommands (`start`, `stop`, `restart`, `pull`, `update`) need to find the file, and they look in the current directory by default.

```bash
cd /path/to/the/dir/with/your/docker-compose.yml
imessage start
```

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
| `setup` | `docker exec -it Rustpush-Matrix imessage-setup` |
| `login` | `docker exec -it Rustpush-Matrix /entrypoint.sh login` |
| `logs` | `tail -F <host bind-mount>/logs/bridge.log` (read from host directly; works even when the container is stopped or restart-looping) |
| `status` | `docker ps --filter name=^Rustpush-Matrix$` |
| `shell` | `docker exec -it Rustpush-Matrix bash` |
| `bbctl <args>` | `docker exec -it Rustpush-Matrix bbctl <args>` |
| `start` | `docker compose up -d` |
| `stop` | `docker compose stop Rustpush-Matrix` |
| `restart` | `docker compose restart Rustpush-Matrix` |
| `pull` | `docker compose pull` |
| `update` | `docker compose pull && docker compose up -d` |
| `migrate` | Pure host-side script — strips the bare-Linux managed-alias block from `~/.bashrc` / `~/.zshrc`, stops + disables + removes the `mautrix-imessage` systemd unit, rewrites absolute DB paths in config.yaml to `/data`. No `docker` calls. |
| `fix-perms` | Reads bind-mount source + `user:` from your compose file (via `docker compose config`), then `chown -R <uid>:<gid> <bind source>`. Compose-driven, so it works before the container is ever created. Same CWD rule as `start`. |
| `help` | Prints the subcommand list. |

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

## Migrating from a bare-Linux install

If you already have a bare-Linux install (the systemd-unit + `~/.local/share/mautrix-imessage` setup), you can move to Docker without losing state, login, or the iCloud Keychain trust circle:

### Step 1 — Install the host CLI

Same as the fresh-install Step 1:

```bash
curl -fsSL https://raw.githubusercontent.com/lrhodin/imessage/master/scripts/install-imessage.sh | sudo bash
```

### Step 2 — Run the migration helper

```bash
imessage migrate
```

This:
1. Strips the bare-Linux systemd-based shell aliases from `~/.bashrc` / `~/.zshrc` (they shell out to `systemctl` and would no longer work).
2. Stops, disables, and removes the `mautrix-imessage` systemd unit (user and system scopes — uses `sudo` only for the system scope).
3. Confirms your state at `~/.local/share/mautrix-imessage` is intact.

Safe to re-run if something didn't complete the first time.

### Step 3 — Drop in the compose file

```bash
curl -L https://raw.githubusercontent.com/lrhodin/imessage/master/docker-compose.example.yml \
    -o docker-compose.yml
```

The default bind mounts already point at your existing state, so no edits to volumes are needed unless your host UID isn't 1000 (set `user:` in compose) or your bare-Linux install used non-default state paths.

### Step 4 — Start

```bash
imessage start
```

The bridge resumes against your existing config / session / trustedpeers / sqlite as-is. No re-login, no re-setup. `imessage logs` should show the bridge picking up where it left off within a few seconds.

If you want to flip toggles later: `imessage setup`. If you want to re-login: `imessage login`.

---

## How the privilege model works

The container runs as the `bridge` user (UID:GID `1000:1000`) from PID 1 — set via `USER bridge` in the Dockerfile. **No root process, ever.** No `gosu`, no privilege transitions, no setuid binaries in the image.

To run as a different UID/GID, override with Docker's `user:` directive in `docker-compose.yml`:

```yaml
user: "99:100"     # UNRAID
user: "568:568"    # TrueNAS Scale
user: "0:0"        # root (discouraged — loses the non-root containment)
```

Whichever UID:GID you pick, the host bind-mount source paths must be chowned to match. `imessage fix-perms` reads compose, finds every bind-mount source, and chowns each to the right UID:GID in one step.

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

If those don't match what the container runs as (default `1000:1000`, override via `user:` in compose), the container can't read/write — fix in one of two ways:

- Run `imessage fix-perms` (recommended — reads compose, chowns every bind-mount source to match the `user:` directive).
- Or manually: `sudo chown -R <uid>:<gid> <bind-mount-path>`.

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

**Option A — leave the container at its default 1000:1000 (recommended)**

The image runs as UID:GID `1000:1000` (the bundled non-root `bridge` user). Match by chowning your bind-mount sources to 1000:1000:

```bash
imessage fix-perms     # chowns every bind mount in compose to 1000:1000
```

This is the cleanest split: bridge runs non-root, your root login has full read access (because root bypasses owner checks), and `imessage` host commands keep working.

**Option B — run the container as root via `user: "0:0"`**

```yaml
user: "0:0"
```

The container then runs everything as root. Functionally fine (Apple's servers don't care), but you lose the non-root containment that the image gives you by default. Use Option A unless you specifically can't.

Either way, `imessage` host commands work — they call `docker` directly without caring what UID the container ultimately runs as.

You don't need to `mkdir` the bind-mount source paths manually. If the host paths don't exist, Docker creates them on first `imessage start`, and `imessage fix-perms` also creates them if it can't find them. Both default to root-owned, which is why `fix-perms` follows the mkdir with the chown.

---

## Apple Silicon NAC relay

If your hardware key was extracted from an Apple Silicon Mac, the bridge fetches NAC validation data from a relay running on that Mac. The relay URL, bearer token, and TLS fingerprint are all embedded in the base64 key blob — there's nothing to configure in compose.

The Mac running the relay is a separate machine from the Docker host. The key needs to have been extracted with a hostname/IP that's routable from your Docker host (LAN, VPN, or port-forwarded WAN). Intel hardware keys don't need a relay at all — the x86_64 unicorn emulator runs entirely in-process inside the container.

---

## Troubleshooting

**`imessage start` says "Cannot connect to the Docker daemon"** — make sure Docker is running and your user is in the `docker` group. Log out and back in after adding yourself.

**`imessage logs` keeps showing `no /data/config.yaml yet`** — you never finished `imessage setup`, or the wizard exited mid-flow. Run it again; it's safe to re-run.

**Container restarts in a loop** — `imessage logs` shows the actual error. Most common cause: UID mismatch between the host bind-mount sources and the container's user. Run `imessage fix-perms` to chown both bind mounts to the right UID:GID in one step.

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
