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
)

//go:embed index.html
var page []byte

// Slots lists the runners the guard holds.
type Slots interface {
	Slots(ctx context.Context) ([]api.Slot, error)
}

// State is GET /api/state.
type State struct {
	Name     string     `json:"name"`
	Version  string     `json:"version"`
	Now      time.Time  `json:"now"`
	MaxSlots int        `json:"max_slots"`
	Slots    []api.Slot `json:"slots"`
	GuardErr string     `json:"guard_error,omitempty"`
	Snapshot
	// Peers are the other scalers' own states (one per pool), so one page can show every pool.
	Peers []State `json:"peers,omitempty"`
	// PeerErr is set on a peer entry that could not be fetched.
	PeerErr string `json:"peer_error,omitempty"`
}

// Peer is another scaler's dashboard, e.g. {"lite", "http://scaler-lite:8080"}.
type Peer struct{ Name, URL string }

// Server serves the page and its JSON. It is read-only: GET and HEAD only, and nothing on it changes anything.
type Server struct {
	Name, Version string
	Info          api.Info
	Guard         Slots
	Tracker       *Tracker
	Peers         []Peer // other pools to fold into this page; they must share Token
	Token         string // if set, required as a bearer token, ?token= or the password of basic auth
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	return s.secure(mux)
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	st := State{Name: s.Name, Version: s.Version, Now: time.Now(), MaxSlots: s.Info.MaxSlots, Snapshot: s.Tracker.Snapshot(), Slots: []api.Slot{}}
	if slots, err := s.Guard.Slots(ctx); err != nil {
		st.GuardErr = "guard unreachable"
	} else if slots != nil {
		st.Slots = slots
	}
	if r.URL.Query().Get("local") == "" { // peers answer with local=1, so peers never chain
		st.Peers = s.peerStates(ctx)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(st)
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
		if s.Token != "" && r.URL.Path != "/healthz" && !s.authorised(r) {
			h.Set("WWW-Authenticate", `Basic realm="runner-scaling"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authorised(r *http.Request) bool {
	got := r.URL.Query().Get("token")
	if _, pw, ok := r.BasicAuth(); ok {
		got = pw
	}
	if a := r.Header.Get("Authorization"); len(a) > 7 && a[:7] == "Bearer " {
		got = a[7:]
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1
}
