# Self-hosting

Boxes runs as a single VM: one `boxes-server` control plane, one `boxes-agent`
that manages the local Docker and Sysbox runtime, and Traefik as the public
edge. The installer provisions all of it on Ubuntu or Debian.

Production installs on a fresh VPS and Sysbox operation on real VPS hardware
are not yet verified. The local `runc` end-to-end path is covered by
`scripts/e2e-local.sh`.

## Requirements

- Ubuntu or Debian (the installer exits on other distributions), `amd64` or `arm64`.
- Root access (`sudo`).
- Ports **80** and **443** open to the internet. Traefik binds them on the host
  network. Port 80 serves the Let's Encrypt HTTP-01 challenge.
- A DNS name that resolves to the VM for `--domain`. You can use your own domain
  or a sslip.io name such as `boxes.203.0.113.10.sslip.io`.
- Enough RAM for the boxes you plan to run. Each box size caps at 1, 2, 4, 8, or 16
  CPUs and 2, 4, 8, 16, or 32 GiB of RAM. The server admits a new box only when
  the host has room after a buffer equal to the larger of 15% of RAM or 1 GiB,
  and admission assumes at least 300 MiB per box. The installer has no hard
  minimum.
- Swap: if the host has none, the installer creates a swap file at
  `/var/lib/boxes/swapfile`, sized to RAM and capped at 8 GiB, and sets
  `vm.swappiness=10`.

The installer installs Docker, Sysbox (`sysbox-ce` 0.7.1 by default), restic,
and Traefik, pulls the box and tools images, and writes systemd units.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/tony1908/boxes-oss/main/install/install.sh \
  | sudo bash -s -- --domain boxes.example.com --email ops@example.com
```

Flags:

| Flag | Meaning |
| --- | --- |
| `--domain DOMAIN` | Public hostname. Required. Traefik requests its certificate for this name. |
| `--email EMAIL` | Contact address for the Let's Encrypt account. Required. |
| `--public-url URL` | Must be `https://` plus the `--domain` host, so it is effectively optional. |
| `--host-base HOST_BASE` | Hostname suffix for box URLs. Defaults to `<public IPv4>.sslip.io`, detected from the VM. |
| `--image IMAGE` | Box image. Defaults to `ghcr.io/tony1908/boxes-box:latest`. |
| `--memory-overcommit N` | Positive number, default `1.0`. Memory reservation headroom. |
| `--s3-endpoint URL --s3-bucket NAME` | Use S3-compatible snapshot storage (see below). |
| `--s3-prefix P`, `--s3-region R` | Optional S3 prefix (default `boxes`) and region. |

The tools image is set with the `BOXES_TOOLS_IMAGE` environment variable (default
`ghcr.io/tony1908/boxes-tools:latest`; use `none` to disable it).

Re-running the full install command rewrites `/etc/boxes/server.env` and
`/etc/boxes/agent.env`. On an installed host, edit those files and restart the
services, or use `--upgrade` (below) to refresh binaries.

## First login

When installation finishes, the installer prints:

```
Boxes dashboard: https://boxes.example.com
Owner setup link (one time): https://boxes.example.com/signup/?setup=<token>
To read the setup token again: sudo cat /var/lib/boxes/admin.token
```

1. Open the owner setup link. Its token is the value in
   `/var/lib/boxes/admin.token`.
2. Create the owner account with an email address and a password of at least
   12 characters. While no users exist, signup requires the setup token; without
   it the server rejects the request with `setup_token_required`. Later accounts
   are created by an owner.
3. Sign in at `https://<domain>/login/` with the same email and password.

CLI login uses the same account:

```sh
boxes login --server https://boxes.example.com --email you@example.com
```

API keys for scripts are created in the web UI or with `POST /api/v1/api-keys`.
Store one with `boxes login --server URL --key-stdin`.

## Services and configuration

The installer writes:

- `/etc/boxes/server.env`: server settings (public URL, box and tools images,
  snapshot interval, memory overcommit, storage). Mode `0600`.
- `/etc/boxes/agent.env`: agent settings (server connection, runtime,
  tools image, restic repository). Mode `0600`.
- `/etc/systemd/system/boxes-server.service` and `boxes-agent.service`.
- `/etc/boxes/traefik/`: Traefik static and dynamic configuration.
- `/var/lib/boxes/`: database, `admin.token`, snapshot repository, CLI binaries.

Restart services after editing environment files:

```sh
sudo systemctl restart boxes-server boxes-agent
sudo journalctl -u boxes-server -f
```

The server environment variables are:

