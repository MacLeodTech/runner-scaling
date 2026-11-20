package fence

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// Runner keeps a Spec applied and a gate file present exactly while it is.
type Runner struct {
	Spec     Spec
	Gate     string        // created while the fence is verified in place, removed otherwise
	Interval time.Duration // how often the kernel's ruleset is read back
	Log      *slog.Logger

	ok, reapplied, failures atomic.Int64
	fwd, in                 atomic.Uint64
}

// Run applies the fence and then verifies it every Interval, re-applying it (and withdrawing the gate while it is
// missing) when it has changed. It removes the gate when ctx ends, leaving the rules in place.
func (r *Runner) Run(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(r.Gate), 0o755); err != nil {
		return err
	}
	defer os.Remove(r.Gate)
	if r.Interval <= 0 {
		r.Interval = 3 * time.Second
	}
	if lacks6(r.Spec) && ipv6Forwarding() {
		r.Log.Warn("IPv6 forwarding is on but no IPv6 range is fenced (--src6): runner networks without IPv6 are unaffected, " +
			"but an IPv6-enabled runner network would not be fenced")
	}
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		r.tick()
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (r *Runner) tick() {
	st, err := r.Spec.Verify()
	if err == nil && st.OK {
		r.ok.Store(1)
		r.fwd.Store(st.DroppedForward)
		r.in.Store(st.DroppedInput)
		if _, e := os.Stat(r.Gate); e != nil {
			if os.WriteFile(r.Gate, nil, 0o644) == nil {
				r.Log.Info("fence in place; runners allowed")
			}
		}
		return
	}
	r.ok.Store(0)
	os.Remove(r.Gate) // before anything else: no new slot may start while the fence is in doubt
	problem := st.Problem
	if err != nil {
		problem = err.Error()
	}
	r.Log.Warn("fence not in place; applying", "problem", problem)
	if err := r.Spec.Apply(); err != nil {
		r.failures.Add(1)
		r.Log.Error("fence apply FAILED; runners blocked", "err", err)
		return
	}
	if st, err := r.Spec.Verify(); err != nil || !st.OK {
		r.failures.Add(1)
		r.Log.Error("fence applied but does not verify; runners blocked", "err", err, "problem", st.Problem)
		return
	}
	r.reapplied.Add(1)
	r.ok.Store(1)
	if os.WriteFile(r.Gate, nil, 0o644) == nil {
		r.Log.Info("fence applied; runners allowed")
	}
}

// Metrics serves Prometheus text on addr (e.g. 127.0.0.1:9102) until ctx ends.
func (r *Runner) Metrics(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "# TYPE runner_fence_ok gauge\nrunner_fence_ok %d\n", r.ok.Load())
		fmt.Fprintf(w, "# TYPE runner_fence_reapplied_total counter\nrunner_fence_reapplied_total %d\n", r.reapplied.Load())
		fmt.Fprintf(w, "# TYPE runner_fence_apply_failures_total counter\nrunner_fence_apply_failures_total %d\n", r.failures.Load())
		fmt.Fprintf(w, "# TYPE runner_fence_dropped_packets counter\nrunner_fence_dropped_packets{chain=\"forward\"} %d\nrunner_fence_dropped_packets{chain=\"input\"} %d\n", r.fwd.Load(), r.in.Load())
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	if err := srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func lacks6(s Spec) bool { return len(s.Src6) == 0 }

func ipv6Forwarding() bool {
	b, err := os.ReadFile("/proc/sys/net/ipv6/conf/all/forwarding")
	return err == nil && len(b) > 0 && b[0] == '1'
}
