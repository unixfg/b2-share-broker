package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/unixfg/b2-share-broker/internal/broker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := broker.LoadConfigFromEnv()
	if err != nil {
		logger.Error("configuration failed", "error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sessions := broker.NewSessionManager(cfg)
	auth := broker.NewSessionAuthenticator(sessions)
	login, err := broker.NewOIDCLogin(ctx, cfg, sessions)
	if err != nil {
		logger.Error("oidc setup failed", "error", err)
		os.Exit(1)
	}

	store, err := broker.NewB2Store(ctx, cfg)
	if err != nil {
		logger.Error("b2 setup failed", "error", err)
		os.Exit(1)
	}

	handler := broker.NewServerWithLogin(cfg, auth, login, store, logger)
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	logger.Info("starting b2-share-broker", "addr", cfg.ListenAddr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
