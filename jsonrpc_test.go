package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewRequest(t *testing.T) {
	data, err := newRequest(1, "initialize", InitializeParams{
		ProtocolVersion: 1,
		ClientInfo:      ClientInfo{Name: "kiro-bridge", Title: "Kiro Bridge", Version: "0.1.0"},
	})
	if err != nil {
		t.Fatal(err)
	}

	var req Request
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatal(err)
	}
	if req.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want %q", req.JSONRPC, "2.0")
	}
	if req.ID.Num != 1 {
		t.Errorf("id = %d, want 1", req.ID.Num)
	}
	if req.Method != "initialize" {
		t.Errorf("method = %q, want %q", req.Method, "initialize")
	}

	var params InitializeParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		t.Fatal(err)
	}
	if params.ProtocolVersion != 1 {
		t.Errorf("protocolVersion = %d, want 1", params.ProtocolVersion)
	}
	if params.ClientInfo.Name != "kiro-bridge" {
		t.Errorf("clientInfo.name = %q, want %q", params.ClientInfo.Name, "kiro-bridge")
	}
}

func TestSessionUpdateParsing(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantUpdate string
		wantText   string
	}{
		{
			name:       "agent message chunk",
			input:      `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"}}`,
			wantUpdate: "agent_message_chunk",
			wantText:   "hello",
		},
		{
			name:       "tool call",
			input:      `{"sessionUpdate":"tool_call","toolCallId":"call_1","status":"pending"}`,
			wantUpdate: "tool_call",
		},
		{
			name:       "tool call update completed",
			input:      `{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"completed"}`,
			wantUpdate: "tool_call_update",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var su SessionUpdate
			if err := json.Unmarshal([]byte(tt.input), &su); err != nil {
				t.Fatal(err)
			}
			if su.SessionUpdate != tt.wantUpdate {
				t.Errorf("sessionUpdate = %q, want %q", su.SessionUpdate, tt.wantUpdate)
			}
			if tt.wantText != "" && su.ContentText() != tt.wantText {
				t.Errorf("content.text = %q, want %q", su.ContentText(), tt.wantText)
			}
		})
	}
}

