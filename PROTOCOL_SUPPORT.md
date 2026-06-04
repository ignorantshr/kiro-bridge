# Protocol Support Matrix

Current translation coverage between OpenAI Chat Completions API and ACP (Agent Client Protocol) over JSON-RPC 2.0.

Legend: ✅ Supported | ⚠️ Partial | ❌ Not supported | 🔄 Custom handling | — Not applicable

## OpenAI Chat Completions → ACP

### Request fields

| Field | Status | Notes |
|-------|--------|-------|
| `model` | ⚠️ | Echoed in response. Not forwarded to ACP — Kiro selects model internally. |
| `messages` | ⚠️ | System + user + assistant flattened to prompt. Assistant included when `KIRO_BRIDGE_REPLAY_HISTORY` enabled. |
| `stream` | ✅ | Maps to SSE via ACP session notifications. |
| `temperature` | ❌ | No ACP equivalent. Silently ignored. |
| `top_p` | ❌ | No ACP equivalent. Silently ignored. |
| `max_tokens` | ❌ | No ACP equivalent. Silently ignored. |
| `stop` | ❌ | No ACP equivalent. Silently ignored. |
| `tools` | ❌ | ACP tools are agent-side. Client tool definitions ignored. |
| `tool_choice` | ❌ | Kiro decides tool usage autonomously. |
| `n` | ❌ | ACP returns single response. |
| `response_format` | ❌ | No ACP structured output mode. |
| `stream_options` | ❌ | No ACP usage reporting. |
| `seed` | ❌ | No ACP equivalent. |
| `user` | ❌ | Not forwarded. |

### Message types

| Type | Status | Notes |
|------|--------|-------|
| `system` | ⚠️ | Prepended as "System: " text. Loses role structure. |
| `user` (string) | ✅ | Direct mapping. |
| `user` (content array) | ⚠️ | Text parts extracted. `image_url` parts are parsed separately and forwarded when `KIRO_BRIDGE_ENABLE_IMAGES` is set; other non-text parts are dropped. |
| `assistant` | ⚠️ | Flattened into prompt text when `KIRO_BRIDGE_REPLAY_HISTORY` is enabled; otherwise dropped. |
| `tool` | ❌ | Dropped. ACP tools execute server-side. |

### Content parts

| Type | Status | Notes |
|------|--------|-------|
| `text` | ✅ | Direct mapping to ACP text ContentBlock. |
| `image_url` | ⚠️ | Parsed and forwarded as ACP image block when `KIRO_BRIDGE_ENABLE_IMAGES` set. |
| `input_audio` | ❌ | Kiro declares `audio: false`. |

### Response fields

| Field | Status | Notes |
|-------|--------|-------|
| `id` | 🔄 | Bridge-generated `chatcmpl-{ts}-{n}`. |
| `object` | 🔄 | `chat.completion` or `chat.completion.chunk`. |
| `created` | 🔄 | Bridge timestamp. |
| `model` | 🔄 | Echoed from request. |
| `choices[].message.content` | ✅ | From `agent_message_chunk`. |
| `choices[].message.tool_calls` | ❌ | Not mapped. Text annotations behind FF instead. |
| `choices[].finish_reason` | ✅ | Maps ACP stop reasons: end_turn→stop, max_tokens→length, etc. |
| `usage` | ✅ | Estimated from `_kiro.dev/metadata` contextUsagePercentage × 200k context window. |
| `system_fingerprint` | ❌ | Not generated. |

### Streaming

| Feature | Status | Notes |
|---------|--------|-------|
| SSE `data:` format | ✅ | |
| `[DONE]` sentinel | ✅ | |
| `delta.content` | ✅ | From `agent_message_chunk`. |
| `delta.role` | ✅ | `"assistant"` on first chunk. |
| `delta.tool_calls` | ❌ | Not mapped. |

## ACP → OpenAI

### Agent methods (client → agent)

| Method | Status | Notes |
|--------|--------|-------|
| `initialize` | ✅ | Declares promptCapabilities.image. |
| `authenticate` | ❌ | Not needed — kiro-cli handles auth. |
| `session/new` | ✅ | Creates session with CWD. Parses models from response. |
| `session/load` | ✅ | Implemented internally for ACP session management and tests; not exposed as HTTP API. |
| `session/prompt` | ✅ | Sends `params.content` with text content blocks and optional image blocks when `KIRO_BRIDGE_ENABLE_IMAGES` is enabled. |
| `session/set_mode` | ✅ | Activates agent config. |
| `session/list` | ❌ | Not implemented. |

### Agent notifications (client → agent)

