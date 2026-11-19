package dashboard

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookie = "rs_session"
	sessionTTL    = 12 * time.Hour
	maxFailures   = 5
	lockout       = 5 * time.Minute
)

type session struct {
	id       string // random, shown in the session list; not derived from the cookie
	user     string
	expires  time.Time
	created  time.Time
	lastSeen time.Time
	ip, fwd  string
	agent    string
}

// Auth is the login layer: accounts, sessions (in memory, so a restart logs everyone out) and failed-login lockouts.
type Auth struct {
	Accounts *Accounts
	Log      *slog.Logger
	Audit    *Audit // optional: account and login events are appended here

	mu       sync.Mutex
	sessions map[string]session // sha256(cookie value) -> session
	fails    map[string]*failure
}

type failure struct {
	n     int
	until time.Time
}

// NewAuth returns a login layer over the accounts.
func NewAuth(a *Accounts, log *slog.Logger) *Auth {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Auth{Accounts: a, Log: log, sessions: map[string]session{}, fails: map[string]*failure{}}
}

func tokenKey(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }

func (a *Auth) start(w http.ResponseWriter, r *http.Request, user string) {
	b := make([]byte, 32)
	rand.Read(b)
	v := hex.EncodeToString(b)
	a.mu.Lock()
	for k, s := range a.sessions { // sweep expired ones as we go
		if time.Now().After(s.expires) {
			delete(a.sessions, k)
		}
	}
	id := make([]byte, 8)
	rand.Read(id)
	now := time.Now()
	ip, fwd := peerInfo(r)
	a.sessions[tokenKey(v)] = session{id: hex.EncodeToString(id), user: user, expires: now.Add(sessionTTL), created: now,
		lastSeen: now, ip: ip, fwd: fwd, agent: trunc(r.UserAgent(), 200)}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: v, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: isHTTPS(r)})
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// current returns the logged-in account for the request, if any. A deleted account's session is dead.
func (a *Auth) current(r *http.Request) (Account, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return Account{}, false
	}
	a.mu.Lock()
	s, ok := a.sessions[tokenKey(c.Value)]
	if ok && time.Now().After(s.expires) {
		delete(a.sessions, tokenKey(c.Value))
		ok = false
	}
	if ok {
		s.lastSeen = time.Now()
		a.sessions[tokenKey(c.Value)] = s
	}
	a.mu.Unlock()
	if !ok {
		return Account{}, false
	}
	return a.Accounts.Get(s.user)
}

// endSessions logs a user out everywhere, optionally keeping one cookie (the one that just changed the password).
func (a *Auth) endSessions(user string, keep string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, s := range a.sessions {
		if s.user == user && k != keep {
			delete(a.sessions, k)
		}
	}
}

func (a *Auth) locked(user string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.fails[user]
	return f != nil && f.n >= maxFailures && time.Now().Before(f.until)
}

func (a *Auth) fail(user string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.fails) > 1000 { // names are attacker-chosen: don't let the table grow without bound
		clear(a.fails)
	}
	f := a.fails[user]
	if f == nil || time.Now().After(f.until) && f.n >= maxFailures {
		f = &failure{}
		a.fails[user] = f
	}
	f.n++
	f.until = time.Now().Add(lockout)
}

func (a *Auth) clear(user string) { a.mu.Lock(); delete(a.fails, user); a.mu.Unlock() }

type apiErr struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// post guards the state-changing endpoints against cross-site requests: a JSON content type (a plain form or a
// cross-site fetch without a CORS preflight cannot send it) and, when the browser says where the request came from,
// the same host. The session cookie is SameSite=Strict as well.
func post(w http.ResponseWriter, r *http.Request, in any) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeJSON(w, http.StatusUnsupportedMediaType, apiErr{"send JSON"})
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		if u, err := url.Parse(o); err != nil || u.Host != r.Host {
			writeJSON(w, http.StatusForbidden, apiErr{"cross-site request refused"})
			return false
		}
	}
	if in != nil {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(in); err != nil {
			writeJSON(w, http.StatusBadRequest, apiErr{"bad request"})
			return false
		}
	}
	return true
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// peerInfo returns the TCP peer and the X-Forwarded-For header as sent. Behind a reverse proxy the peer is the proxy,
// and the header is whatever the client claimed, so it is recorded as a hint and never trusted.
func peerInfo(r *http.Request) (ip, fwd string) {
	return clientIP(r), trunc(r.Header.Get("X-Forwarded-For"), 100)
}

// trunc cuts attacker-controlled text to n bytes (on a rune boundary) before it is stored or logged.
func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// audit records an event about user, from this request.
func (a *Auth) audit(r *http.Request, event, user, by, detail string) {
	ip, fwd := peerInfo(r)
	if by == user {
		by = ""
	}
	a.Audit.Add(AuditEntry{Event: event, User: trunc(user, 64), By: by, IP: ip, Fwd: fwd, Detail: trunc(detail, 200)})
}

