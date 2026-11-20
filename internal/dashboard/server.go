package dashboard

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MacLeodTech/runner-scaling/internal/api"
	"github.com/MacLeodTech/runner-scaling/internal/ghapi"
)

//go:embed index.html
var page []byte

// Slots lists the runners the guard holds, with their resource use.
type Slots interface {
	SlotStats(ctx context.Context) ([]api.Slot, error)
}

// State is GET /api/state.
type State struct {
	Name     string     `json:"name"`
	Version  string     `json:"version"`
	Now      time.Time  `json:"now"`
	MaxSlots int        `json:"max_slots"`
	Slots    []api.Slot `json:"slots"`
	GuardErr string     `json:"guard_error,omitempty"`
	// GitHub is the health of the job-detail poller (rate limit, missing permission); nil when it is off.
	GitHub *ghapi.Status `json:"github,omitempty"`
	Snapshot
	// Peers are the other scalers' own states (one per pool), so one page can show every pool.
	Peers []State `json:"peers,omitempty"`
	// PeerErr is set on a peer entry that could not be fetched.
	PeerErr string `json:"peer_error,omitempty"`
}

// Peer is another scaler's dashboard, e.g. {"lite", "http://scaler-lite:8080"}.
type Peer struct{ Name, URL string }

// Server serves the page and its JSON. The dashboard itself is read-only. The only thing that can change is the account
// list, and only when Auth is set (see auth.go): then a browser needs a login, while Token stays valid as the credential
// for machines (peer scalers, the runners CLI, Prometheus).
type Server struct {
	Name, Version string
	Info          api.Info
	Guard         Slots
	Tracker       *Tracker
	GitHub        *Enricher     // optional: steps and run details from GitHub
	StallAfter    time.Duration // see MarkStalled; default DefaultStallAfter
	Peers         []Peer        // other scalers to fold into this page; they must share Token
	Locals        []*Server     // other pools of this same process, folded in without an HTTP hop
	Token         string        // if set, required as a bearer token, ?token= or the password of basic auth
	Auth          *Auth         // if set, a login session is accepted instead of the token, and the account API is served
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("GET /metrics", s.metrics)
	if s.Auth != nil {
		s.Auth.routes(mux)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	return s.secure(mux)
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	st := s.local(ctx)
	if r.URL.Query().Get("local") == "" { // peers answer with local=1, so peers never chain
		for _, l := range s.Locals {
			st.Peers = append(st.Peers, l.local(ctx))
		}
		st.Peers = append(st.Peers, s.peerStates(ctx)...)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(st)
}

// local is this pool's own state, without peers.
func (s *Server) local(ctx context.Context) State {
	st := State{Name: s.Name, Version: s.Version, Now: time.Now(), MaxSlots: s.Info.MaxSlots, Snapshot: s.Tracker.Snapshot(), Slots: []api.Slot{}}
	if slots, err := s.Guard.SlotStats(ctx); err != nil {
		st.GuardErr = "guard unreachable"
	} else if slots != nil {
		st.Slots = slots
	}
	if s.GitHub != nil {
		gs := s.GitHub.Status()
		st.GitHub = &gs
	}
	MarkStalled(st.Running, st.Slots, st.Now, s.StallAfter)
	return st
}

func (s *Server) peerStates(ctx context.Context) []State {
	out := make([]State, len(s.Peers))
	var wg sync.WaitGroup
	for i, p := range s.Peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = State{Name: p.Name, Slots: []api.Slot{}, Snapshot: Snapshot{Queued: []Job{}, Running: []Job{}, Recent: []Job{}}}
			req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(p.URL, "/")+"/api/state?local=1", nil)
			if err == nil {
				if s.Token != "" {
					req.Header.Set("Authorization", "Bearer "+s.Token)
				}
				var resp *http.Response
				if resp, err = http.DefaultClient.Do(req); err == nil {
					defer resp.Body.Close()
					if resp.StatusCode != 200 {
						err = fmt.Errorf("status %d", resp.StatusCode)
					} else {
						var got State
						if err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&got); err == nil {
							got.Name, got.Peers = p.Name, nil
							out[i] = got
						}
					}
				}
			}
			if err != nil {
				out[i].PeerErr = p.Name + " unreachable"
			}
		}()
	}
	wg.Wait()
	return out
}

// secure adds the headers every response gets and, when a token is set, the check. /healthz is open.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'")
		if s.Auth != nil {
			if !s.openPath(r.URL.Path) && !s.sessionOK(r) && !s.authorised(r) {
				writeJSON(w, http.StatusUnauthorized, apiErr{"login required"}) // no Basic challenge: the page has its own form
				return
			}
		} else if s.Token != "" && r.URL.Path != "/healthz" && !s.authorised(r) {
			h.Set("WWW-Authenticate", `Basic realm="runner-scaling"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// openPath lists what needs no credential when accounts are on: the static page (which holds no data), the login
// endpoints, and the account endpoints that check the session themselves. Everything else is denied by default.
func (s *Server) openPath(p string) bool {
	switch p {
	case "/", "/healthz", "/api/me", "/api/login", "/api/logout", "/api/password":
		return true
	}
	return strings.HasPrefix(p, "/api/users") || strings.HasPrefix(p, "/api/sessions") || p == "/api/audit"
}

// sessionOK is a login session that is past its forced password change.
func (s *Server) sessionOK(r *http.Request) bool {
	u, ok := s.Auth.current(r)
	return ok && !u.MustChange
}

func (s *Server) authorised(r *http.Request) bool {
	if s.Token == "" {
		return false
	}
	got := r.URL.Query().Get("token")
	if _, pw, ok := r.BasicAuth(); ok {
		got = pw
	}
	if a := r.Header.Get("Authorization"); len(a) > 7 && a[:7] == "Bearer " {
		got = a[7:]
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1
}
