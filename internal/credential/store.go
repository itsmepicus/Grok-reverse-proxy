package credential

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	AuthPatterns   []string
	StateFile      string
	OAuthClientID  string
	TokenURL       string
	RefreshLead    time.Duration
	MaxConcurrency int
	HTTPClient     *http.Client
}

type AccountInfo struct {
	ID      string
	Email   string
	UserID  string
	TeamID  string
	Expires time.Time
}

type account struct {
	mu           sync.Mutex
	ID           string    `json:"id"`
	Email        string    `json:"email,omitempty"`
	UserID       string    `json:"user_id,omitempty"`
	TeamID       string    `json:"team_id,omitempty"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	inFlight     int
	cooldownTill time.Time
}

type stateDocument struct {
	Version  int        `json:"version"`
	Accounts []*account `json:"accounts"`
}

type Store struct {
	mu       sync.Mutex
	accounts map[string]*account
	cfg      Config
}

type Lease struct {
	store   *Store
	account *account
	once    sync.Once
}

func Load(cfg Config) (*Store, error) {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.RefreshLead <= 0 {
		cfg.RefreshLead = 10 * time.Minute
	}
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = 1
	}
	store := &Store{accounts: make(map[string]*account), cfg: cfg}
	if cfg.StateFile != "" {
		_ = store.loadFile(cfg.StateFile, true)
	}
	matched := 0
	for _, pattern := range cfg.AuthPatterns {
		paths, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid auth file pattern %q: %w", pattern, err)
		}
		if len(paths) == 0 {
			paths = []string{pattern}
		}
		for _, path := range paths {
			if err := store.loadFile(path, false); err == nil {
				matched++
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("load Grok credentials from %s: %w", path, err)
			}
		}
	}
	if len(store.accounts) == 0 {
		return nil, errors.New("no usable Grok credentials found; run `grok login` and set GROK_AUTH_FILES")
	}
	_ = matched // State-only startup is valid after a successful import.
	if err := store.save(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.accounts)
}

func (s *Store) List() []AccountInfo {
	s.mu.Lock()
	accounts := make([]*account, 0, len(s.accounts))
	for _, item := range s.accounts {
		accounts = append(accounts, item)
	}
	s.mu.Unlock()
	result := make([]AccountInfo, 0, len(accounts))
	for _, item := range accounts {
		item.mu.Lock()
		result = append(result, AccountInfo{ID: item.ID, Email: item.Email, UserID: item.UserID, TeamID: item.TeamID, Expires: item.ExpiresAt})
		item.mu.Unlock()
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (s *Store) Acquire() (*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var selected *account
	for _, item := range s.accounts {
		if item.inFlight >= s.cfg.MaxConcurrency || item.cooldownTill.After(now) {
			continue
		}
		if selected == nil || item.inFlight < selected.inFlight || (item.inFlight == selected.inFlight && item.UpdatedAt.Before(selected.UpdatedAt)) {
			selected = item
		}
	}
	if selected == nil {
		return nil, errors.New("all Grok accounts are busy or cooling down")
	}
	selected.inFlight++
	return &Lease{store: s, account: selected}, nil
}

func (l *Lease) Release() {
	if l == nil || l.store == nil || l.account == nil {
		return
	}
	l.once.Do(func() {
		l.store.mu.Lock()
		if l.account.inFlight > 0 {
			l.account.inFlight--
		}
		l.store.mu.Unlock()
	})
}

func (l *Lease) Cooldown(duration time.Duration) {
	if l == nil || l.store == nil || l.account == nil {
		return
	}
	if duration <= 0 {
		duration = time.Minute
	}
	l.store.mu.Lock()
	l.account.cooldownTill = time.Now().Add(duration)
	l.store.mu.Unlock()
}

func (l *Lease) Info() AccountInfo {
	l.account.mu.Lock()
	defer l.account.mu.Unlock()
	return AccountInfo{ID: l.account.ID, Email: l.account.Email, UserID: l.account.UserID, TeamID: l.account.TeamID, Expires: l.account.ExpiresAt}
}

func (l *Lease) AccessToken(ctx context.Context, force bool) (string, error) {
	if l == nil || l.account == nil {
		return "", errors.New("Grok account lease is unavailable")
	}
	a := l.account
	a.mu.Lock()
	if !force && a.AccessToken != "" && a.ExpiresAt.After(time.Now().Add(l.store.cfg.RefreshLead)) {
		token := a.AccessToken
		a.mu.Unlock()
		return token, nil
	}
	if strings.TrimSpace(a.RefreshToken) == "" {
		a.mu.Unlock()
		return "", errors.New("Grok refresh token is unavailable")
	}
	tokens, err := refresh(ctx, l.store.cfg.HTTPClient, l.store.cfg.TokenURL, l.store.cfg.OAuthClientID, a.RefreshToken)
	if err != nil {
		a.mu.Unlock()
		return "", err
	}
	a.AccessToken = tokens.AccessToken
	a.RefreshToken = tokens.RefreshToken
	a.ExpiresAt = tokens.ExpiresAt
	a.UpdatedAt = time.Now().UTC()
	token := a.AccessToken
	a.mu.Unlock()
	if err := l.store.save(); err != nil {
		return "", errors.New("refreshed OAuth token but failed to save secure runtime state")
	}
	return token, nil
}

func (s *Store) loadFile(path string, state bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var parsed []*account
	if state {
		var doc stateDocument
		if err := json.Unmarshal(raw, &doc); err != nil || doc.Version != 1 {
			return errors.New("invalid runtime state")
		}
		parsed = doc.Accounts
	} else {
		item, err := Parse(raw)
		if err != nil {
			return err
		}
		parsed = []*account{item}
	}
	for _, item := range parsed {
		if item == nil || item.AccessToken == "" || item.RefreshToken == "" {
			continue
		}
		if item.ID == "" {
			item.ID = accountID(item.UserID, item.TeamID, item.Email)
		}
		existing := s.accounts[item.ID]
		switch {
		case existing == nil || existing.RefreshToken == "":
			s.accounts[item.ID] = item
		case state && item.UpdatedAt.After(existing.UpdatedAt):
			s.accounts[item.ID] = item
		case !state && item.ExpiresAt.After(existing.ExpiresAt.Add(time.Minute)):
			// A newly logged-in source account can supersede persisted state,
			// while an older auth.json cannot roll back a rotated refresh token.
			s.accounts[item.ID] = item
		}
	}
	return nil
}

func (s *Store) save() error {
	if strings.TrimSpace(s.cfg.StateFile) == "" {
		return nil
	}
	s.mu.Lock()
	items := make([]*account, 0, len(s.accounts))
	for _, item := range s.accounts {
		item.mu.Lock()
		clone := &account{ID: item.ID, Email: item.Email, UserID: item.UserID, TeamID: item.TeamID, AccessToken: item.AccessToken, RefreshToken: item.RefreshToken, ExpiresAt: item.ExpiresAt, UpdatedAt: item.UpdatedAt}
		item.mu.Unlock()
		items = append(items, clone)
	}
	s.mu.Unlock()
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	raw, err := json.MarshalIndent(stateDocument{Version: 1, Accounts: items}, "", "  ")
	if err != nil {
		return errors.New("encode runtime credential state")
	}
	dir := filepath.Dir(s.cfg.StateFile)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create credential state directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".accounts-*.tmp")
	if err != nil {
		return fmt.Errorf("create credential state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.cfg.StateFile); err != nil {
		return fmt.Errorf("replace credential state: %w", err)
	}
	return os.Chmod(s.cfg.StateFile, 0600)
}

func Parse(raw []byte) (*account, error) {
	var root any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, errors.New("invalid JSON")
	}
	values, ok := findCredentialObject(root)
	if !ok {
		return nil, errors.New("OAuth access and refresh token pair not found")
	}
	access := firstString(values, "access_token", "accessToken", "key")
	refreshToken := firstString(values, "refresh_token", "refreshToken")
	claims, err := jwtClaims(access)
	if err != nil {
		return nil, errors.New("access token is not a valid JWT")
	}
	expiresAt, err := claimTime(claims, "exp")
	if err != nil {
		return nil, errors.New("access token has no valid expiry")
	}
	email := strings.ToLower(firstNonEmpty(findString(root, "email"), claimString(claims, "email", "preferred_username")))
	userID := firstNonEmpty(findString(root, "user_id", "userId"), claimString(claims, "user_id", "userId", "sub"))
	teamID := firstNonEmpty(findString(root, "team_id", "teamId"), claimString(claims, "team_id", "teamId", "organization_id", "org_id"))
	if email == "" && userID == "" {
		return nil, errors.New("access token has no usable identity")
	}
	return &account{
		ID: accountID(userID, teamID, email), Email: email, UserID: userID, TeamID: teamID,
		AccessToken: access, RefreshToken: refreshToken, ExpiresAt: expiresAt, UpdatedAt: time.Now().UTC(),
	}, nil
}

func findCredentialObject(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		if firstString(typed, "access_token", "accessToken", "key") != "" && firstString(typed, "refresh_token", "refreshToken") != "" {
			return typed, true
		}
		for _, child := range typed {
			if found, ok := findCredentialObject(child); ok {
				return found, true
			}
		}
	case []any:
		for _, child := range typed {
			if found, ok := findCredentialObject(child); ok {
				return found, true
			}
		}
	}
	return nil, false
}

func findString(value any, keys ...string) string {
	targets := map[string]bool{}
	for _, key := range keys {
		targets[normalizeKey(key)] = true
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if targets[normalizeKey(key)] {
				if text, ok := child.(string); ok && strings.TrimSpace(text) != "" {
					return strings.TrimSpace(text)
				}
			}
		}
		for _, child := range typed {
			if found := findString(child, keys...); found != "" {
				return found
			}
		}
	case []any:
		for _, child := range typed {
			if found := findString(child, keys...); found != "" {
				return found
			}
		}
	}
	return ""
}

func firstString(values map[string]any, keys ...string) string {
	for _, wanted := range keys {
		for key, value := range values {
			if normalizeKey(key) == normalizeKey(wanted) {
				if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
					return strings.TrimSpace(text)
				}
			}
		}
	}
	return ""
}

func jwtClaims(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, errors.New("invalid JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&claims); err != nil {
		return nil, err
	}
	return claims, nil
}

func claimTime(claims map[string]any, key string) (time.Time, error) {
	value, ok := claims[key]
	if !ok {
		return time.Time{}, errors.New("missing claim")
	}
	var seconds int64
	switch typed := value.(type) {
	case json.Number:
		seconds, _ = typed.Int64()
	case float64:
		seconds = int64(typed)
	case string:
		seconds, _ = strconv.ParseInt(typed, 10, 64)
	}
	if seconds <= 0 {
		return time.Time{}, errors.New("invalid claim")
	}
	return time.Unix(seconds, 0).UTC(), nil
}

func claimString(claims map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := claims[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func accountID(userID, teamID, email string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(userID) + "\x00" + strings.TrimSpace(teamID) + "\x00" + strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(sum[:8])
}

func normalizeKey(value string) string {
	return strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(strings.TrimSpace(value)))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

type refreshedTokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

func refresh(ctx context.Context, client *http.Client, endpoint, clientID, refreshToken string) (*refreshedTokens, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {refreshToken}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, errors.New("prepare OAuth refresh")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("OAuth endpoint is unavailable")
	}
	defer resp.Body.Close()
	var payload map[string]any
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, errors.New("OAuth endpoint returned invalid JSON")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code, _ := payload["error"].(string)
		switch code {
		case "invalid_grant", "invalid_client", "temporarily_unavailable", "server_error":
		default:
			code = "oauth_error"
		}
		return nil, fmt.Errorf("OAuth refresh failed: %s", code)
	}
	access, _ := payload["access_token"].(string)
	rotated, _ := payload["refresh_token"].(string)
	access = strings.TrimSpace(access)
	if access == "" {
		return nil, errors.New("OAuth response omitted access_token")
	}
	if strings.TrimSpace(rotated) == "" {
		rotated = refreshToken
	}
	claims, err := jwtClaims(access)
	if err != nil {
		return nil, errors.New("OAuth response contained invalid access token")
	}
	expires, err := claimTime(claims, "exp")
	if err != nil {
		if number, ok := payload["expires_in"].(json.Number); ok {
			seconds, _ := number.Int64()
			expires = time.Now().UTC().Add(time.Duration(seconds) * time.Second)
		}
	}
	if expires.IsZero() || !expires.After(time.Now()) {
		return nil, errors.New("OAuth response omitted a valid token expiry")
	}
	return &refreshedTokens{AccessToken: access, RefreshToken: strings.TrimSpace(rotated), ExpiresAt: expires}, nil
}
