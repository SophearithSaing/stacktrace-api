package postgres

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestSuggestedAgentsExclusionsCountsAndPause(t *testing.T) {
	store := feedTestStore(t)
	ctx := context.Background()
	viewer, _ := contentTestActor(t, store, "suggested_viewer")
	followed := socialAgent(t, store, "suggested_followed", nil)
	popular := socialAgent(t, store, "suggested_popular", nil)
	paused := socialAgent(t, store, "suggested_paused", nil)
	disabled := socialAgent(t, store, "suggested_disabled", nil)
	feedExec(t, store, `UPDATE agent_settings SET enabled=false WHERE agent_id=$1`, paused.AgentID)
	feedExec(t, store, `UPDATE accounts SET disabled_at=statement_timestamp() WHERE id=$1`, disabled.AgentID)
	feedExec(t, store, `INSERT INTO follows(follower_id,followed_id,created_at) VALUES($1,$2,statement_timestamp())`, viewer, followed.AgentID)
	for index := range 2 {
		follower, _ := contentTestActor(t, store, "suggested_follower_"+string(rune('a'+index)))
		feedExec(t, store, `INSERT INTO follows(follower_id,followed_id,created_at) VALUES($1,$2,statement_timestamp())`, follower, popular.AgentID)
	}
	socialPost(t, store, popular.AgentID, "popular post")
	profiles, err := store.SuggestedAgents(ctx, viewer, app.SuggestionQuery{Limit: 20})
	if err != nil || len(profiles) != 2 || profiles[0].Account.ID != popular.AgentID || profiles[0].FollowerCount != 2 || profiles[0].PostCount != 1 || profiles[0].ViewerFollowing == nil || *profiles[0].ViewerFollowing || profiles[1].Account.ID != paused.AgentID {
		t.Fatalf("viewer suggestions=%#v, %v", profiles, err)
	}
	anonymous, err := store.SuggestedAgents(ctx, "", app.SuggestionQuery{Limit: 20})
	if err != nil || len(anonymous) != 3 || anonymous[0].ViewerFollowing != nil {
		t.Fatalf("anonymous suggestions=%#v, %v", anonymous, err)
	}
}

func TestDiscoverySuggestionAndTrendHTTPContracts(t *testing.T) {
	store, handler := identityHandler(t)
	viewer := registerBrowser(t, handler, "discovery_http_viewer", "192.0.2.220")
	agent := socialAgent(t, store, "discovery_http_agent", nil)
	if response := contentRequest(handler, "GET", "/agents/suggested?limit=1", "", "", &viewer, "192.0.2.220"); response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "private, no-store" || !strings.Contains(response.Body.String(), string(agent.AgentID)) || !strings.Contains(response.Body.String(), `"following":false`) {
		t.Fatalf("suggestions response=%d %s", response.Code, response.Body.String())
	}
	for _, path := range []string{"/agents/suggested?limit=0", "/agents/suggested?limit=21", "/agents/suggested?limit=1&limit=2", "/agents/suggested?unknown=1", "/trends?limit=0", "/trends?limit=7"} {
		feedHTTPError(t, handler, path, nil, http.StatusBadRequest, "invalid_query")
	}
	_, session := contentTestActor(t, store, "discovery_http_trend_author")
	post := feedPost(t, store, session, "discovery-http-trend", "#httptrend")
	feedExec(t, store, `UPDATE posts SET created_at=statement_timestamp()-interval '1 hour' WHERE id=$1`, post.ID)
	response := contentRequest(handler, "GET", "/trends", "", "", nil, "192.0.2.220")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("trends response=%d %s", response.Code, response.Body.String())
	}
	var page struct {
		Items []struct {
			Slug    string   `json:"slug"`
			Change  *float64 `json:"change_percent"`
			Current int64    `json:"post_count"`
		} `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || len(page.Items) != 1 || page.Items[0].Slug != "httptrend" || page.Items[0].Current != 1 || page.Items[0].Change != nil {
		t.Fatalf("trend body=%s err=%v", response.Body.String(), err)
	}
}

func TestTrendsWindowsAndOrdering(t *testing.T) {
	store := feedTestStore(t)
	_, session := contentTestActor(t, store, "trend_author")
	currentRising := feedPost(t, store, session, "trend-rising-current", "#rising")
	currentRisingTwo := feedPost(t, store, session, "trend-rising-current-two", "#rising")
	previousRising := feedPost(t, store, session, "trend-rising-previous", "#rising")
	currentFalling := feedPost(t, store, session, "trend-falling-current", "#falling")
	previousFalling := feedPost(t, store, session, "trend-falling-previous", "#falling")
	previousFallingTwo := feedPost(t, store, session, "trend-falling-previous-two", "#falling")
	currentNew := feedPost(t, store, session, "trend-new-current", "#newtag")
	quotedSource := feedPost(t, store, session, "trend-quoted-source", "#sourceonly")
	quote := feedPost(t, store, session, "trend-quote-current", "#quoteown")
	deleted := feedPost(t, store, session, "trend-deleted", "#deletedtag")
	feedExec(t, store, `UPDATE posts SET quoted_post_id=$1 WHERE id=$2`, quotedSource.ID, quote.ID)
	if _, err := store.SetRepost(context.Background(), session, quote.ID, true); err != nil {
		t.Fatal(err)
	}
	feedExec(t, store, `UPDATE posts SET created_at=statement_timestamp()-interval '1 hour' WHERE id IN ($1,$2,$3,$4,$5)`, currentRising.ID, currentRisingTwo.ID, currentFalling.ID, currentNew.ID, quote.ID)
	feedExec(t, store, `UPDATE posts SET created_at=statement_timestamp()-interval '25 hours' WHERE id IN ($1,$2,$3,$4)`, previousRising.ID, previousFalling.ID, previousFallingTwo.ID, quotedSource.ID)
	feedExec(t, store, `UPDATE posts SET deleted_at=statement_timestamp() WHERE id=$1`, deleted.ID)
	trends, err := store.Trends(context.Background(), app.TrendQuery{Limit: 6})
	if err != nil || len(trends) != 4 || trends[0].Slug != "rising" || trends[0].PostCount != 2 || trends[0].PreviousPostCount != 1 || trends[0].ChangePercent == nil || *trends[0].ChangePercent != 100 || trends[1].Slug != "falling" || trends[1].PostCount != 1 || trends[1].PreviousPostCount != 2 || trends[1].ChangePercent == nil || *trends[1].ChangePercent != -50 || trends[2].Slug != "newtag" || trends[2].ChangePercent != nil || trends[3].Slug != "quoteown" || trends[3].PostCount != 1 || trends[3].PreviousPostCount != 0 || trends[3].ChangePercent != nil {
		t.Fatalf("trends=%#v, %v", trends, err)
	}
}
