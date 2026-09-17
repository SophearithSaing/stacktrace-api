package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/api"
	"github.com/SophearithSaing/stacktrace-api/internal/config"
	"github.com/SophearithSaing/stacktrace-api/internal/postgres"
)

func main() {
	cfg, err := config.Load(config.Server)
	if err != nil {
		slog.Error("invalid server configuration", "error", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	store, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.NewHandler(store, cfg.ClientOrigins, cfg.CSRFSigningKey, cfg.SecureCookies, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	logger.Info("starting HTTP server", "address", cfg.HTTPAddr, "api_origin", cfg.APIPublicOrigin)
	return serve(ctx, server, 10*time.Second, logger)
}
