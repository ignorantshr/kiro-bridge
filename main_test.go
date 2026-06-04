package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBridgeHolderGetReturnsNilBeforeSet(t *testing.T) {
	h := &bridgeHolder{}
	if h.Get() != nil {
		t.Fatal("expected nil before Set")
	}
}

func TestBridgeHolderSetThenGet(t *testing.T) {
	h := &bridgeHolder{}
	mock := &mockBridge{}
	h.Set(mock)
	if h.Get() != mock {
		t.Fatal("expected mock bridge after Set")
	}
}

func TestConnectWithBackoffSucceedsImmediately(t *testing.T) {
	orig := newBridgeFunc
	defer func() { newBridgeFunc = orig }()

	mock := &mockBridge{}
	newBridgeFunc = func(cfg BridgeConfig) (Bridge, error) {
		return mock, nil
	}

	holder := &bridgeHolder{}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		connectWithBackoff(BridgeConfig{}, holder, stop)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("connectWithBackoff did not return")
	}

	if holder.Get() != mock {
		t.Fatal("expected bridge to be set")
	}
}

func TestConnectWithBackoffRetriesThenSucceeds(t *testing.T) {
	orig := newBridgeFunc
	defer func() { newBridgeFunc = orig }()

	calls := 0
	mock := &mockBridge{}
	newBridgeFunc = func(cfg BridgeConfig) (Bridge, error) {
		calls++
		if calls < 3 {
			return nil, http.ErrServerClosed // any error
		}
		return mock, nil
	}

	holder := &bridgeHolder{}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		connectWithBackoff(BridgeConfig{}, holder, stop)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("connectWithBackoff did not return")
	}

	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
	if holder.Get() != mock {
		t.Fatal("expected bridge to be set after retries")
	}
}

func TestConnectWithBackoffStopsOnSignal(t *testing.T) {
	orig := newBridgeFunc
	defer func() { newBridgeFunc = orig }()

	newBridgeFunc = func(cfg BridgeConfig) (Bridge, error) {
		return nil, http.ErrServerClosed
	}

	holder := &bridgeHolder{}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		connectWithBackoff(BridgeConfig{}, holder, stop)
		close(done)
	}()

	// Let it fail at least once, then stop
	time.Sleep(100 * time.Millisecond)
	close(stop)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("connectWithBackoff did not stop")
	}

	if holder.Get() != nil {
		t.Fatal("expected bridge to remain nil after stop")
	}
}

