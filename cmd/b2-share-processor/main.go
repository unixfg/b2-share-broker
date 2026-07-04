package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
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

	setupCtx, setupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer setupCancel()

	sessions := broker.NewSessionManager(cfg)
	sessionAuth := broker.NewSessionAuthenticator(sessions)
	bearerAuth, err := broker.NewBearerAuthenticator(setupCtx, cfg)
	if err != nil {
		logger.Error("bearer oidc setup failed", "error", err)
		os.Exit(1)
	}
	auth := broker.NewCombinedAuthenticator(sessionAuth, bearerAuth)

	store, err := broker.NewB2Store(setupCtx, cfg)
	if err != nil {
		logger.Error("b2 setup failed", "error", err)
		os.Exit(1)
	}
	metadata, err := broker.NewPostgresMetadataStore(setupCtx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("metadata setup failed", "error", err)
		os.Exit(1)
	}
	defer metadata.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	handler := broker.NewServer(cfg, auth, store, metadata, logger)
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Hour,
		WriteTimeout:      2 * time.Hour,
		IdleTimeout:       120 * time.Second,
	}
	transcoder := broker.NewTranscoder(cfg, store, metadata, nil, logger)

	errCh := make(chan error, 2)
	go func() {
		logger.Info("starting b2-share-processor api", "addr", cfg.ListenAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	go func() {
		logger.Info("starting b2-share-processor worker", "poll", cfg.TranscoderPoll.String(), "workDir", cfg.TranscoderWorkDir, "stagingDir", cfg.StagingDir)
		if err := transcoder.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err != nil {
			logger.Error("processor stopped", "error", err)
			stop()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdownCtx)
			os.Exit(1)
		}
	}
}
