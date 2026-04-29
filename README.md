# Claude Harness Bridge

## Use the subscription bucket you paid for, from the tools you actually use.

This is a tiny middle proxy for apps that speak Anthropic/Claude but need request sanitizing before the real upstream.

There are two modes:

```text
direct-anthropic:
OpenClaw / Hermes / Claude harness -> this proxy -> real Anthropic

openai-proxy:
OpenClaw / Hermes / Claude harness -> this proxy -> cliproxy / OpenAI-compatible proxy -> OpenAI, xAI, Anthropic, etc.
```

The first mode is the simple "I want OpenClaw to use Claude" path. The second mode is for putting this in front of another OpenAI-compatible proxy.

## Requirements

Supported runtime: Linux, macOS, or WSL.

You need:

- Git
- Go 1.22 or newer
- Claude Code installed, so you can run `claude setup-token`
- OpenClaw, Hermes, or another Anthropic-compatible harness

If Go is missing, install it from https://go.dev/dl/ or with your system package manager.

If Claude Code is missing, follow Anthropic's setup docs: https://docs.anthropic.com/en/docs/claude-code/getting-started

## Hermes/OpenClaw/... With Claude

1. Build the proxy:

```bash
git clone https://github.com/zoidypuh/claude-code-impersonation.git
cd claude-code-impersonation
go build -o miniproxy ./cmd/miniproxy
```

2. Get a Claude OAuth token:

```bash
claude setup-token
```

Copy the token it prints.

3. Start this proxy in direct Anthropic mode:

```bash
export ANTHROPIC_BASE_URL_REAL="https://api.anthropic.com/v1"
export ANTHROPIC_API_KEY_REAL="paste-the-claude-setup-token-here"

./miniproxy direct-anthropic
```

4. In the terminal where you start OpenClaw, Hermes, or your harness, point it at this proxy:

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8787"
export ANTHROPIC_API_KEY="dummy"
```

In Hermes/OpenClaw settings, choose an OpenAI-compatible endpoint as the provider/backend, not the built-in Anthropic provider. Use this proxy URL as the endpoint and a dummy key; this proxy handles the real upstream token.

5. Use a Claude model in the harness, for example:

```text
claude-sonnet-4-6
```

## Direct Anthropic Mode (this is probably what you want)

Flow:

```text
OpenClaw / Hermes / Claude harness
  -> http://127.0.0.1:8787/v1/messages
  -> miniproxy sanitizer
  -> https://api.anthropic.com/v1/messages
```

Run direct to Anthropic:

```bash
./miniproxy direct-anthropic
```

Environment used by this proxy:

```bash
export ANTHROPIC_BASE_URL_REAL="https://api.anthropic.com/v1"
export ANTHROPIC_API_KEY_REAL="sk-ant-oat..."
```

Optional:

```bash
export ANTHROPIC_MODEL_REAL="claude-sonnet-4-6"
```

In direct Anthropic mode, do not set `UPSTREAM_MODEL`. The harness should request the Claude model, and this proxy forwards that model to Anthropic unless you explicitly set `ANTHROPIC_MODEL_REAL`.

Only set `ANTHROPIC_MODEL_REAL` if you want the proxy to force every request to that model. Otherwise the model requested by the harness passes through.

Token note: this proxy does not refresh or manage the token. It reads `ANTHROPIC_API_KEY_REAL` at startup. If the token stops working, rerun `claude setup-token`, update the env var, and restart the proxy.

## OpenAI-Compatible Proxy Mode (if you use a second proxy to rotate keys/track usage/etc)

Use this mode when the next hop is cliproxy or another proxy that exposes OpenAI-compatible `/v1/chat/completions`.

Flow:

```text
OpenClaw / Hermes / Claude harness
  -> http://127.0.0.1:8787/v1/messages
  -> miniproxy sanitizer + Anthropic-to-OpenAI conversion
  -> http://127.0.0.1:4000/v1/chat/completions
  -> your main proxy/provider
```

Start it:

```bash
export UPSTREAM_BASE_URL="http://127.0.0.1:4000/v1"
export UPSTREAM_API_KEY="real-key-for-that-upstream"
export UPSTREAM_MODEL="model-name-your-upstream-accepts"

./miniproxy openai-proxy
```

`UPSTREAM_MODEL` is optional, but usually useful in this mode. It forces the OpenAI request model to whatever your next proxy accepts. If the next proxy exposes `claude-sonnet-4-6`, set that. If it exposes an xAI/OpenAI/local model, set that model instead.

## Client Settings

These are for OpenClaw, Claude Code, or whichever app is making Anthropic requests to this proxy:

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8787"
export ANTHROPIC_API_KEY="dummy"
```

If your client has a provider picker, choose OpenAI-compatible endpoint. Do not select the built-in Anthropic provider for the Hermes/OpenClaw harness path.

The client-side `ANTHROPIC_API_KEY` can be a dummy value because the proxy uses `ANTHROPIC_API_KEY_REAL` or `UPSTREAM_API_KEY` for the real upstream call.

## Models Endpoint

The proxy exposes:

```text
GET http://127.0.0.1:8787/v1/models
```

