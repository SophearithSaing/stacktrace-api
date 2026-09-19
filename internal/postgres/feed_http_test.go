package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

type httpFeedEntry struct {
	ID         string          `json:"id"`
	Kind       app.FeedKind    `json:"kind"`
	OccurredAt string          `json:"occurred_at"`
	Reposter   json.RawMessage `json:"reposter"`
	Post       json.RawMessage `json:"post"`
}

type httpFeedPage struct {
	Items      []httpFeedEntry `json:"items"`
	NextCursor *string         `json:"next_cursor"`
}

func readHTTPFeed(t *testing.T, handler http.Handler, path string, session *browserSession) httpFeedPage {
	t.Helper()
	w := contentRequest(handler, "GET", path, "", "", session, "192.0.2.190")
	if w.Code != 200 {
		t.Fatalf("GET %s status=%d body=%s", path, w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "private, no-store" || w.Header().Get("Access-Control-Allow-Origin") != testOrigin || w.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("feed headers=%v", w.Header())
	}
	var page httpFeedPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Items == nil || (page.NextCursor == nil && !strings.Contains(w.Body.String(), `"next_cursor":null`)) {
		t.Fatalf("empty/null contract: %s", w.Body.String())
	}
	return page
}

func feedHTTPError(t *testing.T, handler http.Handler, path string, session *browserSession, status int, code string) {
	t.Helper()
	w := contentRequest(handler, "GET", path, "", "", session, "192.0.2.191")
	if w.Code != status || !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("GET %s status=%d want=%d body=%s", path, w.Code, status, w.Body.String())
	}
}

func feedHTTPPost(t *testing.T, raw json.RawMessage) httpPost {
	t.Helper()
	var post httpPost
	if err := json.Unmarshal(raw, &post); err != nil {
		t.Fatal(err)
	}
	return post
}