| Variable | Purpose |
| --- | --- |
| `BOXES_PUBLIC_URL` | Public origin, for example `https://boxes.example.com`. |
| `BOXES_IMAGE` | Default box image. |
| `BOXES_TOOLS_IMAGE` | Tools image mounted read-only into boxes. |
| `BOXES_SNAPSHOT_INTERVAL` | Periodic snapshot interval (for example `60s`). |
| `BOXES_SNAPSHOT_BUSY_INTERVAL` | Minimum interval after repeated changed snapshots (default `5m`). |
| `BOXES_DESKTOP_SESSION_TTL` | Desktop session lifetime (default `8h`). See [desktop.md](desktop.md). |
| `BOXES_MEMORY_OVERCOMMIT` | Reservation headroom factor (installer default `1.0`). |
| `BOXES_STORAGE_*` | Snapshot repository, written by the installer. |
| `BOXES_WEBHOOK_ALLOW_PRIVATE` | Allow webhooks to reach private addresses (see below). |
| `BOXES_SERVER_ADMIN_URL` | Admin endpoint used by `boxes-server storage set` (default `http://127.0.0.1:8080/api/v1/admin/storage`). |

The installer's systemd unit starts `boxes-server` with `--memory-overcommit`
only. To change the other admission flags listed below, add them to
`ExecStart` in `/etc/systemd/system/boxes-server.service`, then run
`sudo systemctl daemon-reload && sudo systemctl restart boxes-server`. An
upgrade rewrites the unit file, so re-apply the change afterwards.

## Host settings

On hosts that expose `/proc/sys/kernel/apparmor_restrict_unprivileged_userns`,
the installer sets `kernel.apparmor_restrict_unprivileged_userns = 0` in
`/etc/sysctl.d/99-boxes.conf` and applies it immediately. Sysbox uses user
namespaces, and the Ubuntu restriction can stop `bwrap` inside a box from
creating its uid map and can prevent desktop sessions from starting.

## Webhook delivery

Webhook destinations that resolve to loopback, private, link-local, carrier-grade
NAT, multicast, or unspecified addresses are blocked. To let webhooks reach a
private service, add `BOXES_WEBHOOK_ALLOW_PRIVATE=true` to
`/etc/boxes/server.env` and restart `boxes-server`. This also allows private IP
literals in webhook configuration.

## Traefik and desktop authentication

Traefik runs as the `boxes-traefik` Docker container (`traefik:v3.3`) with host
networking, binding ports 80 and 443. It obtains certificates from Let's Encrypt
with the HTTP-01 challenge. The dynamic configuration routes the `--domain` host
to `boxes-server` on `127.0.0.1:8080`.

Desktop and private host routes use forward-auth against the local agent at
`http://127.0.0.1:7001/auth/forward`. The agent checks the request's token
against the stored hash (SHA-256) and sets the authorization cookie. Defaults:

- `BOXES_FORWARD_AUTH_ADDR=127.0.0.1:7001`: local forward-auth listener.
- `BOXES_FORWARD_AUTH_URL=http://127.0.0.1:7001/auth/forward`: URL written into Traefik's dynamic configuration.

Override these only when Traefik and the agent need a different reachable
address. The URL must be reachable from the host-networked Traefik container.

## Capacity and admission

`boxes-server` uses usage-based admission by default. It reads memory and
memory-pressure (PSI) health from the agent and admits a box only when:

- The health data is fresh (not older than 45 seconds).
- The host has room after a buffer equal to the larger of `--admission-buffer-percent`
  (default 15%) of RAM or `--admission-buffer-min` (default 1 GiB).
- Memory PSI `some avg10` is below `--admission-psi-max` (default 10).
- The sum of non-stopped box reservations stays within 95% of RAM.

Swap use up to `--admission-swap-allowance` (default 1 GiB) is ignored; each byte
above that reduces the memory treated as available. `--admission-expected-box`
(default 300 MiB) is the minimum memory assumed per box.

Usage-based admission packs boxes according to observed free memory, so it can
admit more idle boxes than reservation accounting. Boxes that suddenly use
memory are the risk. Sustained swap slows boxes and does not replace RAM, and
high PSI blocks new placements even when `MemAvailable` looks sufficient.

For predictable capacity, start the server with `--admission reservation`. That
mode accounts for reservations against the `--memory-overcommit` budget. It is
more conservative and can leave usable memory idle.

## Snapshot storage

Snapshots use a local restic repository at `/var/lib/boxes/restic` by default.
To use S3-compatible storage at install time, pass `--s3-endpoint` and
`--s3-bucket` (and optionally `--s3-prefix` and `--s3-region`). Credentials are
read from `BOXES_S3_ACCESS_KEY` and `BOXES_S3_SECRET_KEY` in the installer
environment, not from command-line flags:

