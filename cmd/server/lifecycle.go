package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// serve gives active requests time to finish, then closes any remaining
// connections if graceful shutdown exceeds its deadline.
func serve(ctx context.Context, server *http.Server, shutdownTimeout time.Duration, logger *slog.Logger) error {
	defer server.Close()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()
	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
		logger.Info("server shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down HTTP: %w", err)
		}
		return nil
	}
}
