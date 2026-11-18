package dashboard

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/MacLeodTech/runner-scaling/internal/api"
)

type slotsWithCounters struct {
	fakeSlots
	counters api.Counters
}

func (f slotsWithCounters) SlotStats(context.Context) ([]api.Slot, error) {
	return []api.Slot{
		{Slot: 1, RunnerName: "ci-1-x", RunnerState: "running", Stats: &api.SlotStats{MemBytes: 300}},
		{Slot: 2, RunnerName: "ci-2-y", RunnerState: "running", Stats: &api.SlotStats{MemBytes: 200}},
		{Slot: 3, Starting: true},
	}, nil
}
func (f slotsWithCounters) Counters(context.Context) (api.Counters, error) { return f.counters, nil }

func wantLines(t *testing.T, body string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(body, l+"\n") {
			t.Errorf("metrics lack %q\n%s", l, body)
		}
	}
}

func TestMetrics(t *testing.T) {
	tr := NewTracker()
	q := time.Unix(1000, 0)
	b := base(1)
	b.QueueTime = q
	tr.Observe(&scaleset.RunnerScaleSetMessage{JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: b}}})
	// Picked up 25s after it was queued: lands in the 30s bucket, not the 20s one.
	started := b
	started.RunnerAssignTime = q.Add(25 * time.Second)
	tr.Observe(&scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{RunnerName: "ci-1-x", JobMessageBase: started}}})
	// It ran for 100s.
	done := started
	done.FinishTime = q.Add(125 * time.Second)
	tr.Observe(&scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{Result: "succeeded", RunnerName: "ci-1-x", JobMessageBase: done}}})
	tr.Observe(&scaleset.RunnerScaleSetMessage{JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: base(2)}}}) // still queued
	tr.SlotStart(3*time.Second, nil)
	tr.SlotStart(time.Second, errors.New("boom"))

	srv := &Server{
		Name: "ci", Info: api.Info{MaxSlots: 6, MemoryBudget: 8 << 30}, Tracker: tr, Token: "tok",
		Guard: slotsWithCounters{counters: api.Counters{"denied_total": 0, "admission_refused_total": 4, "reaped_memory_budget_total": 1}},
	}
	h := srv.Handler()
	if w := get(h, "/metrics"); w.Code != 401 {
		t.Fatalf("metrics without the token: %d", w.Code)
	}
	w := get(h, "/metrics", "Authorization", "Bearer tok")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("metrics: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	wantLines(t, body,
		`runner_scaling_jobs{pool="ci",state="queued"} 1`,
		`runner_scaling_jobs{pool="ci",state="running"} 0`,
		`runner_scaling_max_slots{pool="ci"} 6`,
		`runner_scaling_memory_budget_bytes{pool="ci"} 8589934592`,
		`runner_scaling_memory_used_bytes{pool="ci"} 500`,
		`runner_scaling_slot_memory_bytes{pool="ci",slot="1"} 300`,
		`runner_scaling_slots{pool="ci",state="running"} 2`,
		`runner_scaling_slots{pool="ci",state="starting"} 1`,
		`runner_scaling_guard_up{pool="ci"} 1`,
		`runner_scaling_jobs_completed_total{pool="ci",result="succeeded"} 1`,
		`runner_scaling_slot_starts_total{pool="ci",outcome="ok"} 1`,
		`runner_scaling_slot_starts_total{pool="ci",outcome="error"} 1`,
		// cumulative buckets: 25s is above le=20 and within le=30
		`runner_scaling_job_start_latency_seconds_bucket{pool="ci",le="20"} 0`,
		`runner_scaling_job_start_latency_seconds_bucket{pool="ci",le="30"} 1`,
		`runner_scaling_job_start_latency_seconds_bucket{pool="ci",le="+Inf"} 1`,
		`runner_scaling_job_start_latency_seconds_sum{pool="ci"} 25`,
		`runner_scaling_job_start_latency_seconds_count{pool="ci"} 1`,
		`runner_scaling_job_duration_seconds_bucket{pool="ci",le="60"} 0`,
		`runner_scaling_job_duration_seconds_bucket{pool="ci",le="180"} 1`,
		`runner_scaling_job_duration_seconds_sum{pool="ci"} 100`,
		`runner_scaling_slot_start_seconds_count{pool="ci"} 2`,
		`runner_scaling_guard_denied_total{pool="ci"} 0`,
		`runner_scaling_guard_admission_refused_total{pool="ci"} 4`,
		`runner_scaling_guard_reaped_memory_budget_total{pool="ci"} 1`,
	)
	if strings.Contains(body, "tok") {
		t.Error("the token leaked into the metrics")
	}
}

func TestMetricsWhenTheGuardIsDown(t *testing.T) {
	srv := &Server{Name: "ci", Tracker: NewTracker(), Guard: fakeSlots{err: errors.New("down")}}
	w := get(srv.Handler(), "/metrics")
	if w.Code != 200 {
		t.Fatalf("metrics: %d", w.Code)
	}
	wantLines(t, w.Body.String(), `runner_scaling_guard_up{pool="ci"} 0`)
	if strings.Contains(w.Body.String(), "runner_scaling_slots{") {
		t.Error("slot metrics reported although the guard is down")
	}
}

func TestMetricsNilTrackerSlotStart(t *testing.T) {
	var tr *Tracker
	tr.SlotStart(time.Second, nil) // must not panic
}
