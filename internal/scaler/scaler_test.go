package scaler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/actions/scaleset"

	"github.com/MacLeodTech/runner-scaling/internal/api"
)

type fakeGuard struct {
	slots   map[int]api.Slot
	failing bool
	kept    []string // runners whose caches were vouched for
}

func (g *fakeGuard) Info(context.Context) (api.Info, error) { return api.Info{}, nil }
func (g *fakeGuard) Slots(context.Context) ([]api.Slot, error) {
	var out []api.Slot
	for _, s := range g.slots {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b api.Slot) int { return a.Slot - b.Slot })
	return out, nil
}
func (g *fakeGuard) Start(_ context.Context, n int, r api.StartRequest) error {
	if g.failing {
		return errors.New("boom")
	}
	g.slots[n] = api.Slot{Slot: n, RunnerName: r.RunnerName, RunnerState: "running"}
	return nil
}
func (g *fakeGuard) KeepCache(_ context.Context, _ int, runner string) error {
	g.kept = append(g.kept, runner)
	return nil
}
func (g *fakeGuard) Stop(_ context.Context, n int) error { delete(g.slots, n); return nil }

type fakeGitHub struct {
	nextID   int
	removed  []int64
	busyIDs  map[int64]bool
	byName   map[string]int
	acquired []int64
}

func (f *fakeGitHub) GenerateJitRunnerConfig(_ context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, _ int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	f.nextID++
	f.byName[s.Name] = f.nextID
	return &scaleset.RunnerScaleSetJitRunnerConfig{Runner: &scaleset.RunnerReference{ID: f.nextID, Name: s.Name}, EncodedJITConfig: "jit"}, nil
}
func (f *fakeGitHub) GetRunnerByName(_ context.Context, n string) (*scaleset.RunnerReference, error) {
	id, ok := f.byName[n]
	if !ok {
		return nil, nil
	}
	return &scaleset.RunnerReference{ID: id, Name: n}, nil
}
func (f *fakeGitHub) RemoveRunner(_ context.Context, id int64) error {
	if f.busyIDs[id] {
		return errors.New("runner is busy")
	}
	f.removed = append(f.removed, id)
	return nil
}
func (f *fakeGitHub) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	f.acquired = append(f.acquired, ids...)
	return ids, nil
}

func newScaler(max int) (*Scaler, *fakeGuard, *fakeGitHub) {
	g := &fakeGuard{slots: map[int]api.Slot{}}
	gh := &fakeGitHub{busyIDs: map[int64]bool{}, byName: map[string]int{}}
	return &Scaler{
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Guard: g, GitHub: gh, Session: gh,
		Info: api.Info{Prefix: "ci", MaxSlots: max, WorkDir: "/tmp/runner"}, Max: max,
	}, g, gh
}

func stats(assigned int) *scaleset.RunnerScaleSetMessage {
	return &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: assigned}}
}

func TestScalesUpToMaxAndNames(t *testing.T) {
	s, g, gh := newScaler(3)
	msg := stats(5)
	msg.JobAvailableMessages = []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 7}}}
	if err := s.Scale(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if len(g.slots) != 3 {
		t.Fatalf("want 3 slots, got %v", g.slots)
	}
	for n, sl := range g.slots {
		if !strings.HasPrefix(sl.RunnerName, "ci-"+string(rune('0'+n))+"-") {
			t.Errorf("slot %d runner %q", n, sl.RunnerName)
		}
	}
	if !slices.Equal(gh.acquired, []int64{7}) {
		t.Errorf("acquired %v", gh.acquired)
	}
}

