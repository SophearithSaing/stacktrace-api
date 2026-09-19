package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

type feedHTTPStore struct {
	Store
	account    app.Account
	sessionErr error
	readErr    error
	query      app.FeedQuery
	window     app.FeedWindow
	viewer     app.ID
	target     app.ID
	hash       string
	reads      int
	handle     string
}

func (s *feedHTTPStore) Ready(context.Context) error { return nil }
func (s *feedHTTPStore) SessionAccount(context.Context, string) (app.Account, error) {
	return s.account, s.sessionErr
}
func (s *feedHTTPStore) ListFeed(_ context.Context, viewer app.ID, hash string, query app.FeedQuery) (app.FeedPage, error) {
	s.query, s.viewer, s.hash = query, viewer, hash
	s.reads++
	return app.FeedPage{}, s.readErr
}
func (s *feedHTTPStore) ListAccountFeed(_ context.Context, target, viewer app.ID, window app.FeedWindow) (app.FeedPage, error) {
	s.target, s.viewer, s.window = target, viewer, window
	s.reads++
	return app.FeedPage{}, s.readErr
}

func (s *feedHTTPStore) ProfileByHandle(_ context.Context, handle string, _ app.ID) (app.AccountProfile, error) {
	s.handle = handle
	return app.AccountProfile{Account: app.Account{ID: app.NewID(), Handle: handle}}, nil
}

