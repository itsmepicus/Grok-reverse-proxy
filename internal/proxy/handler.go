package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/itsmepicus/grok-reverse-proxy/internal/config"
	"github.com/itsmepicus/grok-reverse-proxy/internal/credential"
)

type Handler struct {
	cfg    config.Config
	store  *credential.Store
	client *http.Client
}

func NewHandler(cfg config.Config, store *credential.Store, client *http.Client) *Handler {
	return &Handler{cfg: cfg, store: store, client: client}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
		return
	}
	if !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="grok-reverse-proxy"`)
		writeError(w, http.StatusUnauthorized, "authentication_error", "invalid proxy API key")
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		h.models(w)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusNotFound, "not_found_error", "route not found")
		return
	}
	var format responseFormat
	switch r.URL.Path {
	case "/v1/chat/completions":
		format = formatOpenAIChat
	case "/v1/responses":
		format = formatResponses
	case "/v1/messages":
		format = formatAnthropic
	default:
		writeError(w, http.StatusNotFound, "not_found_error", "route not found")
		return
	}
	h.proxy(w, r, format)
}

func (h *Handler) authorized(r *http.Request) bool {
	if h.cfg.APIKey == "" {
		return true
	}
	provided := strings.TrimSpace(r.Header.Get("X-API-Key"))
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		provided = strings.TrimSpace(auth[7:])
	}
	if len(provided) != len(h.cfg.APIKey) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(h.cfg.APIKey)) == 1
}

func (h *Handler) models(w http.ResponseWriter) {
	now := time.Now().Unix()
	data := make([]any, 0, len(publicModels))
	for _, model := range publicModels {
		data = append(data, map[string]any{"id": model, "object": "model", "created": now, "owned_by": "xai"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (h *Handler) proxy(w http.ResponseWriter, r *http.Request, format responseFormat) {
	body, err := readBody(w, r, h.cfg.MaxBodyBytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	prepared, err := prepareRequest(body, format)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	requestID := first(strings.TrimSpace(r.Header.Get("X-Request-ID")), newRequestID())
	attempts := h.store.Count()
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		lease, err := h.store.Acquire()
		if err != nil {
			lastErr = err
			break
		}
		accountID := lease.Info().ID
		token, err := lease.AccessToken(r.Context(), false)
		if err != nil {
			lease.Cooldown(time.Minute)
			lease.Release()
			lastErr = err
			continue
		}
		resp, err := h.forwardRequest(r.Context(), r.Header.Get("Accept"), requestID, prepared.body, token, lease.Info())
		if err != nil {
			lease.Cooldown(15 * time.Second)
			lease.Release()
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized {
			drain(resp.Body)
			token, err = lease.AccessToken(r.Context(), true)
			if err == nil {
				resp, err = h.forwardRequest(r.Context(), r.Header.Get("Accept"), requestID, prepared.body, token, lease.Info())
			}
		}
		if err != nil {
			lease.Cooldown(time.Minute)
			lease.Release()
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized {
			drain(resp.Body)
			lease.Cooldown(5 * time.Minute)
			lease.Release()
			lastErr = errors.New("Grok rejected refreshed OAuth credentials")
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			cooldown := retryAfter(resp.Header.Get("Retry-After"))
			drain(resp.Body)
			lease.Cooldown(cooldown)
			lease.Release()
			lastErr = errors.New("Grok account is rate limited")
			continue
		}
		if resp.StatusCode >= 500 {
			drain(resp.Body)
			lease.Cooldown(15 * time.Second)
			lease.Release()
			lastErr = fmt.Errorf("Grok upstream returned HTTP %d", resp.StatusCode)
			continue
		}
		defer lease.Release()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			_ = copyUpstreamError(w, resp)
			slog.Info("request rejected by upstream", "request_id", requestID, "account_id", accountID, "status", resp.StatusCode)
			return
		}
		w.Header().Set("X-Request-ID", requestID)
		if prepared.stream {
			if format == formatResponses {
				err = streamResponses(w, resp)
			} else {
				err = streamConverted(w, resp, format, prepared.publicModel, requestID)
			}
		} else {
			err = writeNonStream(w, resp, format, prepared.publicModel, requestID)
		}
		if err != nil {
			slog.Warn("response relay failed", "request_id", requestID, "account_id", accountID, "error", err)
		}
		return
	}
	message := "no Grok account is currently available"
	if lastErr != nil {
		slog.Warn("request failed before response", "request_id", requestID, "error", lastErr)
	}
	writeError(w, http.StatusServiceUnavailable, "service_unavailable_error", message)
}

func (h *Handler) forwardRequest(ctx context.Context, accept, requestID string, body []byte, token string, info credential.AccountInfo) (*http.Response, error) {
	endpoint := strings.TrimRight(h.cfg.UpstreamURL, "/")
	if strings.HasSuffix(endpoint, "/v1/responses") {
		// already complete
	} else if strings.HasSuffix(endpoint, "/v1") {
		endpoint += "/responses"
	} else {
		endpoint += "/v1/responses"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid Grok upstream URL")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", first(strings.TrimSpace(accept), "application/json"))
	req.Header.Set("X-Request-ID", requestID)
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	req.Header.Set("X-Grok-Client-Version", h.cfg.ClientVersion)
	req.Header.Set("X-Grok-Client-Identifier", "grok-shell")
	req.Header.Set("User-Agent", "grok-shell/"+h.cfg.ClientVersion+" (linux; x86_64)")
	if info.UserID != "" {
		req.Header.Set("X-UserID", info.UserID)
	}
	if info.Email != "" {
		req.Header.Set("X-Email", info.Email)
	}
	return h.client.Do(req)
}

func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	defer r.Body.Close()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, fmt.Errorf("request body exceeds %d bytes", limit)
		}
		return nil, errors.New("failed to read request body")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("request body is empty")
	}
	return raw, nil
}

func writeError(w http.ResponseWriter, status int, kind, message string) {
	writeJSONStatus(w, status, map[string]any{"error": map[string]any{"type": kind, "message": message}})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	writeJSONStatus(w, status, payload)
}

func writeJSONStatus(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func newRequestID() string {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "req_" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return "req_" + hex.EncodeToString(raw)
}

func retryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		if seconds > 3600 {
			seconds = 3600
		}
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil {
		if duration := time.Until(deadline); duration > 0 {
			return min(duration, time.Hour)
		}
	}
	return time.Minute
}

func drain(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	_ = body.Close()
}
