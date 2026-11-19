// Package dashboard is a read-only status page for the scaler: which runners exist, which jobs are queued, running
// and recently finished. It holds state in memory only (a restart empties the history) and has no controls.
package dashboard

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MacLeodTech/runner-scaling/internal/ghapi"
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
	// Detail is what GitHub's REST API says about the job (steps, who triggered it, ...); nil until fetched or when the
	// credential can't read Actions.
	Detail *Detail `json:"detail,omitempty"`
	// Stalled is set when the job has been in one step for a long time while its runner does almost nothing.
	Stalled   bool   `json:"stalled,omitempty"`
	StallNote string `json:"stall_note,omitempty"`
}

// Step is one step of a job.
type Step struct {
	Number     int       `json:"number"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion,omitempty"`
	Started    time.Time `json:"started,omitempty"`
	Completed  time.Time `json:"completed,omitempty"`
}

// Detail is the GitHub side of a job. A Detail is replaced, never changed in place, so snapshots may share it.
type Detail struct {
	GitHubJobID int64     `json:"github_job_id,omitempty"`
	URL         string    `json:"url,omitempty"` // the job's page on GitHub
	RunURL      string    `json:"run_url,omitempty"`
	Title       string    `json:"title,omitempty"` // commit message or PR title of the run
	Actor       string    `json:"actor,omitempty"`
	Branch      string    `json:"branch,omitempty"`
	SHA         string    `json:"sha,omitempty"`
	PRs         []int     `json:"prs,omitempty"`
	Attempt     int       `json:"attempt,omitempty"`
	Labels      []string  `json:"labels,omitempty"`
	Steps       []Step    `json:"steps,omitempty"`
	Step        string    `json:"step,omitempty"` // the step in progress
	StepNumber  int       `json:"step_number,omitempty"`
	StepsTotal  int       `json:"steps_total,omitempty"`
	StepStarted time.Time `json:"step_started,omitempty"`
	Conclusion  string    `json:"conclusion,omitempty"`
	FailedStep  string    `json:"failed_step,omitempty"`
	LogTail     []string  `json:"log_tail,omitempty"`
	Final       bool      `json:"final,omitempty"` // GitHub reported the job completed; no more polling
	Fetched     time.Time `json:"fetched,omitempty"`
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
	active  map[string]*Job
	history []Job // newest first
	stats   Stats
	updated time.Time
	now     func() time.Time
	metrics *metrics
}

func NewTracker() *Tracker {
	return &Tracker{active: map[string]*Job{}, now: time.Now, metrics: newMetrics()}
}

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
		t.metrics.mu.Lock()
		t.metrics.completed[j.Result]++
		if !job.Started.IsZero() && job.Finished.After(job.Started) {
			t.metrics.jobDuration.observe(job.Finished.Sub(job.Started).Seconds())
		}
		t.metrics.mu.Unlock()
		delete(t.active, jobKey(&j.JobMessageBase))
		t.history = append([]Job{*job}, t.history...)
		if len(t.history) > HistorySize {
			t.history = t.history[:HistorySize]
		}
	}
}

// upsert updates the active job for a request, creating it if needed. A job already running is not moved back to
// queued by a late "assigned" message.
func (t *Tracker) upsert(b *scaleset.JobMessageBase, state, runner string) *Job {
	key := jobKey(b)
	j := t.active[key]
	if j == nil {
		j = &Job{ID: b.RunnerRequestID}
		t.active[key] = j
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
		if !j.Queued.IsZero() && j.Started.After(j.Queued) {
			t.metrics.mu.Lock()
			t.metrics.startLatency.observe(j.Started.Sub(j.Queued).Seconds())
			t.metrics.mu.Unlock()
		}
	}
	if !b.FinishTime.IsZero() {
		j.Finished = b.FinishTime
	}
	return j
}

