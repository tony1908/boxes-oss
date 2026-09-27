#!/usr/bin/env bash
set -euo pipefail

REPO="${BOXES_GITHUB_REPO:-boxes-oss/boxes}"
SYSBOX_VERSION="${SYSBOX_VERSION:-0.7.1}"
DOMAIN=""
EMAIL=""
PUBLIC_URL=""
IMAGE="boxes/box:latest"
IMAGE_CUSTOM=0
HOST_BASE=""
MEMORY_OVERCOMMIT="1.0"
TOOLS_IMAGE="${BOXES_TOOLS_IMAGE:-}"
S3_ENDPOINT=""
S3_BUCKET=""
S3_PREFIX="boxes"
S3_REGION=""
UPGRADE=0
ALLOW_RUNTIME_RESTART=0

usage() {
  printf 'Usage: %s --domain DOMAIN --email EMAIL [--public-url URL] [--host-base HOST_BASE] [--image IMAGE] [--memory-overcommit N] [--s3-endpoint URL --s3-bucket NAME [--s3-prefix P] [--s3-region R]]\n' "$0" >&2
  printf '       %s --upgrade [--allow-runtime-restart]\n' "$0" >&2
}

require_root() {
  if [[ "$(id -u)" -ne 0 ]]; then
    printf 'Run this installer as root (for example: sudo bash install.sh ...).\n' >&2
    exit 1
  fi
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) printf 'Unsupported architecture: %s\n' "$(uname -m)" >&2; exit 1 ;;
  esac
}

check_os() {
  if [[ ! -r /etc/os-release ]]; then
    printf 'This installer supports Ubuntu and Debian only.\n' >&2
    exit 1
  fi
  # shellcheck disable=SC1091
  . /etc/os-release
  case "${ID:-}" in
    ubuntu|debian) ;;
    *) printf 'This installer supports Ubuntu and Debian only (found %s).\n' "${ID:-unknown}" >&2; exit 1 ;;
  esac
}

configure_userns() {
  local setting=/proc/sys/kernel/apparmor_restrict_unprivileged_userns
  if [[ -e "$setting" && ! -e /etc/sysctl.d/99-boxes.conf ]]; then
    cat > /etc/sysctl.d/99-boxes.conf <<'SYSCTL'
# Boxes are the only workload on this host; Sysbox uses user namespaces itself,
# and this restriction prevents bwrap inside Boxes from creating its uid map.
kernel.apparmor_restrict_unprivileged_userns = 0
SYSCTL
    sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
    printf 'Boxes: relaxed the host AppArmor user-namespace restriction for Sysbox workloads.\n'
  fi
}

configure_swap() {
  local swapfile=/var/lib/boxes/swapfile total_kib size_mib created=0
  if ! swapon --show --noheadings | awk 'NF { found=1 } END { exit !found }'; then
    total_kib="$(awk '/^MemTotal:/ { print $2; exit }' /proc/meminfo)"
    size_mib=$((total_kib / 1024))
    ((size_mib > 8192)) && size_mib=8192
    ((size_mib > 0)) || size_mib=1
    install -d -m 0755 "$(dirname "$swapfile")"
    if [[ ! -f "$swapfile" ]]; then
      fallocate -l "${size_mib}M" "$swapfile" || dd if=/dev/zero of="$swapfile" bs=1M count="$size_mib" status=none
      created=1
    fi
    chmod 0600 "$swapfile"
    if ((created)); then mkswap -f "$swapfile"; fi
    swapon "$swapfile"
    printf 'Boxes: enabled %s MiB of host swap.\n' "$size_mib"
  fi
  if [[ ! -e /etc/sysctl.d/99-boxes-swap.conf ]]; then
    cat > /etc/sysctl.d/99-boxes-swap.conf <<'SYSCTL'
vm.swappiness = 10
SYSCTL
    sysctl -w vm.swappiness=10
  fi
}

install_docker() {
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL "https://download.docker.com/linux/${ID}/gpg" -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
  printf 'deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/%s %s stable\n' \
    "$(dpkg --print-architecture)" "$ID" "$VERSION_CODENAME" \
    > /etc/apt/sources.list.d/docker.list
  apt-get -o DPkg::Lock::Timeout=600 update
  DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=600 install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
  systemctl enable --now docker
}

install_sysbox() {
  local deb
  deb="$(mktemp --suffix=.deb)"
  curl -fsSL "https://github.com/nestybox/sysbox/releases/download/v${SYSBOX_VERSION}/sysbox-ce_${SYSBOX_VERSION}.linux_${ARCH}.deb" -o "$deb"
  DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=600 install -y "$deb"
  rm -f "$deb"
  systemctl restart docker
}