func TestScalesDownOnlyIdle(t *testing.T) {
	s, g, gh := newScaler(3)
	ctx := context.Background()
	s.Scale(ctx, stats(3))
	busyName := g.slots[2].RunnerName
	s.Scale(ctx, &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{RunnerName: busyName}}})

	// One idle runner just took a job that we haven't heard about: GitHub refuses to remove it.
	gh.busyIDs[int64(gh.byName[g.slots[3].RunnerName])] = true

	s.Scale(ctx, stats(0))
	if _, ok := g.slots[1]; ok {
		t.Error("idle slot 1 kept")
	}
	if _, ok := g.slots[2]; !ok {
		t.Error("busy slot 2 removed")
	}
	if _, ok := g.slots[3]; !ok {
		t.Error("slot 3 removed although GitHub said it was busy")
	}
}

func TestJobCompletedFreesSlotAndRefills(t *testing.T) {
	s, g, _ := newScaler(2)
	ctx := context.Background()
	s.Scale(ctx, stats(1))
	name := g.slots[1].RunnerName
	msg := stats(1) // another job is queued behind it
	msg.JobCompletedMessages = []*scaleset.JobCompleted{{RunnerName: name}}
	s.Scale(ctx, msg)
	if len(g.slots) != 1 || g.slots[1].RunnerName == name {
		t.Fatalf("want a fresh runner in slot 1, got %v", g.slots)
	}
}

func TestFailedStartRemovesRegistration(t *testing.T) {
	s, g, gh := newScaler(2)
	g.failing = true
	if err := s.Scale(context.Background(), stats(1)); err != nil {
		t.Fatal(err)
	}
	if len(gh.removed) != 1 {
		t.Fatalf("registration not cleaned up: %v", gh.removed)
	}
}

func TestNilMessageReconverges(t *testing.T) {
	s, g, _ := newScaler(2)
	ctx := context.Background()
	s.Scale(ctx, stats(2))
	delete(g.slots, 1) // reaped by the guard
	s.Scale(ctx, nil)
	if len(g.slots) != 2 {
		t.Fatalf("not refilled: %v", g.slots)
	}
}

func TestTrustedJobsKeepCaches(t *testing.T) {
	cases := []struct {
		name, event, ref string
		refs             []string
		keep             bool
	}{
		{"push to main", "push", "refs/heads/main", []string{"refs/heads/main"}, true},
		{"schedule on main", "schedule", "refs/heads/main", []string{"refs/heads/main"}, true},
		{"push to a branch", "push", "refs/heads/feature", []string{"refs/heads/main"}, false},
		{"pull request", "pull_request", "refs/pull/3/merge", []string{"refs/heads/main"}, false},
		{"pull_request_target runs on main", "pull_request_target", "refs/heads/main", []string{"refs/heads/main"}, false},
		{"workflow_run on main", "workflow_run", "refs/heads/main", []string{"refs/heads/main"}, false},
		{"nothing trusted", "push", "refs/heads/main", nil, false},
		{"tag push", "push", "refs/tags/v1", []string{"refs/heads/main"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, g, _ := newScaler(2)
			s.TrustedRefs = c.refs
			if err := s.Scale(context.Background(), stats(1)); err != nil {
				t.Fatal(err)
			}
			name := g.slots[1].RunnerName
			started := &scaleset.JobStarted{RunnerName: name, JobMessageBase: scaleset.JobMessageBase{
				EventName: c.event, JobWorkflowRef: "example-org/app/.github/workflows/rust.yml@" + c.ref,
			}}
			if err := s.Scale(context.Background(), &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{started}}); err != nil {
				t.Fatal(err)
			}
			if got := slices.Contains(g.kept, name); got != c.keep {
				t.Errorf("kept=%v (%v), want %v", got, g.kept, c.keep)
			}
		})
	}
}

func TestIdleRunnerKeepsCachesWhenRemoved(t *testing.T) {
	s, g, _ := newScaler(2)
	if err := s.Scale(context.Background(), stats(2)); err != nil {
		t.Fatal(err)
	}
	if err := s.Scale(context.Background(), stats(1)); err != nil {
		t.Fatal(err)
	}
	if len(g.slots) != 1 || len(g.kept) != 1 {
		t.Fatalf("slots=%v kept=%v: want one idle runner removed with its caches kept", g.slots, g.kept)
	}
}
