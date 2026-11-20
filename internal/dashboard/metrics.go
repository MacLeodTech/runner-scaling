package dashboard

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MacLeodTech/runner-scaling/internal/api"
)

// Bucket upper bounds in seconds.
var (
	// Wait from a job being queued to a runner picking it up: mostly a runner starting, plus any wait for a free slot.
	startLatencyBuckets = []float64{1, 5, 10, 20, 30, 60, 120, 300, 600, 1800}
	jobDurationBuckets  = []float64{10, 30, 60, 180, 300, 600, 1200, 1800, 3600, 7200, 21600}
	// Time the scaler spent bringing one slot up (JIT config, guard start, dind healthy).
	slotStartBuckets = []float64{1, 2, 5, 10, 20, 30, 60, 120}
)

// histogram is a Prometheus-style cumulative histogram. The zero value must be set up with newHistogram.
type histogram struct {
	bounds []float64
	counts []int64 // one per bound, plus +Inf at the end; cumulative at render time
	sum    float64
	n      int64
}

func newHistogram(bounds []float64) *histogram {
	return &histogram{bounds: bounds, counts: make([]int64, len(bounds)+1)}
}

func (h *histogram) observe(v float64) {
	i := sort.SearchFloat64s(h.bounds, v) // first bound >= v
	h.counts[i]++
	h.sum += v
	h.n++
}

func (h *histogram) write(w io.Writer, name, labels string) {
	var cum int64
	for i, b := range h.bounds {
		cum += h.counts[i]
		fmt.Fprintf(w, "%s_bucket{%sle=\"%s\"} %d\n", name, labels, strconv.FormatFloat(b, 'g', -1, 64), cum)
	}
	cum += h.counts[len(h.bounds)]
	fmt.Fprintf(w, "%s_bucket{%sle=\"+Inf\"} %d\n", name, labels, cum)
	fmt.Fprintf(w, "%s_sum%s %s\n", name, wrap(labels), strconv.FormatFloat(h.sum, 'g', -1, 64))
	fmt.Fprintf(w, "%s_count%s %d\n", name, wrap(labels), h.n)
}

// wrap turns `a="b",` into `{a="b"}` and "" into "".
func wrap(labels string) string {
	if labels == "" {
		return ""
	}
	return "{" + strings.TrimSuffix(labels, ",") + "}"
}

// metrics are what the tracker counts beyond its snapshot.
type metrics struct {
	mu           sync.Mutex
	startLatency *histogram
	jobDuration  *histogram
	slotStart    *histogram
	completed    map[string]int64 // by result
	slotStarts   map[string]int64 // "ok" or "error"
}

func newMetrics() *metrics {
	return &metrics{
		startLatency: newHistogram(startLatencyBuckets), jobDuration: newHistogram(jobDurationBuckets),
		slotStart: newHistogram(slotStartBuckets), completed: map[string]int64{}, slotStarts: map[string]int64{},
	}
}

// SlotStart records how long the scaler took to bring one slot up, and whether it worked. Nil-safe.
func (t *Tracker) SlotStart(d time.Duration, err error) {
	if t == nil {
		return
	}
	t.metrics.mu.Lock()
	defer t.metrics.mu.Unlock()
	t.metrics.slotStart.observe(d.Seconds())
	if err != nil {
		t.metrics.slotStarts["error"]++
	} else {
		t.metrics.slotStarts["ok"]++
	}
}

// Counters is the guard's counter source.
type Counters interface {
	Counters(ctx context.Context) (api.Counters, error)
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	out := s.metricsText(ctx)
	if len(s.Locals) > 0 {
		parts := []string{out}
		for _, l := range s.Locals {
			parts = append(parts, l.metricsText(ctx))
		}
		out = mergeExpositions(parts)
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, out)
}

