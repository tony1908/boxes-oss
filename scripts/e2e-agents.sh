#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
command -v pnpm >/dev/null 2>&1 || { printf 'required command not found: pnpm\n' >&2; exit 1; }
if [[ ! -d web/node_modules ]]; then pnpm --dir web install --frozen-lockfile; fi
make web

source scripts/e2e-lib.sh
e2e_init

web_root="$(curl -fsS "$SERVER_URL/")"
[[ "$web_root" == *'Paddock — machines for agents'* ]]
[[ "$web_root" == *'/_next/static/'* ]]
[[ "$web_root" != *'Dashboard assets have not been built yet'* ]]
web_route="$(curl -fsS "$SERVER_URL/sandboxes")"
[[ "$web_route" == *'Paddock — machines for agents'* ]]
[[ "$web_route" == *'/_next/static/'* ]]
[[ "$web_route" != *'Dashboard assets have not been built yet'* ]]
printf 'web app and SPA route served\n'

HOST_BIN_DIR="$E2E_DIR/bin"
HOST_CLI="$HOST_BIN_DIR/paddock"
if [[ ! -d "$HOST_BIN_DIR" ]]; then mkdir "$HOST_BIN_DIR"; HOST_BIN_DIR_CREATED=1; fi
if [[ -e "$HOST_CLI" ]]; then printf 'refusing to overwrite pre-existing CLI binary %s\n' "$HOST_CLI" >&2; exit 1; fi
go build -o "$HOST_CLI" ./cmd/paddock
HOST_CLI_CREATED=1

TEMP_HOME="$E2E_DIR/home-$RUN_ID"
if [[ -e "$TEMP_HOME" ]]; then printf 'refusing to overwrite pre-existing HOME %s\n' "$TEMP_HOME" >&2; exit 1; fi
mkdir -m 0700 "$TEMP_HOME"
TEMP_HOME_CREATED=1
export HOME="$TEMP_HOME"
export XDG_CONFIG_HOME="$TEMP_HOME/xdg-config"
export PADDOCK_PASSWORD=e2e-password-long

"$HOST_CLI" login --server "$SERVER_URL" --email e2e@example.test >/dev/null
new_output="$("$HOST_CLI" new --type small --ttl 0)"
cli_box_id="${new_output%%$'\t'*}"
[[ "$cli_box_id" == bx_* ]]
BOX_IDS+=("$cli_box_id")
e2e_wait_box_ready "$cli_box_id"
list_output="$("$HOST_CLI" list --json)"
cli_exec_output="$("$HOST_CLI" exec "$cli_box_id" -- echo cli-ok)"
[[ "${cli_exec_output%$'\n'}" == cli-ok ]]
"$HOST_CLI" delete "$cli_box_id" >/dev/null
BOX_IDS=()
printf 'CLI login, create, list, exec, and delete passed\n'

box_id="$(e2e_create_box)"
BOX_IDS+=("$box_id")
e2e_wait_box_ready "$box_id"
mcp_body="$(jq -cn --rawfile script scripts/e2e-mcp-stdio.js '{cmd:["node","-e",$script],user:"abc"}')"
mcp_response="$(curl -fsS -X POST "$API/sandboxes/$box_id/commands" -H "$AUTH_HEADER" -H 'Content-Type: application/json' --data-binary "$mcp_body")"
[[ "$(jq -er '.exitCode' <<<"$mcp_response")" == 0 ]]
mcp_output="$(jq -er '.stdout' <<<"$mcp_response")"
jq -se 'any(.[]; .id == 2 and ([.result.tools[].name] | index("screenshot") != null and index("click") != null and index("type") != null))' <<<"$mcp_output" >/dev/null
jq -se 'any(.[]; .id == 3 and any(.result.content[]; .type == "image" and .mimeType == "image/png" and (.data | test("^iVBORw0KGgo"))))' <<<"$mcp_output" >/dev/null
printf 'computer-use MCP initialize, tools, and PNG screenshot passed\n'

