package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// buildPromptText renders the ordered text transcript used for debug logging and
// for text-only fallbacks when building ACP prompt blocks.
func buildPromptText(messages []ChatMessage, caps PromptCapabilities) string {
	var parts []string
	for _, block := range buildPromptBlocks(messages, caps) {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// buildPromptBlocks preserves message order and, when possible, the original
// OpenAI semantics while projecting them onto ACP's linear content-block model.
func buildPromptBlocks(messages []ChatMessage, caps PromptCapabilities) []ContentBlock {
	var blocks []ContentBlock
	for _, m := range messages {
		blocks = append(blocks, promptBlocksForMessage(m, caps)...)
	}
	if len(blocks) == 0 {
		return []ContentBlock{{Type: "text", Text: ""}}
	}
	return blocks
}

func messageRoleLabel(role string) (string, bool) {
	switch role {
	case "developer":
		return "Developer", true
	case "system":
		return "System", true
	case "user":
		return "User", true
	case "assistant":
		return "Assistant", true
	case "function":
		return "Function", true
	case "tool":
		return "Tool", true
	default:
		return "", false
	}
}

func orderedContentParts(content ChatContent) []ChatContentPart {
	return content.OrderedParts()
}

func promptBlocksForMessage(message ChatMessage, caps PromptCapabilities) []ContentBlock {
	label, include := messageRoleLabel(message.Role)
	if !include {
		return nil
	}

	header := label
	if message.Name != "" {
		header += "[" + message.Name + "]"
	}
	if message.ToolCallID != "" {
		header += "[" + message.ToolCallID + "]"
	}
	header += ": "

	var blocks []ContentBlock
	headerPending := true
	for _, part := range orderedContentParts(message.Content) {
		switch part.Type {
		case "text":
			blocks = appendTextPromptBlock(blocks, &headerPending, header, part.Text)
		case "refusal":
			blocks = appendTextPromptBlock(blocks, &headerPending, header, "Refusal: "+part.Refusal)
		case "image_url":
			if block, ok := imagePromptBlock(part.ImageURL, caps); ok {
				blocks = appendHeaderIfNeeded(blocks, &headerPending, header)
				blocks = append(blocks, block)
				continue
			}
			blocks = appendStructuredPromptBlock(blocks, &headerPending, header, "Image", part)
		case "input_audio":
			if block, ok := audioPromptBlock(part.InputAudio, caps); ok {
				blocks = appendHeaderIfNeeded(blocks, &headerPending, header)
				blocks = append(blocks, block)
				continue
			}
			blocks = appendStructuredPromptBlock(blocks, &headerPending, header, "InputAudio", part)
		case "file":
			if block, ok := filePromptBlock(part.File, caps); ok {
				blocks = appendHeaderIfNeeded(blocks, &headerPending, header)
				blocks = append(blocks, block)
				continue
			}
			blocks = appendStructuredPromptBlock(blocks, &headerPending, header, "File", part)
		}
	}

	if message.FunctionCall != nil {
		blocks = appendStructuredPromptBlock(blocks, &headerPending, header, "FunctionCall", message.FunctionCall)
	}
	if len(message.ToolCalls) > 0 {
		blocks = appendStructuredPromptBlock(blocks, &headerPending, header, "ToolCalls", message.ToolCalls)
	}
	if message.Audio != nil {
		blocks = appendStructuredPromptBlock(blocks, &headerPending, header, "Audio", message.Audio)
	}
	if headerPending {
		blocks = append(blocks, ContentBlock{Type: "text", Text: strings.TrimSpace(header)})
	}
	return blocks
}

func appendHeaderIfNeeded(blocks []ContentBlock, headerPending *bool, header string) []ContentBlock {
	if *headerPending {
		blocks = append(blocks, ContentBlock{Type: "text", Text: strings.TrimSpace(header)})
		*headerPending = false
	}
	return blocks
}

func appendTextPromptBlock(blocks []ContentBlock, headerPending *bool, header, text string) []ContentBlock {
	if text == "" {
		return blocks
	}
	if *headerPending {
		text = header + text
		*headerPending = false
	}
	return append(blocks, ContentBlock{Type: "text", Text: text})
}

func appendStructuredPromptBlock(blocks []ContentBlock, headerPending *bool, header, label string, payload any) []ContentBlock {
	data, err := json.Marshal(payload)
	if err != nil {
		return appendTextPromptBlock(blocks, headerPending, header, label)
	}
	return appendTextPromptBlock(blocks, headerPending, header, fmt.Sprintf("%s: %s", label, data))
}

// imagePromptBlock upgrades OpenAI image_url parts to native ACP image blocks
// only when the negotiated prompt capabilities explicitly allow image input.
func imagePromptBlock(part *ImageURLPart, caps PromptCapabilities) (ContentBlock, bool) {
	if !caps.Image || part == nil {
		return ContentBlock{}, false
	}
	mimeType, data := parseDataURI(part.URL)
	if data == "" {
		return ContentBlock{}, false
	}
	return ContentBlock{Type: "image", MimeType: mimeType, Data: data}, true
}

// audioPromptBlock upgrades OpenAI input_audio parts to native ACP audio blocks
// only when the negotiated prompt capabilities explicitly allow audio input.
func audioPromptBlock(part *InputAudioPart, caps PromptCapabilities) (ContentBlock, bool) {
	if !caps.Audio || part == nil || part.Data == "" {
		return ContentBlock{}, false
	}
	mimeType, ok := openAIInputAudioMimeType(part.Format)
	if !ok {
		return ContentBlock{}, false
	}
	return ContentBlock{Type: "audio", MimeType: mimeType, Data: part.Data}, true
}

func openAIInputAudioMimeType(format string) (string, bool) {
	switch strings.ToLower(format) {
	case "wav":
		return "audio/wav", true
	case "mp3":
		return "audio/mpeg", true
	default:
		return "", false
	}
}

// filePromptBlock upgrades inline OpenAI file content into an embedded ACP
// resource. File IDs without inline bytes still fall back to structured text
// because the bridge has no dereferenceable URI for the agent to load.
func filePromptBlock(part *FileContentPart, caps PromptCapabilities) (ContentBlock, bool) {
	if !caps.EmbeddedContext || part == nil || part.FileData == "" {
		return ContentBlock{}, false
	}
	decoded, err := base64.StdEncoding.DecodeString(part.FileData)
	if err != nil {
		return ContentBlock{}, false
	}
	mimeType := mime.TypeByExtension(strings.ToLower(fileExtension(part.Filename)))
	resource := &EmbeddedResource{
		URI:      embeddedResourceURI(part),
		MimeType: mimeType,
	}
	if text, ok := embeddedResourceText(decoded, mimeType); ok {
		resource.Text = text
		if resource.MimeType == "" {
			resource.MimeType = "text/plain"
		}
	} else {
		resource.Blob = part.FileData
		if resource.MimeType == "" {
			resource.MimeType = "application/octet-stream"
		}
	}
	return ContentBlock{Type: "resource", Resource: resource}, true
}

// embeddedResourceURI reuses the original file identifier when possible instead
// of inventing a synthetic scheme-specific URI.
func embeddedResourceURI(part *FileContentPart) string {
	if part == nil {
		return "embedded"
	}
	switch {
	case part.FileID != "":
		return part.FileID
	case part.Filename != "":
		return part.Filename
	default:
		return "embedded"
	}
}

func embeddedResourceText(decoded []byte, mimeType string) (string, bool) {
	if !utf8.Valid(decoded) {
		return "", false
	}
	if mimeType == "" || strings.HasPrefix(mimeType, "text/") {
		return string(decoded), true
	}
	switch mimeType {
	case "application/json",
		"application/xml",
		"application/yaml",
		"application/x-yaml",
		"application/javascript",
		"application/x-javascript",
		"application/ecmascript",
		"image/svg+xml":
		return string(decoded), true
	default:
		return "", false
	}
}

func fileExtension(name string) string {
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		return name[idx:]
	}
	return ""
}

var completionCounter int64

var maxBodyBytes int64 = 1 << 20 // 1MB default

var showToolAnnotations = os.Getenv("KIRO_BRIDGE_SHOW_TOOLS") != ""
var allIP = os.Getenv("KIRO_BRIDGE_ALL_IP") != ""

func init() {
	if v := os.Getenv("KIRO_BRIDGE_MAX_BODY"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			maxBodyBytes = n
		}
	}
}

