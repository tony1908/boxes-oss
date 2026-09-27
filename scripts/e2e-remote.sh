#!/usr/bin/env bash
set -euo pipefail
set +x
umask 077

CURRENT_STEP="validate inputs"
for command in curl jq go ssh ssh-keygen; do
	command -v "$command" >/dev/null 2>&1 || {
		printf 'required command not found: %s\n' "$command" >&2
		exit 1
	}
done

: "${BOXES_URL:?set BOXES_URL to the live deployment URL}"
: "${BOXES_EMAIL:?set BOXES_EMAIL to the deployment account email}"
if [[ -n "${BOXES_PASSWORD_FILE:-}" ]]; then
	[[ -r "$BOXES_PASSWORD_FILE" ]] || { printf 'BOXES_PASSWORD_FILE is not readable\n' >&2; exit 1; }
	BOXES_PASSWORD="$(<"$BOXES_PASSWORD_FILE")"
fi
: "${BOXES_PASSWORD:?set BOXES_PASSWORD or BOXES_PASSWORD_FILE}"
[[ "$BOXES_URL" == https://* ]] || { printf 'BOXES_URL must use HTTPS\n' >&2; exit 1; }
BOXES_URL="${BOXES_URL%/}"
if [[ -n "${BOXES_SSH_HOST:-}" && ! "$BOXES_SSH_HOST" =~ ^root@[A-Za-z0-9.-]+$ ]]; then
	printf 'BOXES_SSH_HOST must be root@<ip-or-hostname>\n' >&2
	exit 1
fi
if [[ -n "${E2E_OPENCODE_AUTH_FILE:-}" && ( ! -f "$E2E_OPENCODE_AUTH_FILE" || ! -s "$E2E_OPENCODE_AUTH_FILE" ) ]]; then
	printf 'E2E_OPENCODE_AUTH_FILE must name a non-empty file\n' >&2
	exit 1
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/boxes-e2e-remote.XXXXXX")"
mkdir -m 0700 "$TEMP_DIR/home" "$TEMP_DIR/bin"
mkdir -m 0700 "$TEMP_DIR/home/.config"
export HOME="$TEMP_DIR/home"
export XDG_CONFIG_HOME="$TEMP_DIR/home/.config"
export PATH="$TEMP_DIR/bin:$PATH"
export BOXES_PASSWORD
API="$BOXES_URL/api/v1"
SESSION_TOKEN=""
AUTH_HEADER=""
BOX_IDS=()

fail() {
	printf 'E2E REMOTE failed during %s: %s\n' "$CURRENT_STEP" "$1" >&2
	exit 1
}

on_error() {
	status=$?
	printf 'E2E REMOTE failed during %s (line %s)\n' "$CURRENT_STEP" "$LINENO" >&2
	exit "$status"
}

cleanup() {
	status=$?
	trap - EXIT ERR
	set +e
	if [[ -n "$SESSION_TOKEN" ]]; then
		for box_id in "${BOX_IDS[@]}"; do
			deleted=0
			for attempt in 1 2 3; do
				code="$(curl --connect-timeout 5 --max-time 20 -sS -o /dev/null -w '%{http_code}' \
					-X DELETE -H "Authorization: Bearer $SESSION_TOKEN" "$API/sandboxes/$box_id" 2>/dev/null || true)"
				if [[ "$code" == 200 || "$code" == 204 || "$code" == 404 ]]; then
					deleted=1
					break
				fi
				sleep 1
			done
			if [[ "$deleted" != 1 ]]; then
				printf 'cleanup could not delete sandbox %s (HTTP %s)\n' "$box_id" "${code:-none}" >&2
				[[ "$status" != 0 ]] || status=1
			fi
		done
	fi
	rm -rf "$TEMP_DIR"
	exit "$status"
}
trap on_error ERR
trap cleanup EXIT

cd "$ROOT_DIR"
CURRENT_STEP="build temporary boxes CLI"
go build -o "$TEMP_DIR/bin/boxes" ./cmd/boxes

CURRENT_STEP="login and check machine health"
login_status="$(BOXES_PASSWORD="$BOXES_PASSWORD" jq -cn --arg email "$BOXES_EMAIL" \
	'{email:$email,password:env.BOXES_PASSWORD}' | \
	curl --connect-timeout 5 --max-time 30 -sS -o "$TEMP_DIR/login.json" -w '%{http_code}' \
		-H 'Content-Type: application/json' --data-binary @- "$API/auth/login" || true)"
[[ "$login_status" == 200 ]] || fail "login returned HTTP ${login_status:-none}"
SESSION_TOKEN="$(jq -er '.token // .sessionToken' "$TEMP_DIR/login.json")" || fail "login response did not contain a session token"
AUTH_HEADER="Authorization: Bearer $SESSION_TOKEN"

machine_deadline=$((SECONDS + 120))
machine_healthy=0
while (( SECONDS < machine_deadline )); do
	if curl --connect-timeout 4 --max-time 10 -sS -H "$AUTH_HEADER" "$API/machine" \
		-o "$TEMP_DIR/machine.json" 2>/dev/null && jq -e '.machine.status == "healthy" and .machine.cpus > 0 and .machine.memoryBytes > 0' "$TEMP_DIR/machine.json" >/dev/null 2>&1; then
		machine_healthy=1
		break
	fi
	sleep 2
done
[[ "$machine_healthy" == 1 ]] || fail 'machine did not become healthy within 2 minutes'
printf 'login and healthy machine verified\n'

api_json() {
	local method="$1" path="$2" expected="$3" body="$4" max_time="${5:-60}" code
	code="$(curl --connect-timeout 5 --max-time "$max_time" -sS -o "$TEMP_DIR/api-response" -w '%{http_code}' \
		-X "$method" -H "$AUTH_HEADER" -H 'Content-Type: application/json' \
		--data-binary "$body" "$API$path" 2>/dev/null || true)"
	if [[ "$code" != "$expected" ]]; then
		api_error="$(jq -r 'if .error then ((.error.code // "api_error") + ": " + (.error.message // "request failed")) else "request failed" end' \
			"$TEMP_DIR/api-response" 2>/dev/null || printf 'request failed')"
		fail "$method $path returned HTTP ${code:-none}; expected $expected ($api_error)"
	fi
	API_BODY="$(<"$TEMP_DIR/api-response")"
}

api_no_body() {
	local method="$1" path="$2" expected="$3" code
	code="$(curl --connect-timeout 5 --max-time 60 -sS -o "$TEMP_DIR/api-response" -w '%{http_code}' \
		-X "$method" -H "$AUTH_HEADER" "$API$path" 2>/dev/null || true)"
	[[ "$code" == "$expected" ]] || fail "$method $path returned HTTP ${code:-none}; expected $expected"
	API_BODY="$(<"$TEMP_DIR/api-response")"
}

api_upload() {
	local path="$1" file="$2" expected="$3" code
	code="$(curl --connect-timeout 5 --max-time 60 -sS -o "$TEMP_DIR/api-response" -w '%{http_code}' \
		-X PUT -H "$AUTH_HEADER" -H 'Content-Type: application/octet-stream' \
		--data-binary "@$file" "$API$path" 2>/dev/null || true)"
	[[ "$code" == "$expected" ]] || fail "PUT $path returned HTTP ${code:-none}; expected $expected"
}

create_box() {
	local description="$1" ttl="${2:-0}" response id
	api_json POST /sandboxes 201 "{\"type\":\"small\",\"ttlSeconds\":$ttl}" 120
	response="$API_BODY"
	id="$(jq -er '.id' <<<"$response")" || fail "$description response did not contain an ID"
	BOX_IDS+=("$id")
	[[ "$id" =~ ^bx_[a-z2-7]{12}$ ]] || fail "$description returned an invalid sandbox ID"
	CREATED_BOX_ID="$id"
}

create_cli_ttl_box() {
	local description="$1" id
	if ! boxes new --type small --ttl 1800 >"$TEMP_DIR/new-box.json"; then
		fail "$description could not be created with boxes new"
	fi
	id="$(jq -er '.id' "$TEMP_DIR/new-box.json")" || fail "$description response did not contain an ID"
	BOX_IDS+=("$id")
	[[ "$id" =~ ^bx_[a-z2-7]{12}$ ]] || fail "$description returned an invalid sandbox ID"
	CREATED_BOX_ID="$id"
}

wait_box_state() {
	local box_id="$1" wanted="$2" timeout="$3" deadline code
	deadline=$((SECONDS + timeout))
	while (( SECONDS < deadline )); do
		code="$(curl --connect-timeout 4 --max-time 10 -sS -o "$TEMP_DIR/box.json" -w '%{http_code}' \
			-H "$AUTH_HEADER" "$API/sandboxes/$box_id" 2>/dev/null || true)"
		if [[ "$code" == 200 ]] && jq -e --arg state "$wanted" '.sandbox.state == $state' "$TEMP_DIR/box.json" >/dev/null 2>&1; then
			return 0
		fi
		sleep 2
	done
	fail "sandbox $box_id did not reach state $wanted within ${timeout}s"
}

run_box_command() {
	local box_id="$1" description="$2" command_text="$3" timeout="${4:-120}" body code exit_code
	body="$(jq -cn --arg command "$command_text" --argjson timeout "$timeout" \
		'{user:"abc",cmd:["sh","-lc",$command],timeoutSeconds:$timeout}')"
	code="$(curl --connect-timeout 5 --max-time "$((timeout + 20))" -sS -o "$TEMP_DIR/exec.json" -w '%{http_code}' \
		-X POST -H "$AUTH_HEADER" -H 'Content-Type: application/json' \
		--data-binary "$body" "$API/sandboxes/$box_id/commands" 2>/dev/null || true)"
	[[ "$code" == 200 ]] || fail "$description returned HTTP ${code:-none}"
	exit_code="$(jq -er '.exitCode' "$TEMP_DIR/exec.json")" || fail "$description response did not contain an exit code"
	[[ "$exit_code" == 0 ]] || fail "$description exited with code $exit_code"
	EXEC_STDOUT="$(jq -r '.stdout // ""' "$TEMP_DIR/exec.json")"
}

assert_persisted_state() {
	local box_id="$1" description="$2" note
	curl --connect-timeout 5 --max-time 30 -fsS -H "$AUTH_HEADER" \
		-o "$TEMP_DIR/note.txt" "$API/sandboxes/$box_id/files?path=%2Fconfig%2Fnote.txt" 2>/dev/null || \
		fail "$description lost /config/note.txt"
	note="$(<"$TEMP_DIR/note.txt")"
	[[ "$note" == 'boxes remote e2e snapshot note' ]] || fail "$description changed /config/note.txt"
	run_box_command "$box_id" "$description system-state assertion" \
		'command -v hello >/dev/null && test ! -e /etc/issue && test -d /usr/share/doc/bash && test -f /usr/share/doc/bash/only-this && test "$(find /usr/share/doc/bash -mindepth 1 -maxdepth 1 -printf "%f\n" | sort)" = only-this' 120
}

box_ssh_host_key() {
	local box_id="$1" file="$TEMP_DIR/$1-ssh-host-key.pub" key
	curl --connect-timeout 5 --max-time 30 -fsS -H "$AUTH_HEADER" \
		-o "$file" "$API/sandboxes/$box_id/files?path=%2Fetc%2Fssh%2Fssh_host_ed25519_key.pub" 2>/dev/null || \
		fail "sandbox $box_id has no readable SSH host public key"
	[[ -s "$file" ]] || fail "sandbox $box_id has an empty SSH host public key"
	key="$(<"$file")"
	printf '%s\n' "$key"
}

printf 'boxes remote e2e snapshot note\n' >"$TEMP_DIR/note.txt"

CURRENT_STEP="stop immediately after new"
create_box 'immediate-stop sandbox'
IMMEDIATE_STOP_ID="$CREATED_BOX_ID"
api_no_body POST "/sandboxes/$IMMEDIATE_STOP_ID/stop" 200
wait_box_state "$IMMEDIATE_STOP_ID" stopped 240
printf 'stop immediately after new succeeded\n'

CURRENT_STEP="create and verify a Sysbox sandbox"
create_box 'source sandbox'
BOX_ID="$CREATED_BOX_ID"
wait_box_state "$BOX_ID" ready 240
run_box_command "$BOX_ID" 'default user check' 'id -un'
[[ "$EXEC_STDOUT" == abc ]] || fail "id -un returned $EXEC_STDOUT; expected abc"
run_box_command "$BOX_ID" 'passwordless sudo check' 'sudo -n true'
run_box_command "$BOX_ID" 'Docker-in-box hello-world check' 'sudo docker run --rm hello-world' 300
printf 'small sandbox ready; abc, sudo, and Docker-in-box verified\n'

CURRENT_STEP="write config file and mutate the system"
api_upload "/sandboxes/$BOX_ID/files?path=%2Fconfig%2Fnote.txt&mode=0644" "$TEMP_DIR/note.txt" 204
run_box_command "$BOX_ID" 'install hello and mutate system files' \
	'sudo apt-get update && sudo apt-get install -y hello && sudo rm -f /etc/issue && sudo rm -rf /usr/share/doc/bash && sudo mkdir -p /usr/share/doc/bash && sudo touch /usr/share/doc/bash/only-this' 600

CURRENT_STEP="stop and resume snapshot verification"
SOURCE_HOST_KEY="$(box_ssh_host_key "$BOX_ID")"
api_no_body POST "/sandboxes/$BOX_ID/stop" 200
wait_box_state "$BOX_ID" stopped 240
api_json POST "/sandboxes/$BOX_ID/resume" 200 '{}' 120
wait_box_state "$BOX_ID" ready 300
assert_persisted_state "$BOX_ID" 'resumed sandbox'
RESUMED_HOST_KEY="$(box_ssh_host_key "$BOX_ID")"
[[ "$RESUMED_HOST_KEY" == "$SOURCE_HOST_KEY" ]] || fail 'SSH host key changed across stop/resume'
printf 'stop/resume preserved config and system changes\n'

CURRENT_STEP="fork snapshot verification"
api_json POST "/sandboxes/$BOX_ID/fork" 201 '{"type":"small","ttlSeconds":0}' 300
FORK_ID="$(jq -er '.sandbox.id' <<<"$API_BODY")" || fail 'fork response did not contain an ID'
BOX_IDS+=("$FORK_ID")
[[ "$FORK_ID" =~ ^bx_[a-z2-7]{12}$ ]] || fail 'fork returned an invalid sandbox ID'
wait_box_state "$FORK_ID" ready 300
assert_persisted_state "$FORK_ID" 'forked sandbox'
FORK_HOST_KEY="$(box_ssh_host_key "$FORK_ID")"
[[ "$FORK_HOST_KEY" != "$SOURCE_HOST_KEY" ]] || fail 'fork inherited the source SSH host key'
printf 'fork preserved config and system changes\n'

CURRENT_STEP="desktop TLS and authentication"
api_json POST "/sandboxes/$BOX_ID/desktop" 200 '{}' 60
DESKTOP_URL="$(jq -er '.url' <<<"$API_BODY")" || fail 'desktop response did not contain a URL'
DESKTOP_HOST="$(jq -er '.url | capture("^https://(?<host>[^/]+)").host' <<<"$API_BODY")" || fail 'desktop URL was invalid'
desktop_deadline=$((SECONDS + 120))
desktop_status=""
while (( SECONDS < desktop_deadline )); do
	desktop_status="$(curl --connect-timeout 5 --max-time 12 -sS -L -c "$TEMP_DIR/desktop.cookies" \
		-b "$TEMP_DIR/desktop.cookies" -o "$TEMP_DIR/desktop.html" -w '%{http_code}' "$DESKTOP_URL" 2>/dev/null || true)"
	[[ "$desktop_status" == 200 ]] && break
	sleep 3
done
[[ "$desktop_status" == 200 ]] || fail 'desktop did not present a valid TLS certificate and return HTTP 200 within 2 minutes'
unauthenticated_status="$(curl --connect-timeout 5 --max-time 15 -sS -o /dev/null -w '%{http_code}' \
	"https://$DESKTOP_HOST/" 2>/dev/null || true)"
[[ "$unauthenticated_status" == 401 ]] || fail "desktop without a token returned HTTP ${unauthenticated_status:-none}; expected 401"
authenticated_status="$(curl --connect-timeout 5 --max-time 15 -sS -L -c "$TEMP_DIR/desktop.cookies" \
	-b "$TEMP_DIR/desktop.cookies" -o "$TEMP_DIR/desktop.html" -w '%{http_code}' "$DESKTOP_URL" 2>/dev/null || true)"
[[ "$authenticated_status" == 200 ]] || fail "desktop with a token returned HTTP ${authenticated_status:-none}; expected 200"
printf 'desktop TLS and token authentication verified\n'

CURRENT_STEP="expose a live HTTP host over HTTPS"
run_box_command "$BOX_ID" 'prepare known HTTP response' \
	'mkdir -p /config/e2e-http && printf "boxes-remote-e2e-host-ok\\n" > /config/e2e-http/index.html'
host_body="$(jq -cn \
	'{user:"abc",cmd:["python3","-m","http.server","8000","--directory","/config/e2e-http"],detached:true}')"
host_exec_status="$(curl --connect-timeout 5 --max-time 30 -sS -o "$TEMP_DIR/host-process.json" -w '%{http_code}' \
	-X POST -H "$AUTH_HEADER" -H 'Content-Type: application/json' --data-binary "$host_body" \
	"$API/sandboxes/$BOX_ID/commands" 2>/dev/null || true)"
[[ "$host_exec_status" == 202 ]] || fail "detached HTTP server returned HTTP ${host_exec_status:-none}"
jq -e '.processId | strings | length > 0' "$TEMP_DIR/host-process.json" >/dev/null 2>&1 || fail 'detached HTTP server did not start'
api_json POST "/sandboxes/$BOX_ID/hosts" 201 '{"port":8000}' 60
HOST_URL="$(jq -er '.host.url' <<<"$API_BODY")" || fail 'host response did not contain a public URL'
host_deadline=$((SECONDS + 120))
host_status=""
host_content=""
while (( SECONDS < host_deadline )); do
	host_status="$(curl --connect-timeout 5 --max-time 12 -sS -o "$TEMP_DIR/host-response" -w '%{http_code}' \
		"$HOST_URL" 2>/dev/null || true)"
	if [[ "$host_status" == 200 ]]; then
		host_content="$(<"$TEMP_DIR/host-response")"
		[[ "$host_content" == 'boxes-remote-e2e-host-ok' ]] && break
	fi
	sleep 3
done
[[ "$host_status" == 200 && "$host_content" == 'boxes-remote-e2e-host-ok' ]] || \
	fail 'public host did not return the expected body over valid TLS within 2 minutes'
printf 'public host returned the expected response over valid TLS\n'

CURRENT_STEP="SSH through the boxes CLI proxy"
ssh-keygen -q -t ed25519 -N '' -C boxes-remote-e2e -f "$TEMP_DIR/ssh-key"
ssh_public_key="$(<"$TEMP_DIR/ssh-key.pub")"
ssh_key_body="$(jq -cn --arg publicKey "$ssh_public_key" '{publicKey:$publicKey}')"
api_json POST "/sandboxes/$BOX_ID/ssh-key" 204 "$ssh_key_body" 60
if ! boxes login --server "$BOXES_URL" --email "$BOXES_EMAIL" >/dev/null 2>&1; then
	fail 'host boxes CLI login failed'
fi
unset BOXES_PASSWORD
ssh_output="$(ssh -i "$TEMP_DIR/ssh-key" -o IdentitiesOnly=yes -o "ProxyCommand=boxes proxy $BOX_ID" -o BatchMode=yes -o ConnectTimeout=30 \
	-o ServerAliveInterval=5 -o ServerAliveCountMax=3 -o StrictHostKeyChecking=accept-new \
	-o "UserKnownHostsFile=$TEMP_DIR/known_hosts" "abc@$BOX_ID" echo ssh-ok 2>/dev/null)" || fail 'SSH through boxes proxy failed'
[[ "$ssh_output" == ssh-ok ]] || fail 'SSH through boxes proxy returned an unexpected response'
printf 'SSH key authentication through boxes proxy verified\n'

if [[ -n "${E2E_OPENCODE_AUTH_FILE:-}" ]]; then
	CURRENT_STEP="OpenCode computer screenshot prompt"
	api_upload "/sandboxes/$BOX_ID/files?path=%2Fconfig%2F.local%2Fshare%2Fopencode%2Fauth.json&mode=0600" \
		"$E2E_OPENCODE_AUTH_FILE" 204
	run_box_command "$BOX_ID" 'OpenCode auth file mode check' \
		'test "$(stat -c %a /config/.local/share/opencode/auth.json)" = 600'
	prompt_text='Call the computer_screenshot tool once, then reply with exactly: boxes-ok'
	prompt_body="$(jq -cn --arg prompt "$prompt_text" \
		'{provider:"opencode",model:"opencode-go/gpt-6-luna",prompt:$prompt}')"
	api_json POST "/sandboxes/$BOX_ID/prompt" 202 "$prompt_body" 60
	run_id="$(jq -er '.runId' <<<"$API_BODY")" || fail 'OpenCode prompt response did not contain a run ID'
	conversation_id="$(jq -er '.conversationId' <<<"$API_BODY")" || fail 'OpenCode prompt response did not contain a conversation ID'
	run_deadline=$((SECONDS + 300))
	run_status=""
	while (( SECONDS < run_deadline )); do
		code="$(curl --connect-timeout 5 --max-time 15 -sS -o "$TEMP_DIR/run.json" -w '%{http_code}' \
			-H "$AUTH_HEADER" "$API/sandboxes/$BOX_ID/runs/$run_id" 2>/dev/null || true)"
		if [[ "$code" == 200 ]]; then
			run_status="$(jq -r '.status // ""' "$TEMP_DIR/run.json")"
			case "$run_status" in
				completed) break ;;
				failed|interrupted) fail "OpenCode run ended with status $run_status" ;;
			esac
		fi
		sleep 3
	done
	[[ "$run_status" == completed ]] || fail 'OpenCode run did not complete within 5 minutes'

	events_deadline=$((SECONDS + 60))
	events_valid=0
	while (( SECONDS < events_deadline )); do
		# Page through every event of this conversation.
		printf '[]' > "$TEMP_DIR/all-events.json"
		after=0
		while :; do
			code="$(curl --connect-timeout 5 --max-time 15 -sS -o "$TEMP_DIR/events.json" -w '%{http_code}' \
				-H "$AUTH_HEADER" "$API/sandboxes/$BOX_ID/events?after=$after&conversation=$conversation_id" 2>/dev/null || true)"
			[[ "$code" == 200 ]] || break
			count="$(jq '.events | length' "$TEMP_DIR/events.json")"
			jq -s '.[0] + .[1].events' "$TEMP_DIR/all-events.json" "$TEMP_DIR/events.json" > "$TEMP_DIR/merged.json" && mv "$TEMP_DIR/merged.json" "$TEMP_DIR/all-events.json"
			next="$(jq -r '.cursor' "$TEMP_DIR/events.json")"
			(( count > 0 )) && [[ "$next" != "$after" ]] || break
			after="$next"
		done
		if jq -e '
			[.[] | select(.type == "tool.call" and .data.name == "computer_screenshot")] as $calls |
			[.[] | select(.type == "message")] as $messages |
			($calls | length) == 1 and ($messages | length) > 0 and
			($messages[-1].data.text == "boxes-ok")
		' "$TEMP_DIR/all-events.json" >/dev/null 2>&1; then
			events_valid=1
			break
		fi
		sleep 2
	done
	if [[ "$events_valid" != 1 ]]; then
		jq -r '"events=\(length) tool.calls=\([.[] | select(.type=="tool.call") | .data.name] | join(",")) last_message=\([.[] | select(.type=="message")] | last | .data.text // "none" | .[0:120])"' "$TEMP_DIR/all-events.json" >&2 || true
		fail 'expected exactly one computer_screenshot tool.call and final message boxes-ok'
	fi
	printf 'OpenCode completed with one screenshot tool call and exact final message\n'
