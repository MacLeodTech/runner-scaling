package dashboard

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/MacLeodTech/runner-scaling/internal/api"
	"github.com/MacLeodTech/runner-scaling/internal/ghapi"
)

func base(id int64) scaleset.JobMessageBase {
	return scaleset.JobMessageBase{RunnerRequestID: id, OwnerName: "o", RepositoryName: "r", JobDisplayName: "build",
		JobWorkflowRef: "o/r/.github/workflows/ci.yml@refs/heads/main", EventName: "push", QueueTime: time.Unix(1000+id, 0)}
}

func TestLifecycle(t *testing.T) {
	tr := NewTracker()
	tr.Observe(&scaleset.RunnerScaleSetMessage{JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: base(1)}}})
	if s := tr.Snapshot(); len(s.Queued) != 1 || s.Queued[0].Workflow != ".github/workflows/ci.yml" || s.Queued[0].Ref != "refs/heads/main" {
		t.Fatalf("queued: %+v", s)
	}
	tr.Observe(&scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{RunnerName: "ci-1-x", JobMessageBase: base(1)}}})
	tr.Observe(&scaleset.RunnerScaleSetMessage{JobAssignedMessages: []*scaleset.JobAssigned{{JobMessageBase: base(1)}}}) // late: must not regress
	if s := tr.Snapshot(); len(s.Running) != 1 || s.Running[0].Runner != "ci-1-x" || len(s.Queued) != 0 {
		t.Fatalf("running: %+v", s)
	}
	tr.Observe(&scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{Result: "succeeded", RunnerName: "ci-1-x", JobMessageBase: base(1)}}})
	s := tr.Snapshot()
	if len(s.Running) != 0 || len(s.Recent) != 1 || s.Recent[0].State != "succeeded" || s.Recent[0].Finished.IsZero() {
		t.Fatalf("done: %+v", s)
	}
}

func TestHistoryBounded(t *testing.T) {
	tr := NewTracker()
	for i := int64(0); i < HistorySize+20; i++ {
		tr.Observe(&scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{Result: "failed", JobMessageBase: base(i)}}})
	}
	if s := tr.Snapshot(); len(s.Recent) != HistorySize || s.Recent[0].ID != HistorySize+19 {
		t.Fatalf("history %d, newest %d", len(s.Recent), s.Recent[0].ID)
	}
}

func TestNilTrackerIsSafe(t *testing.T) {
	var tr *Tracker
	tr.Observe(&scaleset.RunnerScaleSetMessage{})
	_ = tr.Snapshot()
}

type fakeSlots struct{ err error }

func (f fakeSlots) SlotStats(context.Context) ([]api.Slot, error) {
	return []api.Slot{{Slot: 1, RunnerName: "ci-1-x", RunnerState: "running"}}, f.err
}

