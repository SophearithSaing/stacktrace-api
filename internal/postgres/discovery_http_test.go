package postgres

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/api"
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
	store, handler := identityHandler(t)
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
	_, session := contentTestActor(t, store, "search_http_limits")
	for index := range 5 {
		feedPost(t, store, session, "search-limit-"+string(rune('a'+index)), "limit searchable")
	}
	if page := readHTTPSearch(t, handler, "/search/posts?q=searchable", nil); len(page.Items) != 4 || page.NextCursor == nil {
		t.Fatalf("default page=%+v", page)
	}
	if page := readHTTPSearch(t, handler, "/search/posts?q=searchable&limit=20", nil); len(page.Items) != 5 || page.NextCursor != nil {
		t.Fatalf("max page=%+v", page)
	}
	feedHTTPError(t, handler, "/search/posts?q=ok&limit=1&limit=2", nil, http.StatusBadRequest, "invalid_query")
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
	nearMatch := registerBrowser(t, handler, "search_http_near_match", "192.0.2.219")
	viewerHash, authorHash := app.SessionHash(viewer.cookie.Value), app.SessionHash(author.cookie.Value)
	body := feedPost(t, store, authorHash, "search-http-body", "exact jsonb_path_ops phrase")
	matched := feedPost(t, store, authorHash, "search-http-author", "unrelated body")
	nearMatched := feedPost(t, store, app.SessionHash(nearMatch.cookie.Value), "search-http-near-match", "ordinary content")
	feedExec(t, store, `UPDATE accounts SET display_name='literal%_\name' WHERE id=$1`, author.Account.ID)
	feedExec(t, store, `UPDATE accounts SET display_name='literalXXZname' WHERE id=$1`, nearMatch.Account.ID)
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
		if item.Post.ID == nearMatched.ID {
			t.Fatalf("literal author wildcard matched=%+v", page)
		}
		if item.Post.ID == matched.ID && item.Post.Viewer == nil {
			t.Fatal("authenticated viewer projection missing")
		}
	}
	detail := decodeHTTPPost(t, contentRequest(handler, "GET", "/posts/"+string(body.ID), "", "", &viewer, "192.0.2.215"), http.StatusOK)
	page = readHTTPSearch(t, handler, "/search/posts?q=jsonb_path_ops", &viewer)
	if len(page.Items) != 1 || page.Items[0].Post.ID != detail.ID || page.Items[0].Post.Counts.Replies != detail.Counts.Replies || page.Items[0].Post.Counts.Reposts != detail.Counts.Reposts || page.Items[0].Post.Viewer == nil || !page.Items[0].Post.Viewer.Reposted {
		t.Fatalf("canonical projection=%+v detail=%+v", page, detail)
	}
	feedExec(t, store, `UPDATE posts SET deleted_at=statement_timestamp() WHERE id=$1`, body.ID)
	if page := readHTTPSearch(t, handler, "/search/posts?q=jsonb_path_ops", nil); len(page.Items) != 0 {
		t.Fatalf("deleted search=%+v", page)
	}
}

