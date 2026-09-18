package postgres

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func contentCounts(t *testing.T, store *Store) [5]int {
	t.Helper()
	var counts [5]int
	for i, query := range []string{
		"SELECT count(*) FROM posts WHERE deleted_at IS NULL",
		"SELECT count(*) FROM replies WHERE deleted_at IS NULL",
		"SELECT count(*) FROM reactions",
		"SELECT count(*) FROM reposts",
		"SELECT count(*) FROM bookmarks",
	} {
		if err := store.db.QueryRowContext(context.Background(), query).Scan(&counts[i]); err != nil {
			t.Fatal(err)
		}
	}
	return counts
}

func TestContentHTTPGuardsAndErrorsRegression(t *testing.T) {
	store, handler := identityHandler(t)
	alice := registerBrowser(t, handler, "guard_alice", "192.0.2.151")
	post := decodeHTTPPost(t, contentRequest(handler, "POST", "/posts", `{"body":"parent"}`, "guard-post", &alice, "192.0.2.151"), http.StatusCreated)
	reply := contentRequest(handler, "POST", "/posts/"+string(post.ID)+"/replies", `{"body":"reply"}`, "guard-reply", &alice, "192.0.2.151")
	var replyResult struct {
		Reply struct {
			ID app.ID `json:"id"`
		} `json:"reply"`
	}
	if err := json.Unmarshal(reply.Body.Bytes(), &replyResult); err != nil || reply.Code != http.StatusCreated {
		t.Fatalf("create reply: %d %s", reply.Code, reply.Body.String())
	}

	for _, tt := range []struct{ method, path, body, key string }{
		{"POST", "/posts", `{"body":"rejected"}`, "guard-create"},
		{"POST", "/posts/" + string(post.ID) + "/replies", `{"body":"rejected"}`, "guard-reply-create"},
		{"DELETE", "/posts/" + string(post.ID), "", ""},
		{"DELETE", "/replies/" + string(replyResult.Reply.ID), "", ""},
		{"PUT", "/posts/" + string(post.ID) + "/reaction", `{"kind":"useful"}`, ""},
		{"PUT", "/posts/" + string(post.ID) + "/repost", "", ""},
		{"PUT", "/posts/" + string(post.ID) + "/bookmark", "", ""},
	} {
		t.Run(tt.method+tt.path, func(t *testing.T) {
			before := contentCounts(t, store)
			r := httptest.NewRequest(tt.method, "/api/v1"+tt.path, strings.NewReader(tt.body))
			r.RemoteAddr = "192.0.2.151:12345"
			r.Header.Set("Content-Type", "application/json")
			if tt.key != "" {
				r.Header.Set("Idempotency-Key", tt.key)
			}
			r.AddCookie(alice.cookie) // A live cookie without Origin must still fail before a store mutation.
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden || contentCounts(t, store) != before {
				t.Fatalf("rejected request mutated or wrong status: %d %s", w.Code, w.Body.String())
			}
		})
	}

	for _, session := range []*browserSession{
		{cookie: alice.cookie},
		{cookie: alice.cookie, CSRFToken: func() *string { token := "wrong"; return &token }()},
	} {
		before := contentCounts(t, store)
		if w := contentRequest(handler, "PUT", "/posts/"+string(post.ID)+"/reaction", `{"kind":"useful"}`, "", session, "192.0.2.151"); w.Code != http.StatusForbidden || contentCounts(t, store) != before {
			t.Fatalf("CSRF rejection mutated or status=%d", w.Code)
		}
	}
	if w := contentRequest(handler, "POST", "/posts", `{"body":"anonymous"}`, "anon", nil, "192.0.2.152"); w.Code != http.StatusUnauthorized {
		t.Fatalf("trusted-origin anonymous write status=%d", w.Code)
	}

	for _, tt := range []struct {
		name, method, path, body string
		status                   int
	}{
		{"unknown", "GET", "/posts/" + string(app.NewID()), "", 404},
		{"bad ID", "GET", "/posts/not-a-uuid", "", 400},
		{"domain", "POST", "/posts", `{"body":" "}`, 422},
		{"body too large", "POST", "/posts", `{"body":"` + strings.Repeat("x", 65<<10) + `"}`, 413},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := contentRequest(handler, tt.method, tt.path, tt.body, "error-"+tt.name, &alice, "192.0.2.151")
			if w.Code != tt.status || !json.Valid(w.Body.Bytes()) || w.Header().Get("Access-Control-Allow-Origin") != testOrigin {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	// contentRequest starts as JSON; use a direct request for the media-type contract.
	r := httptest.NewRequest("POST", "/api/v1/posts", strings.NewReader(`{"body":"wrong media"}`))
	r.RemoteAddr = "192.0.2.151:12345"
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Set("Idempotency-Key", "wrong-media")
	r.AddCookie(alice.cookie)
	r.Header.Set("X-CSRF-Token", *alice.CSRFToken)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("wrong media status=%d", w.Code)
	}
	method := contentRequest(handler, "PATCH", "/posts/"+string(post.ID), "", "", nil, "192.0.2.153")
	if method.Code != http.StatusMethodNotAllowed || method.Header().Get("Allow") != "GET, HEAD, DELETE" || !json.Valid(method.Body.Bytes()) || method.Header().Get("Access-Control-Allow-Origin") != testOrigin {
		t.Fatalf("method response: %d allow=%q body=%s", method.Code, method.Header().Get("Allow"), method.Body.String())
	}
}

func TestContentHTTPRetriesDeletesAndDTORegression(t *testing.T) {
	_, handler := identityHandler(t)
	alice := registerBrowser(t, handler, "retry_alice", "192.0.2.161")
	bob := registerBrowser(t, handler, "retry_bob", "192.0.2.162")
	createdResponse := contentRequest(handler, "POST", "/posts", `{"body":"  <script>alert(1)</script> #Go #go #API  ","code":{"language":" go ","filename":" main.go ","source":"\tpackage main\n"}}`, "dto", &alice, "192.0.2.161")
	created := decodeHTTPPost(t, createdResponse, 201)
	if created.Body != "<script>alert(1)</script> #Go #go #API" || created.Code == nil || created.Code.Language != "go" || created.Code.Filename != "main.go" || created.Code.Source != "\tpackage main\n" || len(created.Tags) != 2 || created.Tags[0].Slug != "go" || created.Tags[1].Slug != "api" || len(created.Counts.Kinds) != 5 || created.Counts.ReactionsTotal != 0 || created.Viewer == nil {
		t.Fatalf("noncanonical DTO: %+v", created)
	}
	var createdRaw map[string]json.RawMessage
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &createdRaw); err != nil {
		t.Fatal(err)
	}
	var createdAt string
	var author struct {
		ID app.ID `json:"id"`
	}
	var counts struct {
		Kinds map[string]int64 `json:"reactions_by_kind"`
	}
	if err := json.Unmarshal(createdRaw["created_at"], &createdAt); err != nil || json.Unmarshal(createdRaw["author"], &author) != nil || json.Unmarshal(createdRaw["counts"], &counts) != nil {
		t.Fatalf("invalid canonical DTO: %s", createdResponse.Body.String())
	}
	createdTime, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil || createdTime.UTC().Format(time.RFC3339Nano) != createdAt || author.ID != alice.Account.ID || len(counts.Kinds) != 5 {
		t.Fatalf("incorrect timestamp, summary, or reaction keys: time=%q author=%q reactions=%v body=%s", createdAt, author.ID, counts.Kinds, createdResponse.Body.String())
	}
	for _, kind := range []string{"useful", "agree", "brilliant", "spicy", "ship"} {
		if counts.Kinds[kind] != 0 {
			t.Fatalf("nonzero or missing %s count: %s", kind, createdResponse.Body.String())
		}
	}
	// Unknown authority and curated/count fields must never be accepted on either creation route.
	for _, body := range []string{`{"body":"x","author_id":"` + string(bob.Account.ID) + `"}`, `{"body":"x","quoted_post":{"id":"` + string(created.ID) + `"}}`, `{"body":"x","is_spicy":true}`, `{"body":"x","is_generated":true}`, `{"body":"x","counts":{}}`} {
		if w := contentRequest(handler, "POST", "/posts", body, "authority-"+string(rune('a'+len(body)%20)), &alice, "192.0.2.161"); w.Code != 400 {
			t.Fatalf("post authority accepted: %d", w.Code)
		}
	}
	if w := contentRequest(handler, "POST", "/posts", `{"body":"invalid key"}`, "invalid key", &alice, "192.0.2.161"); w.Code != 400 {
		t.Fatalf("invalid idempotency key accepted: %d", w.Code)
	}
	if w := contentRequest(handler, "POST", "/posts/"+string(created.ID)+"/replies", `{"body":"x","author_id":"`+string(bob.Account.ID)+`"}`, "reply-authority", &alice, "192.0.2.161"); w.Code != 400 {
		t.Fatalf("reply authority accepted: %d", w.Code)
	}

	first := decodeHTTPPost(t, contentRequest(handler, "POST", "/posts", `{"body":" normalized "}`, "same-key", &alice, "192.0.2.161"), 201)
	plain := contentRequest(handler, "GET", "/posts/"+string(first.ID), "", "", nil, "192.0.2.163")
	var plainRaw map[string]json.RawMessage
	if err := json.Unmarshal(plain.Body.Bytes(), &plainRaw); err != nil || plain.Code != 200 || string(plainRaw["viewer"]) != "null" || string(plainRaw["code"]) != "null" || string(plainRaw["quote"]) != "null" || string(plainRaw["tags"]) != "[]" || !strings.Contains(string(plainRaw["reply_preview"]), `"items":[]`) || strings.Contains(plain.Body.String(), "bookmark_count") {
		t.Fatalf("plain anonymous DTO leaked optional/private fields: %d %s", plain.Code, plain.Body.String())
	}
	if w := contentRequest(handler, "POST", "/posts", `{"body":"different"}`, "same-key", &alice, "192.0.2.161"); w.Code != 409 {
		t.Fatalf("different idempotency body=%d", w.Code)
	}
	if w := contentRequest(handler, "PUT", "/posts/"+string(first.ID)+"/reaction", `{"kind":"useful"}`, "", &bob, "192.0.2.162"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := contentRequest(handler, "POST", "/posts/"+string(first.ID)+"/replies", `{"body":"current"}`, "current-reply", &bob, "192.0.2.162"); w.Code != 201 {
		t.Fatal(w.Code)
	}
	retry := decodeHTTPPost(t, contentRequest(handler, "POST", "/posts", `{"body":"normalized"}`, "same-key", &alice, "192.0.2.161"), 201)
	if retry.ID != first.ID || retry.Counts.Replies != 1 || retry.Counts.ReactionsTotal != 1 {
		t.Fatalf("retry stale: %+v", retry.Counts)
	}

	source := decodeHTTPPost(t, contentRequest(handler, "POST", "/posts", `{"body":"secret"}`, "source", &alice, "192.0.2.161"), 201)
	quote := decodeHTTPPost(t, contentRequest(handler, "POST", "/posts", `{"body":"quote","quoted_post_id":"`+string(source.ID)+`"}`, "quote", &alice, "192.0.2.161"), 201)
	if w := contentRequest(handler, "DELETE", "/posts/"+string(source.ID), "", "", &alice, "192.0.2.161"); w.Code != 204 {
		t.Fatal(w.Code)
	}
	quotedRetry := decodeHTTPPost(t, contentRequest(handler, "POST", "/posts", `{"body":"quote","quoted_post_id":"`+string(source.ID)+`"}`, "quote", &alice, "192.0.2.161"), 201)
	if quotedRetry.ID != quote.ID || len(quotedRetry.Quote) != 2 {
		t.Fatalf("quote retry did not redact: %+v", quotedRetry.Quote)
	}
	if w := contentRequest(handler, "DELETE", "/posts/"+string(quote.ID), "", "", &alice, "192.0.2.161"); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if w := contentRequest(handler, "POST", "/posts", `{"body":"quote","quoted_post_id":"`+string(source.ID)+`"}`, "quote", &alice, "192.0.2.161"); w.Code != 410 {
		t.Fatalf("deleted quote retry=%d", w.Code)
	}
}

func TestContentHTTPReplyAndRelationshipDeleteRegression(t *testing.T) {
	_, handler := identityHandler(t)
	alice := registerBrowser(t, handler, "delete_alice", "192.0.2.171")
	bob := registerBrowser(t, handler, "delete_bob", "192.0.2.172")
	post := decodeHTTPPost(t, contentRequest(handler, "POST", "/posts", `{"body":"parent"}`, "delete-parent", &alice, "192.0.2.171"), 201)
	created := contentRequest(handler, "POST", "/posts/"+string(post.ID)+"/replies", `{"body":"child"}`, "delete-child", &alice, "192.0.2.171")
	var reply struct {
		Reply struct {
			ID app.ID `json:"id"`
		} `json:"reply"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &reply); err != nil || created.Code != 201 {
		t.Fatalf("create reply: %d %s", created.Code, created.Body.String())
	}
	if w := contentRequest(handler, "DELETE", "/replies/"+string(reply.Reply.ID), "", "", &bob, "192.0.2.172"); w.Code != 403 {
		t.Fatalf("nonowner reply delete=%d", w.Code)
	}
	if w := contentRequest(handler, "DELETE", "/replies/"+string(reply.Reply.ID), "", "", &alice, "192.0.2.171"); w.Code != 204 {
		t.Fatalf("owner reply delete=%d", w.Code)
	}
	if w := contentRequest(handler, "POST", "/posts/"+string(post.ID)+"/replies", `{"body":"child"}`, "delete-child", &alice, "192.0.2.171"); w.Code != 410 {
		t.Fatalf("deleted reply retry=%d", w.Code)
	}
	if w := contentRequest(handler, "PUT", "/posts/"+string(post.ID)+"/reaction", `{"kind":"useful"}`, "", &alice, "192.0.2.171"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	removedReaction := decodeHTTPPost(t, contentRequest(handler, "DELETE", "/posts/"+string(post.ID)+"/reaction", "", "", &alice, "192.0.2.171"), 200)
	if removedReaction.Viewer == nil || removedReaction.Viewer.Reaction != nil || removedReaction.Counts.ReactionsTotal != 0 {
		t.Fatal("reaction delete was not refreshed")
	}
	if w := contentRequest(handler, "PUT", "/posts/"+string(post.ID)+"/bookmark", "", "", &alice, "192.0.2.171"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := contentRequest(handler, "DELETE", "/posts/"+string(post.ID)+"/bookmark", "", "", &alice, "192.0.2.171"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := contentRequest(handler, "GET", "/me/bookmarks", "", "", &alice, "192.0.2.171"); w.Code != 200 || !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatalf("bookmark delete list=%d %s", w.Code, w.Body.String())
	}
	if w := contentRequest(handler, "POST", "/posts/"+string(post.ID)+"/replies", `{"body":"parent retry"}`, "parent-retry", &alice, "192.0.2.171"); w.Code != 201 {
		t.Fatalf("create reply for parent retry=%d", w.Code)
	}
	if w := contentRequest(handler, "DELETE", "/posts/"+string(post.ID), "", "", &alice, "192.0.2.171"); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if w := contentRequest(handler, "POST", "/posts/"+string(post.ID)+"/replies", `{"body":"parent retry"}`, "parent-retry", &alice, "192.0.2.171"); w.Code != 410 {
		t.Fatalf("deleted parent retry=%d", w.Code)
	}
	if w := contentRequest(handler, "POST", "/posts/"+string(post.ID)+"/replies", `{"body":"other"}`, "parent-deleted", &bob, "192.0.2.172"); w.Code != 410 {
		t.Fatalf("deleted parent reply=%d", w.Code)
	}
}

func TestContentHTTPPreviewAndNewestCursorRegression(t *testing.T) {
	_, handler := identityHandler(t)
	alice := registerBrowser(t, handler, "page_alice", "192.0.2.181")
	post := decodeHTTPPost(t, contentRequest(handler, "POST", "/posts", `{"body":"page parent"}`, "page-parent", &alice, "192.0.2.181"), 201)
	var replyIDs [3]app.ID
	for i, body := range []string{"one", "two", "three"} {
		w := contentRequest(handler, "POST", "/posts/"+string(post.ID)+"/replies", `{"body":"`+body+`"}`, "page-"+body, &alice, "192.0.2.181")
		var result struct {
			Reply struct {
				ID app.ID `json:"id"`
			} `json:"reply"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 201 || result.Reply.ID == "" {
			t.Fatalf("create %s: %d %s", body, w.Code, w.Body.String())
		}
		replyIDs[i] = result.Reply.ID
	}
	detail := decodeHTTPPost(t, contentRequest(handler, "GET", "/posts/"+string(post.ID), "", "", nil, "192.0.2.182"), 200)
	if detail.Counts.Replies != 3 || len(detail.ReplyPreview.Items) != 2 || detail.ReplyPreview.NextCursor == nil {
		t.Fatal("missing bounded preview continuation")
	}
	previewNext := contentRequest(handler, "GET", "/posts/"+string(post.ID)+"/replies?cursor="+*detail.ReplyPreview.NextCursor, "", "", nil, "192.0.2.182")
	var oldest struct {
		Items []struct {
			ID app.ID `json:"id"`
		} `json:"items"`
		Next *string `json:"next_cursor"`
	}
	var previewIDs [2]struct {
		ID app.ID `json:"id"`
	}
	if err := json.Unmarshal(detail.ReplyPreview.Items[0], &previewIDs[0]); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(detail.ReplyPreview.Items[1], &previewIDs[1]); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(previewNext.Body.Bytes(), &oldest); err != nil {
		t.Fatal(err)
	}
	if previewIDs[0].ID != replyIDs[0] || previewIDs[1].ID != replyIDs[1] || previewNext.Code != 200 || len(oldest.Items) != 1 || oldest.Items[0].ID != replyIDs[2] || oldest.Next != nil {
		t.Fatalf("preview did not continue: %d %s", previewNext.Code, previewNext.Body.String())
	}
	newest := contentRequest(handler, "GET", "/posts/"+string(post.ID)+"/replies?sort=newest&limit=1", "", "", nil, "192.0.2.182")
	var firstNewest struct {
		Items []struct {
			ID app.ID `json:"id"`
		} `json:"items"`
		Next *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(newest.Body.Bytes(), &firstNewest); err != nil || newest.Code != 200 || len(firstNewest.Items) != 1 || firstNewest.Items[0].ID != replyIDs[2] || firstNewest.Next == nil {
		t.Fatalf("newest page: %d %s", newest.Code, newest.Body.String())
	}
	if w := contentRequest(handler, "POST", "/posts/"+string(post.ID)+"/replies", `{"body":"later"}`, "page-later", &alice, "192.0.2.181"); w.Code != 201 {
		t.Fatal(w.Code)
	}
	continued := contentRequest(handler, "GET", "/posts/"+string(post.ID)+"/replies?sort=newest&cursor="+*firstNewest.Next, "", "", nil, "192.0.2.182")
	var continuedPage struct {
		Items []struct {
			ID app.ID `json:"id"`
		} `json:"items"`
		Next *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(continued.Body.Bytes(), &continuedPage); err != nil || continued.Code != 200 || len(continuedPage.Items) != 2 || continuedPage.Items[0].ID != replyIDs[1] || continuedPage.Items[1].ID != replyIDs[0] || continuedPage.Next != nil || strings.Contains(continued.Body.String(), `"body":"later"`) {
		t.Fatalf("old ceiling admitted later reply: %s", continued.Body.String())
	}
	fresh := contentRequest(handler, "GET", "/posts/"+string(post.ID)+"/replies?sort=newest&limit=1", "", "", nil, "192.0.2.182")
	var freshPage struct {
		Items []struct {
			ID   app.ID `json:"id"`
			Body string `json:"body"`
		} `json:"items"`
	}
	if err := json.Unmarshal(fresh.Body.Bytes(), &freshPage); err != nil || fresh.Code != 200 || len(freshPage.Items) != 1 || freshPage.Items[0].Body != "later" || !strings.Contains(fresh.Body.String(), `"body":"later"`) {
		t.Fatalf("fresh newest omitted later reply: %s", fresh.Body.String())
	}
	if w := contentRequest(handler, "GET", "/posts/"+string(post.ID)+"/replies?sort=oldest&cursor="+*firstNewest.Next, "", "", nil, "192.0.2.182"); w.Code != 400 {
		t.Fatalf("sort binding accepted: %d", w.Code)
	}
	other := decodeHTTPPost(t, contentRequest(handler, "POST", "/posts", `{"body":"other"}`, "other-parent", &alice, "192.0.2.181"), 201)
	if w := contentRequest(handler, "GET", "/posts/"+string(other.ID)+"/replies?sort=newest&cursor="+*firstNewest.Next, "", "", nil, "192.0.2.182"); w.Code != 400 {
		t.Fatalf("cross post cursor accepted: %d", w.Code)
	}
	for _, query := range []string{"?limit=", "?limit=0", "?limit=51", "?limit=1&limit=2", "?sort=popular", "?unknown=x"} {
		if w := contentRequest(handler, "GET", "/posts/"+string(post.ID)+"/replies"+query, "", "", nil, "192.0.2.182"); w.Code != 400 {
			t.Fatalf("invalid query %q accepted: %d", query, w.Code)
		}
	}
}
