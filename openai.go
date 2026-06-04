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

// ChatMessage is the bridge's normalized representation of an OpenAI message,
// including optional tool-call metadata for response encoding.
type ChatMessage struct {
	Role      string      `json:"role,omitempty"`
	Content   ChatContent `json:"content"`
	ToolCalls []ToolCall  `json:"tool_calls,omitempty"`
}

type ToolCall struct {
	Index    int              `json:"index"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// ChatContent handles both "content": "string" and "content": [{"type":"text","text":"..."}]
type ChatContent struct {
	Text   string
	Images []ImageContent
}

// ImageContent stores one parsed image payload that can be forwarded as an ACP
// image content block when image support is enabled.
type ImageContent struct {
	MimeType string
	Data     string
}

func (c *ChatContent) UnmarshalJSON(data []byte) error {
	c.Text = ""
	c.Images = nil

	if string(data) == "null" {
		return nil
	}

	// Try string first
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		c.Text = s
		return nil
	}
	// Try array of content parts
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(data, &parts); err == nil {
		for _, p := range parts {
			switch p.Type {
			case "text":
				c.Text += p.Text
			case "image_url":
				if p.ImageURL != nil {
					mime, b64 := parseDataURI(p.ImageURL.URL)
					if b64 != "" {
						c.Images = append(c.Images, ImageContent{MimeType: mime, Data: b64})
					}
				}
			}
		}
		return nil
	}

	return fmt.Errorf("unsupported content format")
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

func (c ChatContent) MarshalJSON() ([]byte, error) {
	return json.Marshal(c.Text)
}

// ChatCompletionResponse is used for both non-streaming responses and SSE
// chunks, mirroring the OpenAI chat completions response envelope.
type ChatCompletionResponse struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Created int64           `json:"created"`
	Model   string          `json:"model"`
	Choices []ChatChoice    `json:"choices"`
	Usage   *ChatUsage      `json:"usage,omitempty"`
}

// ChatUsage carries the token accounting exposed back to the OpenAI client.
type ChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatChoice models either a final assistant message or an incremental stream
// delta depending on which fields are populated.
type ChatChoice struct {
	Index        int          `json:"index"`
	Message      *ChatMessage `json:"message,omitempty"`
	Delta        *ChatMessage `json:"delta,omitempty"`
	FinishReason *string      `json:"finish_reason"`
}
