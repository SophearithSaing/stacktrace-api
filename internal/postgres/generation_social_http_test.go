package postgres

import (
	"net/http"
	"strings"
	"testing"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestGenerationSocialHTTPAtomicity(t *testing.T) {
	store, handler := identityHandler(t)
	socialAgent(t, store, "http_agent", nil)
	alice := registerBrowser(t, handler, "trigger_http", "192.0.2.211")
	body := `{"body":"hello @HTTP_AGENT"}`
	w := contentRequest(handler, "POST", "/posts", body, "trigger-post", &alice, "192.0.2.211")
	post := decodeHTTPPost(t, w, http.StatusCreated)
	if strings.Contains(w.Body.String(), "trigger_key") || strings.Contains(w.Body.String(), "persona") || strings.Contains(w.Body.String(), "cooldown") {
		t.Fatal("generation internals leaked into DTO")
	}
	jobs := socialJobs(t, store)
	if len(jobs) != 1 || *jobs[0].SourcePostID != post.ID {
		t.Fatal("HTTP write did not atomically trigger")
	}
	retry := decodeHTTPPost(t, contentRequest(handler, "POST", "/posts", body, "trigger-post", &alice, "192.0.2.211"), http.StatusCreated)
	if retry.ID != post.ID || len(socialJobs(t, store)) != 1 {
		t.Fatal("HTTP retry duplicated")
	}
	if w := contentRequest(handler, "DELETE", "/posts/"+string(post.ID), "", "", &alice, "192.0.2.211"); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if socialJobs(t, store)[0].Status != app.JobCancelled {
		t.Fatal("HTTP delete did not invalidate job")
	}
	// A different responder avoids the first responder's retained spacing.
	socialAgent(t, store, "failure_agent", nil)
	generationSQL(t, store, `CREATE FUNCTION http_fail_generation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'secret provider detail'; END $$;
		CREATE TRIGGER http_fail_generation BEFORE INSERT ON generation_jobs FOR EACH ROW EXECUTE FUNCTION http_fail_generation()`)
	w = contentRequest(handler, "POST", "/posts", `{"body":"@failure_agent"}`, "failed-trigger", &alice, "192.0.2.211")
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("DB failure not safely mapped: %d %s", w.Code, w.Body.String())
	}
	var keys int
	if err := store.db.QueryRow(`SELECT count(*) FROM idempotency_keys WHERE key='failed-trigger'`).Scan(&keys); err != nil || keys != 0 {
		t.Fatalf("failed HTTP write committed key: %d %v", keys, err)
	}
	if len(socialJobs(t, store)) != 1 {
		t.Fatal("failed HTTP write committed jobs")
	}
}
