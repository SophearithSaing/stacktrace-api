// Package api handles the HTTP transport and its JSON response types.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"path"
	"time"
)

type server struct {
	mux     *http.ServeMux
	logger  *slog.Logger
	limiter *rateLimiter
}

// NewHandler serves API/infrastructure routes with the shared HTTP controls.
// ready checks database connectivity and exact migration compatibility.
func NewHandler(ready func(context.Context) error, logger *slog.Logger) http.Handler {
	return newServer(ready, logger).handler()
}

func newServer(ready func(context.Context) error, logger *slog.Logger) *server {
	s := &server{
		mux: http.NewServeMux(), logger: logger,
		limiter: newRateLimiter(120, time.Minute, 10000),
	}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := ready(ctx); err != nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "Database or schema is unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	s.mux.HandleFunc("/healthz", methodNotAllowed("GET, HEAD"))
	s.mux.HandleFunc("/readyz", methodNotAllowed("GET, HEAD"))
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "Resource not found")
	})
	return s
}

func methodNotAllowed(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method is not allowed")
	}
}

func (s *server) handler() http.Handler {
	return s.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reject noncanonical paths rather than ServeMux's HTML redirect response.
		if path.Clean(r.URL.Path) != r.URL.Path {
			writeError(w, http.StatusNotFound, "not_found", "Resource not found")
			return
		}
		s.mux.ServeHTTP(w, r)
	}))
}
