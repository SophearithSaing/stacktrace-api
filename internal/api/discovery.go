package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

type searchCursorPayload struct {
	Version  int    `json:"version"`
	Scope    string `json:"scope"`
	Viewer   string `json:"viewer"`
	Query    string `json:"query"`
	Ceiling  string `json:"ceiling"`
	Position string `json:"position"`
	ID       string `json:"id"`
}

type searchItemResponse struct {
	Post    postResponse `json:"post"`
	Snippet string       `json:"snippet"`
}

type searchPageResponse struct {
	Items      []searchItemResponse `json:"items"`
	NextCursor *string              `json:"next_cursor"`
}

// listSearchPosts handles bounded public post search requests.
func (s *server) listSearchPosts(w http.ResponseWriter, r *http.Request) {
	values, err := strictQuery(r.URL.RawQuery, "q", "limit", "cursor")
	if err != nil {
		writeFailure(w, err)
		return
	}
	query := app.SearchQuery{Text: values.Get("q")}
	query, err = query.Normalize()
	if err != nil {
		writeFailure(w, invalidQueryError())
		return
	}
	viewer, err := s.viewer(r)
	if err != nil {
		writeFailure(w, err)
		return
	}
	query.Window, err = s.searchWindow(values, viewer.ID, query.Text)
	if err != nil {
		writeFailure(w, err)
		return
	}
	page, err := s.store.SearchPosts(r.Context(), viewer.ID, query)
	if err != nil {
		writeFailure(w, err)
		return
	}
	items := make([]searchItemResponse, 0, len(page.Items))
	for _, item := range page.Items {
		post, err := s.postDTO(item.Post)
		if err != nil {
			writeFailure(w, err)
			return
		}
		items = append(items, searchItemResponse{Post: post, Snippet: item.Snippet})
	}
	next, err := s.encodeSearchCursor(viewer.ID, query.Text, page.Ceiling, page.NextPosition)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, searchPageResponse{Items: items, NextCursor: next})
}

// searchWindow parses a search pagination window.
func (s *server) searchWindow(values url.Values, viewer app.ID, text string) (app.SearchWindow, error) {
	window := app.SearchWindow{Limit: app.DefaultSearchLimit}
	if raw := values.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > app.MaxSearchLimit {
			return window, invalidQueryError()
		}
		window.Limit = limit
	}
	if token := values.Get("cursor"); token != "" {
		position, ceiling, err := s.decodeSearchCursor(token, viewer, text)
		if err != nil {
			return window, cursorError(err)
		}
		window.Position, window.InitialCeiling = &position, ceiling
	}
	return window, nil
}

// encodeSearchCursor signs a search continuation bound to viewer and query.
func (s *server) encodeSearchCursor(viewer app.ID, text string, ceiling time.Time, position *app.SearchPosition) (*string, error) {
	if position == nil {
		return nil, nil
	}
	hash := sha256.Sum256([]byte(text))
	payload := searchCursorPayload{1, "search-posts-newest", string(viewer), base64.RawURLEncoding.EncodeToString(hash[:]), ceiling.UTC().Format(time.RFC3339Nano), position.CreatedAt.UTC().Format(time.RFC3339Nano), string(position.ID)}
	contents, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, s.cursorSigningKey)
	_, _ = mac.Write(contents)
	token := base64.RawURLEncoding.EncodeToString(contents) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return &token, nil
}

// decodeSearchCursor verifies a canonical search continuation.
func (s *server) decodeSearchCursor(token string, viewer app.ID, text string) (app.SearchPosition, time.Time, error) {
	if token == "" || len(token) > maxCursorBytes || bytes.Count([]byte(token), []byte(".")) != 1 {
		return app.SearchPosition{}, time.Time{}, errInvalidCursor
	}
	parts := bytes.SplitN([]byte(token), []byte("."), 2)
	contents, err := base64.RawURLEncoding.DecodeString(string(parts[0]))
	if err != nil || base64.RawURLEncoding.EncodeToString(contents) != string(parts[0]) {
		return app.SearchPosition{}, time.Time{}, errInvalidCursor
	}
	signature, err := base64.RawURLEncoding.DecodeString(string(parts[1]))
	if err != nil || len(signature) != sha256.Size || base64.RawURLEncoding.EncodeToString(signature) != string(parts[1]) {
		return app.SearchPosition{}, time.Time{}, errInvalidCursor
	}
	mac := hmac.New(sha256.New, s.cursorSigningKey)
	_, _ = mac.Write(contents)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return app.SearchPosition{}, time.Time{}, errInvalidCursor
	}
	var payload searchCursorPayload
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil || decoder.Decode(new(any)) != io.EOF {
		return app.SearchPosition{}, time.Time{}, errInvalidCursor
	}
	canonical, err := json.Marshal(payload)
	if err != nil || !bytes.Equal(contents, canonical) {
		return app.SearchPosition{}, time.Time{}, errInvalidCursor
	}
	hash := sha256.Sum256([]byte(text))
	if payload.Version != 1 || payload.Scope != "search-posts-newest" || payload.Viewer != string(viewer) || payload.Query != base64.RawURLEncoding.EncodeToString(hash[:]) {
		return app.SearchPosition{}, time.Time{}, errInvalidCursor
	}
	ceiling, err := time.Parse(time.RFC3339Nano, payload.Ceiling)
	if err != nil || ceiling.IsZero() || payload.Ceiling != ceiling.UTC().Format(time.RFC3339Nano) {
		return app.SearchPosition{}, time.Time{}, errInvalidCursor
	}
	position, err := time.Parse(time.RFC3339Nano, payload.Position)
	if err != nil || position.IsZero() || position.After(ceiling) || payload.Position != position.UTC().Format(time.RFC3339Nano) {
		return app.SearchPosition{}, time.Time{}, errInvalidCursor
	}
	id, err := app.ParseID(payload.ID)
	if err != nil || id == "00000000-0000-0000-0000-000000000000" || payload.ID != string(id) {
		return app.SearchPosition{}, time.Time{}, errInvalidCursor
	}
	return app.SearchPosition{CreatedAt: position.UTC(), ID: id}, ceiling.UTC(), nil
}
