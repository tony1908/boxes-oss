# Snapshots and resume

Boxes snapshots contain filesystem state only. They do not include memory, running processes, or `/var/lib/docker` inside the box.

## Capture

The agent captures the paths reported by `docker diff` (added and changed paths, plus deletion records) and the full contents of image-declared Docker volumes such as `/config`. It writes a tar that preserves ownership, xattrs, and ACLs, then streams the tar into `restic backup --stdin`. System excludes and `.boxignore` rules apply.

Snapshots are taken:

- **Periodically** for every box in the `ready` or `idle` state. The default interval is one minute (`paddock-server --snapshot-interval`, or `PADDOCK_SNAPSHOT_INTERVAL`; the installer sets `60s`). After repeated changed captures, the minimum interval is `--snapshot-busy-interval` (`PADDOCK_SNAPSHOT_BUSY_INTERVAL`, default `5m`).
- **On stop**, always, before the container is stopped. If the capture fails, the box stays running.
- **Manually**, with `paddock snapshot <id> <name>` or `POST /api/v1/named-snapshots`.

The server keeps the latest five automatic snapshots per box by default. Set `keepSnapshots` (1 to 50) on create or with `PATCH /api/v1/sandboxes/{id}` (`paddock new --keep-snapshots N`, `paddock update <id> --keep-snapshots N`) to change that; `0` restores the default. Named snapshots are created on demand and kept separately, and are never pruned.

## Restore

Resume creates a fresh container from the pinned image, streams `restic dump` output through `docker cp -a` into that container, and starts it. The image's `/custom-cont-init.d/00-paddock-restore` init script applies recorded deletions. Resume always creates a fresh container.

Fork creates another box from a fresh capture of the source. `paddock new --from NAME` restores a new box from a named snapshot.

## Rollback

`POST /api/v1/sandboxes/{id}/restore` with `{"snapshotId": "..."}` (or `paddock restore <id> <snapshotId>`) rolls a box back to one of its own snapshots. A running box is stopped first, which captures a final snapshot, so the rollback can itself be undone by restoring that newer snapshot. The box is then resumed from the chosen snapshot. A stopped box keeps the choice and its next resume uses it; the choice is cleared once a resume succeeds. The snapshot must belong to the box, otherwise the request returns `404 snapshot_not_found`.

SSH host keys are generated at first startup and are included in box filesystem snapshots, so they remain stable across stop and resume. A fork receives a restore marker and generates new host keys on first startup rather than inheriting the source box's SSH identity.

## Repository

Snapshots are stored in a restic repository. The default is a local repository at `/var/lib/paddock/restic`. To use S3-compatible storage, pass `--s3-endpoint`, `--s3-bucket`, and optionally `--s3-prefix` and `--s3-region` to the installer. Credentials come from `PADDOCK_S3_ACCESS_KEY` and `PADDOCK_S3_SECRET_KEY` in the installer environment, never from command-line flags. See [self-hosting.md](self-hosting.md#snapshot-storage).

Existing installations can switch repositories with `paddock-server storage set`. Snapshots created earlier keep the repository and credentials they were created with, so stopped boxes can still resume from their previous repository.

## Downloads and inspection

Snapshots can be listed per box (`GET /api/v1/sandboxes/{id}/snapshots`) or across boxes (`GET /api/v1/snapshots`). Each snapshot can be downloaded as a tar archive (`GET /api/v1/snapshots/{id}/download`), listed as a tree (`GET /api/v1/snapshots/{id}/tree`), or read one file at a time (`GET /api/v1/snapshots/{id}/file`). From the CLI, `paddock snapshot pull <snapshotId|boxId> [-o file.tar.gz]` downloads one, and `paddock snapshot rm <name|snapshotId>` deletes one.

## Verification

The local `runc` end-to-end script (`scripts/e2e-local.sh`) checks `/config` data and system-level package and file changes across stop and resume, and `/config` data across fork. Sysbox operation on real VPS hardware is not yet verified; see [self-hosting.md](self-hosting.md) for runtime details.
