package postgres

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/SophearithSaing/stacktrace-api/internal/api"
	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestGenerationPublicationHTTPProvenance(t *testing.T) {
	for _, kind := range []app.GenerationOutput{app.OutputQuote, app.OutputReply} {
		t.Run(string(kind), func(t *testing.T) {
			store, job, attempt, output := publicationFixture(t, kind, "Public provenance without diagnostics #Provenance")
			published, err := store.PublishGeneration(context.Background(), job.ID, 1, attempt.ID, output)
			if err != nil {
				t.Fatal(err)
			}
			handler := api.NewHandler(store, []string{testOrigin}, []byte(strings.Repeat("k", 32)), []byte(strings.Repeat("c", 32)), false, slog.New(slog.NewJSONHandler(io.Discard, nil)))
			viewer := registerBrowser(t, handler, "provenance_viewer", "192.0.2.219")
			postID := job.SourcePostID
			if kind == app.OutputQuote {
				postID = published.ResultPostID
			}
			if _, err := store.SetBookmark(context.Background(), app.SessionHash(viewer.cookie.Value), *postID, true); err != nil {
				t.Fatal(err)
			}
			assertSafe := func(body string) {
				t.Helper()
				for _, forbidden := range []string{"generation_output_v1", "context_hash", "persona_version", "trigger_key", "lease_version", "published_attempt_id", "provider_request_id", string(attempt.ID), string(job.ID)} {
					if strings.Contains(body, forbidden) {
						t.Fatalf("private diagnostic exposed: %s", forbidden)
					}
				}
			}
			assertPost := func(raw []byte) {
				t.Helper()
				assertSafe(string(raw))
				var post struct {
					ID        app.ID                     `json:"id"`
					Generated bool                       `json:"is_generated"`
					Quote     map[string]json.RawMessage `json:"quote"`
					Preview   struct {
						Items []struct {
							ID        app.ID `json:"id"`
							Generated bool   `json:"is_generated"`
						} `json:"items"`
					} `json:"reply_preview"`
				}
				if err := json.Unmarshal(raw, &post); err != nil || post.ID != *postID || post.Generated != (kind == app.OutputQuote) {
					t.Fatalf("post provenance: %s %v", raw, err)
				}
				if kind == app.OutputQuote {
					if _, leaked := post.Quote["is_generated"]; leaked || len(post.Quote) == 0 {
						t.Fatalf("changed reduced quote projection: %v", post.Quote)
					}
				} else if len(post.Preview.Items) != 2 || post.Preview.Items[0].Generated || !post.Preview.Items[1].Generated || post.Preview.Items[1].ID != *published.ResultReplyID {
					t.Fatalf("preview provenance: %s", raw)
				}
			}
			for _, session := range []*browserSession{nil, &viewer} {
				w := contentRequest(handler, "GET", "/posts/"+string(*postID), "", "", session, "192.0.2.219")
				if w.Code != http.StatusOK {
					t.Fatal(w.Body.String())
				}
				assertPost(w.Body.Bytes())
				feed := readHTTPFeed(t, handler, "/feed", session)
				found := false
				for _, item := range feed.Items {
					var post struct {
						ID app.ID `json:"id"`
					}
					if err := json.Unmarshal(item.Post, &post); err != nil {
						t.Fatal(err)
					}
					if post.ID == *postID {
						found = true
						assertPost(item.Post)
					}
				}
				if !found {
					t.Fatal("published post missing from feed")
				}
			}
			w := contentRequest(handler, "GET", "/me/bookmarks", "", "", &viewer, "192.0.2.219")
			var saved struct {
				Items []json.RawMessage `json:"items"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil || w.Code != http.StatusOK || len(saved.Items) != 1 {
				t.Fatalf("bookmarks: %s %v", w.Body.String(), err)
			}
			assertPost(saved.Items[0])
			if kind == app.OutputReply {
				w = contentRequest(handler, "GET", "/posts/"+string(*postID)+"/replies", "", "", nil, "192.0.2.219")
				var page struct {
					Items []struct {
						ID        app.ID `json:"id"`
						Generated bool   `json:"is_generated"`
					} `json:"items"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != http.StatusOK || len(page.Items) != 2 || page.Items[0].Generated || !page.Items[1].Generated || page.Items[1].ID != *published.ResultReplyID {
					t.Fatalf("reply provenance: %s %v", w.Body.String(), err)
				}
				assertSafe(w.Body.String())
			}
			// Agent identity alone is not provenance; human idempotency/auth stays
			// on its separate session boundary.
			legacy, _ := socialPost(t, store, job.AgentID, "Legacy agent content")
			w = contentRequest(handler, "GET", "/posts/"+string(legacy), "", "", nil, "192.0.2.219")
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"is_generated":false`) {
				t.Fatal(w.Body.String())
			}
			first := contentRequest(handler, "POST", "/posts", `{"body":"Human authored content"}`, "human-provenance", &viewer, "192.0.2.219")
			firstPost := decodeHTTPPost(t, first, http.StatusCreated)
			retry := decodeHTTPPost(t, contentRequest(handler, "POST", "/posts", `{"body":"Human authored content"}`, "human-provenance", &viewer, "192.0.2.219"), http.StatusCreated)
			if retry.ID != firstPost.ID || !strings.Contains(first.Body.String(), `"is_generated":false`) {
				t.Fatal("human idempotency/provenance changed")
			}
			denied := contentRequest(handler, "POST", "/posts", `{"body":"Unauthorized"}`, "denied-provenance", nil, "192.0.2.219")
			if denied.Code != http.StatusUnauthorized && denied.Code != http.StatusForbidden {
				t.Fatalf("human auth bypass: %d", denied.Code)
			}
		})
	}
}