install_traefik() {
  local config=/etc/boxes/traefik/traefik.yml
  install -d -m 0755 /etc/boxes/traefik/dynamic
  if [[ ! -e /etc/boxes/traefik/acme.json ]]; then
    install -m 0600 /dev/null /etc/boxes/traefik/acme.json
  fi
  local script_dir
  script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
  if [[ -r "${script_dir}/traefik.yml" ]]; then
    sed "s|__ACME_EMAIL__|${EMAIL}|g" "${script_dir}/traefik.yml" > "$config"
  else
    curl -fsSL "https://raw.githubusercontent.com/${REPO}/main/install/traefik.yml" \
      | sed "s|__ACME_EMAIL__|${EMAIL}|g" > "$config"
  fi
  chmod 0644 "$config"
  docker pull traefik:v3.3
  docker rm -f boxes-traefik >/dev/null 2>&1 || true
  docker run -d --name boxes-traefik --restart unless-stopped \
    --network host \
    -v /etc/boxes/traefik:/etc/boxes/traefik \
    traefik:v3.3 --configFile=/etc/boxes/traefik/traefik.yml
}

install_registry() {
  install -d -m 0750 /etc/boxes /var/lib/boxes/registry
  local registry_env=/etc/boxes/registry.env
  if [[ -s "$registry_env" ]]; then
    # shellcheck disable=SC1090
    source "$registry_env"
  else
    BOXES_REGISTRY_USERNAME="boxes-$(openssl rand -hex 6)"
    BOXES_REGISTRY_PASSWORD="$(openssl rand -hex 32)"
  fi
  BOXES_REGISTRY_URL="$DOMAIN"
  cat > "$registry_env" <<EOF
BOXES_REGISTRY_URL=${BOXES_REGISTRY_URL}
BOXES_REGISTRY_USERNAME=${BOXES_REGISTRY_USERNAME}
BOXES_REGISTRY_PASSWORD=${BOXES_REGISTRY_PASSWORD}
EOF
  chmod 0600 "$registry_env"
  htpasswd -Bbn "$BOXES_REGISTRY_USERNAME" "$BOXES_REGISTRY_PASSWORD" > /etc/boxes/registry.htpasswd
  chmod 0600 /etc/boxes/registry.htpasswd
  docker pull registry:2
  docker rm -f boxes-registry >/dev/null 2>&1 || true
  docker run -d --name boxes-registry --restart unless-stopped \
    -p 127.0.0.1:5000:5000 \
    -v /var/lib/boxes/registry:/var/lib/registry \
    -v /etc/boxes/registry.htpasswd:/auth/htpasswd:ro \
    -e REGISTRY_AUTH=htpasswd \
    -e REGISTRY_AUTH_HTPASSWD_REALM='Boxes Registry' \
    -e REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd \
    registry:2
}

release_asset() {
  local name=$1 tag version tmp archive
  if [[ -n "${BOXES_BINARIES_DIR:-}" ]]; then
    if [[ ! -f "${BOXES_BINARIES_DIR}/${name}" ]]; then
      printf 'Local binary not found: %s/%s (BOXES_BINARIES_DIR is set).\n' "$BOXES_BINARIES_DIR" "$name" >&2
      exit 1
    fi
    install -m 0755 "${BOXES_BINARIES_DIR}/${name}" "/usr/local/bin/${name}"
    return
  fi
  tag="$(curl -fsSL -H 'Accept: application/vnd.github+json' "https://api.github.com/repos/${REPO}/releases/latest" | jq -er '.tag_name')"
  version="${tag#v}"
  tmp="$(mktemp -d)"
  archive="${tmp}/boxes_${version}_linux_${ARCH}.tar.gz"
  curl -fsSL "https://github.com/${REPO}/releases/download/${tag}/boxes_${version}_linux_${ARCH}.tar.gz" -o "$archive"
  tar -xzf "$archive" -C "$tmp" "$name"
  install -m 0755 "${tmp}/${name}" "/usr/local/bin/${name}"
  rm -rf "$tmp"
}

install_agent_platform_binaries() {
  [[ -n "${BOXES_BINARIES_DIR:-}" ]] || return 0
  local arch name
  install -d -m 0755 /var/lib/boxes/bin
  for arch in amd64 arm64; do
    name="boxes-agent-linux-${arch}"
    if [[ ! -f "${BOXES_BINARIES_DIR}/${name}" ]]; then
      printf 'Local agent binary not found: %s/%s (BOXES_BINARIES_DIR is set).\n' "$BOXES_BINARIES_DIR" "$name" >&2
      exit 1
    fi
    install -m 0755 "${BOXES_BINARIES_DIR}/${name}" "/var/lib/boxes/bin/${name}"
  done
}

