#!/usr/bin/env bash

e2e_init() {
	ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
	cd "$ROOT_DIR"
	for command in docker go jq curl; do
		command -v "$command" >/dev/null 2>&1 || { printf 'required command not found: %s\n' "$command" >&2; exit 1; }
	done

	RUN_ID="$(date +%s)-$$"
	E2E_DIR="$ROOT_DIR/.e2e"
	BIN_DIR="$E2E_DIR/bin-$RUN_ID"
	BIN_DIR_CONTEXT=".e2e/bin-$RUN_ID"
	SERVER_IMAGE="boxes/server:e2e-$RUN_ID"
	AGENT_IMAGE="boxes/agent:e2e-$RUN_ID"
	SERVER_CONTAINER="boxes-e2e-server-$RUN_ID"
	AGENT_CONTAINER="boxes-e2e-agent-$RUN_ID"
	TRAEFIK_CONTAINER="boxes-e2e-traefik-$RUN_ID"
	SERVER_VOLUME="boxes-e2e-server-$RUN_ID"
	RESTIC_VOLUME="boxes-e2e-restic-$RUN_ID"
	TRAEFIK_VOLUME="boxes-e2e-traefik-$RUN_ID"
	SERVER_URL="http://127.0.0.1:18080"
	API="$SERVER_URL/api/v1"
	RESPONSE_FILE="$E2E_DIR/response-$RUN_ID.json"
	COOKIE_FILE="$E2E_DIR/cookies-$RUN_ID.txt"
	E2E_DIR_CREATED=0
	BIN_DIR_CREATED=0
	BINARIES_CREATED=0
	NETWORK_CREATED=0
	SERVER_VOLUME_CREATED=0
	RESTIC_VOLUME_CREATED=0
	TRAEFIK_VOLUME_CREATED=0
	HOST_BIN_DIR_CREATED=0
	HOST_CLI_CREATED=0
	TEMP_HOME_CREATED=0
	INITIAL_MANAGED_CONTAINERS=$'\n'
	SESSION_TOKEN=""
	BOX_IDS=()

	trap e2e_cleanup EXIT
	trap 'status=$?; printf "E2E failed at line %s: %s\n" "$LINENO" "$BASH_COMMAND" >&2; exit "$status"' ERR

	if [[ ! -d "$E2E_DIR" ]]; then mkdir "$E2E_DIR"; E2E_DIR_CREATED=1; fi
	if [[ -e "$BIN_DIR" ]]; then printf 'refusing to overwrite pre-existing E2E directory %s\n' "$BIN_DIR" >&2; exit 1; fi
	mkdir "$BIN_DIR"
	BIN_DIR_CREATED=1
	BINARIES_CREATED=1
	INITIAL_MANAGED_CONTAINERS=$'\n'"$(docker ps -aq --filter label=boxes.managed=true)"$'\n'
	[[ -z "$(docker image inspect boxes/box:dev 2>/dev/null || true)" ]] && { printf 'required image boxes/box:dev not found; build it first\n' >&2; exit 1; }

	DOCKER_ARCH="$(docker info --format '{{.Architecture}}')"
	case "$DOCKER_ARCH" in
		arm64|aarch64) GOARCH=arm64 ;;
		amd64|x86_64) GOARCH=amd64 ;;
		*) printf 'unsupported Docker server architecture: %s\n' "$DOCKER_ARCH" >&2; exit 1 ;;
	esac
	CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -o "$BIN_DIR/boxes-server" ./cmd/boxes-server
	CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -o "$BIN_DIR/boxes-agent" ./cmd/boxes-agent
	DOCKER_BUILDKIT=0 docker build --build-arg BOXES_BIN_DIR="$BIN_DIR_CONTEXT" -f deploy/dev/Dockerfile.server.prebuilt -t "$SERVER_IMAGE" .
	DOCKER_BUILDKIT=0 docker build --build-arg BOXES_BIN_DIR="$BIN_DIR_CONTEXT" -f deploy/dev/Dockerfile.agent.prebuilt -t "$AGENT_IMAGE" .

	if ! docker network inspect boxes-net >/dev/null 2>&1; then docker network create --driver bridge boxes-net >/dev/null; NETWORK_CREATED=1; fi
	docker volume create "$SERVER_VOLUME" >/dev/null; SERVER_VOLUME_CREATED=1
	docker volume create "$RESTIC_VOLUME" >/dev/null; RESTIC_VOLUME_CREATED=1
	docker volume create "$TRAEFIK_VOLUME" >/dev/null; TRAEFIK_VOLUME_CREATED=1
	docker run -d --name "$SERVER_CONTAINER" --network boxes-net --network-alias boxes-e2e-server \
		-e GOMEMLIMIT=2MiB -e GOGC=1 -e GOMAXPROCS=1 -e GODEBUG=madvdontneed=1 \
		-e BOXES_STORAGE_RESTIC_PASSWORD=e2e \
		-p 18080:8080 -v "$SERVER_VOLUME:/var/lib/boxes" \
		"$SERVER_IMAGE" --addr :8080 --data-dir /var/lib/boxes \
			--public-url http://boxes-e2e-server:8080 --image boxes/box:dev --memory-overcommit 4 --snapshot-interval 1h >/dev/null
	e2e_wait_http "$SERVER_URL/" 60 'boxes-server'
	local signup_response node_response
	signup_response="$(curl -fsS -X POST "$API/auth/signup" -H 'Content-Type: application/json' -d '{"email":"e2e@example.test","password":"e2e-password-long"}')"
	SESSION_TOKEN="$(jq -er '.token // .sessionToken' <<<"$signup_response")"
	AUTH_HEADER="Authorization: Bearer $SESSION_TOKEN"
	node_response="$(curl -fsS -X POST "$API/nodes" -H "$AUTH_HEADER" -H 'Content-Type: application/json' -d '{"name":"local-e2e","hostBase":"boxes.localhost"}')"
	NODE_ID="$(jq -er '.node.id' <<<"$node_response")"
	NODE_TOKEN="$(jq -er '.joinToken' <<<"$node_response")"
	docker run -d --name "$AGENT_CONTAINER" --pid host --network boxes-net --network-alias boxes-e2e-agent \
		-e GOMEMLIMIT=2MiB -e GOGC=1 -e GOMAXPROCS=1 -e GODEBUG=madvdontneed=1 \
		-e BOXES_SERVER_URL=ws://boxes-e2e-server:8080/api/v1/agent/connect \
		-e BOXES_NODE_TOKEN="$NODE_TOKEN" -e BOXES_NODE_ID="$NODE_ID" \
		-e BOXES_RUNTIME=runc -e BOXES_FORWARD_AUTH_ADDR=0.0.0.0:7001 \
		-e BOXES_HOST_PROC=/proc \
		-e BOXES_FORWARD_AUTH_URL=http://boxes-e2e-agent:7001/auth/forward \
		-e BOXES_TRAEFIK_DIR=/etc/boxes/traefik/dynamic \
		-e RESTIC_REPOSITORY=/var/lib/boxes/restic -e RESTIC_PASSWORD=e2e \
		-v /var/run/docker.sock:/var/run/docker.sock \
		-v /var/lib/containerd:/var/lib/containerd:ro -v /var/lib/docker:/var/lib/docker:ro \
		-v "$RESTIC_VOLUME:/var/lib/boxes/restic" -v "$TRAEFIK_VOLUME:/etc/boxes/traefik/dynamic" \
		"$AGENT_IMAGE" >/dev/null
	e2e_wait_node
}

