package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

func buildPromptText(messages []ChatMessage) string {
	var parts []string
	for _, m := range messages {
		switch m.Role {
		case "system":
			parts = append(parts, "System: "+m.Content.Text)
		case "user":
			parts = append(parts, m.Content.Text)
		case "assistant":
			if replayHistory {
				parts = append(parts, "Assistant: "+m.Content.Text)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func buildPromptBlocks(messages []ChatMessage) []ContentBlock {
	text := buildPromptText(messages)
	blocks := []ContentBlock{{Type: "text", Text: text}}
	if enableImages {
		for _, m := range messages {
			for _, img := range m.Content.Images {
				blocks = append(blocks, ContentBlock{Type: "image", MimeType: img.MimeType, Data: img.Data})
			}
		}
	}
	return blocks
}

var completionCounter int64

var maxBodyBytes int64 = 1 << 20 // 1MB default

var showToolAnnotations = os.Getenv("KIRO_BRIDGE_SHOW_TOOLS") != ""
var replayHistory = os.Getenv("KIRO_BRIDGE_REPLAY_HISTORY") != ""
var enableImages = os.Getenv("KIRO_BRIDGE_ENABLE_IMAGES") != ""
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

		if len(req.Messages) == 0 {
			log.Printf("error: empty messages")
			http.Error(w, "messages required", http.StatusBadRequest)
			return
		}

		promptText := buildPromptText(req.Messages)
		promptBlocks := buildPromptBlocks(req.Messages)
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
		var delta *ChatMessage
		switch ev.Type {
		case EventText:
			delta = &ChatMessage{Content: ChatContent{Text: ev.Text}}
		case EventToolCall:
			if !showToolAnnotations {
				return
			}
			delta = &ChatMessage{Content: ChatContent{Text: fmt.Sprintf("\n\n🔧 %s\n\n---\n\n", ev.ToolName)}}
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
			Message:      &ChatMessage{Role: "assistant", Content: ChatContent{Text: full.String()}},
			FinishReason: &fr,
		}},
		Usage: &ChatUsage{TotalTokens: usage.TotalTokens},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
