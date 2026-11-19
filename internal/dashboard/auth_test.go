package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MacLeodTech/runner-scaling/internal/api"
)

func TestMain(m *testing.M) {
	pbkdf2Iter = 1000 // the real cost is for production; it makes the suite take minutes under -race
	os.Exit(m.Run())
}

type noSlots struct{}

func (noSlots) SlotStats(context.Context) ([]api.Slot, error) { return nil, nil }

// client is a browser: it keeps cookies and sends JSON.
type client struct {
	t   *testing.T
	c   *http.Client
	url string
}

func newServer(t *testing.T, token string) (*client, *Accounts, string) {
	return newServerWith(t, token, nil)
}

// newServerWith is newServer with a hook to adjust the Auth (for example to attach an audit log) before serving.
func newServerWith(t *testing.T, token string, tweak func(*Auth)) (*client, *Accounts, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "users.json")
	accts, fresh, err := OpenAccounts(path)
	if err != nil || !fresh {
		t.Fatalf("open: %v fresh=%v", err, fresh)
	}
	auth := NewAuth(accts, nil)
	if tweak != nil {
		tweak(auth)
	}
	srv := &Server{Name: "t", Tracker: NewTracker(), Guard: noSlots{}, Token: token, Auth: auth}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	return &client{t, &http.Client{Jar: jar}, ts.URL}, accts, path
}

func (c *client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req, _ := http.NewRequest(method, c.url+path, rd)
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.c.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (c *client) login(u, p string) int {
	code, _ := c.do("POST", "/api/login", map[string]string{"username": u, "password": p})
	return code
}

func TestDefaultAdminMustChangePasswordBeforeAnything(t *testing.T) {
	c, _, _ := newServer(t, "tok")
	if code, _ := c.do("GET", "/api/state", nil); code != 401 {
		t.Fatalf("state without login: %d", code)
	}
	if c.login("admin", "wrong") != 401 {
		t.Fatal("wrong password accepted")
	}
	if c.login("admin", "admin") != 200 {
		t.Fatal("admin:admin refused")
	}
	if _, me := c.do("GET", "/api/me", nil); me["must_change"] != true {
		t.Fatalf("me: %v", me)
	}
	for _, p := range []string{"/api/state", "/api/users", "/metrics"} {
		if code, _ := c.do("GET", p, nil); code != 401 && code != 403 {
			t.Errorf("%s reachable before the password change: %d", p, code)
		}
	}
	if code, _ := c.do("POST", "/api/password", map[string]string{"old": "admin", "new": "admin"}); code != 400 {
		t.Errorf("reusing admin: %d", code)
	}
	if code, _ := c.do("POST", "/api/password", map[string]string{"old": "admin", "new": "short"}); code != 400 {
		t.Errorf("short password: %d", code)
	}
	if code, _ := c.do("POST", "/api/password", map[string]string{"old": "bad", "new": "a-new-passw0rd"}); code != 403 {
		t.Errorf("wrong old password: %d", code)
	}
	if code, _ := c.do("POST", "/api/password", map[string]string{"old": "admin", "new": "a-new-passw0rd"}); code != 200 {
		t.Fatalf("change: %d", code)
	}
	if code, _ := c.do("GET", "/api/state", nil); code != 200 {
		t.Errorf("state after change: %d", code)
	}
}

func TestAdminAddsUserWhoMustChangeTheGeneratedPassword(t *testing.T) {
	c, accts, path := newServer(t, "tok")
	c.login("admin", "admin")
	c.do("POST", "/api/password", map[string]string{"old": "admin", "new": "a-new-passw0rd"})

	code, out := c.do("POST", "/api/users", map[string]string{"username": "Bob"})
	pw, _ := out["password"].(string)
	if code != 200 || len(pw) != generatedLen || out["username"] != "bob" {
		t.Fatalf("add: %d %v", code, out)
	}
	if code, _ := c.do("POST", "/api/users", map[string]string{"username": "bob"}); code != 400 {
		t.Errorf("duplicate username: %d", code)
	}
	if code, _ := c.do("POST", "/api/users", map[string]string{"username": "../x"}); code != 400 {
		t.Errorf("bad username: %d", code)
	}
	if b, _ := accts.Get("bob"); strings.Contains(b.Hash, pw) || !b.MustChange || b.Role != RoleUser {
		t.Errorf("stored account: %+v", b)
	}

	bob := &client{t, &http.Client{Jar: func() http.CookieJar { j, _ := cookiejar.New(nil); return j }()}, c.url}
	if bob.login("bob", pw) != 200 {
		t.Fatal("generated password refused")
	}
	if code, _ := bob.do("GET", "/api/state", nil); code == 200 {
		t.Error("bob saw the dashboard before changing the password")
	}
	if code, _ := bob.do("POST", "/api/password", map[string]string{"old": pw, "new": "bobs-own-passw0rd"}); code != 200 {
		t.Fatal("bob could not change it")
	}
	if code, _ := bob.do("GET", "/api/state", nil); code != 200 {
		t.Error("bob locked out after changing")
	}
	if code, _ := bob.do("GET", "/api/users", nil); code != 403 {
		t.Errorf("a plain user listed accounts: %d", code)
	}
	if code, _ := bob.do("POST", "/api/users", map[string]string{"username": "eve"}); code != 403 {
		t.Errorf("a plain user created an account: %d", code)
	}
	if b, _ := accts.Get("bob"); b.MustChange {
		t.Error("must_change not cleared")
	}

	// reset gives a new temporary password, kicks the user out, and forces another change
	_, rs := c.do("POST", "/api/users/bob/reset", nil)
	if code, _ := bob.do("GET", "/api/state", nil); code != 401 {
		t.Errorf("session survived a reset: %d", code)
	}
	if bob.login("bob", "bobs-own-passw0rd") != 401 || bob.login("bob", rs["password"].(string)) != 200 {
		t.Error("reset did not replace the password")
	}

	// the file is private and persists
	a2, fresh, err := OpenAccounts(path)
	if err != nil || fresh || len(a2.List()) != 2 {
		t.Errorf("reopen: %v fresh=%v n=%d", err, fresh, len(a2.List()))
	}
}

func TestDeleteRules(t *testing.T) {
	c, _, _ := newServer(t, "tok")
	c.login("admin", "admin")
	c.do("POST", "/api/password", map[string]string{"old": "admin", "new": "a-new-passw0rd"})
	if code, _ := c.do("POST", "/api/users/admin/delete", nil); code != 400 {
		t.Errorf("deleting yourself: %d", code)
	}
	_, o := c.do("POST", "/api/users", map[string]string{"username": "bob", "role": "admin"})
	b := &client{t, &http.Client{}, c.url}
	jar, _ := cookiejar.New(nil)
	b.c.Jar = jar
	b.login("bob", o["password"].(string))
	b.do("POST", "/api/password", map[string]string{"old": o["password"].(string), "new": "bobs-own-passw0rd"})
	if code, _ := c.do("POST", "/api/users/bob/delete", nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := b.do("GET", "/api/state", nil); code != 401 {
		t.Errorf("deleted user's session still works: %d", code)
	}
}

func TestTokenStillWorksForMachinesAndOpenPathsStayOpen(t *testing.T) {
	c, _, _ := newServer(t, "tok")
	req, _ := http.NewRequest("GET", c.url+"/api/state", nil)
	req.Header.Set("Authorization", "Bearer tok")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("bearer token: %v %v", err, resp)
	}
	req.Header.Set("Authorization", "Bearer nope")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") != "" {
		t.Errorf("bad token: %d", resp.StatusCode)
	}
	for _, p := range []string{"/", "/healthz", "/api/me"} {
		if code, _ := c.do("GET", p, nil); code != 200 {
			t.Errorf("%s: %d", p, code)
		}
	}
	if code, _ := c.do("GET", "/nope", nil); code == 200 {
		t.Error("unknown path reachable without login")
	}
}