```sh
export BOXES_S3_ACCESS_KEY='access-key'
export BOXES_S3_SECRET_KEY='secret-key'
curl -fsSL https://raw.githubusercontent.com/tony1908/boxes-oss/main/install/install.sh \
  | sudo --preserve-env=BOXES_S3_ACCESS_KEY,BOXES_S3_SECRET_KEY bash -s -- \
    --domain boxes.example.com --email ops@example.com \
    --s3-endpoint https://s3.us-east-1.amazonaws.com --s3-bucket boxes-prod \
    --s3-prefix snapshots --s3-region us-east-1
```

From a clone, run `sudo --preserve-env=BOXES_S3_ACCESS_KEY,BOXES_S3_SECRET_KEY bash install/install.sh ...` with the same flags.

Common endpoint formats:

| Provider | Endpoint |
| --- | --- |
| Cloudflare R2 | `https://<account-id>.r2.cloudflarestorage.com` |
| Backblaze B2 | `https://s3.<region>.backblazeb2.com` |
| AWS S3 | `https://s3.<region>.amazonaws.com` |
| MinIO | the reachable MinIO URL, for example `https://minio.example.com` |

To switch an existing installation, run this on the server. The access key and
secret are accepted only through environment variables:

```sh
BOXES_S3_ACCESS_KEY='access-key' BOXES_S3_SECRET_KEY='secret-key' \
  boxes-server storage set --s3-endpoint https://<account-id>.r2.cloudflarestorage.com \
  --s3-bucket boxes-prod --s3-prefix snapshots
```

The command updates the server's encrypted storage settings and schedules a
fresh capture for every running box. Existing snapshots keep the repository and
credentials they were created with. The web Settings page shows the storage
type and non-secret location to owners.

## Upgrading

On an existing server, run the installer with `--upgrade` (from a clone, or
through the same `curl` pipe with `-s -- --upgrade`):

```sh
sudo bash install/install.sh --upgrade
```

The upgrade reads `/etc/boxes/server.env`, installs the latest server and agent
binaries from the GitHub release, installs the CLI binaries for every supported
platform into `/var/lib/boxes/cli/`, refreshes the systemd units, and restarts
`boxes-server` and `boxes-agent`. Use `--upgrade` by itself; it does not accept
the install flags.

To install binaries you have already built, set `BOXES_BINARIES_DIR` to a
directory containing `boxes-server`, `boxes-agent`, `boxes-darwin-arm64`,
`boxes-darwin-amd64`, `boxes-linux-amd64`, `boxes-linux-arm64`,
`boxes-windows-amd64.exe`, and `boxes-windows-arm64.exe`:

```sh
sudo BOXES_BINARIES_DIR=/path/to/binaries bash install/install.sh --upgrade
```

Set `BOXES_GITHUB_REPO` to download releases from a fork.

Upgrades are safe to rerun. Existing values in `/etc/boxes/server.env` and
`/etc/boxes/agent.env` are kept; only missing settings get defaults. Storage
settings and encryption secrets are preserved, and the Restic repository is not
replaced with a local path. The `boxes-net` Docker network and host sysctl
settings are created only when absent. Running boxes are not stopped when the
installed Sysbox version already matches.

### Sysbox upgrades

The installer compares the installed Sysbox release with `SYSBOX_VERSION`
(default `0.7.1`). When they match, `--upgrade` leaves Sysbox, containerd, and
Docker alone. If they differ, the upgrade refuses to run unless
`--allow-runtime-restart` is given. That flag restarts Docker, so run it in a
maintenance window:

```sh
export SYSBOX_VERSION=0.7.2 # replace with the desired Sysbox release
sudo --preserve-env=SYSBOX_VERSION bash install/install.sh --upgrade --allow-runtime-restart
```

With that flag the installer uses the local server API to snapshot and stop
every running box, installs Sysbox, restarts Docker, and resumes the boxes from
those snapshots. The upgrade stops before changing runtime packages if the server
cannot stop every box. If Docker or a resume fails after the package change,
the box IDs are saved under `/run/boxes-runtime-upgrade.*` so you can repair the
runtime and resume them manually.

### Tools image

Installed agents use `BOXES_TOOLS_IMAGE=ghcr.io/tony1908/boxes-tools:latest` by
default. The agent copies each locally available tools-image digest once into a
shared, digest-named Docker volume (`boxes-tools-<digest>`). It mounts that volume
read-only at `/opt/boxes-tools`. Boxes created later use the new digest, while
running boxes keep their original mount. If the image is not present locally, the
agent logs once and creates boxes without the mount. The volume is not part of
snapshots.