fi

if [[ -n "${BOXES_SSH_HOST:-}" ]]; then
	CURRENT_STEP="60-second Xvfb socket and container-log health check"
	remote_command="set -eu; box_id='$BOX_ID'; container=\$(docker ps -q --filter label=boxes.box-id=\$box_id); test -n \"\$container\"; i=0; while [ \"\$i\" -lt 12 ]; do count=\$(docker exec \"\$container\" sh -c 'test -d /tmp/.X11-unix && find /tmp/.X11-unix -maxdepth 1 -type s -name \"X*\" | wc -l'); test \"\$count\" -lt 64 || { printf 'Xvfb socket count exceeded 63: %s\\n' \"\$count\" >&2; exit 1; }; sleep 5; i=\$((i + 1)); done; count=\$(docker exec \"\$container\" sh -c 'test -d /tmp/.X11-unix && find /tmp/.X11-unix -maxdepth 1 -type s -name \"X*\" | wc -l'); test \"\$count\" -lt 64 || { printf 'Xvfb socket count exceeded 63 after 60 seconds: %s\\n' \"\$count\" >&2; exit 1; }; logs=\$(docker logs \"\$container\" 2>&1); case \"\$logs\" in *Aborted*) printf 'box container logs contain Aborted\\n' >&2; exit 1;; esac"
	if ! ssh -o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=5 -o ServerAliveCountMax=3 \
		"$BOXES_SSH_HOST" "$remote_command" >/dev/null; then
		fail 'remote Xvfb socket or container-log health check failed'
	fi
	printf 'Xvfb socket count stayed below 64 for 60 seconds; no Aborted in container logs\n'
