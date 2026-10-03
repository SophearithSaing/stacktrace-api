package postgres

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

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

func TestGeneratedReplyFreshNewestLongPageAndDeletion(t *testing.T) {
	store, job, attempt, output := publicationFixture(t, app.OutputReply, "later generated reply")
	oldAt := time.Now().UTC().Add(-time.Minute)
	oldIDs := make([]app.ID, 0, app.DefaultReadLimit+1)
	for index := range app.DefaultReadLimit + 1 {
		id := app.NewID()
		oldIDs = append(oldIDs, id)
		feedExec(t, store, `INSERT INTO replies(id,post_id,author_id,body,created_at) VALUES($1,$2,$3,$4,$5)`, id, *job.SourcePostID, job.AgentID, "older reply "+string(rune('a'+index)), oldAt)
	}
	handler := api.NewHandler(store, []string{testOrigin}, []byte(strings.Repeat("k", 32)), []byte(strings.Repeat("c", 32)), false, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	beforeNewest := contentRequest(handler, "GET", "/posts/"+string(*job.SourcePostID)+"/replies?sort=newest&limit=20", "", "", nil, "192.0.2.220")
	beforeOldest := contentRequest(handler, "GET", "/posts/"+string(*job.SourcePostID)+"/replies?limit=20", "", "", nil, "192.0.2.220")
	preview := decodeHTTPPost(t, contentRequest(handler, "GET", "/posts/"+string(*job.SourcePostID), "", "", nil, "192.0.2.220"), http.StatusOK)
	var before struct {
		Next *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(beforeNewest.Body.Bytes(), &before); err != nil || beforeNewest.Code != http.StatusOK || before.Next == nil {
		t.Fatalf("before newest=%d %s err=%v", beforeNewest.Code, beforeNewest.Body.String(), err)
	}
	newestCursor := *before.Next
	if err := json.Unmarshal(beforeOldest.Body.Bytes(), &before); err != nil || beforeOldest.Code != http.StatusOK || before.Next == nil || preview.ReplyPreview.NextCursor == nil {
		t.Fatalf("before oldest/preview=%d %s err=%v", beforeOldest.Code, beforeOldest.Body.String(), err)
	}
	oldestCursor, previewCursor := *before.Next, *preview.ReplyPreview.NextCursor
	if w := contentRequest(handler, "GET", "/posts/"+string(*job.SourcePostID)+"/replies?sort=oldest&cursor="+newestCursor, "", "", nil, "192.0.2.220"); w.Code != http.StatusBadRequest {
		t.Fatalf("sort binding=%d", w.Code)
	}
	published, err := store.PublishGeneration(context.Background(), job.ID, job.LeaseVersion, attempt.ID, output)
	if err != nil || published.ResultReplyID == nil {
		t.Fatalf("publish generated reply=%+v, %v", published, err)
	}
	w := contentRequest(handler, "GET", "/posts/"+string(*job.SourcePostID)+"/replies?sort=newest&limit=20", "", "", nil, "192.0.2.220")
	var page struct {
		Items []struct {
			ID        app.ID `json:"id"`
			Generated bool   `json:"is_generated"`
		} `json:"items"`
		Next *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != http.StatusOK || len(page.Items) != app.DefaultReadLimit || page.Next == nil || page.Items[0].ID != *published.ResultReplyID || !page.Items[0].Generated {
		t.Fatalf("fresh newest generated page=%d %s err=%v", w.Code, w.Body.String(), err)
	}
	sort.Slice(oldIDs, func(left, right int) bool { return oldIDs[left] > oldIDs[right] })
	for index := 1; index < len(page.Items); index++ {
		if page.Items[index].ID != oldIDs[index-1] || page.Items[index].Generated {
			t.Fatalf("tied newest order=%+v want=%v", page.Items, oldIDs)
		}
	}
	continued := contentRequest(handler, "GET", "/posts/"+string(*job.SourcePostID)+"/replies?sort=newest&cursor="+*page.Next, "", "", nil, "192.0.2.220")
	var rest struct {
		Items []struct {
			ID app.ID `json:"id"`
		} `json:"items"`
		Next *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(continued.Body.Bytes(), &rest); err != nil || continued.Code != http.StatusOK || len(rest.Items) != 3 || rest.Next != nil || rest.Items[0].ID != oldIDs[19] || rest.Items[1].ID != oldIDs[20] || strings.Contains(continued.Body.String(), string(*published.ResultReplyID)) {
		t.Fatalf("newest continuation=%d %s err=%v", continued.Code, continued.Body.String(), err)
	}
	for _, path := range []string{
		"/posts/" + string(*job.SourcePostID) + "/replies?sort=newest&cursor=" + newestCursor,
		"/posts/" + string(*job.SourcePostID) + "/replies?cursor=" + oldestCursor,
		"/posts/" + string(*job.SourcePostID) + "/replies?cursor=" + previewCursor,
	} {
		w := contentRequest(handler, "GET", path, "", "", nil, "192.0.2.220")
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), string(*published.ResultReplyID)) {
			t.Fatalf("old window admitted generated reply=%d %s", w.Code, w.Body.String())
		}
	}
	feedExec(t, store, `UPDATE replies SET deleted_at=statement_timestamp() WHERE id=$1`, *published.ResultReplyID)
	fresh := contentRequest(handler, "GET", "/posts/"+string(*job.SourcePostID)+"/replies?sort=newest&limit=20", "", "", nil, "192.0.2.220")
	if fresh.Code != http.StatusOK || strings.Contains(fresh.Body.String(), string(*published.ResultReplyID)) {
		t.Fatalf("deleted generated reply remained visible=%s", fresh.Body.String())
	}
}
