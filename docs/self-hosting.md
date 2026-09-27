# Self-hosting

The install scripts provision Docker, Sysbox, the Boxes services, and Traefik on Ubuntu/Debian nodes. Production installation on a fresh VPS and Sysbox operation on real VPS hardware are not yet verified; see the [project status](../README.md#status).

## Host settings

On hosts that expose `/proc/sys/kernel/apparmor_restrict_unprivileged_userns`, the install scripts set `kernel.apparmor_restrict_unprivileged_userns = 0` in `/etc/sysctl.d/99-boxes.conf` and apply it immediately. Boxes is the only workload on these hosts, and Sysbox itself uses user namespaces; leaving the Ubuntu restriction enabled can prevent `bwrap` inside a box from creating its uid map and can stop desktop sessions from starting.

## Traefik and desktop authentication

Traefik runs as a Docker container with **host networking**, binding the node's public ports 80 and 443. Desktop routers use forward-auth against the local agent at `http://127.0.0.1:7001/auth/forward`; the agent validates the desktop token and sets the authorization cookie.

The agent defaults to:

- `BOXES_FORWARD_AUTH_ADDR=127.0.0.1:7001` — local forward-auth listener.
- `BOXES_FORWARD_AUTH_URL=http://127.0.0.1:7001/auth/forward` — URL written into Traefik's dynamic configuration.

Override these when Traefik and the agent need a different reachable address. In particular, the URL must be reachable from the host-networked Traefik container.

## Capacity & admission

By default, `boxes-server` uses usage-based admission. Every 15 seconds it reads each connected agent’s `/proc/meminfo` and memory PSI. A box is admitted only when fresh health data is available, the node has room after accounting for recently started boxes, excess swap use, and a host buffer (the larger of 15% of RAM or 1 GiB), PSI `some avg10` is below 10, and non-stopped box reservation floors remain within 95% of RAM. Swap use up to 1 GiB is ignored; each byte above that allowance reduces effective available memory. Set `--admission-swap-allowance` to tune the allowance (bytes). Expected usage is at least 300 MiB per box; per-size floors are xsmall 128 MiB, small 256 MiB, default 512 MiB, large 1 GiB, and xlarge 2 GiB. These floors also set Docker `--memory-reservation`; hard `--memory` caps are unchanged.

Usage-based admission packs boxes according to observed free memory, which can admit more idle boxes than reservation-only scheduling. It carries more risk when boxes suddenly use memory. Swap can absorb brief spikes, but sustained swap use slows boxes and does not replace RAM. PSI reports time stalled on memory pressure; high pressure blocks new placements even when `MemAvailable` looks sufficient. Tune with `--admission-buffer-percent`, `--admission-buffer-min`, `--admission-expected-box`, and `--admission-psi-max`.

For predictable reservation-only capacity, start the server with `--admission reservation`. This uses the size floors multiplied by four and the existing RAM reservation budget (`--memory-overcommit`); it is more conservative and may leave usable memory idle.

## Upgrading

On an existing server, run the installer with `--upgrade`:

```sh
sudo bash install/install.sh --upgrade
```

The upgrade detects the installation from `/etc/boxes/server.env`, installs the current server and agent binaries and the supported CLI binaries, refreshes the systemd units, then restarts the services. It uses the latest GitHub release by default. To install binaries you have already built, set `BOXES_BINARIES_DIR` to a directory containing `boxes-server`, `boxes-agent`, `boxes-darwin-arm64`, `boxes-darwin-amd64`, `boxes-linux-amd64`, `boxes-linux-arm64`, `boxes-windows-amd64.exe`, and `boxes-windows-arm64.exe`:

```sh
sudo BOXES_BINARIES_DIR=/path/to/binaries bash install/install.sh --upgrade
```

Upgrade is safe to rerun. Existing values in `/etc/boxes/server.env` and `/etc/boxes/agent.env` are retained; only missing settings receive defaults. Existing join and Restic passwords, database-backed admin/master-key data, registry credentials, and storage settings are preserved. The server database and `/etc/boxes/storage.json` remain authoritative for S3 storage, so upgrading does not replace the Restic repository with a local path. The registry, Boxes Docker network, host sysctl settings, and swap are created only when absent. The installer refreshes the systemd units and restarts `boxes-server` and `boxes-agent`; running boxes are not stopped or deleted when the installed Sysbox version already matches the requested version.

The installer compares the installed Sysbox release (ignoring the package's `.linux` suffix) with `SYSBOX_VERSION` (default `0.7.1`). When they match, `--upgrade` does not reinstall Sysbox or restart Sysbox, containerd, or Docker. If the requested Sysbox version differs, the installer refuses to upgrade unless `--allow-runtime-restart` is provided. A Sysbox upgrade restarts Docker, so run it during a maintenance window:

```sh
export SYSBOX_VERSION=0.7.2 # replace with the desired Sysbox release
sudo --preserve-env=SYSBOX_VERSION bash install/install.sh --upgrade --allow-runtime-restart
```

With that flag, the installer uses the local server API to snapshot and stop every running box before changing Sysbox, restarts Docker, then resumes the boxes from those snapshots. The upgrade is aborted without changing runtime packages if the server cannot stop all boxes; if Docker cannot restart or boxes cannot resume after the package change, their IDs remain in `/run/boxes-runtime-upgrade.*` so the runtime can be repaired and the boxes resumed manually.

Installed agents use `BOXES_TOOLS_IMAGE=boxes/tools:latest` by default. The agent copies each locally available tools-image digest once into a shared, digest-named Docker volume and mounts that volume read-only at `/opt/boxes-tools`; boxes created later use new image digests while running boxes retain their original mount. Set `BOXES_TOOLS_IMAGE=none` to disable it. If the configured image is not present locally, the agent logs once and creates boxes without the mount. The volume is runtime-mounted rather than declared by the box image and is not included in snapshots.

## Snapshot storage

Snapshots use a local Restic repository at `/var/lib/boxes/restic` by default. To use S3-compatible storage during installation, provide an endpoint and bucket; the optional prefix defaults to `boxes`. Credentials are read from `BOXES_S3_ACCESS_KEY` and `BOXES_S3_SECRET_KEY` in the installer environment and are written into protected service configuration. Do not put credentials in command-line flags.

```sh
export BOXES_S3_ACCESS_KEY='access-key'
export BOXES_S3_SECRET_KEY='secret-key'
sudo --preserve-env=BOXES_S3_ACCESS_KEY,BOXES_S3_SECRET_KEY \
  ./install/install.sh --domain boxes.example.com --email ops@example.com \
  --s3-endpoint https://nyc3.digitaloceanspaces.com --s3-bucket boxes-prod \
  --s3-prefix snapshots --s3-region nyc3
```

The same `--s3-endpoint`, `--s3-bucket`, optional `--s3-prefix`, and optional `--s3-region` flags are available on `install/node-install.sh`. All nodes joined to an installation receive its shared repository and Restic password.

Common endpoint examples:

| Provider | Endpoint example |
| --- | --- |
| DigitalOcean Spaces | `https://<region>.digitaloceanspaces.com` (for example, `https://nyc3.digitaloceanspaces.com`) |
| Cloudflare R2 | `https://<account-id>.r2.cloudflarestorage.com` |
| Backblaze B2 | `https://s3.<region>.backblazeb2.com` |
| AWS S3 | `https://s3.<region>.amazonaws.com` |
| MinIO | `https://minio.example.com` (or the reachable MinIO URL) |

To switch an existing installation, run this on the server. The access key and secret are accepted only through environment variables:

```sh
BOXES_S3_ACCESS_KEY='access-key' BOXES_S3_SECRET_KEY='secret-key' \
  boxes-server storage set --s3-endpoint https://<account-id>.r2.cloudflarestorage.com \
  --s3-bucket boxes-prod --s3-prefix snapshots
```

The server updates its encrypted storage settings, initializes the repository on connected agents, and schedules a fresh capture for every running box. Existing snapshots keep the repository and encrypted credentials they were created with, so stopped boxes can still resume from their previous repository. The web Settings page shows the storage type and non-secret location details to owners.

## Autoscaling pool credentials

Provider credentials configured for an autoscaling pool are encrypted at rest with the server master key and are never returned by the API. Use narrowly scoped provider tokens where available. The cloud provider can read instance `user_data`; it contains a one-time node join token and the node's snapshot-storage and configured registry credentials. Treat access to the provider account and its instance metadata/user-data APIs as access to those secrets.
