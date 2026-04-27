# Claude Harness Bridge

## Use the subscription bucket you paid for, from the tools you actually use.

This is a tiny middle proxy for apps that speak Anthropic/Claude but need request sanitizing before the real upstream.

There are two modes:

```text
direct-anthropic:
OpenClaw / Claude harness -> this proxy -> real Anthropic

openai-proxy:
OpenClaw / Claude harness -> this proxy -> cliproxy / OpenAI-compatible proxy -> OpenAI, xAI, Anthropic, etc.
```

The first mode is the simple "I want OpenClaw to use Claude" path. The second mode is for putting this in front of another OpenAI-compatible proxy.

## OpenClaw With Claude

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

4. Point OpenClaw or any Anthropic-compatible harness at this proxy:

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8787"
export ANTHROPIC_API_KEY="dummy"
```

5. Use a Claude model in the harness, for example:

```text
claude-sonnet-4-6
```

In direct Anthropic mode, do not set `UPSTREAM_MODEL`. The harness should request the Claude model, and this proxy forwards that model to Anthropic unless you explicitly set `ANTHROPIC_MODEL_REAL`.

## Modes

Run direct to Anthropic:

```bash
./miniproxy direct-anthropic
```

Run through an OpenAI-compatible proxy:

```bash
./miniproxy openai-proxy
```

Flag form also works:

```bash
./miniproxy --mode direct-anthropic
./miniproxy --mode openai-proxy
```

## Direct Anthropic Mode

Flow:

```text
OpenClaw / Claude harness
  -> http://127.0.0.1:8787/v1/messages
  -> miniproxy sanitizer
  -> https://api.anthropic.com/v1/messages
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

Only set `ANTHROPIC_MODEL_REAL` if you want the proxy to force every request to that model. Otherwise the model requested by the harness passes through.

Token note: this proxy does not refresh or manage the token. It reads `ANTHROPIC_API_KEY_REAL` at startup. If the token stops working, rerun `claude setup-token`, update the env var, and restart the proxy.

## OpenAI-Compatible Proxy Mode

Use this mode when the next hop is cliproxy or another proxy that exposes OpenAI-compatible `/v1/chat/completions`.

Flow:

```text
OpenClaw / Claude harness
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

## Debug Bad Tool Schemas

If Anthropic says `tools.0.custom.input_schema` or another tool schema is invalid, run the proxy with dumps enabled:

```bash
export DEBUG_DUMP_DIR="/tmp/claude-impersonation-dumps"
./miniproxy direct-anthropic
```

Retry the failing harness request, then inspect the latest `*sanitized-anthropic-request.json` or `*anthropic-upstream-request.json` file in that directory. `tools.0` is the first item in the `tools` array.

## Library Use

The sanitizer is also available as a Go package, but most users should start with the `miniproxy` binary.
