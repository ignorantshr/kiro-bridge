package main

import (
	"encoding/json"
	"fmt"
)

// RPCID represents a JSON-RPC 2.0 id that can be a string or integer.
type RPCID struct {
	Str string
	Num int
}

func (id RPCID) MarshalJSON() ([]byte, error) {
	if id.Str != "" {
		return json.Marshal(id.Str)
	}
	return json.Marshal(id.Num)
}

func (id *RPCID) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &id.Str)
	}
	return json.Unmarshal(data, &id.Num)
}

func IntID(n int) RPCID    { return RPCID{Num: n} }
func StrID(s string) RPCID { return RPCID{Str: s} }

// Request is a generic JSON-RPC request or request-like envelope used for ACP
// commands coming from either the bridge or the child process.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      RPCID           `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is the generic JSON-RPC response envelope paired with bridge-issued
// ACP requests.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      RPCID           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type messageType int

const (
	messageResponse messageType = iota
	messageRequest
	messageNotification
	messageUnknown
)

func (m messageType) String() string {
	switch m {
	case messageResponse:
		return "response"
	case messageRequest:
		return "request"
	case messageNotification:
		return "notification"
	default:
		return "unknown"
	}
}

// classifyMessage determines if a JSON-RPC message is a response, request, or notification.
// Response: has id + (result or error), no method
// Request: has id + method (expects a response)
// Notification: has method, no id
func classifyMessage(line []byte) messageType {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return messageUnknown
	}
	_, hasID := raw["id"]
	_, hasMethod := raw["method"]
	_, hasResult := raw["result"]
	_, hasError := raw["error"]

	if hasID && (hasResult || hasError) {
		return messageResponse
	}
	if hasID && hasMethod {
		return messageRequest
	}
	if hasMethod && !hasID {
		return messageNotification
	}
	return messageUnknown
}

func isResponse(line []byte) bool {
	return classifyMessage(line) == messageResponse
}

// RPCError preserves ACP/JSON-RPC error payloads so higher layers can report
// agent failures without losing protocol context.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("code %d: %s (data: %s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("code %d: %s", e.Code, e.Message)
}

// Notification is the generic JSON-RPC notification envelope used for ACP
// session updates and Kiro-specific extension events.
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// ACP param/result types

// InitializeParams declares the client capabilities the bridge presents to ACP.
type InitializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
	ClientInfo         ClientInfo         `json:"clientInfo"`
}

// ClientCapabilities is the subset of ACP capabilities the bridge currently advertises.
type ClientCapabilities struct {
	PromptCapabilities *PromptCapabilities `json:"promptCapabilities,omitempty"`
}

// PromptCapabilities describes the ACP prompt content types the bridge may send.
type PromptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

// ClientInfo identifies the bridge to the ACP server during initialization.
type ClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

// InitializeResult captures the server capability metadata returned by ACP.
type InitializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
}

// AgentCapabilities is the subset of initialize response capabilities the
// bridge needs in order to decide which prompt block types it may send.
type AgentCapabilities struct {
	LoadSession        bool               `json:"loadSession,omitempty"`
	PromptCapabilities PromptCapabilities `json:"promptCapabilities"`
}

// SessionNewParams requests a fresh ACP session rooted at a working directory.
type SessionNewParams struct {
	CWD        string `json:"cwd"`
	MCPServers []any  `json:"mcpServers"`
}

func NewSessionNewParams(cwd string) SessionNewParams {
	return SessionNewParams{CWD: cwd, MCPServers: []any{}}
}

// SessionNewResult holds the ACP session identifier and optional model catalog
// returned when a new session is created.
type SessionNewResult struct {
	SessionID string         `json:"sessionId"`
	Models    *SessionModels `json:"models,omitempty"`
}

// SessionLoadParams requests an existing ACP session by identifier.
type SessionLoadParams struct {
	SessionID string `json:"sessionId"`
}

// SessionLoadResult mirrors the load-session response shape used internally by the bridge.
type SessionLoadResult struct {
	SessionID string         `json:"sessionId"`
	Models    *SessionModels `json:"models,omitempty"`
}

// SessionModels is the model catalog returned by ACP for a session context.
type SessionModels struct {
	CurrentModelID  string         `json:"currentModelId"`
	AvailableModels []SessionModel `json:"availableModels"`
}

