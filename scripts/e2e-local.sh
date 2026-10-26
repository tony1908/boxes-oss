#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/e2e-lib.sh"
e2e_init

box_id="$(e2e_create_box)"
BOX_IDS+=("$box_id")
e2e_wait_box_ready "$box_id"
printf 'sandbox ready: %s\n' "$box_id"

command_response="$(curl -fsS -X POST "$API/sandboxes/$box_id/commands" -H "$AUTH_HEADER" \
	-H 'Content-Type: application/json' -d '{"cmd":["echo","hi"]}')"
[[ "$(jq -er '.stdout | rtrimstr("\n")' <<<"$command_response")" == hi ]]

printf 'snapshot survives stop and resume\n' | curl -fsS -X PUT \
	"$API/sandboxes/$box_id/files?path=%2Fconfig%2Fe2e.txt&mode=0644" \
	-H "$AUTH_HEADER" -H 'Content-Type: application/octet-stream' --data-binary @- >/dev/null

command_response="$(curl -fsS -X POST "$API/sandboxes/$box_id/commands" -H "$AUTH_HEADER" \
	-H 'Content-Type: application/json' \
	-d '{"cmd":["sh","-lc","sudo apt-get update && sudo apt-get install -y --no-install-recommends hello && sudo mkdir -p /opt/e2e && sudo touch /opt/e2e/pkg && sudo rm -f /etc/issue"]}')"
[[ "$(jq -er '.exitCode' <<<"$command_response")" == 0 ]]
printf 'sandbox command and file setup passed\n'

curl -fsS -X POST "$API/sandboxes/$box_id/stop" -H "$AUTH_HEADER" \
	>/dev/null
e2e_wait_box_state "$box_id" stopped
curl -fsS -X POST "$API/sandboxes/$box_id/resume" -H "$AUTH_HEADER" \
	-H 'Content-Type: application/json' -d '{}' >/dev/null
e2e_wait_box_ready "$box_id"
printf 'sandbox stop and resume passed\n'

file_content="$(curl -fsS "$API/sandboxes/$box_id/files?path=%2Fconfig%2Fe2e.txt" -H "$AUTH_HEADER")"
[[ "$file_content" == 'snapshot survives stop and resume' ]]
command_response="$(curl -fsS -X POST "$API/sandboxes/$box_id/commands" -H "$AUTH_HEADER" \
	-H 'Content-Type: application/json' -d '{"cmd":["sh","-lc","test -f /opt/e2e/pkg && command -v hello >/dev/null && test ! -e /etc/issue"]}')"
[[ "$(jq -er '.exitCode' <<<"$command_response")" == 0 ]]

fork_response="$(curl -fsS -X POST "$API/sandboxes/$box_id/fork" -H "$AUTH_HEADER" \
	-H 'Content-Type: application/json' -d '{"type":"small","ttlSeconds":0}')"
fork_id="$(jq -er '.sandbox.id' <<<"$fork_response")"
BOX_IDS+=("$fork_id")
e2e_wait_box_ready "$fork_id"
fork_file="$(curl -fsS "$API/sandboxes/$fork_id/files?path=%2Fconfig%2Fe2e.txt" -H "$AUTH_HEADER")"
[[ "$fork_file" == 'snapshot survives stop and resume' ]]

docker run -d --name "$TRAEFIK_CONTAINER" --network paddock-net --network-alias paddock-e2e-traefik \
	-e GOMEMLIMIT=64MiB -e GOGC=20 -p 18443:443 -p 18000:80 \
	-v "$TRAEFIK_VOLUME:/etc/traefik/dynamic" traefik:v3.3 \
	--entrypoints.web.address=:80 --entrypoints.websecure.address=:443 \
	--certificatesresolvers.letsencrypt.acme.email=e2e@example.test \
	--certificatesresolvers.letsencrypt.acme.storage=/tmp/acme.json \
	--certificatesresolvers.letsencrypt.acme.httpchallenge.entrypoint=web \
	--providers.file.directory=/etc/traefik/dynamic --providers.file.watch=true \
	--api.insecure=false >/dev/null

desktop_result="$(curl -sS -w $'\n%{http_code}' -X POST "$API/sandboxes/$box_id/desktop" -H "$AUTH_HEADER" \
	-H 'Content-Type: application/json' -d '{}')"
desktop_status="${desktop_result##*$'\n'}"
desktop_response="${desktop_result%$'\n'*}"
if [[ "$desktop_status" != 200 ]]; then printf 'desktop route creation failed (HTTP %s): %s\n' "$desktop_status" "$desktop_response" >&2; exit 1; fi
desktop_host="$(jq -er '.url | capture("^https://(?<host>[^/]+)").host' <<<"$desktop_response")"
desktop_query="$(jq -er '.url | split("?")[1]' <<<"$desktop_response")"
: >"$COOKIE_FILE"
desktop_status=""
desktop_deadline=$((SECONDS + 90))
while (( SECONDS < desktop_deadline )); do
	desktop_status="$(curl -k -L --connect-timeout 2 --max-time 8 -sS -c "$COOKIE_FILE" -b "$COOKIE_FILE" -o /dev/null -w '%{http_code}' \
		--resolve "$desktop_host:18443:127.0.0.1" "https://$desktop_host:18443/?$desktop_query" 2>/dev/null || true)"
	[[ "$desktop_status" == 200 ]] && break
	sleep 1
done
[[ "$desktop_status" == 200 ]]
unauthenticated_status="$(curl -k -sS -o /dev/null -w '%{http_code}' --resolve "$desktop_host:18443:127.0.0.1" "https://$desktop_host:18443/")"
[[ "$unauthenticated_status" == 401 ]]
curl -fsS "$API/sandboxes/$box_id/conversations" -H "$AUTH_HEADER" >/dev/null

for id in "${BOX_IDS[@]}"; do curl -fsS -X DELETE "$API/sandboxes/$id" -H "$AUTH_HEADER" >/dev/null; done
BOX_IDS=()
printf 'E2E PASS\n'
