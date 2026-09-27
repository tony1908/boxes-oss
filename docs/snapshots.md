# Snapshots and resume

Boxes snapshots contain filesystem state only. They do not include memory, running processes, or `/var/lib/docker` inside the box.

## Capture and restore

The agent captures paths selected by `docker diff` (added/changed paths and deletion records), plus the full contents of image-declared Docker volumes such as `/config`. It writes a tar preserving ownership, xattrs, and ACLs, then streams the tar to `restic backup --stdin`. System excludes and `.boxignore` rules apply.

Restore creates a fresh container from the pinned image, streams `restic dump` through `docker cp -a` into that container, and starts it. The image's `/custom-cont-init.d/00-boxes-restore` init script applies recorded deletions. Resume always creates a fresh container; same-node and cross-node resume therefore use the same restore path. Fork creates another box from a fresh source capture.

SSH host keys are generated at first startup and are included in box filesystem snapshots, so they remain stable across stop/resume. Forks receive a restore marker and generate new host keys on first startup rather than inheriting the source box's SSH identity.

The restic repository is configured with `RESTIC_REPOSITORY` and `RESTIC_PASSWORD`; it can be local or S3-compatible. The server captures ready and idle boxes periodically (default: every 60 seconds; configure with `boxes-server --snapshot-interval`). It records every successful capture and retains the latest five snapshots per box, in addition to any named snapshots. Stop always captures a final snapshot before stopping the running container; if capture fails, the box remains running.

## Current verification

The local `runc` E2E checks `/config` data and system-level package/file changes across stop/resume, and `/config` data across fork. Sysbox and cross-node resume on real servers are pending verification. See [self-hosting](self-hosting.md) for runtime and node details.
