#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  cat <<'EOF'
Usage:
  scripts/live-smoke-hermes.sh [options]

Runs a live smoke test without changing your parent shell environment:
  1. builds miniproxy into a temp directory
  2. starts it on a temporary localhost port in direct-anthropic mode
  3. sends live Anthropic Messages requests through the bridge
  4. runs Hermes chat with a temporary home pointed at the bridge
  5. stops the proxy and removes temp files unless --keep-workdir is used

Options:
  --token TOKEN        Claude setup-token / OAuth token for the real upstream.
                       If omitted, the script uses ANTHROPIC_API_KEY_REAL,
                       ANTHROPIC_AUTH_TOKEN, CLAUDE_CODE_OAUTH_TOKEN, or prompts.
  --model MODEL        Model to request. Default: claude-sonnet-4-6
  --listen ADDR        Listen address for the temporary proxy. Default: random 127.0.0.1 port
  --real-base-url URL  Real upstream Anthropic base URL. Default: https://api.anthropic.com/v1
  --skip-hermes        Only test the bridge with curl; do not invoke hermes.
  --keep-workdir       Keep the temp directory with proxy logs, dumps, and responses.
  -h, --help           Show this help.

Examples:
  scripts/live-smoke-hermes.sh --token "$(claude setup-token)"
  scripts/live-smoke-hermes.sh --model claude-sonnet-4-6
EOF
}