func TestSearchHTTPPaginationCursorContracts(t *testing.T) {
	store, handler := identityHandler(t)
	viewer := registerBrowser(t, handler, "search_cursor_viewer", "192.0.2.216")
	other := registerBrowser(t, handler, "search_cursor_other", "192.0.2.217")
	session := app.SessionHash(viewer.cookie.Value)
	first := feedPost(t, store, session, "search-cursor-first", "cursor marker")
	second := feedPost(t, store, session, "search-cursor-second", "cursor marker")
	third := feedPost(t, store, session, "search-cursor-third", "cursor marker")
	tie := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	feedExec(t, store, `UPDATE posts SET created_at=$1 WHERE id IN ($2,$3,$4)`, tie, first.ID, second.ID, third.ID)
	page := readHTTPSearch(t, handler, "/search/posts?q=cursor+marker&limit=1", &viewer)
	if len(page.Items) != 1 || page.NextCursor == nil {
		t.Fatalf("first page=%+v", page)
	}
	continued := readHTTPSearch(t, handler, "/search/posts?q="+url.QueryEscape("  cursor marker  ")+"&limit=2&cursor="+url.QueryEscape(*page.NextCursor), &viewer)
	if len(continued.Items) != 2 || continued.NextCursor != nil {
		t.Fatalf("changed limit continuation=%+v", continued)
	}
	seen := map[app.ID]bool{page.Items[0].Post.ID: true}
	for _, item := range continued.Items {
		if seen[item.Post.ID] {
			t.Fatalf("duplicate tie result=%+v", item)
		}
		seen[item.Post.ID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("tie pagination missing results=%v", seen)
	}
	want := []app.ID{first.ID, second.ID, third.ID}
	sort.Slice(want, func(left, right int) bool { return want[left] > want[right] })
	got := []app.ID{page.Items[0].Post.ID, continued.Items[0].Post.ID, continued.Items[1].Post.ID}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("tie order=%v want=%v", got, want)
		}
	}
	feedHTTPError(t, handler, "/search/posts?q=other+marker&cursor="+url.QueryEscape(*page.NextCursor), &viewer, http.StatusBadRequest, "invalid_cursor")
	feedHTTPError(t, handler, "/search/posts?q=cursor+marker&cursor="+url.QueryEscape(*page.NextCursor), &other, http.StatusBadRequest, "invalid_cursor")
	feedHTTPError(t, handler, "/search/posts?q=cursor+marker&cursor="+url.QueryEscape(*page.NextCursor), nil, http.StatusBadRequest, "invalid_cursor")

	anonymous := readHTTPSearch(t, handler, "/search/posts?q=cursor+marker&limit=1", nil)
	if anonymous.NextCursor == nil {
		t.Fatal("anonymous first page has no cursor")
	}
	feedHTTPError(t, handler, "/search/posts?q=cursor+marker&cursor="+url.QueryEscape(*anonymous.NextCursor), &viewer, http.StatusBadRequest, "invalid_cursor")

	time.Sleep(5 * time.Millisecond)
	later := feedPost(t, store, session, "search-cursor-later", "cursor marker")
	continued = readHTTPSearch(t, handler, "/search/posts?q=cursor+marker&cursor="+url.QueryEscape(*page.NextCursor), &viewer)
	for _, item := range continued.Items {
		if item.Post.ID == later.ID {
			t.Fatalf("continuation crossed original ceiling=%+v", continued)
		}
	}
	fresh := readHTTPSearch(t, handler, "/search/posts?q=cursor+marker&limit=20", &viewer)
	foundLater := false
	for _, item := range fresh.Items {
		foundLater = foundLater || item.Post.ID == later.ID
	}
	if !foundLater {
		t.Fatalf("fresh read omitted later post=%+v", fresh)
	}
}

