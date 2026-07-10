package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/itsmepicus/grok-reverse-proxy/internal/config"
	"github.com/itsmepicus/grok-reverse-proxy/internal/credential"
	proxyhandler "github.com/itsmepicus/grok-reverse-proxy/internal/proxy"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	client, err := newHTTPClient(cfg)
	if err != nil {
		logger.Error("invalid egress configuration", "error", err)
		os.Exit(1)
	}
	store, err := credential.Load(credential.Config{
		AuthPatterns: cfg.AuthFiles, StateFile: cfg.StateFile, OAuthClientID: cfg.OAuthClientID,
		TokenURL: cfg.TokenURL, RefreshLead: cfg.RefreshLead,
		MaxConcurrency: cfg.MaxConcurrencyPerAccount, HTTPClient: client,
	})
	if err != nil {
		logger.Error("failed to load Grok credentials", "error", err)
		os.Exit(1)
	}
	server := &http.Server{
		Addr: cfg.ListenAddr, Handler: proxyhandler.NewHandler(cfg, store, client),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 1 << 20,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Warn("graceful shutdown failed", "error", err)
		}
	}()
	logger.Info("Grok reverse proxy started", "listen", cfg.ListenAddr, "accounts", store.Count())
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func newHTTPClient(cfg config.Config) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Avoid silently sending OAuth credentials through ambient HTTP_PROXY.
	transport.Proxy = nil
	if cfg.EgressProxy != "" {
		proxyURL, err := url.Parse(cfg.EgressProxy)
		if err != nil {
			return nil, err
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	transport.MaxIdleConns = 64
	transport.MaxIdleConnsPerHost = 16
	transport.IdleConnTimeout = 90 * time.Second
	return &http.Client{Transport: transport, Timeout: cfg.RequestTimeout}, nil
}
