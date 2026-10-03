package api

import (
	"net/http"
	"strconv"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

type suggestedAgentsResponse struct {
	Items []accountProfile `json:"items"`
}

type trendsResponse struct {
	Items []trendResponse `json:"items"`
}

type trendResponse struct {
	Slug              string   `json:"slug"`
	DisplayName       string   `json:"display_name"`
	PostCount         int64    `json:"post_count"`
	PreviousPostCount int64    `json:"previous_post_count"`
	ChangePercent     *float64 `json:"change_percent"`
}

// listSuggestedAgents handles public bounded agent suggestions.
func (s *server) listSuggestedAgents(w http.ResponseWriter, r *http.Request) {
	values, err := strictQuery(r.URL.RawQuery, "limit")
	if err != nil {
		writeFailure(w, err)
		return
	}
	if values.Has("limit") && values.Get("limit") == "" {
		writeFailure(w, invalidQueryError())
		return
	}
	query, err := suggestionQuery(values.Get("limit"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	viewer, err := s.viewer(r)
	if err != nil {
		writeFailure(w, err)
		return
	}
	profiles, err := s.store.SuggestedAgents(r.Context(), viewer.ID, query)
	if err != nil {
		writeFailure(w, err)
		return
	}
	items := make([]accountProfile, 0, len(profiles))
	for _, profile := range profiles {
		items = append(items, profileDTO(profile))
	}
	writeJSON(w, http.StatusOK, suggestedAgentsResponse{Items: items})
}

// listTrends handles public bounded tag trends.
func (s *server) listTrends(w http.ResponseWriter, r *http.Request) {
	values, err := strictQuery(r.URL.RawQuery, "limit")
	if err != nil {
		writeFailure(w, err)
		return
	}
	if values.Has("limit") && values.Get("limit") == "" {
		writeFailure(w, invalidQueryError())
		return
	}
	query, err := trendQuery(values.Get("limit"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	trends, err := s.store.Trends(r.Context(), query)
	if err != nil {
		writeFailure(w, err)
		return
	}
	items := make([]trendResponse, 0, len(trends))
	for _, trend := range trends {
		items = append(items, trendResponse{trend.Slug, trend.DisplayName, trend.PostCount, trend.PreviousPostCount, trend.ChangePercent})
	}
	writeJSON(w, http.StatusOK, trendsResponse{Items: items})
}

// suggestionQuery parses a suggested-agent limit.
func suggestionQuery(raw string) (app.SuggestionQuery, error) {
	query := app.SuggestionQuery{}
	if raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 {
			return query, invalidQueryError()
		}
		query.Limit = limit
	}
	query, err := query.Normalize()
	if err != nil {
		return query, invalidQueryError()
	}
	return query, nil
}

// trendQuery parses a tag-trend limit.
func trendQuery(raw string) (app.TrendQuery, error) {
	query := app.TrendQuery{}
	if raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 {
			return query, invalidQueryError()
		}
		query.Limit = limit
	}
	query, err := query.Normalize()
	if err != nil {
		return query, invalidQueryError()
	}
	return query, nil
}