func TestSearchHTTPGeneratedQuoteProjection(t *testing.T) {
	store, job, attempt, output := publicationFixture(t, app.OutputQuote, "generated discovery marker")
	published, err := store.PublishGeneration(context.Background(), job.ID, job.LeaseVersion, attempt.ID, output)
	if err != nil || published.ResultPostID == nil {
		t.Fatalf("publish generated quote=%+v, %v", published, err)
	}
	handler := api.NewHandler(store, []string{testOrigin}, []byte(strings.Repeat("k", 32)), []byte(strings.Repeat("c", 32)), false, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	viewer := registerBrowser(t, handler, "search_generated_viewer", "192.0.2.218")
	if _, err := store.SetRepost(context.Background(), app.SessionHash(viewer.cookie.Value), *published.ResultPostID, true); err != nil {
		t.Fatal(err)
	}
	page := readHTTPSearch(t, handler, "/search/posts?q=generated+discovery+marker", &viewer)
	if len(page.Items) != 1 || page.Items[0].Post.ID != *published.ResultPostID || page.Items[0].Post.Viewer == nil || !page.Items[0].Post.Viewer.Reposted || page.Items[0].Post.Quote == nil {
		t.Fatalf("generated search projection=%+v", page)
	}
	if _, leaked := page.Items[0].Post.Quote["is_generated"]; leaked {
		t.Fatalf("quote leaked generated provenance=%s", mustJSON(page.Items[0].Post.Quote))
	}
	response := contentRequest(handler, "GET", "/search/posts?q=generated+discovery+marker", "", "", &viewer, "192.0.2.218")
	if !strings.Contains(response.Body.String(), `"is_generated":true`) || strings.Contains(response.Body.String(), "generation_job") || strings.Contains(response.Body.String(), "generation_attempt") {
		t.Fatalf("search leaked private generation data=%s", response.Body.String())
	}
	detail := decodeHTTPPost(t, contentRequest(handler, "GET", "/posts/"+string(*published.ResultPostID), "", "", &viewer, "192.0.2.218"), http.StatusOK)
	item := page.Items[0].Post
	if item.Counts.Replies != detail.Counts.Replies || item.Counts.Reposts != detail.Counts.Reposts || item.Counts.ReactionsTotal != detail.Counts.ReactionsTotal || item.Body != detail.Body {
		t.Fatalf("generated canonical counts=%+v detail=%+v", item, detail)
	}
}

func TestDiscoveryCrossProjectionContract(t *testing.T) {
	_, handler := identityHandler(t)
	viewer := registerBrowser(t, handler, "cross_projection", "192.0.2.221")
	post := decodeHTTPPost(t, contentRequest(handler, "POST", "/posts", `{"body":"cross projection marker"}`, "cross-post", &viewer, "192.0.2.221"), http.StatusCreated)
	if w := contentRequest(handler, "POST", "/posts/"+string(post.ID)+"/replies", `{"body":"counted reply"}`, "cross-reply", &viewer, "192.0.2.221"); w.Code != http.StatusCreated {
		t.Fatal(w.Code)
	}
	for _, path := range []string{"/reaction", "/repost", "/bookmark"} {
		body := ""
		if path == "/reaction" {
			body = `{"kind":"useful"}`
		}
		if w := contentRequest(handler, "PUT", "/posts/"+string(post.ID)+path, body, "", &viewer, "192.0.2.221"); w.Code != http.StatusOK {
			t.Fatalf("%s=%d", path, w.Code)
		}
	}
	detail := decodeHTTPPost(t, contentRequest(handler, "GET", "/posts/"+string(post.ID), "", "", &viewer, "192.0.2.221"), http.StatusOK)
	for _, path := range []string{"/accounts/" + string(viewer.Account.ID), "/accounts/by-handle/" + strings.ToUpper(viewer.Account.Handle)} {
		w := contentRequest(handler, "GET", path, "", "", &viewer, "192.0.2.221")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"`+string(viewer.Account.ID)+`"`) {
			t.Fatalf("independent account lookup %s=%d %s", path, w.Code, w.Body.String())
		}
	}
	search := readHTTPSearch(t, handler, "/search/posts?q=cross+projection", &viewer)
	feed := readHTTPFeed(t, handler, "/feed", &viewer)
	if len(search.Items) != 1 || search.Items[0].Post.ID != post.ID || len(feed.Items) != 2 || detail.Author.ID != viewer.Account.ID || detail.Counts.Replies != 1 || detail.Counts.Reposts != 1 || detail.Counts.ReactionsTotal != 1 || detail.Viewer == nil || !detail.Viewer.Bookmarked || !detail.Viewer.Reposted || detail.Viewer.Reaction == nil {
		t.Fatalf("detail/search/feed=%+v %+v %+v", detail, search, feed)
	}
	for _, entry := range feed.Items {
		projected := feedHTTPPost(t, entry.Post)
		if projected.ID != detail.ID || projected.Author.ID != detail.Author.ID || projected.Counts.Replies != detail.Counts.Replies || projected.Counts.Reposts != detail.Counts.Reposts || projected.Counts.ReactionsTotal != detail.Counts.ReactionsTotal || projected.Viewer == nil || !projected.Viewer.Bookmarked || !projected.Viewer.Reposted || projected.Viewer.Reaction == nil {
			t.Fatalf("feed projection=%+v detail=%+v", projected, detail)
		}
	}
	if item := search.Items[0].Post; item.Author.ID != detail.Author.ID || item.Counts.Replies != detail.Counts.Replies || item.Counts.Reposts != detail.Counts.Reposts || item.Counts.ReactionsTotal != detail.Counts.ReactionsTotal || item.Viewer == nil || !item.Viewer.Bookmarked || !item.Viewer.Reposted || item.Viewer.Reaction == nil {
		t.Fatalf("search projection=%+v detail=%+v", item, detail)
	}
}
