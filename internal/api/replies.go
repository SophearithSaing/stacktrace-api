package api

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

type codeRequestDTO struct {
	Language string `json:"language"`
	Filename string `json:"filename"`
	Source   string `json:"source"`
}

func (s *server) createReply(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWrite(w, r); !ok {
		return
	}
	key, err := idempotencyKey(r)
	if err != nil {
		writeFailure(w, err)
		return
	}
	postID, err := app.ParseID(r.PathValue("postID"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	var input struct {
		Body string `json:"body"`
	}
	if err = decodeJSON(r, &input); err != nil {
		writeFailure(w, err)
		return
	}
	creation, err := app.NewReplyCreation(postID, input.Body)
	if err != nil {
		writeFailure(w, err)
		return
	}
	result, err := s.store.CreateReply(r.Context(), app.SessionHash(s.sessionToken(r)), key, creation)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		Reply      replyResponse `json:"reply"`
		ReplyTotal int64         `json:"reply_total"`
	}{replyDTO(result.Reply), result.ReplyTotal})
}

func (s *server) deleteReply(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWrite(w, r); !ok {
		return
	}
	id, err := app.ParseID(r.PathValue("replyID"))
	if err == nil {
		err = s.store.DeleteReply(r.Context(), app.SessionHash(s.sessionToken(r)), id)
	}
	if err != nil {
		writeFailure(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) listReplies(w http.ResponseWriter, r *http.Request) {
	postID, err := app.ParseID(r.PathValue("postID"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	values, err := strictQuery(r.URL.RawQuery, "sort", "limit", "cursor")
	if err != nil {
		writeFailure(w, err)
		return
	}
	sort := app.ReplySortOldest
	if raw := values.Get("sort"); raw != "" {
		sort = app.ReplySort(raw)
		if sort != app.ReplySortOldest && sort != app.ReplySortNewest {
			writeFailure(w, invalidQueryError())
			return
		}
	}
	window, err := s.readWindow(values, "replies", postID, "", sort)
	if err != nil {
		writeFailure(w, err)
		return
	}
	// Although this projection is viewer-independent, do not turn an
	// authenticated lookup outage into an anonymous success.
	if _, err = s.viewer(r); err != nil {
		writeFailure(w, err)
		return
	}
	page, err := s.store.ListReplies(r.Context(), postID, window)
	if err != nil {
		writeFailure(w, err)
		return
	}
	items := make([]replyResponse, 0, len(page.Items))
	for _, reply := range page.Items {
		items = append(items, replyDTO(reply))
	}
	next, err := s.encodeCursor("replies", postID, "", sort, page.Ceiling, page.NextPosition)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Items      []replyResponse `json:"items"`
		TotalCount int64           `json:"total_count"`
		NextCursor *string         `json:"next_cursor"`
	}{items, page.ReplyTotal, next})
}

func (s *server) listBookmarks(w http.ResponseWriter, r *http.Request) {
	viewer, err := s.viewer(r)
	if err != nil {
		writeFailure(w, err)
		return
	}
	if viewer.ID == "" {
		writeFailure(w, app.ErrUnauthenticated)
		return
	}
	values, err := strictQuery(r.URL.RawQuery, "limit", "cursor")
	if err != nil {
		writeFailure(w, err)
		return
	}
	window, err := s.readWindow(values, "bookmarks", "", viewer.ID, app.ReplySortNewest)
	if err != nil {
		writeFailure(w, err)
		return
	}
	page, err := s.store.ListBookmarks(r.Context(), app.SessionHash(s.sessionToken(r)), window)
	if err != nil {
		writeFailure(w, err)
		return
	}
	items := make([]postResponse, 0, len(page.Items))
	for _, post := range page.Items {
		item, err := s.postDTO(post)
		if err != nil {
			writeFailure(w, err)
			return
		}
		items = append(items, item)
	}
	next, err := s.encodeCursor("bookmarks", "", viewer.ID, app.ReplySortNewest, page.Ceiling, page.NextPosition)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Items      []postResponse `json:"items"`
		NextCursor *string        `json:"next_cursor"`
	}{items, next})
}

func strictQuery(raw string, allowed ...string) (url.Values, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil, invalidQueryError()
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = true
	}
	for key, entries := range values {
		if !allowedSet[key] || len(entries) != 1 || entries[0] == "" {
			return nil, invalidQueryError()
		}
	}
	return values, nil
}

func (s *server) readWindow(values url.Values, kind string, target, viewer app.ID, sort app.ReplySort) (app.ReadWindow, error) {
	limit := app.DefaultReadLimit
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > app.MaxReadLimit {
			return app.ReadWindow{}, invalidQueryError()
		}
		limit = parsed
	}
	window := app.ReadWindow{Sort: sort, Limit: limit}
	if raw := values.Get("cursor"); raw != "" {
		position, ceiling, err := s.decodeCursor(raw, kind, target, viewer, sort)
		if err != nil {
			return app.ReadWindow{}, cursorError(err)
		}
		window.Position, window.InitialCeiling = &position, ceiling
	}
	return window, nil
}
