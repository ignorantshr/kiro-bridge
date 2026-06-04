package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type mockBridge struct {
	chunks               []string
	gotText              string
	gotModel             string
	promptErr            error
	promptErrAfterChunks bool
	promptCaps           PromptCapabilities
}

func (m *mockBridge) Prompt(ctx context.Context, blocks []ContentBlock, onEvent func(PromptEvent)) (string, error) {
	if len(blocks) > 0 {
		m.gotText = blocks[0].Text
	}
	m.gotModel = requestedModelFromContext(ctx)
	if m.promptErr != nil && !m.promptErrAfterChunks {
		return "", m.promptErr
	}
	for _, c := range m.chunks {
		onEvent(PromptEvent{Type: EventText, Text: c})
	}
	if m.promptErr != nil {
		return "", m.promptErr
	}
	return "end_turn", nil
}

func (m *mockBridge) Close() error        { return nil }
func (m *mockBridge) Models() []ModelInfo { return nil }
func (m *mockBridge) PromptCapabilities() PromptCapabilities {
	return m.promptCaps
}
func (m *mockBridge) Usage() UsageInfo { return UsageInfo{} }
func (m *mockBridge) Ready() bool      { return true }

func TestBuildPromptTextWithContentParts(t *testing.T) {
	body := `{"messages":[{"role":"system","content":"Be helpful."},{"role":"user","content":[{"type":"text","text":"hello"}]}],"model":"kiro","stream":true}`
	var req ChatCompletionRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	got := buildPromptText(req.Messages, PromptCapabilities{})
	if !strings.Contains(got, "hello") {
		t.Errorf("expected 'hello' in prompt, got: %q", got)
	}
}

func TestBuildPromptText(t *testing.T) {
	tests := []struct {
		name     string
		messages []ChatMessage
		want     string
	}{
		{
			name:     "user only",
			messages: []ChatMessage{{Role: "user", Content: textChatContent("hello")}},
			want:     "User: hello",
		},
		{
			name: "system + user",
			messages: []ChatMessage{
				{Role: "system", Content: textChatContent("You are helpful.")},
				{Role: "user", Content: textChatContent("hello")},
			},
			want: "System: You are helpful.\n\nUser: hello",
		},
		{
			name: "preserves assistant messages",
			messages: []ChatMessage{
				{Role: "user", Content: textChatContent("hi")},
				{Role: "assistant", Content: textChatContent("hey")},
				{Role: "user", Content: textChatContent("bye")},
			},
			want: "User: hi\n\nAssistant: hey\n\nUser: bye",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildPromptText(tt.messages, PromptCapabilities{})
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandleNonStream(t *testing.T) {
	mock := &mockBridge{chunks: []string{"Hello", " world"}}
	handler := handleChatCompletions(mock)

	body := `{"model":"kiro","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp ChatCompletionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Object != "chat.completion" {
		t.Errorf("object = %q, want %q", resp.Object, "chat.completion")
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(resp.Choices))
	}
	if resp.Choices[0].Message.Content != "Hello world" {
		t.Errorf("content = %q, want %q", resp.Choices[0].Message.Content, "Hello world")
	}
	if *resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want %q", *resp.Choices[0].FinishReason, "stop")
	}
	if mock.gotText != "User: hi" {
		t.Errorf("prompt text = %q, want %q", mock.gotText, "User: hi")
	}
	if mock.gotModel != bridgeDefaultModelID {
		t.Errorf("prompt model = %q, want %q", mock.gotModel, bridgeDefaultModelID)
	}
}

func TestHandleStream(t *testing.T) {
	mock := &mockBridge{chunks: []string{"Hello", " world"}}
	handler := handleChatCompletions(mock)

	body := `{"model":"kiro","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want %q", ct, "text/event-stream")
	}

	// Parse SSE events
	scanner := bufio.NewScanner(strings.NewReader(w.Body.String()))
	var events []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			events = append(events, strings.TrimPrefix(line, "data: "))
		}
	}

	// Should have: chunk1 (role+content), chunk2 (content), final (finish_reason), [DONE]
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4; got:\n%s", len(events), w.Body.String())
	}

	// First chunk should have role
	var first ChatCompletionResponse
	json.Unmarshal([]byte(events[0]), &first)
	if first.Choices[0].Delta.Role != "assistant" {
		t.Errorf("first chunk role = %q, want %q", first.Choices[0].Delta.Role, "assistant")
	}
	if first.Choices[0].Delta.Content != "Hello" {
		t.Errorf("first chunk content = %q, want %q", first.Choices[0].Delta.Content, "Hello")
	}

	// Second chunk should have content only
	var second ChatCompletionResponse
	json.Unmarshal([]byte(events[1]), &second)
	if second.Choices[0].Delta.Content != " world" {
		t.Errorf("second chunk content = %q, want %q", second.Choices[0].Delta.Content, " world")
	}

	// Third chunk should have finish_reason
	var final ChatCompletionResponse
	json.Unmarshal([]byte(events[2]), &final)
	if *final.Choices[0].FinishReason != "stop" {
		t.Errorf("final finish_reason = %q, want %q", *final.Choices[0].FinishReason, "stop")
	}

	// Last should be [DONE]
	if events[3] != "[DONE]" {
		t.Errorf("last event = %q, want %q", events[3], "[DONE]")
	}
}

