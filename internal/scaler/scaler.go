// Package scaler turns runner scale set messages from GitHub into guard calls. It never touches Docker.
package scaler

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/MacLeodTech/runner-scaling/internal/api"
	"github.com/MacLeodTech/runner-scaling/internal/dashboard"
)

// GitHub is the part of *scaleset.Client the scaler uses.
type GitHub interface {
	GenerateJitRunnerConfig(ctx context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	GetRunnerByName(ctx context.Context, name string) (*scaleset.RunnerReference, error)
	RemoveRunner(ctx context.Context, runnerID int64) error
}

// JobAcquirer is the part of the message session the scaler uses.
type JobAcquirer interface {
	AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error)
}

type Scaler struct {
	Log        *slog.Logger
	Guard      Guard
	GitHub     GitHub
	Session    JobAcquirer
	ScaleSetID int
	Info       api.Info // from the guard: prefix, slot count, work dir
	Min, Max   int
	// TrustedRefs are the git refs (e.g. refs/heads/main) whose push, schedule, dispatch and release jobs may reuse
	// and leave behind the slot's warm caches. Every other job's caches are wiped by the guard. Empty: wipe always.
	TrustedRefs []string
	// Dash, if set, records every message for the dashboard. It never influences scaling.
	Dash *dashboard.Tracker
	// RetryDelay is how long to wait before retrying a failed start without a new message from GitHub (default 15s).
	RetryDelay time.Duration

	mu           sync.Mutex // serialises Scale and the retry timer
	retry        *time.Timer
	busy         map[string]bool // runners that have started a job
	lastAssigned int
}

var _ listener.Scaler = (*Scaler)(nil)

// Scale handles one poll. It only returns an error when the guard can't be reached, which stops the listener (and
// the process, so the container restarts). Everything else is logged and retried on the next poll.
func (s *Scaler) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scale(ctx, msg)
}

func (s *Scaler) scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	if s.busy == nil {
		s.busy = map[string]bool{}
	}
	s.Dash.Observe(msg)
	if msg == nil {
		return s.converge(ctx, s.lastAssigned)
	}
	if len(msg.JobAvailableMessages) > 0 {
		ids := make([]int64, 0, len(msg.JobAvailableMessages))
		for _, j := range msg.JobAvailableMessages {
			ids = append(ids, j.RunnerRequestID)
		}
		if _, err := s.Session.AcquireJobs(ctx, ids); err != nil {
			return fmt.Errorf("acquire jobs: %w", err)
		}
	}
	for _, j := range msg.JobStartedMessages {
		s.Log.Info("job started", "runner", j.RunnerName, "repo", j.OwnerName+"/"+j.RepositoryName, "job", j.JobDisplayName)
		s.busy[j.RunnerName] = true
		if s.trusted(&j.JobMessageBase) {
			if err := s.keepCache(ctx, j.RunnerName); err != nil {
				return err
			}
		}
	}
	if len(msg.JobCompletedMessages) > 0 {
		slots, err := s.Guard.Slots(ctx)
		if err != nil {
			return err
		}
		for _, j := range msg.JobCompletedMessages {
			s.Log.Info("job completed", "runner", j.RunnerName, "result", j.Result, "repo", j.OwnerName+"/"+j.RepositoryName)
			delete(s.busy, j.RunnerName)
			// The runner exits by itself after its one job and the guard reaps it; stopping here just frees the
			// slot sooner. A job that was cancelled before it reached a runner has no runner name.
			for _, sl := range slots {
				if j.RunnerName != "" && sl.RunnerName == j.RunnerName {
					s.stopAsync(ctx, sl.Slot)
				}
			}
		}
	}
	if msg.Statistics != nil {
		s.lastAssigned = msg.Statistics.TotalAssignedJobs
	}
	return s.converge(ctx, s.lastAssigned)
}

// trustedEvents are the triggers that only someone with write access to the repo can fire. pull_request,
// pull_request_target, issue_comment, workflow_run and the like can carry content from elsewhere.
var trustedEvents = []string{"push", "schedule", "workflow_dispatch", "release"}

// trusted reports whether a job is a trusted event on a trusted ref. The ref comes from job_workflow_ref
// (owner/repo/.github/workflows/x.yml@refs/heads/main); for a reusable workflow that is the called workflow's ref,
// not the caller's, so a push to another branch that calls a reusable workflow at main counts as trusted.
func (s *Scaler) trusted(j *scaleset.JobMessageBase) bool {
	_, ref, ok := strings.Cut(j.JobWorkflowRef, "@")
	return ok && slices.Contains(s.TrustedRefs, ref) && slices.Contains(trustedEvents, j.EventName)
}

// keepCache asks the guard to leave the caches of the slot running runner alone when it is torn down. A guard that
// can't be reached is an error; a refusal (the runner already left) only means the caches get wiped.
func (s *Scaler) keepCache(ctx context.Context, runner string) error {
	slots, err := s.Guard.Slots(ctx)
	if err != nil {
		return err
	}
	for _, sl := range slots {
		if sl.RunnerName == runner {
			if err := s.Guard.KeepCache(ctx, sl.Slot, runner); err != nil {
				s.Log.Warn("keep cache", "slot", sl.Slot, "runner", runner, "err", err)
			}
		}
	}
	return nil
}

