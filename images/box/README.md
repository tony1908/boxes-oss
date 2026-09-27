# Boxes box image

`boxes/box` is a LinuxServer Webtop XFCE desktop image containing the `boxd`
agent, browser, coding harnesses, SSH access, and optional Docker-in-Docker for
Sysbox-backed boxes.

Build it from the repository root:

```sh
docker build -f images/box/Dockerfile -t boxes/box:dev .
```

Run it with a per-box `BOXD_TOKEN` and LinuxServer desktop password:

```sh
docker run -d --name my-box -e BOXD_TOKEN=replace-me -e PASSWORD=replace-me boxes/box:dev
```

`boxd serve` listens on port 7070 and requires `BOXD_TOKEN`. The image starts
`boxd setup` asynchronously at container initialization. SSH listens on port
22 and accepts public keys from `/config/.ssh/authorized_keys`; password
authentication is disabled. The Docker daemon is enabled only when the
container is running in a user namespace (such as Sysbox), and otherwise its
service remains idle.

Firefox is installed on amd64 and arm64. Google Chrome is additionally
installed on amd64. Selkies uses a fixed 1920×1080 desktop resolution.
