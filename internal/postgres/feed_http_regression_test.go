package postgres

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestFeedHTTPDeletionRedactionAndAuthoritativeState(t *testing.T) {
	_, handler := identityHandler(t)
	alice := registerBrowser(t, handler, "feed_delete_alice", "192.0.2.151")
	bob := registerBrowser(t, handler, "feed_delete_bob", "192.0.2.152")
	create := func(body, key string) httpPost {
		t.Helper()
		return decodeHTTPPost(t, contentRequest(handler, "POST", "/posts", body, key, &alice, "192.0.2.151"), 201)
	}
	rootBody := `{"body":"secret_original #FeedRegression","code":{"language":"go","filename":"secret.go","source":"// secret_code\n"}}`
	root := create(rootBody, "root")
	middleBody := `{"body":"  plain <script> 🦀 #FeedRegression  ","quoted_post_id":"` + string(root.ID) + `","code":{"language":" go ","filename":" main.go ","source":"\t// preserved <script> 🦀\n"}}`
	middle := create(middleBody, "middle")
	outer := create(`{"body":"outer #FeedRegression","quoted_post_id":"`+string(middle.ID)+`"}`, "outer")
	for _, id := range []app.ID{root.ID, middle.ID, outer.ID} {
		decodeHTTPPost(t, contentRequest(handler, "PUT", "/posts/"+string(id)+"/bookmark", "", "", &alice, "192.0.2.151"), 200)
	}
	for _, id := range []app.ID{root.ID, middle.ID} {
		w := contentRequest(handler, "PUT", "/posts/"+string(id)+"/repost", "", "", &alice, "192.0.2.151")
		if w.Code != 200 {
			t.Fatalf("repost=%d %s", w.Code, w.Body.String())
		}
	}
	if w := contentRequest(handler, "POST", "/posts/"+string(middle.ID)+"/replies", `{"body":"reply 🦀"}`, "reply", &bob, "192.0.2.152"); w.Code != 201 {
		t.Fatalf("reply=%d %s", w.Code, w.Body.String())
	}
	decodeHTTPPost(t, contentRequest(handler, "PUT", "/posts/"+string(middle.ID)+"/reaction", `{"kind":"ship"}`, "", &bob, "192.0.2.152"), 200)
	authoritative := decodeHTTPPost(t, contentRequest(handler, "PUT", "/posts/"+string(middle.ID)+"/reaction", `{"kind":"useful"}`, "", &alice, "192.0.2.151"), 200)
	if authoritative.ID != middle.ID || authoritative.Body != "plain <script> 🦀 #FeedRegression" || authoritative.Code == nil || authoritative.Code.Language != "go" || authoritative.Code.Filename != "main.go" || authoritative.Code.Source != "\t// preserved <script> 🦀\n" || authoritative.Counts.ReactionsTotal != 2 || authoritative.Counts.Replies != 1 || authoritative.Counts.Reposts != 1 || authoritative.Viewer == nil || !authoritative.Viewer.Bookmarked || !authoritative.Viewer.Reposted || authoritative.Viewer.Reaction == nil || *authoritative.Viewer.Reaction != "useful" {
		t.Fatalf("mutation did not return canonical current state: %+v", authoritative)
	}
	// Creation retries must rebuild current state, not replay the original empty
	// counts. No preview cursor is needed for one reply, so compare complete DTOs.
	retry := create(middleBody, "middle")
	if !reflect.DeepEqual(retry, authoritative) {
		t.Fatalf("retry=%+v authoritative=%+v", retry, authoritative)
	}
	savedPosts := func() []json.RawMessage {
		t.Helper()
		w := contentRequest(handler, "GET", "/me/bookmarks", "", "", &alice, "192.0.2.151")
		var saved struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil || w.Code != 200 {
			t.Fatalf("saved=%d %s err=%v", w.Code, w.Body.String(), err)
		}
		return saved.Items
	}
	for _, entry := range readHTTPFeed(t, handler, "/feed?tag=feedregression", &alice).Items {
		p := feedHTTPPost(t, entry.Post)
		if p.ID == middle.ID && !reflect.DeepEqual(p, authoritative) {
			t.Fatal("feed disagrees with authoritative mutation")
		}
		if p.ID == outer.ID {
			if len(p.Quote) != 4 || string(p.Quote["id"]) != `"`+string(middle.ID)+`"` || p.Quote["code"] != nil || p.Quote["quote"] != nil || strings.Contains(string(entry.Post), "secret_original") || strings.Contains(string(entry.Post), "secret_code") {
				t.Fatalf("recursive quote/code expansion: %s", entry.Post)
			}
		}
	}
	for _, raw := range savedPosts() {
		if p := feedHTTPPost(t, raw); p.ID == middle.ID && !reflect.DeepEqual(p, authoritative) {
			t.Fatal("saved post disagrees with authoritative mutation")
		}
	}
	detail := decodeHTTPPost(t, contentRequest(handler, "GET", "/posts/"+string(middle.ID), "", "", &alice, "192.0.2.151"), 200)
	if !reflect.DeepEqual(detail, authoritative) {
		t.Fatal("detail disagrees with authoritative mutation")
	}

	for _, deleted := range []app.ID{root.ID, middle.ID} {
		for attempt := range 2 { // Repeated deletion keeps the existing 410 contract.
			w := contentRequest(handler, "DELETE", "/posts/"+string(deleted), "", "", &alice, "192.0.2.151")
			want := http.StatusNoContent
			if attempt == 1 {
				want = http.StatusGone
			}
			if w.Code != want {
				t.Fatalf("delete=%d %s", w.Code, w.Body.String())
			}
		}
		feedHTTPError(t, handler, "/posts/"+string(deleted), nil, 410, "deleted")
		feedHTTPError(t, handler, "/posts/"+string(deleted)+"/replies", nil, 410, "deleted")
		for _, path := range []string{"/feed", "/feed?sort=reacted", "/feed?view=following", "/feed?tag=feedregression", "/accounts/" + string(alice.Account.ID) + "/feed"} {
			page := readHTTPFeed(t, handler, path, &alice)
			want := 3 // middle original/repost and outer original
			if deleted == middle.ID {
				want = 1
			}
			if len(page.Items) != want {
				t.Fatalf("deleted source retained feed events: %s %+v", path, page)
			}
			for _, entry := range page.Items {
				p := feedHTTPPost(t, entry.Post)
				if p.ID == deleted || p.ID == root.ID || strings.Contains(string(entry.Post), "secret_original") || strings.Contains(string(entry.Post), "secret_code") {
					t.Fatalf("deleted content leaked: %s", entry.Post)
				}
				if string(p.Quote["id"]) == `"`+string(deleted)+`"` && (len(p.Quote) != 2 || string(p.Quote["availability"]) != `"deleted"`) {
					t.Fatalf("quote was not redacted: %s", entry.Post)
				}
			}
		}
		for _, raw := range savedPosts() {
			p := feedHTTPPost(t, raw)
			if p.ID == deleted || p.ID == root.ID || strings.Contains(string(raw), "secret_original") || strings.Contains(string(raw), "secret_code") {
				t.Fatalf("deleted saved content leaked: %s", raw)
			}
		}
	}
	if saved := savedPosts(); len(saved) != 1 || feedHTTPPost(t, saved[0]).ID != outer.ID {
		t.Fatalf("saved deletion projection=%s", saved)
	}
	outerDetail := decodeHTTPPost(t, contentRequest(handler, "GET", "/posts/"+string(outer.ID), "", "", nil, "192.0.2.153"), 200)
	if len(outerDetail.Quote) != 2 || string(outerDetail.Quote["availability"]) != `"deleted"` {
		t.Fatalf("detail quote redaction=%+v", outerDetail.Quote)
	}
	for _, tc := range []struct{ method, path, body, key string }{
		{http.MethodPost, "/posts", rootBody, "root"},
		{http.MethodPost, "/posts", middleBody, "middle"},
		{http.MethodPut, "/posts/" + string(root.ID) + "/repost", "", ""},
		{http.MethodPut, "/posts/" + string(root.ID) + "/bookmark", "", ""},
	} {
		w := contentRequest(handler, tc.method, tc.path, tc.body, tc.key, &alice, "192.0.2.151")
		if w.Code != 410 || strings.Contains(w.Body.String(), "secret_original") || strings.Contains(w.Body.String(), "secret_code") {
			t.Fatalf("deleted retry/mutation=%d %s", w.Code, w.Body.String())
		}
	}
}