func (s *Scaler) converge(ctx context.Context, assigned int) error {
	slots, err := s.Guard.Slots(ctx)
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for _, sl := range slots {
		live[sl.RunnerName] = true
	}
	for name := range s.busy {
		if !live[name] { // reaped without a JobCompleted reaching us
			delete(s.busy, name)
		}
	}
	target := min(s.Max, s.Min+assigned)
	current := len(slots)
	switch {
	case target > current:
		used := map[int]bool{}
		for _, sl := range slots {
			used[sl.Slot] = true
		}
		need := target - current
		s.Log.Info("scaling up", "current", current, "target", target)
		var free []int
		for n := 1; n <= s.Info.MaxSlots && len(free) < need; n++ {
			if !used[n] {
				free = append(free, n)
			}
		}
		// Each start waits for dind to be healthy, so run them side by side.
		var wg sync.WaitGroup
		var failed atomic.Bool
		for _, n := range free {
			wg.Go(func() {
				if err := s.startSlot(ctx, n); err != nil {
					s.Log.Error("start slot", "slot", n, "err", err)
					failed.Store(true)
				}
			})
		}
		wg.Wait()
		if failed.Load() {
			s.scheduleRetry(ctx)
		}
	case target < current:
		// Only idle runners go: one that is mid-job finishes, exits and is reaped.
		excess := current - target
		for _, sl := range slots {
			if excess == 0 {
				break
			}
			if sl.Starting || sl.RunnerName == "" || s.busy[sl.RunnerName] {
				continue
			}
			if s.removeIdle(ctx, sl) {
				excess--
			}
		}
	}
	return nil
}

// stopAsync frees a slot in the background: the guard's wipe and prune can take minutes and Scale shouldn't wait
// for them. The slot counts as occupied until it is gone, so converge runs again once the stop has finished.
func (s *Scaler) stopAsync(ctx context.Context, slot int) {
	go func() {
		if err := s.Guard.Stop(ctx, slot); err != nil {
			s.Log.Warn("stop slot", "slot", slot, "err", err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if err := s.converge(ctx, s.lastAssigned); err != nil {
			s.Log.Warn("converge after stop", "err", err) // the next poll hits the same error and stops the listener
		}
	}()
}

// scheduleRetry converges again after RetryDelay, so a failed start doesn't wait for the next message from GitHub
// (which can be close to a minute when idle). Callers hold s.mu. At most one retry is pending.
func (s *Scaler) scheduleRetry(ctx context.Context) {
	if s.retry != nil {
		return
	}
	d := s.RetryDelay
	if d <= 0 {
		d = 15 * time.Second
	}
	s.retry = time.AfterFunc(d, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.retry = nil
		if ctx.Err() != nil {
			return
		}
		if err := s.converge(ctx, s.lastAssigned); err != nil {
			s.Log.Warn("retry after failed start", "err", err) // the next poll hits the same error and stops the listener
		}
	})
}

func (s *Scaler) startSlot(ctx context.Context, n int) error {
	name := s.Info.Prefix + "-" + strconv.Itoa(n) + "-" + randSuffix()
	jit, err := s.GitHub.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name, WorkFolder: s.Info.WorkDir}, s.ScaleSetID)
	if err != nil {
		return fmt.Errorf("jit config: %w", err)
	}
	if err := s.Guard.Start(ctx, n, api.StartRequest{RunnerName: name, JITConfig: jit.EncodedJITConfig}); err != nil {
		// Don't leave an offline registration behind.
		if jit.Runner != nil {
			if rerr := s.GitHub.RemoveRunner(context.WithoutCancel(ctx), int64(jit.Runner.ID)); rerr != nil {
				s.Log.Warn("remove unused registration", "runner", name, "err", rerr)
			}
		}
		return err
	}
	s.Log.Info("runner started", "slot", n, "runner", name)
	return nil
}

// removeIdle deregisters an idle runner and then frees its slot. GitHub refuses to remove a runner that has just
// picked up a job, in which case the runner is left alone.
func (s *Scaler) removeIdle(ctx context.Context, sl api.Slot) bool {
	ref, err := s.GitHub.GetRunnerByName(ctx, sl.RunnerName)
	if err != nil {
		s.Log.Warn("look up idle runner", "runner", sl.RunnerName, "err", err)
		return false
	}
	if ref != nil {
		if err := s.GitHub.RemoveRunner(ctx, int64(ref.ID)); err != nil {
			s.Log.Info("idle runner not removed (probably took a job)", "runner", sl.RunnerName, "err", err)
			return false
		}
	}
	// It never ran a job, so its caches are as clean as it found them.
	if err := s.Guard.KeepCache(ctx, sl.Slot, sl.RunnerName); err != nil {
		s.Log.Warn("keep cache of idle runner", "slot", sl.Slot, "err", err)
	}
	if err := s.Guard.Stop(ctx, sl.Slot); err != nil {
		s.Log.Warn("stop slot", "slot", sl.Slot, "err", err)
		return false
	}
	s.Log.Info("scaled down idle runner", "slot", sl.Slot, "runner", sl.RunnerName)
	return true
}

func randSuffix() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
