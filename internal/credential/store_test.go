package credential

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(raw) + ".signature"
}

func TestParseNativeAuth(t *testing.T) {
	token := testJWT(t, map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "sub": "user-1", "email": "USER@example.com"})
	raw, _ := json.Marshal(map[string]any{"https://auth.x.ai::client": map[string]any{"key": token, "refresh_token": "refresh-value"}})
	account, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if account.Email != "user@example.com" || account.UserID != "user-1" || account.RefreshToken != "refresh-value" {
		t.Fatalf("unexpected parsed account: %#v", account)
	}
}

func TestParseRequiresTokenPairInSameObject(t *testing.T) {
	token := testJWT(t, map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "sub": "user-1"})
	raw, _ := json.Marshal(map[string]any{"left": map[string]any{"key": token}, "right": map[string]any{"refresh_token": "refresh"}})
	if _, err := Parse(raw); err == nil {
		t.Fatal("tokens from unrelated objects were accepted")
	}
}

func TestPersistedAccountDoesNotSerializeMutexOrPoolState(t *testing.T) {
	raw, err := json.Marshal(&account{AccessToken: "access-secret", RefreshToken: "refresh-secret", inFlight: 9, cooldownTill: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "inFlight") || strings.Contains(text, "cooldown") || !strings.Contains(text, "access-secret") {
		t.Fatalf("unexpected state serialization: %s", text)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRefreshPersistsRotatedTokenWithPrivatePermissions(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	statePath := filepath.Join(dir, "state", "accounts.json")
	oldAccess := testJWT(t, map[string]any{"exp": time.Now().Add(time.Minute).Unix(), "sub": "user-1"})
	newAccess := testJWT(t, map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "sub": "user-1"})
	auth, _ := json.Marshal(map[string]any{"session": map[string]any{"key": oldAccess, "refresh_token": "old-refresh"}})
	if err := os.WriteFile(authPath, auth, 0600); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("refresh_token") != "old-refresh" || r.Form.Get("client_id") != "client" {
			t.Fatalf("unexpected refresh form: %v", r.Form)
		}
		payload, _ := json.Marshal(map[string]any{"access_token": newAccess, "refresh_token": "rotated-refresh"})
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(payload)))}, nil
	})}
	store, err := Load(Config{AuthPatterns: []string{authPath}, StateFile: statePath, OAuthClientID: "client", TokenURL: "https://auth.x.ai/token", RefreshLead: 10 * time.Minute, MaxConcurrency: 1, HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	got, err := lease.AccessToken(context.Background(), false)
	if err != nil || got != newAccess {
		t.Fatalf("refreshed token = %q, err=%v", got, err)
	}
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(state), "rotated-refresh") || strings.Contains(string(state), "old-refresh") {
		t.Fatalf("rotated credential was not persisted safely: %s", state)
	}
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("state mode = %o, want 600", info.Mode().Perm())
	}
}
