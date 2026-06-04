package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// OpenAI Chat Completions API types

// ChatCompletionRequest is the subset of the OpenAI chat completions request
// shape that the bridge accepts and translates into ACP turns.
type ChatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream,omitempty"`
}

// ChatMessage models one incoming OpenAI chat message. Request parsing keeps
// message-level semantics intact and leaves protocol projection to buildPromptBlocks.
type ChatMessage struct {
	Role         string          `json:"role,omitempty"`
	Name         string          `json:"name,omitempty"`
	Content      ChatContent     `json:"content"`
	ToolCalls    []ChatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID   string          `json:"tool_call_id,omitempty"`
	FunctionCall *FunctionCall   `json:"function_call,omitempty"`
	Audio        *AssistantAudio `json:"audio,omitempty"`
}

// ChatToolCall preserves one assistant tool call embedded in a chat message.
type ChatToolCall struct {
	ID       string            `json:"id,omitempty"`
	Type     string            `json:"type,omitempty"`
	Function *ToolCallFunction `json:"function,omitempty"`
	Custom   *CustomToolCall   `json:"custom,omitempty"`
}

// ToolCallFunction is the function-tool payload used by both requests and responses.
type ToolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// CustomToolCall preserves OpenAI custom tool calls when clients include them
// in assistant history. The bridge does not execute them itself, but it should
// retain the original wire shape.
type CustomToolCall struct {
	Name  string `json:"name"`
	Input string `json:"input"`
}

// FunctionCall preserves the legacy Chat Completions function_call field so the
// bridge can still round-trip older OpenAI-compatible clients.
type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// AssistantAudio captures assistant audio metadata from OpenAI-compatible
// clients. ACP cannot consume it directly, but parsing should preserve it.
type AssistantAudio struct {
	ID string `json:"id,omitempty"`
}

// ChatContent keeps the original OpenAI request shape: either a string or an
// ordered array of content parts.
type ChatContent struct {
	String *string
	Parts  []ChatContentPart
}

func textChatContent(s string) ChatContent {
	return ChatContent{String: &s}
}

// ChatContentPart is one OpenAI request content part.
type ChatContentPart struct {
	Type       string           `json:"type"`
	Text       string           `json:"text,omitempty"`
	ImageURL   *ImageURLPart    `json:"image_url,omitempty"`
	InputAudio *InputAudioPart  `json:"input_audio,omitempty"`
	File       *FileContentPart `json:"file,omitempty"`
	Refusal    string           `json:"refusal,omitempty"`
}

// ImageURLPart preserves the official OpenAI image_url content-part shape.
type ImageURLPart struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// InputAudioPart preserves the official OpenAI input_audio content-part shape.
type InputAudioPart struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

// FileContentPart preserves the official OpenAI file content-part shape.
type FileContentPart struct {
	FileID   string `json:"file_id,omitempty"`
	FileData string `json:"file_data,omitempty"`
	Filename string `json:"filename,omitempty"`
}

func (c *ChatContent) UnmarshalJSON(data []byte) error {
	c.String = nil
	c.Parts = nil

	if string(data) == "null" {
		return nil
	}

	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		c.String = &s
		return nil
	}

	var parts []ChatContentPart
	if err := json.Unmarshal(data, &parts); err == nil {
		for i, p := range parts {
			switch p.Type {
			case "text":
			case "image_url":
				if p.ImageURL == nil {
					return fmt.Errorf("content part %d: image_url part missing image_url object", i)
				}
			case "input_audio":
				if p.InputAudio == nil {
					return fmt.Errorf("content part %d: input_audio part missing input_audio object", i)
				}
			case "file":
				if p.File == nil {
					return fmt.Errorf("content part %d: file part missing file object", i)
				}
			case "refusal":
			default:
				return fmt.Errorf("content part %d: unsupported type %q", i, p.Type)
			}
		}
		c.Parts = parts
		return nil
	}

	return fmt.Errorf("unsupported content format")
}

func (c ChatContent) MarshalJSON() ([]byte, error) {
	if c.String != nil {
		return json.Marshal(*c.String)
	}
	return json.Marshal(c.Parts)
}