func TestIsResponse(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{
			name: "response with id",
			line: `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`,
			want: true,
		},
		{
			name: "notification without id",
			line: `{"jsonrpc":"2.0","method":"session/update","params":{}}`,
			want: false,
		},
		{
			name: "error response with id",
			line: `{"jsonrpc":"2.0","id":2,"error":{"code":-1,"message":"fail"}}`,
			want: true,
		},
		{
			name: "invalid json",
			line: `not json`,
			want: false,
		},
		{
			name: "empty object",
			line: `{}`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isResponse([]byte(tt.line))
			if got != tt.want {
				t.Errorf("isResponse(%s) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

func TestSessionNewRequestIncludesMcpServers(t *testing.T) {
	data, err := newRequest(1, "session/new", NewSessionNewParams("/tmp"))
	if err != nil {
		t.Fatal(err)
	}

	// Must contain "mcpServers":[] not "mcpServers":null
	if !strings.Contains(string(data), `"mcpServers":[]`) {
		t.Errorf("expected mcpServers:[], got: %s", data)
	}
}

func TestRPCErrorParseWithData(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"Internal error","data":"session expired"}}`
	var resp Response
	if err := json.Unmarshal([]byte(input), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if resp.Error.Code != -32603 {
		t.Errorf("code = %d, want -32603", resp.Error.Code)
	}
	if resp.Error.Message != "Internal error" {
		t.Errorf("message = %q, want %q", resp.Error.Message, "Internal error")
	}
	if resp.Error.Data == nil {
		t.Fatal("expected data to be present")
	}
	if string(resp.Error.Data) != `"session expired"` {
		t.Errorf("data = %s, want %q", resp.Error.Data, `"session expired"`)
	}
}

func TestRPCErrorParseWithoutData(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`
	var resp Response
	if err := json.Unmarshal([]byte(input), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error.Data != nil {
		t.Errorf("expected nil data, got %s", resp.Error.Data)
	}
}

func TestRPCErrorParseWithStructuredData(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"Server error","data":{"detail":"rate limited","retry_after":30}}}`
	var resp Response
	if err := json.Unmarshal([]byte(input), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error.Data == nil {
		t.Fatal("expected structured data")
	}
	if !strings.Contains(string(resp.Error.Data), "rate limited") {
		t.Errorf("data = %s, want rate limited info", resp.Error.Data)
	}
}

func TestRPCErrorFormatIncludesData(t *testing.T) {
	err := &RPCError{Code: -32603, Message: "Internal error", Data: json.RawMessage(`"extra info"`)}
	got := err.Error()
	if !strings.Contains(got, "code -32603") {
		t.Errorf("error = %q, missing code", got)
	}
	if !strings.Contains(got, "Internal error") {
		t.Errorf("error = %q, missing message", got)
	}
	if !strings.Contains(got, "extra info") {
		t.Errorf("error = %q, missing data", got)
	}
}

func TestRPCErrorFormatOmitsDataWhenAbsent(t *testing.T) {
	err := &RPCError{Code: -32601, Message: "Method not found"}
	got := err.Error()
	if !strings.Contains(got, "code -32601") {
		t.Errorf("error = %q, missing code", got)
	}
	if !strings.Contains(got, "Method not found") {
		t.Errorf("error = %q, missing message", got)
	}
	if strings.Contains(got, "data") {
		t.Errorf("error = %q, should not contain data", got)
	}
}

func TestRPCIDUnmarshalInt(t *testing.T) {
	var id RPCID
	if err := json.Unmarshal([]byte(`42`), &id); err != nil {
		t.Fatal(err)
	}
	if id.Str != "" || id.Num != 42 {
		t.Errorf("got %+v, want Num=42", id)
	}
}

func TestRPCIDUnmarshalString(t *testing.T) {
	var id RPCID
	if err := json.Unmarshal([]byte(`"c0104696-fd9f-46a0-ad16-be66b3f71402"`), &id); err != nil {
		t.Fatal(err)
	}
	if id.Str != "c0104696-fd9f-46a0-ad16-be66b3f71402" {
		t.Errorf("got %+v, want Str=UUID", id)
	}
}

func TestRPCIDMarshalInt(t *testing.T) {
	id := RPCID{Num: 5}
	data, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "5" {
		t.Errorf("got %s, want 5", data)
	}
}

func TestRPCIDMarshalString(t *testing.T) {
	id := RPCID{Str: "abc-123"}
	data, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `"abc-123"` {
		t.Errorf("got %s, want %q", data, "abc-123")
	}
}

func TestResponseWithStringID(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":"c0104696-fd9f","result":{}}`
	var resp Response
	if err := json.Unmarshal([]byte(input), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID.Str != "c0104696-fd9f" {
		t.Errorf("id = %+v, want Str=c0104696-fd9f", resp.ID)
	}
}

func TestResponseWithIntID(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`
	var resp Response
	if err := json.Unmarshal([]byte(input), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID.Num != 3 {
		t.Errorf("id = %+v, want Num=3", resp.ID)
	}
}

func TestIsResponseWithStringID(t *testing.T) {
	line := `{"jsonrpc":"2.0","id":"uuid-here","result":{}}`
	if !isResponse([]byte(line)) {
		t.Error("expected isResponse=true for string ID response")
	}
}

func TestIncomingRequestHasIDAndMethod(t *testing.T) {
	line := `{"jsonrpc":"2.0","id":"c010","method":"session/request_permission","params":{}}`
	msg := classifyMessage([]byte(line))
	if msg != messageRequest {
		t.Errorf("got %v, want messageRequest", msg)
	}
}

func TestNotificationHasMethodNoID(t *testing.T) {
	line := `{"jsonrpc":"2.0","method":"session/update","params":{}}`
	msg := classifyMessage([]byte(line))
	if msg != messageNotification {
		t.Errorf("got %v, want messageNotification", msg)
	}
}

func TestResponseHasIDNoMethod(t *testing.T) {
	line := `{"jsonrpc":"2.0","id":1,"result":{}}`
	msg := classifyMessage([]byte(line))
	if msg != messageResponse {
		t.Errorf("got %v, want messageResponse", msg)
	}
}

func TestNewResponseMarshal(t *testing.T) {
	id := RPCID{Str: "abc-123"}
	data, err := newResponse(id, json.RawMessage(`{"outcome":{"outcome":"selected","optionId":"allow_once"}}`))
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"id":"abc-123"`) {
		t.Errorf("missing string id: %s", s)
	}
	if !strings.Contains(s, `"result":{`) {
		t.Errorf("missing result: %s", s)
	}
	if !strings.Contains(s, `"jsonrpc":"2.0"`) {
		t.Errorf("missing jsonrpc: %s", s)
	}
}

func TestSessionUpdateToolCallContentArray(t *testing.T) {
	// ACP tool_call sends content as an array, not a single ContentBlock
	input := `{"sessionUpdate":"tool_call","toolCallId":"call_1","title":"Creating file","content":[{"type":"diff","path":"/tmp/test.txt","oldText":null,"newText":"hello\n"}]}`
	var su SessionUpdate
	err := json.Unmarshal([]byte(input), &su)
	if err != nil {
		t.Fatalf("should parse without error, got: %v", err)
	}
	if su.ToolCallID != "call_1" {
		t.Errorf("toolCallId = %q, want %q", su.ToolCallID, "call_1")
	}
}

func TestContentBlockSupportsSchemaFields(t *testing.T) {
	block := ContentBlock{
		Type:        "resource_link",
		URI:         "file:///tmp/example.txt",
		Name:        "example.txt",
		Title:       "Example",
		Description: "Schema-aligned resource link",
		Size:        12,
	}
	data, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"type":"resource_link"`) {
		t.Fatalf("missing type: %s", s)
	}
	if !strings.Contains(s, `"uri":"file:///tmp/example.txt"`) {
		t.Fatalf("missing uri: %s", s)
	}
	if !strings.Contains(s, `"name":"example.txt"`) {
		t.Fatalf("missing name: %s", s)
	}
}

func TestInitializeParamsPromptCapabilitiesIncludeSchemaFlags(t *testing.T) {
	params := InitializeParams{
		ProtocolVersion: 1,
		ClientCapabilities: ClientCapabilities{
			PromptCapabilities: &PromptCapabilities{
				Image:           true,
				Audio:           true,
				EmbeddedContext: true,
			},
		},
	}
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"image":true`) {
		t.Fatalf("missing image capability: %s", s)
	}
	if !strings.Contains(s, `"audio":true`) {
		t.Fatalf("missing audio capability: %s", s)
	}
	if !strings.Contains(s, `"embeddedContext":true`) {
		t.Fatalf("missing embeddedContext capability: %s", s)
	}
}
