# AGENTS.md

## Quick orientation

Zero-dependency Go HTTP server. Translates OpenAI API → Kiro ACP (JSON-RPC over stdio). Any OpenAI-compatible client can use Kiro as a backend.

## Changing behaviour

- HTTP handling → `handler.go`
- ACP protocol / kiro-cli lifecycle → `bridge.go`
- OpenAI request/response types → `openai.go`
- JSON-RPC / ACP types → `jsonrpc.go`
- Server setup, logging, signal handling → `main.go`
- Allowed tools / model selection → `agent.json` (security-sensitive, see below)

## Before you commit

- `go vet ./... && go test ./...`
- Conventional Commits format
- No ignored error returns (`_, _`)

## Writing tests

- Test file mirrors source: `handler.go` → `handler_test.go`
- Table-driven tests for multiple input scenarios
- Every function: test what it should do AND what it should NOT do
- DI via `Bridge` interface → `mockBridge` in tests
- Benchmarks in `bench_test.go`
- E2E tests: `go test -tags e2e -timeout 60s ./...`

## Security decisions

- `agent.json` controls what tools Kiro can use unsupervised. Changing this is a security change.

## Going deeper

Architecture decisions and Go conventions are in `.kiro/` (gitignored, local only).
