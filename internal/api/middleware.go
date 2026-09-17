package api

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"time"
)

const (
	maxBodyBytes   = 64 << 10
	requestTimeout = 5 * time.Second
)

type responseRecorder struct {
	http.ResponseWriter
	status  int
	failure string
}

func (w *responseRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(contents []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(contents)
}

func (w *responseRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestID := rand.Text()
		w.Header().Set("X-Request-ID", requestID)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		recorder := &responseRecorder{ResponseWriter: w}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
		defer func() {
			status := recorder.status
			if status == 0 {
				status = http.StatusOK
			}
			route := r.Pattern
			if route == "" || route == "/" {
				route = "unmatched"
			}
			level := slog.LevelInfo
			if status >= 500 || recorder.failure == "panic" {
				level = slog.LevelError
			}
			// Log only server-selected patterns/classifications, never raw URLs,
			// forwarded headers, cookies, body contents, or panic/driver errors.
			s.logger.Log(r.Context(), level, "HTTP request",
				"request_id", requestID, "route", route, "status", status,
				"duration_ms", time.Since(started).Milliseconds(), "failure", recorder.failure)
		}()
		defer func() {
			if recover() != nil {
				if recorder.status != 0 {
					recorder.failure = "panic"
					// A committed response cannot be replaced with valid JSON.
					panic(http.ErrAbortHandler)
				}
				writeError(recorder, http.StatusInternalServerError, "internal_error", "Internal server error")
				recorder.failure = "panic"
			}
		}()
		r.Body = http.MaxBytesReader(recorder, r.Body, maxBodyBytes)
		defer r.Body.Close()
		if r.ContentLength > maxBodyBytes {
			writeError(recorder, http.StatusRequestEntityTooLarge, "body_too_large", "Request body exceeds the byte limit")
			return
		}
		probe := (r.Method == http.MethodGet || r.Method == http.MethodHead) && (r.URL.Path == "/healthz" || r.URL.Path == "/readyz")
		if !probe {
			// Use only the connection peer; forwarding headers cannot change quotas.
			// Unknown peers share the invalid-address bucket.
			peer, _ := netip.ParseAddrPort(r.RemoteAddr)
			ip := peer.Addr().Unmap()
			if retry := s.limiter.allow(ip.String(), time.Now()); retry > 0 {
				seconds := max(1, int((retry+time.Second-1)/time.Second))
				recorder.Header().Set("Retry-After", strconv.Itoa(seconds))
				writeError(recorder, http.StatusTooManyRequests, "rate_limited", "Request rate limit exceeded")
				return
			}
		}
		if err := r.Context().Err(); err != nil {
			writeFailure(recorder, err)
			return
		}
		next.ServeHTTP(recorder, r)
	})
}