// jobKey identifies one job across its messages. GitHub's runnerRequestId has been seen as 0 on every message of a live
// scale set, which collapsed all jobs into one entry, so it is only trusted when set; then the job id; then the
// repo, run and display name, which are the same on every message of a job.
func jobKey(b *scaleset.JobMessageBase) string {
	switch {
	case b.RunnerRequestID != 0:
		return "r" + strconv.FormatInt(b.RunnerRequestID, 10)
	case b.JobID != "":
		return "j" + b.JobID
	}
	return "n" + b.OwnerName + "/" + b.RepositoryName + "#" + strconv.FormatInt(b.WorkflowRunID, 10) + "/" + b.JobDisplayName
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

// RunRef names a workflow run to look up.
type RunRef struct {
	Owner, Repo string
	RunID       int64
}

// finalWindow is how long after a job ends its final steps are still fetched.
const finalWindow = 15 * time.Minute

// Wanted lists the runs worth asking GitHub about: those with a queued or running job, and those with a job that
// finished recently and has no final detail yet.
func (t *Tracker) Wanted() []RunRef {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	seen := map[RunRef]bool{}
	var out []RunRef
	add := func(j *Job) {
		owner, repo, ok := strings.Cut(j.Repo, "/")
		if !ok || j.RunID == 0 {
			return
		}
		r := RunRef{owner, repo, j.RunID}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	for _, j := range t.active {
		add(j)
	}
	now := t.now()
	for i := range t.history {
		j := &t.history[i]
		if (j.Detail == nil || !j.Detail.Final) && now.Sub(j.Finished) < finalWindow {
			add(j)
		}
	}
	return out
}

// ApplyRun attaches what GitHub says about run (and its jobs) to the matching tracked jobs. A running or finished job
// is matched by its runner's name, a queued one by its display name.
func (t *Tracker) ApplyRun(ref RunRef, run *ghapi.Run, jobs []ghapi.Job, now time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	repo := ref.Owner + "/" + ref.Repo
	find := func(j *Job) *ghapi.Job {
		var byName *ghapi.Job
		n := 0
		for i := range jobs {
			g := &jobs[i]
			if j.Runner != "" && g.RunnerName == j.Runner {
				return g
			}
			if g.Name == j.Name {
				byName = g
				n++
			}
		}
		if j.Runner == "" && n == 1 {
			return byName
		}
		return nil
	}
	apply := func(j *Job) {
		if j.Repo != repo || j.RunID != ref.RunID {
			return
		}
		g := find(j)
		if g == nil {
			return
		}
		d := &Detail{GitHubJobID: g.ID, URL: g.HTMLURL, Attempt: g.RunAttempt, Labels: g.Labels, SHA: g.HeadSHA, Branch: g.HeadBranch,
			Conclusion: g.Conclusion, Final: g.Status == "completed", Fetched: now}
		if run != nil {
			d.RunURL, d.Title, d.Actor = run.HTMLURL, run.DisplayTitle, run.TriggeringActor.Login
			if d.Actor == "" {
				d.Actor = run.Actor.Login
			}
			for _, p := range run.PullRequests {
				d.PRs = append(d.PRs, p.Number)
			}
		}
		for _, st := range g.Steps {
			d.Steps = append(d.Steps, Step{st.Number, st.Name, st.Status, st.Conclusion, st.StartedAt, st.CompletedAt})
			if st.Status == "in_progress" && d.Step == "" {
				d.Step, d.StepNumber, d.StepStarted = st.Name, st.Number, st.StartedAt
			}
			if st.Conclusion == "failure" && d.FailedStep == "" {
				d.FailedStep = st.Name
			}
		}
		d.StepsTotal = len(g.Steps)
		if old := j.Detail; old != nil && old.GitHubJobID == d.GitHubJobID {
			d.LogTail = old.LogTail
		}
		j.Detail = d
	}
	for _, j := range t.active {
		apply(j)
	}
	for i := range t.history {
		apply(&t.history[i])
	}
}

// NeedsLog lists finished, failed jobs whose log tail has not been fetched.
func (t *Tracker) NeedsLog() []Job {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Job
	for _, j := range t.history {
		if d := j.Detail; d != nil && d.Final && d.Conclusion == "failure" && d.LogTail == nil && d.GitHubJobID != 0 {
			out = append(out, j)
		}
	}
	return out
}

// SetLogTail stores the log tail for the job with GitHub job id id. An empty tail is stored as "no output" so the job
// is not asked for again.
func (t *Tracker) SetLogTail(id int64, lines []string) {
	if t == nil {
		return
	}
	if lines == nil {
		lines = []string{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.history {
		if d := t.history[i].Detail; d != nil && d.GitHubJobID == id {
			c := *d
			c.LogTail = lines
			t.history[i].Detail = &c
		}
	}
}
