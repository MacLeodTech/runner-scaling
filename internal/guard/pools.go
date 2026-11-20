package guard

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MacLeodTech/runner-scaling/internal/api"
)

// Pools is every pool one guard process runs. Each pool is a full Guard with its own prefix, slots, networks and
// template; what they share is the process, the socket and the memory budget (which the Guards already compute from the
// containers the daemon reports, so it needs no extra wiring).
type Pools struct {
	list []*Guard
	by   map[string]*Guard
}

// NewPools builds a Guard per config. Keys (names, or prefixes) must be distinct; LoadConfigs checks that.
func NewPools(cfgs []*Config, d Docker, log *slog.Logger) *Pools {
	p := &Pools{by: map[string]*Guard{}}
	for _, c := range cfgs {
		l := log
		if len(cfgs) > 1 {
			l = log.With("pool", c.Key())
		}
		g := New(c, d, l)
		p.list = append(p.list, g)
		p.by[c.Key()] = g
	}
	return p
}

// All returns the pools in config order.
func (p *Pools) All() []*Guard { return p.list }

// Setup does the one-time work of every pool at start (images, stale work volumes, module blocklist) and starts the
// reapers. Images are pulled once per distinct image, the second pool finds it present.
func (p *Pools) Setup(ctx context.Context) error {
	for _, g := range p.list {
		pctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		err := g.EnsureImages(pctx)
		cancel()
		if err != nil {
			return fmt.Errorf("pool %s: %w", g.cfg.Key(), err)
		}
	}
	var wg sync.WaitGroup
	for _, g := range p.list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wctx, wcancel := context.WithTimeout(ctx, 15*time.Minute)
			g.WipeStale(wctx)
			wcancel()
			mctx, mcancel := context.WithTimeout(ctx, 2*time.Minute)
			_ = g.ApplyModuleBlocklist(mctx) // logs and counts its own failure; the pool still runs without it
			mcancel()
		}()
	}
	wg.Wait()
	for _, g := range p.list {
		go g.ReapLoop(ctx)
	}
	return nil
}

// Handler serves GET /v1/pools and, per pool, the whole single-pool API under /v1/pools/{pool}/. A guard with exactly
// one pool also answers on /v1/... directly, as guards did before pools existed.
func (p *Pools) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/pools", func(w http.ResponseWriter, r *http.Request) {
		out := make([]api.Info, 0, len(p.list))
		for _, g := range p.list {
			out = append(out, g.Info())
		}
		writeJSON(w, http.StatusOK, out)
	})
	for _, g := range p.list {
		pre := "/v1/pools/" + g.cfg.Key()
		h := g.Handler()
		mux.HandleFunc(pre+"/", func(w http.ResponseWriter, r *http.Request) {
			// /v1/pools/<pool>/x is the single-pool API's /v1/x.
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/v1/" + strings.TrimPrefix(r.URL.Path, pre+"/")
			r2.URL.RawPath = ""
			h.ServeHTTP(w, r2)
		})
	}
	if len(p.list) == 1 {
		mux.Handle("/v1/", p.list[0].Handler())
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		slog.Warn("DENY", "method", r.Method, "path", truncate(r.URL.Path, 200), "why", "no such pool or route")
		writeJSON(w, http.StatusNotFound, api.Error{Error: "no such pool or route"})
	})
	return mux
}

// Key lists the pool keys, for log lines.
func (p *Pools) Keys() string {
	var k []string
	for _, g := range p.list {
		k = append(k, g.cfg.Key())
	}
	return strings.Join(k, ",")
}