install_cli_binaries() {
  local strict=${1:-0} destination=/var/lib/boxes/cli name
  install -d -m 0755 "$destination"
  local -a platforms=(darwin-arm64 darwin-amd64 linux-amd64 linux-arm64 windows-amd64 windows-arm64)
  local tag="" version="" tmp="" platform goos goarch archive binary
  if [[ -n "${BOXES_BINARIES_DIR:-}" ]]; then
    for platform in "${platforms[@]}"; do
      name="boxes-${platform}"
      [[ "$platform" == windows-* ]] && name+=".exe"
      if [[ ! -f "${BOXES_BINARIES_DIR}/${name}" ]]; then
        if ((strict == 0)); then continue; fi
        printf 'Local CLI binary not found: %s/%s (BOXES_BINARIES_DIR is set).\n' "$BOXES_BINARIES_DIR" "$name" >&2
        exit 1
      fi
      install -m 0755 "${BOXES_BINARIES_DIR}/${name}" "${destination}/${name}"
    done
    return
  fi
  ((strict)) || return 0
  tag="$(curl -fsSL -H 'Accept: application/vnd.github+json' "https://api.github.com/repos/${REPO}/releases/latest" | jq -er '.tag_name')"
  version="${tag#v}"
  tmp="$(mktemp -d)"
  for platform in "${platforms[@]}"; do
    name="boxes-${platform}"
    goos="${platform%-*}"
    goarch="${platform##*-}"
    if [[ "$goos" == windows ]]; then
      archive="${tmp}/boxes_${version}_${goos}_${goarch}.zip"
      curl -fsSL "https://github.com/${REPO}/releases/download/${tag}/boxes_${version}_${goos}_${goarch}.zip" -o "$archive"
      unzip -p "$archive" boxes.exe > "${tmp}/boxes.exe"
      binary="${tmp}/boxes.exe"
    else
      archive="${tmp}/boxes_${version}_${goos}_${goarch}.tar.gz"
      curl -fsSL "https://github.com/${REPO}/releases/download/${tag}/boxes_${version}_${goos}_${goarch}.tar.gz" -o "$archive"
      tar -xzf "$archive" -C "$tmp" boxes
      binary="${tmp}/boxes"
    fi
    [[ "$goos" == windows ]] && name+=".exe"
    install -m 0755 "$binary" "${destination}/${name}"
    rm -f "$binary"
  done
  rm -rf "$tmp"
}

