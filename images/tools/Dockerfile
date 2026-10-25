ARG CODEX_VERSION=0.156.0
ARG OPENCODE_VERSION=1.18.25
ARG GH_VERSION=2.77.0
ARG NPM_VERSION=11.6.2
ARG GO_VERSION=1.26.8
ARG RUST_VERSION=1.98.1
ARG PI_VERSION=0.73.1
ARG PRIME_AGENT_VERSION=0.9.6
ARG KIMI_CODE_VERSION=2.1.1
ARG PI_MCP_EXTENSION_VERSION=1.5.0

FROM node:22.22.2-bookworm-slim AS node

ARG TARGETARCH

FROM ubuntu:24.04

ARG TARGETARCH
ARG CODEX_VERSION
ARG OPENCODE_VERSION
ARG GH_VERSION
ARG NPM_VERSION
ARG GO_VERSION
ARG RUST_VERSION
ARG PI_VERSION
ARG PRIME_AGENT_VERSION
ARG KIMI_CODE_VERSION
ARG PI_MCP_EXTENSION_VERSION
# INSTALL_KIMI=0 skips Kimi Code (its CDN blocks some cloud IPs, e.g. DigitalOcean).
ARG INSTALL_KIMI=1

ENV PATH=/opt/boxes-tools/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

COPY --from=node /usr/local/bin/node /opt/boxes-tools/bin/node
COPY --from=node /usr/local/lib/node_modules/npm /opt/boxes-tools/lib/node_modules/npm

RUN set -eux; \
    ln -s ../lib/node_modules/npm/bin/npm-cli.js /opt/boxes-tools/bin/npm; \
    ln -s ../lib/node_modules/npm/bin/npx-cli.js /opt/boxes-tools/bin/npx; \
    npm install --prefix /opt/boxes-tools --global "npm@${NPM_VERSION}"; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ca-certificates curl; \
    case "${TARGETARCH}" in \
        amd64) gh_arch=amd64; go_arch=amd64; rust_triple=x86_64-unknown-linux-gnu ;; \
        arm64) gh_arch=arm64; go_arch=arm64; rust_triple=aarch64-unknown-linux-gnu ;; \
        *) echo "unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    mkdir -p /opt/boxes-tools/go /opt/boxes-tools/rust; \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${go_arch}.tar.gz" \
        | tar -xz --strip-components=1 -C /opt/boxes-tools/go; \
    ln -s ../go/bin/go /opt/boxes-tools/bin/go; \
    ln -s ../go/bin/gofmt /opt/boxes-tools/bin/gofmt; \
    curl -fsSL "https://static.rust-lang.org/dist/rust-${RUST_VERSION}-${rust_triple}.tar.gz" \
        | tar -xz -C /tmp; \
    cd "/tmp/rust-${RUST_VERSION}-${rust_triple}"; \
    ./install.sh --prefix=/opt/boxes-tools/rust --components=rustc,cargo,rust-std-${rust_triple},rustfmt-preview,clippy-preview; \
    cd /; \
    ln -s ../rust/bin/cargo /opt/boxes-tools/bin/cargo; \
    ln -s ../rust/bin/rustc /opt/boxes-tools/bin/rustc; \
    ln -s ../rust/bin/rustdoc /opt/boxes-tools/bin/rustdoc; \
    ln -s ../rust/bin/rustfmt /opt/boxes-tools/bin/rustfmt; \
    ln -s ../rust/bin/cargo-clippy /opt/boxes-tools/bin/cargo-clippy; \
    ln -s ../rust/bin/clippy-driver /opt/boxes-tools/bin/clippy-driver; \
    curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${gh_arch}.tar.gz" \
        | tar -xz --strip-components=2 -C /opt/boxes-tools/bin "gh_${GH_VERSION}_linux_${gh_arch}/bin/gh"; \
    npm install --prefix /opt/boxes-tools --global \
        "@openai/codex@${CODEX_VERSION}" \
        "opencode-ai@${OPENCODE_VERSION}" \
        "@mariozechner/pi-coding-agent@${PI_VERSION}" \
        "pi-mcp-extension@${PI_MCP_EXTENSION_VERSION}" \
        @anthropic-ai/claude-code \
        "https://github.com/PrimeIntellect-ai/prime-agent/releases/download/v${PRIME_AGENT_VERSION}/prime-agent-${PRIME_AGENT_VERSION}.tgz"; \
    rm -rf /var/lib/apt/lists/* /root/.npm /tmp/*; \
    chmod 0755 /opt/boxes-tools/bin/node /opt/boxes-tools/bin/gh; \
    go version; \
    cargo --version; \
    rustc --version

RUN set -eux; \
    if [ "${INSTALL_KIMI}" = "1" ]; then \
        case "${TARGETARCH}" in amd64) kimi_arch=x64 ;; arm64) kimi_arch=arm64 ;; *) echo "unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; esac; \
        kimi_sha="$(curl -fsSL "https://code.kimi.com/kimi-code/binaries/${KIMI_CODE_VERSION}/manifest.json" | grep -o "\"linux-${kimi_arch}\": {[^}]*}" | sed -n 's/.*"checksum": "\([a-f0-9]\{64\}\)".*/\1/p')"; \
        test -n "${kimi_sha}"; \
        curl -fsSL "https://code.kimi.com/kimi-code/binaries/${KIMI_CODE_VERSION}/kimi-code-linux-${kimi_arch}" -o /opt/boxes-tools/bin/kimi; \
        echo "${kimi_sha}  /opt/boxes-tools/bin/kimi" | sha256sum -c -; \
        chmod 0755 /opt/boxes-tools/bin/kimi; \
        kimi --version; \
    fi; \
    apt-get purge -y --auto-remove curl; \
    rm -rf /var/lib/apt/lists/* /tmp/*

WORKDIR /opt/boxes-tools