// OrderedParts returns content in the same order the client provided it.
func (c ChatContent) OrderedParts() []ChatContentPart {
	if c.String != nil {
		return []ChatContentPart{{Type: "text", Text: *c.String}}
	}
	return append([]ChatContentPart(nil), c.Parts...)
}

// TextValue concatenates text parts for diagnostics and legacy assertions.
func (c ChatContent) TextValue() string {
	if c.String != nil {
		return *c.String
	}
	var b strings.Builder
	for _, part := range c.Parts {
		if part.Type == "text" {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

func parseDataURI(uri string) (mimeType, data string) {
	// data:image/png;base64,iVBOR...
	if !strings.HasPrefix(uri, "data:") {
		return "", ""
	}
	uri = uri[5:] // strip "data:"
	idx := strings.Index(uri, ",")
	if idx < 0 {
		return "", ""
	}
	meta := uri[:idx]
	data = uri[idx+1:]
	meta = strings.TrimSuffix(meta, ";base64")
	return meta, data
}

// ChatCompletionResponse is the OpenAI-compatible response envelope emitted by the bridge.
type ChatCompletionResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   *ChatUsage   `json:"usage,omitempty"`
}

// ChatUsage carries the token accounting exposed back to the OpenAI client.
type ChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatCompletionMessage models the non-streaming assistant message shape.
type ChatCompletionMessage struct {
	Role         string                  `json:"role,omitempty"`
	Content      string                  `json:"content,omitempty"`
	Refusal      *string                 `json:"refusal,omitempty"`
	Annotations  []ChatMessageAnnotation `json:"annotations,omitempty"`
	Audio        *ChatCompletionAudio    `json:"audio,omitempty"`
	ToolCalls    []ChatToolCall          `json:"tool_calls,omitempty"`
	FunctionCall *FunctionCall           `json:"function_call,omitempty"`
}

// ChatCompletionDelta models one streaming delta chunk.
type ChatCompletionDelta struct {
	Role         string                 `json:"role,omitempty"`
	Content      string                 `json:"content,omitempty"`
	Refusal      string                 `json:"refusal,omitempty"`
	ToolCalls    []ChatToolCallDelta    `json:"tool_calls,omitempty"`
	FunctionCall *ToolCallFunctionDelta `json:"function_call,omitempty"`
}

// ChatChoice models either a final assistant message or an incremental stream
// delta depending on which fields are populated.
type ChatChoice struct {
	Index        int                    `json:"index"`
	Message      *ChatCompletionMessage `json:"message,omitempty"`
	Delta        *ChatCompletionDelta   `json:"delta,omitempty"`
	FinishReason *string                `json:"finish_reason"`
}

// ChatToolCallDelta models the streaming tool_call delta shape, where index is
// part of the protocol and function fields may arrive incrementally.
type ChatToolCallDelta struct {
	Index    int                    `json:"index"`
	ID       string                 `json:"id,omitempty"`
	Type     string                 `json:"type,omitempty"`
	Function *ToolCallFunctionDelta `json:"function,omitempty"`
}

// ToolCallFunctionDelta preserves partial function-call fields in streamed chunks.
type ToolCallFunctionDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ChatMessageAnnotation mirrors the annotation envelope used on assistant messages.
type ChatMessageAnnotation struct {
	Type        string       `json:"type,omitempty"`
	URLCitation *URLCitation `json:"url_citation,omitempty"`
}

// URLCitation is the currently documented chat annotation payload.
type URLCitation struct {
	StartIndex int    `json:"start_index"`
	EndIndex   int    `json:"end_index"`
	Title      string `json:"title"`
	URL        string `json:"url"`
}

// ChatCompletionAudio is the audio metadata attached to assistant messages when
// an audio modality is requested.
type ChatCompletionAudio struct {
	ID         string `json:"id"`
	Data       string `json:"data,omitempty"`
	ExpiresAt  int64  `json:"expires_at,omitempty"`
	Transcript string `json:"transcript,omitempty"`
}