e2e_wait_http() {
	local url=$1 timeout=$2 description=$3 deadline
	deadline=$((SECONDS + timeout))
	while (( SECONDS < deadline )); do
		if curl --connect-timeout 2 --max-time 4 -sS -o /dev/null "$url"; then return 0; fi
		sleep 1
	done
	printf '%s did not become ready: %s\n' "$description" "$url" >&2
	return 1
}

e2e_wait_node() {
	local deadline=$((SECONDS + 150))
	while (( SECONDS < deadline )); do
		if curl --connect-timeout 2 --max-time 4 -fsS "$API/nodes" -H "$AUTH_HEADER" 2>/dev/null | jq -e --arg id "$NODE_ID" \
			'any(.nodes[]; .id == $id and .status == "healthy" and .cpus >= 2 and .memoryBytes >= 4294967296)' >/dev/null; then
			printf 'node online with small-box capacity\n'
			return 0
		fi
		sleep 1
	done
	printf 'node did not become healthy with small-box capacity\n' >&2
	return 1
}

e2e_create_box() {
	local result response status
	result="$(curl -sS -w $'\n%{http_code}' -X POST "$API/sandboxes" -H "$AUTH_HEADER" -H 'Content-Type: application/json' \
		-d "{\"type\":\"small\",\"ttlSeconds\":0,\"node\":\"$NODE_ID\"}")"
	status="${result##*$'\n'}"
	response="${result%$'\n'*}"
	if [[ "$status" != 201 ]]; then printf 'sandbox creation failed (HTTP %s): %s\n' "$status" "$response" >&2; return 1; fi
	jq -er '.id' <<<"$response"
}