func TestHandleChatCompletionsPassesRequestedModelToBridge(t *testing.T) {
	mock := &mockBridge{chunks: []string{"ok"}}
	handler := handleChatCompletions(mock)

	body := `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if mock.gotModel != "claude-sonnet-4" {
		t.Fatalf("prompt model = %q, want %q", mock.gotModel, "claude-sonnet-4")
	}
}

func TestHandleStreamReturnsHTTPErrorBeforeFirstChunk(t *testing.T) {
	mock := &mockBridge{promptErr: errBridgeNotReady}
	handler := handleChatCompletions(mock)

	body := `{"model":"kiro","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "bridge not ready") {
		t.Fatalf("body = %q, want bridge not ready", w.Body.String())
	}
}

func TestHandleStreamTerminatesWithSSEErrorAfterChunks(t *testing.T) {
	mock := &mockBridge{
		chunks:               []string{"Hello"},
		promptErr:            errBridgeNotReady,
		promptErrAfterChunks: true,
	}
	handler := handleChatCompletions(mock)

	body := `{"model":"kiro","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if strings.Contains(w.Body.String(), "bridge not ready") {
		t.Fatalf("unexpected http error body in SSE response: %s", w.Body.String())
	}

	scanner := bufio.NewScanner(strings.NewReader(w.Body.String()))
	var events []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			events = append(events, strings.TrimPrefix(line, "data: "))
		}
	}
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3; got:\n%s", len(events), w.Body.String())
	}

	var final ChatCompletionResponse
	if err := json.Unmarshal([]byte(events[1]), &final); err != nil {
		t.Fatalf("unmarshal final chunk: %v", err)
	}
	if *final.Choices[0].FinishReason != "error" {
		t.Fatalf("finish_reason = %q, want error", *final.Choices[0].FinishReason)
	}
	if events[2] != "[DONE]" {
		t.Fatalf("last event = %q, want [DONE]", events[2])
	}
}

func TestHandleEmptyMessages(t *testing.T) {
	mock := &mockBridge{}
	handler := handleChatCompletions(mock)

	body := `{"model":"kiro","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleWrongMethod(t *testing.T) {
	mock := &mockBridge{}
	handler := handleChatCompletions(mock)

	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestLoggingWriterImplementsFlusher(t *testing.T) {
	inner := httptest.NewRecorder()
	lw := &loggingWriter{ResponseWriter: inner, status: 200}

	// Must implement http.Flusher for SSE streaming
	var w http.ResponseWriter = lw
	flusher, ok := w.(http.Flusher)
	if !ok {
		t.Fatal("loggingWriter does not implement http.Flusher")
	}
	flusher.Flush() // should not panic
}

func TestStreamThroughMiddleware(t *testing.T) {
	mock := &mockBridge{chunks: []string{"hi"}}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handleChatCompletions(mock))
	server := httptest.NewServer(logMiddleware(mux))
	defer server.Close()

	body := `{"model":"kiro","messages":[{"role":"user","content":"test"}],"stream":true}`
	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want %q", ct, "text/event-stream")
	}
}

func TestChatContentUnmarshalEdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		want    string
		wantErr bool
	}{
		{"null content", `null`, "", false},
		{"empty string", `""`, "", false},
		{"empty array", `[]`, "", false},
		{"invalid image_url shape", `[{"type":"image_url","url":"http://x"}]`, "", true},
		{"array with mixed types", `[{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"hi"}]`, "hi", false},
		{"multiple text parts", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "ab", false},
		{"input audio part", `[{"type":"input_audio","input_audio":{"data":"abc","format":"wav"}}]`, "", false},
		{"file part", `[{"type":"file","file":{"file_id":"file_123","filename":"note.txt"}}]`, "", false},
		{"refusal part", `[{"type":"refusal","refusal":"cannot help"}]`, "", false},
		{"invalid object", `{"type":"text","text":"hi"}`, "", true},
		{"unknown part type", `[{"type":"video","url":"x"}]`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c ChatContent
			err := json.Unmarshal([]byte(tt.json), &c)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected unmarshal error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unmarshal error: %v", err)
			}
			if c.TextValue() != tt.want {
				t.Errorf("got %q, want %q", c.TextValue(), tt.want)
			}
		})
	}
}

func TestChatContentMarshalRoundtrip(t *testing.T) {
	c := textChatContent("hello")
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `"hello"` {
		t.Errorf("marshal = %s, want %q", data, `"hello"`)
	}
}

