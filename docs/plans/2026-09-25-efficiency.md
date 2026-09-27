# Box efficiency plan

The full XFCE desktop stays. Baseline, measured on the droplet (Sysbox, amd64):
- Image: 7.94 GB. Webtop base about 3.4 GB; agent CLIs + gh + node + docker about 1.55 GB; Firefox + Chrome about 0.8 GB.
- Idle box: about 700 MB RSS. XFCE about 280 MB, dockerd + containerd 125 MB, Xvfb 95 MB, selkies 75 MB, boxd + pulse 30 MB.
- Admission: 4 GiB reserved per small box, so an 8 GB node fits only 3 running boxes.

## Track D: density (server, agent, installers)
- New size `xsmall`: 1 vCPU / 2 GiB hard cap / 12 GiB disk.
- Every size gets a soft memory reservation: xsmall 512 MiB, small 1 GiB, default 2 GiB, large 4 GiB, xlarge 8 GiB. `docker create` gets `--memory-reservation <res>`, with `--memory <hard cap>` unchanged. The scheduler admits by the sum of reservations of non-stopped boxes ≤ node memory × 0.9. `--memory-overcommit` becomes the reservation headroom factor (default 1.0 under this model). Document the trade-off.
- Installers create a swapfile when the host has none: size = min(RAM, 8 GiB), `vm.swappiness=10`. It is idempotent.
- Shared tools volume. The agent mounts a read-only named volume `boxes-tools-<shortdigest>` at `/opt/boxes-tools`, populated once per tools-image digest from `BOXES_TOOLS_IMAGE` (default `boxes/tools:latest`): `docker run --rm -v <vol>:/dst <image> cp -a /opt/boxes-tools/. /dst/`. New boxes use the newest digest, and running boxes keep theirs. Snapshots must not capture it; it isn't image-declared, so check.

## Track I: image
- `images/tools/Dockerfile` builds `/opt/boxes-tools`: node (binary), npm-installed codex, opencode, claude-code, and gh, with `bin/` on PATH.
- Box image: drop the agent CLIs, node/npm and gh (they come from the tools volume, and `/etc/profile.d` plus the boxd env put `/opt/boxes-tools/bin` on PATH). One browser: Chrome on amd64, Firefox on arm64. No second Docker install. Docker starts lazily: the s6 dockerd service is down by default, and a `/usr/local/bin/docker` wrapper starts it on first use (and `sudo dockerd` still works). Trim XFCE autostart: no xfdesktop icons daemon, no thunar daemon. Panel, menus, WM and apps stay.
- Candidate A: Webtop base (current pin) plus the changes above.
- Candidate B: `ghcr.io/linuxserver/baseimage-selkies:ubuntunoble` plus a minimal XFCE (`xfce4-session xfwm4 xfce4-panel xfce4-terminal thunar xfce4-settings` + fonts, `--no-install-recommends`) plus the changes above. It runs on Ubuntu 24.04, so the glycin/bwrap crash doesn't apply.
- For each candidate, measure on the droplet under Sysbox: image size, idle RSS after 2 minutes, boot-to-ready time. Functional checks: boxd healthz, desktop 200 via Selkies, xdotool screenshot, sudo, `docker run hello-world` (lazy start), browser launches, and an agent CLI runs from `/opt/boxes-tools`. Pick the smallest candidate that passes everything.
