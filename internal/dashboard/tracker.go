// Package dashboard is a read-only status page for the scaler: which runners exist, which jobs are queued, running
// and recently finished. It holds state in memory only (a restart empties the history) and has no controls.
package dashboard

import (
	"sync"
	"time"

	"github.com/actions/scaleset"
)

// HistorySize is how many finished jobs are remembered.
const HistorySize = 100

// Job is one workflow job as the scale set reported it.
type Job struct {
	ID       int64     `json:"id"` // runner request id
	Repo     string    `json:"repo"`
	Name     string    `json:"name"`
	Workflow string    `json:"workflow"` // path of the workflow file
	Ref      string    `json:"ref"`
	Event    string    `json:"event"`
	RunID    int64     `json:"run_id"`
	Runner   string    `json:"runner,omitempty"`
	State    string    `json:"state"`            // queued, running, success, failure, cancelled, ...
	Queued   time.Time `json:"queued,omitempty"` // when GitHub queued it
	Started  time.Time `json:"started,omitempty"`
	Finished time.Time `json:"finished,omitempty"`
}

// Stats mirrors the scale set's own counters.
type Stats struct {
	Available  int `json:"available"`
	Acquired   int `json:"acquired"`
	Assigned   int `json:"assigned"`
	Running    int `json:"running"`
	Registered int `json:"registered_runners"`
	Busy       int `json:"busy_runners"`
	Idle       int `json:"idle_runners"`
}

// Tracker records job messages. The zero value is not usable; call NewTracker. A nil *Tracker ignores everything.
type Tracker struct {
	mu      sync.Mutex
	active  map[int64]*Job
	history []Job // newest first
	stats   Stats
	updated time.Time
	now     func() time.Time
}

func NewTracker() *Tracker { return &Tracker{active: map[int64]*Job{}, now: time.Now} }

// Observe records one scale set message.
func (t *Tracker) Observe(msg *scaleset.RunnerScaleSetMessage) {
	if t == nil || msg == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.updated = t.now()
	if s := msg.Statistics; s != nil {
		t.stats = Stats{s.TotalAvailableJobs, s.TotalAcquiredJobs, s.TotalAssignedJobs, s.TotalRunningJobs,
			s.TotalRegisteredRunners, s.TotalBusyRunners, s.TotalIdleRunners}
	}
	for _, j := range msg.JobAvailableMessages {
		t.upsert(&j.JobMessageBase, "queued", "")
	}
	for _, j := range msg.JobAssignedMessages {
		t.upsert(&j.JobMessageBase, "queued", "")
	}
	for _, j := range msg.JobStartedMessages {
		t.upsert(&j.JobMessageBase, "running", j.RunnerName)
	}
	for _, j := range msg.JobCompletedMessages {
		job := t.upsert(&j.JobMessageBase, j.Result, j.RunnerName)
		if job.Finished.IsZero() {
			job.Finished = t.now()
		}
		delete(t.active, job.ID)
		t.history = append([]Job{*job}, t.history...)
		if len(t.history) > HistorySize {
			t.history = t.history[:HistorySize]
		}
	}
}

// upsert updates the active job for a request, creating it if needed. A job already running is not moved back to
// queued by a late "assigned" message.
func (t *Tracker) upsert(b *scaleset.JobMessageBase, state, runner string) *Job {
	j := t.active[b.RunnerRequestID]
	if j == nil {
		j = &Job{ID: b.RunnerRequestID}
		t.active[j.ID] = j
	}
	j.Repo = b.OwnerName + "/" + b.RepositoryName
	j.Name = b.JobDisplayName
	j.Workflow, j.Ref = splitRef(b.JobWorkflowRef, j.Repo)
	j.Event = b.EventName
	j.RunID = b.WorkflowRunID
	j.Queued = b.QueueTime
	if runner != "" {
		j.Runner = runner
	}
	if !(j.State == "running" && state == "queued") {
		j.State = state
	}
	if state == "running" && j.Started.IsZero() {
		j.Started = t.now()
		if !b.RunnerAssignTime.IsZero() {
			j.Started = b.RunnerAssignTime
		}
	}
	if !b.FinishTime.IsZero() {
		j.Finished = b.FinishTime
	}
	return j
}

// splitRef turns "owner/repo/.github/workflows/ci.yml@refs/heads/main" into the workflow path and the ref.
func splitRef(wfRef, repo string) (workflow, ref string) {
	for i := 0; i < len(wfRef); i++ {
		if wfRef[i] == '@' {
			workflow, ref = wfRef[:i], wfRef[i+1:]
			break
		}
	}
	if workflow == "" {
		return wfRef, ""
	}
	if len(workflow) > len(repo)+1 && workflow[:len(repo)] == repo {
		workflow = workflow[len(repo)+1:]
	}
	return workflow, ref
}

// Snapshot is a copy of the tracker's state.
type Snapshot struct {
	Stats   Stats     `json:"stats"`
	Queued  []Job     `json:"queued"`
	Running []Job     `json:"running"`
	Recent  []Job     `json:"recent"`
	Updated time.Time `json:"updated,omitempty"`
}

func (t *Tracker) Snapshot() Snapshot {
	s := Snapshot{Queued: []Job{}, Running: []Job{}, Recent: []Job{}}
	if t == nil {
		return s
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s.Stats, s.Updated = t.stats, t.updated
	for _, j := range t.active {
		if j.State == "running" {
			s.Running = append(s.Running, *j)
		} else {
			s.Queued = append(s.Queued, *j)
		}
	}
	sortJobs(s.Queued)
	sortJobs(s.Running)
	s.Recent = append(s.Recent, t.history...)
	return s
}

func sortJobs(js []Job) {
	for i := 1; i < len(js); i++ { // tiny lists: insertion sort, oldest queue time first
		for k := i; k > 0 && js[k].Queued.Before(js[k-1].Queued); k-- {
			js[k], js[k-1] = js[k-1], js[k]
		}
	}
}
