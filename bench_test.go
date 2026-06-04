package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func BenchmarkBuildPromptText(b *testing.B) {
	msgs := []ChatMessage{
		{Role: "system", Content: textChatContent(strings.Repeat("system context ", 100))},
		{Role: "user", Content: textChatContent("what is 2+2?")},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buildPromptText(msgs, PromptCapabilities{})
	}
}

func BenchmarkHandleNonStream(b *testing.B) {
	mock := &mockBridge{chunks: []string{"The answer is 4."}}
	handler := handleChatCompletions(mock)
	body := `{"model":"kiro","messages":[{"role":"user","content":"what is 2+2?"}]}`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler(w, req)
	}
}

func BenchmarkHandleStream(b *testing.B) {
	mock := &mockBridge{chunks: []string{"The ", "answer ", "is ", "4."}}
	handler := handleChatCompletions(mock)
	body := `{"model":"kiro","messages":[{"role":"user","content":"what is 2+2?"}],"stream":true}`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler(w, req)
	}
}

func BenchmarkNewRequest(b *testing.B) {
	params := SessionPromptParams{
		SessionID: "test-session",
		Content:   []ContentBlock{{Type: "text", Text: "what is 2+2?"}},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		newRequest(1, "session/prompt", params)
	}
}

func BenchmarkChatContentUnmarshalString(b *testing.B) {
	data := []byte(`"hello world"`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var c ChatContent
		json.Unmarshal(data, &c)
	}
}

func BenchmarkChatContentUnmarshalArray(b *testing.B) {
	data := []byte(`[{"type":"text","text":"hello"},{"type":"text","text":" world"}]`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var c ChatContent
		json.Unmarshal(data, &c)
	}
}
