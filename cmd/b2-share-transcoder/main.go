package main

import (
	"context"
	"errors"
	"log/slog"
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

	transcoder := broker.NewTranscoder(cfg, store, metadata, nil, logger)
	logger.Info("starting b2-share-transcoder", "poll", cfg.TranscoderPoll.String(), "workDir", cfg.TranscoderWorkDir)
	if err := transcoder.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("transcoder stopped", "error", err)
		os.Exit(1)
	}
}