| Notification | Status | Notes |
|-------------|--------|-------|
| `session/cancel` | ✅ | Sent when the HTTP request context is cancelled. |

### Client methods (agent → client)

| Method | Status | Notes |
|--------|--------|-------|
| `session/request_permission` | ✅ | Responds with `reject_once`. |
| `fs/read_text_file` | ❌ | Not implemented. |
| `fs/write_text_file` | ❌ | Not implemented. |
| `terminal/create` | ❌ | Out of scope. |
| `terminal/output` | ❌ | Out of scope. |
| `terminal/release` | ❌ | Out of scope. |
| `terminal/wait_for_exit` | ❌ | Out of scope. |
| `terminal/kill` | ❌ | Out of scope. |

### Session update notifications (agent → client)

| Subtype | Status | Notes |
|---------|--------|-------|
| `agent_message_chunk` / `AgentMessageChunk` | ✅ | Streamed as SSE text content. |
| `tool_call` / `ToolCall` | ⚠️ | Parsed. Text annotation behind `KIRO_BRIDGE_SHOW_TOOLS`. |
| `tool_call_update` / `ToolCallUpdate` | ⚠️ | Parsed. Not surfaced to client. |
| `turn_end` / `TurnEnd` | ✅ | Parsed. Preferred source for final stop reason. |
| `plan` | ❌ | Dropped. Never observed from Kiro. |
| `thought_message_chunk` | ❌ | Dropped. Never observed from Kiro. |
| `user_message_chunk` | ❌ | Dropped. |
| `mode_change` | ❌ | Dropped. |
| `available_commands` | ❌ | Dropped. |

### Content block types

| Type | Status | Notes |
|------|--------|-------|
| `text` | ✅ | |
| `image` | ⚠️ | Forwarded in prompts when `KIRO_BRIDGE_ENABLE_IMAGES` is enabled. ACP image responses are not surfaced. |
| `audio` | ❌ | Kiro declares unsupported. |
| `resource` (embedded) | ❌ | Kiro declares unsupported. |
| `resource_link` | ❌ | Not handled. |

### Stop reasons

| ACP Reason | OpenAI Mapping | Status |
|------------|---------------|--------|
| `end_turn` | `stop` | ✅ |
| `max_tokens` | `length` | ✅ |
| `max_turn_requests` | `stop` | ✅ |
| `refusal` | `stop` | ✅ |
| `cancelled` | `stop` | ✅ |

### Tool call fields

| Field | Status | Notes |
|-------|--------|-------|
| `toolCallId` | ✅ | Parsed. |
| `title` | ✅ | Used as tool name in annotations. |
| `kind` | ❌ | Parsed but not surfaced. |
| `status` | ✅ | Parsed. |
| `rawInput` | ✅ | Parsed as ToolInput. |
| `rawOutput` | ❌ | Not surfaced. |
| `content` (array) | ✅ | Accepts both single ContentBlock and array via json.RawMessage. |
| `locations` | ❌ | Not surfaced. |

## JSON-RPC 2.0

| Feature | Status | Notes |
|---------|--------|-------|
| Request/response | ✅ | |
| Notifications (send) | ✅ | `session/cancel` sent on cancel. |
| Notifications (receive) | ✅ | `session/update` and `session/notification` handled. |
| Error object (code + message + data) | ✅ | |
| ID as integer | ✅ | Used for bridge → agent requests. |
| ID as string | ✅ | Handled for agent → client requests. |
| Batch requests | ❌ | Not needed for stdio. |
| Method not found error (-32601) | ✅ | Responds -32601 for unhandled agent requests. |

## Actionable gaps (priority order)

No remaining protocol gaps for the current HTTP surface. `session/load` is implemented internally but not exposed as a public HTTP session API by design.

## Known limitations

### Client-defined tools not supported

OpenAI's tool protocol is **bidirectional** — model proposes a tool call, client executes it, client returns the result. ACP's tool protocol is **unidirectional** — the agent executes tools internally, the client only observes.

These cannot be cleanly bridged. Client-defined tools (e.g. Raycast's `@calculator`, `@location`) require the model to output structured `tool_calls` that the client executes. Kiro doesn't know about client tools and has no mechanism to invoke them.

Kiro's own tools (read, grep, glob, web_search, etc.) run transparently inside the ACP session. They can be surfaced as text annotations via `KIRO_BRIDGE_SHOW_TOOLS` but cannot be rendered as interactive tool call UI in OpenAI-compatible clients.

**Raycast config:** Set `tools.supported: false` to avoid errors when using tool-dependent extensions.