func TestFeedHTTPQueryAndForwarding(t *testing.T) {
	store := &feedHTTPStore{account: app.Account{ID: app.NewID()}}
	handler := NewHandler(store, []string{"http://localhost:5173"}, []byte(strings.Repeat("k", 32)), []byte(strings.Repeat("c", 32)), false, testLogger())
	target := app.NewID()
	accountPath := "/api/v1/accounts/" + string(target) + "/feed"
	for _, tc := range []struct {
		path  string
		view  app.FeedView
		sort  app.FeedSort
		tag   string
		limit int
	}{
		{"/api/v1/feed", app.FeedViewForYou, app.FeedSortNewest, "", 20},
		{"/api/v1/feed?view=following&sort=relevant&tag=GO_123&limit=50", app.FeedViewFollowing, app.FeedSortRelevant, "go_123", 50},
		{"/api/v1/feed?view=spicy&sort=reacted&limit=1", app.FeedViewSpicy, app.FeedSortReacted, "", 1},
		{accountPath, "", "", "", 20},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		r.AddCookie(&http.Cookie{Name: "stacktrace_session_dev", Value: strings.Repeat("A", 43)})
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"items":[],"next_cursor":null}` || store.viewer != store.account.ID {
			t.Fatalf("path=%s code=%d body=%s viewer=%s", tc.path, w.Code, w.Body.String(), store.viewer)
		}
		if tc.path == accountPath {
			if store.target != target || store.window.Limit != tc.limit {
				t.Fatalf("account request=%+v", store)
			}
		} else if store.query.View != tc.view || store.query.Sort != tc.sort || store.query.Tag != tc.tag || store.query.Window.Limit != tc.limit || store.hash != app.SessionHash(strings.Repeat("A", 43)) {
			t.Fatalf("global request=%+v", store)
		}
	}
	for _, raw := range []string{"unknown=x", "view=", "sort=", "tag=", "cursor=", "limit=", "view=unknown", "sort=oldest", "tag=%23go", "tag=go,rust", "tag=%C3%A9", "tag=" + strings.Repeat("a", 65), "limit=0", "limit=-1", "limit=51", "limit=1.5", "limit=99999999999999999999999", "limit=1&limit=2", "tag=go&tag=go", "view=for-you&view=spicy", "sort=newest&sort=newest", "cursor=x&cursor=x", "tag=%zz", "tag=go;limit=1"} {
		before := store.reads
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/feed?"+raw, nil))
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"invalid_query"`) || store.reads != before {
			t.Fatalf("query=%s code=%d body=%s", raw, w.Code, w.Body.String())
		}
	}
	for _, raw := range []string{"view=for-you", "sort=newest", "tag=go", "unknown=x", "limit=0", "limit=51", "cursor=", "limit=1&limit=2"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", accountPath+"?"+raw, nil))
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"invalid_query"`) {
			t.Fatalf("account query=%s code=%d body=%s", raw, w.Code, w.Body.String())
		}
	}
}

func TestFeedHTTPSessionOutageAndReauthorization(t *testing.T) {
	store := &feedHTTPStore{sessionErr: app.ErrUnavailable}
	handler := NewHandler(store, nil, []byte(strings.Repeat("k", 32)), []byte(strings.Repeat("c", 32)), false, testLogger())
	for _, path := range []string{"/api/v1/feed", "/api/v1/feed?view=following", "/api/v1/accounts/" + string(app.NewID()) + "/feed"} {
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: "stacktrace_session_dev", Value: strings.Repeat("A", 43)})
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 503 || !strings.Contains(w.Body.String(), `"code":"unavailable"`) || store.reads != 0 {
			t.Fatalf("outage path=%s status=%d body=%s", path, w.Code, w.Body.String())
		}
	}
	// Middleware's successful lookup is not sufficient authorization: errors
	// from the store's in-snapshot Following authorization must propagate.
	store.sessionErr, store.readErr = nil, app.ErrUnauthenticated
	store.account.ID = app.NewID()
	r := httptest.NewRequest("GET", "/api/v1/feed?view=following", nil)
	r.AddCookie(&http.Cookie{Name: "stacktrace_session_dev", Value: strings.Repeat("A", 43)})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 401 || store.reads != 1 {
		t.Fatalf("reauthorization status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestFeedHTTPMethodsAndCORS(t *testing.T) {
	handler := NewHandler(unusedStore{}, []string{"http://localhost:5173"}, []byte(strings.Repeat("k", 32)), []byte(strings.Repeat("c", 32)), false, testLogger())
	for _, path := range []string{"/api/v1/feed", "/api/v1/accounts/" + string(app.NewID()) + "/feed"} {
		for _, method := range []string{"POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
			r := httptest.NewRequest(method, path, nil)
			r.Header.Set("Origin", "http://localhost:5173")
			if method == "OPTIONS" {
				r.Header.Set("Access-Control-Request-Method", "GET")
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if method == "OPTIONS" {
				if w.Code != 204 {
					t.Fatalf("preflight status=%d body=%s", w.Code, w.Body.String())
				}
			} else if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" || !strings.Contains(w.Body.String(), `"code":"method_not_allowed"`) {
				t.Fatalf("method=%s status=%d allow=%s body=%s", method, w.Code, w.Header().Get("Allow"), w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "private, no-store" || w.Header().Get("Access-Control-Allow-Origin") != "http://localhost:5173" || w.Header().Get("Access-Control-Allow-Credentials") != "true" {
				t.Fatalf("headers=%v", w.Header())
			}
		}
	}
}

func TestFeedHTTPAccountRouteDiscrimination(t *testing.T) {
	store := &feedHTTPStore{}
	handler := NewHandler(store, []string{"http://localhost:5173"}, []byte(strings.Repeat("k", 32)), []byte(strings.Repeat("c", 32)), false, testLogger())
	accountPath := "/api/v1/accounts/" + string(app.NewID())
	for _, suffix := range []string{"unknown", "foo%2Ffeed", "%2Ffeed", "feed%2Fextra", "feed/extra", "feed/", ""} {
		for _, method := range []string{"GET", "HEAD", "POST", "PUT", "DELETE"} {
			r := httptest.NewRequest(method, accountPath+"/"+suffix, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != 404 || w.Header().Get("Allow") != "" || !strings.Contains(w.Body.String(), `"code":"not_found"`) {
				t.Fatalf("%s %s: status=%d allow=%s body=%s", method, r.URL, w.Code, w.Header().Get("Allow"), w.Body.String())
			}
		}
	}
	for _, resource := range []string{"feed", "f%65ed"} {
		for _, method := range []string{"GET", "HEAD", "POST"} {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(method, accountPath+"/"+resource, nil))
			if method == "POST" {
				if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
					t.Fatalf("encoded feed method status=%d allow=%s", w.Code, w.Header().Get("Allow"))
				}
			} else if w.Code != 200 {
				t.Fatalf("encoded feed read status=%d body=%s", w.Code, w.Body.String())
			}
		}
	}
	for _, method := range []string{"GET", "HEAD", "POST"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, accountPath+"/follow", nil))
		if w.Code != 405 || w.Header().Get("Allow") != "PUT, DELETE" {
			t.Fatalf("follow %s status=%d allow=%s", method, w.Code, w.Header().Get("Allow"))
		}
	}
	for _, handle := range []string{"feed", "follow", "f%65ed"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/accounts/by-handle/"+handle, nil))
		want := handle
		if handle == "f%65ed" {
			want = "feed"
		}
		if w.Code != 200 || store.handle != want {
			t.Fatalf("profile handle=%s got=%s status=%d body=%s", handle, store.handle, w.Code, w.Body.String())
		}
	}
	for _, tc := range []struct {
		path, method string
		status       int
	}{
		{accountPath + "/feed", "GET", 204}, {accountPath + "/f%65ed", "HEAD", 204},
		{accountPath + "/feed", "POST", 404}, {accountPath + "/follow", "GET", 404},
		{accountPath + "/follow", "PUT", 204}, {accountPath + "/follow", "DELETE", 204},
		{accountPath + "/unknown", "GET", 404}, {accountPath + "/unknown", "POST", 404},
		{accountPath + "/foo%2Ffeed", "GET", 404}, {accountPath + "/%2Ffeed", "HEAD", 404},
		{accountPath + "/feed/extra", "GET", 404}, {accountPath + "/feed/", "GET", 404},
		{"/api/v1/accounts/by-handle/feed", "GET", 204}, {"/api/v1/accounts/by-handle/follow", "GET", 204},
	} {
		r := httptest.NewRequest("OPTIONS", tc.path, nil)
		r.Header.Set("Origin", "http://localhost:5173")
		r.Header.Set("Access-Control-Request-Method", tc.method)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("preflight %s %s status=%d body=%s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}