fi

CURRENT_STEP="multi-box stop, resume, data isolation, and TTL lifecycle"
# Release capacity used by the preceding snapshot and fork checks before starting
# the two lifecycle boxes. The machine may also host one always-on box.
for box_id in "$BOX_ID" "$FORK_ID"; do
	api_no_body POST "/sandboxes/$box_id/stop" 200
	wait_box_state "$box_id" stopped 240
done

create_cli_ttl_box 'lifecycle box A'
LIFECYCLE_A_ID="$CREATED_BOX_ID"
wait_box_state "$LIFECYCLE_A_ID" ready 240
create_cli_ttl_box 'lifecycle box B'
LIFECYCLE_B_ID="$CREATED_BOX_ID"
wait_box_state "$LIFECYCLE_B_ID" ready 240

run_box_command "$LIFECYCLE_A_ID" 'write box A lifecycle data' \
	'printf "%s" box-a-config > /config/important.txt && sudo mkdir -p /opt/proj && sudo sh -c '\''printf "#!/bin/sh\nprintf tool-a\n" > /opt/proj/tool'\'' && sudo chmod 0755 /opt/proj/tool'
run_box_command "$LIFECYCLE_B_ID" 'write box B lifecycle data' \
	'printf "%s" box-b-config > /config/important.txt && sudo mkdir -p /opt/proj && sudo sh -c '\''printf "#!/bin/sh\nprintf tool-b\n" > /opt/proj/tool'\'' && sudo chmod 0755 /opt/proj/tool'

