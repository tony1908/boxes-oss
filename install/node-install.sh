#!/usr/bin/env bash
set -euo pipefail
TIMING_FILE=/var/lib/boxes/node-install-timing.json
INSTALL_STARTED=$SECONDS
declare -A PHASE_SECONDS=()
phase_start=$SECONDS
record_phase() {
  local phase="$1"
  PHASE_SECONDS["$phase"]=$((SECONDS - phase_start))
  phase_start=$SECONDS
  install -d -m 0755 /var/lib/boxes
  local first=1 key
  { printf '{'; for key in "${!PHASE_SECONDS[@]}"; do
      if ((first)); then first=0; else printf ','; fi
      printf '"%s":%s' "$key" "${PHASE_SECONDS[$key]}"
    done
    printf '}\n'; } > "$TIMING_FILE"
}

REPO="${BOXES_GITHUB_REPO:-boxes-oss/boxes}"
SYSBOX_VERSION="${SYSBOX_VERSION:-0.7.1}"
SERVER="${BOXES_SERVER_URL:-}"
TOKEN="${BOXES_NODE_TOKEN:-}"
PUBLIC_URL="${BOXES_PUBLIC_URL:-}"
RUNTIME="sysbox-runc"
RESTIC_REPOSITORY="${RESTIC_REPOSITORY:-/var/lib/boxes/restic}"
RESTIC_PASSWORD_VALUE="${RESTIC_PASSWORD:-}"
S3_ENDPOINT=""
S3_BUCKET=""
S3_PREFIX="boxes"
S3_REGION=""
EMAIL="admin@example.com"
TOOLS_IMAGE="${BOXES_TOOLS_IMAGE:-boxes/tools:latest}"
REGISTRY_URL="${BOXES_REGISTRY_URL:-}"
REGISTRY_USERNAME="${BOXES_REGISTRY_USERNAME:-}"
REGISTRY_PASSWORD="${BOXES_REGISTRY_PASSWORD:-}"
INSTALL_MODE="${BOXES_INSTALL_MODE:-${BOXES_NODE_MODE:-}}"

usage() {
  printf 'Usage: BOXES_SERVER_URL=https://boxes.example.com BOXES_NODE_TOKEN=TOKEN %s [--server URL --token TOKEN] [--runtime sysbox-runc|runc] [--restic-repository PATH] [--restic-password PASSWORD] [--s3-endpoint URL --s3-bucket NAME [--s3-prefix P] [--s3-region R]] [--email EMAIL]\n' "$0" >&2
}

configure_swap() {
  local swapfile=/var/lib/boxes/swapfile total_kib size_mib
  if ! swapon --show --noheadings | awk 'NF { found=1 } END { exit !found }'; then
    total_kib="$(awk '/^MemTotal:/ { print $2; exit }' /proc/meminfo)"
    size_mib=$((total_kib / 1024))
    ((size_mib > 8192)) && size_mib=8192
    ((size_mib > 0)) || size_mib=1
    install -d -m 0755 "$(dirname "$swapfile")"
    if [[ ! -f "$swapfile" ]]; then
      fallocate -l "${size_mib}M" "$swapfile" || dd if=/dev/zero of="$swapfile" bs=1M count="$size_mib" status=none
    fi
    chmod 0600 "$swapfile"
    mkswap -f "$swapfile"
    swapon "$swapfile"
    printf 'Boxes: enabled %s MiB of host swap.\n' "$size_mib"
  fi
  cat > /etc/sysctl.d/99-boxes-swap.conf <<'SYSCTL'
vm.swappiness = 10
SYSCTL
  sysctl -w vm.swappiness=10
}