In `openai-proxy` mode, it tries to pass through `GET /v1/models` from your upstream OpenAI-compatible proxy.

In `direct-anthropic` mode, it tries `GET /v1/models` from the real Anthropic upstream. If that fails, it returns a small fallback list containing `ANTHROPIC_MODEL_REAL` or `claude-sonnet-4-6`.

## What Gets Sanitized

- Adds Claude-Code-like system/billing blocks.
- Adds Claude-Code-like metadata `user_id`.
- Moves body `betas` into request-local state used for headers.
- Normalizes OAuth-style request defaults such as adaptive thinking and context management.
- Renames non-Claude-Code tools to Claude-Code-like tool names before the upstream call.
- Reverses those tool names on the way back to your harness.
- Strips tool descriptions and some obvious harness-specific schema property names, including nested `custom.input_schema` tools from newer Claude Code builds.
- Normalizes tool JSON Schemas enough to avoid common upstream rejects: missing object roots, stale `$schema` / `$id`, invalid `required`, bad `format`, and draft-04 boolean exclusive min/max.
- Preserves short persona/system prompts.
- Replaces huge third-party harness system templates with a short neutral software-engineering reminder.
- Obfuscates Hermes/OpenClaw identifying terms in outbound prompt text, including Hermes agent names, `soul.md`, and OpenClaw markers.
- Preserves `thinking` and `redacted_thinking` blocks byte-for-byte when text replacements are configured.

## Environment Variables

Common:

- `LISTEN_ADDR`: proxy listen address. Default: `127.0.0.1:8787`.
- `CLAUDE_CODE_OAUTH_SHAPE`: default `true`.
- `SIGN_CCH`: default `true`.
- `ADD_FAKE_USER_ID`: default `true`.
- `CORE_PROMPT`: optional sanitizer prompt override.
- `DEBUG_DUMP_DIR`: optional local directory for sanitized request and upstream response JSON dumps. Use only while debugging because dumps may contain prompts, tool inputs, and keys from upstream error bodies.

Direct Anthropic mode:

- `ANTHROPIC_BASE_URL_REAL`: real Anthropic base URL. Default: `https://api.anthropic.com/v1`.
- `ANTHROPIC_API_KEY_REAL`: real Claude OAuth/API token.
- `ANTHROPIC_MODEL_REAL`: optional forced Claude model.

OpenAI-compatible proxy mode:

- `UPSTREAM_BASE_URL`: next proxy base URL, usually ending in `/v1`.
- `UPSTREAM_API_KEY`: key for the next proxy.
- `UPSTREAM_MODEL`: optional forced model for the next proxy.

Backward-compatible aliases still accepted:

- `UPSTREAM_TYPE=anthropic` is treated as `direct-anthropic`.
- `UPSTREAM_TYPE=openai` is treated as `openai-proxy`.
- `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_AUTH_TOKEN_REAL`, and `ANTHROPIC_UPSTREAM_TOKEN` are accepted as token aliases.

## Test

```bash
go test ./...
```

## Live Smoke Test

To check the real Anthropic/Hermes path without changing your normal shell
environment, run:

```bash
scripts/live-smoke-hermes.sh --token "$(claude setup-token)"
```

Requirements:

- Go 1.22 or newer
- `curl`
- Hermes installed and available as `hermes`
- a fresh Claude setup token from `claude setup-token`

What the script does:

1. Builds `miniproxy` into a temporary directory.
2. Starts the bridge on a random `127.0.0.1` port in `direct-anthropic` mode.
3. Sends a live text request through `/v1/messages`.
4. Sends a live forced-tool request and verifies the bridge reverses the sanitized tool name back to the client-facing name.
5. Runs `hermes chat --query` through the bridge with an isolated temporary `HERMES_HOME`.
6. Stops the temporary proxy and removes temp files, unless `--keep-workdir` is set.

The token is not written into the repo or your parent shell environment. It is
only passed to the temporary proxy process for the smoke test. The Hermes check
uses a temp config and a minimal toolset so it does not depend on your normal
Hermes settings.

Successful output ends with:

```text
live smoke passed
proxy: http://127.0.0.1:<port>
model: claude-sonnet-4-6
```

Useful options:

```bash
scripts/live-smoke-hermes.sh --model claude-sonnet-4-6
scripts/live-smoke-hermes.sh --skip-hermes --keep-workdir
```

Use `--skip-hermes` when you only want to check the bridge's direct Anthropic
path. Use `--keep-workdir` when debugging; it preserves proxy logs, request
dumps, and response bodies under `/tmp/claude-harness-bridge-live.*`.

## Debug Bad Tool Schemas

If Anthropic says `tools.0.custom.input_schema` or another tool schema is invalid, run the proxy with dumps enabled:

```bash
export DEBUG_DUMP_DIR="/tmp/claude-impersonation-dumps"
./miniproxy direct-anthropic
```

Retry the failing harness request, then inspect the latest `*sanitized-anthropic-request.json` or `*anthropic-upstream-request.json` file in that directory. `tools.0` is the first item in the `tools` array.

## Library Use

The sanitizer is also available as a Go package, but most users should start with the `miniproxy` binary.