for box_id in "$LIFECYCLE_A_ID" "$LIFECYCLE_B_ID"; do
	api_no_body POST "/sandboxes/$box_id/stop" 200
	wait_box_state "$box_id" stopped 240
done
if ! boxes list --filter s >"$TEMP_DIR/stopped-boxes.json"; then
	fail 'boxes list --filter s failed'
fi
jq -e --arg a "$LIFECYCLE_A_ID" --arg b "$LIFECYCLE_B_ID" \
	'any(.[]; .id == $a and .state == "stopped") and any(.[]; .id == $b and .state == "stopped")' \
	"$TEMP_DIR/stopped-boxes.json" >/dev/null || fail 'boxes list --filter s did not show both stopped lifecycle boxes'

if ! boxes resume "$LIFECYCLE_B_ID" >/dev/null; then
	fail 'could not resume lifecycle box B'
fi
wait_box_state "$LIFECYCLE_B_ID" ready 300
if ! boxes resume "$LIFECYCLE_A_ID" >/dev/null; then
	fail 'could not resume lifecycle box A'
fi
wait_box_state "$LIFECYCLE_A_ID" ready 300
run_box_command "$LIFECYCLE_A_ID" 'verify box A data isolation' \
	'test "$(cat /config/important.txt)" = box-a-config && test "$(stat -c %u:%g /opt/proj/tool)" = 0:0 && test -x /opt/proj/tool && test "$(/opt/proj/tool)" = tool-a'
