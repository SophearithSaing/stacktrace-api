// Package api handles the HTTP transport and its JSON response types.
package api

import (
	"context"
	"net/http"
	"time"
)

// Infrastructure exposes only liveness/readiness probes. It deliberately
// carries no public API routes, admin controls, provider calls or diagnostic
// details. The readiness check is supplied by the caller and should complete
// quickly; it must never return raw errors, credentials or payloads.
type Infrastructure struct {
	Ready func(context.Context) bool
}

// Handler returns a minimal handler with only GET /healthz and GET /readyz.
// Unknown routes and methods are rejected without exposing other endpoints.
func (h *Infrastructure) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if h.Ready == nil || !h.Ready(ctx) {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "Not ready")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("/healthz", methodNotAllowed("GET, HEAD"))
	mux.HandleFunc("/readyz", methodNotAllowed("GET, HEAD"))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	})
}
