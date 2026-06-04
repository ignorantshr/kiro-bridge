# kiro-bridge

Bring Kiro to every AI tool in your workflow — Raycast, Continue, Open WebUI, and anything that speaks the OpenAI API.

```
Client  ──POST /v1/chat/completions──▶  kiro-bridge  ──JSON-RPC/stdio──▶  kiro-cli acp
        ◀──SSE stream────────────────               ◀──session/update────
```

kiro-bridge is a lightweight HTTP server that translates between the [OpenAI Chat Completions API](https://platform.openai.com/docs/api-reference/chat) and Kiro's [Agent Client Protocol (ACP)](https://agentclientprotocol.com). The ACP v1 schema reference used by this project is https://agentclientprotocol.com/protocol/v1/schema, but runtime behavior follows `kiro-cli` when the published schema and the actual CLI diverge. It spawns `kiro-cli acp` as a backend, so you get everything Kiro offers — models, tools, file access, web search — streamed back through a standard OpenAI endpoint that any client can consume.

## Quick start

### 1. Install

Download a prebuilt binary from [Releases](https://github.com/ignorantshr/kiro-bridge/releases):

```bash
# macOS Apple Silicon
curl -L https://github.com/ignorantshr/kiro-bridge/releases/latest/download/kiro-bridge_darwin_arm64.tar.gz | tar xz
```

On macOS, remove the quarantine flag before running:
```bash
xattr -d com.apple.quarantine kiro-bridge
```

Or build from source:

```bash
go build -ldflags "-X main.version=$(cat .version)" -o kiro-bridge .
```

### 2. Install the agent config

Copy the agent config to your Kiro agents directory:

```bash
mkdir -p ~/.kiro/agents
cp agent.json ~/.kiro/agents/kiro-bridge.json
```

This pre-approves read-only tools (`fs_read`, `grep`, `glob`, `web_search`) so Kiro can use them without prompting for approval in headless mode.

Optionally, add `resources` to load steering docs into every session:

```json
"resources": [
  "file://~/.kiro/steering/coding.md",
  "file://~/.kiro/steering/workflow.md"
]
```

### 3. Configure `.env`

```bash
cp .env.example .env
```

Edit `.env` as needed. The bridge loads `.env` from the current working directory automatically. Real environment variables still win if both are set. If you need a different dotenv path, set `KIRO_BRIDGE_ENV_FILE=/absolute/path/to/.env` in the parent process environment, not inside `.env` itself.

### 4. Run it

```bash
./kiro-bridge

# or run without building a binary
go run .
```

That's it — the bridge is running at `http://127.0.0.1:11435/v1`. No background service needed to try it out. See [Running as a background service](#running-as-a-background-service) when you want it always-on.

### 5. Connect your client

The bridge exposes an OpenAI-compatible API at `http://127.0.0.1:11435/v1`. Point any OpenAI-compatible client at it.

Example for Raycast — add to `~/.config/raycast/ai/providers.yaml`:

```yaml
providers:
  - id: kiro
    name: Kiro
    base_url: http://localhost:11435/v1
    api_key: change-me
    models:
      - id: kiro
        name: "Kiro (Claude via ACP)"
        context: 218000
        abilities:
          temperature:
            supported: false
          vision:
            supported: false
          system_message:
            supported: true
          tools:
            supported: false
```

## Features

- **OpenAI-compatible API** — `POST /v1/chat/completions` with streaming (SSE) and non-streaming responses
- **Dynamic model list** — `GET /v1/models` serves real models from Kiro (Claude Opus, Sonnet, Haiku, DeepSeek, and more)
- **Vision support** — forward images from OpenAI `image_url` content to Kiro (experimental)
- **Tool transparency** — Kiro's tools (file search, grep, web search) run inside the ACP session with optional annotations
- **Ordered transcript projection** — preserve OpenAI message order and content-part ordering when projecting requests into ACP prompt blocks
- **Token usage estimation** — approximate token counts from Kiro's context usage metadata
- **Health endpoint** — `GET /healthz` reports ACP process readiness
- **Resilient** — supervised `kiro-cli` restarts, per-request session isolation by default, graceful cancellation

> **Tool permissions:** `session/request_permission` is rejected by default with `reject_once`. Pre-approved tools in the agent config still bypass the prompt entirely.

## Configuration

All configuration is via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `KIRO_BRIDGE_PORT` | `11435` | HTTP server port |
| `KIRO_BRIDGE_API_KEY` | none, required | Bearer token required on `/v1/*` requests |
| `KIRO_BRIDGE_ENV_FILE` | `.env` in current working directory | Optional dotenv path, but it must be set in the parent process environment because it decides which file gets loaded |
| `KIRO_BRIDGE_CWD` | current directory | Working directory for ACP sessions |
| `KIRO_CLI_PATH` | `kiro-cli` | Path to kiro-cli binary |
| `KIRO_BRIDGE_AGENT` | `kiro-bridge` | Kiro agent config to activate |
| `KIRO_BRIDGE_MAX_BODY` | `1048576` | Max request body size in bytes (default 1MB) |
| `KIRO_BRIDGE_VERBOSE` | unset | Set to enable debug logging |
| `KIRO_BRIDGE_SHOW_TOOLS` | unset | Set to show tool call annotations in responses (experimental) |
| `KIRO_BRIDGE_SESSION_MODE` | `per_request` | ACP session strategy: `per_request` creates a new session for each HTTP request, `shared` reuses one session |
| `KIRO_BRIDGE_ALL_IP` | unset | Set to bind the HTTP server to `0.0.0.0` instead of `127.0.0.1` |
| `KIRO_BRIDGE_CONTEXT_WINDOW` | `218000` | Context window size for token usage estimation |

## Running as a background service

### macOS launchd plist

Create `~/Library/LaunchAgents/com.kiro-bridge.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.kiro-bridge</string>
  <key>Program</key>
  <string>/path/to/kiro-bridge</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>KIRO_BRIDGE_ENV_FILE</key>
    <string>/Users/you/path/to/kiro-bridge/.env</string>
    <key>KIRO_BRIDGE_CWD</key>
    <string>/Users/you</string>
    <key>KIRO_CLI_PATH</key>
    <string>/path/to/kiro-cli</string>
  </dict>
  <key>KeepAlive</key>
  <true/>
  <key>RunAtLoad</key>
  <true/>
  <key>StandardOutPath</key>
  <string>/tmp/kiro-bridge.log</string>
  <key>StandardErrorPath</key>
  <string>/tmp/kiro-bridge.log</string>
</dict>
</plist>
```

Load with:

```bash
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.kiro-bridge.plist
```

## Performance

The bridge adds ~5µs of overhead per request. Model inference (500ms-5s) dominates latency by 100,000x.

| Operation | Time | Allocs |
|-----------|------|--------|
| Full stream handler (4 chunks) | 5.2µs | 85 |
| Full non-stream handler | 2.8µs | 54 |
| JSON-RPC request serialize | 0.5µs | 4 |
| Content unmarshal (string) | 0.2µs | 5 |

Run benchmarks with `go test -bench=. -benchmem ./...`

## Development

```bash
# run tests
go test -v ./...

# run e2e tests (requires authenticated kiro-cli)
go test -tags e2e -timeout 60s -v ./...
```

## Release

```bash
version="$(tr -d '[:space:]' < .version)"
git tag -a "v$version" -m "Release v$version"
git push origin "$(git branch --show-current)" --tags
```

## How it works

- The bridge spawns `kiro-cli acp` as a child process and communicates via JSON-RPC over stdio.
- On startup it loads configuration from `.env` in the current working directory, unless `KIRO_BRIDGE_ENV_FILE` points to a different file. Existing process environment variables override dotenv values.
- Public OpenAI-compatible endpoints require `Authorization: Bearer <KIRO_BRIDGE_API_KEY>`. `/healthz` stays unauthenticated for liveness checks.
- On startup failure or child-process exit, it retries with exponential backoff (1s→60s cap) instead of crashing. The HTTP server starts immediately and returns 503 while connecting or reconnecting.
- It keeps one supervised ACP process alive and, by default, creates a fresh ACP session for each HTTP request. Set `KIRO_BRIDGE_SESSION_MODE=shared` to reuse a single ACP session instead.
- Incoming OpenAI `/v1/chat/completions` requests are translated to ACP `session/prompt` calls using `params.prompt`.
- ACP `agent_message_chunk` / `AgentMessageChunk` notifications are streamed back as OpenAI SSE chunks, and `TurnEnd` is used when available to determine the final stop reason.
- Kiro tool calls (file search, web fetch, etc.) happen transparently inside the ACP session — only the final text response is returned to the client.
- When the HTTP client disconnects or times out, the bridge forwards `session/cancel` to ACP.
- When Kiro requests permission for write tools, the bridge answers `reject_once` by default. Pre-approved tools in the agent config bypass this.
- OpenAI messages are parsed according to the Chat Completions message/content schema first, then projected into ordered ACP content blocks so multi-turn history and mixed content-part ordering are preserved as much as ACP allows.
- OpenAI `image_url` content is forwarded as ACP image blocks when the negotiated prompt capabilities include image.
- OpenAI `input_audio` content is forwarded as ACP audio blocks when the negotiated prompt capabilities include audio; otherwise it falls back to tagged text.
- OpenAI file parts with inline `file_data` are forwarded as embedded ACP resources when the negotiated prompt capabilities include embedded context; otherwise they fall back to tagged text.

---

Built by [ignorantshr](https://github.com/ignorantshr) with Codex and for the ❤️ of useful software.
