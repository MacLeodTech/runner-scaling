package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/MacLeodTech/runner-scaling/internal/api"
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

func (f fakeSlots) Slots(context.Context) ([]api.Slot, error) {
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
