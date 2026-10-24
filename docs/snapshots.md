# Snapshots and resume

Boxes snapshots contain filesystem state only. They do not include memory, running processes, or `/var/lib/docker` inside the box.

## Capture

The agent captures the paths reported by `docker diff` (added and changed paths, plus deletion records) and the full contents of image-declared Docker volumes such as `/config`. It writes a tar that preserves ownership, xattrs, and ACLs, then streams the tar into `restic backup --stdin`. System excludes and `.boxignore` rules apply.

Snapshots are taken:

- **Periodically** for every box in the `ready` or `idle` state. The default interval is one minute (`boxes-server --snapshot-interval`, or `BOXES_SNAPSHOT_INTERVAL`; the installer sets `60s`). After repeated changed captures, the minimum interval is `--snapshot-busy-interval` (`BOXES_SNAPSHOT_BUSY_INTERVAL`, default `5m`).
- **On stop**, always, before the container is stopped. If the capture fails, the box stays running.
- **Manually**, with `boxes snapshot <id> <name>` or `POST /api/v1/named-snapshots`.

The server keeps the latest five automatic snapshots per box. Named snapshots are created on demand and kept separately.

## Restore

Resume creates a fresh container from the pinned image, streams `restic dump` output through `docker cp -a` into that container, and starts it. The image's `/custom-cont-init.d/00-boxes-restore` init script applies recorded deletions. Resume always creates a fresh container.

Fork creates another box from a fresh capture of the source. `boxes new --from NAME` restores a new box from a named snapshot.

SSH host keys are generated at first startup and are included in box filesystem snapshots, so they remain stable across stop and resume. A fork receives a restore marker and generates new host keys on first startup rather than inheriting the source box's SSH identity.

## Repository

Snapshots are stored in a restic repository. The default is a local repository at `/var/lib/boxes/restic`. To use S3-compatible storage, pass `--s3-endpoint`, `--s3-bucket`, and optionally `--s3-prefix` and `--s3-region` to the installer. Credentials come from `BOXES_S3_ACCESS_KEY` and `BOXES_S3_SECRET_KEY` in the installer environment, never from command-line flags. See [self-hosting.md](self-hosting.md#snapshot-storage).

Existing installations can switch repositories with `boxes-server storage set`. Snapshots created earlier keep the repository and credentials they were created with, so stopped boxes can still resume from their previous repository.

## Downloads and inspection

Snapshots can be listed per box (`GET /api/v1/sandboxes/{id}/snapshots`) or across boxes (`GET /api/v1/snapshots`). Each snapshot can be downloaded as a tar archive (`GET /api/v1/snapshots/{id}/download`), listed as a tree (`GET /api/v1/snapshots/{id}/tree`), or read one file at a time (`GET /api/v1/snapshots/{id}/file`). From the CLI, `boxes snapshot pull <snapshotId|boxId> [-o file.tar.gz]` downloads one, and `boxes snapshot rm <name|snapshotId>` deletes one.

## Verification

The local `runc` end-to-end script (`scripts/e2e-local.sh`) checks `/config` data and system-level package and file changes across stop and resume, and `/config` data across fork. Sysbox operation on real VPS hardware is not yet verified; see [self-hosting.md](self-hosting.md) for runtime details.