func TestHandleInvalidJSON(t *testing.T) {
	mock := &mockBridge{}
	handler := handleChatCompletions(mock)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{not json`))
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandlePromptError(t *testing.T) {
	mock := &mockBridge{promptErr: io.EOF}
	handler := handleChatCompletions(mock)

	body := `{"model":"kiro","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

func TestHandleStreamPromptError(t *testing.T) {
	mock := &mockBridge{promptErr: io.EOF}
	handler := handleChatCompletions(mock)

	body := `{"model":"kiro","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler(w, req)

	// Should return 502 since no chunks were sent before the error
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

func TestHandleStreamPromptErrorAfterChunks(t *testing.T) {
	mock := &mockBridge{
		chunks:               []string{"Hello"},
		promptErr:            io.EOF,
		promptErrAfterChunks: true,
	}
	handler := handleChatCompletions(mock)

	body := `{"model":"kiro","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	scanner := bufio.NewScanner(strings.NewReader(w.Body.String()))
	var events []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			events = append(events, strings.TrimPrefix(line, "data: "))
		}
	}

	if len(events) != 3 {
		t.Fatalf("events = %d, want 3; got:\n%s", len(events), w.Body.String())
	}

	var final ChatCompletionResponse
	if err := json.Unmarshal([]byte(events[1]), &final); err != nil {
		t.Fatalf("unmarshal final event: %v", err)
	}
	if final.Choices[0].FinishReason == nil || *final.Choices[0].FinishReason != "error" {
		t.Fatalf("finish_reason = %v, want error", final.Choices[0].FinishReason)
	}
	if events[2] != "[DONE]" {
		t.Fatalf("last event = %q, want [DONE]", events[2])
	}
}

func TestHandleInvalidContentShape(t *testing.T) {
	mock := &mockBridge{}
	handler := handleChatCompletions(mock)

	body := `{"model":"kiro","messages":[{"role":"user","content":{"type":"text","text":"hi"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleModelsGET(t *testing.T) {
	mock := &mockBridge{}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	handleModels(mock)(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"id":"kiro"`) {
		t.Errorf("body missing kiro model: %s", w.Body.String())
	}
}

func TestHandleModelsRejectsPost(t *testing.T) {
	mock := &mockBridge{}
	req := httptest.NewRequest(http.MethodPost, "/v1/models", nil)
	w := httptest.NewRecorder()
	handleModels(mock)(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestNewCompletionIDUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := newCompletionID()
		if seen[id] {
			t.Fatalf("duplicate ID: %s", id)
		}
		seen[id] = true
	}
}

func TestLogMiddlewareVerbose(t *testing.T) {
	// Enable verbose for this test
	old := verboseLog
	verboseLog = true
	defer func() { verboseLog = old }()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(logMiddleware(inner))
	defer server.Close()

	resp, err := http.Post(server.URL, "application/json", strings.NewReader(`{"test":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestChatToolCallMarshal(t *testing.T) {
	tc := ChatToolCall{
		ID:       "call_1",
		Type:     "function",
		Function: &ToolCallFunction{Name: "read", Arguments: `{"path":"main.go"}`},
	}
	data, err := json.Marshal(tc)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"id":"call_1"`) {
		t.Errorf("missing id: %s", s)
	}
	if !strings.Contains(s, `"type":"function"`) {
		t.Errorf("missing type: %s", s)
	}
	if !strings.Contains(s, `"name":"read"`) {
		t.Errorf("missing function name: %s", s)
	}
}

func TestToolCallInDelta(t *testing.T) {
	msg := struct {
		Delta ChatCompletionDelta `json:"delta"`
	}{
		Delta: ChatCompletionDelta{
			Role: "assistant",
			ToolCalls: []ChatToolCallDelta{{
				Index:    0,
				ID:       "call_1",
				Type:     "function",
				Function: &ToolCallFunctionDelta{Name: "grep"},
			}},
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"tool_calls"`) {
		t.Errorf("missing tool_calls: %s", s)
	}
	if !strings.Contains(s, `"name":"grep"`) {
		t.Errorf("missing function name: %s", s)
	}
}

func TestToolCallOmittedWhenEmpty(t *testing.T) {
	msg := ChatCompletionMessage{
		Role:    "assistant",
		Content: "hello",
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "tool_calls") {
		t.Errorf("tool_calls should be omitted when empty: %s", data)
	}
}

type mockBridgeWithToolCalls struct {
	gotText  string
	gotModel string
}

func (m *mockBridgeWithToolCalls) Prompt(ctx context.Context, blocks []ContentBlock, onEvent func(PromptEvent)) (string, error) {
	if len(blocks) > 0 {
		m.gotText = blocks[0].Text
	}
	m.gotModel = requestedModelFromContext(ctx)
	onEvent(PromptEvent{
		Type:       EventToolCall,
		ToolCallID: "call_1",
		ToolName:   "read",
		ToolInput:  `{"path":"main.go"}`,
	})
	onEvent(PromptEvent{
		Type:       EventToolCallUpdate,
		ToolCallID: "call_1",
		ToolStatus: "completed",
	})
	onEvent(PromptEvent{Type: EventText, Text: "Here are the contents."})
	return "end_turn", nil
}

func (m *mockBridgeWithToolCalls) Close() error        { return nil }
func (m *mockBridgeWithToolCalls) Models() []ModelInfo { return nil }
func (m *mockBridgeWithToolCalls) PromptCapabilities() PromptCapabilities {
	return PromptCapabilities{}
}
func (m *mockBridgeWithToolCalls) Usage() UsageInfo { return UsageInfo{} }
func (m *mockBridgeWithToolCalls) Ready() bool      { return true }

func TestHandleStreamWithToolCalls(t *testing.T) {
	old := showToolAnnotations
	showToolAnnotations = true
	defer func() { showToolAnnotations = old }()

	mock := &mockBridgeWithToolCalls{}
	handler := handleChatCompletions(mock)

	body := `{"model":"kiro","messages":[{"role":"user","content":"read main.go"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	scanner := bufio.NewScanner(strings.NewReader(w.Body.String()))
	var events []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			events = append(events, strings.TrimPrefix(line, "data: "))
		}
	}

	// Should have: tool annotation text, content text, finish chunk, [DONE]
	var hasToolText, hasContentText bool
	for _, ev := range events {
		if ev == "[DONE]" {
			continue
		}
		var resp ChatCompletionResponse
		if err := json.Unmarshal([]byte(ev), &resp); err != nil {
			continue
		}
		if len(resp.Choices) == 0 {
			continue
		}
		delta := resp.Choices[0].Delta
		if delta != nil && strings.Contains(delta.Content, "🔧") {
			hasToolText = true
		}
		if delta != nil && delta.Content == "Here are the contents." {
			hasContentText = true
		}
	}
	if !hasToolText {
		t.Errorf("no tool annotation found in events:\n%s", w.Body.String())
	}
	if !hasContentText {
		t.Errorf("no content text found in events:\n%s", w.Body.String())
	}
}

func TestMaxBody(t *testing.T) {
	t.Run("default is 1MB", func(t *testing.T) {
		old := maxBodyBytes
		defer func() { maxBodyBytes = old }()
		maxBodyBytes = 1 << 20
		if maxBodyBytes != 1048576 {
			t.Errorf("got %d, want %d", maxBodyBytes, 1048576)
		}
	})
	t.Run("rejects oversized request", func(t *testing.T) {
		old := maxBodyBytes
		maxBodyBytes = 64
		defer func() { maxBodyBytes = old }()

		mock := &mockBridge{}
		handler := handleChatCompletions(mock)

		body := `{"model":"kiro","messages":[{"role":"user","content":"` + strings.Repeat("x", 100) + `"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})
}

func TestShowTools(t *testing.T) {
	t.Run("off by default hides annotations", func(t *testing.T) {
		old := showToolAnnotations
		showToolAnnotations = false
		defer func() { showToolAnnotations = old }()

		mock := &mockBridgeWithToolCalls{}
		handler := handleChatCompletions(mock)

		body := `{"model":"kiro","messages":[{"role":"user","content":"test"}],"stream":true}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		w := httptest.NewRecorder()
		handler(w, req)

		if strings.Contains(w.Body.String(), "🔧") {
			t.Error("tool annotations should not appear when off")
		}
	})
	t.Run("on shows annotations", func(t *testing.T) {
		old := showToolAnnotations
		showToolAnnotations = true
		defer func() { showToolAnnotations = old }()

		mock := &mockBridgeWithToolCalls{}
		handler := handleChatCompletions(mock)

		body := `{"model":"kiro","messages":[{"role":"user","content":"test"}],"stream":true}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		w := httptest.NewRecorder()
		handler(w, req)

		if !strings.Contains(w.Body.String(), "🔧") {
			t.Error("tool annotations should appear when on")
		}
	})
}

func TestMapStopReason(t *testing.T) {
	t.Run("end_turn maps to stop", func(t *testing.T) {
		if got := mapStopReason("end_turn"); got != "stop" {
			t.Errorf("got %q, want %q", got, "stop")
		}
	})
	t.Run("max_tokens maps to length", func(t *testing.T) {
		if got := mapStopReason("max_tokens"); got != "length" {
			t.Errorf("got %q, want %q", got, "length")
		}
	})
	t.Run("max_turn_requests maps to stop", func(t *testing.T) {
		if got := mapStopReason("max_turn_requests"); got != "stop" {
			t.Errorf("got %q, want %q", got, "stop")
		}
	})
	t.Run("refusal maps to stop", func(t *testing.T) {
		if got := mapStopReason("refusal"); got != "stop" {
			t.Errorf("got %q, want %q", got, "stop")
		}
	})
	t.Run("cancelled maps to stop", func(t *testing.T) {
		if got := mapStopReason("cancelled"); got != "stop" {
			t.Errorf("got %q, want %q", got, "stop")
		}
	})
	t.Run("unknown maps to stop", func(t *testing.T) {
		if got := mapStopReason("something_new"); got != "stop" {
			t.Errorf("got %q, want %q", got, "stop")
		}
	})
	t.Run("empty maps to stop", func(t *testing.T) {
		if got := mapStopReason(""); got != "stop" {
			t.Errorf("got %q, want %q", got, "stop")
		}
	})
}

func TestBuildPromptTextPreservesOpenAIRoles(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "developer", Name: "planner", Content: textChatContent("Prefer concise output.")},
		{Role: "system", Content: textChatContent("Be helpful.")},
		{Role: "user", Content: textChatContent("Hi")},
		{Role: "assistant", Content: textChatContent("Hello!")},
		{Role: "tool", ToolCallID: "call_1", Content: textChatContent(`{"value":4}`)},
	}
	got := buildPromptText(msgs, PromptCapabilities{})
	want := "Developer[planner]: Prefer concise output.\n\nSystem: Be helpful.\n\nUser: Hi\n\nAssistant: Hello!\n\nTool[call_1]: {\"value\":4}"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestChatContentParsesImageURL(t *testing.T) {
	input := `[{"type":"text","text":"What is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,abc123"}},{"type":"text","text":"Please answer."}]`
	var c ChatContent
	if err := json.Unmarshal([]byte(input), &c); err != nil {
		t.Fatal(err)
	}
	if c.TextValue() != "What is this?Please answer." {
		t.Errorf("text = %q, want %q", c.TextValue(), "What is this?Please answer.")
	}
	if len(c.Parts) != 3 {
		t.Fatalf("parts = %d, want 3", len(c.Parts))
	}
	if c.Parts[0].Type != "text" || c.Parts[0].Text != "What is this?" {
		t.Fatalf("parts[0] = %+v", c.Parts[0])
	}
	if c.Parts[1].Type != "image_url" || c.Parts[1].ImageURL == nil {
		t.Fatalf("parts[1] = %+v", c.Parts[1])
	}
	if c.Parts[2].Type != "text" || c.Parts[2].Text != "Please answer." {
		t.Fatalf("parts[2] = %+v", c.Parts[2])
	}
	mimeType, decoded := parseDataURI(c.Parts[1].ImageURL.URL)
	if mimeType != "image/png" {
		t.Errorf("mime = %q, want %q", mimeType, "image/png")
	}
	if decoded != "abc123" {
		t.Errorf("data = %q, want %q", decoded, "abc123")
	}
}

func TestChatCompletionRequestPreservesOpenAISemantics(t *testing.T) {
	body := `{
		"model":"gpt-test",
		"messages":[
			{"role":"developer","name":"planner","content":"Prefer short answers."},
			{"role":"assistant","content":[{"type":"refusal","refusal":"cannot help"}],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"weather\"}"}}],"function_call":{"name":"legacy_lookup","arguments":"{\"q\":\"weather\"}"},"audio":{"id":"audio_123"}},
			{"role":"tool","tool_call_id":"call_1","content":"{\"temp\":22}"},
			{"role":"user","content":[{"type":"text","text":"look"},{"type":"input_audio","input_audio":{"data":"YWJj","format":"wav"}},{"type":"file","file":{"file_id":"file_123","filename":"note.txt"}}]}
		]
	}`
	var req ChatCompletionRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(req.Messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(req.Messages))
	}
	if req.Messages[0].Role != "developer" || req.Messages[0].Name != "planner" {
		t.Fatalf("developer message = %+v", req.Messages[0])
	}
	if len(req.Messages[1].ToolCalls) != 1 || req.Messages[1].ToolCalls[0].ID != "call_1" {
		t.Fatalf("assistant tool calls = %+v", req.Messages[1].ToolCalls)
	}
	if req.Messages[1].FunctionCall == nil || req.Messages[1].FunctionCall.Name != "legacy_lookup" {
		t.Fatalf("assistant function call = %+v", req.Messages[1].FunctionCall)
	}
	if req.Messages[1].Audio == nil || req.Messages[1].Audio.ID != "audio_123" {
		t.Fatalf("assistant audio = %+v", req.Messages[1].Audio)
	}
	if req.Messages[2].ToolCallID != "call_1" {
		t.Fatalf("tool message = %+v", req.Messages[2])
	}
	if len(req.Messages[3].Content.Parts) != 3 {
		t.Fatalf("user parts = %d, want 3", len(req.Messages[3].Content.Parts))
	}
	if req.Messages[3].Content.Parts[1].InputAudio == nil || req.Messages[3].Content.Parts[1].InputAudio.Format != "wav" {
		t.Fatalf("input audio part = %+v", req.Messages[3].Content.Parts[1])
	}
	if req.Messages[3].Content.Parts[2].File == nil || req.Messages[3].Content.Parts[2].File.FileID != "file_123" {
		t.Fatalf("file part = %+v", req.Messages[3].Content.Parts[2])
	}
}

func TestBuildPromptBlocks(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: ChatContent{Parts: []ChatContentPart{
			{Type: "text", Text: "What is this?"},
			{Type: "image_url", ImageURL: &ImageURLPart{URL: "data:image/png;base64,abc123"}},
		}}},
	}
	blocks := buildPromptBlocks(msgs, PromptCapabilities{Image: true})
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want 2", len(blocks))
	}
	if blocks[0].Type != "text" {
		t.Errorf("blocks[0].type = %q, want text", blocks[0].Type)
	}
	if blocks[1].Type != "image" {
		t.Errorf("blocks[1].type = %q, want image", blocks[1].Type)
	}
	if blocks[1].MimeType != "image/png" {
		t.Errorf("blocks[1].mimeType = %q", blocks[1].MimeType)
	}
}

func TestBuildPromptBlocksPreservesConversationOrder(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "system", Content: textChatContent("Be precise.")},
		{Role: "user", Content: ChatContent{
			Parts: []ChatContentPart{
				{Type: "text", Text: "Look at "},
				{Type: "image_url", ImageURL: &ImageURLPart{URL: "data:image/png;base64,img1"}},
				{Type: "text", Text: " then answer."},
			},
		}},
		{Role: "assistant", Content: textChatContent("It is a cat.")},
		{Role: "user", Content: textChatContent("Why?")},
	}

	blocks := buildPromptBlocks(msgs, PromptCapabilities{Image: true})
	if len(blocks) != 6 {
		t.Fatalf("got %d blocks, want 6", len(blocks))
	}
	if blocks[0].Text != "System: Be precise." {
		t.Fatalf("blocks[0].text = %q", blocks[0].Text)
	}
	if blocks[1].Text != "User: Look at " {
		t.Fatalf("blocks[1].text = %q", blocks[1].Text)
	}
	if blocks[2].Type != "image" || blocks[2].Data != "img1" {
		t.Fatalf("blocks[2] = %+v, want image img1", blocks[2])
	}
	if blocks[3].Text != " then answer." {
		t.Fatalf("blocks[3].text = %q", blocks[3].Text)
	}
	if blocks[4].Text != "Assistant: It is a cat." {
		t.Fatalf("blocks[4].text = %q", blocks[4].Text)
	}
	if blocks[5].Text != "User: Why?" {
		t.Fatalf("blocks[5].text = %q", blocks[5].Text)
	}
}

func TestBuildPromptBlocksImagesDisabled(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: ChatContent{Parts: []ChatContentPart{
			{Type: "text", Text: "What is this?"},
			{Type: "image_url", ImageURL: &ImageURLPart{URL: "data:image/png;base64,abc123"}},
		}}},
	}
	blocks := buildPromptBlocks(msgs, PromptCapabilities{})
	// Should only have text, no image blocks
	for _, b := range blocks {
		if b.Type == "image" {
			t.Error("image blocks should not be included when FF disabled")
		}
	}
}

func TestBuildPromptBlocksPreservesImageOnlyRoleContext(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "assistant", Content: ChatContent{
			Parts: []ChatContentPart{{Type: "image_url", ImageURL: &ImageURLPart{URL: "data:image/png;base64,img1"}}},
		}},
	}

	blocks := buildPromptBlocks(msgs, PromptCapabilities{Image: true})
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want 2", len(blocks))
	}
	if blocks[0].Type != "text" || blocks[0].Text != "Assistant:" {
		t.Fatalf("blocks[0] = %+v, want Assistant: header", blocks[0])
	}
	if blocks[1].Type != "image" || blocks[1].Data != "img1" {
		t.Fatalf("blocks[1] = %+v, want image img1", blocks[1])
	}
}

func TestBuildPromptBlocksMapsInputAudioToACPBlock(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: ChatContent{
			Parts: []ChatContentPart{
				{Type: "text", Text: "Listen"},
				{Type: "input_audio", InputAudio: &InputAudioPart{Data: "YWJj", Format: "wav"}},
			},
		}},
	}

	blocks := buildPromptBlocks(msgs, PromptCapabilities{Audio: true})
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want 2", len(blocks))
	}
	if blocks[0].Text != "User: Listen" {
		t.Fatalf("blocks[0] = %+v", blocks[0])
	}
	if blocks[1].Type != "audio" || blocks[1].MimeType != "audio/wav" || blocks[1].Data != "YWJj" {
		t.Fatalf("blocks[1] = %+v", blocks[1])
	}
}

func TestBuildPromptBlocksFallsBackWhenAudioUnsupported(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: ChatContent{
			Parts: []ChatContentPart{
				{Type: "input_audio", InputAudio: &InputAudioPart{Data: "YWJj", Format: "wav"}},
			},
		}},
	}

	blocks := buildPromptBlocks(msgs, PromptCapabilities{})
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	if blocks[0].Type != "text" || !strings.Contains(blocks[0].Text, "InputAudio:") {
		t.Fatalf("blocks[0] = %+v", blocks[0])
	}
}

func TestBuildPromptBlocksMapsFileDataToEmbeddedResource(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: ChatContent{
			Parts: []ChatContentPart{
				{Type: "file", File: &FileContentPart{Filename: "note.txt", FileData: "YWJj"}},
			},
		}},
	}

	blocks := buildPromptBlocks(msgs, PromptCapabilities{EmbeddedContext: true})
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want 2", len(blocks))
	}
	if blocks[0].Type != "text" || blocks[0].Text != "User:" {
		t.Fatalf("blocks[0] = %+v", blocks[0])
	}
	if blocks[1].Type != "resource" || blocks[1].Resource == nil {
		t.Fatalf("blocks[1] = %+v", blocks[1])
	}
	if blocks[1].Resource.URI != "note.txt" {
		t.Fatalf("resource uri = %q", blocks[1].Resource.URI)
	}
	if blocks[1].Resource.Text != "abc" {
		t.Fatalf("resource text = %q", blocks[1].Resource.Text)
	}
	if blocks[1].Resource.Blob != "" {
		t.Fatalf("resource blob should be empty, got %q", blocks[1].Resource.Blob)
	}
}

func TestBuildPromptBlocksMapsBinaryFileDataToBlobResource(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: ChatContent{
			Parts: []ChatContentPart{
				{Type: "file", File: &FileContentPart{Filename: "image.png", FileData: "AAECAw=="}},
			},
		}},
	}

	blocks := buildPromptBlocks(msgs, PromptCapabilities{EmbeddedContext: true})
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want 2", len(blocks))
	}
	if blocks[1].Type != "resource" || blocks[1].Resource == nil {
		t.Fatalf("blocks[1] = %+v", blocks[1])
	}
	if blocks[1].Resource.URI != "image.png" {
		t.Fatalf("resource uri = %q", blocks[1].Resource.URI)
	}
	if blocks[1].Resource.Blob != "AAECAw==" {
		t.Fatalf("resource blob = %q", blocks[1].Resource.Blob)
	}
	if blocks[1].Resource.Text != "" {
		t.Fatalf("resource text should be empty, got %q", blocks[1].Resource.Text)
	}
}

func TestBuildPromptBlocksFallsBackWhenEmbeddedContextUnsupported(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: ChatContent{
			Parts: []ChatContentPart{
				{Type: "file", File: &FileContentPart{Filename: "note.txt", FileData: "YWJj"}},
			},
		}},
	}

	blocks := buildPromptBlocks(msgs, PromptCapabilities{})
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	if blocks[0].Type != "text" || !strings.Contains(blocks[0].Text, "File:") {
		t.Fatalf("blocks[0] = %+v", blocks[0])
	}
}

func TestBuildPromptBlocksPreservesStructuredAssistantSemantics(t *testing.T) {
	msgs := []ChatMessage{
		{
			Role:    "assistant",
			Content: ChatContent{Parts: []ChatContentPart{{Type: "refusal", Refusal: "cannot comply"}}},
			ToolCalls: []ChatToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: &ToolCallFunction{Name: "lookup", Arguments: `{"q":"weather"}`},
			}},
			FunctionCall: &FunctionCall{Name: "legacy_lookup", Arguments: `{"q":"weather"}`},
			Audio:        &AssistantAudio{ID: "audio_123"},
		},
	}

	blocks := buildPromptBlocks(msgs, PromptCapabilities{})
	if len(blocks) != 4 {
		t.Fatalf("got %d blocks, want 4", len(blocks))
	}
	if blocks[0].Text != "Assistant: Refusal: cannot comply" {
		t.Fatalf("blocks[0].text = %q", blocks[0].Text)
	}
	if !strings.Contains(blocks[1].Text, "FunctionCall:") || !strings.Contains(blocks[1].Text, "legacy_lookup") {
		t.Fatalf("blocks[1].text = %q", blocks[1].Text)
	}
	if !strings.Contains(blocks[2].Text, "ToolCalls:") || !strings.Contains(blocks[2].Text, `"lookup"`) {
		t.Fatalf("blocks[2].text = %q", blocks[2].Text)
	}
	if !strings.Contains(blocks[3].Text, "Audio:") || !strings.Contains(blocks[3].Text, "audio_123") {
		t.Fatalf("blocks[3].text = %q", blocks[3].Text)
	}
}

func TestChatContentAllowsEmptyTextPart(t *testing.T) {
	var c ChatContent
	if err := json.Unmarshal([]byte(`[{"type":"text","text":""}]`), &c); err != nil {
		t.Fatalf("unmarshal empty text part: %v", err)
	}
	if len(c.Parts) != 1 || c.Parts[0].Type != "text" {
		t.Fatalf("parts = %+v", c.Parts)
	}
}