// SessionModel is one ACP model descriptor inside a session model catalog.
type SessionModel struct {
	ModelID     string `json:"modelId"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// SessionPromptParams is the ACP request payload for one prompt turn.
type SessionPromptParams struct {
	SessionID string         `json:"sessionId"`
	Content   []ContentBlock `json:"prompt"`
}

// ContentBlock mirrors the ACP ContentBlock schema. The bridge currently emits
// only text and, when enabled, image blocks, but it keeps the full schema here
// so protocol handling does not invent unsupported shapes.
type ContentBlock struct {
	Type        string            `json:"type"`
	Text        string            `json:"text,omitempty"`
	MimeType    string            `json:"mimeType,omitempty"`
	Data        string            `json:"data,omitempty"`
	URI         string            `json:"uri,omitempty"`
	Name        string            `json:"name,omitempty"`
	Title       string            `json:"title,omitempty"`
	Description string            `json:"description,omitempty"`
	Size        int64             `json:"size,omitempty"`
	Resource    *EmbeddedResource `json:"resource,omitempty"`
	Annotations json.RawMessage   `json:"annotations,omitempty"`
}

// EmbeddedResource mirrors ACP's embedded resource payload, which can carry
// either inline text content or inline binary content.
type EmbeddedResource struct {
	URI         string          `json:"uri"`
	MimeType    string          `json:"mimeType,omitempty"`
	Text        string          `json:"text,omitempty"`
	Blob        string          `json:"blob,omitempty"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
}

// SessionPromptResult captures the fallback stop reason returned in the final
// prompt response when no explicit TurnEnd update is observed.
type SessionPromptResult struct {
	StopReason string `json:"stopReason"`
}

// SessionSetModeParams switches an ACP session onto a named Kiro agent mode.
type SessionSetModeParams struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

// SessionCancelParams identifies the ACP session whose current turn should be cancelled.
type SessionCancelParams struct {
	SessionID string `json:"sessionId"`
}

// RequestPermissionParams is the ACP client method payload an agent sends when
// it needs the user to choose a permission option for a tool call.
type RequestPermissionParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  PermissionToolCall `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}

// PermissionToolCall is the subset of tool-call metadata surfaced in ACP
// permission requests and used for user-facing prompts.
type PermissionToolCall struct {
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title,omitempty"`
	Kind       string          `json:"kind,omitempty"`
	RawInput   json.RawMessage `json:"rawInput,omitempty"`
}

// PermissionOption is one selectable outcome presented to the user.
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name,omitempty"`
	Kind     string `json:"kind,omitempty"`
}

// RequestPermissionResult is the client's response payload for
// session/request_permission.
type RequestPermissionResult struct {
	Outcome RequestPermissionOutcome `json:"outcome"`
}

// RequestPermissionOutcome is either a concrete selected option or a cancelled
// result when the current prompt turn was aborted.
type RequestPermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

// SessionUpdateParams wraps one ACP session update notification payload.
type SessionUpdateParams struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

// SessionUpdate is the normalized outer shape used to decode ACP session
// notifications before they are mapped into bridge prompt events.
type SessionUpdate struct {
	SessionUpdate string          `json:"sessionUpdate"`
	Content       json.RawMessage `json:"content,omitempty"`
	ToolCallID    string          `json:"toolCallId,omitempty"`
	Title         string          `json:"title,omitempty"`
	Status        string          `json:"status,omitempty"`
	StopReason    string          `json:"stopReason,omitempty"`
}

// ContentText extracts text from a content field that may be a single ContentBlock or an array.
func (su *SessionUpdate) ContentText() string {
	if len(su.Content) == 0 {
		return ""
	}
	// Try single ContentBlock
	var block ContentBlock
	if err := json.Unmarshal(su.Content, &block); err == nil && block.Text != "" {
		return block.Text
	}
	// Some ACP updates carry content as an array of blocks instead of a single block.
	var blocks []ContentBlock
	if err := json.Unmarshal(su.Content, &blocks); err == nil {
		var text string
		for _, block := range blocks {
			text += block.Text
		}
		return text
	}
	return ""
}

func newRequest(id int, method string, params any) ([]byte, error) {
	p, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Request{JSONRPC: "2.0", ID: IntID(id), Method: method, Params: p})
}

func newResponse(id RPCID, result json.RawMessage) ([]byte, error) {
	return json.Marshal(Response{JSONRPC: "2.0", ID: id, Result: result})
}

func newErrorResponse(id RPCID, code int, message string) ([]byte, error) {
	return json.Marshal(Response{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: message}})
}
