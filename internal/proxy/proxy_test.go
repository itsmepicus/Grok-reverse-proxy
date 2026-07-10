package proxy

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/itsmepicus/grok-reverse-proxy/internal/config"
	"github.com/itsmepicus/grok-reverse-proxy/internal/credential"
)

func TestPrepareChatRequest(t *testing.T) {
	prepared, err := prepareRequest([]byte(`{
        "model":"grok-build","stream":true,"reasoning_effort":"xhigh",
        "messages":[{"role":"system","content":"Be concise"},{"role":"user","content":"Hello"}],
        "tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]
    }`), formatOpenAIChat)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.stream || prepared.model != "grok-4.5" || prepared.publicModel != "grok-build" {
		t.Fatalf("unexpected request metadata: %#v", prepared)
	}
	text := string(prepared.body)
	for _, want := range []string{`"instructions":"Be concise"`, `"model":"grok-4.5"`, `"store":false`, `"effort":"high"`, `"name":"lookup"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("upstream body missing %s: %s", want, text)
		}
	}
}

func TestNonStreamingChatResponse(t *testing.T) {
	upstream := `{"id":"resp_1","created_at":42,"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"}],"usage":{"input_tokens":3,"output_tokens":2,"input_tokens_details":{"cached_tokens":1}}}`
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(upstream))}
	recorder := httptest.NewRecorder()
	if err := writeNonStream(recorder, resp, formatOpenAIChat, "grok-build", "fallback"); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	for _, want := range []string{`"object":"chat.completion"`, `"content":"hello"`, `"finish_reason":"tool_calls"`, `"cached_tokens":1`} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %s: %s", want, body)
		}
	}
}

func TestStreamingChatResponse(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1"}}`, "",
		`data: {"type":"response.output_text.delta","delta":"hello"}`, "",
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":2,"output_tokens":1}}}`, "",
	}, "\n")
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(upstream))}
	recorder := httptest.NewRecorder()
	if err := streamConverted(recorder, resp, formatOpenAIChat, "grok-4.5", "req_1"); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	for _, want := range []string{`"role":"assistant"`, `"content":"hello"`, `"finish_reason":"stop"`, `data: [DONE]`} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %s: %s", want, body)
		}
	}
}

func TestHandlerAuthenticatesAndDoesNotForwardClientSecret(t *testing.T) {
	var calls atomic.Int32
	var expectedAccess string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer "+expectedAccess {
			t.Errorf("upstream authorization = %q", got)
		}
		if got := r.Header.Get("X-API-Key"); got != "" {
			t.Errorf("client API key leaked upstream: %q", got)
		}
		if got := r.Header.Get("X-XAI-Token-Auth"); got != "xai-grok-cli" {
			t.Errorf("missing Grok token auth header: %q", got)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "grok-4.5" || body["store"] != false {
			t.Errorf("unexpected upstream body: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	claims, _ := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "sub": "test-user", "email": "private@example.test"})
	token := "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	expectedAccess = token
	auth, _ := json.Marshal(map[string]any{"session": map[string]any{"key": token, "refresh_token": "synthetic-refresh-token"}})
	if err := os.WriteFile(authPath, auth, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := credential.Load(credential.Config{
		AuthPatterns: []string{authPath}, StateFile: filepath.Join(dir, "state.json"), OAuthClientID: "client",
		TokenURL: upstream.URL + "/token", RefreshLead: time.Minute, MaxConcurrency: 1, HTTPClient: upstream.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{APIKey: "client-secret", UpstreamURL: upstream.URL, ClientVersion: "test", MaxBodyBytes: 1 << 20}
	handler := NewHandler(cfg, store, upstream.Client())

	unauthorized := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4.5","messages":[{"role":"user","content":"hello"}]}`))
	unauthorized.Header.Set("X-API-Key", "wrong")
	unauthorizedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(unauthorizedRecorder, unauthorized)
	if unauthorizedRecorder.Code != http.StatusUnauthorized || calls.Load() != 0 {
		t.Fatalf("unauthorized request code=%d calls=%d", unauthorizedRecorder.Code, calls.Load())
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-build","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer client-secret")
	request.Header.Set("X-API-Key", "must-not-leak")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"content":"ok"`) || calls.Load() != 1 {
		t.Fatalf("code=%d calls=%d body=%s", recorder.Code, calls.Load(), recorder.Body.String())
	}
}
