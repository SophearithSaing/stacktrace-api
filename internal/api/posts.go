package api

import (
	"net/http"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func (s *server) createPost(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWrite(w, r); !ok {
		return
	}
	key, err := idempotencyKey(r)
	if err != nil {
		writeFailure(w, err)
		return
	}
	var input struct {
		Body         string          `json:"body"`
		QuotedPostID *app.ID         `json:"quoted_post_id"`
		Code         *codeRequestDTO `json:"code"`
	}
	if err = decodeJSON(r, &input); err != nil {
		writeFailure(w, err)
		return
	}
	var code *app.Code
	if input.Code != nil {
		code = &app.Code{Language: input.Code.Language, Filename: input.Code.Filename, Source: input.Code.Source}
	}
	creation, err := app.NewPostCreation(input.Body, input.QuotedPostID, code)
	if err != nil {
		writeFailure(w, err)
		return
	}
	post, err := s.store.CreatePost(r.Context(), app.SessionHash(s.sessionToken(r)), key, creation)
	if err != nil {
		writeFailure(w, err)
		return
	}
	response, err := s.postDTO(post)
	if err != nil {
		writeFailure(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/posts/"+string(post.ID))
	writeJSON(w, http.StatusCreated, response)
}

func (s *server) getPost(w http.ResponseWriter, r *http.Request) {
	id, err := app.ParseID(r.PathValue("postID"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	viewer, err := s.viewer(r)
	if err != nil {
		writeFailure(w, err)
		return
	}
	post, err := s.store.PostByID(r.Context(), id, viewer.ID)
	if err != nil {
		writeFailure(w, err)
		return
	}
	s.writePostResponse(w, http.StatusOK, post)
}

func (s *server) deletePost(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWrite(w, r); !ok {
		return
	}
	id, err := app.ParseID(r.PathValue("postID"))
	if err == nil {
		err = s.store.DeletePost(r.Context(), app.SessionHash(s.sessionToken(r)), id)
	}
	if err != nil {
		writeFailure(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) reaction(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWrite(w, r); !ok {
		return
	}
	id, err := app.ParseID(r.PathValue("postID"))
	var kind *app.ReactionKind
	if err == nil && r.Method == http.MethodPut {
		var input struct {
			Kind string `json:"kind"`
		}
		if err = decodeJSON(r, &input); err == nil {
			parsed, parseErr := app.ParseReactionKind(input.Kind)
			err, kind = parseErr, &parsed
		}
	}
	if err != nil {
		writeFailure(w, err)
		return
	}
	post, err := s.store.SetReaction(r.Context(), app.SessionHash(s.sessionToken(r)), id, kind)
	if err != nil {
		writeFailure(w, err)
		return
	}
	s.writePostResponse(w, http.StatusOK, post)
}

func (s *server) repost(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWrite(w, r); !ok {
		return
	}
	id, err := app.ParseID(r.PathValue("postID"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	post, err := s.store.SetRepost(r.Context(), app.SessionHash(s.sessionToken(r)), id, r.Method == http.MethodPut)
	if err != nil {
		writeFailure(w, err)
		return
	}
	postDTO, err := s.postDTO(post)
	if err != nil {
		writeFailure(w, err)
		return
	}
	var entryID, occurredAt *string
	if post.Viewer != nil && post.Viewer.ViewerRepost != nil {
		id := "repost:" + string(post.Viewer.ViewerRepost.ID)
		at := formatTimestamp(post.Viewer.ViewerRepost.CreatedAt)
		entryID, occurredAt = &id, &at
	}
	writeJSON(w, http.StatusOK, struct {
		Post             postResponse `json:"post"`
		RepostEntryID    *string      `json:"repost_entry_id"`
		RepostOccurredAt *string      `json:"repost_occurred_at"`
	}{postDTO, entryID, occurredAt})
}

func (s *server) bookmark(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWrite(w, r); !ok {
		return
	}
	id, err := app.ParseID(r.PathValue("postID"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	post, err := s.store.SetBookmark(r.Context(), app.SessionHash(s.sessionToken(r)), id, r.Method == http.MethodPut)
	if err != nil {
		writeFailure(w, err)
		return
	}
	s.writePostResponse(w, http.StatusOK, post)
}

func (s *server) writePostResponse(w http.ResponseWriter, status int, post app.Post) {
	response, err := s.postDTO(post)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, status, response)
}

func idempotencyKey(r *http.Request) (string, error) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) != 1 || app.ValidateIdempotencyKey(values[0]) != nil {
		return "", app.ErrInvalidIdempotencyKey
	}
	return values[0], nil
}