die() {
  echo "error: $*" >&2
  exit 1
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

pick_port() {
  local port
  for _ in {1..80}; do
    port=$((20000 + RANDOM % 20000))
    if ! (echo >/dev/tcp/127.0.0.1/"$port") >/dev/null 2>&1; then
      printf '127.0.0.1:%s\n' "$port"
      return 0
    fi
  done
  return 1
}

wait_for_pid_with_timeout() {
  local pid=$1
  local timeout_s=$2
  local label=$3
  local start=$SECONDS

  while kill -0 "$pid" >/dev/null 2>&1; do
    if ((SECONDS - start >= timeout_s)); then
      echo "$label timed out after ${timeout_s}s" >&2
      kill "$pid" >/dev/null 2>&1 || true
      wait "$pid" 2>/dev/null || true
      return 124
    fi
    sleep 1
  done

  wait "$pid"
}

json_escape() {
  local value=$1
  value=${value//\\/\\\\}
  value=${value//\"/\\\"}
  value=${value//$'\n'/\\n}
  value=${value//$'\r'/\\r}
  value=${value//$'\t'/\\t}
  printf '%s' "$value"
}

wait_for_proxy() {
  local url=$1
  local log_file=$2
  local i
  for i in {1..100}; do
    if curl -fsS "$url/healthz" >/dev/null 2>&1; then
      return 0
    fi
    if ! kill -0 "$proxy_pid" >/dev/null 2>&1; then
      echo "proxy exited early; log follows:" >&2
      sed -n '1,220p' "$log_file" >&2 || true
      return 1
    fi
    sleep 0.1
  done
  echo "proxy did not become healthy; log follows:" >&2
  sed -n '1,220p' "$log_file" >&2 || true
  return 1
}

post_json() {
  local name=$1
  local url=$2
  local request_file=$3
  local response_file=$4
  local header_file=$5
  local status

  status=$(
    curl -sS \
      --compressed \
      -D "$header_file" \
      -o "$response_file" \
      -w '%{http_code}' \
      -X POST "$url/v1/messages" \
      -H 'content-type: application/json' \
      -H 'anthropic-version: 2023-06-01' \
      -H 'x-api-key: dummy' \
      --data-binary @"$request_file"
  ) || {
    echo "$name curl failed" >&2
    return 1
  }

  case "$status" in
    2*) return 0 ;;
    *)
      echo "$name returned HTTP $status" >&2
      echo "response:" >&2
      sed -n '1,220p' "$response_file" >&2 || true
      return 1
      ;;
  esac
}

print_latest_bridge_error() {
  local latest
  latest=$(find "$tmpdir/dumps" -type f -name '*client-anthropic-response.json' 2>/dev/null | sort | tail -1 || true)
  if [[ -n "$latest" && -s "$latest" ]]; then
    echo "latest bridge client response ($latest):" >&2
    sed -n '1,220p' "$latest" >&2 || true
  fi

  latest=$(find "$tmpdir/hermes-home/sessions" -type f -name 'request_dump_*.json' 2>/dev/null | sort | tail -1 || true)
  if [[ -n "$latest" && -s "$latest" ]]; then
    echo "latest Hermes request dump ($latest):" >&2
    sed -n '1,80p' "$latest" >&2 || true
  fi
}

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
token="${ANTHROPIC_API_KEY_REAL:-${ANTHROPIC_AUTH_TOKEN:-${CLAUDE_CODE_OAUTH_TOKEN:-}}}"
model="claude-sonnet-4-6"
listen_addr=""
real_base_url="https://api.anthropic.com/v1"
run_hermes=1
keep_workdir=0

while (($#)); do
  case "$1" in
    --token)
      shift
      (($#)) || die "--token requires a value"
      token=$1
      ;;
    --model)
      shift
      (($#)) || die "--model requires a value"
      model=$1
      ;;
    --listen)
      shift
      (($#)) || die "--listen requires a value"
      listen_addr=$1
      ;;
    --real-base-url)
      shift
      (($#)) || die "--real-base-url requires a value"
      real_base_url=$1
      ;;
    --skip-hermes)
      run_hermes=0
      ;;
    --keep-workdir)
      keep_workdir=1
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
  shift
done

need_cmd go
need_cmd curl
if ((run_hermes)); then
  need_cmd hermes
fi

if [[ -z "${token// }" ]]; then
  if [[ -t 0 ]]; then
    read -r -s -p "Paste Claude setup-token / OAuth token: " token
    echo
  else
    die "no token supplied; pass --token or set ANTHROPIC_API_KEY_REAL for this command only"
  fi
fi

tmpdir="$(mktemp -d -t claude-harness-bridge-live.XXXXXX)"
proxy_pid=""
hermes_pid=""

cleanup() {
  local code=$?
  if [[ -n "${hermes_pid:-}" ]] && kill -0 "$hermes_pid" >/dev/null 2>&1; then
    kill "$hermes_pid" >/dev/null 2>&1 || true
    wait "$hermes_pid" 2>/dev/null || true
  fi
  if [[ -n "${proxy_pid:-}" ]] && kill -0 "$proxy_pid" >/dev/null 2>&1; then
    kill "$proxy_pid" >/dev/null 2>&1 || true
    wait "$proxy_pid" 2>/dev/null || true
  fi
  if ((keep_workdir)); then
    echo "kept workdir: $tmpdir"
  else
    rm -rf "$tmpdir"
  fi
  exit "$code"
}
trap cleanup EXIT

if [[ -z "$listen_addr" ]]; then
  listen_addr="$(pick_port)" || die "could not find a free localhost port"
fi
proxy_url="http://$listen_addr"
model_json="$(json_escape "$model")"

echo "building miniproxy..."
(cd "$repo_root" && go build -o "$tmpdir/miniproxy" ./cmd/miniproxy)

mkdir -p "$tmpdir/dumps"
proxy_log="$tmpdir/proxy.log"

echo "starting bridge at $proxy_url..."
(
  cd "$repo_root"
  env \
    LISTEN_ADDR="$listen_addr" \
    ANTHROPIC_BASE_URL_REAL="${real_base_url%/}" \
    ANTHROPIC_API_KEY_REAL="$token" \
    ANTHROPIC_MODEL_REAL="$model" \
    DEBUG_DUMP_DIR="$tmpdir/dumps" \
    "$tmpdir/miniproxy" direct-anthropic
) >"$proxy_log" 2>&1 &
proxy_pid=$!

wait_for_proxy "$proxy_url" "$proxy_log"

direct_request="$tmpdir/direct-request.json"
direct_response="$tmpdir/direct-response.json"
direct_headers="$tmpdir/direct-headers.txt"
cat >"$direct_request" <<EOF
{
  "model": "$model_json",
  "max_tokens": 32,
  "system": "Hermes live bridge smoke test. OpenClaw and soul.md marker text should be sanitized before upstream.",
  "messages": [
    {
      "role": "user",
      "content": "Reply with exactly BRIDGE_SMOKE_OK and no other text."
    }
  ]
}
EOF

echo "checking live text request through bridge..."
post_json "text smoke" "$proxy_url" "$direct_request" "$direct_response" "$direct_headers"
if ! grep -q '"type"[[:space:]]*:[[:space:]]*"message"' "$direct_response"; then
  echo "text smoke response did not look like an Anthropic message:" >&2
  sed -n '1,220p' "$direct_response" >&2 || true
  exit 1
fi
if ! grep -q 'BRIDGE_SMOKE_OK' "$direct_response"; then
  echo "warning: text smoke got HTTP 2xx but did not contain BRIDGE_SMOKE_OK" >&2
  sed -n '1,160p' "$direct_response" >&2 || true
fi

tool_request="$tmpdir/tool-request.json"
tool_response="$tmpdir/tool-response.json"
tool_headers="$tmpdir/tool-headers.txt"
cat >"$tool_request" <<EOF
{
  "model": "$model_json",
  "max_tokens": 64,
  "messages": [
    {
      "role": "user",
      "content": "Use the browser_navigate tool for https://example.com."
    }
  ],
  "tools": [
    {
      "name": "browser_navigate",
      "description": "Open a URL from a harness browser tool.",
      "input_schema": {
        "type": "object",
        "properties": {
          "url": {
            "type": "string",
            "format": "uri",
            "description": "Target URL"
          },
          "session_id": {
            "type": "string",
            "description": "Harness session"
          }
        },
        "required": ["url", "session_id", "missing"]
      }
    }
  ],
  "tool_choice": {
    "type": "tool",
    "name": "browser_navigate"
  }
}
EOF

echo "checking live forced-tool request through bridge..."
post_json "tool smoke" "$proxy_url" "$tool_request" "$tool_response" "$tool_headers"
if ! grep -q '"type"[[:space:]]*:[[:space:]]*"tool_use"' "$tool_response" || ! grep -q '"name"[[:space:]]*:[[:space:]]*"browser_navigate"' "$tool_response"; then
  echo "tool smoke did not return a reversed browser_navigate tool_use:" >&2
  sed -n '1,220p' "$tool_response" >&2 || true
  exit 1
fi

if ((run_hermes)); then
  hermes_home="$tmpdir/hermes-home"
  mkdir -p "$hermes_home"
  cat >"$hermes_home/config.yaml" <<EOF
model:
  default: "$model"
  provider: "anthropic"
  base_url: "$proxy_url"
platform_toolsets:
  cli: [clarify]
agent:
  max_turns: 2
EOF
  : >"$hermes_home/.env"

  echo "checking Hermes oneshot through bridge with isolated HERMES_HOME..."
  hermes_out="$tmpdir/hermes-stdout.txt"
  hermes_err="$tmpdir/hermes-stderr.txt"
  (
    env \
      HERMES_HOME="$hermes_home" \
      ANTHROPIC_TOKEN="dummy" \
      HERMES_INFERENCE_PROVIDER="anthropic" \
      hermes \
      chat \
      --provider anthropic \
      --model "$model" \
      --quiet \
      --toolsets clarify \
      --max-turns 1 \
      --ignore-rules \
      --accept-hooks \
      --query "Reply with exactly BRIDGE_AGENT_OK and no other text."
  ) >"$hermes_out" 2>"$hermes_err" &
  hermes_pid=$!
  if ! wait_for_pid_with_timeout "$hermes_pid" 180 "Hermes smoke"; then
    hermes_pid=""
    echo "Hermes smoke failed; stderr follows:" >&2
    sed -n '1,220p' "$hermes_err" >&2 || true
    echo "stdout:" >&2
    sed -n '1,220p' "$hermes_out" >&2 || true
    print_latest_bridge_error
    exit 1
  fi
  hermes_pid=""
  if ! grep -q 'BRIDGE_AGENT_OK' "$hermes_out"; then
    echo "Hermes smoke exited 0 but did not return BRIDGE_AGENT_OK" >&2
    echo "stdout:" >&2
    sed -n '1,220p' "$hermes_out" >&2 || true
    echo "stderr:" >&2
    sed -n '1,220p' "$hermes_err" >&2 || true
    print_latest_bridge_error
    exit 1
  fi
fi

echo "live smoke passed"
echo "proxy: $proxy_url"
echo "model: $model"
