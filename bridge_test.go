package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestNewBridgeFailsWhenSetModeFails(t *testing.T) {
	oldExecCommand := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		cs := []string{"-test.run=TestBridgeHelperProcess", "--"}
		cs = append(cs, args...)
		cmd := exec.Command(os.Args[0], cs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"KIRO_BRIDGE_HELPER_MODE=set-mode-error",
		)
		return cmd
	}
	defer func() { execCommand = oldExecCommand }()

	b, err := NewBridge(BridgeConfig{
		CLIPath: "kiro-cli",
		CWD:     ".",
		Agent:   "kiro-bridge",
		Version: "test",
	})
	if err == nil {
		if b != nil {
			b.Close()
		}
		t.Fatal("expected NewBridge to fail")
	}
	if !strings.Contains(err.Error(), "set mode") {
		t.Fatalf("error = %q, want set mode context", err)
	}
}

func TestBridgeHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	mode := os.Getenv("KIRO_BRIDGE_HELPER_MODE")
	countFile := os.Getenv("KIRO_BRIDGE_HELPER_COUNT_FILE")
	expectedPermissionOption := os.Getenv("KIRO_BRIDGE_EXPECT_PERMISSION_OPTION")
	scanner := bufio.NewScanner(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	sessionCount := 0
	promptCount := 0
	appendCount := func(value string) {
		if countFile == "" {
			return
		}
		f, err := os.OpenFile(countFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if _, err := f.WriteString(value + "\n"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err := f.Close(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}

	for scanner.Scan() {
		var req Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}

		var resp any
		switch req.Method {
		case "initialize":
			resp = map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"protocolVersion": 1,
					"agentCapabilities": map[string]any{
						"loadSession": true,
						"promptCapabilities": map[string]any{
							"image":           true,
							"audio":           true,
							"embeddedContext": true,
						},
					},
				},
			}
		case "session/new":
			sessionCount++
			if mode == "session-ids" {
				resp = map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]any{
						"sessionId": fmt.Sprintf("sess-%d", sessionCount),
					},
				}
				break
			}
			if mode == "with-models" {
				resp = map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]any{
						"sessionId": "sess-1",
						"models": map[string]any{
							"currentModelId": "claude-sonnet-4",
							"availableModels": []map[string]any{
								{"modelId": "auto", "name": "auto", "description": "Auto"},
								{"modelId": "claude-sonnet-4", "name": "claude-sonnet-4", "description": "Sonnet 4"},
							},
						},
					},
				}
				break
			}
			resp = map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"sessionId": "sess-1",
				},
			}
		case "session/set_mode":
			if mode == "set-mode-error" {
				resp = map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"error": map[string]any{
						"code":    -32000,
						"message": "mode activation failed",
					},
				}
				break
			}
			resp = map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]any{},
			}
		case "session/load":
			resp = map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"sessionId": "loaded-session",
				},
			}
		case "session/cancel":
			appendCount("cancel")
			continue
		case "session/prompt":
			promptCount++
			if mode == "crash-on-prompt" {
				os.Exit(1)
			}
			if mode == "prompt-field" {
				var params map[string]json.RawMessage
				if err := json.Unmarshal(req.Params, &params); err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(2)
				}
				if _, ok := params["prompt"]; !ok {
					fmt.Fprintln(os.Stderr, "missing prompt field")
					os.Exit(2)
				}
				if _, ok := params["content"]; ok {
					fmt.Fprintln(os.Stderr, "unexpected content field")
					os.Exit(2)
				}
				resp = map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result":  map[string]any{"stopReason": "end_turn"},
				}
				break
			}
			if mode == "session-ids" {
				var params SessionPromptParams
				if err := json.Unmarshal(req.Params, &params); err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(2)
				}
				chunk := map[string]any{
					"jsonrpc": "2.0",
					"method":  "session/update",
					"params": map[string]any{
						"sessionId": params.SessionID,
						"update": map[string]any{
							"sessionUpdate": "agent_message_chunk",
							"content":       map[string]any{"type": "text", "text": params.SessionID},
						},
					},
				}
				d, _ := json.Marshal(chunk)
				writer.Write(append(d, '\n'))
				writer.Flush()
				resp = map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result":  map[string]any{"stopReason": "end_turn"},
				}
				break
			}
			if mode == "with-metadata" {
				chunk := map[string]any{
					"jsonrpc": "2.0",
					"method":  "session/update",
					"params": map[string]any{
						"sessionId": "sess-1",
						"update": map[string]any{
							"sessionUpdate": "agent_message_chunk",
							"content":       map[string]any{"type": "text", "text": "hi"},
						},
					},
				}
				d, _ := json.Marshal(chunk)
				writer.Write(append(d, '\n'))

				meta := map[string]any{
					"jsonrpc": "2.0",
					"method":  "_kiro.dev/metadata",
					"params": map[string]any{
						"sessionId":              "sess-1",
						"contextUsagePercentage": 1.5,
						"turnDurationMs":         3000,
					},
				}
				d, _ = json.Marshal(meta)
				writer.Write(append(d, '\n'))
				writer.Flush()

				resp = map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result":  map[string]any{"stopReason": "end_turn"},
				}
				break
			}
			if mode == "error-then-ok" {
				if promptCount <= 3 {
					resp = map[string]any{
						"jsonrpc": "2.0",
						"id":      req.ID,
						"error":   map[string]any{"code": -32603, "message": "Internal error"},
					}
					break
				}
				chunk := map[string]any{
					"jsonrpc": "2.0",
					"method":  "session/update",
					"params": map[string]any{
						"sessionId": "sess-1",
						"update": map[string]any{
							"sessionUpdate": "agent_message_chunk",
							"content":       map[string]any{"type": "text", "text": "ok"},
						},
					},
				}
				d, _ := json.Marshal(chunk)
				writer.Write(append(d, '\n'))
				writer.Flush()
				resp = map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result":  map[string]any{"stopReason": "end_turn"},
				}
				break
			}
			if mode == "turn-end" {
				turnEnd := map[string]any{
					"jsonrpc": "2.0",
					"method":  "session/notification",
					"params": map[string]any{
						"sessionId": "sess-1",
						"update": map[string]any{
							"sessionUpdate": "TurnEnd",
							"stopReason":    "max_tokens",
						},
					},
				}
				d, _ := json.Marshal(turnEnd)
				writer.Write(append(d, '\n'))
				writer.Flush()
				resp = map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result":  map[string]any{"stopReason": "end_turn"},
				}
				break
			}
			if mode == "slow-prompt" {
				// Send first chunk
				chunk := map[string]any{
					"jsonrpc": "2.0",
					"method":  "session/update",
					"params": map[string]any{
						"sessionId": "sess-1",
						"update": map[string]any{
							"sessionUpdate": "agent_message_chunk",
							"content":       map[string]any{"type": "text", "text": "working..."},
						},
					},
				}
				d, _ := json.Marshal(chunk)
				writer.Write(append(d, '\n'))
				writer.Flush()

				// Wait for cancel notification from bridge
				if scanner.Scan() {
					// Got cancel — respond with cancelled stop reason
					resp = map[string]any{
						"jsonrpc": "2.0",
						"id":      req.ID,
						"result":  map[string]any{"stopReason": "cancelled"},
					}
				}
				break
			}
			if mode == "cancel-hangs" {
				chunk := map[string]any{
					"jsonrpc": "2.0",
					"method":  "session/update",
					"params": map[string]any{
						"sessionId": "sess-1",
						"update": map[string]any{
							"sessionUpdate": "agent_message_chunk",
							"content":       map[string]any{"type": "text", "text": "working..."},
						},
					},
				}
				d, _ := json.Marshal(chunk)
				writer.Write(append(d, '\n'))
				writer.Flush()
				continue
			}
			if mode == "response-shuffle" {
				extra := map[string]any{
					"jsonrpc": "2.0",
					"id":      999,
					"result":  map[string]any{"stopReason": "wrong"},
				}
				d, _ := json.Marshal(extra)
				writer.Write(append(d, '\n'))

				chunk := map[string]any{
					"jsonrpc": "2.0",
					"method":  "session/update",
					"params": map[string]any{
						"sessionId": "sess-1",
						"update": map[string]any{
							"sessionUpdate": "agent_message_chunk",
							"content":       map[string]any{"type": "text", "text": "ok"},
						},
					},
				}
				d, _ = json.Marshal(chunk)
				writer.Write(append(d, '\n'))
				writer.Flush()

				resp = map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result":  map[string]any{"stopReason": "end_turn"},
				}
				break
			}
			if mode == "bad-json" {
				writer.WriteString("not json\n")
				writer.Flush()
				time.Sleep(10 * time.Second)
				continue
			}
			if mode == "unknown-method" {
				// Send an unknown agent→client request
				unknownReq := map[string]any{
					"jsonrpc": "2.0",
					"id":      "unknown-1",
					"method":  "fs/read_text_file",
					"params":  map[string]any{"path": "/tmp/test.txt"},
				}
				d, _ := json.Marshal(unknownReq)
				writer.Write(append(d, '\n'))
				writer.Flush()

				// Wait for bridge to respond
				if !scanner.Scan() {
					os.Exit(2)
				}

				// Send text chunk and prompt response
				chunk := map[string]any{
					"jsonrpc": "2.0",
					"method":  "session/update",
					"params": map[string]any{
						"sessionId": "sess-1",
						"update": map[string]any{
							"sessionUpdate": "agent_message_chunk",
							"content":       map[string]any{"type": "text", "text": "ok"},
						},
					},
				}
				d, _ = json.Marshal(chunk)
				writer.Write(append(d, '\n'))
				writer.Flush()

				resp = map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result":  map[string]any{"stopReason": "end_turn"},
				}
				break
			}
			if mode == "tool-call-with-meta" {
				toolCall := map[string]any{
					"jsonrpc": "2.0",
					"method":  "session/update",
					"params": map[string]any{
						"sessionId": "sess-1",
						"update": map[string]any{
							"sessionUpdate": "tool_call",
							"toolCallId":    "call_meta",
							"title":         "Finding *.go",
							"kind":          "search",
							"_meta":         map[string]any{"tool_name": "glob"},
						},
					},
				}
				d, _ := json.Marshal(toolCall)
				writer.Write(append(d, '\n'))

				chunk := map[string]any{
					"jsonrpc": "2.0",
					"method":  "session/update",
					"params": map[string]any{
						"sessionId": "sess-1",
						"update": map[string]any{
							"sessionUpdate": "agent_message_chunk",
							"content":       map[string]any{"type": "text", "text": "done"},
						},
					},
				}
				d, _ = json.Marshal(chunk)
				writer.Write(append(d, '\n'))
				writer.Flush()

				resp = map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result":  map[string]any{"stopReason": "end_turn"},
				}
				break
			}
			if mode == "tool-call" {
				// Send tool_call notification
				toolCall := map[string]any{
					"jsonrpc": "2.0",
					"method":  "session/update",
					"params": map[string]any{
						"sessionId": "sess-1",
						"update": map[string]any{
							"sessionUpdate": "tool_call",
							"toolCallId":    "call_abc",
							"title":         "Reading main.go",
							"kind":          "read",
							"rawInput":      map[string]any{"path": "main.go"},
						},
					},
				}
				d, _ := json.Marshal(toolCall)
				writer.Write(append(d, '\n'))

				// Send tool_call_update notification
				toolUpdate := map[string]any{
					"jsonrpc": "2.0",
					"method":  "session/update",
					"params": map[string]any{
						"sessionId": "sess-1",
						"update": map[string]any{
							"sessionUpdate": "tool_call_update",
							"toolCallId":    "call_abc",
							"status":        "completed",
						},
					},
				}
				d, _ = json.Marshal(toolUpdate)
				writer.Write(append(d, '\n'))

				// Send text chunk
				chunk := map[string]any{
					"jsonrpc": "2.0",
					"method":  "session/update",
					"params": map[string]any{
						"sessionId": "sess-1",
						"update": map[string]any{
							"sessionUpdate": "agent_message_chunk",
							"content":       map[string]any{"type": "text", "text": "file contents here"},
						},
					},
				}
				d, _ = json.Marshal(chunk)
				writer.Write(append(d, '\n'))
				writer.Flush()

				resp = map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result":  map[string]any{"stopReason": "end_turn"},
				}
				break
			}
			if mode == "permission-request" {
				// Send a permission request (agent→client) before responding
				permReq := map[string]any{
					"jsonrpc": "2.0",
					"id":      "perm-uuid-001",
					"method":  "session/request_permission",
					"params": map[string]any{
						"sessionId": "sess-1",
						"toolCall":  map[string]any{"toolCallId": "call_1", "title": "Writing file"},
						"options": []map[string]any{
							{"optionId": "allow_once", "name": "Yes", "kind": "allow_once"},
							{"optionId": "reject_once", "name": "No", "kind": "reject_once"},
						},
					},
				}
				permData, _ := json.Marshal(permReq)
				writer.Write(append(permData, '\n'))
				writer.Flush()

				// Wait for the bridge to respond to the permission request
				if !scanner.Scan() {
					os.Exit(2)
				}
				var permResp Response
				if err := json.Unmarshal(scanner.Bytes(), &permResp); err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(2)
				}
				var permResult RequestPermissionResult
				if err := json.Unmarshal(permResp.Result, &permResult); err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(2)
				}
				if expectedPermissionOption != "" && permResult.Outcome.OptionID != expectedPermissionOption {
					fmt.Fprintf(os.Stderr, "permission option = %q, want %q\n", permResult.Outcome.OptionID, expectedPermissionOption)
					os.Exit(2)
				}

				// Now send a message chunk and the prompt response
				chunk := map[string]any{
					"jsonrpc": "2.0",
					"method":  "session/update",
					"params": map[string]any{
						"sessionId": "sess-1",
						"update": map[string]any{
							"sessionUpdate": "agent_message_chunk",
							"content":       map[string]any{"type": "text", "text": "done"},
						},
					},
				}
				chunkData, _ := json.Marshal(chunk)
				writer.Write(append(chunkData, '\n'))
				writer.Flush()

				resp = map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result":  map[string]any{"stopReason": "end_turn"},
				}
				break
			}
			resp = map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]any{"stopReason": "end_turn"},
			}
			resp = map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]any{},
			}
		}

		data, err := json.Marshal(resp)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if _, err := writer.Write(append(data, '\n')); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err := writer.Flush(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}

	os.Exit(0)
}