func get(h http.Handler, path string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	if len(hdr) == 2 {
		r.Header.Set(hdr[0], hdr[1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestServer(t *testing.T) {
	s := &Server{Name: "ci", Guard: fakeSlots{}, Tracker: NewTracker(), Token: "sekret"}
	h := s.Handler()
	if w := get(h, "/api/state"); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
	if w := get(h, "/api/state?token=wrong"); w.Code != 401 {
		t.Fatalf("wrong token: %d", w.Code)
	}
	if w := get(h, "/healthz"); w.Code != 200 {
		t.Fatalf("healthz: %d", w.Code)
	}
	w := get(h, "/api/state", "Authorization", "Bearer sekret")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ci-1-x"`) || strings.Contains(w.Body.String(), "sekret") {
		t.Fatalf("state: %d %s", w.Code, w.Body)
	}
	if w := get(h, "/?token=sekret"); w.Code != 200 || !strings.Contains(w.Body.String(), "<title>") {
		t.Fatalf("page: %d", w.Code)
	}
	r := httptest.NewRequest("POST", "/api/state?token=sekret", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 405 && rec.Code != 404 {
		t.Fatalf("POST allowed: %d", rec.Code)
	}
}

func TestOpenWithoutToken(t *testing.T) {
	h := (&Server{Guard: fakeSlots{}, Tracker: NewTracker()}).Handler()
	if w := get(h, "/api/state"); w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
}

func TestPeers(t *testing.T) {
	lite := httptest.NewServer((&Server{Name: "lite", Guard: fakeSlots{}, Tracker: NewTracker(), Token: "t"}).Handler())
	defer lite.Close()
	h := (&Server{Name: "main", Guard: fakeSlots{}, Tracker: NewTracker(), Token: "t",
		Peers: []Peer{{"lite", lite.URL}, {"gone", "http://127.0.0.1:1"}}}).Handler()
	w := get(h, "/api/state?token=t")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `"name":"lite"`) || !strings.Contains(body, `"peer_error":"gone unreachable"`) {
		t.Fatalf("%d %s", w.Code, body)
	}
	if w := get(h, "/api/state?token=t&local=1"); strings.Contains(w.Body.String(), `"peers"`) {
		t.Fatalf("local=1 returned peers")
	}
}

// Live scale set messages carry runnerRequestId 0; jobs must still be told apart (once all collapsed into one).
func TestJobsWithoutRequestID(t *testing.T) {
	for _, byJobID := range []bool{true, false} {
		mk := func(name string) scaleset.JobMessageBase {
			b := base(0)
			b.JobDisplayName = name
			if byJobID {
				b.JobID = "guid-" + name
			}
			return b
		}
		tr := NewTracker()
		for _, n := range []string{"a", "b", "c"} {
			tr.Observe(&scaleset.RunnerScaleSetMessage{JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: mk(n)}}})
		}
		if s := tr.Snapshot(); len(s.Queued) != 3 {
			t.Fatalf("byJobID=%v queued: %+v", byJobID, s.Queued)
		}
		for _, n := range []string{"a", "b"} {
			tr.Observe(&scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{RunnerName: "r-" + n, JobMessageBase: mk(n)}}})
		}
		if s := tr.Snapshot(); len(s.Running) != 2 || len(s.Queued) != 1 {
			t.Fatalf("byJobID=%v running: %+v queued: %+v", byJobID, s.Running, s.Queued)
		}
		tr.Observe(&scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{Result: "succeeded", RunnerName: "r-a", JobMessageBase: mk("a")}}})
		s := tr.Snapshot()
		if len(s.Running) != 1 || s.Running[0].Runner != "r-b" || len(s.Recent) != 1 {
			t.Fatalf("byJobID=%v after completion: running %+v recent %+v", byJobID, s.Running, s.Recent)
		}
	}
}

