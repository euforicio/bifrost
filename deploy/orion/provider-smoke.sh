#!/usr/bin/env bash
set -euo pipefail

base_url=${BIFROST_BASE_URL:-https://bifrost.riftlabs.app}
: "${BIFROST_XAI_VIRTUAL_KEY:?BIFROST_XAI_VIRTUAL_KEY is required}"
: "${BIFROST_CODEX_VIRTUAL_KEY:?BIFROST_CODEX_VIRTUAL_KEY is required}"
: "${BIFROST_CURSOR_VIRTUAL_KEY:?BIFROST_CURSOR_VIRTUAL_KEY is required}"
: "${BIFROST_OLLAMA_VIRTUAL_KEY:?BIFROST_OLLAMA_VIRTUAL_KEY is required}"
: "${BIFROST_FIREWORKS_VIRTUAL_KEY:?BIFROST_FIREWORKS_VIRTUAL_KEY is required}"

smoke_dir=$(mktemp -d)
trap 'rm -rf -- "$smoke_dir"' EXIT

write_auth_config() {
  local path=$1
  local token=$2
  umask 077
  printf 'header = "Authorization: Bearer %s"\n' "$token" > "$path"
}

assert_json_contains() {
  local path=$1
  local sentinel=$2
  python3 - "$path" "$sentinel" <<'PY'
import json
import sys

path, sentinel = sys.argv[1:]
with open(path, encoding="utf-8") as handle:
    payload = json.load(handle)

values = []
def collect(value):
    if isinstance(value, str):
        values.append(value)
    elif isinstance(value, list):
        for item in value:
            collect(item)
    elif isinstance(value, dict):
        for item in value.values():
            collect(item)

collect(payload)
if sentinel not in "".join(values):
    raise SystemExit(f"expected sentinel {sentinel!r} in JSON response")
PY
}

assert_sse_contains() {
  local path=$1
  local sentinel=$2
  local terminal=$3
  python3 - "$path" "$sentinel" "$terminal" <<'PY'
import json
import sys

path, sentinel, terminal = sys.argv[1:]
values = []
event_types = []
text_keys = {"content", "text", "delta", "arguments", "output_text"}

def collect_text(value):
    if isinstance(value, list):
        for item in value:
            collect_text(item)
    elif isinstance(value, dict):
        for key, item in value.items():
            if key in text_keys and isinstance(item, str):
                values.append(item)
            elif not isinstance(item, str):
                collect_text(item)

with open(path, encoding="utf-8") as handle:
    for line in handle:
        line = line.strip()
        if not line.startswith("data:"):
            continue
        data = line[5:].strip()
        if not data or data == "[DONE]":
            continue
        payload = json.loads(data)
        if isinstance(payload, dict) and isinstance(payload.get("type"), str):
            event_types.append(payload["type"])
        collect_text(payload)

if sentinel not in "".join(values):
    raise SystemExit(f"expected sentinel {sentinel!r} in SSE response")
if terminal and terminal not in event_types:
    raise SystemExit(f"expected terminal event {terminal!r}, got {event_types!r}")
PY
}

assert_function_call() {
  local path=$1
  local function_name=$2
  local sentinel=$3
  python3 - "$path" "$function_name" "$sentinel" <<'PY'
import json
import sys

path, function_name, sentinel = sys.argv[1:]
with open(path, encoding="utf-8") as handle:
    payload = json.load(handle)

stack = [payload]
while stack:
    value = stack.pop()
    if isinstance(value, list):
        stack.extend(value)
    elif isinstance(value, dict):
        if value.get("type") == "function_call":
            name = str(value.get("name", ""))
            arguments = str(value.get("arguments", ""))
            if name.split(".")[-1] == function_name and sentinel in arguments:
                break
        stack.extend(value.values())
else:
    raise SystemExit(f"expected {function_name!r} function call containing {sentinel!r}")
PY
}

assert_model_exists() {
  local path=$1
  local model=$2
  python3 - "$path" "$model" <<'PY'
import json
import sys

path, model = sys.argv[1:]
with open(path, encoding="utf-8") as handle:
    payload = json.load(handle)

models = [item.get("id") for item in payload.get("data", []) if isinstance(item, dict)]
if model not in models:
    raise SystemExit(f"expected model {model!r} in provider catalog")
PY
}

select_model_for_method() {
  local path=$1
  local method=$2
  python3 - "$path" "$method" <<'PY'
import json
import sys

path, method = sys.argv[1:]
with open(path, encoding="utf-8") as handle:
    payload = json.load(handle)
for model in payload.get("data", []):
    if method in model.get("supported_methods", []):
        print(model["id"])
        break
else:
    raise SystemExit(f"no model supports {method!r}")
PY
}

response_payload() {
  local model=$1
  local prompt=$2
  python3 - "$model" "$prompt" <<'PY'
import json
import sys
print(json.dumps({"model": sys.argv[1], "input": sys.argv[2]}))
PY
}

xai_auth="$smoke_dir/xai.curl"
codex_auth="$smoke_dir/codex.curl"
cursor_auth="$smoke_dir/cursor.curl"
ollama_auth="$smoke_dir/ollama.curl"
fireworks_auth="$smoke_dir/fireworks.curl"
write_auth_config "$xai_auth" "$BIFROST_XAI_VIRTUAL_KEY"
write_auth_config "$codex_auth" "$BIFROST_CODEX_VIRTUAL_KEY"
write_auth_config "$cursor_auth" "$BIFROST_CURSOR_VIRTUAL_KEY"
write_auth_config "$ollama_auth" "$BIFROST_OLLAMA_VIRTUAL_KEY"
write_auth_config "$fireworks_auth" "$BIFROST_FIREWORKS_VIRTUAL_KEY"

curl --fail --silent --show-error --max-time 90 --config "$xai_auth" \
  -H 'Content-Type: application/json' \
  --data '{"model":"xai/grok-4-1-fast-non-reasoning","messages":[{"role":"user","content":"Reply with exactly: BIFROST_XAI_UNARY_OK"}]}' \
  "$base_url/v1/chat/completions" > "$smoke_dir/xai-unary.json"
assert_json_contains "$smoke_dir/xai-unary.json" BIFROST_XAI_UNARY_OK

curl --fail --silent --show-error --no-buffer --max-time 90 --config "$xai_auth" \
  -H 'Content-Type: application/json' \
  --data '{"model":"xai/grok-4-1-fast-non-reasoning","stream":true,"messages":[{"role":"user","content":"Reply with exactly: BIFROST_XAI_STREAM_OK"}]}' \
  "$base_url/v1/chat/completions" > "$smoke_dir/xai-stream.sse"
assert_sse_contains "$smoke_dir/xai-stream.sse" BIFROST_XAI_STREAM_OK ''

curl --fail --silent --show-error --max-time 120 --config "$xai_auth" \
  -H 'Content-Type: application/json' \
  --data '{"model":"xai/grok-4-1-fast-non-reasoning","input":"Reply with exactly: BIFROST_XAI_RESPONSES_OK"}' \
  "$base_url/v1/responses" > "$smoke_dir/xai-responses.json"
assert_json_contains "$smoke_dir/xai-responses.json" BIFROST_XAI_RESPONSES_OK

curl --fail --silent --show-error --max-time 120 --config "$xai_auth" \
  -H 'Content-Type: application/json' \
  --data '{"model":"xai/grok-4-1-fast-non-reasoning","input":"You must call the echo function with value BIFROST_XAI_TOOL_OK. Do not answer directly.","tools":[{"type":"function","name":"echo","description":"Echo the supplied value","parameters":{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}}]}' \
  "$base_url/v1/responses" > "$smoke_dir/xai-tool.json"
assert_function_call "$smoke_dir/xai-tool.json" echo BIFROST_XAI_TOOL_OK

curl --fail --silent --show-error --max-time 120 --config "$codex_auth" \
  -H 'Content-Type: application/json' \
  --data '{"model":"openai-codex/gpt-5.6-sol","input":"Reply with exactly: BIFROST_CODEX_UNARY_OK","reasoning":{"effort":"low","summary":"auto"}}' \
  "$base_url/v1/responses" > "$smoke_dir/codex-unary.json"
assert_json_contains "$smoke_dir/codex-unary.json" BIFROST_CODEX_UNARY_OK

curl --fail --silent --show-error --max-time 120 --config "$codex_auth" \
  -H 'Content-Type: application/json' \
  --data '{"model":"openai-codex/gpt-5.6-sol","input":"You must call the echo function with value BIFROST_CODEX_TOOL_OK. Do not answer directly.","tools":[{"type":"function","name":"echo","description":"Echo the supplied value","parameters":{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false},"strict":true}],"reasoning":{"effort":"low","summary":"auto"}}' \
  "$base_url/v1/responses" > "$smoke_dir/codex-tool.json"
assert_function_call "$smoke_dir/codex-tool.json" echo BIFROST_CODEX_TOOL_OK

curl --fail --silent --show-error --no-buffer --max-time 120 --config "$codex_auth" \
  -H 'Content-Type: application/json' \
  --data '{"model":"openai-codex/gpt-5.6-sol","stream":true,"input":"Reply with exactly: BIFROST_CODEX_STREAM_OK","reasoning":{"effort":"low","summary":"auto"}}' \
  "$base_url/v1/responses" > "$smoke_dir/codex-stream.sse"
assert_sse_contains "$smoke_dir/codex-stream.sse" BIFROST_CODEX_STREAM_OK response.completed

curl --fail --silent --show-error --max-time 60 --config "$cursor_auth" \
  "$base_url/v1/models?provider=cursor" > "$smoke_dir/cursor-models.json"
assert_model_exists "$smoke_dir/cursor-models.json" cursor/default

curl --fail --silent --show-error --max-time 180 --config "$cursor_auth" \
  -H 'Content-Type: application/json' \
  --data '{"model":"cursor/default","input":"Reply with exactly: BIFROST_CURSOR_UNARY_OK"}' \
  "$base_url/v1/responses" > "$smoke_dir/cursor-unary.json"
assert_json_contains "$smoke_dir/cursor-unary.json" BIFROST_CURSOR_UNARY_OK

curl --fail --silent --show-error --max-time 180 --config "$cursor_auth" \
  -H 'Content-Type: application/json' \
  --data '{"model":"cursor/default","input":"You must call the echo function with value BIFROST_CURSOR_TOOL_OK. Do not answer directly.","tools":[{"type":"function","name":"echo","description":"Echo the supplied value","parameters":{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}}]}' \
  "$base_url/v1/responses" > "$smoke_dir/cursor-tool.json"
assert_function_call "$smoke_dir/cursor-tool.json" echo BIFROST_CURSOR_TOOL_OK

curl --fail --silent --show-error --no-buffer --max-time 180 --config "$cursor_auth" \
  -H 'Content-Type: application/json' \
  --data '{"model":"cursor/default","stream":true,"input":"Reply with exactly: BIFROST_CURSOR_STREAM_OK"}' \
  "$base_url/v1/responses" > "$smoke_dir/cursor-stream.sse"
assert_sse_contains "$smoke_dir/cursor-stream.sse" BIFROST_CURSOR_STREAM_OK response.completed

curl --fail --silent --show-error --max-time 60 --config "$ollama_auth" \
  "$base_url/v1/models?provider=ollama" > "$smoke_dir/ollama-models.json"
assert_model_exists "$smoke_dir/ollama-models.json" ollama/gpt-oss:20b

curl --fail --silent --show-error --max-time 180 --config "$ollama_auth" \
  -H 'Content-Type: application/json' \
  --data '{"model":"ollama/gpt-oss:20b","messages":[{"role":"user","content":"Reply with exactly: BIFROST_OLLAMA_UNARY_OK"}]}' \
  "$base_url/v1/chat/completions" > "$smoke_dir/ollama-unary.json"
assert_json_contains "$smoke_dir/ollama-unary.json" BIFROST_OLLAMA_UNARY_OK

curl --fail --silent --show-error --no-buffer --max-time 180 --config "$ollama_auth" \
  -H 'Content-Type: application/json' \
  --data '{"model":"ollama/gpt-oss:20b","stream":true,"messages":[{"role":"user","content":"Reply with exactly: BIFROST_OLLAMA_STREAM_OK"}]}' \
  "$base_url/v1/chat/completions" > "$smoke_dir/ollama-stream.sse"
assert_sse_contains "$smoke_dir/ollama-stream.sse" BIFROST_OLLAMA_STREAM_OK ''

curl --fail --silent --show-error --max-time 90 --config "$fireworks_auth" \
  "$base_url/v1/models?provider=fireworks" > "$smoke_dir/fireworks-models.json"
fireworks_chat_model=$(select_model_for_method "$smoke_dir/fireworks-models.json" chat_completion)

curl --fail --silent --show-error --max-time 180 --config "$fireworks_auth" \
  -H 'Content-Type: application/json' \
  --data "$(response_payload "$fireworks_chat_model" 'Reply with exactly: BIFROST_FIREWORKS_RESPONSES_OK')" \
  "$base_url/v1/responses" > "$smoke_dir/fireworks-responses.json"
assert_json_contains "$smoke_dir/fireworks-responses.json" BIFROST_FIREWORKS_RESPONSES_OK

curl --fail --silent --show-error --max-time 180 --config "$fireworks_auth" \
  -H 'Content-Type: application/json' \
  --data "$(python3 - "$fireworks_chat_model" <<'PY'
import json
import sys
print(json.dumps({"model": sys.argv[1], "messages": [{"role": "user", "content": "Reply with exactly: BIFROST_FIREWORKS_UNARY_OK"}]}))
PY
)" \
  "$base_url/v1/chat/completions" > "$smoke_dir/fireworks-unary.json"
assert_json_contains "$smoke_dir/fireworks-unary.json" BIFROST_FIREWORKS_UNARY_OK

echo "Provider inference smoke checks passed."
