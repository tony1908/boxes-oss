#!/bin/sh
set -eu

SERVER_URL='__BOXES_PUBLIC_URL__'

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) printf 'Unsupported operating system: %s\n' "$(uname -s)" >&2; exit 1 ;;
esac

case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=amd64 ;;
  *) printf 'Unsupported architecture: %s\n' "$(uname -m)" >&2; exit 1 ;;
esac

binary="boxes-${os}-${arch}"
url="${SERVER_URL%/}/install/cli/${binary}"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT HUP INT TERM
if ! curl -fsSL "$url" -o "$tmp"; then
  printf 'Could not download %s from %s. Ask the server administrator to install CLI binaries.\n' "$binary" "$SERVER_URL" >&2
  exit 1
fi

if mkdir -p "$HOME/.local/bin" 2>/dev/null && [ -w "$HOME/.local/bin" ]; then
  destination="$HOME/.local/bin/boxes"
elif [ -w /usr/local/bin ]; then
  destination=/usr/local/bin/boxes
else
  printf 'Cannot write to ~/.local/bin or /usr/local/bin. Create ~/.local/bin and rerun this installer.\n' >&2
  exit 1
fi
install -m 0755 "$tmp" "$destination"
printf 'Installed boxes to %s\n' "$destination"
printf 'Next steps:\n  boxes login --server %s --key-stdin\n  boxes skill install\n' "$SERVER_URL"