func TestHandlerReturns503WhenBridgeNil(t *testing.T) {
	holder := &bridgeHolder{}

	handler := func(w http.ResponseWriter, r *http.Request) {
		b := holder.Get()
		if b == nil || !b.Ready() {
			http.Error(w, "bridge not ready", http.StatusServiceUnavailable)
			return
		}
		handleChatCompletions(b)(w, r)
	}

	body := `{"model":"kiro","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "bridge not ready") {
		t.Errorf("body = %q, want 'bridge not ready'", w.Body.String())
	}
}

func TestEnvDefaults(t *testing.T) {
	t.Run("returns fallback when unset", func(t *testing.T) {
		if got := env("KIRO_BRIDGE_UNSET_12345", "fallback"); got != "fallback" {
			t.Errorf("got %q, want %q", got, "fallback")
		}
	})
	t.Run("returns value when set", func(t *testing.T) {
		t.Setenv("KIRO_BRIDGE_TEST_VAR", "custom")
		if got := env("KIRO_BRIDGE_TEST_VAR", "default"); got != "custom" {
			t.Errorf("got %q, want %q", got, "custom")
		}
	})
	t.Run("PORT defaults to 11435", func(t *testing.T) {
		t.Setenv("KIRO_BRIDGE_PORT", "")
		if got := env("KIRO_BRIDGE_PORT", "11435"); got != "11435" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("PORT override", func(t *testing.T) {
		t.Setenv("KIRO_BRIDGE_PORT", "9999")
		if got := env("KIRO_BRIDGE_PORT", "11435"); got != "9999" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("CWD defaults to .", func(t *testing.T) {
		if got := env("KIRO_BRIDGE_CWD", "."); got != "." {
			t.Errorf("got %q", got)
		}
	})
	t.Run("CWD override", func(t *testing.T) {
		t.Setenv("KIRO_BRIDGE_CWD", "/tmp")
		if got := env("KIRO_BRIDGE_CWD", "."); got != "/tmp" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("CLI_PATH defaults to kiro-cli", func(t *testing.T) {
		if got := env("KIRO_CLI_PATH", "kiro-cli"); got != "kiro-cli" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("CLI_PATH override", func(t *testing.T) {
		t.Setenv("KIRO_CLI_PATH", "/usr/local/bin/kiro-cli")
		if got := env("KIRO_CLI_PATH", "kiro-cli"); got != "/usr/local/bin/kiro-cli" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("AGENT defaults to kiro-bridge", func(t *testing.T) {
		if got := env("KIRO_BRIDGE_AGENT", "kiro-bridge"); got != "kiro-bridge" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("AGENT override", func(t *testing.T) {
		t.Setenv("KIRO_BRIDGE_AGENT", "custom-agent")
		if got := env("KIRO_BRIDGE_AGENT", "kiro-bridge"); got != "custom-agent" {
			t.Errorf("got %q", got)
		}
	})
}

func TestLoadBridgeAPIKey(t *testing.T) {
	t.Run("returns error when unset", func(t *testing.T) {
		t.Setenv("KIRO_BRIDGE_API_KEY", "")
		_, err := loadBridgeAPIKey()
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("trims configured token", func(t *testing.T) {
		t.Setenv("KIRO_BRIDGE_API_KEY", "  secret-token  ")
		apiKey, err := loadBridgeAPIKey()
		if err != nil {
			t.Fatalf("loadBridgeAPIKey: %v", err)
		}
		if apiKey != "secret-token" {
			t.Fatalf("apiKey = %q, want %q", apiKey, "secret-token")
		}
	})
}

func TestIsAuthorizedBearerToken(t *testing.T) {
	testCases := []struct {
		name   string
		header string
		want   bool
	}{
		{name: "missing header"},
		{name: "wrong scheme", header: "Basic secret-token"},
		{name: "missing token", header: "Bearer"},
		{name: "wrong token", header: "Bearer nope"},
		{name: "correct token", header: "Bearer secret-token", want: true},
		{name: "case-insensitive scheme", header: "bearer secret-token", want: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := isAuthorizedBearerToken(tc.header, "secret-token")
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBearerAuthMiddleware(t *testing.T) {
	handler := bearerAuthMiddleware("secret-token")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	t.Run("rejects missing token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		if got := w.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
			t.Fatalf("WWW-Authenticate = %q, want Bearer", got)
		}

		var resp openAIErrorResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal error response: %v", err)
		}
		if resp.Error.Type != "authentication_error" {
			t.Fatalf("type = %q, want authentication_error", resp.Error.Type)
		}
		if resp.Error.Code != "invalid_api_key" {
			t.Fatalf("code = %q, want invalid_api_key", resp.Error.Code)
		}
	})

	t.Run("allows correct token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer secret-token")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", w.Code)
		}
	})
}

func TestStderrWriterAlwaysLogs(t *testing.T) {
	var w stderrWriter
	w.prefix = "[test] "
	n, err := w.Write([]byte("something went wrong"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 20 {
		t.Errorf("n = %d, want 20", n)
	}
	// The key assertion: this should log regardless of verboseLog setting
	// We can't easily capture log output, but we verify the type exists
	// and accepts writes without the verbose flag
}

func TestHealthz(t *testing.T) {
	t.Run("returns 200 when bridge connected", func(t *testing.T) {
		holder := &bridgeHolder{}
		holder.Set(&mockBridge{})

		handler := handleHealthz(holder)
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
		if !strings.Contains(w.Body.String(), "ok") {
			t.Errorf("body = %q, want ok", w.Body.String())
		}
	})
	t.Run("returns 503 when bridge not ready", func(t *testing.T) {
		holder := &bridgeHolder{}

		handler := handleHealthz(holder)
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", w.Code)
		}
	})
}