e2e_wait_box_state() {
	local box_id=$1 wanted_state=$2 deadline=$((SECONDS + 150))
	while (( SECONDS < deadline )); do
		if curl --connect-timeout 2 --max-time 4 -fsS "$API/sandboxes/$box_id" -H "$AUTH_HEADER" 2>/dev/null | \
			jq -e --arg state "$wanted_state" '.sandbox.state == $state' >/dev/null; then return 0; fi
		sleep 1
	done
	printf 'sandbox %s did not reach state %s\n' "$box_id" "$wanted_state" >&2
	return 1
}

e2e_wait_box_ready() {
	local box_id=$1 code deadline
	e2e_wait_box_state "$box_id" ready
	deadline=$((SECONDS + 120))
	while (( SECONDS < deadline )); do
		code="$(curl --connect-timeout 2 --max-time 4 -sS -o "$RESPONSE_FILE" -w '%{http_code}' -H "$AUTH_HEADER" \
			"$API/sandboxes/$box_id/boxd/healthz" 2>/dev/null || true)"
		if [[ "$code" == 200 ]] && jq -e '.status == "ok" or .ok == true or .healthy == true' "$RESPONSE_FILE" >/dev/null 2>&1; then return 0; fi
		sleep 1
	done
	printf 'boxd healthz did not become healthy for %s (HTTP %s)\n' "$box_id" "${code:-none}" >&2
	return 1
}

e2e_cleanup() {
	local status=$?
	trap - EXIT
	if (( status != 0 )); then
		if [[ -n "${SESSION_TOKEN:-}" ]]; then
			printf '\n--- node status ---\n' >&2
			curl -fsS "$API/nodes" -H "Authorization: Bearer $SESSION_TOKEN" 2>/dev/null | jq '.nodes[] | {id, status, cpus, memoryBytes, freeMemoryBytes, diskFreeBytes}' >&2 || true
		fi
		for container in "${AGENT_CONTAINER:-}" "${SERVER_CONTAINER:-}" "${TRAEFIK_CONTAINER:-}"; do
			if [[ -n "$container" ]] && docker container inspect "$container" >/dev/null 2>&1; then
				printf '\n--- %s logs ---\n' "$container" >&2
				docker logs --tail 100 "$container" >&2 2>/dev/null || true
			fi
		done
	fi
	if [[ -n "${SESSION_TOKEN:-}" ]]; then
		for box_id in "${BOX_IDS[@]:-}"; do curl -sS -o /dev/null -X DELETE -H "Authorization: Bearer $SESSION_TOKEN" "$API/sandboxes/$box_id" || true; done
	fi
	for container in "${AGENT_CONTAINER:-}" "${TRAEFIK_CONTAINER:-}" "${SERVER_CONTAINER:-}"; do
		[[ -n "$container" ]] && docker rm -f "$container" >/dev/null 2>&1 || true
	done
	while IFS= read -r container_id; do
		[[ -n "$container_id" ]] || continue
		[[ "${INITIAL_MANAGED_CONTAINERS:-}" == *$'\n'"$container_id"$'\n'* ]] && continue
		docker rm -f "$container_id" >/dev/null 2>&1 || true
	done < <(docker ps -aq --filter label=boxes.managed=true 2>/dev/null || true)
	(( ${SERVER_VOLUME_CREATED:-0} )) && docker volume rm "$SERVER_VOLUME" >/dev/null 2>&1 || true
	(( ${RESTIC_VOLUME_CREATED:-0} )) && docker volume rm "$RESTIC_VOLUME" >/dev/null 2>&1 || true
	(( ${TRAEFIK_VOLUME_CREATED:-0} )) && docker volume rm "$TRAEFIK_VOLUME" >/dev/null 2>&1 || true
	(( ${NETWORK_CREATED:-0} )) && docker network rm boxes-net >/dev/null 2>&1 || true
	[[ -n "${SERVER_IMAGE:-}" ]] && docker image rm "$SERVER_IMAGE" "${AGENT_IMAGE:-}" >/dev/null 2>&1 || true
	[[ -n "${RESPONSE_FILE:-}" ]] && rm -f "$RESPONSE_FILE" "$COOKIE_FILE"
	if (( ${BINARIES_CREATED:-0} )); then rm -f "$BIN_DIR/boxes-server" "$BIN_DIR/boxes-agent"; fi
	if (( ${BIN_DIR_CREATED:-0} )); then rmdir "$BIN_DIR" 2>/dev/null || true; fi
	if (( ${HOST_CLI_CREATED:-0} )); then rm -f "$HOST_CLI"; fi
	if (( ${HOST_BIN_DIR_CREATED:-0} )); then rmdir "$HOST_BIN_DIR" 2>/dev/null || true; fi
	if (( ${TEMP_HOME_CREATED:-0} )); then rm -rf "$TEMP_HOME"; fi
	if (( ${E2E_DIR_CREATED:-0} )); then rmdir "$E2E_DIR" 2>/dev/null || true; fi
	exit "$status"
}