func TestFeedHTTPViewsAndAccounts(t *testing.T) {
	store, handler := identityHandler(t)
	ctx := context.Background()
	alice := registerBrowser(t, handler, "feed_http_alice", "192.0.2.181")
	bob := registerBrowser(t, handler, "feed_http_bob", "192.0.2.182")
	carol := registerBrowser(t, handler, "feed_http_carol", "192.0.2.183")
	aliceHash, bobHash, carolHash := app.SessionHash(alice.cookie.Value), app.SessionHash(bob.cookie.Value), app.SessionHash(carol.cookie.Value)
	self := feedPost(t, store, aliceHash, "self", "self #Go")
	followed := feedPost(t, store, bobHash, "followed", "followed #Rust")
	outside := feedPost(t, store, carolHash, "outside", "outside #Go")
	reactionOnly := feedPost(t, store, carolHash, "reaction", "reaction #Rust")
	feedExec(t, store, `UPDATE posts SET is_spicy=true WHERE id IN ($1,$2)`, followed.ID, outside.ID)
	if _, err := store.SetFollow(ctx, aliceHash, bob.Account.ID, true); err != nil {
		t.Fatal(err)
	}
	var followedRepost, selfRepost, outsideRepost app.ID
	for _, tc := range []struct {
		hash string
		post app.ID
		id   *app.ID
	}{{bobHash, outside.ID, &followedRepost}, {aliceHash, reactionOnly.ID, &selfRepost}, {carolHash, self.ID, &outsideRepost}} {
		post, err := store.SetRepost(ctx, tc.hash, tc.post, true)
		if err != nil {
			t.Fatal(err)
		}
		*tc.id = post.Viewer.ViewerRepost.ID
	}
	spicy := app.ReactionSpicy
	if _, err := store.SetReaction(ctx, aliceHash, reactionOnly.ID, &spicy); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want []string
	}{
		{"/feed?view=following", []string{"post:" + string(self.ID), "post:" + string(followed.ID), "repost:" + string(followedRepost), "repost:" + string(selfRepost)}},
		{"/feed?view=following&tag=GO", []string{"post:" + string(self.ID), "repost:" + string(followedRepost)}},
		{"/feed?view=spicy", []string{"post:" + string(followed.ID), "post:" + string(outside.ID), "repost:" + string(followedRepost)}},
		{"/feed?view=spicy&tag=go", []string{"post:" + string(outside.ID), "repost:" + string(followedRepost)}},
		{"/feed?tag=Go", []string{"post:" + string(self.ID), "post:" + string(outside.ID), "repost:" + string(followedRepost), "repost:" + string(outsideRepost)}},
		{"/accounts/" + string(bob.Account.ID) + "/feed", []string{"post:" + string(followed.ID), "repost:" + string(followedRepost)}},
		{"/feed?tag=unknown", []string{}},
	} {
		page := readHTTPFeed(t, handler, tc.path, &alice)
		got := make([]string, 0, len(page.Items))
		for _, entry := range page.Items {
			got = append(got, entry.ID)
		}
		sort.Strings(got)
		sort.Strings(tc.want)
		if !reflect.DeepEqual(got, tc.want) || page.NextCursor != nil {
			t.Fatalf("path=%s got=%v want=%v", tc.path, got, tc.want)
		}
	}
	global := readHTTPFeed(t, handler, "/feed", nil)
	newest := readHTTPFeed(t, handler, "/feed?view=for-you&sort=newest", nil)
	relevant := readHTTPFeed(t, handler, "/feed?sort=relevant", nil)
	if len(global.Items) != 7 || !reflect.DeepEqual(global, newest) || !reflect.DeepEqual(global, relevant) {
		t.Fatalf("default/newest/relevant mismatch: %+v %+v %+v", global, newest, relevant)
	}
	for _, entry := range global.Items {
		post := feedHTTPPost(t, entry.Post)
		if post.Viewer != nil || entry.OccurredAt == "" || !strings.HasPrefix(entry.ID, string(entry.Kind)+":") || len(entry.Reposter) == 0 || (string(entry.Reposter) == "null") != (entry.Kind == app.FeedKindPost) {
			t.Fatalf("event contract=%+v post=%+v", entry, post)
		}
		if entry.Kind == app.FeedKindPost && entry.ID != "post:"+string(post.ID) {
			t.Fatalf("canonical identity=%+v", entry)
		}
	}
	reacted := readHTTPFeed(t, handler, "/feed?sort=reacted", nil)
	if feedHTTPPost(t, reacted.Items[0].Post).ID != reactionOnly.ID || feedHTTPPost(t, reacted.Items[1].Post).ID != reactionOnly.ID || reacted.Items[0].Kind != app.FeedKindRepost {
		t.Fatalf("reacted ordering=%+v", reacted)
	}
	rankedFirst := readHTTPFeed(t, handler, "/feed?sort=reacted&limit=1", nil)
	if rankedFirst.NextCursor == nil {
		t.Fatal("missing reacted continuation")
	}
	rankedRest := readHTTPFeed(t, handler, "/feed?sort=reacted&cursor="+url.QueryEscape(*rankedFirst.NextCursor), nil)
	if len(rankedRest.Items) != 6 || !reflect.DeepEqual(rankedRest.Items, reacted.Items[1:]) {
		t.Fatal("reacted score/tuple continuation disagrees with full ranking")
	}
	feedHTTPError(t, handler, "/accounts/"+string(app.NewID())+"/feed", nil, 404, "not_found")
	feedHTTPError(t, handler, "/accounts/invalid/feed", nil, 400, "invalid_id")
	feedExec(t, store, `UPDATE accounts SET disabled_at=statement_timestamp() WHERE id=$1`, carol.Account.ID)
	feedHTTPError(t, handler, "/accounts/"+string(carol.Account.ID)+"/feed", nil, 404, "not_found")
	if page := readHTTPFeed(t, handler, "/feed", nil); len(page.Items) != 7 {
		t.Fatal("disabled author public content changed")
	}
}

