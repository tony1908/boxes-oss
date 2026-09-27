#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 ]]; then
    printf 'usage: %s <image> [tools-volume]\n' "$0" >&2
    exit 2
fi

image=$1
tools_volume=${2:-}
name="boxes-measure-${USER:-root}-$$-${RANDOM}"
container_started=false
tmpdir=$(mktemp -d)
checks=(healthz selkies scrot sudo docker browser codex opencode claude no_aborted xvfb_sockets)
declare -A result=()

cleanup() {
    if [[ "$container_started" == true ]]; then
        docker rm -f "$name" >/dev/null 2>&1 || true
    fi
    rm -rf "$tmpdir"
}
trap cleanup EXIT

check() {
    local key=$1
    shift
    if "$@" >"$tmpdir/$key.out" 2>&1; then
        result[$key]=true
        printf '%-14s PASS\n' "$key" >&2
    else
        result[$key]=false
        printf '%-14s FAIL\n' "$key" >&2
        if [[ -s "$tmpdir/$key.out" ]]; then
            tr '\n' ' ' <"$tmpdir/$key.out" >&2
            printf '\n' >&2
        fi
    fi
}

image_size=$(docker image inspect --format '{{.Size}}' "$image")
start_time=$(date +%s)
run_args=(run -d --name "$name" --runtime sysbox-runc -e PASSWORD=x -e BOXD_TOKEN=t -e PUID=1000 -e PGID=1000 --memory 4g --shm-size 1g)
if [[ -n "$tools_volume" ]]; then
    run_args+=(-v "${tools_volume}:/opt/boxes-tools:ro")
fi
run_args+=("$image")
docker "${run_args[@]}" >/dev/null
container_started=true

ready=false
boot_seconds=null
for _ in {1..180}; do
    if docker exec "$name" curl -fsS http://127.0.0.1:7070/healthz >/dev/null 2>&1; then
        ready=true
        boot_seconds=$(( $(date +%s) - start_time ))
        break
    fi
    sleep 1
done
result[healthz]=$ready
if [[ "$ready" == true ]]; then
    printf '%-14s PASS (%ss)\n' healthz "$boot_seconds" >&2
    sleep 120
    idle_rss_kb=$(docker exec "$name" ps -e -o rss= | awk '{ total += $1 } END { printf "%.0f", total }')
else
    printf '%-14s FAIL (timeout)\n' healthz >&2
    idle_rss_kb=null
fi

check selkies docker exec -u 1000:1000 -e HOME=/config "$name" bash -lc \
    'test "$(curl -sS -o /dev/null -w "%{http_code}" -u abc:x http://127.0.0.1:3000/)" = 200'
check scrot docker exec -u 1000:1000 -e HOME=/config "$name" bash -lc \
    'DISPLAY=:1 scrot /tmp/boxes-screen.png && test "$(od -An -tx1 -N8 /tmp/boxes-screen.png | tr -d " \n")" = 89504e470d0a1a0a'
check sudo docker exec -u 1000:1000 -e HOME=/config "$name" sudo -n true
check docker docker exec -u 1000:1000 -e HOME=/config "$name" bash -lc 'docker run --rm hello-world'

check browser docker exec -u 1000:1000 -e HOME=/config "$name" bash -lc '
    if command -v google-chrome >/dev/null 2>&1; then
        google-chrome --headless --no-sandbox --disable-gpu --disable-dev-shm-usage --no-zygote \
            --user-data-dir=/tmp/boxes-chrome-profile https://example.com >/tmp/boxes-browser.log 2>&1 &
    elif command -v firefox >/dev/null 2>&1; then
        firefox --headless --new-instance https://example.com >/tmp/boxes-browser.log 2>&1 &
    else
        echo "no supported browser executable" >&2
        exit 1
    fi
    browser_pid=$!
    sleep 5
    if ! kill -0 "$browser_pid" 2>/dev/null; then
        wait "$browser_pid"
        exit 1
    fi
    kill "$browser_pid" 2>/dev/null || true
    wait "$browser_pid" 2>/dev/null || true
'
check codex docker exec -u 1000:1000 -e HOME=/config "$name" bash -lc 'codex --version'
check opencode docker exec -u 1000:1000 -e HOME=/config "$name" bash -lc 'opencode --version'
check claude docker exec -u 1000:1000 -e HOME=/config "$name" bash -lc 'claude --version'
check no_aborted bash -c '! docker logs "$1" 2>&1 | grep -Fq Aborted' _ "$name"
socket_count=$(docker exec "$name" bash -lc 'find /tmp/.X11-unix -maxdepth 1 -type s | wc -l' | tr -d ' ')
if [[ "$socket_count" =~ ^[0-9]+$ ]] && (( socket_count < 40 )); then
    result[xvfb_sockets]=true
    printf '%-14s PASS (%s)\n' xvfb_sockets "$socket_count" >&2
else
    result[xvfb_sockets]=false
    printf '%-14s FAIL (%s)\n' xvfb_sockets "$socket_count" >&2
fi

passed=true
for key in "${checks[@]}"; do
    [[ "${result[$key]}" == true ]] || passed=false
done

printf '{"image":"%s","image_size_bytes":%s,"idle_rss_kb":%s,"boot_seconds":%s,"checks":{' "$image" "$image_size" "$idle_rss_kb" "$boot_seconds"
separator=
for key in "${checks[@]}"; do
    printf '%s"%s":%s' "$separator" "$key" "${result[$key]}"
    separator=,
done
printf '},"xvfb_socket_count":%s,"passed":%s}\n' "$socket_count" "$passed"
