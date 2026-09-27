#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 || ! $3 =~ ^[1-9][0-9]*$ ]]; then
    printf 'usage: %s <image> <tools-volume> <N>\n' "$0" >&2
    exit 2
fi

image=$1
tools_volume=$2
count=$3
prefix="boxes-density-${USER:-root}-$$-${RANDOM}"
names=()
exec_pids=()
tmpdir=$(mktemp -d)
host_used_mb=0
host_available_mb=0
peak_used_mb=0
min_available_mb=0
last_swap_pages=$(awk '$1 == "pswpin" {in_pages=$2} $1 == "pswpout" {out_pages=$2} END {print in_pages+out_pages}' /proc/vmstat)
swap_activity_seconds=0

cleanup() {
    if ((${#names[@]})); then
        docker rm -f "${names[@]}" >/dev/null 2>&1 || true
    fi
    rm -rf "$tmpdir"
}
trap cleanup EXIT

sample_memory() {
    read -r host_used_mb host_available_mb < <(
        free -m | awk '$1 == "Mem:" {used=$3; available=$7} END {print used+0, available+0}'
    )
}

check_safety() {
    local swap_pages
    sample_memory
    if ((host_available_mb < 600)); then
        printf 'stopping: host available memory is %s MB (< 600 MB)\n' "$host_available_mb" >&2
        exit 3
    fi

    swap_pages=$(awk '$1 == "pswpin" {in_pages=$2} $1 == "pswpout" {out_pages=$2} END {print in_pages+out_pages}' /proc/vmstat)
    if ((swap_pages > last_swap_pages)); then
        ((swap_activity_seconds += 1))
    else
        swap_activity_seconds=0
    fi
    last_swap_pages=$swap_pages
    if ((swap_activity_seconds >= 2)); then
        printf 'stopping: sustained swap activity detected\n' >&2
        exit 3
    fi
}

wait_for_ready() {
    local name=$1
    for _ in {1..180}; do
        check_safety
        if docker exec "$name" curl -fsS http://127.0.0.1:7070/healthz >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    printf 'box did not become ready: %s\n' "$name" >&2
    return 1
}

for ((i = 1; i <= count; i++)); do
    name="${prefix}-${i}"
    names+=("$name")
    printf 'starting box %d/%d (%s)\n' "$i" "$count" "$name" >&2
    docker run -d --name "$name" --runtime sysbox-runc \
        -e PASSWORD=x -e BOXD_TOKEN=t -e PUID=1000 -e PGID=1000 \
        --memory 2g --memory-reservation 512m --shm-size 1g \
        -v "${tools_volume}:/opt/boxes-tools:ro" "$image" >/dev/null
    wait_for_ready "$name"
    printf 'ready %s\n' "$name" >&2
done

printf 'all boxes ready; observing 120 seconds of idle\n' >&2
for _ in {1..120}; do
    check_safety
    sleep 1
done

sample_memory
idle_used_mb=$host_used_mb
idle_available_mb=$host_available_mb
min_available_mb=$host_available_mb
peak_used_mb=$host_used_mb
load_average=$(awk '{print $1 "," $2 "," $3}' /proc/loadavg)
box_rss=()
for name in "${names[@]}"; do
    rss_kb=$(docker exec "$name" ps -e -o rss= | awk '{total += $1} END {printf "%.0f", total}')
    box_rss+=("$rss_kb")
done

printf 'starting concurrent 30-second agent workload in all boxes\n' >&2
for name in "${names[@]}"; do
    docker exec -u 1000:1000 -e HOME=/config "$name" bash -lc '
        node -e '\''const until=Date.now()+30000; while(Date.now()<until) {}'\'' &
        busy_pid=$!
        if command -v google-chrome >/dev/null 2>&1; then
            browser=google-chrome
        elif command -v firefox >/dev/null 2>&1; then
            browser=firefox
        else
            echo "no supported browser executable" >&2
            exit 1
        fi
        timeout 30s "$browser" --headless --no-sandbox --disable-gpu --disable-dev-shm-usage \
            --no-zygote --user-data-dir="/tmp/boxes-density-browser-$$" https://example.com \
            >/tmp/boxes-density-browser.log 2>&1 &
        browser_pid=$!
        wait "$busy_pid"
        wait "$browser_pid" || status=$?
        [[ ${status:-0} -eq 0 || ${status:-0} -eq 124 ]]
    ' >/"$tmpdir/${name}.load.log" 2>&1 &
    exec_pids+=("$!")
done

load_failed=false
for second in {1..30}; do
    check_safety
    sample_memory
    ((host_used_mb > peak_used_mb)) && peak_used_mb=$host_used_mb
    if ((host_available_mb < min_available_mb)); then
        min_available_mb=$host_available_mb
    fi
    sleep 1
done
for pid in "${exec_pids[@]}"; do
    if ! wait "$pid"; then
        load_failed=true
    fi
done

printf '{"image":"%s","tools_volume":"%s","n":%s,"idle_used_mb":%s,"idle_available_mb":%s,"load_average":"%s","box_rss_kb":[' \
    "$image" "$tools_volume" "$count" "$idle_used_mb" "$idle_available_mb" "$load_average"
separator=
for rss_kb in "${box_rss[@]}"; do
    printf '%s%s' "$separator" "$rss_kb"
    separator=,
done
printf '],"peak_used_mb":%s,"min_available_mb":%s,"load_passed":%s}\n' \
    "$peak_used_mb" "$min_available_mb" "$([[ $load_failed == false ]] && printf true || printf false)"

if [[ $load_failed == true ]]; then
    for name in "${names[@]}"; do
        if [[ -s "$tmpdir/${name}.load.log" ]]; then
            printf 'workload failed in %s: ' "$name" >&2
            tr '\n' ' ' <"$tmpdir/${name}.load.log" >&2
            printf '\n' >&2
        fi
    done
    exit 1
fi