while (($#)); do
  case "$1" in
    --server) (($# >= 2)) || { usage; exit 2; }; SERVER="$2"; shift 2 ;;
    --token) (($# >= 2)) || { usage; exit 2; }; TOKEN="$2"; shift 2 ;;
    --runtime) (($# >= 2)) || { usage; exit 2; }; RUNTIME="$2"; shift 2 ;;
    --restic-repository) (($# >= 2)) || { usage; exit 2; }; RESTIC_REPOSITORY="$2"; shift 2 ;;
    --restic-password) (($# >= 2)) || { usage; exit 2; }; RESTIC_PASSWORD_VALUE="$2"; shift 2 ;;
    --s3-endpoint) (($# >= 2)) || { usage; exit 2; }; S3_ENDPOINT="$2"; shift 2 ;;
    --s3-bucket) (($# >= 2)) || { usage; exit 2; }; S3_BUCKET="$2"; shift 2 ;;
    --s3-prefix) (($# >= 2)) || { usage; exit 2; }; S3_PREFIX="$2"; shift 2 ;;
    --s3-region) (($# >= 2)) || { usage; exit 2; }; S3_REGION="$2"; shift 2 ;;
    --email) (($# >= 2)) || { usage; exit 2; }; EMAIL="$2"; shift 2 ;;
    --mode) (($# >= 2)) || { usage; exit 2; }; INSTALL_MODE="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) printf 'Unknown option: %s\n' "$1" >&2; usage; exit 2 ;;
  esac
done

if [[ "$(id -u)" -ne 0 ]]; then
  printf 'Run this installer as root (for example: sudo bash node-install.sh ...).\n' >&2
  exit 1
fi
if [[ -z "$INSTALL_MODE" ]]; then
  if [[ -s /var/lib/boxes/baked.json ]]; then INSTALL_MODE=FAST; else INSTALL_MODE=FULL; fi
fi
INSTALL_MODE="${INSTALL_MODE^^}"
if [[ "$INSTALL_MODE" != FULL && "$INSTALL_MODE" != FAST && "$INSTALL_MODE" != BAKE ]]; then
  printf 'Install mode must be FULL, FAST, or BAKE.\n' >&2; exit 2
fi
if [[ "$INSTALL_MODE" != BAKE && ( ! "$SERVER" =~ ^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?$ && ! "$SERVER" =~ ^wss?://[A-Za-z0-9.-]+(:[0-9]{1,5})?/api/v1/agent/connect$ || ! "$TOKEN" =~ ^[A-Za-z0-9._-]+$ ) ]]; then
  usage
  printf 'A valid --server/BOXES_SERVER_URL and --token/BOXES_NODE_TOKEN are required.\n' >&2
  exit 2
fi
if [[ "$SERVER" =~ ^https?:// ]]; then
  SERVER_ORIGIN="${SERVER%/}"
  if [[ "$SERVER_ORIGIN" == https://* ]]; then TUNNEL_SCHEME=wss; else TUNNEL_SCHEME=ws; fi
  TUNNEL_URL="${TUNNEL_SCHEME}://${SERVER_ORIGIN#*://}/api/v1/agent/connect"
else
  TUNNEL_URL="$SERVER"
  if [[ "$TUNNEL_URL" == wss://* ]]; then SERVER_SCHEME=https; else SERVER_SCHEME=http; fi
  SERVER_AUTHORITY="${TUNNEL_URL#*://}"
  SERVER_AUTHORITY="${SERVER_AUTHORITY%/api/v1/agent/connect}"
  SERVER_ORIGIN="${SERVER_SCHEME}://${SERVER_AUTHORITY}"
fi
if [[ "$RUNTIME" != sysbox-runc && "$RUNTIME" != runc ]]; then
  printf 'Runtime must be sysbox-runc or runc.\n' >&2
  exit 2
fi
if [[ "$TOOLS_IMAGE" != "none" && ! "$TOOLS_IMAGE" =~ ^[A-Za-z0-9._/:@-]+$ ]]; then
  printf 'The tools image must be a valid container image reference or none.\n' >&2
  exit 2
fi
if [[ -n "$REGISTRY_URL$REGISTRY_USERNAME$REGISTRY_PASSWORD" ]]; then
  if [[ ! "$REGISTRY_URL" =~ ^[A-Za-z0-9.-]+(:[0-9]{1,5})?$ || ! "$REGISTRY_USERNAME" =~ ^[A-Za-z0-9._-]+$ || ! "$REGISTRY_PASSWORD" =~ ^[A-Za-z0-9._-]+$ ]]; then
    printf 'BOXES_REGISTRY_URL, BOXES_REGISTRY_USERNAME, and BOXES_REGISTRY_PASSWORD must contain valid registry credentials.\n' >&2
    exit 2
  fi
fi
if [[ "$INSTALL_MODE" == BAKE ]] && [[ -z "$PUBLIC_URL" || -z "${BOXES_BAKE_REPORT_URL:-}" || -z "${BOXES_BAKE_TOKEN:-}" || -z "${BOXES_BOX_IMAGE:-}" ]]; then
  printf 'BAKE requires BOXES_PUBLIC_URL, BOXES_BAKE_REPORT_URL, BOXES_BAKE_TOKEN, and BOXES_BOX_IMAGE.\n' >&2; exit 2
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
if [[ -z "$S3_ENDPOINT" && -z "$S3_BUCKET" && -n "$S3_REGION" ]]; then
  printf 'S3 region requires --s3-endpoint and --s3-bucket.\n' >&2
  exit 2
fi
if [[ ! "$EMAIL" =~ ^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$ || ! "$RESTIC_REPOSITORY" =~ ^(/[A-Za-z0-9._/-]+|s3:[A-Za-z0-9._/:@-]+)$ || ! "$RESTIC_PASSWORD_VALUE" =~ ^[A-Za-z0-9._-]*$ ]]; then
  printf 'Invalid email, restic repository, or restic password.\n' >&2
  exit 2
fi

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) printf 'Unsupported architecture: %s\n' "$(uname -m)" >&2; exit 1 ;;
esac
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

if [[ "$INSTALL_MODE" != FAST ]] && [[ -e /proc/sys/kernel/apparmor_restrict_unprivileged_userns ]]; then
  cat > /etc/sysctl.d/99-boxes.conf <<'SYSCTL'
# Boxes are the only workload on this host; Sysbox uses user namespaces itself,
# and this restriction prevents bwrap inside Boxes from creating its uid map.
kernel.apparmor_restrict_unprivileged_userns = 0
SYSCTL
  sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
  printf 'Boxes: relaxed the host AppArmor user-namespace restriction for Sysbox workloads.\n'
fi

export DEBIAN_FRONTEND=noninteractive
if [[ "$INSTALL_MODE" != FAST ]] || ! command -v docker >/dev/null 2>&1 || ! dpkg-query -W -f='${Status}' restic 2>/dev/null | grep -q 'install ok installed'; then
apt-get -o DPkg::Lock::Timeout=600 update
apt-get -o DPkg::Lock::Timeout=600 install -y ca-certificates curl gnupg jq openssl procps restic
fi
record_phase packages
if [[ "$INSTALL_MODE" != FAST ]]; then configure_swap; fi
if [[ "$INSTALL_MODE" != FAST ]] || ! command -v docker >/dev/null 2>&1; then
install -m 0755 -d /etc/apt/keyrings
curl -fsSL "https://download.docker.com/linux/${ID}/gpg" -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc
printf 'deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/%s %s stable\n' \
  "$(dpkg --print-architecture)" "$ID" "$VERSION_CODENAME" \
  > /etc/apt/sources.list.d/docker.list
apt-get -o DPkg::Lock::Timeout=600 update
apt-get -o DPkg::Lock::Timeout=600 install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
systemctl enable --now docker
fi
record_phase docker

if [[ "$INSTALL_MODE" != FAST ]] || ! command -v sysbox-runc >/dev/null 2>&1; then
sysbox_deb="$(mktemp --suffix=.deb)"
curl -fsSL "https://github.com/nestybox/sysbox/releases/download/v${SYSBOX_VERSION}/sysbox-ce_${SYSBOX_VERSION}.linux_${ARCH}.deb" -o "$sysbox_deb"
apt-get -o DPkg::Lock::Timeout=600 install -y "$sysbox_deb"
rm -f "$sysbox_deb"
systemctl restart docker
fi
record_phase sysbox
docker network inspect boxes-net >/dev/null 2>&1 || docker network create --driver bridge --opt com.docker.network.bridge.enable_icc=false boxes-net >/dev/null
if [[ -n "$REGISTRY_URL$REGISTRY_USERNAME$REGISTRY_PASSWORD" ]]; then
  printf '%s\n' "$REGISTRY_PASSWORD" | docker login "$REGISTRY_URL" \
    --username "$REGISTRY_USERNAME" --password-stdin
fi
record_phase registry_login
record_phase host_settings

if [[ "$INSTALL_MODE" != FAST ]]; then
install -d -m 0755 /etc/boxes/traefik/dynamic
if [[ ! -e /etc/boxes/traefik/acme.json ]]; then
  install -m 0600 /dev/null /etc/boxes/traefik/acme.json
fi
script_path="${BASH_SOURCE[0]:-}"
script_dir=""
if [[ -n "$script_path" && -f "$script_path" ]]; then
  script_dir="$(cd -- "$(dirname -- "$script_path")" && pwd)"
fi
if [[ -n "$script_dir" && -r "${script_dir}/traefik.yml" ]]; then
  sed "s|__ACME_EMAIL__|${EMAIL}|g" "${script_dir}/traefik.yml" > /etc/boxes/traefik/traefik.yml
else
  CONFIG_BASE_URL="${PUBLIC_URL:-$SERVER_ORIGIN}"
  CONFIG_BASE_URL="${CONFIG_BASE_URL%/}"
  curl -fsSL "${CONFIG_BASE_URL}/install/traefik.yml" \
    | sed "s|__ACME_EMAIL__|${EMAIL}|g" > /etc/boxes/traefik/traefik.yml
fi
chmod 0644 /etc/boxes/traefik/traefik.yml
docker pull traefik:v3.3
fi
record_phase image_pulls
docker rm -f boxes-traefik >/dev/null 2>&1 || true
docker run -d --name boxes-traefik --restart unless-stopped \
  --network host \
  -v /etc/boxes/traefik:/etc/boxes/traefik \
  traefik:v3.3 --configFile=/etc/boxes/traefik/traefik.yml
record_phase traefik

if [[ -z "$RESTIC_PASSWORD_VALUE" ]]; then
  if [[ -s /etc/boxes/restic-password ]]; then
    RESTIC_PASSWORD_VALUE="$(< /etc/boxes/restic-password)"
  else
    RESTIC_PASSWORD_VALUE="$(openssl rand -hex 32)"
  fi
fi
install -d -m 0750 /etc/boxes
if [[ "$RESTIC_REPOSITORY" == /* ]]; then
  install -d -m 0750 "$(dirname "$RESTIC_REPOSITORY")" "$RESTIC_REPOSITORY"
fi
printf '%s\n' "$RESTIC_PASSWORD_VALUE" > /etc/boxes/restic-password
chmod 0600 /etc/boxes/restic-password
if [[ "$INSTALL_MODE" != BAKE ]]; then
cat > /etc/boxes/agent.env <<EOF
BOXES_SERVER_URL=${TUNNEL_URL}
BOXES_NODE_TOKEN=${TOKEN}
BOXES_RUNTIME=${RUNTIME}
BOXES_TOOLS_IMAGE=${TOOLS_IMAGE}
BOXES_TRAEFIK_DIR=/etc/boxes/traefik/dynamic
RESTIC_REPOSITORY=${RESTIC_REPOSITORY}
RESTIC_PASSWORD=${RESTIC_PASSWORD_VALUE}
AWS_ACCESS_KEY_ID=${BOXES_S3_ACCESS_KEY:-}
AWS_SECRET_ACCESS_KEY=${BOXES_S3_SECRET_KEY:-}
AWS_DEFAULT_REGION=${S3_REGION}
EOF
chmod 0600 /etc/boxes/agent.env
fi
install -d -m 0755 /usr/local/bin

if [[ -n "${BOXES_BINARIES_DIR:-}" ]]; then
  if [[ ! -f "${BOXES_BINARIES_DIR}/boxes-agent" ]]; then
    printf 'Local binary not found: %s/boxes-agent (BOXES_BINARIES_DIR is set).\n' "$BOXES_BINARIES_DIR" >&2
    exit 1
  fi
  agent_source="${BOXES_BINARIES_DIR}/boxes-agent"
elif [[ -n "$PUBLIC_URL" ]]; then
  agent_tmp="$(mktemp -d)"
  agent_source="${agent_tmp}/boxes-agent"
  curl -fsSL "${PUBLIC_URL%/}/install/agent/boxes-agent-linux-${ARCH}" -o "$agent_source"
  chmod 0755 "$agent_source"
else
  tag="$(curl -fsSL -H 'Accept: application/vnd.github+json' "https://api.github.com/repos/${REPO}/releases/latest" | jq -er '.tag_name')"
  version="${tag#v}"
  agent_tmp="$(mktemp -d)"
  archive="${agent_tmp}/boxes_${version}_linux_${ARCH}.tar.gz"
  curl -fsSL "https://github.com/${REPO}/releases/download/${tag}/boxes_${version}_linux_${ARCH}.tar.gz" -o "$archive"
  tar -xzf "$archive" -C "$agent_tmp" boxes-agent
  agent_source="${agent_tmp}/boxes-agent"
fi
record_phase agent_download
install -m 0755 "$agent_source" /usr/local/bin/boxes-agent
if [[ ! -s /usr/local/bin/boxes-agent || ! -x /usr/local/bin/boxes-agent ]] || ! /usr/local/bin/boxes-agent --help >/dev/null 2>&1; then
  printf 'Installed boxes-agent is empty or cannot execute on this node.\n' >&2
  exit 1
fi
if [[ -n "${agent_tmp:-}" ]]; then
  rm -rf "$agent_tmp"
fi

cat > /etc/systemd/system/boxes-agent.service <<'UNIT'
[Unit]
Description=Boxes node agent
After=network-online.target docker.service
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
if [[ "$INSTALL_MODE" == BAKE ]]; then
  box_ref="$BOXES_BOX_IMAGE"
  docker pull "$box_ref"
  box_digest="$(docker image inspect --format '{{index .RepoDigests 0}}' "$box_ref")"
  tools_digest=""
  if [[ "$TOOLS_IMAGE" != none ]]; then
    docker pull "$TOOLS_IMAGE"
    tools_digest="$(docker image inspect --format '{{index .RepoDigests 0}}' "$TOOLS_IMAGE")"
    # Populate the same digest-keyed volume used by boxes-agent.EnsureToolsVolume.
    tools_id="$(docker image inspect --format '{{.Id}}' "$TOOLS_IMAGE")"
    tools_volume="boxes-tools-$(printf '%s' "${tools_id#sha256:}" | cut -c1-12)"
    if ! docker volume inspect "$tools_volume" >/dev/null 2>&1; then
      docker volume create "$tools_volume" >/dev/null
      docker run --rm -v "$tools_volume:/dst" "$TOOLS_IMAGE" cp -a /opt/boxes-tools/. /dst/
    fi
  fi
  [[ "$box_digest" == *@sha256:* ]] || { echo 'Unable to resolve box image repo digest' >&2; exit 1; }
  if [[ "$TOOLS_IMAGE" != none ]]; then [[ "$tools_digest" == *@sha256:* ]] || { echo 'Unable to resolve tools image repo digest' >&2; exit 1; }; fi
  jq -n --arg boxImage "$box_ref" --arg boxDigest "$box_digest" --arg toolsImage "$TOOLS_IMAGE" --arg toolsDigest "$tools_digest" --arg bakedAt "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '{boxImage:$boxImage,boxDigest:$boxDigest,toolsImage:$toolsImage,toolsDigest:$toolsDigest,bakedAt:$bakedAt}' > /var/lib/boxes/baked.json
  curl -fsS -X POST -H "Authorization: Bearer ${BOXES_BAKE_TOKEN}" -H 'Content-Type: application/json' --data "$(jq -c '{boxDigest,toolsDigest}' /var/lib/boxes/baked.json)" "$BOXES_BAKE_REPORT_URL" >/dev/null
  docker logout "${REGISTRY_URL:-}" >/dev/null 2>&1 || true
  rm -f /etc/boxes/*.env /etc/boxes/*token* /etc/boxes/restic-password /root/.docker/config.json
  rm -f /etc/ssh/ssh_host_*
  cloud-init clean --logs
  truncate -s 0 /etc/machine-id
  rm -f /var/lib/dbus/machine-id
  systemctl poweroff
  exit 0
fi
if [[ "$INSTALL_MODE" == FAST ]]; then
  ssh_keys_missing=0
  compgen -G '/etc/ssh/ssh_host_*_key' >/dev/null || ssh_keys_missing=1
  if ((ssh_keys_missing)); then ssh-keygen -A; fi
  baked_box="$(jq -er '.boxDigest' /var/lib/boxes/baked.json)"
  baked_tools="$(jq -r '.toolsDigest // ""' /var/lib/boxes/baked.json)"
  expected_box="${BOXES_BOX_DIGEST:-$baked_box}"
  expected_tools="${BOXES_TOOLS_DIGEST:-$baked_tools}"
  actual_box="$(docker image inspect --format '{{index .RepoDigests 0}}' "$BOXES_BOX_IMAGE" 2>/dev/null || true)"
  if [[ "$actual_box" != "$expected_box" ]]; then docker pull "$BOXES_BOX_IMAGE"; fi
  if [[ "$TOOLS_IMAGE" != none ]]; then
    actual_tools="$(docker image inspect --format '{{index .RepoDigests 0}}' "$TOOLS_IMAGE" 2>/dev/null || true)"
    if [[ "$actual_tools" != "$expected_tools" ]]; then docker pull "$TOOLS_IMAGE"; fi
    tools_id="$(docker image inspect --format '{{.Id}}' "$TOOLS_IMAGE")"
    volume="boxes-tools-$(printf '%s' "${tools_id#sha256:}" | cut -c1-12)"
    if ! docker volume inspect "$volume" >/dev/null 2>&1; then docker volume create "$volume" >/dev/null; docker run --rm -v "$volume:/dst" "$TOOLS_IMAGE" cp -a /opt/boxes-tools/. /dst/; fi
  fi
fi
systemctl daemon-reload
systemctl enable --now boxes-agent.service
PHASE_SECONDS[total]=$((SECONDS - INSTALL_STARTED))
record_phase agent_start

printf 'Boxes node connected to %s\n' "$SERVER_ORIGIN"
