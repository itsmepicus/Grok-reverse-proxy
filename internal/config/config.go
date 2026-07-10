package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultOAuthClientID = "b1a00492-073a-47ea-816f-4c329264a828"
	defaultTokenURL      = "https://auth.x.ai/oauth2/token"
	defaultUpstreamURL   = "https://cli-chat-proxy.grok.com"
)

type Config struct {
	ListenAddr               string
	APIKey                   string
	AuthFiles                []string
	StateFile                string
	OAuthClientID            string
	TokenURL                 string
	UpstreamURL              string
	ClientVersion            string
	EgressProxy              string
	RefreshLead              time.Duration
	RequestTimeout           time.Duration
	MaxBodyBytes             int64
	MaxConcurrencyPerAccount int
	AllowInsecureUpstream    bool
	AllowCustomEndpoints     bool
}

func Load() (Config, error) {
	home, _ := os.UserHomeDir()
	defaultAuth := filepath.Join(home, ".grok", "auth.json")
	cfg := Config{
		ListenAddr:               env("GROK_LISTEN_ADDR", "127.0.0.1:8080"),
		APIKey:                   strings.TrimSpace(os.Getenv("GROK_PROXY_API_KEY")),
		AuthFiles:                splitList(env("GROK_AUTH_FILES", defaultAuth)),
		StateFile:                expandPath(env("GROK_STATE_FILE", "./data/accounts.json")),
		OAuthClientID:            env("GROK_OAUTH_CLIENT_ID", defaultOAuthClientID),
		TokenURL:                 env("GROK_TOKEN_URL", defaultTokenURL),
		UpstreamURL:              env("GROK_UPSTREAM_URL", defaultUpstreamURL),
		ClientVersion:            env("GROK_CLIENT_VERSION", "0.2.93"),
		EgressProxy:              strings.TrimSpace(os.Getenv("GROK_EGRESS_PROXY")),
		AllowInsecureUpstream:    envBool("GROK_ALLOW_INSECURE_UPSTREAM", false),
		AllowCustomEndpoints:     envBool("GROK_ALLOW_CUSTOM_ENDPOINTS", false),
		MaxConcurrencyPerAccount: envInt("GROK_MAX_CONCURRENCY_PER_ACCOUNT", 1),
		MaxBodyBytes:             envInt64("GROK_MAX_BODY_BYTES", 8<<20),
	}
	var err error
	if cfg.RefreshLead, err = envDuration("GROK_REFRESH_LEAD", 10*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.RequestTimeout, err = envDuration("GROK_REQUEST_TIMEOUT", 2*time.Minute); err != nil {
		return Config{}, err
	}
	for i := range cfg.AuthFiles {
		cfg.AuthFiles[i] = expandPath(cfg.AuthFiles[i])
	}
	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.ListenAddr) == "" {
		return errors.New("GROK_LISTEN_ADDR is required")
	}
	host, _, err := net.SplitHostPort(c.ListenAddr)
	if err != nil {
		return fmt.Errorf("invalid GROK_LISTEN_ADDR: %w", err)
	}
	if c.APIKey == "" && !isLoopbackHost(host) {
		return errors.New("GROK_PROXY_API_KEY is required when listening outside loopback")
	}
	if len(c.AuthFiles) == 0 {
		return errors.New("GROK_AUTH_FILES must contain at least one path")
	}
	if c.OAuthClientID == "" {
		return errors.New("GROK_OAUTH_CLIENT_ID is required")
	}
	if err := validateEndpoint(c.TokenURL, c.AllowInsecureUpstream, c.AllowCustomEndpoints, "x.ai"); err != nil {
		return fmt.Errorf("invalid GROK_TOKEN_URL: %w", err)
	}
	if err := validateEndpoint(c.UpstreamURL, c.AllowInsecureUpstream, c.AllowCustomEndpoints, "grok.com", "x.ai"); err != nil {
		return fmt.Errorf("invalid GROK_UPSTREAM_URL: %w", err)
	}
	if c.EgressProxy != "" {
		parsed, err := url.Parse(c.EgressProxy)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https" && parsed.Scheme != "socks5") {
			return errors.New("GROK_EGRESS_PROXY must be an http, https, or socks5 URL")
		}
	}
	if c.RefreshLead <= 0 || c.RequestTimeout <= 0 {
		return errors.New("timeouts must be positive")
	}
	if c.MaxBodyBytes < 1024 || c.MaxBodyBytes > 64<<20 {
		return errors.New("GROK_MAX_BODY_BYTES must be between 1024 and 67108864")
	}
	if c.MaxConcurrencyPerAccount < 1 || c.MaxConcurrencyPerAccount > 64 {
		return errors.New("GROK_MAX_CONCURRENCY_PER_ACCOUNT must be between 1 and 64")
	}
	return nil
}

func validateEndpoint(raw string, allowInsecure, allowCustom bool, trustedSuffixes ...string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return errors.New("must be an absolute URL")
	}
	if u.Scheme != "https" && !(allowInsecure && u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return errors.New("must use https")
	}
	if u.User != nil {
		return errors.New("must not contain embedded credentials")
	}
	if !allowCustom && !(allowInsecure && isLoopbackHost(u.Hostname())) && !trustedHost(u.Hostname(), trustedSuffixes...) {
		return errors.New("host is not trusted; set GROK_ALLOW_CUSTOM_ENDPOINTS=true only if intentional")
	}
	return nil
}

func trustedHost(host string, suffixes ...string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for _, suffix := range suffixes {
		suffix = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(suffix), "."))
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if host == "localhost" || host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	return err == nil && parsed
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil {
		return fallback
	}
	return value
}

func envInt64(name string, fallback int64) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64)
	if err != nil {
		return fallback
	}
	return value
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", name, err)
	}
	return parsed, nil
}

func splitList(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func expandPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return filepath.Clean(path)
}
