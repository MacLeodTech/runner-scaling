package dashboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/MacLeodTech/runner-scaling/internal/api"
	"github.com/MacLeodTech/runner-scaling/internal/ghapi"
)

// Source is the part of the GitHub client the enricher needs.
type Source interface {
	Run(ctx context.Context, owner, repo string, runID int64) (*ghapi.Run, error)
	Jobs(ctx context.Context, owner, repo string, runID int64) ([]ghapi.Job, error)
	LogTail(ctx context.Context, owner, repo string, jobID int64, n int) ([]string, error)
	Status() ghapi.Status
}

// Enricher polls GitHub for the runs the tracker knows about and attaches steps and run details to the jobs.
type Enricher struct {
	Src         Source
	Tracker     *Tracker
	Every       time.Duration // poll interval; default 20s
	FailureLogs bool          // fetch the log tail of failed jobs
	Log         *slog.Logger

	mu      sync.Mutex
	runs    map[int64]*ghapi.Run // run details barely change; fetched once while the run is tracked
	backoff map[string]time.Time // repo -> do not ask again before
	warned  map[string]bool
}

const (
	maxRunsPerPoll = 20
	deniedBackoff  = 5 * time.Minute
	logTailLines   = 40
)

// Status is the GitHub client's health for the dashboard; Enabled is false when there is no enricher.
func (e *Enricher) Status() ghapi.Status {
	if e == nil || e.Src == nil {
		return ghapi.Status{}
	}
	return e.Src.Status()
}

// Loop polls until ctx ends.
func (e *Enricher) Loop(ctx context.Context) {
	every := e.Every
	if every <= 0 {
		every = 20 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		e.Once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once does one poll.
func (e *Enricher) Once(ctx context.Context) {
	if e.runs == nil {
		e.runs, e.backoff, e.warned = map[int64]*ghapi.Run{}, map[string]time.Time{}, map[string]bool{}
	}
	now := time.Now()
	refs := e.Tracker.Wanted()
	if len(refs) > maxRunsPerPoll {
		refs = refs[:maxRunsPerPoll]
	}
	live := map[int64]bool{}
	for _, ref := range refs {
		live[ref.RunID] = true
		repo := ref.Owner + "/" + ref.Repo
		if now.Before(e.backoff[repo]) {
			continue
		}
		jobs, err := e.Src.Jobs(ctx, ref.Owner, ref.Repo, ref.RunID)
		if err != nil {
			if errors.Is(err, ghapi.ErrRateLow) {
				e.warnOnce("rate", "GitHub rate limit nearly used up; pausing job detail until it resets")
				return
			}
			var ae *ghapi.APIError
			if errors.As(err, &ae) && ae.Denied() {
				e.backoff[repo] = now.Add(deniedBackoff)
				e.warnOnce("denied:"+repo, "no job detail for "+repo+": the GitHub credential cannot read its workflow runs (needs Actions: read, and the repo selected for the App)")
			} else if ctx.Err() == nil {
				e.Log.Warn("job detail poll failed", "repo", repo, "err", err)
			}
			continue
		}
		delete(e.warned, "denied:"+repo)
		e.mu.Lock()
		run := e.runs[ref.RunID]
		e.mu.Unlock()
		if run == nil {
			if r, err := e.Src.Run(ctx, ref.Owner, ref.Repo, ref.RunID); err == nil {
				run = r
				e.mu.Lock()
				e.runs[ref.RunID] = r
				e.mu.Unlock()
			}
		}
		e.Tracker.ApplyRun(ref, run, jobs, now)
	}
	e.mu.Lock()
	for id := range e.runs { // forget runs that are no longer tracked
		if !live[id] {
			delete(e.runs, id)
		}
	}
	e.mu.Unlock()
	if e.FailureLogs {
		for _, j := range e.Tracker.NeedsLog() {
			owner, repo := splitRepo(j.Repo)
			lines, err := e.Src.LogTail(ctx, owner, repo, j.Detail.GitHubJobID, logTailLines)
			if err != nil {
				if errors.Is(err, ghapi.ErrRateLow) {
					return
				}
				lines = []string{} // do not retry a log that cannot be had
			}
			e.Tracker.SetLogTail(j.Detail.GitHubJobID, lines)
		}
	}
}

func (e *Enricher) warnOnce(key, msg string) {
	if e.warned[key] {
		return
	}
	e.warned[key] = true
	e.Log.Warn(msg)
}

func splitRepo(r string) (string, string) {
	for i := 0; i < len(r); i++ {
		if r[i] == '/' {
			return r[:i], r[i+1:]
		}
	}
	return r, ""
}

// DefaultStallAfter is how long a step may run with an idle runner before the job is flagged.
const DefaultStallAfter = 10 * time.Minute

// stallCPU is the CPU percentage (of one core) below which a runner counts as idle.
const stallCPU = 5.0

// MarkStalled flags running jobs whose current step has run longer than after while the runner's containers use
// almost no CPU: typically a download or a lock that is not going anywhere. It only sees what GitHub reported, so a
// job without step detail is never flagged.
func MarkStalled(running []Job, slots []api.Slot, now time.Time, after time.Duration) int {
	if after <= 0 {
		after = DefaultStallAfter
	}
	byRunner := map[string]api.Slot{}
	for _, s := range slots {
		if s.RunnerName != "" {
			byRunner[s.RunnerName] = s
		}
	}
	n := 0
	for i := range running {
		j := &running[i]
		d := j.Detail
		if d == nil || d.Step == "" || d.StepStarted.IsZero() {
			continue
		}
		s, ok := byRunner[j.Runner]
		if !ok || s.Stats == nil {
			continue
		}
		if in := now.Sub(d.StepStarted); in > after && s.Stats.CPUPercent < stallCPU {
			j.Stalled = true
			j.StallNote = "step \"" + d.Step + "\" has run " + in.Round(time.Second).String() + " with the runner at " + pct(s.Stats.CPUPercent) + " CPU"
			n++
		}
	}
	return n
}

func pct(f float64) string { return fmt.Sprintf("%.0f%%", f) }
