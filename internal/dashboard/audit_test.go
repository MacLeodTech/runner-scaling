package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// getList fetches a JSON array endpoint (client.do decodes objects only).
func (c *client) getList(path string) (int, []map[string]any) {
	c.t.Helper()
	resp, err := c.c.Get(c.url + path)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func newBrowser(t *testing.T, url string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t, &http.Client{Jar: jar}, url}
}

func TestAuditRotatesAndStaysBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	a := NewAudit(path, 600, nil)
	for i := 0; i < 50; i++ {
		a.Add(AuditEntry{Event: "login", User: fmt.Sprintf("u%02d", i)})
	}
	for _, p := range []string{path, path + ".1"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() > 600 {
			t.Errorf("%s is %d bytes, cap 600", p, fi.Size())
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", p, fi.Mode().Perm())
		}
	}
	got, err := a.Recent(1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].User != "u49" {
		t.Fatalf("newest first expected u49, got %+v", got)
	}
	if len(got) >= 50 {
		t.Errorf("nothing was dropped: %d entries", len(got))
	}
	for i := 1; i < len(got); i++ { // no gaps: what survives is a contiguous tail
		if want := fmt.Sprintf("u%02d", 49-i); got[i].User != want {
			t.Fatalf("entry %d is %s, want %s", i, got[i].User, want)
		}
	}
	if few, _ := a.Recent(3); len(few) != 3 {
		t.Errorf("limit: %d", len(few))
	}
}

func TestAuditToleratesDamageAndNil(t *testing.T) {
	var none *Audit
	none.Add(AuditEntry{Event: "x"}) // must not panic
	if es, err := none.Recent(5); err != nil || len(es) != 0 {
		t.Errorf("nil audit: %v %v", es, err)
	}
	path := filepath.Join(t.TempDir(), "a.jsonl")
	os.WriteFile(path, []byte("not json\n{\"event\":\"login\",\"user\":\"x\"}\n\n{\"user\":\"no-event\"}\n"), 0o600)
	es, err := NewAudit(path, 0, nil).Recent(10)
	if err != nil || len(es) != 1 || es[0].User != "x" {
		t.Errorf("damaged file: %v %v", es, err)
	}
	if es, err := NewAudit(filepath.Join(t.TempDir(), "missing"), 0, nil).Recent(10); err != nil || len(es) != 0 {
		t.Errorf("missing file: %v %v", es, err)
	}
}

func TestAuditTrailOfAccountEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	c, _, _ := newServerWith(t, "tok", func(a *Auth) { a.Audit = NewAudit(path, 0, nil) })

	c.login("admin", "wrong")
	c.login("admin", "admin")
	c.do("POST", "/api/password", map[string]string{"old": "admin", "new": "a-new-passw0rd"})
	_, out := c.do("POST", "/api/users", map[string]string{"username": "bob"})
	pw := out["password"].(string)
	c.do("POST", "/api/users/bob/reset", map[string]string{})
	c.do("POST", "/api/users/bob/delete", map[string]string{})
	c.do("POST", "/api/logout", map[string]string{})

	raw, _ := os.ReadFile(path)
	text := string(raw)
	for _, ev := range []string{"login_failed", `"event":"login"`, "password_changed", "user_created", "password_reset", "user_deleted", "logout"} {
		if !strings.Contains(text, ev) {
			t.Errorf("audit lacks %s:\n%s", ev, text)
		}
	}
	// the user names are expected; passwords and hashes never are
	for _, secret := range []string{"a-new-passw0rd", pw, `"wrong"`, "pbkdf2"} {
		if strings.Contains(text, secret) {
			t.Errorf("audit contains %q:\n%s", secret, text)
		}
	}
	if !strings.Contains(text, `"by":"admin"`) {
		t.Errorf("admin actions should name the admin:\n%s", text)
	}
}

func TestAuditEndpointIsAdminOnly(t *testing.T) {
	c, _, _ := newServerWith(t, "tok", func(a *Auth) { a.Audit = NewAudit(filepath.Join(t.TempDir(), "a.jsonl"), 0, nil) })
	if code, _ := c.getList("/api/audit"); code != 401 {
		t.Errorf("anonymous: %d", code)
	}
	c.login("admin", "admin")
	c.do("POST", "/api/password", map[string]string{"old": "admin", "new": "a-new-passw0rd"})
	code, es := c.getList("/api/audit")
	if code != 200 || len(es) == 0 || es[0]["event"] != "password_changed" {
		t.Fatalf("admin: %d %v", code, es)
	}
	_, out := c.do("POST", "/api/users", map[string]string{"username": "bob"})
	bob := newBrowser(t, c.url)
	bob.login("bob", out["password"].(string))
	bob.do("POST", "/api/password", map[string]string{"old": out["password"].(string), "new": "bobs-own-passw0rd"})
	if code, _ := bob.getList("/api/audit"); code != 403 {
		t.Errorf("non-admin read the audit log: %d", code)
	}
}