func newCompletionID() string {
	return fmt.Sprintf("chatcmpl-%d-%d", time.Now().Unix(), atomic.AddInt64(&completionCounter, 1))
}

func handleChatCompletions(b Bridge) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !b.Ready() {
			http.Error(w, "bridge not ready", http.StatusServiceUnavailable)
			return
		}

		var req ChatCompletionRequest
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("error: decode request: %v", err)
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		caps := b.PromptCapabilities()
		if len(req.Messages) == 0 {
			log.Printf("error: empty messages")
			http.Error(w, "messages required", http.StatusBadRequest)
			return
		}

		promptText := buildPromptText(req.Messages, caps)
		promptBlocks := buildPromptBlocks(req.Messages, caps)
		completionID := newCompletionID()
		created := time.Now().Unix()
		model := req.Model
		if model == "" {
			model = "kiro"
		}

		debugf("prompt: stream=%v model=%q len=%d", req.Stream, model, len(promptText))

		if req.Stream {
			handleStream(r.Context(), w, b, promptBlocks, completionID, created, model)
		} else {
			handleNonStream(r.Context(), w, b, promptBlocks, completionID, created, model)
		}
	}
}

var stopReasonMap = map[string]string{
	"end_turn":          "stop",
	"max_tokens":        "length",
	"max_turn_requests": "stop",
	"refusal":           "stop",
	"cancelled":         "stop",
}

