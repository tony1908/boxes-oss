FROM --platform=$BUILDPLATFORM golang:1.26 AS boxd-build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/boxd ./cmd/boxd
COPY internal ./internal
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/boxd ./cmd/boxd

FROM lscr.io/linuxserver/webtop:ubuntu-xfce@sha256:1bd141d5d7aaf3e98e47b7d9665f50657d1628617b4ef47bc3bbd43d726fd77e

ARG TARGETARCH

RUN rm -f /etc/apt/sources.list.d/docker.list \
    && apt-get -o APT::Get::Always-Include-Phased-Updates=true update \
    && DEBIAN_FRONTEND=noninteractive apt-get remove -y \
        docker-ce \
        docker-ce-cli \
        containerd.io \
        docker-buildx-plugin \
        docker-compose-plugin \
    && DEBIAN_FRONTEND=noninteractive apt-get -o APT::Get::Always-Include-Phased-Updates=true install -y --no-install-recommends \
        ca-certificates \
        curl \
        docker.io \
        git \
        gnupg \
        jq \
        openssh-server \
        scrot \
        sudo \
        wmctrl \
        xdotool \
    && rm -f /etc/s6-overlay/s6-rc.d/user/contents.d/svc-docker \
    && rm -f /etc/xdg/autostart/xfdesktop.desktop /etc/xdg/autostart/thunar.desktop \
    && rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    if [ "${TARGETARCH}" = amd64 ]; then \
        DEBIAN_FRONTEND=noninteractive apt-get remove -y firefox; \
        curl -fsSL https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb -o /tmp/google-chrome.deb; \
        DEBIAN_FRONTEND=noninteractive apt-get update; \
        DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends /tmp/google-chrome.deb; \
        rm -f /tmp/google-chrome.deb; \
    elif [ "${TARGETARCH}" = arm64 ]; then \
        install -d -m 0755 /etc/apt/keyrings; \
        curl -fsSL https://packages.mozilla.org/apt/repo-signing-key.gpg -o /etc/apt/keyrings/packages.mozilla.org.asc; \
        printf 'deb [signed-by=/etc/apt/keyrings/packages.mozilla.org.asc] https://packages.mozilla.org/apt mozilla main\n' > /etc/apt/sources.list.d/mozilla.list; \
        printf 'Package: *\nPin: origin packages.mozilla.org\nPin-Priority: 1000\n' > /etc/apt/preferences.d/mozilla; \
        apt-get update; \
        DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends firefox; \
    else \
        echo "unsupported architecture: ${TARGETARCH}" >&2; exit 1; \
    fi; \
    ln -s "$(command -v google-chrome || command -v firefox)" /usr/local/bin/boxes-browser; \
    rm -rf /var/lib/apt/lists/*

ENV SELKIES_IS_MANUAL_RESOLUTION_MODE=true \
    SELKIES_MANUAL_WIDTH=1920 \
    SELKIES_MANUAL_HEIGHT=1080 \
    BROWSER=boxes-browser

COPY --from=boxd-build /out/boxd /usr/local/bin/boxd
COPY images/box/root/ /

RUN chmod 0755 \
        /custom-cont-init.d/00-boxes-restore \
        /custom-cont-init.d/01-boxes-ssh-hostkeys \
        /custom-cont-init.d/10-boxes-setup \
        /custom-services.d/boxd \
        /custom-services.d/sshd \
        /usr/local/bin/docker \
    && install -d -m 0755 /run/sshd /var/lib/docker \
    && printf '%s\n' \
        'Port 22' \
        'PubkeyAuthentication yes' \
        'PasswordAuthentication no' \
        'KbdInteractiveAuthentication no' \
        'PermitRootLogin no' \
        'AllowUsers abc' \
        'AuthorizedKeysFile /config/.ssh/authorized_keys' \
        'UsePAM no' \
        > /etc/ssh/sshd_config.d/boxes.conf \
    && printf 'abc ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/abc \
    && chmod 0440 /etc/sudoers.d/abc \
    && visudo -cf /etc/sudoers.d/abc \
    && ssh-keygen -A \
    && sshd -t \
    && rm -f /etc/ssh/ssh_host_* \
    && test -x /usr/local/bin/boxd
