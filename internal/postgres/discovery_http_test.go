package postgres

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

type httpSearchItem struct {
	Post    httpPost `json:"post"`
	Snippet string   `json:"snippet"`
}

type httpSearchPage struct {
	Items      []httpSearchItem `json:"items"`
	NextCursor *string          `json:"next_cursor"`
}

func readHTTPSearch(t *testing.T, handler http.Handler, path string, session *browserSession) httpSearchPage {
	t.Helper()
	w := contentRequest(handler, "GET", path, "", "", session, "192.0.2.211")
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", path, w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "private, no-store" || w.Header().Get("Access-Control-Allow-Origin") != testOrigin {
		t.Fatalf("headers=%v", w.Header())
	}
	var page httpSearchPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Items == nil || (page.NextCursor == nil && !strings.Contains(w.Body.String(), `"next_cursor":null`)) {
		t.Fatalf("page contract=%s", w.Body.String())
	}
	return page
}

func TestSearchHTTPValidationAndEmptyContract(t *testing.T) {
	_, handler := identityHandler(t)
	for _, path := range []string{
		"/search/posts", "/search/posts?q=", "/search/posts?q=x", "/search/posts?q=ok&q=again", "/search/posts?q=ok&unknown=1",
		"/search/posts?q=ok&limit=0", "/search/posts?q=ok&limit=21", "/search/posts?q=ok&limit=no", "/search/posts?q=%00x", "/search/posts?q=%FFx",
		"/search/posts?q=" + url.QueryEscape(strings.Repeat("界", 101)),
	} {
		feedHTTPError(t, handler, path, nil, http.StatusBadRequest, "invalid_query")
	}
	page := readHTTPSearch(t, handler, "/search/posts?q="+url.QueryEscape(strings.Repeat("界", 2)), nil)
	if len(page.Items) != 0 || page.NextCursor != nil {
		t.Fatalf("empty search=%+v", page)
	}
	w := contentRequest(handler, "POST", "/search/posts?q=ok", "", "", nil, "192.0.2.212")
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("method=%d allow=%q", w.Code, w.Header().Get("Allow"))
	}
}

func TestSearchHTTPSemanticsAndCanonicalProjection(t *testing.T) {
	store, handler := identityHandler(t)
	ctx := context.Background()
	viewer := registerBrowser(t, handler, "search_http_viewer", "192.0.2.213")
	author := registerBrowser(t, handler, "search_http_author", "192.0.2.214")
	viewerHash, authorHash := app.SessionHash(viewer.cookie.Value), app.SessionHash(author.cookie.Value)
	body := feedPost(t, store, authorHash, "search-http-body", "exact jsonb_path_ops phrase")
	matched := feedPost(t, store, authorHash, "search-http-author", "unrelated body")
	feedExec(t, store, `UPDATE accounts SET display_name='literal%_\name' WHERE id=$1`, author.Account.ID)
	if _, err := store.SetRepost(ctx, viewerHash, body.ID, true); err != nil {
		t.Fatal(err)
	}
	page := readHTTPSearch(t, handler, "/search/posts?q="+url.QueryEscape(`"jsonb_path_ops" OR missing`), nil)
	if len(page.Items) != 1 || page.Items[0].Post.ID != body.ID || page.Items[0].Post.Viewer != nil {
		t.Fatalf("body semantics=%+v", page)
	}
	page = readHTTPSearch(t, handler, "/search/posts?q="+url.QueryEscape(`literal%_\name`), &viewer)
	if len(page.Items) != 2 {
		t.Fatalf("literal author count=%d", len(page.Items))
	}
	for _, item := range page.Items {
		if item.Post.ID == matched.ID && item.Post.Viewer == nil {
			t.Fatal("authenticated viewer projection missing")
		}
	}
	detail := decodeHTTPPost(t, contentRequest(handler, "GET", "/posts/"+string(body.ID), "", "", &viewer, "192.0.2.215"), http.StatusOK)
	page = readHTTPSearch(t, handler, "/search/posts?q=jsonb_path_ops", &viewer)
	if len(page.Items) != 1 || page.Items[0].Post.ID != detail.ID || page.Items[0].Post.Counts.Replies != detail.Counts.Replies || page.Items[0].Post.Counts.Reposts != detail.Counts.Reposts || page.Items[0].Post.Viewer == nil {
		t.Fatalf("canonical projection=%+v detail=%+v", page, detail)
	}
	feedExec(t, store, `UPDATE posts SET deleted_at=statement_timestamp() WHERE id=$1`, body.ID)
	if page := readHTTPSearch(t, handler, "/search/posts?q=jsonb_path_ops", nil); len(page.Items) != 0 {
		t.Fatalf("deleted search=%+v", page)
	}
}