if [[ -n "${E2E_OPENCODE_AUTH_FILE:-}" ]]; then
	[[ -f "$E2E_OPENCODE_AUTH_FILE" && -s "$E2E_OPENCODE_AUTH_FILE" ]] || { printf 'E2E_OPENCODE_AUTH_FILE must name a non-empty file\n' >&2; exit 1; }
	auth_path='%2Fconfig%2F.local%2Fshare%2Fopencode%2Fauth.json'
	curl -fsS -o /dev/null -X PUT "$API/sandboxes/$box_id/files?path=$auth_path&mode=0600" \
		-H "$AUTH_HEADER" -H 'Content-Type: application/octet-stream' --data-binary "@$E2E_OPENCODE_AUTH_FILE"
	mode_response="$(curl -fsS -X POST "$API/sandboxes/$box_id/commands" -H "$AUTH_HEADER" -H 'Content-Type: application/json' \
		-d '{"user":"abc","cmd":["stat","-c","%a","/config/.local/share/opencode/auth.json"]}')"
	[[ "$(jq -er '.stdout | rtrimstr("\n")' <<<"$mode_response")" == 600 ]]
	mcp_config_response="$(curl -fsS -X POST "$API/sandboxes/$box_id/commands" -H "$AUTH_HEADER" -H 'Content-Type: application/json' \
		-d '{"user":"abc","cmd":["jq","-e",".mcp.computer.type == \"local\" and .mcp.computer.command == [\"boxd\",\"mcp\"] and .mcp.computer.enabled == true","/config/.config/opencode/opencode.json"]}')"
	[[ "$(jq -er '.exitCode' <<<"$mcp_config_response")" == 0 ]] || { printf 'OpenCode computer MCP config is missing: %s\n' "$(jq -r '.stderr' <<<"$mcp_config_response")" >&2; exit 1; }
	prompt_text='Reply with exactly: paddock-ok'
	prompt_body="$(jq -cn --arg prompt "$prompt_text" '{provider:"opencode",model:"opencode-go/gpt-6-luna",prompt:$prompt}')"
	prompt_response="$(curl -fsS -X POST "$API/sandboxes/$box_id/prompt" -H "$AUTH_HEADER" -H 'Content-Type: application/json' --data-binary "$prompt_body")"
	run_id="$(jq -er '.runId' <<<"$prompt_response")"
	conversation_id="$(jq -er '.conversationId' <<<"$prompt_response")"
	run_deadline=$((SECONDS + 300))
	cursor=0
	message_found=0
	while (( SECONDS < run_deadline )); do
		event_response="$(curl -fsS "$API/sandboxes/$box_id/events?after=$cursor&wait=3&conversation=$conversation_id" -H "$AUTH_HEADER")"
		cursor="$(jq -er '.cursor // 0' <<<"$event_response")"
		if jq -e 'any(.events[]; .type == "message" and ((.data.text // "") | tostring | contains("paddock-ok")))' <<<"$event_response" >/dev/null; then message_found=1; fi
		run_response="$(curl -fsS "$API/sandboxes/$box_id/runs/$run_id" -H "$AUTH_HEADER")"
		run_status="$(jq -er '.status' <<<"$run_response")"
		if [[ "$run_status" == failed || "$run_status" == interrupted ]]; then
			printf 'OpenCode run ended with status %s\n' "$run_status" >&2
			exit 1
		fi
		if [[ "$run_status" == completed ]]; then break; fi
	done
	[[ "$run_status" == completed ]] || { printf 'OpenCode run %s did not complete within 5 minutes\n' "$run_id" >&2; exit 1; }
	if [[ "$message_found" != 1 ]]; then
		final_events="$(curl -fsS "$API/sandboxes/$box_id/events?after=0&conversation=$conversation_id" -H "$AUTH_HEADER" | jq -c '[.events[] | {type,textChars:((.data.text // "") | tostring | length),containsExpected:((.data.text // "") | tostring | contains("paddock-ok")),dataFields:(.data | keys)}]')"
		printf 'OpenCode completed but expected message was not observed (event summary: %s)\n' "$final_events" >&2
		exit 1
	fi
	final_event_response="$(curl -fsS "$API/sandboxes/$box_id/events?after=0&conversation=$conversation_id" -H "$AUTH_HEADER")"
	jq -e --arg prompt "$prompt_text" '[.events[] | select(.type == "message")] as $messages | ($messages | length > 0) and ((($messages[-1].data.text // "") | tostring | contains("paddock-ok")) and ((($messages[-1].data.text // "") | tostring | contains($prompt)) | not))' <<<"$final_event_response" >/dev/null || {
		final_message="$(jq -c '[.events[] | select(.type == "message") | .data.text] | last' <<<"$final_event_response")"
		printf 'OpenCode final message contains the prompt or is missing the expected response: %s\n' "$final_message" >&2
		exit 1
	}
	conversation_response="$(curl -fsS "$API/sandboxes/$box_id/conversations" -H "$AUTH_HEADER")"
	jq -e --arg id "$conversation_id" 'any(.[]; .id == $id)' <<<"$conversation_response" >/dev/null
	printf 'real OpenCode prompt, events, and conversation passed\n'
else
	printf 'SKIP: real OpenCode prompt (E2E_OPENCODE_AUTH_FILE is unset)\n'
fi

for id in "${BOX_IDS[@]}"; do curl -fsS -X DELETE "$API/sandboxes/$id" -H "$AUTH_HEADER" >/dev/null; done
BOX_IDS=()
printf 'E2E AGENTS PASS\n'