func TestCrossSiteAndFormPostsAreRefused(t *testing.T) {
	c, _, _ := newServer(t, "tok")
	req, _ := http.NewRequest("POST", c.url+"/api/login", strings.NewReader(`{"username":"admin","password":"admin"}`))
	req.Header.Set("Content-Type", "text/plain")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 415 {
		t.Errorf("text/plain post: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("POST", c.url+"/api/login", strings.NewReader(`{"username":"admin","password":"admin"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 403 {
		t.Errorf("foreign origin: %d", resp.StatusCode)
	}
}

func TestLockoutAfterRepeatedFailures(t *testing.T) {
	c, _, _ := newServer(t, "tok")
	for range maxFailures {
		c.login("admin", "wrong")
	}
	if code := c.login("admin", "admin"); code != 429 {
		t.Errorf("correct password during lockout: %d", code)
	}
}

func TestSessionCookieFlags(t *testing.T) {
	c, _, _ := newServer(t, "tok")
	b, _ := json.Marshal(map[string]string{"username": "admin", "password": "admin"})
	req, _ := http.NewRequest("POST", c.url+"/api/login", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, _ := http.DefaultClient.Do(req)
	ck := resp.Cookies()
	if len(ck) != 1 || !ck[0].HttpOnly || !ck[0].Secure || ck[0].SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie: %+v", ck)
	}
}

func TestPasswordHashing(t *testing.T) {
	h, _ := hashPassword("correct horse")
	if !verifyPassword(h, "correct horse") || verifyPassword(h, "correct hors") || verifyPassword("garbage", "x") {
		t.Error("verify")
	}
	h2, _ := hashPassword("correct horse")
	if h == h2 {
		t.Error("salt not random")
	}
	seen := map[string]bool{}
	for range 50 {
		pw, _, _ := newGenerated()
		if len(pw) != generatedLen || strings.ContainsAny(pw, "0O1lI") || seen[pw] {
			t.Fatalf("generated %q", pw)
		}
		seen[pw] = true
	}
}

func TestSessionExpires(t *testing.T) {
	accts, _, _ := OpenAccounts(filepath.Join(t.TempDir(), "u.json"))
	au := NewAuth(accts, nil)
	ts := httptest.NewServer((&Server{Name: "t", Tracker: NewTracker(), Guard: noSlots{}, Auth: au}).Handler())
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	c := &client{t, &http.Client{Jar: jar}, ts.URL}
	c.login("admin", "admin")
	c.do("POST", "/api/password", map[string]string{"old": "admin", "new": "a-new-passw0rd"})
	if code, _ := c.do("GET", "/api/state", nil); code != 200 {
		t.Fatalf("state: %d", code)
	}
	au.mu.Lock()
	for k, s := range au.sessions {
		s.expires = time.Now().Add(-time.Second)
		au.sessions[k] = s
	}
	au.mu.Unlock()
	if code, _ := c.do("GET", "/api/state", nil); code != 401 {
		t.Errorf("expired session accepted: %d", code)
	}
}

func TestFilePermissionsAndNoAdminRefused(t *testing.T) {
	_, _, path := newServer(t, "tok")
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("users file: %v %v", st, err)
	}
	os.WriteFile(path, []byte(`[{"name":"bob","hash":"x","role":"user"}]`), 0o600)
	if _, _, err := OpenAccounts(path); err == nil {
		t.Error("a users file with no admin must be refused")
	}
}