func mapStopReason(acpReason string) string {
	if r, ok := stopReasonMap[acpReason]; ok {
		return r
	}
	return "stop"
}

func writeStreamTerminal(w http.ResponseWriter, flusher http.Flusher, id string, created int64, model, finishReason string) {
	finalJSON := fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","created":%d,"model":%q,"choices":[{"index":0,"delta":{},"finish_reason":%q}]}`, id, created, model, finishReason)
	fmt.Fprintf(w, "data: %s\n\n", finalJSON)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func handleStream(ctx context.Context, w http.ResponseWriter, b Bridge, prompt []ContentBlock, id string, created int64, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	first := true
	streamStarted := false
	stopReason, err := b.Prompt(ctx, prompt, func(ev PromptEvent) {
		var delta *ChatCompletionDelta
		switch ev.Type {
		case EventText:
			delta = &ChatCompletionDelta{Content: ev.Text}
		case EventToolCall:
			if !showToolAnnotations {
				return
			}
			delta = &ChatCompletionDelta{Content: fmt.Sprintf("\n\n🔧 %s\n\n---\n\n", ev.ToolName)}
		default:
			return
		}
		if first {
			delta.Role = "assistant"
			first = false
		}
		resp := ChatCompletionResponse{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []ChatChoice{{Index: 0, Delta: delta}},
		}
		data, err := json.Marshal(resp)
		if err != nil {
			log.Printf("error: marshal chunk: %v", err)
			return
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			log.Printf("error: write chunk: %v", err)
			return
		}
		streamStarted = true
		flusher.Flush()
	})

	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return
		}
		if !streamStarted && errors.Is(err, errBridgeNotReady) {
			http.Error(w, "bridge not ready", http.StatusServiceUnavailable)
			return
		}
		log.Printf("error: prompt failed: %v", err)
		if !streamStarted && first {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeStreamTerminal(w, flusher, id, created, model, "error")
		return
	}

	writeStreamTerminal(w, flusher, id, created, model, mapStopReason(stopReason))
}

func handleNonStream(ctx context.Context, w http.ResponseWriter, b Bridge, prompt []ContentBlock, id string, created int64, model string) {
	var full strings.Builder
	stopReason, err := b.Prompt(ctx, prompt, func(ev PromptEvent) {
		if ev.Type == EventText {
			full.WriteString(ev.Text)
		}
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return
		}
		if errors.Is(err, errBridgeNotReady) {
			http.Error(w, "bridge not ready", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	fr := mapStopReason(stopReason)
	usage := b.Usage()
	resp := ChatCompletionResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []ChatChoice{{
			Index:        0,
			Message:      &ChatCompletionMessage{Role: "assistant", Content: full.String()},
			FinishReason: &fr,
		}},
		Usage: &ChatUsage{TotalTokens: usage.TotalTokens},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
