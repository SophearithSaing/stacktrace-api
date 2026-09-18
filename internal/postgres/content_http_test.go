package postgres

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

type httpPost struct {
	ID   app.ID `json:"id"`
	Body string `json:"body"`
	Code *struct {
		Language string `json:"language"`
		Filename string `json:"filename"`
		Source   string `json:"source"`
	} `json:"code"`
	Tags []struct {
		Slug        string `json:"slug"`
		DisplayName string `json:"display_name"`
	} `json:"tags"`
	Author struct {
		ID app.ID `json:"id"`
	} `json:"author"`
	Counts struct {
		Replies        int64            `json:"replies"`
		Reposts        int64            `json:"reposts"`
		ReactionsTotal int64            `json:"reactions_total"`
		Kinds          map[string]int64 `json:"reactions_by_kind"`
	} `json:"counts"`
	Viewer *struct {
		Reaction   *string `json:"reaction"`
		Reposted   bool    `json:"reposted"`
		Bookmarked bool    `json:"bookmarked"`
	} `json:"viewer"`
	ReplyPreview struct {
		Items      []json.RawMessage `json:"items"`
		NextCursor *string           `json:"next_cursor"`
	} `json:"reply_preview"`
	Quote map[string]json.RawMessage `json:"quote"`
}

func contentRequest(handler http.Handler, method, path, body, key string, session *browserSession, ip string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api/v1"+path, strings.NewReader(body))
	r.RemoteAddr = ip + ":12345"
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if session != nil {
		if session.cookie != nil {
			r.AddCookie(session.cookie)
		}
		if session.CSRFToken != nil {
			r.Header.Set("X-CSRF-Token", *session.CSRFToken)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func decodeHTTPPost(t *testing.T, w *httptest.ResponseRecorder, status int) httpPost {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body.String())
	}
	var post httpPost
	if err := json.Unmarshal(w.Body.Bytes(), &post); err != nil {
		t.Fatal(err)
	}
	return post
}

func TestContentLifecycleAndPaginationHTTP(t *testing.T) {
	_, handler := identityHandler(t)
	alice := registerBrowser(t, handler, "content_alice", "192.0.2.31")
	bob := registerBrowser(t, handler, "content_bob", "192.0.2.32")

	if w := contentRequest(handler, "POST", "/posts", `{"body":"spoof","author_id":"`+string(bob.Account.ID)+`"}`, "spoof", &alice, "192.0.2.31"); w.Code != 400 {
		t.Fatalf("accepted author spoof: %d %s", w.Code, w.Body.String())
	}
	if w := contentRequest(handler, "POST", "/posts", `{"body":"missing key"}`, "", &alice, "192.0.2.31"); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_idempotency_key") {
		t.Fatalf("accepted missing idempotency key: %d %s", w.Code, w.Body.String())
	}
	duplicate := httptest.NewRequest("POST", "/api/v1/posts", strings.NewReader(`{"body":"duplicate key"}`))
	duplicate.RemoteAddr = "192.0.2.31:12345"
	duplicate.Header.Set("Origin", testOrigin)
	duplicate.Header.Set("Content-Type", "application/json")
	duplicate.Header.Add("Idempotency-Key", "one")
	duplicate.Header.Add("Idempotency-Key", "two")
	duplicate.AddCookie(alice.cookie)
	duplicate.Header.Set("X-CSRF-Token", *alice.CSRFToken)
	duplicateResponse := httptest.NewRecorder()
	handler.ServeHTTP(duplicateResponse, duplicate)
	if duplicateResponse.Code != 400 {
		t.Fatalf("accepted duplicate key header: %d", duplicateResponse.Code)
	}

	created := decodeHTTPPost(t, contentRequest(handler, "POST", "/posts", `{"body":" hello #Go ","code":{"language":"go","filename":"main.go","source":"package main"}}`, "post-one", &alice, "192.0.2.31"), 201)
	if created.ID == "" || created.Author.ID != alice.Account.ID || len(created.Counts.Kinds) != 5 || created.Viewer == nil || created.Viewer.Bookmarked || len(created.ReplyPreview.Items) != 0 {
		t.Fatalf("incorrect canonical create projection: %+v", created)
	}
	location := "/api/v1/posts/" + string(created.ID)
	createAgain := contentRequest(handler, "POST", "/posts", `{"body":"hello #Go","code":{"language":"go","filename":"main.go","source":"package main"}}`, "post-one", &alice, "192.0.2.31")
	if retry := decodeHTTPPost(t, createAgain, 201); retry.ID != created.ID || createAgain.Header().Get("Location") != location {
		t.Fatal("idempotent retry did not return canonical post/location")
	}

	for i := 0; i < 3; i++ {
		w := contentRequest(handler, "POST", "/posts/"+string(created.ID)+"/replies", `{"body":"reply `+string(rune('a'+i))+`"}`, "reply-"+string(rune('a'+i)), &bob, "192.0.2.32")
		if w.Code != 201 || !strings.Contains(w.Body.String(), `"reply_total":`+string(rune('1'+i))) {
			t.Fatalf("reply %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	detail := decodeHTTPPost(t, contentRequest(handler, "GET", "/posts/"+string(created.ID), "", "", &alice, "192.0.2.31"), 200)
	if detail.Counts.Replies != 3 || len(detail.ReplyPreview.Items) != 2 || detail.ReplyPreview.NextCursor == nil {
		t.Fatalf("incorrect count/preview: %+v", detail)
	}

	first := contentRequest(handler, "GET", "/posts/"+string(created.ID)+"/replies?limit=1", "", "", nil, "192.0.2.33")
	var page struct {
		Items []struct {
			ID app.ID `json:"id"`
		} `json:"items"`
		Total int64   `json:"total_count"`
		Next  *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil || first.Code != 200 || len(page.Items) != 1 || page.Total != 3 || page.Next == nil {
		t.Fatalf("first reply page: %d %s", first.Code, first.Body.String())
	}
	second := contentRequest(handler, "GET", "/posts/"+string(created.ID)+"/replies?limit=2&cursor="+*page.Next, "", "", nil, "192.0.2.33")
	var page2 struct {
		Items []struct {
			ID app.ID `json:"id"`
		} `json:"items"`
		Next *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &page2); err != nil || second.Code != 200 || len(page2.Items) != 2 || page2.Next != nil || page2.Items[0].ID == page.Items[0].ID {
		t.Fatalf("reply continuation: %d %s", second.Code, second.Body.String())
	}
	if w := contentRequest(handler, "GET", "/posts/"+string(created.ID)+"/replies?cursor="+*page.Next+"x", "", "", nil, "192.0.2.33"); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_cursor") {
		t.Fatalf("tampered cursor: %d %s", w.Code, w.Body.String())
	}
	if w := contentRequest(handler, "GET", "/posts/"+string(created.ID)+"/replies?limit=1&limit=2", "", "", nil, "192.0.2.33"); w.Code != 400 {
		t.Fatalf("duplicate query accepted: %d", w.Code)
	}

	reacted := decodeHTTPPost(t, contentRequest(handler, "PUT", "/posts/"+string(created.ID)+"/reaction", `{"kind":"useful"}`, "", &alice, "192.0.2.31"), 200)
	if reacted.Viewer == nil || reacted.Viewer.Reaction == nil || *reacted.Viewer.Reaction != "useful" || reacted.Counts.ReactionsTotal != 1 {
		t.Fatal("reaction projection incorrect")
	}
	if w := contentRequest(handler, "PUT", "/posts/"+string(created.ID)+"/reaction", `{"kind":"invalid"}`, "", &alice, "192.0.2.31"); w.Code != 422 || !strings.Contains(w.Body.String(), `"kind"`) {
		t.Fatalf("invalid reaction: %d %s", w.Code, w.Body.String())
	}
	var repost struct {
		Post             httpPost `json:"post"`
		RepostEntryID    *string  `json:"repost_entry_id"`
		RepostOccurredAt *string  `json:"repost_occurred_at"`
	}
	firstRepost := contentRequest(handler, "PUT", "/posts/"+string(created.ID)+"/repost", `{"account_id":"`+string(bob.Account.ID)+`"}`, "", &alice, "192.0.2.31")
	if err := json.Unmarshal(firstRepost.Body.Bytes(), &repost); err != nil || firstRepost.Code != 200 || repost.RepostEntryID == nil || !strings.HasPrefix(*repost.RepostEntryID, "repost:") || repost.RepostOccurredAt == nil || repost.Post.Viewer == nil || !repost.Post.Viewer.Reposted {
		t.Fatalf("repost projection: %d %s", firstRepost.Code, firstRepost.Body.String())
	}
	originalEntry, originalTime := *repost.RepostEntryID, *repost.RepostOccurredAt
	secondRepost := contentRequest(handler, "PUT", "/posts/"+string(created.ID)+"/repost", "", "", &alice, "192.0.2.31")
	if err := json.Unmarshal(secondRepost.Body.Bytes(), &repost); err != nil || repost.RepostEntryID == nil || *repost.RepostEntryID != originalEntry || repost.RepostOccurredAt == nil || *repost.RepostOccurredAt != originalTime {
		t.Fatalf("repost retry changed event: %d %s", secondRepost.Code, secondRepost.Body.String())
	}
	removedRepost := contentRequest(handler, "DELETE", "/posts/"+string(created.ID)+"/repost", "", "", &alice, "192.0.2.31")
	if err := json.Unmarshal(removedRepost.Body.Bytes(), &repost); err != nil || removedRepost.Code != 200 || repost.RepostEntryID != nil || repost.RepostOccurredAt != nil || repost.Post.Viewer == nil || repost.Post.Viewer.Reposted {
		t.Fatalf("repost removal projection: %d %s", removedRepost.Code, removedRepost.Body.String())
	}

	bookmarked := decodeHTTPPost(t, contentRequest(handler, "PUT", "/posts/"+string(created.ID)+"/bookmark", `{"account_id":"`+string(bob.Account.ID)+`"}`, "", &alice, "192.0.2.31"), 200)
	if bookmarked.Viewer == nil || !bookmarked.Viewer.Bookmarked {
		t.Fatal("bookmark not reflected")
	}
	bookmarks := contentRequest(handler, "GET", "/me/bookmarks?limit=1", "", "", &alice, "192.0.2.31")
	if bookmarks.Code != 200 || !strings.Contains(bookmarks.Body.String(), string(created.ID)) {
		t.Fatalf("private bookmarks missing: %d %s", bookmarks.Code, bookmarks.Body.String())
	}
	if w := contentRequest(handler, "GET", "/me/bookmarks", "", "", &bob, "192.0.2.32"); w.Code != 200 || strings.Contains(w.Body.String(), string(created.ID)) {
		t.Fatalf("bookmark leaked to bob: %d %s", w.Code, w.Body.String())
	}
	if w := contentRequest(handler, "GET", "/me/bookmarks", "", "", nil, "192.0.2.33"); w.Code != 401 {
		t.Fatalf("anonymous bookmarks status=%d", w.Code)
	}

	quoteSource := decodeHTTPPost(t, contentRequest(handler, "POST", "/posts", `{"body":"quoted secret"}`, "quote-source", &alice, "192.0.2.31"), 201)
	quoted := decodeHTTPPost(t, contentRequest(handler, "POST", "/posts", `{"body":"keeps quote","quoted_post_id":"`+string(quoteSource.ID)+`"}`, "quote-post", &alice, "192.0.2.31"), 201)
	if w := contentRequest(handler, "PUT", "/posts/"+string(quoted.ID)+"/bookmark", "", "", &alice, "192.0.2.31"); w.Code != 200 {
		t.Fatalf("second bookmark: %d %s", w.Code, w.Body.String())
	}
	bookmarkPage := contentRequest(handler, "GET", "/me/bookmarks?limit=1", "", "", &alice, "192.0.2.31")
	var bookmarkList struct {
		Items []httpPost `json:"items"`
		Next  *string    `json:"next_cursor"`
	}
	if err := json.Unmarshal(bookmarkPage.Body.Bytes(), &bookmarkList); err != nil || bookmarkPage.Code != 200 || len(bookmarkList.Items) != 1 || bookmarkList.Next == nil {
		t.Fatalf("bookmark page: %d %s", bookmarkPage.Code, bookmarkPage.Body.String())
	}
	bookmarkCursor := *bookmarkList.Next
	bookmarkNext := contentRequest(handler, "GET", "/me/bookmarks?limit=1&cursor="+bookmarkCursor, "", "", &alice, "192.0.2.31")
	if err := json.Unmarshal(bookmarkNext.Body.Bytes(), &bookmarkList); err != nil || bookmarkNext.Code != 200 || len(bookmarkList.Items) != 1 || bookmarkList.Next != nil {
		t.Fatalf("bookmark continuation: %d %s", bookmarkNext.Code, bookmarkNext.Body.String())
	}
	if w := contentRequest(handler, "GET", "/me/bookmarks?cursor="+bookmarkCursor, "", "", &bob, "192.0.2.32"); w.Code != 400 {
		t.Fatalf("cross-viewer bookmark cursor accepted: %d", w.Code)
	}
	if w := contentRequest(handler, "DELETE", "/posts/"+string(quoteSource.ID), "", "", &alice, "192.0.2.31"); w.Code != 204 {
		t.Fatalf("delete quote source: %d", w.Code)
	}
	redacted := decodeHTTPPost(t, contentRequest(handler, "GET", "/posts/"+string(quoted.ID), "", "", nil, "192.0.2.33"), 200)
	if len(redacted.Quote) != 2 || redacted.Quote["id"] == nil || redacted.Quote["availability"] == nil || strings.Contains(mustJSON(redacted.Quote), "secret") {
		t.Fatalf("deleted quote leaked fields: %s", mustJSON(redacted.Quote))
	}

	if w := contentRequest(handler, "DELETE", "/posts/"+string(created.ID), "", "", &bob, "192.0.2.32"); w.Code != 403 {
		t.Fatalf("non-owner delete: %d", w.Code)
	}
	if w := contentRequest(handler, "DELETE", "/posts/"+string(created.ID), "", "", &alice, "192.0.2.31"); w.Code != 204 {
		t.Fatalf("owner delete: %d %s", w.Code, w.Body.String())
	}
	if w := contentRequest(handler, "GET", "/posts/"+string(created.ID), "", "", nil, "192.0.2.33"); w.Code != 410 {
		t.Fatalf("deleted detail: %d", w.Code)
	}
	if w := contentRequest(handler, "POST", "/posts/"+string(created.ID)+"/replies", `{"body":"late"}`, "late-reply", &bob, "192.0.2.32"); w.Code != 410 {
		t.Fatalf("reply to deleted parent: %d %s", w.Code, w.Body.String())
	}
	if w := contentRequest(handler, "PATCH", "/posts/"+string(created.ID), "", "", nil, "192.0.2.33"); w.Code != 405 {
		t.Fatalf("method status=%d", w.Code)
	}
}

func mustJSON(value any) string {
	contents, _ := json.Marshal(value)
	return string(contents)
}
