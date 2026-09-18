// Package api handles the HTTP transport and its JSON response types.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

type Store interface {
	app.AuthStore
	Ready(context.Context) error
	ProfileByID(context.Context, app.ID, app.ID) (app.AccountProfile, error)
	ProfileByHandle(context.Context, string, app.ID) (app.AccountProfile, error)
	SetFollow(context.Context, string, app.ID, bool) (app.AccountProfile, error)
	CreatePost(context.Context, string, string, app.PostCreation) (app.Post, error)
	PostByID(context.Context, app.ID, app.ID) (app.Post, error)
	DeletePost(context.Context, string, app.ID) error
	CreateReply(context.Context, string, string, app.ReplyCreation) (app.CreateReplyResult, error)
	ListReplies(context.Context, app.ID, app.ReadWindow) (app.ReplyPage, error)
	DeleteReply(context.Context, string, app.ID) error
	SetReaction(context.Context, string, app.ID, *app.ReactionKind) (app.Post, error)
	SetRepost(context.Context, string, app.ID, bool) (app.Post, error)
	SetBookmark(context.Context, string, app.ID, bool) (app.Post, error)
	ListBookmarks(context.Context, string, app.ReadWindow) (app.PostPage, error)
}

type server struct {
	mux                 *http.ServeMux
	logger              *slog.Logger
	limiter             *rateLimiter
	store               Store
	auth                *app.Auth
	clientOrigins       []string
	csrfSigningKey      []byte
	cursorSigningKey    []byte
	secureCookies       bool
	credentialIPLimiter *rateLimiter
	usernameLimiter     *rateLimiter
	accountLimiter      *rateLimiter
}

// NewHandler serves API/infrastructure routes with the shared HTTP controls.
func NewHandler(store Store, clientOrigins []string, csrfSigningKey, cursorSigningKey []byte, secureCookies bool, logger *slog.Logger) http.Handler {
	s := newServer(store.Ready, logger)
	s.store, s.auth = store, app.NewAuth(store)
	s.clientOrigins = append([]string(nil), clientOrigins...)
	s.csrfSigningKey = append([]byte(nil), csrfSigningKey...)
	s.cursorSigningKey = append([]byte(nil), cursorSigningKey...)
	s.secureCookies = secureCookies
	s.mux.HandleFunc("POST /api/v1/auth/register", s.register)
	s.mux.HandleFunc("POST /api/v1/auth/login", s.login)
	s.mux.HandleFunc("GET /api/v1/me", s.me)
	s.mux.HandleFunc("POST /api/v1/auth/logout", s.logout)
	s.mux.HandleFunc("GET /api/v1/accounts/{accountID}", s.accountByID)
	s.mux.HandleFunc("GET /api/v1/accounts/by-handle/{handle}", s.accountByHandle)
	s.mux.HandleFunc("PUT /api/v1/accounts/{accountID}/follow", s.follow)
	s.mux.HandleFunc("DELETE /api/v1/accounts/{accountID}/follow", s.follow)
	s.mux.HandleFunc("POST /api/v1/posts", s.createPost)
	s.mux.HandleFunc("GET /api/v1/posts/{postID}", s.getPost)
	s.mux.HandleFunc("DELETE /api/v1/posts/{postID}", s.deletePost)
	s.mux.HandleFunc("GET /api/v1/posts/{postID}/replies", s.listReplies)
	s.mux.HandleFunc("POST /api/v1/posts/{postID}/replies", s.createReply)
	s.mux.HandleFunc("DELETE /api/v1/replies/{replyID}", s.deleteReply)
	s.mux.HandleFunc("PUT /api/v1/posts/{postID}/reaction", s.reaction)
	s.mux.HandleFunc("DELETE /api/v1/posts/{postID}/reaction", s.reaction)
	s.mux.HandleFunc("PUT /api/v1/posts/{postID}/repost", s.repost)
	s.mux.HandleFunc("DELETE /api/v1/posts/{postID}/repost", s.repost)
	s.mux.HandleFunc("PUT /api/v1/posts/{postID}/bookmark", s.bookmark)
	s.mux.HandleFunc("DELETE /api/v1/posts/{postID}/bookmark", s.bookmark)
	s.mux.HandleFunc("GET /api/v1/me/bookmarks", s.listBookmarks)
	return s.handler()
}

func newServer(ready func(context.Context) error, logger *slog.Logger) *server {
	s := &server{
		mux: http.NewServeMux(), logger: logger,
		limiter:             newRateLimiter(120, time.Minute, 10000),
		credentialIPLimiter: newRateLimiter(20, 15*time.Minute, 10000),
		usernameLimiter:     newRateLimiter(10, 15*time.Minute, 10000),
		accountLimiter:      newRateLimiter(60, time.Minute, 10000),
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
	s.mux.HandleFunc("/", s.notFound)
	return s
}

// Let ServeMux resolve allowed methods instead of registering overlapping
// methodless wildcard patterns (by-handle/{handle} and {id}/follow overlap).
func (s *server) notFound(w http.ResponseWriter, r *http.Request) {
	var allowed []string
	probe := r.Clone(r.Context())
	for _, method := range []string{"GET", "HEAD", "POST", "PUT", "DELETE"} {
		probe.Method = method
		_, pattern := s.mux.Handler(probe)
		if strings.Contains(pattern, " ") {
			allowed = append(allowed, method)
		}
	}
	if len(allowed) != 0 {
		methodNotAllowed(strings.Join(allowed, ", "))(w, r)
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "Resource not found")
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