run_box_command "$LIFECYCLE_B_ID" 'verify box B data isolation' \
	'test "$(cat /config/important.txt)" = box-b-config && test "$(stat -c %u:%g /opt/proj/tool)" = 0:0 && test -x /opt/proj/tool && test "$(/opt/proj/tool)" = tool-b'

if ! boxes extend "$LIFECYCLE_A_ID" --no-auto-stop >/dev/null; then
	fail 'boxes extend --no-auto-stop failed for lifecycle box A'
fi
if ! boxes info "$LIFECYCLE_A_ID" >"$TEMP_DIR/lifecycle-a-info.json" || \
	! jq -e '.autoStop == false' "$TEMP_DIR/lifecycle-a-info.json" >/dev/null; then
	fail 'lifecycle box A autoStop did not become false'
fi
if ! boxes extend "$LIFECYCLE_A_ID" --hours 1 >/dev/null; then
	fail 'boxes extend --hours 1 failed for lifecycle box A'
fi
if ! boxes info "$LIFECYCLE_A_ID" >"$TEMP_DIR/lifecycle-a-info.json" || \
	! jq -e '.autoStop == true and .ttlRemainingSeconds >= 3500 and .ttlRemainingSeconds <= 3600' \
		"$TEMP_DIR/lifecycle-a-info.json" >/dev/null; then
	fail 'lifecycle box A did not receive an approximately one-hour TTL'
fi

if ! boxes delete "$LIFECYCLE_A_ID" --yes >/dev/null; then
	fail 'could not delete lifecycle box A'
fi
if ! boxes delete "$LIFECYCLE_B_ID" --yes >/dev/null; then
	fail 'could not delete lifecycle box B'
fi
printf 'multi-box lifecycle, isolated persistence, and TTL extensions verified\n'

CURRENT_STEP="cleanup and completion"
for box_id in "${BOX_IDS[@]}"; do
	code="$(curl --connect-timeout 5 --max-time 30 -sS -o /dev/null -w '%{http_code}' \
		-X DELETE -H "$AUTH_HEADER" "$API/sandboxes/$box_id" 2>/dev/null || true)"
	[[ "$code" == 200 || "$code" == 204 || "$code" == 404 ]] || fail "could not delete sandbox $box_id (HTTP ${code:-none})"
done
BOX_IDS=()
printf 'E2E REMOTE PASS\n'
