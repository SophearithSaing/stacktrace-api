package api

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

type feedEntryResponse struct {
	ID         string          `json:"id"`
	Kind       app.FeedKind    `json:"kind"`
	OccurredAt string          `json:"occurred_at"`
	Reposter   *accountSummary `json:"reposter"`
	Post       postResponse    `json:"post"`
}

func (s *server) listFeed(w http.ResponseWriter, r *http.Request) {
	values, err := strictQuery(r.URL.RawQuery, "view", "sort", "tag", "limit", "cursor")
	if err != nil {
		writeFailure(w, err)
		return
	}
	query, err := (app.FeedQuery{View: app.FeedView(values.Get("view")), Sort: app.FeedSort(values.Get("sort")), Tag: values.Get("tag")}).Normalize()
	if err != nil {
		writeFailure(w, invalidQueryError())
		return
	}
	viewer, err := s.viewer(r)
	if err != nil {
		writeFailure(w, err)
		return
	}
	if query.View == app.FeedViewFollowing && viewer.ID == "" {
		writeFailure(w, app.ErrUnauthenticated)
		return
	}
	binding := feedCursorBinding{Scope: "feed", Viewer: viewer.ID, View: query.View, Tag: query.Tag, Sort: query.Sort}
	query.Window, err = s.feedWindow(values, binding)
	if err != nil {
		writeFailure(w, err)
		return
	}
	page, err := s.store.ListFeed(r.Context(), viewer.ID, app.SessionHash(s.sessionToken(r)), query)
	if err != nil {
		writeFailure(w, err)
		return
	}
	s.writeFeedResponse(w, page, binding)
}

func (s *server) listAccountFeed(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("resource") != "feed" {
		s.notFound(w, r)
		return
	}
	accountID, err := app.ParseID(r.PathValue("accountID"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	values, err := strictQuery(r.URL.RawQuery, "limit", "cursor")
	if err != nil {
		writeFailure(w, err)
		return
	}
	viewer, err := s.viewer(r)
	if err != nil {
		writeFailure(w, err)
		return
	}
	binding := feedCursorBinding{Scope: "account-feed", Target: accountID, Viewer: viewer.ID, Sort: app.FeedSortNewest}
	window, err := s.feedWindow(values, binding)
	if err != nil {
		writeFailure(w, err)
		return
	}
	page, err := s.store.ListAccountFeed(r.Context(), accountID, viewer.ID, window)
	if err != nil {
		writeFailure(w, err)
		return
	}
	s.writeFeedResponse(w, page, binding)
}

func (s *server) feedWindow(values url.Values, binding feedCursorBinding) (app.FeedWindow, error) {
	window := app.FeedWindow{Limit: app.DefaultReadLimit}
	if raw := values.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > app.MaxReadLimit {
			return window, invalidQueryError()
		}
		window.Limit = limit
	}
	if token := values.Get("cursor"); token != "" {
		position, ceiling, err := s.decodeFeedCursor(token, binding)
		if err != nil {
			return window, cursorError(err)
		}
		window.Position, window.InitialCeiling = &position, ceiling
	}
	return window, nil
}

func (s *server) writeFeedResponse(w http.ResponseWriter, page app.FeedPage, binding feedCursorBinding) {
	items := make([]feedEntryResponse, 0, len(page.Items))
	for _, entry := range page.Items {
		post, err := s.postDTO(entry.Post)
		if err != nil {
			writeFailure(w, err)
			return
		}
		item := feedEntryResponse{ID: string(entry.Kind) + ":" + string(entry.ID), Kind: entry.Kind, OccurredAt: formatTimestamp(entry.OccurredAt), Post: post}
		if entry.Reposter != nil {
			reposter := summarizeAccount(*entry.Reposter)
			item.Reposter = &reposter
		}
		items = append(items, item)
	}
	next, err := s.encodeFeedCursor(binding, page.Ceiling, page.NextPosition)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Items      []feedEntryResponse `json:"items"`
		NextCursor *string             `json:"next_cursor"`
	}{items, next})
}
