#!/usr/bin/env bash
set -euo pipefail

suffix="$$"
box="boxes-spike-source-$suffix"
restored="boxes-spike-restored-$suffix"
repo="boxes-spike-repo-$suffix"
restic_image="restic/restic"
restic_password="boxes-snapshot-spike"
temp_dir="${TMPDIR:-/tmp}"
diff_file="$(mktemp "$temp_dir/boxes-spike-diff.XXXXXX")"
files_file="$(mktemp "$temp_dir/boxes-spike-files.XXXXXX")"
deletions_file="$(mktemp "$temp_dir/boxes-spike-deletions.XXXXXX")"

cleanup() {
	docker rm -f "$box" "$restored" >/dev/null 2>&1 || true
	docker volume rm "$repo" >/dev/null 2>&1 || true
	rm -f "$diff_file" "$files_file" "$deletions_file"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

docker volume create "$repo" >/dev/null

restic() {
	docker run --rm -i \
		-v "$repo:/repo" \
		-e RESTIC_REPOSITORY=/repo \
		-e RESTIC_PASSWORD="$restic_password" \
		"$restic_image" "$@"
}

restic init

docker run -d --runtime runc --name "$box" ubuntu:24.04 sleep infinity >/dev/null
docker exec "$box" bash -c 'apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y cowsay'
docker exec "$box" rm -f /etc/issue
docker exec "$box" bash -c 'printf "%s\n" "snapshot marker" > /root/marker && chown 1234:1234 /root/marker'

docker diff "$box" >"$diff_file"
: >"$deletions_file"
printf '%s\0' '/.boxes/deletions' >"$files_file"

is_excluded() {
	case "$1" in
		/proc|/proc/*|/sys|/sys/*|/dev|/dev/*|/run|/run/*|/tmp|/tmp/*|/var/tmp|/var/tmp/*|/var/lib/docker|/var/lib/docker/*|/var/cache/apt|/var/cache/apt/*|/config/.cache|/config/.cache/*|/.boxes/restored|/.boxes/restored/*)
			return 0
			;;
	esac
	return 1
}

while IFS= read -r change; do
	status="${change%% *}"
	path="${change#* }"
	[ -n "$path" ] || continue
	is_excluded "$path" && continue
	case "$status" in
		A|C)
			[ "$path" = "/.boxes/deletions" ] || printf '%s\0' "$path" >>"$files_file"
			;;
		D) printf '%s\0' "$path" >>"$deletions_file" ;;
	esac
done <"$diff_file"

docker exec "$box" mkdir -p /.boxes
docker exec -i "$box" bash -c 'cat > /.boxes/deletions' <"$deletions_file"

backup_output="$(docker exec -i "$box" tar --create --no-recursion --numeric-owner --xattrs --acls --file - --null --files-from - <"$files_file" | restic backup --stdin --stdin-filename rootfs.tar --tag "box=$box" --json)"
snapshot_id="$(printf '%s\n' "$backup_output" | sed -n 's/.*"snapshot_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | tail -n 1)"
if [ -z "$snapshot_id" ]; then
	printf '%s\n' "Could not read snapshot ID from restic backup output" >&2
	exit 1
fi

docker create --runtime runc --name "$restored" ubuntu:24.04 sleep infinity >/dev/null
# Restore before first start, like a real resume.
restic dump "$snapshot_id" rootfs.tar | docker cp -a - "$restored":/
docker start "$restored" >/dev/null

docker exec "$restored" bash -c 'while IFS= read -r -d "" path; do rm -rf -- "$path"; done < /.boxes/deletions && mv /.boxes/deletions /.boxes/restored'
docker stop "$restored" >/dev/null
docker start "$restored" >/dev/null

docker exec "$restored" test -x /usr/games/cowsay
if docker exec "$restored" test -e /etc/issue; then
	printf '%s\n' "/etc/issue was not deleted during restore" >&2
	exit 1
fi
marker="$(docker exec "$restored" cat /root/marker)"
marker_uid="$(docker exec "$restored" stat -c '%u' /root/marker)"
[ "$marker" = "snapshot marker" ]
[ "$marker_uid" = "1234" ]

printf '%s\n' 'SPIKE PASS'
