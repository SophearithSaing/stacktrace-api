package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/api"
	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

const testOrigin = "http://localhost:5173"
const testPassword = "  correct horse battery  "

type browserSession struct {
	Account *struct {
		ID     app.ID `json:"id"`
		Handle string `json:"handle"`
		Type   string `json:"type"`
	} `json:"account"`
	CSRFToken *string `json:"csrf_token"`
	cookie    *http.Cookie
}

func identityHandler(t *testing.T) (*Store, http.Handler) {
	t.Helper()
	store := testStore(t)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store, api.NewHandler(store, []string{testOrigin}, []byte(strings.Repeat("k", 32)), []byte(strings.Repeat("c", 32)), false, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

func identityRequest(handler http.Handler, method, path, body string, session *browserSession, ip string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api/v1"+path, strings.NewReader(body))
	r.RemoteAddr = ip + ":12345"
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Content-Type", "application/json")
	if session != nil {
		if session.cookie != nil {
			r.AddCookie(session.cookie)
		}
		if session.CSRFToken != nil {
			r.Header.Set("X-CSRF-Token", *session.CSRFToken)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func credentials(username, password string, register bool) string {
	input := map[string]string{"username": username, "password": password}
	if register {
		input["display_name"] = "Test Human"
	}
	body, _ := json.Marshal(input)
	return string(body)
}

func readSession(t *testing.T, w *httptest.ResponseRecorder, status int) browserSession {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body.String())
	}
	var session browserSession
	if err := json.Unmarshal(w.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == "stacktrace_session_dev" {
			session.cookie = cookie
		}
	}
	if strings.Contains(w.Body.String(), "password") || strings.Contains(w.Body.String(), "token_hash") || (session.cookie != nil && strings.Contains(w.Body.String(), session.cookie.Value)) {
		t.Fatal("private credential disclosed")
	}
	return session
}

func registerBrowser(t *testing.T, handler http.Handler, username, ip string) browserSession {
	t.Helper()
	session := readSession(t, identityRequest(handler, "POST", "/auth/register", credentials(username, testPassword, true), nil, ip), 201)
	if session.Account == nil || session.CSRFToken == nil || session.cookie == nil || session.Account.Type != "human" {
		t.Fatal("incomplete registration")
	}
	return session
}

func TestAuthenticationAndProfilesHTTP(t *testing.T) {
	store, handler := identityHandler(t)
	ctx := context.Background()
	for range 2 {
		if err := store.SeedDemo(ctx); err != nil {
			t.Fatal(err)
		}
	}
	anon := readSession(t, identityRequest(handler, "GET", "/me", "", nil, "192.0.2.1"), 200)
	if anon.Account != nil || anon.CSRFToken != nil {
		t.Fatal("anonymous /me disclosed session data")
	}
	alice := registerBrowser(t, handler, " Alice ", "192.0.2.1")
	bob := registerBrowser(t, handler, "bob", "192.0.2.2")
	if alice.Account.Handle != "alice" {
		t.Fatal("username not normalized")
	}
	for range 2 {
		me := readSession(t, identityRequest(handler, "GET", "/me", "", &alice, "192.0.2.1"), 200)
		if me.Account.ID != alice.Account.ID || *me.CSRFToken != *alice.CSRFToken || me.cookie != nil {
			t.Fatal("GET /me rotated or changed session/CSRF")
		}
	}
	var hash, tokenHash string
	if err := store.db.QueryRowContext(ctx, `SELECT c.password_hash, s.token_hash FROM password_credentials c JOIN sessions s ON s.account_id=c.account_id WHERE c.account_id=$1`, alice.Account.ID).Scan(&hash, &tokenHash); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$") || strings.Contains(hash, testPassword) || tokenHash != app.SessionHash(alice.cookie.Value) {
		t.Fatal("credentials are not hashed at rest")
	}
	for _, username := range []string{"ALICE", " GoLang "} {
		w := identityRequest(handler, "POST", "/auth/register", credentials(username, testPassword, true), nil, "192.0.2.3")
		if w.Code != 409 {
			t.Fatalf("collision accepted: %d %s", w.Code, w.Body.String())
		}
	}
	for _, badBody := range []string{`{"username":"agent_choice","password":"long-password","display_name":"Agent","type":"agent"}`, `{"username":"fake_owner","password":"long-password","display_name":"Owner","account_id":"` + string(bob.Account.ID) + `"}`} {
		if w := identityRequest(handler, "POST", "/auth/register", badBody, nil, "192.0.2.4"); w.Code != 400 {
			t.Fatalf("accepted privileged fields: %d", w.Code)
		}
	}
	path := "/accounts/" + string(bob.Account.ID) + "/follow"
	for range 2 {
		w := identityRequest(handler, "PUT", path, "", &alice, "192.0.2.1")
		var profile struct {
			FollowerCount int `json:"follower_count"`
			PostCount     int `json:"post_count"`
			Viewer        struct {
				Following bool `json:"following"`
			} `json:"viewer"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &profile); err != nil || w.Code != 200 || profile.FollowerCount != 1 || !profile.Viewer.Following || profile.PostCount != 0 {
			t.Fatalf("follow: %d %s", w.Code, w.Body.String())
		}
	}
	for _, lookup := range []string{"/accounts/" + string(bob.Account.ID), "/accounts/by-handle/BOB"} {
		w := identityRequest(handler, "GET", lookup, "", nil, "192.0.2.4")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"viewer":null`) || !strings.Contains(w.Body.String(), `"follower_count":1`) {
			t.Fatalf("anonymous profile: %s", w.Body.String())
		}
	}
	for _, tt := range []struct {
		name    string
		session *browserSession
		status  int
	}{
		{"no session", nil, 401},
		{"missing csrf", &browserSession{cookie: alice.cookie}, 403},
		{"other account csrf", &browserSession{cookie: alice.cookie, CSRFToken: bob.CSRFToken}, 403},
	} {
		if w := identityRequest(handler, "DELETE", path, "", tt.session, "192.0.2.4"); w.Code != tt.status {
			t.Fatalf("%s: %d", tt.name, w.Code)
		}
	}
	if w := identityRequest(handler, "PUT", "/accounts/"+string(alice.Account.ID)+"/follow", "", &alice, "192.0.2.1"); w.Code != 422 {
		t.Fatalf("self follow: %d", w.Code)
	}
	// Supplied actor IDs cannot change browser authority; bob cannot delete alice's follow.
	if w := identityRequest(handler, "DELETE", "/accounts/"+string(alice.Account.ID)+"/follow", `{"follower_id":"`+string(alice.Account.ID)+`"}`, &bob, "192.0.2.2"); w.Code != 200 {
		t.Fatalf("bob no-op unfollow: %d", w.Code)
	}
	profile, err := store.ProfileByID(ctx, bob.Account.ID, alice.Account.ID)
	if err != nil || profile.FollowerCount != 1 || profile.ViewerFollowing == nil || !*profile.ViewerFollowing {
		t.Fatal("cross-account request changed alice's follow")
	}
	for range 2 {
		if w := identityRequest(handler, "DELETE", path, "", &alice, "192.0.2.1"); w.Code != 200 || !strings.Contains(w.Body.String(), `"follower_count":0`) {
			t.Fatalf("unfollow: %s", w.Body.String())
		}
	}
	rotated := readSession(t, identityRequest(handler, "POST", "/auth/login", credentials(" ALICE ", testPassword, false), &alice, "192.0.2.1"), 200)
	if rotated.cookie == nil || rotated.cookie.Value == alice.cookie.Value || *rotated.CSRFToken == *alice.CSRFToken {
		t.Fatal("login did not rotate session")
	}
	old := readSession(t, identityRequest(handler, "GET", "/me", "", &alice, "192.0.2.1"), 200)
	if old.Account != nil {
		t.Fatal("old session remained valid")
	}
	if w := identityRequest(handler, "POST", "/auth/logout", "", &rotated, "192.0.2.1"); w.Code != 204 || len(w.Result().Cookies()) != 1 || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatalf("logout: %d", w.Code)
	}
	if me := readSession(t, identityRequest(handler, "GET", "/me", "", &rotated, "192.0.2.1"), 200); me.Account != nil {
		t.Fatal("logout did not revoke session")
	}
	// Registration also revokes the presented session, even when switching identity.
	carol := readSession(t, identityRequest(handler, "POST", "/auth/register", credentials("carol", testPassword, true), &bob, "192.0.2.2"), 201)
	if carol.Account.ID == bob.Account.ID {
		t.Fatal("registration claimed another account")
	}
	if me := readSession(t, identityRequest(handler, "GET", "/me", "", &bob, "192.0.2.2"), 200); me.Account != nil {
		t.Fatal("registration did not rotate session")
	}
	for _, path := range []string{"/accounts/" + string(app.NewID()), "/accounts/by-handle/unknown"} {
		if w := identityRequest(handler, "GET", path, "", nil, "192.0.2.5"); w.Code != 404 {
			t.Fatalf("unknown profile: %d", w.Code)
		}
	}
	if w := identityRequest(handler, "GET", "/accounts/not-a-uuid", "", nil, "192.0.2.5"); w.Code != 400 {
		t.Fatalf("malformed id: %d", w.Code)
	}
	seedProfile, err := store.ProfileByHandle(ctx, "golang", "")
	if err != nil || seedProfile.FollowerCount != 1 || seedProfile.FollowingCount != 1 || seedProfile.Account.Type != app.AccountAgent {
		t.Fatal("demo seed is not repeatable or counts are wrong")
	}
}

func TestGenericLoginFailuresAndSessionInvalidationHTTP(t *testing.T) {
	store, handler := identityHandler(t)
	disabled := registerBrowser(t, handler, "disabled", "192.0.2.1")
	active := registerBrowser(t, handler, "active", "192.0.2.2")
	if _, err := store.db.Exec(`UPDATE accounts SET disabled_at=now() WHERE id=$1`, disabled.Account.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SeedDemo(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ username, password string }{
		{"active", "incorrect-password"}, {"missing", testPassword}, {"golang", testPassword}, {"disabled", testPassword}, {"bad!", testPassword},
		{"active", strings.TrimSpace(testPassword)}, {"active", "short"}, {"active", strings.Repeat("a", 1025)},
	} {
		w := identityRequest(handler, "POST", "/auth/login", credentials(tt.username, tt.password, false), nil, "192.0.2.3")
		var response struct {
			Error struct{ Code, Message string }
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != 401 || response.Error.Code != "invalid_credentials" || response.Error.Message != "Invalid username or password" {
			t.Fatalf("non-generic login error: %d %s", w.Code, w.Body.String())
		}
		if len(w.Result().Cookies()) != 0 {
			t.Fatal("invalid login set a cookie")
		}
	}
	if me := readSession(t, identityRequest(handler, "GET", "/me", "", &disabled, "192.0.2.1"), 200); me.Account != nil {
		t.Fatal("disabled session valid")
	}
	if w := identityRequest(handler, "GET", "/accounts/"+string(disabled.Account.ID), "", nil, "192.0.2.1"); w.Code != 404 {
		t.Fatal("disabled profile visible")
	}
	if _, err := store.db.Exec(`UPDATE sessions SET created_at=now()-interval '8 days', expires_at=now()-interval '1 day' WHERE account_id=$1`, active.Account.ID); err != nil {
		t.Fatal(err)
	}
	if me := readSession(t, identityRequest(handler, "GET", "/me", "", &active, "192.0.2.2"), 200); me.Account != nil {
		t.Fatal("expired session valid")
	}
	if w := identityRequest(handler, "PUT", "/accounts/"+string(active.Account.ID)+"/follow", "", &disabled, "192.0.2.1"); w.Code != 401 {
		t.Fatal("disabled account could write")
	}
}

func TestConcurrentRegistrationAndFollowsHTTP(t *testing.T) {
	store, handler := identityHandler(t)
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 2)
	var workers sync.WaitGroup
	for i := range 2 {
		workers.Go(func() {
			<-start
			results <- identityRequest(handler, "POST", "/auth/register", credentials(" SAME_USER ", testPassword, true), nil, fmt.Sprintf("192.0.2.%d", i+1))
		})
	}
	close(start)
	workers.Wait()
	close(results)
	var winner browserSession
	statuses := map[int]int{}
	for w := range results {
		statuses[w.Code]++
		if w.Code == 201 {
			winner = readSession(t, w, 201)
		}
	}
	if statuses[201] != 1 || statuses[409] != 1 {
		t.Fatalf("registration race: %v", statuses)
	}
	for _, table := range []string{"accounts", "password_credentials", "sessions"} {
		var count int
		if err := store.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("partial registration in %s: %d %v", table, count, err)
		}
	}
	target := registerBrowser(t, handler, "target", "192.0.2.3")
	for i := range 8 {
		workers.Go(func() {
			w := identityRequest(handler, "PUT", "/accounts/"+string(target.Account.ID)+"/follow", "", &winner, fmt.Sprintf("198.51.100.%d", i+1))
			if w.Code != 200 {
				t.Errorf("concurrent follow: %d %s", w.Code, w.Body.String())
			}
		})
	}
	workers.Wait()
	profile, err := store.ProfileByID(context.Background(), target.Account.ID, winner.Account.ID)
	if err != nil || profile.FollowerCount != 1 {
		t.Fatal("duplicate follows after concurrent writes")
	}
	var created time.Time
	if err := store.db.QueryRow(`SELECT created_at FROM follows WHERE follower_id=$1 AND followed_id=$2`, winner.Account.ID, target.Account.ID).Scan(&created); err != nil {
		t.Fatal(err)
	}
	identityRequest(handler, "PUT", "/accounts/"+string(target.Account.ID)+"/follow", "", &winner, "192.0.2.1")
	var repeated time.Time
	if err := store.db.QueryRow(`SELECT created_at FROM follows WHERE follower_id=$1 AND followed_id=$2`, winner.Account.ID, target.Account.ID).Scan(&repeated); err != nil || !created.Equal(repeated) {
		t.Fatal("no-op follow changed relationship time")
	}
}

func TestPasswordBoundariesAndLoginLimitsHTTP(t *testing.T) {
	_, handler := identityHandler(t)
	for i, size := range []int{11, 12, 1024, 1025} {
		w := identityRequest(handler, "POST", "/auth/register", credentials(fmt.Sprintf("boundary_%d", size), strings.Repeat("a", size), true), nil, fmt.Sprintf("192.0.2.%d", i+1))
		want := 201
		if size == 11 || size == 1025 {
			want = 422
		}
		if w.Code != want {
			t.Fatalf("%d-byte password: %d %s", size, w.Code, w.Body.String())
		}
	}
	// Short invalid passwords keep these quota tests fast; limits apply before hashing.
	for i := range 11 {
		username := "NO_SUCH_USER"
		if i%2 == 0 {
			username = " no_such_user "
		}
		w := identityRequest(handler, "POST", "/auth/login", credentials(username, "short", false), nil, fmt.Sprintf("198.51.100.%d", i+1))
		want := 401
		if i == 10 {
			want = 429
		}
		if w.Code != want || (want == 429 && w.Header().Get("Retry-After") == "") {
			t.Fatalf("normalized username quota at %d: %d", i, w.Code)
		}
	}
	for i := range 21 {
		w := identityRequest(handler, "POST", "/auth/login", credentials(fmt.Sprintf("ip_user_%d", i), "short", false), nil, "203.0.113.1")
		want := 401
		if i == 20 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("IP quota at %d: %d", i, w.Code)
		}
	}
	actor := registerBrowser(t, handler, "rate_actor", "192.0.2.50")
	target := registerBrowser(t, handler, "rate_target", "192.0.2.51")
	for i := range 61 {
		w := identityRequest(handler, "DELETE", "/accounts/"+string(target.Account.ID)+"/follow", "", &actor, fmt.Sprintf("198.51.100.%d", i+20))
		want := 200
		if i == 60 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("account quota at %d: %d", i, w.Code)
		}
	}
}