valid_ipv4() {
  local address=$1 part
  local -a octets
  [[ "$address" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || return 1
  IFS=. read -r -a octets <<< "$address"
  ((${#octets[@]} == 4)) || return 1
  for part in "${octets[@]}"; do
    [[ "$part" =~ ^[0-9]{1,3}$ ]] && ((10#$part <= 255)) || return 1
  done
}

detect_host_base() {
  local address candidate
  address="$(curl -fsS --max-time 10 https://api.ipify.org 2>/dev/null || true)"
  if ! valid_ipv4 "$address"; then
    address=""
    for candidate in $(hostname -I 2>/dev/null || true); do
      if valid_ipv4 "$candidate"; then
        address="$candidate"
        break
      fi
    done
  fi
  if [[ -z "$address" ]]; then
    printf 'Could not detect a public IPv4 address. Re-run with --host-base HOST_BASE.\n' >&2
    exit 1
  fi
  printf '%s.sslip.io' "$address"
}

ensure_box_image() {
  if docker image inspect "$IMAGE" >/dev/null 2>&1; then
    return
  fi
  if ! docker pull "$IMAGE"; then
    printf 'Box image %s is not available locally and could not be pulled. Check the image reference and registry access.\n' "$IMAGE" >&2
    exit 1
  fi
}

write_systemd_units() {
  cat > /etc/systemd/system/boxes-server.service <<'UNIT'
[Unit]
Description=Boxes control plane server
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
Environment=BOXES_MEMORY_OVERCOMMIT=1.0
EnvironmentFile=/etc/boxes/server.env
ExecStart=/usr/local/bin/boxes-server --addr :8080 --data-dir /var/lib/boxes --public-url ${BOXES_PUBLIC_URL} --image ${BOXES_IMAGE} --snapshot-interval ${BOXES_SNAPSHOT_INTERVAL} --memory-overcommit ${BOXES_MEMORY_OVERCOMMIT}
Restart=on-failure
RestartSec=5
StateDirectory=boxes
UMask=0077

[Install]
WantedBy=multi-user.target
UNIT

  cat > /etc/systemd/system/boxes-agent.service <<'UNIT'
[Unit]
Description=Boxes node agent
After=network-online.target docker.service boxes-server.service
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/boxes/agent.env
ExecStart=/usr/local/bin/boxes-agent
Restart=always
RestartSec=5
UMask=0077

[Install]
WantedBy=multi-user.target
UNIT
}

env_read() {
  local file=$1 wanted=$2 key value
  REPLY=""
  [[ -r "$file" ]] || return 1
  while IFS='=' read -r key value || [[ -n "$key" ]]; do
    if [[ "$key" == "$wanted" ]]; then
      REPLY="$value"
      return 0
    fi
  done < "$file"
  return 1
}

env_add_default() {
  local file=$1 key=$2 value=$3
  if ! env_read "$file" "$key"; then
    printf '%s=%s\n' "$key" "$value" >> "$file"
  fi
}

verify_registry_route() {
  local public_url=$1 headers status attempt
  headers="$(mktemp)"
  for attempt in {1..20}; do
    status="$(curl -s -D "$headers" -o /dev/null -w '%{http_code}' "${public_url%/}/v2/" 2>/dev/null || true)"
    if [[ "$status" == 401 ]] && awk 'tolower($0) ~ /^docker-distribution-api-version:/ { found=1 } END { exit !found }' "$headers"; then
      rm -f "$headers"
      return 0
    fi
    sleep 3
  done
  rm -f "$headers"
  printf 'Registry route verification failed for %s/v2/: expected HTTP 401 and the Docker-Distribution-Api-Version response header. Check the Traefik registry route and registry container.\n' "${public_url%/}" >&2
  return 1
}

runtime_admin_post() {
  local endpoint=$1 payload=$2 response_file=$3
  curl -sS --connect-timeout 2 --max-time 3600 --config "$RUNTIME_AUTH_CONFIG" \
    -H 'Content-Type: application/json' -o "$response_file" -w '%{http_code}' \
    --data "$payload" "http://127.0.0.1:8080/api/v1/admin/runtime/${endpoint}"
}

clear_runtime_auth() {
  if [[ -n "${RUNTIME_AUTH_CONFIG:-}" ]]; then
    rm -f "$RUNTIME_AUTH_CONFIG"
    unset RUNTIME_AUTH_CONFIG
  fi
}

resume_runtime_boxes() {
  local ids_file=$1 work_dir=$2 payload status attempt expected
  [[ -s "$ids_file" ]] || return 0
  expected="$(wc -l < "$ids_file" | tr -d ' ')"
  payload="$(jq -Rsc '{boxIds: split("\n") | map(select(length > 0))}' "$ids_file")"
  for attempt in {1..120}; do
    status="$(runtime_admin_post resume "$payload" "${work_dir}/resume.json" 2>/dev/null || true)"
    if [[ "$status" == 200 ]] && jq -e --argjson expected "$expected" '(.failed | length) == 0 and (.resumed | length) == $expected' "${work_dir}/resume.json" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  printf 'Could not resume every box after the runtime restart. The stopped box IDs are saved in %s.\n' "$ids_file" >&2
  return 1
}

upgrade_sysbox_if_needed() {
  local installed_version installed_release deb work_dir stop_status stopped_count
  installed_version="$(dpkg-query -W -f='${Version}' sysbox-ce 2>/dev/null || true)"
  installed_release="${installed_version%.linux}"
  if [[ -n "$installed_version" ]] && dpkg --compare-versions "$installed_release" eq "$SYSBOX_VERSION"; then
    printf 'Sysbox %s is already installed; leaving Sysbox, containerd, and Docker untouched.\n' "$SYSBOX_VERSION"
    return 0
  fi
  if ((ALLOW_RUNTIME_RESTART == 0)); then
    printf 'Sysbox version change required (installed: %s, requested: %s). Refusing to restart the runtime; rerun with --upgrade --allow-runtime-restart during a maintenance window.\n' "${installed_version:-not installed}" "$SYSBOX_VERSION" >&2
    return 1
  fi
  if [[ ! -r /var/lib/boxes/admin.token ]]; then
    printf 'Cannot safely upgrade Sysbox: /var/lib/boxes/admin.token is unavailable for the local server API.\n' >&2
    return 1
  fi

  work_dir="$(mktemp -d /run/boxes-runtime-upgrade.XXXXXX)"
  chmod 0700 "$work_dir"
  RUNTIME_AUTH_CONFIG="${work_dir}/curl.conf"
  printf 'header = "Authorization: Bearer %s"\n' "$(< /var/lib/boxes/admin.token)" > "$RUNTIME_AUTH_CONFIG"
  chmod 0600 "$RUNTIME_AUTH_CONFIG"
  : > "${work_dir}/stopped.ids"

  deb="${work_dir}/sysbox-ce_${SYSBOX_VERSION}.linux_${ARCH}.deb"
  if ! curl -fsSL "https://github.com/nestybox/sysbox/releases/download/v${SYSBOX_VERSION}/sysbox-ce_${SYSBOX_VERSION}.linux_${ARCH}.deb" -o "$deb"; then
    rm -rf "$work_dir"
    clear_runtime_auth
    printf 'Could not download Sysbox %s; no boxes were stopped and no runtime packages were changed.\n' "$SYSBOX_VERSION" >&2
    return 1
  fi

  stop_status="$(runtime_admin_post stop '{}' "${work_dir}/stop.json" 2>/dev/null || true)"
  if ! jq -e '.stopped | type == "array"' "${work_dir}/stop.json" >/dev/null 2>&1; then
    rm -rf "$work_dir"
    clear_runtime_auth
    printf 'Cannot safely upgrade Sysbox: the local server API did not return a valid box-stop response. No runtime packages were changed.\n' >&2
    return 1
  fi
  jq -r '.stopped[]?' "${work_dir}/stop.json" > "${work_dir}/stopped.ids"
  stopped_count="$(wc -l < "${work_dir}/stopped.ids" | tr -d ' ')"
  if [[ "$stop_status" != 200 ]] || ! jq -e '(.failed | type == "array") and (.failed | length == 0)' "${work_dir}/stop.json" >/dev/null 2>&1; then
    if ! resume_runtime_boxes "${work_dir}/stopped.ids" "$work_dir"; then
      clear_runtime_auth
      return 1
    fi
    rm -rf "$work_dir"
    clear_runtime_auth
    printf 'Cannot safely upgrade Sysbox: the server could not snapshot and stop every running box. No runtime packages were changed.\n' >&2
    return 1
  fi

  if ! DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=600 install -y "$deb"; then
    systemctl restart docker || true
    if ! resume_runtime_boxes "${work_dir}/stopped.ids" "$work_dir"; then
      clear_runtime_auth
      return 1
    fi
    rm -rf "$work_dir"
    clear_runtime_auth
    printf 'Sysbox %s installation failed; stopped boxes were resumed. Check the package manager before retrying.\n' "$SYSBOX_VERSION" >&2
    return 1
  fi
  if ! systemctl restart docker; then
    printf 'Sysbox was upgraded, but Docker could not restart. Boxes remain stopped; repair Docker, then resume them through the server API. IDs: %s\n' "${work_dir}/stopped.ids" >&2
    clear_runtime_auth
    return 1
  fi
  if ! resume_runtime_boxes "${work_dir}/stopped.ids" "$work_dir"; then
    clear_runtime_auth
    return 1
  fi
  rm -rf "$work_dir"
  clear_runtime_auth
  printf 'Upgraded Sysbox from %s to %s; restarted Docker and resumed %s boxes from snapshots.\n' "${installed_version:-not installed}" "$SYSBOX_VERSION" "$stopped_count"
}

ensure_htpasswd() {
  if ! command -v htpasswd >/dev/null 2>&1; then
    apt-get -o DPkg::Lock::Timeout=600 update
    DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=600 install -y apache2-utils
  fi
}

upgrade_registry() {
  local domain=$1 username password registry_url default_value
  install -d -m 0750 /etc/boxes /var/lib/boxes/registry /etc/boxes/traefik/dynamic

  if ! env_read /etc/boxes/server.env BOXES_REGISTRY_URL; then
    default_value="$domain"
    if env_read /etc/boxes/registry.env BOXES_REGISTRY_URL; then default_value="$REPLY"; fi
    env_add_default /etc/boxes/server.env BOXES_REGISTRY_URL "$default_value"
  fi
  if ! env_read /etc/boxes/server.env BOXES_REGISTRY_USERNAME; then
    if env_read /etc/boxes/registry.env BOXES_REGISTRY_USERNAME; then
      default_value="$REPLY"
    else
      default_value="boxes-$(openssl rand -hex 6)"
    fi
    env_add_default /etc/boxes/server.env BOXES_REGISTRY_USERNAME "$default_value"
  fi
  if ! env_read /etc/boxes/server.env BOXES_REGISTRY_PASSWORD; then
    if env_read /etc/boxes/registry.env BOXES_REGISTRY_PASSWORD; then
      default_value="$REPLY"
    else
      default_value="$(openssl rand -hex 32)"
    fi
    env_add_default /etc/boxes/server.env BOXES_REGISTRY_PASSWORD "$default_value"
  fi
  env_read /etc/boxes/server.env BOXES_REGISTRY_URL || { printf 'Registry URL is missing from server configuration.\n' >&2; exit 1; }
  registry_url="$REPLY"
  env_read /etc/boxes/server.env BOXES_REGISTRY_USERNAME || { printf 'Registry username is missing from server configuration.\n' >&2; exit 1; }
  username="$REPLY"
  env_read /etc/boxes/server.env BOXES_REGISTRY_PASSWORD || { printf 'Registry password is missing from server configuration.\n' >&2; exit 1; }
  password="$REPLY"
  if [[ ! "$registry_url" =~ ^[A-Za-z0-9.-]+(:[0-9]{1,5})?$ ]]; then
    printf 'Registry URL in server configuration is invalid.\n' >&2
    exit 1
  fi

  if [[ ! -s /etc/boxes/registry.htpasswd ]]; then
    ensure_htpasswd
    htpasswd -Bbn "$username" "$password" > /etc/boxes/registry.htpasswd
    chmod 0600 /etc/boxes/registry.htpasswd
  fi
  printf 'http:\n  routers:\n    boxes-registry:\n      rule: "Host(`%s`) && PathPrefix(`/v2/`)"\n      entryPoints: [websecure]\n      service: boxes-registry\n      priority: 100\n      tls:\n        certResolver: letsencrypt\n  services:\n    boxes-registry:\n      loadBalancer:\n        servers:\n          - url: "http://127.0.0.1:5000"\n' "$registry_url" > /etc/boxes/traefik/dynamic/registry.yml
  chmod 0600 /etc/boxes/traefik/dynamic/registry.yml
  if ! docker container inspect boxes-registry >/dev/null 2>&1; then
    docker pull registry:2
    docker run -d --name boxes-registry --restart unless-stopped \
      -p 127.0.0.1:5000:5000 \
      -v /var/lib/boxes/registry:/var/lib/registry \
      -v /etc/boxes/registry.htpasswd:/auth/htpasswd:ro \
      -e REGISTRY_AUTH=htpasswd \
      -e REGISTRY_AUTH_HTPASSWD_REALM='Boxes Registry' \
      -e REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd \
      registry:2 >/dev/null
  fi
}

upgrade_install() {
  local public_url domain image tools_image
  require_root
  if [[ ! -s /etc/boxes/server.env ]]; then
    printf 'Cannot upgrade: /etc/boxes/server.env does not exist; run a fresh install first.\n' >&2
    exit 1
  fi
  if [[ ! -s /etc/boxes/agent.env ]]; then
    printf 'Cannot upgrade: /etc/boxes/agent.env does not exist; refusing to recreate node credentials.\n' >&2
    exit 1
  fi
  env_read /etc/boxes/server.env BOXES_PUBLIC_URL || { printf 'Cannot upgrade: BOXES_PUBLIC_URL is missing from /etc/boxes/server.env.\n' >&2; exit 1; }
  public_url="$REPLY"
  domain="${public_url#https://}"
  domain="${domain%%/*}"
  if [[ ! "$domain" =~ ^[A-Za-z0-9.-]+(:[0-9]{1,5})?$ ]]; then
    printf 'Cannot upgrade: BOXES_PUBLIC_URL in /etc/boxes/server.env is invalid.\n' >&2
    exit 1
  fi
  detect_arch
  check_os
  upgrade_sysbox_if_needed
  configure_userns
  configure_swap
  install -d -m 0750 /etc/boxes /var/lib/boxes

  image="${domain}/boxes/box:latest"
  tools_image="${domain}/boxes/tools:latest"
  env_add_default /etc/boxes/server.env BOXES_IMAGE "$image"
  env_add_default /etc/boxes/server.env BOXES_TOOLS_IMAGE "$tools_image"
  env_add_default /etc/boxes/server.env BOXES_SNAPSHOT_INTERVAL 60s
  env_add_default /etc/boxes/server.env BOXES_MEMORY_OVERCOMMIT 1.0
  env_add_default /etc/boxes/agent.env BOXES_TOOLS_IMAGE "$tools_image"
  env_add_default /etc/boxes/agent.env BOXES_TRAEFIK_DIR /etc/boxes/traefik/dynamic
  env_add_default /etc/boxes/agent.env BOXES_STORAGE_FILE /etc/boxes/storage.json
  chmod 0600 /etc/boxes/server.env
  if [[ -e /etc/boxes/agent.env ]]; then chmod 0600 /etc/boxes/agent.env; fi

  upgrade_registry "$domain"
  docker network inspect boxes-net >/dev/null 2>&1 || docker network create --driver bridge --opt com.docker.network.bridge.enable_icc=false boxes-net >/dev/null
  install_agent_platform_binaries
  release_asset boxes-server
  release_asset boxes-agent
  install_cli_binaries 1
  write_systemd_units
  systemctl daemon-reload
  systemctl enable boxes-server.service boxes-agent.service
  systemctl restart boxes-server.service boxes-agent.service
  verify_registry_route "$public_url"
  printf 'Boxes services upgraded for %s.\n' "$public_url"
}

while (($#)); do
  case "$1" in
    --upgrade) UPGRADE=1; shift ;;
    --allow-runtime-restart) ALLOW_RUNTIME_RESTART=1; shift ;;
    --domain) (($# >= 2)) || { usage; exit 2; }; DOMAIN="$2"; shift 2 ;;
    --email) (($# >= 2)) || { usage; exit 2; }; EMAIL="$2"; shift 2 ;;
    --public-url) (($# >= 2)) || { usage; exit 2; }; PUBLIC_URL="$2"; shift 2 ;;
    --host-base) (($# >= 2)) || { usage; exit 2; }; HOST_BASE="$2"; shift 2 ;;
    --image) (($# >= 2)) || { usage; exit 2; }; IMAGE="$2"; IMAGE_CUSTOM=1; shift 2 ;;
    --memory-overcommit) (($# >= 2)) || { usage; exit 2; }; MEMORY_OVERCOMMIT="$2"; shift 2 ;;
    --s3-endpoint) (($# >= 2)) || { usage; exit 2; }; S3_ENDPOINT="$2"; shift 2 ;;
    --s3-bucket) (($# >= 2)) || { usage; exit 2; }; S3_BUCKET="$2"; shift 2 ;;
    --s3-prefix) (($# >= 2)) || { usage; exit 2; }; S3_PREFIX="$2"; shift 2 ;;
    --s3-region) (($# >= 2)) || { usage; exit 2; }; S3_REGION="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) printf 'Unknown option: %s\n' "$1" >&2; usage; exit 2 ;;
  esac
done

if ((UPGRADE)); then
  if [[ -n "$DOMAIN$EMAIL$PUBLIC_URL$HOST_BASE$S3_ENDPOINT$S3_BUCKET$S3_REGION" || $IMAGE_CUSTOM -ne 0 ]]; then
    printf 'Use --upgrade by itself; it preserves the existing configuration.\n' >&2
    exit 2
  fi
  upgrade_install
  exit 0
fi
if ((ALLOW_RUNTIME_RESTART)); then
  printf '--allow-runtime-restart is only valid with --upgrade.\n' >&2
  exit 2
fi

require_root
if [[ ! "$DOMAIN" =~ ^[A-Za-z0-9.-]+$ || ! "$EMAIL" =~ ^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$ ]]; then
  usage
  printf 'A valid --domain and --email are required.\n' >&2
  exit 2
fi
PUBLIC_URL="${PUBLIC_URL:-https://${DOMAIN}}"
if ((IMAGE_CUSTOM == 0)); then IMAGE="${DOMAIN}/boxes/box:latest"; fi
TOOLS_IMAGE="${TOOLS_IMAGE:-${DOMAIN}/boxes/tools:latest}"
if [[ ! "$PUBLIC_URL" =~ ^https://[A-Za-z0-9.-]+(:[0-9]{1,5})?$ ]]; then
  printf 'The public URL must be an https origin (for example, https://boxes.example.com).\n' >&2
  exit 2
fi
if [[ "$PUBLIC_URL" != "https://${DOMAIN}" ]]; then
  printf 'The public URL host must match --domain.\n' >&2
  exit 2
fi
if [[ ! "$IMAGE" =~ ^[A-Za-z0-9._/:@-]+$ ]]; then
  printf 'The image must be a valid container image reference.\n' >&2
  exit 2
fi
if [[ ! "$MEMORY_OVERCOMMIT" =~ ^[0-9]+([.][0-9]+)?$ ]] || ! awk -v value="$MEMORY_OVERCOMMIT" 'BEGIN { exit !(value > 0) }'; then
  printf 'Memory overcommit must be a positive number.\n' >&2
  exit 2
fi
if [[ "$TOOLS_IMAGE" != "none" && ! "$TOOLS_IMAGE" =~ ^[A-Za-z0-9._/:@-]+$ ]]; then
  printf 'The tools image must be a valid container image reference or none.\n' >&2
  exit 2
fi
if [[ -n "$HOST_BASE" && ! "$HOST_BASE" =~ ^[A-Za-z0-9.-]+$ ]]; then
  printf 'The host base must be a valid DNS hostname.\n' >&2
  exit 2
fi
RESTIC_REPOSITORY="/var/lib/boxes/restic"
if [[ -z "$S3_ENDPOINT" && -z "$S3_BUCKET" && -n "$S3_REGION" ]]; then
  printf 'S3 region requires --s3-endpoint and --s3-bucket.\n' >&2
  exit 2
fi
if [[ -n "$S3_ENDPOINT" || -n "$S3_BUCKET" ]]; then
  if [[ ! "$S3_ENDPOINT" =~ ^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?$ || ! "$S3_BUCKET" =~ ^[A-Za-z0-9._-]+$ || ! "$S3_PREFIX" =~ ^[A-Za-z0-9._/-]+$ || "$S3_PREFIX" == *..* ]]; then
    printf 'S3 endpoint, bucket, or prefix is invalid.\n' >&2
    exit 2
  fi
  if [[ ! "${BOXES_S3_ACCESS_KEY:-}" =~ ^[A-Za-z0-9._+=/-]+$ || ! "${BOXES_S3_SECRET_KEY:-}" =~ ^[A-Za-z0-9._+=/-]+$ ]]; then
    printf 'Set BOXES_S3_ACCESS_KEY and BOXES_S3_SECRET_KEY in the environment to use S3 storage.\n' >&2
    exit 2
  fi
  S3_ENDPOINT="${S3_ENDPOINT%/}"
  RESTIC_REPOSITORY="s3:${S3_ENDPOINT}/${S3_BUCKET}/${S3_PREFIX#/}"
fi

detect_arch
check_os
configure_userns
export DEBIAN_FRONTEND=noninteractive
# Fresh VPSes run first-boot apt jobs; wait so apt-get update does not race them for the lists lock.
if command -v cloud-init >/dev/null 2>&1; then cloud-init status --wait >/dev/null 2>&1 || true; fi
apt-get -o DPkg::Lock::Timeout=600 update
apt-get -o DPkg::Lock::Timeout=600 install -y apache2-utils ca-certificates curl gnupg jq openssl procps restic unzip
configure_swap
install_docker
# Sysbox refuses to install while any container exists, so it goes before the registry.
install_sysbox
install_registry
if ((IMAGE_CUSTOM)); then ensure_box_image; fi
docker network inspect boxes-net >/dev/null 2>&1 || docker network create --driver bridge --opt com.docker.network.bridge.enable_icc=false boxes-net >/dev/null
install_traefik

install -d -m 0750 /etc/boxes
if [[ "$RESTIC_REPOSITORY" == /* ]]; then install -d -m 0750 "$RESTIC_REPOSITORY"; fi
if [[ ! -s /etc/boxes/join-token ]]; then
  openssl rand -hex 32 > /etc/boxes/join-token
  chmod 0600 /etc/boxes/join-token
fi
if [[ ! -s /etc/boxes/restic-password ]]; then
  openssl rand -hex 32 > /etc/boxes/restic-password
  chmod 0600 /etc/boxes/restic-password
fi
JOIN_TOKEN="$(< /etc/boxes/join-token)"
RESTIC_PASSWORD_VALUE="$(< /etc/boxes/restic-password)"
HOST_BASE="${HOST_BASE:-$(detect_host_base)}"

install_agent_platform_binaries
release_asset boxes-server
release_asset boxes-agent
install_cli_binaries
cat > /etc/boxes/server.env <<EOF
BOXES_PUBLIC_URL=${PUBLIC_URL}
BOXES_IMAGE=${IMAGE}
BOXES_TOOLS_IMAGE=${TOOLS_IMAGE}
BOXES_REGISTRY_URL=${BOXES_REGISTRY_URL}
BOXES_REGISTRY_USERNAME=${BOXES_REGISTRY_USERNAME}
BOXES_REGISTRY_PASSWORD=${BOXES_REGISTRY_PASSWORD}
BOXES_BOOTSTRAP_JOIN_TOKEN=${JOIN_TOKEN}
BOXES_BOOTSTRAP_NODE_NAME=local
BOXES_BOOTSTRAP_HOST_BASE=${HOST_BASE}
BOXES_SNAPSHOT_INTERVAL=60s
BOXES_MEMORY_OVERCOMMIT=${MEMORY_OVERCOMMIT}
BOXES_STORAGE_REPOSITORY=${RESTIC_REPOSITORY}
BOXES_STORAGE_ENDPOINT=${S3_ENDPOINT}
BOXES_STORAGE_BUCKET=${S3_BUCKET}
BOXES_STORAGE_PREFIX=${S3_PREFIX}
BOXES_STORAGE_REGION=${S3_REGION}
BOXES_STORAGE_ACCESS_KEY=${BOXES_S3_ACCESS_KEY:-}
BOXES_STORAGE_SECRET_KEY=${BOXES_S3_SECRET_KEY:-}
BOXES_STORAGE_RESTIC_PASSWORD=${RESTIC_PASSWORD_VALUE}
EOF
cat > /etc/boxes/agent.env <<EOF
BOXES_SERVER_URL=wss://${DOMAIN}/api/v1/agent/connect
BOXES_NODE_TOKEN=${JOIN_TOKEN}
BOXES_RUNTIME=sysbox-runc
BOXES_TRAEFIK_DIR=/etc/boxes/traefik/dynamic
BOXES_TOOLS_IMAGE=${TOOLS_IMAGE}
RESTIC_REPOSITORY=${RESTIC_REPOSITORY}
RESTIC_PASSWORD=${RESTIC_PASSWORD_VALUE}
AWS_ACCESS_KEY_ID=${BOXES_S3_ACCESS_KEY:-}
AWS_SECRET_ACCESS_KEY=${BOXES_S3_SECRET_KEY:-}
AWS_DEFAULT_REGION=${S3_REGION}
EOF
chmod 0600 /etc/boxes/server.env /etc/boxes/agent.env

printf 'http:\n  routers:\n    boxes-server:\n      rule: "Host(`%s`)"\n      entryPoints: [websecure]\n      service: boxes-server\n      tls:\n        certResolver: letsencrypt\n  services:\n    boxes-server:\n      loadBalancer:\n        servers:\n          - url: "http://127.0.0.1:8080"\n' "$DOMAIN" \
  > /etc/boxes/traefik/dynamic/boxes-server.yml
chmod 0600 /etc/boxes/traefik/dynamic/boxes-server.yml
printf 'http:\n  routers:\n    boxes-registry:\n      rule: "Host(`%s`) && PathPrefix(`/v2/`)"\n      entryPoints: [websecure]\n      service: boxes-registry\n      priority: 100\n      tls:\n        certResolver: letsencrypt\n  services:\n    boxes-registry:\n      loadBalancer:\n        servers:\n          - url: "http://127.0.0.1:5000"\n' "$DOMAIN" \
  > /etc/boxes/traefik/dynamic/registry.yml
chmod 0600 /etc/boxes/traefik/dynamic/registry.yml
for attempt in {1..10}; do
  if printf '%s\n' "$BOXES_REGISTRY_PASSWORD" | docker login "$BOXES_REGISTRY_URL" \
    --username "$BOXES_REGISTRY_USERNAME" --password-stdin; then
    break
  fi
  if ((attempt == 10)); then
    printf 'Could not authenticate to the Boxes registry at %s.\n' "$BOXES_REGISTRY_URL" >&2
    exit 1
  fi
  sleep 3
done

write_systemd_units
systemctl daemon-reload
systemctl enable --now boxes-server.service
systemctl enable --now boxes-agent.service
verify_registry_route "$PUBLIC_URL"

printf 'Boxes dashboard: %s\n' "$PUBLIC_URL"
