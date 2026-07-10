package config

import "testing"

func TestValidateRequiresKeyOnPublicListener(t *testing.T) {
	cfg := Config{
		ListenAddr: "0.0.0.0:8080", AuthFiles: []string{"auth.json"}, StateFile: "state.json",
		OAuthClientID: "client", TokenURL: "https://auth.x.ai/token", UpstreamURL: "https://example.x.ai",
		RefreshLead: 1, RequestTimeout: 1, MaxBodyBytes: 1024, MaxConcurrencyPerAccount: 1,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("public listener without an API key was accepted")
	}
	cfg.APIKey = "test-only-secret"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestValidateRejectsInsecureEndpoints(t *testing.T) {
	cfg := Config{
		ListenAddr: "127.0.0.1:8080", AuthFiles: []string{"auth.json"}, StateFile: "state.json",
		OAuthClientID: "client", TokenURL: "http://auth.x.ai/token", UpstreamURL: "https://example.x.ai",
		RefreshLead: 1, RequestTimeout: 1, MaxBodyBytes: 1024, MaxConcurrencyPerAccount: 1,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("http OAuth endpoint was accepted")
	}
}