// mergeExpositions joins several pools' metric text into one: each family's HELP/TYPE once, all its samples together.
func mergeExpositions(parts []string) string {
	var order []string
	head := map[string][]string{}
	body := map[string][]string{}
	for _, part := range parts {
		cur := ""
		for _, ln := range strings.Split(part, "\n") {
			if ln == "" {
				continue
			}
			if f, ok := strings.CutPrefix(ln, "# HELP "); ok {
				cur, _, _ = strings.Cut(f, " ")
				if _, seen := head[cur]; !seen {
					order = append(order, cur)
					head[cur] = []string{ln}
				}
				continue
			}
			if strings.HasPrefix(ln, "# TYPE ") {
				if len(head[cur]) == 1 {
					head[cur] = append(head[cur], ln)
				}
				continue
			}
			body[cur] = append(body[cur], ln)
		}
	}
	var b strings.Builder
	for _, f := range order {
		for _, l := range head[f] {
			b.WriteString(l + "\n")
		}
		for _, l := range body[f] {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

func (s *Server) metricsText(ctx context.Context) string {
	snap := s.Tracker.Snapshot()
	pool := fmt.Sprintf("pool=%q,", s.Name)
	p := wrap(pool)
	var b strings.Builder
	gauge := func(name, help string) { fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name) }
	counter := func(name, help string) { fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name) }

	gauge("runner_scaling_jobs", "Jobs known to the scaler by state.")
	fmt.Fprintf(&b, "runner_scaling_jobs{%sstate=\"queued\"} %d\nrunner_scaling_jobs{%sstate=\"running\"} %d\n", pool, len(snap.Queued), pool, len(snap.Running))
	if s.GitHub != nil {
		gs := s.GitHub.Status()
		up := 1
		if gs.Error != "" {
			up = 0
		}
		gauge("runner_scaling_github_up", "1 if the last GitHub job-detail call worked.")
		fmt.Fprintf(&b, "runner_scaling_github_up%s %d\n", p, up)
		gauge("runner_scaling_github_rate_remaining", "GitHub API requests left in the current window.")
		fmt.Fprintf(&b, "runner_scaling_github_rate_remaining%s %d\n", p, gs.Remaining)
		counter("runner_scaling_github_requests_total", "GitHub API requests made for job detail, by outcome.")
		fmt.Fprintf(&b, "runner_scaling_github_requests_total{%soutcome=\"ok\"} %d\nrunner_scaling_github_requests_total{%soutcome=\"not_modified\"} %d\nrunner_scaling_github_requests_total{%soutcome=\"error\"} %d\n",
			pool, gs.Requests-gs.NotModified-gs.Errors, pool, gs.NotModified, pool, gs.Errors)
	}
	gauge("runner_scaling_scale_set", "The scale set's own counters from GitHub.")
	for _, kv := range []struct {
		k string
		v int
	}{{"available", snap.Stats.Available}, {"acquired", snap.Stats.Acquired}, {"assigned", snap.Stats.Assigned},
		{"running", snap.Stats.Running}, {"registered_runners", snap.Stats.Registered}, {"busy_runners", snap.Stats.Busy}, {"idle_runners", snap.Stats.Idle}} {
		fmt.Fprintf(&b, "runner_scaling_scale_set{%sstat=%q} %d\n", pool, kv.k, kv.v)
	}

	gauge("runner_scaling_max_slots", "Most runners the guard allows at once.")
	fmt.Fprintf(&b, "runner_scaling_max_slots%s %d\n", p, s.Info.MaxSlots)
	if s.Info.MemoryBudget > 0 {
		gauge("runner_scaling_memory_budget_bytes", "The guard's memory_budget (shared by all pools on the host).")
		fmt.Fprintf(&b, "runner_scaling_memory_budget_bytes%s %d\n", p, s.Info.MemoryBudget)
	}
	slots, err := s.Guard.SlotStats(ctx)
	gauge("runner_scaling_guard_up", "1 if the guard answered.")
	up := 1
	if err != nil {
		up = 0
	}
	fmt.Fprintf(&b, "runner_scaling_guard_up%s %d\n", p, up)
	if err == nil {
		states := map[string]int{}
		var mem int64
		gauge("runner_scaling_slot_memory_bytes", "Working set of a slot's containers.")
		for _, sl := range slots {
			st := sl.RunnerState
			switch {
			case sl.Starting:
				st = "starting"
			case st == "":
				st = "none"
			}
			states[st]++
			if sl.Stats != nil {
				mem += sl.Stats.MemBytes
				fmt.Fprintf(&b, "runner_scaling_slot_memory_bytes{%sslot=\"%d\"} %d\n", pool, sl.Slot, sl.Stats.MemBytes)
			}
		}
		gauge("runner_scaling_slots", "Slots by runner state (running, exited, starting, ...).")
		for _, k := range slices.Sorted(mapsKeys(states)) {
			fmt.Fprintf(&b, "runner_scaling_slots{%sstate=%q} %d\n", pool, k, states[k])
		}
		gauge("runner_scaling_memory_used_bytes", "Working set of this pool's runners.")
		fmt.Fprintf(&b, "runner_scaling_memory_used_bytes%s %d\n", p, mem)
		if s.GitHub != nil {
			running := append([]Job(nil), snap.Running...)
			gauge("runner_scaling_jobs_stalled", "Running jobs stuck in one step while their runner is idle.")
			fmt.Fprintf(&b, "runner_scaling_jobs_stalled%s %d\n", p, MarkStalled(running, slots, time.Now(), s.StallAfter))
		}
	}

	if t := s.Tracker; t != nil {
		m := t.metrics
		m.mu.Lock()
		fmt.Fprintf(&b, "# HELP runner_scaling_jobs_completed_total Finished jobs by result.\n# TYPE runner_scaling_jobs_completed_total counter\n")
		for _, k := range slices.Sorted(mapsKeys(m.completed)) {
			fmt.Fprintf(&b, "runner_scaling_jobs_completed_total{%sresult=%q} %d\n", pool, k, m.completed[k])
		}
		counter("runner_scaling_slot_starts_total", "Slot starts by outcome.")
		for _, k := range slices.Sorted(mapsKeys(m.slotStarts)) {
			fmt.Fprintf(&b, "runner_scaling_slot_starts_total{%soutcome=%q} %d\n", pool, k, m.slotStarts[k])
		}
		fmt.Fprintf(&b, "# HELP runner_scaling_job_start_latency_seconds Seconds from a job being queued to a runner starting it.\n# TYPE runner_scaling_job_start_latency_seconds histogram\n")
		m.startLatency.write(&b, "runner_scaling_job_start_latency_seconds", pool)
		fmt.Fprintf(&b, "# HELP runner_scaling_job_duration_seconds Seconds a job ran.\n# TYPE runner_scaling_job_duration_seconds histogram\n")
		m.jobDuration.write(&b, "runner_scaling_job_duration_seconds", pool)
		fmt.Fprintf(&b, "# HELP runner_scaling_slot_start_seconds Seconds to bring one slot up (JIT config, guard start, dind healthy).\n# TYPE runner_scaling_slot_start_seconds histogram\n")
		m.slotStart.write(&b, "runner_scaling_slot_start_seconds", pool)
		m.mu.Unlock()
	}

	if c, ok := s.Guard.(Counters); ok {
		if got, err := c.Counters(ctx); err == nil {
			for _, k := range slices.Sorted(mapsKeys(got)) {
				name := "runner_scaling_guard_" + k
				counter(name, "Guard counter "+k+" since the guard started.")
				fmt.Fprintf(&b, "%s%s %d\n", name, p, got[k])
			}
		}
	}
	return b.String()
}

func mapsKeys[V any](m map[string]V) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