type meResp struct {
	Authenticated bool   `json:"authenticated"`
	Name          string `json:"name,omitempty"`
	Role          string `json:"role,omitempty"`
	MustChange    bool   `json:"must_change,omitempty"`
}

// routes registers the account endpoints.
func (a *Auth) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		if u, ok := a.current(r); ok {
			writeJSON(w, 200, meResp{true, u.Name, u.Role, u.MustChange})
			return
		}
		writeJSON(w, 200, meResp{})
	})
	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Username, Password string }
		if !post(w, r, &in) {
			return
		}
		in.Username = strings.ToLower(strings.TrimSpace(in.Username))
		if a.locked(in.Username) {
			a.Log.Warn("login refused: locked out", "user", in.Username, "ip", clientIP(r))
			a.audit(r, "login_locked", in.Username, "", "")
			writeJSON(w, http.StatusTooManyRequests, apiErr{"too many failed attempts: try again in a few minutes"})
			return
		}
		u, ok := a.Accounts.Authenticate(in.Username, in.Password)
		if !ok {
			a.fail(in.Username)
			a.Log.Warn("login failed", "user", in.Username, "ip", clientIP(r))
			a.audit(r, "login_failed", in.Username, "", "")
			writeJSON(w, http.StatusUnauthorized, apiErr{"wrong username or password"})
			return
		}
		a.clear(in.Username)
		a.start(w, r, u.Name)
		a.Log.Info("login", "user", u.Name, "ip", clientIP(r), "must_change", u.MustChange)
		a.audit(r, "login", u.Name, "", "")
		writeJSON(w, 200, meResp{true, u.Name, u.Role, u.MustChange})
	})
	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		if !post(w, r, nil) {
			return
		}
		if c, err := r.Cookie(sessionCookie); err == nil {
			a.mu.Lock()
			s, had := a.sessions[tokenKey(c.Value)]
			delete(a.sessions, tokenKey(c.Value))
			a.mu.Unlock()
			if had {
				a.audit(r, "logout", s.user, "", "")
			}
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		writeJSON(w, 200, meResp{})
	})
	mux.HandleFunc("POST /api/password", func(w http.ResponseWriter, r *http.Request) {
		u, ok := a.current(r)
		if !ok {
			writeJSON(w, 401, apiErr{"not logged in"})
			return
		}
		var in struct{ Old, New string }
		if !post(w, r, &in) {
			return
		}
		if a.locked(u.Name) { // the old-password check is a guessing oracle for a stolen session
			writeJSON(w, http.StatusTooManyRequests, apiErr{"too many failed attempts: try again in a few minutes"})
			return
		}
		if _, ok := a.Accounts.Authenticate(u.Name, in.Old); !ok {
			a.fail(u.Name)
			a.audit(r, "password_change_failed", u.Name, "", "wrong current password")
			writeJSON(w, 403, apiErr{"the current password is wrong"})
			return
		}
		if in.Old == in.New {
			writeJSON(w, 400, apiErr{"choose a different password"})
			return
		}
		if err := a.Accounts.SetPassword(u.Name, in.New); err != nil {
			a.writeErr(w, err)
			return
		}
		a.clear(u.Name)
		keep := ""
		if c, err := r.Cookie(sessionCookie); err == nil {
			keep = tokenKey(c.Value)
		}
		a.endSessions(u.Name, keep) // other devices have to log in again with the new password
		a.Log.Info("password changed", "user", u.Name)
		a.audit(r, "password_changed", u.Name, "", "")
		writeJSON(w, 200, meResp{true, u.Name, u.Role, false})
	})
	mux.HandleFunc("GET /api/users", a.admin(func(w http.ResponseWriter, r *http.Request, _ Account) {
		type row struct {
			Name       string    `json:"name"`
			Role       string    `json:"role"`
			MustChange bool      `json:"must_change"`
			Created    time.Time `json:"created"`
			LastLogin  time.Time `json:"last_login,omitzero"`
		}
		var out []row
		for _, u := range a.Accounts.List() {
			out = append(out, row{u.Name, u.Role, u.MustChange, u.Created, u.LastLogin})
		}
		writeJSON(w, 200, out)
	}))
	mux.HandleFunc("POST /api/users", a.admin(func(w http.ResponseWriter, r *http.Request, by Account) {
		var in struct{ Username, Role string }
		if !post(w, r, &in) {
			return
		}
		in.Username = strings.ToLower(strings.TrimSpace(in.Username))
		if in.Role == "" {
			in.Role = RoleUser
		}
		pw, err := a.Accounts.Add(in.Username, in.Role)
		if err != nil {
			a.writeErr(w, err)
			return
		}
		a.Log.Info("user created", "user", in.Username, "role", in.Role, "by", by.Name)
		a.audit(r, "user_created", in.Username, by.Name, "role "+in.Role)
		writeJSON(w, 200, map[string]string{"username": in.Username, "password": pw})
	}))
	mux.HandleFunc("POST /api/users/{name}/reset", a.admin(func(w http.ResponseWriter, r *http.Request, by Account) {
		if !post(w, r, nil) {
			return
		}
		name := r.PathValue("name")
		pw, err := a.Accounts.Reset(name)
		if err != nil {
			a.writeErr(w, err)
			return
		}
		a.endSessions(name, "")
		a.Log.Info("password reset", "user", name, "by", by.Name)
		a.audit(r, "password_reset", name, by.Name, "")
		writeJSON(w, 200, map[string]string{"username": name, "password": pw})
	}))
	mux.HandleFunc("POST /api/users/{name}/delete", a.admin(func(w http.ResponseWriter, r *http.Request, by Account) {
		if !post(w, r, nil) {
			return
		}
		name := r.PathValue("name")
		if name == by.Name {
			writeJSON(w, 400, apiErr{"you cannot delete your own account"})
			return
		}
		if err := a.Accounts.Delete(name); err != nil {
			a.writeErr(w, err)
			return
		}
		a.endSessions(name, "")
		a.Log.Info("user deleted", "user", name, "by", by.Name)
		a.audit(r, "user_deleted", name, by.Name, "")
		writeJSON(w, 200, map[string]string{"deleted": name})
	}))
	// Sessions: everyone sees and ends their own logins; an admin sees and ends everyone's.
	mux.HandleFunc("GET /api/sessions", a.member(func(w http.ResponseWriter, r *http.Request, u Account) {
		cur := ""
		if c, err := r.Cookie(sessionCookie); err == nil {
			cur = tokenKey(c.Value)
		}
		type row struct {
			ID       string    `json:"id"`
			User     string    `json:"user"`
			IP       string    `json:"ip"`
			Fwd      string    `json:"fwd,omitempty"`
			Agent    string    `json:"agent,omitempty"`
			Created  time.Time `json:"created"`
			LastSeen time.Time `json:"last_seen"`
			Expires  time.Time `json:"expires"`
			Current  bool      `json:"current,omitempty"`
		}
		out := []row{}
		a.mu.Lock()
		for k, s := range a.sessions {
			if time.Now().After(s.expires) || (s.user != u.Name && u.Role != RoleAdmin) {
				continue
			}
			out = append(out, row{s.id, s.user, s.ip, s.fwd, s.agent, s.created, s.lastSeen, s.expires, k == cur})
		}
		a.mu.Unlock()
		sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
		writeJSON(w, 200, out)
	}))
	mux.HandleFunc("POST /api/sessions/{id}/revoke", a.member(func(w http.ResponseWriter, r *http.Request, u Account) {
		if !post(w, r, nil) {
			return
		}
		id := r.PathValue("id")
		a.mu.Lock()
		var gone *session
		for k, s := range a.sessions {
			// someone else's session looks the same as a missing one, so ids cannot be probed
			if s.id == id && (s.user == u.Name || u.Role == RoleAdmin) {
				gone = &s
				delete(a.sessions, k)
				break
			}
		}
		a.mu.Unlock()
		if gone == nil {
			writeJSON(w, 404, apiErr{"no such session"})
			return
		}
		a.Log.Info("session revoked", "user", gone.user, "by", u.Name)
		a.audit(r, "session_revoked", gone.user, u.Name, "")
		writeJSON(w, 200, map[string]string{"revoked": id})
	}))
	mux.HandleFunc("GET /api/audit", a.admin(func(w http.ResponseWriter, r *http.Request, _ Account) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		if n <= 0 || n > 1000 {
			n = 200
		}
		es, err := a.Audit.Recent(n)
		if err != nil {
			a.writeErr(w, err)
			return
		}
		writeJSON(w, 200, es)
	}))
}

func (a *Auth) writeErr(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrPolicy) {
		writeJSON(w, 400, apiErr{strings.TrimPrefix(err.Error(), "policy: ")})
		return
	}
	a.Log.Error("accounts", "err", err)
	writeJSON(w, 500, apiErr{"internal error"})
}

// member wraps a handler that needs any logged-in user who is done with the forced password change.
func (a *Auth) member(h func(http.ResponseWriter, *http.Request, Account)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := a.current(r)
		switch {
		case !ok:
			writeJSON(w, 401, apiErr{"not logged in"})
		case u.MustChange:
			writeJSON(w, 403, apiErr{"change your password first"})
		default:
			h(w, r, u)
		}
	}
}

// admin wraps a handler that needs a logged-in admin who is done with the forced password change.
func (a *Auth) admin(h func(http.ResponseWriter, *http.Request, Account)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := a.current(r)
		switch {
		case !ok:
			writeJSON(w, 401, apiErr{"not logged in"})
		case u.MustChange:
			writeJSON(w, 403, apiErr{"change your password first"})
		case u.Role != RoleAdmin:
			writeJSON(w, 403, apiErr{"admins only"})
		default:
			h(w, r, u)
		}
	}
}