func TestSessionsListAndRevoke(t *testing.T) {
	c, _, _ := newServerWith(t, "tok", func(a *Auth) { a.Audit = NewAudit(filepath.Join(t.TempDir(), "a.jsonl"), 0, nil) })
	c.login("admin", "admin")
	c.do("POST", "/api/password", map[string]string{"old": "admin", "new": "a-new-passw0rd"})
	_, out := c.do("POST", "/api/users", map[string]string{"username": "bob"})
	temp := out["password"].(string)

	const bobPw = "bobs-own-passw0rd"
	bob1 := newBrowser(t, c.url)
	bob1.login("bob", temp)
	bob1.do("POST", "/api/password", map[string]string{"old": temp, "new": bobPw})
	bob2 := newBrowser(t, c.url) // a second device, logged in after the change
	bob2.login("bob", bobPw)

	// bob sees only his own sessions, and the current one is marked
	code, mine := bob1.getList("/api/sessions")
	if code != 200 || len(mine) != 2 {
		t.Fatalf("bob's sessions: %d %v", code, mine)
	}
	var other string
	for _, s := range mine {
		if s["user"] != "bob" {
			t.Errorf("bob sees %v's session", s["user"])
		}
		if s["current"] != true {
			other = s["id"].(string)
		}
	}
	if other == "" {
		t.Fatal("no current marker")
	}
	// an admin sees everyone's, with metadata
	_, all := c.getList("/api/sessions")
	users, adminID := map[any]bool{}, ""
	for _, s := range all {
		users[s["user"]] = true
		if s["user"] == "admin" {
			adminID = s["id"].(string)
		}
		if s["id"] == nil || s["ip"] == "" || s["last_seen"] == nil || s["created"] == nil {
			t.Errorf("session lacks metadata: %v", s)
		}
	}
	if !users["admin"] || !users["bob"] || len(all) != 3 {
		t.Errorf("admin should see admin and both of bob's: %v", all)
	}

	// bob cannot end the admin's session, and a bogus id looks the same as a foreign one
	if code, _ := bob1.do("POST", "/api/sessions/"+adminID+"/revoke", map[string]string{}); code != 404 {
		t.Errorf("bob revoked the admin's session: %d", code)
	}
	if code, _ := c.do("GET", "/api/me", nil); code != 200 {
		t.Error("admin lost the session")
	}
	if code, _ := bob1.do("POST", "/api/sessions/nope/revoke", map[string]string{}); code != 404 {
		t.Errorf("bogus id: %d", code)
	}

	// bob ends his other device himself: that one is logged out, this one is not
	if code, _ := bob1.do("POST", "/api/sessions/"+other+"/revoke", map[string]string{}); code != 200 {
		t.Fatalf("own revoke: %d", code)
	}
	if _, me := bob1.do("GET", "/api/me", nil); me["authenticated"] != true {
		t.Error("bob1 was logged out by revoking the other session")
	}
	if _, me := bob2.do("GET", "/api/me", nil); me["authenticated"] == true {
		t.Error("the revoked session still works")
	}
	// an admin ends bob's last session
	_, left := bob1.getList("/api/sessions")
	if len(left) != 1 {
		t.Fatalf("bob should have one session left: %v", left)
	}
	if code, _ := c.do("POST", "/api/sessions/"+left[0]["id"].(string)+"/revoke", map[string]string{}); code != 200 {
		t.Fatalf("admin revoke: %d", code)
	}
	if _, me := bob1.do("GET", "/api/me", nil); me["authenticated"] == true {
		t.Error("admin revoke did not log bob out")
	}
}

func TestSessionEndpointsNeedAPostFromTheSameSite(t *testing.T) {
	c, _, _ := newServer(t, "tok")
	c.login("admin", "admin")
	c.do("POST", "/api/password", map[string]string{"old": "admin", "new": "a-new-passw0rd"})
	_, all := c.getList("/api/sessions")
	id := all[0]["id"].(string)
	req, _ := http.NewRequest("POST", c.url+"/api/sessions/"+id+"/revoke", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.example")
	resp, err := c.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("cross-site revoke: %d", resp.StatusCode)
	}
	if code, _ := c.getList("/api/sessions"); code != 200 {
		t.Error("session was revoked by a cross-site request")
	}
}