func TestFeedHTTPCursorBindingsAndTies(t *testing.T) {
	store, handler := identityHandler(t)
	ctx := context.Background()
	alice := registerBrowser(t, handler, "feed_cursor_alice", "192.0.2.184")
	bob := registerBrowser(t, handler, "feed_cursor_bob", "192.0.2.185")
	hash := app.SessionHash(alice.cookie.Value)
	one := feedPost(t, store, hash, "one", "one #Go")
	two := feedPost(t, store, hash, "two", "two #Go")
	feedExec(t, store, `UPDATE posts SET created_at='2020-01-01'`)
	feedExec(t, store, `INSERT INTO reposts(id,account_id,post_id,created_at) VALUES($1,$3,$1,'2020-01-01'),($2,$3,$2,'2020-01-01')`, one.ID, two.ID, alice.Account.ID)
	accountPath := "/accounts/" + string(alice.Account.ID) + "/feed"
	first := readHTTPFeed(t, handler, "/feed?tag=GO&limit=1", nil)
	if len(first.Items) != 1 || first.NextCursor == nil {
		t.Fatalf("first=%+v", first)
	}
	token := url.QueryEscape(*first.NextCursor)
	rest := readHTTPFeed(t, handler, "/feed?view=for-you&sort=newest&tag=go&limit=3&cursor="+token, nil)
	got := []string{first.Items[0].ID}
	for _, item := range rest.Items {
		got = append(got, item.ID)
	}
	ids := []string{string(one.ID), string(two.ID)}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	want := []string{"repost:" + ids[0], "repost:" + ids[1], "post:" + ids[0], "post:" + ids[1]}
	if !reflect.DeepEqual(got, want) || rest.NextCursor != nil {
		t.Fatalf("tie/limit change got=%v want=%v", got, want)
	}
	for _, path := range []string{
		"/feed?tag=rust&cursor=" + token, "/feed?cursor=" + token,
		"/feed?tag=go&view=spicy&cursor=" + token,
		"/feed?tag=go&sort=relevant&cursor=" + token,
		"/feed?tag=go&sort=reacted&cursor=" + token,
		accountPath + "?cursor=" + token,
		"/feed?tag=go&cursor=" + token + "x", "/feed?cursor=" + strings.Repeat("x", 2049),
		"/posts/" + string(one.ID) + "/replies?cursor=" + token,
	} {
		feedHTTPError(t, handler, path, nil, 400, "invalid_cursor")
	}
	// Login cannot continue anonymous cursors; changing/logout of a viewer
	// cannot continue authenticated public-feed cursors either.
	feedHTTPError(t, handler, "/feed?tag=go&cursor="+token, &alice, 400, "invalid_cursor")
	private := readHTTPFeed(t, handler, "/feed?limit=1", &alice)
	for _, session := range []*browserSession{nil, &bob} {
		feedHTTPError(t, handler, "/feed?cursor="+url.QueryEscape(*private.NextCursor), session, 400, "invalid_cursor")
	}
	relevant := readHTTPFeed(t, handler, "/feed?sort=relevant&limit=1", nil)
	feedHTTPError(t, handler, "/feed?sort=newest&cursor="+url.QueryEscape(*relevant.NextCursor), nil, 400, "invalid_cursor")
	account := readHTTPFeed(t, handler, accountPath+"?limit=1", nil)
	accountToken := url.QueryEscape(*account.NextCursor)
	readHTTPFeed(t, handler, accountPath+"?limit=2&cursor="+accountToken, nil)
	feedHTTPError(t, handler, "/feed?cursor="+accountToken, nil, 400, "invalid_cursor")
	feedHTTPError(t, handler, "/accounts/"+string(bob.Account.ID)+"/feed?cursor="+accountToken, nil, 400, "invalid_cursor")
	feedHTTPError(t, handler, accountPath+"?cursor="+accountToken, &alice, 400, "invalid_cursor")
	for _, postID := range []app.ID{one.ID, two.ID} {
		if _, err := store.SetBookmark(ctx, hash, postID, true); err != nil {
			t.Fatal(err)
		}
	}
	w := contentRequest(handler, "GET", "/me/bookmarks?limit=1", "", "", &alice, "192.0.2.186")
	var saved struct {
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil || w.Code != 200 || saved.NextCursor == nil {
		t.Fatalf("saved status=%d body=%s error=%v", w.Code, w.Body.String(), err)
	}
	feedHTTPError(t, handler, "/feed?cursor="+url.QueryEscape(*saved.NextCursor), &alice, 400, "invalid_cursor")
	feedHTTPError(t, handler, "/me/bookmarks?cursor="+url.QueryEscape(*private.NextCursor), &alice, 400, "invalid_cursor")
	logout := identityRequest(handler, "POST", "/auth/logout", "", &alice, "192.0.2.187")
	if logout.Code != 204 {
		t.Fatalf("logout=%d %s", logout.Code, logout.Body.String())
	}
	feedHTTPError(t, handler, "/feed?cursor="+url.QueryEscape(*private.NextCursor), &alice, 400, "invalid_cursor")
	readHTTPFeed(t, handler, "/feed?tag=go&cursor="+token, &alice) // stale cookie is anonymous
}

func TestFeedHTTPProjectionParityAndReplyCursor(t *testing.T) {
	store, handler := identityHandler(t)
	ctx := context.Background()
	alice := registerBrowser(t, handler, "feed_parity_alice", "192.0.2.171")
	bob := registerBrowser(t, handler, "feed_parity_bob", "192.0.2.172")
	aliceHash, bobHash := app.SessionHash(alice.cookie.Value), app.SessionHash(bob.cookie.Value)
	quote := feedPost(t, store, bobHash, "quote", "quoted")
	creation, err := app.NewPostCreation("plain <script> #Parity", &quote.ID, &app.Code{Language: "go", Filename: "main.go", Source: "package main\n\t// <script>\n"})
	if err != nil {
		t.Fatal(err)
	}
	post, err := store.CreatePost(ctx, aliceHash, "parity", creation)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		creation, _ := app.NewReplyCreation(post.ID, fmt.Sprintf("reply %d", i))
		if _, err := store.CreateReply(ctx, aliceHash, fmt.Sprintf("reply-%d", i), creation); err != nil {
			t.Fatal(err)
		}
	}
	for _, hash := range []string{aliceHash, bobHash} {
		if _, err := store.SetRepost(ctx, hash, post.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.SetBookmark(ctx, aliceHash, post.ID, true); err != nil {
		t.Fatal(err)
	}
	agree := app.ReactionAgree
	if _, err := store.SetReaction(ctx, aliceHash, post.ID, &agree); err != nil {
		t.Fatal(err)
	}
	page := readHTTPFeed(t, handler, "/feed?tag=parity", &alice)
	if len(page.Items) != 3 {
		t.Fatalf("events=%d", len(page.Items))
	}
	for _, entry := range page.Items {
		if string(entry.Post) != string(page.Items[0].Post) {
			t.Fatal("repeated events differ on canonical post JSON")
		}
		if strings.Contains(string(entry.Post), `"bookmarks"`) || strings.Contains(string(entry.Post), `"bookmarks_count"`) {
			t.Fatal("private aggregate present")
		}
	}
	projected := feedHTTPPost(t, page.Items[0].Post)
	if projected.Code == nil || projected.Code.Source != creation.Content.Code.Source || projected.Body != creation.Content.Body || projected.Viewer == nil || !projected.Viewer.Bookmarked || !projected.Viewer.Reposted || projected.Viewer.Reaction == nil || projected.Counts.Replies != 4 || len(projected.ReplyPreview.Items) != 2 || projected.ReplyPreview.NextCursor == nil || len(projected.Counts.Kinds) != 5 || string(projected.Quote["availability"]) != `"available"` {
		t.Fatalf("post projection=%+v", projected)
	}
	detail := contentRequest(handler, "GET", "/posts/"+string(post.ID), "", "", &alice, "192.0.2.173")
	saved := contentRequest(handler, "GET", "/me/bookmarks", "", "", &alice, "192.0.2.173")
	var bookmarks struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(saved.Body.Bytes(), &bookmarks); err != nil || saved.Code != 200 || len(bookmarks.Items) != 1 || detail.Code != 200 {
		t.Fatalf("detail=%s saved=%s err=%v", detail.Body.String(), saved.Body.String(), err)
	}
	// Each request legitimately has a different preview time ceiling/signature;
	// all remaining canonical fields must match detail and saved projections.
	canonical := func(raw []byte) map[string]any {
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		preview := value["reply_preview"].(map[string]any)
		if preview["next_cursor"] == nil {
			t.Fatal("missing preview continuation")
		}
		delete(preview, "next_cursor")
		return value
	}
	if !reflect.DeepEqual(canonical(page.Items[0].Post), canonical(detail.Body.Bytes())) || !reflect.DeepEqual(canonical(page.Items[0].Post), canonical(bookmarks.Items[0])) {
		t.Fatal("feed/detail/saved canonical state differs")
	}
	for _, session := range []*browserSession{nil, &bob} {
		anonymous := readHTTPFeed(t, handler, "/feed?tag=parity", session)
		p := feedHTTPPost(t, anonymous.Items[0].Post)
		if session == nil && p.Viewer != nil || session != nil && (p.Viewer == nil || p.Viewer.Bookmarked || p.Viewer.Reaction != nil) {
			t.Fatalf("viewer privacy=%+v", p.Viewer)
		}
	}
	previewToken := url.QueryEscape(*projected.ReplyPreview.NextCursor)
	for _, session := range []*browserSession{nil, &alice, &bob} {
		w := contentRequest(handler, "GET", "/posts/"+string(post.ID)+"/replies?cursor="+previewToken, "", "", session, "192.0.2.174")
		var replies struct {
			Items []struct {
				Body string `json:"body"`
			} `json:"items"`
			NextCursor *string `json:"next_cursor"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &replies); err != nil || w.Code != 200 || len(replies.Items) != 2 || replies.Items[0].Body != "reply 2" || replies.NextCursor != nil {
			t.Fatalf("preview continuation=%d %s error=%v", w.Code, w.Body.String(), err)
		}
	}
	feedHTTPError(t, handler, "/posts/"+string(quote.ID)+"/replies?cursor="+previewToken, nil, 400, "invalid_cursor")
	feedHTTPError(t, handler, "/posts/"+string(post.ID)+"/replies?sort=newest&cursor="+previewToken, nil, 400, "invalid_cursor")
	feedHTTPError(t, handler, "/feed?cursor="+previewToken, nil, 400, "invalid_cursor")
}

func TestFeedHTTPFollowingSessionChanges(t *testing.T) {
	store, handler := identityHandler(t)
	alice := registerBrowser(t, handler, "feed_session_alice", "192.0.2.175")
	hash := app.SessionHash(alice.cookie.Value)
	feedPost(t, store, hash, "first", "first")
	feedPost(t, store, hash, "second", "second")
	first := readHTTPFeed(t, handler, "/feed?view=following&limit=1", &alice)
	if first.NextCursor == nil {
		t.Fatal("missing following continuation")
	}
	continuation := "/feed?view=following&cursor=" + url.QueryEscape(*first.NextCursor)
	if next := readHTTPFeed(t, handler, continuation, &alice); len(next.Items) != 1 || next.NextCursor != nil {
		t.Fatalf("following continuation=%+v", next)
	}
	feedHTTPError(t, handler, "/feed?view=following", nil, 401, "unauthenticated")
	stale := browserSession{cookie: &http.Cookie{Name: "stacktrace_session_dev", Value: strings.Repeat("A", 43)}}
	feedHTTPError(t, handler, continuation, &stale, 401, "unauthenticated")
	readHTTPFeed(t, handler, "/feed", &stale)
	for _, tc := range []struct {
		name, change, restore string
		arg                   any
	}{
		{"expired", `UPDATE sessions SET created_at=statement_timestamp()-interval '1 hour',expires_at=statement_timestamp()-interval '1 second' WHERE token_hash=$1`, `UPDATE sessions SET expires_at=statement_timestamp()+interval '1 hour' WHERE token_hash=$1`, hash},
		{"disabled", `UPDATE accounts SET disabled_at=statement_timestamp() WHERE id=$1`, `UPDATE accounts SET disabled_at=NULL WHERE id=$1`, alice.Account.ID},
		{"revoked", `DELETE FROM sessions WHERE token_hash=$1`, "", hash},
	} {
		t.Run(tc.name, func(t *testing.T) {
			feedExec(t, store, tc.change, tc.arg)
			feedHTTPError(t, handler, continuation, &alice, 401, "unauthenticated")
			page := readHTTPFeed(t, handler, "/feed", &alice)
			if len(page.Items) != 2 || feedHTTPPost(t, page.Items[0].Post).Viewer != nil {
				t.Fatal("stale/disabled session must be anonymous on public reads")
			}
			if tc.restore != "" {
				feedExec(t, store, tc.restore, tc.arg)
			}
		})
	}
}

func TestFeedHTTPAccountHandleRouting(t *testing.T) {
	_, handler := identityHandler(t)
	for i, handle := range []string{"feed", "follow"} {
		account := registerBrowser(t, handler, handle, fmt.Sprintf("192.0.2.%d", 176+i))
		w := contentRequest(handler, "GET", "/accounts/by-handle/"+handle, "", "", nil, "192.0.2.178")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"`+string(account.Account.ID)+`"`) {
			t.Fatalf("handle=%s status=%d body=%s", handle, w.Code, w.Body.String())
		}
		page := readHTTPFeed(t, handler, "/accounts/"+string(account.Account.ID)+"/feed", nil)
		if len(page.Items) != 0 || page.NextCursor != nil {
			t.Fatalf("empty account page=%+v", page)
		}
	}
	// Exercise the real handler's escaped-segment discrimination as well.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/accounts/"+string(app.NewID())+"/foo%2Ffeed", nil))
	if w.Code != 404 || w.Header().Get("Allow") != "" {
		t.Fatalf("encoded resource status=%d headers=%v", w.Code, w.Header())
	}
}
