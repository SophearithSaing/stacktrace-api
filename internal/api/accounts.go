package api

import (
	"net/http"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

type accountSummary struct {
	ID            app.ID          `json:"id"`
	Type          app.AccountType `json:"type"`
	Handle        string          `json:"handle"`
	DisplayName   string          `json:"display_name"`
	Initials      string          `json:"initials"`
	StatusText    string          `json:"status_text"`
	AppearanceKey *string         `json:"appearance_key"`
	IsVerified    bool            `json:"is_verified"`
}

type accountProfile struct {
	accountSummary
	Bio            string         `json:"bio"`
	RoleLabel      string         `json:"role_label"`
	Specialty      string         `json:"specialty"`
	FollowerCount  int64          `json:"follower_count"`
	FollowingCount int64          `json:"following_count"`
	PostCount      int64          `json:"post_count"`
	Viewer         *accountViewer `json:"viewer"`
}

type accountViewer struct {
	Following bool `json:"following"`
}

func summarizeAccount(a app.Account) accountSummary {
	return accountSummary{a.ID, a.Type, a.Handle, a.DisplayName, a.Initials, a.StatusText, a.AppearanceKey, a.VerifiedAt != nil}
}

func writeProfile(w http.ResponseWriter, profile app.AccountProfile) {
	a := profile.Account
	response := accountProfile{
		accountSummary: summarizeAccount(a), Bio: a.Bio, RoleLabel: a.RoleLabel, Specialty: a.Specialty,
		FollowerCount: profile.FollowerCount, FollowingCount: profile.FollowingCount, PostCount: profile.PostCount,
	}
	if profile.ViewerFollowing != nil {
		response.Viewer = &accountViewer{Following: *profile.ViewerFollowing}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *server) accountByID(w http.ResponseWriter, r *http.Request) {
	id, err := app.ParseID(r.PathValue("accountID"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	viewer, err := s.viewer(r)
	if err != nil {
		writeFailure(w, err)
		return
	}
	profile, err := s.store.ProfileByID(r.Context(), id, viewer.ID)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeProfile(w, profile)
}

func (s *server) accountByHandle(w http.ResponseWriter, r *http.Request) {
	handle, err := app.NormalizeHandle(r.PathValue("handle"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	viewer, err := s.viewer(r)
	if err != nil {
		writeFailure(w, err)
		return
	}
	profile, err := s.store.ProfileByHandle(r.Context(), handle, viewer.ID)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeProfile(w, profile)
}

func (s *server) follow(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWrite(w, r); !ok {
		return
	}
	id, err := app.ParseID(r.PathValue("accountID"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	profile, err := s.store.SetFollow(r.Context(), app.SessionHash(s.sessionToken(r)), id, r.Method == http.MethodPut)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeProfile(w, profile)
}
