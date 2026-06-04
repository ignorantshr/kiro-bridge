//go:build e2e

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func waitForServer(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url + "/v1/models")
		if err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("server not ready after %s", timeout)
}

func TestE2EStreamResponse(t *testing.T) {
	port := "19876"
	b, err := NewBridge(BridgeConfig{CLIPath: "kiro-cli", CWD: ".", Agent: "kiro-bridge", Version: "test"})
	if err != nil {
		t.Fatalf("failed to start bridge: %v", err)
	}
	defer b.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handleChatCompletions(b))
	mux.HandleFunc("/v1/models", handleModels(b))
	server := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%s", port), Handler: mux}
	go server.ListenAndServe()
	defer server.Close()

	base := fmt.Sprintf("http://127.0.0.1:%s", port)
	if err := waitForServer(base, 10*time.Second); err != nil {
		t.Fatal(err)
	}

	// Send a streaming request
	body := `{"model":"kiro","messages":[{"role":"user","content":"Reply with exactly: hello"}],"stream":true}`
	resp, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	// Parse SSE events
	scanner := bufio.NewScanner(resp.Body)
	var chunks []string
	var gotDone bool
	var firstHasRole bool

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			gotDone = true
			break
		}
		var chunk ChatCompletionResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("failed to parse chunk: %v\ndata: %s", err, data)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Errorf("object = %q, want chat.completion.chunk", chunk.Object)
		}
		if len(chunk.Choices) != 1 {
			t.Fatalf("choices = %d, want 1", len(chunk.Choices))
		}
		if chunk.Choices[0].Delta != nil {
			if chunk.Choices[0].Delta.Role == "assistant" {
				firstHasRole = true
			}
			if chunk.Choices[0].Delta.Content.Text != "" {
				chunks = append(chunks, chunk.Choices[0].Delta.Content.Text)
			}
		}
	}

	if !gotDone {
		t.Error("never received [DONE]")
	}
	if !firstHasRole {
		t.Error("first chunk missing role=assistant")
	}
	if len(chunks) == 0 {
		t.Error("received no content chunks")
	}

	full := strings.Join(chunks, "")
	t.Logf("response: %s", full)
}

func TestE2ENonStreamResponse(t *testing.T) {
	port := "19877"
	b, err := NewBridge(BridgeConfig{CLIPath: "kiro-cli", CWD: ".", Agent: "kiro-bridge", Version: "test"})
	if err != nil {
		t.Fatalf("failed to start bridge: %v", err)
	}
	defer b.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handleChatCompletions(b))
	mux.HandleFunc("/v1/models", handleModels(b))
	server := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%s", port), Handler: mux}
	go server.ListenAndServe()
	defer server.Close()

	base := fmt.Sprintf("http://127.0.0.1:%s", port)
	if err := waitForServer(base, 10*time.Second); err != nil {
		t.Fatal(err)
	}

	body := `{"model":"kiro","messages":[{"role":"user","content":"Reply with exactly: hello"}]}`
	resp, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var result ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", result.Object)
	}
	if len(result.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(result.Choices))
	}
	if result.Choices[0].Message == nil {
		t.Fatal("message is nil")
	}
	if result.Choices[0].Message.Content.Text == "" {
		t.Error("empty response content")
	}

	t.Logf("response: %s", result.Choices[0].Message.Content.Text)
}