func TestApplyRunMatchesByRunnerAndName(t *testing.T) {
	tr := NewTracker()
	mk := func(name string) scaleset.JobMessageBase {
		b := base(0)
		b.JobDisplayName, b.OwnerName, b.RepositoryName, b.WorkflowRunID = name, "o", "r", 77
		return b
	}
	tr.Observe(&scaleset.RunnerScaleSetMessage{
		JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: mk("queued-one")}},
		JobStartedMessages:   []*scaleset.JobStarted{{RunnerName: "r-1", JobMessageBase: mk("building")}},
	})
	refs := tr.Wanted()
	if len(refs) != 1 || refs[0] != (RunRef{"o", "r", 77}) {
		t.Fatalf("wanted: %+v", refs)
	}
	t0 := time.Unix(5000, 0)
	jobs := []ghapi.Job{
		{ID: 1, Name: "building", RunnerName: "r-1", Status: "in_progress", HTMLURL: "u1", Steps: []ghapi.Step{
			{Number: 1, Name: "Set up job", Status: "completed", Conclusion: "success"},
			{Number: 2, Name: "setup-go", Status: "in_progress", StartedAt: t0},
			{Number: 3, Name: "test", Status: "queued"}}},
		{ID: 2, Name: "queued-one", Status: "queued"},
	}
	run := &ghapi.Run{DisplayTitle: "Fix thing", HTMLURL: "ru"}
	run.TriggeringActor.Login = "octocat"
	tr.ApplyRun(refs[0], run, jobs, t0)
	s := tr.Snapshot()
	d := s.Running[0].Detail
	if d == nil || d.Step != "setup-go" || d.StepNumber != 2 || d.StepsTotal != 3 || d.Actor != "octocat" || d.Title != "Fix thing" || d.GitHubJobID != 1 || d.Final {
		t.Fatalf("running detail: %+v", d)
	}
	if q := s.Queued[0].Detail; q == nil || q.GitHubJobID != 2 {
		t.Fatalf("queued detail: %+v", q)
	}

	// completion: the final fetch is still wanted, then the failure log tail
	tr.Observe(&scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{Result: "failed", RunnerName: "r-1", JobMessageBase: mk("building")}}})
	jobs[0].Status, jobs[0].Conclusion = "completed", "failure"
	jobs[0].Steps[1].Status, jobs[0].Steps[1].Conclusion = "completed", "failure"
	tr.ApplyRun(refs[0], run, jobs, t0)
	if len(tr.NeedsLog()) != 1 {
		t.Fatal("a failed finished job should need its log")
	}
	tr.SetLogTail(1, []string{"boom"})
	if len(tr.NeedsLog()) != 0 {
		t.Fatal("log already stored")
	}
	rec := tr.Snapshot().Recent[0].Detail
	if !rec.Final || rec.FailedStep != "setup-go" || len(rec.LogTail) != 1 {
		t.Fatalf("recent detail: %+v", rec)
	}
	for _, r := range tr.Wanted() {
		if r.RunID == 77 && len(tr.Snapshot().Queued) == 0 {
			t.Fatalf("a finalized run with nothing queued should not be polled: %+v", r)
		}
	}
}

func TestMarkStalled(t *testing.T) {
	now := time.Unix(10000, 0)
	job := func(runner string, in time.Duration) Job {
		return Job{Runner: runner, State: "running", Detail: &Detail{Step: "setup-go", StepStarted: now.Add(-in)}}
	}
	slot := func(name string, cpu float64) api.Slot {
		return api.Slot{RunnerName: name, Stats: &api.SlotStats{CPUPercent: cpu}}
	}
	running := []Job{job("idle-long", 20*time.Minute), job("busy-long", 20*time.Minute), job("idle-short", time.Minute), {Runner: "nodetail", State: "running"}}
	slots := []api.Slot{slot("idle-long", 1), slot("busy-long", 90), slot("idle-short", 0), slot("nodetail", 0)}
	if n := MarkStalled(running, slots, now, 10*time.Minute); n != 1 || !running[0].Stalled || running[1].Stalled || running[2].Stalled || running[3].Stalled {
		t.Fatalf("n=%d %+v", n, running)
	}
}

type fakeSrc struct {
	jobs   map[int64][]ghapi.Job
	err    error
	calls  int
	logErr error
}

func (f *fakeSrc) Run(context.Context, string, string, int64) (*ghapi.Run, error) {
	return &ghapi.Run{DisplayTitle: "t"}, nil
}
func (f *fakeSrc) Jobs(_ context.Context, _, _ string, id int64) ([]ghapi.Job, error) {
	f.calls++
	return f.jobs[id], f.err
}
func (f *fakeSrc) LogTail(context.Context, string, string, int64, int) ([]string, error) {
	return []string{"tail"}, f.logErr
}
func (f *fakeSrc) Status() ghapi.Status { return ghapi.Status{Enabled: true} }

func TestEnricherBacksOffWhenDenied(t *testing.T) {
	tr := NewTracker()
	b := base(0)
	b.OwnerName, b.RepositoryName, b.WorkflowRunID = "o", "r", 5
	tr.Observe(&scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{RunnerName: "x", JobMessageBase: b}}})
	src := &fakeSrc{err: &ghapi.APIError{Status: 403, Msg: "no"}}
	e := &Enricher{Src: src, Tracker: tr, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	e.Once(context.Background())
	e.Once(context.Background())
	if src.calls != 1 {
		t.Fatalf("asked GitHub %d times; a denied repo should be left alone for a while", src.calls)
	}
	if tr.Snapshot().Running[0].Detail != nil {
		t.Fatal("no detail expected when denied")
	}
}