func TestNewBridgeSetModeErrorIncludesCode(t *testing.T) {
	oldExecCommand := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		cs := []string{"-test.run=TestBridgeHelperProcess", "--"}
		cs = append(cs, args...)
		cmd := exec.Command(os.Args[0], cs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"KIRO_BRIDGE_HELPER_MODE=set-mode-error",
		)
		return cmd
	}
	defer func() { execCommand = oldExecCommand }()

	_, err := NewBridge(BridgeConfig{
		CLIPath: "kiro-cli",
		CWD:     ".",
		Agent:   "kiro-bridge",
		Version: "test",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "code -32000") {
		t.Fatalf("error = %q, want error code -32000", err)
	}
	if !strings.Contains(err.Error(), "mode activation failed") {
		t.Fatalf("error = %q, want message", err)
	}
}

func TestBridgeDefaultsToRejectPermissionDecision(t *testing.T) {
	oldExecCommand := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		cs := []string{"-test.run=TestBridgeHelperProcess", "--"}
		cs = append(cs, args...)
		cmd := exec.Command(os.Args[0], cs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"KIRO_BRIDGE_HELPER_MODE=permission-request",
			"KIRO_BRIDGE_EXPECT_PERMISSION_OPTION=reject_once",
		)
		return cmd
	}
	defer func() { execCommand = oldExecCommand }()

	b, err := NewBridge(BridgeConfig{
		CLIPath: "kiro-cli",
		CWD:     ".",
		Agent:   "kiro-bridge",
		Version: "test",
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer b.Close()

	var chunks []string
	_, err = b.Prompt(context.Background(), []ContentBlock{{Type: "text", Text: "create a file"}}, func(ev PromptEvent) {
		if ev.Type == EventText {
			chunks = append(chunks, ev.Text)
		}
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}
}

func TestBridgeUsesConfiguredPermissionDecision(t *testing.T) {
	oldExecCommand := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		cs := []string{"-test.run=TestBridgeHelperProcess", "--"}
		cs = append(cs, args...)
		cmd := exec.Command(os.Args[0], cs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"KIRO_BRIDGE_HELPER_MODE=permission-request",
			"KIRO_BRIDGE_EXPECT_PERMISSION_OPTION=allow_once",
		)
		return cmd
	}
	defer func() { execCommand = oldExecCommand }()

	b, err := NewBridge(BridgeConfig{
		CLIPath: "kiro-cli",
		CWD:     ".",
		Agent:   "kiro-bridge",
		Version: "test",
		PermissionDecider: func(RequestPermissionParams) RequestPermissionOutcome {
			return RequestPermissionOutcome{Outcome: "selected", OptionID: "allow_once"}
		},
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer b.Close()

	var chunks []string
	_, err = b.Prompt(context.Background(), []ContentBlock{{Type: "text", Text: "create a file"}}, func(ev PromptEvent) {
		if ev.Type == EventText {
			chunks = append(chunks, ev.Text)
		}
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	// Prompt should complete without deadlocking and the configured choice should
	// be forwarded back to the helper ACP process.
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}
}

func TestDefaultPermissionOutcomePrefersRejectOnce(t *testing.T) {
	params := RequestPermissionParams{
		ToolCall: PermissionToolCall{Title: "Creating file"},
		Options: []PermissionOption{
			{OptionID: "allow_once", Name: "Yes"},
			{OptionID: "reject_once", Name: "No"},
		},
	}
	outcome := defaultPermissionOutcome(params.Options)
	if outcome.OptionID != "reject_once" {
		t.Fatalf("option = %q, want reject_once", outcome.OptionID)
	}
}

func TestBridgeEmitsToolCallEvents(t *testing.T) {
	oldExecCommand := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		cs := []string{"-test.run=TestBridgeHelperProcess", "--"}
		cs = append(cs, args...)
		cmd := exec.Command(os.Args[0], cs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"KIRO_BRIDGE_HELPER_MODE=tool-call",
		)
		return cmd
	}
	defer func() { execCommand = oldExecCommand }()

	b, err := NewBridge(BridgeConfig{
		CLIPath: "kiro-cli",
		CWD:     ".",
		Agent:   "kiro-bridge",
		Version: "test",
	})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer b.Close()

	var events []PromptEvent
	_, err = b.Prompt(context.Background(), []ContentBlock{{Type: "text", Text: "list files"}}, func(ev PromptEvent) {
		events = append(events, ev)
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// Expect: tool_call event, tool_call_update event, text chunk
	var hasToolCall, hasToolUpdate, hasText bool
	for _, ev := range events {
		switch ev.Type {
		case EventToolCall:
			hasToolCall = true
			if ev.ToolCallID == "" {
				t.Error("tool_call event missing ToolCallID")
			}
			if ev.ToolName == "" {
				t.Error("tool_call event missing ToolName")
			}
		case EventToolCallUpdate:
			hasToolUpdate = true
			if ev.ToolCallID == "" {
				t.Error("tool_call_update event missing ToolCallID")
			}
		case EventText:
			hasText = true
			if ev.Text == "" {
				t.Error("text event missing Text")
			}
		}
	}
	if !hasToolCall {
		t.Error("missing tool_call event")
	}
	if !hasToolUpdate {
		t.Error("missing tool_call_update event")
	}
	if !hasText {
		t.Error("missing text event")
	}
}

func TestHandleModelsReturnsRealModels(t *testing.T) {
	oldExecCommand := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		cs := []string{"-test.run=TestBridgeHelperProcess", "--"}
		cs = append(cs, args...)
		cmd := exec.Command(os.Args[0], cs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"KIRO_BRIDGE_HELPER_MODE=with-models",
		)
		return cmd
	}
	defer func() { execCommand = oldExecCommand }()

	b, err := NewBridge(BridgeConfig{CLIPath: "kiro-cli", CWD: ".", Agent: "kiro-bridge", Version: "test"})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer b.Close()

	models := b.Models()
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2", len(models))
	}
	if models[0].ID != "auto" {
		t.Errorf("models[0].ID = %q, want %q", models[0].ID, "auto")
	}
	if models[1].ID != "claude-sonnet-4" {
		t.Errorf("models[1].ID = %q, want %q", models[1].ID, "claude-sonnet-4")
	}
}

func TestInitializeParamsIncludesCapabilities(t *testing.T) {
	params := InitializeParams{
		ProtocolVersion: 1,
		ClientCapabilities: ClientCapabilities{
			PromptCapabilities: &PromptCapabilities{
				Image:           true,
				Audio:           true,
				EmbeddedContext: true,
			},
		},
		ClientInfo: ClientInfo{Name: "kiro-bridge", Title: "Kiro Bridge", Version: "test"},
	}
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"image":true`) {
		t.Errorf("should declare image support: %s", s)
	}
	if !strings.Contains(s, `"audio":true`) {
		t.Errorf("should declare audio capability explicitly: %s", s)
	}
	if !strings.Contains(s, `"embeddedContext":true`) {
		t.Errorf("should declare embeddedContext capability explicitly: %s", s)
	}
	if !strings.Contains(s, `"promptCapabilities"`) {
		t.Errorf("should have promptCapabilities: %s", s)
	}
}

func TestBridgeExtractsToolNameFromMeta(t *testing.T) {
	oldExecCommand := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		cs := []string{"-test.run=TestBridgeHelperProcess", "--"}
		cs = append(cs, args...)
		cmd := exec.Command(os.Args[0], cs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"KIRO_BRIDGE_HELPER_MODE=tool-call-with-meta",
		)
		return cmd
	}
	defer func() { execCommand = oldExecCommand }()

	b, err := NewBridge(BridgeConfig{CLIPath: "kiro-cli", CWD: ".", Agent: "kiro-bridge", Version: "test"})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer b.Close()

	var toolNames []string
	_, err = b.Prompt(context.Background(), []ContentBlock{{Type: "text", Text: "test"}}, func(ev PromptEvent) {
		if ev.Type == EventToolCall {
			toolNames = append(toolNames, ev.ToolName)
		}
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(toolNames) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(toolNames))
	}
	if toolNames[0] != "glob" {
		t.Errorf("tool name = %q, want %q (from _meta.tool_name)", toolNames[0], "glob")
	}
}

func TestBridgeRespondsMethodNotFound(t *testing.T) {
	oldExecCommand := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		cs := []string{"-test.run=TestBridgeHelperProcess", "--"}
		cs = append(cs, args...)
		cmd := exec.Command(os.Args[0], cs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"KIRO_BRIDGE_HELPER_MODE=unknown-method",
		)
		return cmd
	}
	defer func() { execCommand = oldExecCommand }()

	b, err := NewBridge(BridgeConfig{CLIPath: "kiro-cli", CWD: ".", Agent: "kiro-bridge", Version: "test"})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer b.Close()

	// Prompt should complete — the unknown method gets a -32601 response, unblocking the agent
	var chunks []string
	_, err = b.Prompt(context.Background(), []ContentBlock{{Type: "text", Text: "test"}}, func(ev PromptEvent) {
		if ev.Type == EventText {
			chunks = append(chunks, ev.Text)
		}
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}
}

func TestBridgeSendsCancelNotification(t *testing.T) {
	oldExecCommand := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		cs := []string{"-test.run=TestBridgeHelperProcess", "--"}
		cs = append(cs, args...)
		cmd := exec.Command(os.Args[0], cs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"KIRO_BRIDGE_HELPER_MODE=slow-prompt",
		)
		return cmd
	}
	defer func() { execCommand = oldExecCommand }()

	b, err := NewBridge(BridgeConfig{CLIPath: "kiro-cli", CWD: ".", Agent: "kiro-bridge", Version: "test"})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer b.Close()

	// Start prompt in goroutine, cancel after first chunk
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err := b.Prompt(ctx, []ContentBlock{{Type: "text", Text: "slow task"}}, func(ev PromptEvent) {
			if ev.Type == EventText {
				cancel()
			}
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Prompt should complete without error after cancel, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Prompt did not complete after cancel")
	}
}

func TestBridgeReconnectsAfterRepeatedErrors(t *testing.T) {
	oldExecCommand := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		cs := []string{"-test.run=TestBridgeHelperProcess", "--"}
		cs = append(cs, args...)
		cmd := exec.Command(os.Args[0], cs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"KIRO_BRIDGE_HELPER_MODE=error-then-ok",
		)
		return cmd
	}
	defer func() { execCommand = oldExecCommand }()

	b, err := NewBridge(BridgeConfig{CLIPath: "kiro-cli", CWD: ".", Agent: "kiro-bridge", Version: "test"})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer b.Close()

	// First 3 prompts return errors
	for i := 0; i < 3; i++ {
		_, err = b.Prompt(context.Background(), []ContentBlock{{Type: "text", Text: "test"}}, func(ev PromptEvent) {})
		if err == nil {
			t.Fatalf("prompt %d should error", i)
		}
	}

	// 4th prompt should succeed — bridge recreated session after 3 consecutive errors
	_, err = b.Prompt(context.Background(), []ContentBlock{{Type: "text", Text: "test"}}, func(ev PromptEvent) {})
	if err != nil {
		t.Fatalf("prompt after reconnect should succeed, got: %v", err)
	}
}

func TestBridgeCapturesContextUsage(t *testing.T) {
	oldExecCommand := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		cs := []string{"-test.run=TestBridgeHelperProcess", "--"}
		cs = append(cs, args...)
		cmd := exec.Command(os.Args[0], cs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"KIRO_BRIDGE_HELPER_MODE=with-metadata",
		)
		return cmd
	}
	defer func() { execCommand = oldExecCommand }()

	b, err := NewBridge(BridgeConfig{CLIPath: "kiro-cli", CWD: ".", Agent: "kiro-bridge", Version: "test"})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer b.Close()

	if _, err := b.Prompt(context.Background(), []ContentBlock{{Type: "text", Text: "test"}}, func(ev PromptEvent) {}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	usage := b.Usage()
	if usage.ContextPercent == 0 {
		t.Error("expected non-zero context usage after prompt")
	}
	if usage.TotalTokens == 0 {
		t.Error("expected non-zero estimated tokens")
	}
}

func TestBridgeUsesPromptFieldForPrompt(t *testing.T) {
	oldExecCommand := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		cs := []string{"-test.run=TestBridgeHelperProcess", "--"}
		cs = append(cs, args...)
		cmd := exec.Command(os.Args[0], cs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"KIRO_BRIDGE_HELPER_MODE=prompt-field",
		)
		return cmd
	}
	defer func() { execCommand = oldExecCommand }()

	b, err := NewBridge(BridgeConfig{CLIPath: "kiro-cli", CWD: ".", Agent: "", Version: "test"})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer b.Close()

	if _, err := b.Prompt(context.Background(), []ContentBlock{{Type: "text", Text: "test"}}, func(ev PromptEvent) {}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
}

func TestBridgeLoadSession(t *testing.T) {
	oldExecCommand := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		cs := []string{"-test.run=TestBridgeHelperProcess", "--"}
		cs = append(cs, args...)
		cmd := exec.Command(os.Args[0], cs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"KIRO_BRIDGE_HELPER_MODE=with-models",
		)
		return cmd
	}
	defer func() { execCommand = oldExecCommand }()

	rawBridge, err := NewBridge(BridgeConfig{CLIPath: "kiro-cli", CWD: ".", Agent: "", Version: "test"})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer rawBridge.Close()

	internal := rawBridge.(*bridge)
	proc, ready := internal.currentProcess()
	if !ready || proc == nil {
		t.Fatal("bridge should be ready")
	}

	manager := &acpSessionManager{bridge: internal, process: proc}
	session, err := manager.LoadSession("existing-session")
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if session.id != "loaded-session" {
		t.Fatalf("session id = %q, want loaded-session", session.id)
	}
}

func TestBridgeTurnEndOverridesPromptResult(t *testing.T) {
	oldExecCommand := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		cs := []string{"-test.run=TestBridgeHelperProcess", "--"}
		cs = append(cs, args...)
		cmd := exec.Command(os.Args[0], cs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"KIRO_BRIDGE_HELPER_MODE=turn-end",
		)
		return cmd
	}
	defer func() { execCommand = oldExecCommand }()

	b, err := NewBridge(BridgeConfig{CLIPath: "kiro-cli", CWD: ".", Agent: "", Version: "test"})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer b.Close()

	stopReason, err := b.Prompt(context.Background(), []ContentBlock{{Type: "text", Text: "test"}}, func(ev PromptEvent) {})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if stopReason != "max_tokens" {
		t.Fatalf("stop reason = %q, want max_tokens", stopReason)
	}
}

func TestBridgeSessionModes(t *testing.T) {
	oldExecCommand := execCommand
	oldMode := os.Getenv("KIRO_BRIDGE_SESSION_MODE")
	defer func() {
		execCommand = oldExecCommand
		if oldMode == "" {
			os.Unsetenv("KIRO_BRIDGE_SESSION_MODE")
		} else {
			os.Setenv("KIRO_BRIDGE_SESSION_MODE", oldMode)
		}
	}()

	execCommand = func(name string, args ...string) *exec.Cmd {
		cs := []string{"-test.run=TestBridgeHelperProcess", "--"}
		cs = append(cs, args...)
		cmd := exec.Command(os.Args[0], cs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"KIRO_BRIDGE_HELPER_MODE=session-ids",
		)
		return cmd
	}

	t.Run("per_request uses different sessions", func(t *testing.T) {
		os.Unsetenv("KIRO_BRIDGE_SESSION_MODE")
		b, err := NewBridge(BridgeConfig{CLIPath: "kiro-cli", CWD: ".", Agent: "", Version: "test"})
		if err != nil {
			t.Fatalf("NewBridge: %v", err)
		}
		defer b.Close()

		var sessions []string
		for i := 0; i < 2; i++ {
			_, err := b.Prompt(context.Background(), []ContentBlock{{Type: "text", Text: "test"}}, func(ev PromptEvent) {
				if ev.Type == EventText {
					sessions = append(sessions, ev.Text)
				}
			})
			if err != nil {
				t.Fatalf("Prompt: %v", err)
			}
		}
		if len(sessions) != 2 || sessions[0] == sessions[1] {
			t.Fatalf("sessions = %v, want two distinct ids", sessions)
		}
	})

	t.Run("shared reuses one session", func(t *testing.T) {
		os.Setenv("KIRO_BRIDGE_SESSION_MODE", string(SessionModeShared))
		b, err := NewBridge(BridgeConfig{CLIPath: "kiro-cli", CWD: ".", Agent: "", Version: "test"})
		if err != nil {
			t.Fatalf("NewBridge: %v", err)
		}
		defer b.Close()

		var sessions []string
		for i := 0; i < 2; i++ {
			_, err := b.Prompt(context.Background(), []ContentBlock{{Type: "text", Text: "test"}}, func(ev PromptEvent) {
				if ev.Type == EventText {
					sessions = append(sessions, ev.Text)
				}
			})
			if err != nil {
				t.Fatalf("Prompt: %v", err)
			}
		}
		if len(sessions) != 2 || sessions[0] != sessions[1] {
			t.Fatalf("sessions = %v, want reused shared id", sessions)
		}
	})
}

func TestBridgeRecoversAfterProcessExit(t *testing.T) {
	oldExecCommand := execCommand
	var launches int
	execCommand = func(name string, args ...string) *exec.Cmd {
		launches++
		mode := "with-metadata"
		if launches == 1 {
			mode = "crash-on-prompt"
		}
		cs := []string{"-test.run=TestBridgeHelperProcess", "--"}
		cs = append(cs, args...)
		cmd := exec.Command(os.Args[0], cs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"KIRO_BRIDGE_HELPER_MODE="+mode,
		)
		return cmd
	}
	defer func() { execCommand = oldExecCommand }()

	b, err := NewBridge(BridgeConfig{CLIPath: "kiro-cli", CWD: ".", Agent: "", Version: "test"})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer b.Close()

	if _, err := b.Prompt(context.Background(), []ContentBlock{{Type: "text", Text: "test"}}, func(ev PromptEvent) {}); err == nil {
		t.Fatal("first prompt should fail when process crashes")
	}

	deadline := time.Now().Add(5 * time.Second)
	for !b.Ready() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !b.Ready() {
		t.Fatal("bridge did not recover readiness after process exit")
	}

	if _, err := b.Prompt(context.Background(), []ContentBlock{{Type: "text", Text: "test"}}, func(ev PromptEvent) {}); err != nil {
		t.Fatalf("prompt after reconnect should succeed: %v", err)
	}
}

func TestBridgeCancelTimeoutForceClosesProcess(t *testing.T) {
	oldExecCommand := execCommand
	countFile := t.TempDir() + "/cancel-count.txt"
	var launches int
	execCommand = func(name string, args ...string) *exec.Cmd {
		launches++
		mode := "with-metadata"
		if launches == 1 {
			mode = "cancel-hangs"
		}
		cs := []string{"-test.run=TestBridgeHelperProcess", "--"}
		cs = append(cs, args...)
		cmd := exec.Command(os.Args[0], cs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"KIRO_BRIDGE_HELPER_MODE="+mode,
			"KIRO_BRIDGE_HELPER_COUNT_FILE="+countFile,
		)
		return cmd
	}
	defer func() { execCommand = oldExecCommand }()

	b, err := NewBridge(BridgeConfig{CLIPath: "kiro-cli", CWD: ".", Agent: "", Version: "test"})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer b.Close()

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err := b.Prompt(ctx, []ContentBlock{{Type: "text", Text: "slow task"}}, func(ev PromptEvent) {
			if ev.Type == EventText {
				cancel()
			}
		})
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Prompt error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Prompt did not return after forced cancel")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cancel path took too long: %s", elapsed)
	}

	data, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Count(string(data), "cancel\n") != 1 {
		t.Fatalf("cancel count = %q, want exactly one cancel", string(data))
	}

	deadline := time.Now().Add(5 * time.Second)
	for !b.Ready() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !b.Ready() {
		t.Fatal("bridge did not recover readiness after forced close")
	}
	if _, err := b.Prompt(context.Background(), []ContentBlock{{Type: "text", Text: "test"}}, func(PromptEvent) {}); err != nil {
		t.Fatalf("prompt after reconnect should succeed: %v", err)
	}
}

func TestBridgeMatchesResponsesByID(t *testing.T) {
	oldExecCommand := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		cs := []string{"-test.run=TestBridgeHelperProcess", "--"}
		cs = append(cs, args...)
		cmd := exec.Command(os.Args[0], cs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"KIRO_BRIDGE_HELPER_MODE=response-shuffle",
		)
		return cmd
	}
	defer func() { execCommand = oldExecCommand }()

	b, err := NewBridge(BridgeConfig{CLIPath: "kiro-cli", CWD: ".", Agent: "", Version: "test"})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer b.Close()

	var chunks []string
	stopReason, err := b.Prompt(context.Background(), []ContentBlock{{Type: "text", Text: "test"}}, func(ev PromptEvent) {
		if ev.Type == EventText {
			chunks = append(chunks, ev.Text)
		}
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if stopReason != "end_turn" {
		t.Fatalf("stop reason = %q, want end_turn", stopReason)
	}
	if len(chunks) != 1 || chunks[0] != "ok" {
		t.Fatalf("chunks = %v, want [ok]", chunks)
	}
}

func TestBridgeRecoversAfterMalformedJSON(t *testing.T) {
	oldExecCommand := execCommand
	var launches int
	execCommand = func(name string, args ...string) *exec.Cmd {
		launches++
		mode := "with-metadata"
		if launches == 1 {
			mode = "bad-json"
		}
		cs := []string{"-test.run=TestBridgeHelperProcess", "--"}
		cs = append(cs, args...)
		cmd := exec.Command(os.Args[0], cs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"KIRO_BRIDGE_HELPER_MODE="+mode,
		)
		return cmd
	}
	defer func() { execCommand = oldExecCommand }()

	b, err := NewBridge(BridgeConfig{CLIPath: "kiro-cli", CWD: ".", Agent: "", Version: "test"})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer b.Close()

	if _, err := b.Prompt(context.Background(), []ContentBlock{{Type: "text", Text: "test"}}, func(PromptEvent) {}); err == nil {
		t.Fatal("first prompt should fail on malformed JSON")
	}

	deadline := time.Now().Add(5 * time.Second)
	for !b.Ready() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !b.Ready() {
		t.Fatal("bridge did not recover readiness after malformed JSON")
	}
	if _, err := b.Prompt(context.Background(), []ContentBlock{{Type: "text", Text: "test"}}, func(PromptEvent) {}); err != nil {
		t.Fatalf("prompt after reconnect should succeed: %v", err)
	}
}
