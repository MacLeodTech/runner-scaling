package guard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MacLeodTech/runner-scaling/internal/api"
	"github.com/MacLeodTech/runner-scaling/internal/jit"
)

const (
	labelInstance = "io.github.runner-scaling.instance"
	labelSlot     = "io.github.runner-scaling.slot"
	labelRole     = "io.github.runner-scaling.role"
	labelRunner   = "io.github.runner-scaling.runner-name"

	// jitEnv carries the JIT config into the runner container; the entrypoint wrapper unsets it before exec.
	jitEnv = "RUNNER_SCALING_JITCONFIG"
)

// entrypointWrapper moves the JIT config from the environment to the runner's command line, so job steps (which
// inherit the runner's environment) never see it.
const entrypointWrapper = `j="$` + jitEnv + `"; unset ` + jitEnv + `; exec "$@" --jitconfig "$j"`

// ErrBadRequest and ErrConflict map to 400 and 409.
var (
	ErrBadRequest = errors.New("bad request")
	ErrConflict   = errors.New("conflict")
	ErrNotReady   = errors.New("not ready")
)

// Guard creates and removes runner slots from its fixed templates.
type Guard struct {
	cfg *Config
	d   Docker
	log *slog.Logger

	admitMu    sync.Mutex // held across the memory-budget check and marking a slot as starting
	afterAdmit func()     // test seam: runs after the budget check passes, before the slot is claimed; nil in production
	mu         sync.Mutex
	starting   map[int]bool
	keepMu     sync.Mutex     // guards keepCache and cacheDirty
	keepCache  map[int]string // slot -> runner whose caches the scaler vouched for; consumed at teardown
	cacheDirty map[int]bool   // slots whose caches could not be emptied
	overMem    int            // consecutive reap ticks over memory_budget; only touched by Reap
	nameRE     *regexp.Regexp
}

func New(cfg *Config, d Docker, log *slog.Logger) *Guard {
	return &Guard{
		cfg: cfg, d: d, log: log,
		starting: map[int]bool{}, keepCache: map[int]string{}, cacheDirty: map[int]bool{},
		nameRE: regexp.MustCompile(`^` + regexp.QuoteMeta(cfg.Prefix) + `-([0-9]+)-[a-z0-9]{4,16}$`),
	}
}

func (g *Guard) Info() api.Info {
	return api.Info{Prefix: g.cfg.Prefix, MaxSlots: g.cfg.MaxSlots, WorkDir: g.cfg.WorkDir, GitHubURL: g.cfg.GitHubURL}
}

func (g *Guard) names(slot int) (runner, dind, network, volume string) {
	p, s := g.cfg.Prefix, strconv.Itoa(slot)
	return p + "-runner-" + s, p + "-dind-" + s, p + "-slot-" + s, p + "-work-" + s
}

// EnsureImages pulls any template image the daemon doesn't have yet.
func (g *Guard) EnsureImages(ctx context.Context) error {
	imgs := []string{g.cfg.Runner.Image}
	if g.cfg.Dind != nil {
		imgs = append(imgs, g.cfg.Dind.Image)
	}
	for _, img := range imgs {
		ok, err := g.d.ImageExists(ctx, img)
		if err != nil {
			return err
		}
		if !ok {
			g.log.Info("pulling image", "image", img)
			if err := g.d.ImagePull(ctx, img); err != nil {
				return fmt.Errorf("pull %s: %w", img, err)
			}
		}
	}
	return nil
}

// List reports every slot that has containers.
func (g *Guard) List(ctx context.Context) ([]api.Slot, error) {
	cs, err := g.d.ContainerList(ctx, labelInstance+"="+g.cfg.Prefix)
	if err != nil {
		return nil, err
	}
	by := map[int]*api.Slot{}
	for _, c := range cs {
		n, err := strconv.Atoi(c.Labels[labelSlot])
		if err != nil {
			continue
		}
		s := by[n]
		if s == nil {
			s = &api.Slot{Slot: n}
			by[n] = s
		}
		switch c.Labels[labelRole] {
		case "runner":
			s.RunnerName, s.RunnerState = c.Labels[labelRunner], c.State
		case "dind":
			s.DindState = c.State
		}
	}
	g.mu.Lock()
	for n := range g.starting {
		if by[n] == nil {
			by[n] = &api.Slot{Slot: n}
		}
		by[n].Starting = true
	}
	g.mu.Unlock()
	out := make([]api.Slot, 0, len(by))
	for _, k := range slices.Sorted(maps.Keys(by)) {
		out = append(out, *by[k])
	}
	return out, nil
}

// Start brings a slot up: network, fresh work volume, dind (waited on until healthy), then the runner.
func (g *Guard) Start(ctx context.Context, slot int, req api.StartRequest) error {
	if slot < 1 || slot > g.cfg.MaxSlots {
		return fmt.Errorf("%w: slot %d outside 1..%d", ErrBadRequest, slot, g.cfg.MaxSlots)
	}
	m := g.nameRE.FindStringSubmatch(req.RunnerName)
	if m == nil || m[1] != strconv.Itoa(slot) {
		return fmt.Errorf("%w: runner name %q must be %s-%d-<4-16 lowercase alphanumerics>", ErrBadRequest, req.RunnerName, g.cfg.Prefix, slot)
	}
	if err := jit.Validate(req.JITConfig, jit.Expect{
		TenantURL: g.cfg.TenantURL, RunnerName: req.RunnerName, WorkFolder: g.cfg.WorkDir, SharedHosts: g.cfg.SharedHosts,
	}); err != nil {
		g.log.Warn("refused jit config", "runner", req.RunnerName, "shape", jit.Shape(req.JITConfig))
		return fmt.Errorf("%w: %v", ErrBadRequest, err)
	}

	if g.cfg.RequireFile != "" {
		if _, err := os.Stat(g.cfg.RequireFile); err != nil {
			return fmt.Errorf("%w: %s is missing (is the firewall up?)", ErrNotReady, g.cfg.RequireFile)
		}
	}
	if err := g.reserveSlot(ctx, slot); err != nil {
		return err
	}
	defer func() { g.mu.Lock(); delete(g.starting, slot); g.mu.Unlock() }()

	ctx, cancel := context.WithTimeout(ctx, g.cfg.StartTimeout.Duration)
	defer cancel()
	g.keepMu.Lock()
	delete(g.keepCache, slot)
	dirty := g.cacheDirty[slot]
	g.keepMu.Unlock()
	if dirty {
		if err := g.wipeCaches(ctx, slot); err != nil {
			return fmt.Errorf("%w: caches of slot %d could not be emptied: %v", ErrNotReady, slot, err)
		}
	}
	if err := g.start(ctx, slot, req); err != nil {
		g.log.Error("slot start failed, tearing down", "slot", slot, "err", err)
		g.teardown(context.WithoutCancel(ctx), slot, false, false) // no job ran: the caches are as they were
		return err
	}
	g.log.Info("slot started", "slot", slot, "runner", req.RunnerName)
	return nil
}

// reserveSlot checks the memory budget and marks the slot as starting in one step. admitMu serialises it, so two
// concurrent starts on this guard can't both be admitted against the same free memory: the second one's admit sees
// the first one's slot as starting. (Two separate guards are covered by memoryBySlot counting created containers.)
func (g *Guard) reserveSlot(ctx context.Context, slot int) error {
	g.admitMu.Lock()
	defer g.admitMu.Unlock()
	if err := g.admit(ctx, slot); err != nil {
		return err
	}
	if g.afterAdmit != nil {
		g.afterAdmit()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.starting[slot] {
		return fmt.Errorf("%w: slot %d is starting", ErrConflict, slot)
	}
	busy, err := g.slotHasContainers(ctx, slot)
	if err != nil {
		return err
	}
	if busy {
		return fmt.Errorf("%w: slot %d is in use", ErrConflict, slot)
	}
	g.starting[slot] = true
	return nil
}

func (g *Guard) slotHasContainers(ctx context.Context, slot int) (bool, error) {
	cs, err := g.d.ContainerList(ctx, labelInstance+"="+g.cfg.Prefix)
	if err != nil {
		return false, err
	}
	for _, c := range cs {
		if c.Labels[labelSlot] == strconv.Itoa(slot) {
			return true, nil
		}
	}
	return false, nil
}

func (g *Guard) labels(slot int, role, runner string) map[string]string {
	l := map[string]string{}
	switch role {
	case "runner":
		maps.Copy(l, g.cfg.Runner.Labels)
	case "dind":
		maps.Copy(l, g.cfg.Dind.Labels)
	}
	maps.Copy(l, map[string]string{labelInstance: g.cfg.Prefix, labelSlot: strconv.Itoa(slot), labelRole: role})
	if runner != "" {
		l[labelRunner] = runner
	}
	return l
}

func (g *Guard) start(ctx context.Context, slot int, req api.StartRequest) error {
	runnerName, dindName, netName, volName := g.names(slot)
	subnet := ""
	if g.cfg.Subnet != "" {
		subnet = g.cfg.Expand(g.cfg.Subnet, slot)
	}
	if err := g.d.NetworkEnsure(ctx, netName, subnet, g.labels(slot, "network", "")); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	work := volName + ":" + g.cfg.WorkDir
	if g.cfg.WorkBind != "" {
		work = g.cfg.Expand(g.cfg.WorkBind, slot) + ":" + g.cfg.WorkDir
	} else if err := g.d.VolumeCreate(ctx, volName, g.labels(slot, "work", "")); err != nil {
		return fmt.Errorf("work volume: %w", err)
	}

	extraEnv := map[string]string{}
	if g.cfg.Dind != nil {
		spec := g.spec(g.cfg.Dind, slot, netName, work, map[string]string{"DOCKER_TLS_CERTDIR": ""})
		spec.Hostname = dindName
		spec.Labels = g.labels(slot, "dind", "")
		if err := g.run(ctx, dindName, spec); err != nil {
			return fmt.Errorf("dind: %w", err)
		}
		if err := g.waitHealthy(ctx, dindName); err != nil {
			return fmt.Errorf("dind: %w", err)
		}
		extraEnv["DOCKER_HOST"] = "tcp://" + dindName + ":2375"
	}

	if g.cfg.WorkBind != "" {
		if err := g.wipeWork(ctx, slot); err != nil {
			return err
		}
	}

	extraEnv[jitEnv] = req.JITConfig
	extraEnv["RUNNER_SCALING_SLOT"] = strconv.Itoa(slot)
	spec := g.spec(&g.cfg.Runner, slot, netName, work, extraEnv)
	spec.Hostname = req.RunnerName
	spec.Labels = g.labels(slot, "runner", req.RunnerName)
	spec.Entrypoint = []string{"/bin/sh", "-c", entrypointWrapper, "runner-scaling"}
	spec.Cmd = g.cfg.Runner.Command
	if err := g.run(ctx, runnerName, spec); err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	return nil
}

// wipeCmd empties the directory given as $0, including dotfiles, without removing it.
var wipeCmd = []string{"sh", "-c", `find "$0" -mindepth 1 -delete`}

// wipeWork empties a slot's work_bind directory: inside its dind if there is one (it mounts the same directory),
// otherwise in a one-shot container from the runner image with nothing but that directory and no network.
func (g *Guard) wipeWork(ctx context.Context, slot int) error {
	cmd := append(slices.Clone(wipeCmd), g.cfg.WorkDir)
	var code int
	var err error
	if g.cfg.Dind != nil {
		_, dindName, _, _ := g.names(slot)
		code, err = g.d.Exec(ctx, dindName, cmd)
	} else {
		code, err = g.wipeOneShot(ctx, slot, cmd, []string{g.cfg.Expand(g.cfg.WorkBind, slot) + ":" + g.cfg.WorkDir})
	}
	if err == nil && code != 0 {
		err = fmt.Errorf("exit %d", code)
	}
	if err != nil {
		return fmt.Errorf("empty work dir: %w", err)
	}
	return nil
}

func (g *Guard) wipeOneShot(ctx context.Context, slot int, cmd, binds []string) (int, error) {
	name := g.wiperName(slot)
	if err := g.d.ContainerRemove(ctx, name); err != nil {
		return -1, err
	}
	pids := int64(64)
	spec := ContainerSpec{
		Image:      g.cfg.Runner.Image,
		User:       "0",
		Entrypoint: cmd,
		Labels:     g.labels(slot, "wipe", ""),
		HostConfig: HostConfig{
			Binds:         binds,
			NetworkMode:   "none",
			RestartPolicy: map[string]any{"Name": "no"},
			CapDrop:       []string{"ALL"},
			CapAdd:        []string{"DAC_OVERRIDE", "FOWNER"}, // delete files any job user left behind
			SecurityOpt:   []string{"no-new-privileges:true"},
			Memory:        256 << 20,
			PidsLimit:     &pids,
			CgroupParent:  g.cfg.CgroupParent,
		},
	}
	defer g.d.ContainerRemove(context.WithoutCancel(ctx), name)
	if err := g.run(ctx, name, spec); err != nil {
		return -1, err
	}
	for {
		st, err := g.d.ContainerInspect(ctx, name)
		if err != nil {
			return -1, err
		}
		if st.Status == "exited" || st.Status == "dead" {
			return st.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (g *Guard) wiperName(slot int) string { return g.cfg.Prefix + "-wipe-" + strconv.Itoa(slot) }

func (g *Guard) run(ctx context.Context, name string, spec ContainerSpec) error {
	id, err := g.d.ContainerCreate(ctx, name, spec)
	if err != nil {
		return err
	}
	return g.d.ContainerStart(ctx, id)
}

func (g *Guard) waitHealthy(ctx context.Context, name string) error {
	for {
		st, err := g.d.ContainerInspect(ctx, name)
		if err != nil {
			return err
		}
		switch {
		case st.Status != "running" && st.Status != "created":
			return fmt.Errorf("container %s", st.Status)
		case st.Health == "" && st.Status == "running", st.Health == "healthy":
			return nil
		case st.Health == "unhealthy":
			return errors.New("unhealthy")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not healthy in time: %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// spec turns a template into a container spec for one slot. extra env wins over the template's.
func (g *Guard) spec(t *Template, slot int, network, work string, extra map[string]string) ContainerSpec {
	env := map[string]string{}
	for k, v := range t.Env {
		env[k] = g.cfg.Expand(v, slot)
	}
	maps.Copy(env, extra)
	envList := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		envList = append(envList, k+"="+env[k])
	}
	binds := []string{work}
	for _, b := range t.Binds {
		binds = append(binds, g.cfg.Expand(b, slot))
	}
	hc := HostConfig{
		Binds:         binds,
		NetworkMode:   network,
		RestartPolicy: map[string]any{"Name": "no"},
		Privileged:    t.Privileged,
		CapDrop:       t.CapDrop,
		CapAdd:        t.CapAdd,
		SecurityOpt:   t.SecurityOpt,
		DNS:           t.DNS,
		NanoCPUs:      int64(t.CPUs * 1e9),
		CPUShares:     t.CPUShares,
		CgroupParent:  g.cfg.CgroupParent,
	}
	if t.Memory != "" {
		hc.Memory, _ = ParseBytes(t.Memory) // validated at load
	}
	if t.Pids > 0 {
		p := t.Pids
		hc.PidsLimit = &p
	}
	if len(t.Log) > 0 {
		hc.LogConfig = &LogConfig{Type: "json-file", Config: t.Log}
	}
	s := ContainerSpec{
		Image:            t.Image,
		User:             t.User,
		Env:              envList,
		HostConfig:       hc,
		NetworkingConfig: &NetworkingConfig{EndpointsConfig: map[string]struct{}{network: {}}},
	}
	if t.Health != nil {
		s.Healthcheck = &HealthConfig{
			Test: t.Health.Test, Interval: t.Health.Interval.Duration, Timeout: t.Health.Timeout.Duration,
			Retries: t.Health.Retries, StartPeriod: t.Health.StartPeriod.Duration,
		}
	}
	return s
}

// Stop tears a slot down. Stopping a slot that has nothing in it is not an error.
func (g *Guard) Stop(ctx context.Context, slot int) error {
	if slot < 1 || slot > g.cfg.MaxSlots {
		return fmt.Errorf("%w: slot %d outside 1..%d", ErrBadRequest, slot, g.cfg.MaxSlots)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.starting[slot] {
		return fmt.Errorf("%w: slot %d is starting", ErrConflict, slot)
	}
	return g.teardown(ctx, slot, true, !g.takeKeep(slot))
}

// KeepCache records that the runner on slot is running a trusted job, so teardown leaves the slot's caches alone.
// The runner name must be the one currently on the slot, so a late call can't vouch for a later runner.
func (g *Guard) KeepCache(ctx context.Context, slot int, runner string) error {
	if slot < 1 || slot > g.cfg.MaxSlots {
		return fmt.Errorf("%w: slot %d outside 1..%d", ErrBadRequest, slot, g.cfg.MaxSlots)
	}
	slots, err := g.List(ctx)
	if err != nil {
		return err
	}
	for _, s := range slots {
		if s.Slot == slot && s.RunnerName == runner && runner != "" {
			g.keepMu.Lock()
			g.keepCache[slot] = runner
			g.keepMu.Unlock()
			return nil
		}
	}
	return fmt.Errorf("%w: slot %d is not running %q", ErrConflict, slot, runner)
}

// takeKeep consumes the slot's keep-cache vouch.
func (g *Guard) takeKeep(slot int) bool {
	g.keepMu.Lock()
	defer g.keepMu.Unlock()
	_, ok := g.keepCache[slot]
	delete(g.keepCache, slot)
	return ok
}

// wipeCaches empties every cache_dirs directory of the slot in a one-shot container with nothing else mounted and
// no network. The slot's containers must be gone (dind's image store is among them).
func (g *Guard) wipeCaches(ctx context.Context, slot int) error {
	if len(g.cfg.CacheDirs) == 0 {
		return nil
	}
	var binds, targets []string
	for i, d := range g.cfg.CacheDirs {
		t := "/wipe/" + strconv.Itoa(i)
		binds = append(binds, g.cfg.Expand(d, slot)+":"+t)
		targets = append(targets, t)
	}
	cmd := append([]string{"sh", "-c", `for d in "$@"; do find "$d" -mindepth 1 -delete || exit 1; done`, "wipe"}, targets...)
	wctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	code, err := g.wipeOneShot(wctx, slot, cmd, binds)
	if err == nil && code != 0 {
		err = fmt.Errorf("exit %d", code)
	}
	g.keepMu.Lock()
	g.cacheDirty[slot] = err != nil
	g.keepMu.Unlock()
	if err != nil {
		return fmt.Errorf("empty caches: %w", err)
	}
	return nil
}

func (g *Guard) teardown(ctx context.Context, slot int, prune, wipeCaches bool) error {
	runnerName, dindName, _, volName := g.names(slot)
	var errs []error
	if err := g.d.ContainerRemove(ctx, runnerName); err != nil {
		errs = append(errs, err)
	}
	if g.cfg.Dind != nil {
		if st, err := g.d.ContainerInspect(ctx, dindName); err == nil && st.Status == "running" {
			pctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			if g.cfg.WorkBind != "" {
				if err := g.wipeWork(pctx, slot); err != nil {
					g.log.Warn("teardown", "slot", slot, "err", err)
				}
			}
			if prune {
				for _, cmd := range g.cfg.Dind.PruneOnStop {
					if code, err := g.d.Exec(pctx, dindName, cmd); err != nil || code != 0 {
						g.log.Warn("prune command failed", "slot", slot, "cmd", cmd, "code", code, "err", err)
					}
				}
			}
			cancel()
		}
		if err := g.d.ContainerRemove(ctx, dindName); err != nil {
			errs = append(errs, err)
		}
	}
	if g.cfg.Dind == nil && g.cfg.WorkBind != "" {
		wctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		if err := g.wipeWork(wctx, slot); err != nil {
			g.log.Warn("teardown", "slot", slot, "err", err)
		}
		cancel()
	}
	if err := g.d.ContainerRemove(ctx, g.wiperName(slot)); err != nil {
		errs = append(errs, err)
	}
	if wipeCaches {
		if err := g.wipeCaches(ctx, slot); err != nil {
			errs = append(errs, err)
		}
	}
	if g.cfg.WorkBind == "" {
		if err := g.d.VolumeRemove(ctx, volName); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// WipeStale empties the work_bind directory and the cache_dirs of every slot that has no containers, so a crash
// can't leave job data or a poisoned cache behind. Call it at startup, before serving. Failures are logged and
// skipped (a slot whose caches could not be emptied is refused until they are).
func (g *Guard) WipeStale(ctx context.Context) {
	if g.cfg.WorkBind == "" && len(g.cfg.CacheDirs) == 0 {
		return
	}
	for slot := 1; slot <= g.cfg.MaxSlots; slot++ {
		g.mu.Lock()
		busy, err := g.slotHasContainers(ctx, slot)
		if err == nil && !busy && !g.starting[slot] {
			if g.cfg.WorkBind != "" {
				wctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				var code int
				code, err = g.wipeOneShot(wctx, slot, append(slices.Clone(wipeCmd), g.cfg.WorkDir), []string{g.cfg.Expand(g.cfg.WorkBind, slot) + ":" + g.cfg.WorkDir})
				if err == nil && code != 0 {
					err = fmt.Errorf("exit %d", code)
				}
				cancel()
				if err == nil {
					g.log.Info("wiped stale work dir", "slot", slot)
				}
			}
			if cerr := g.wipeCaches(ctx, slot); cerr != nil {
				err = errors.Join(err, cerr)
			} else if len(g.cfg.CacheDirs) > 0 {
				g.log.Info("wiped caches at startup", "slot", slot)
			}
		}
		g.mu.Unlock()
		if err != nil {
			g.log.Warn("startup wipe failed", "slot", slot, "err", err)
		}
	}
}

// runnerStarts maps slot to the creation time of its runner container.
func (g *Guard) runnerStarts(ctx context.Context) (map[int]time.Time, error) {
	cs, err := g.d.ContainerList(ctx, labelInstance+"="+g.cfg.Prefix)
	if err != nil {
		return nil, err
	}
	out := map[int]time.Time{}
	for _, c := range cs {
		if n, err := strconv.Atoi(c.Labels[labelSlot]); err == nil && c.Labels[labelRole] == "runner" && c.Created > 0 {
			out[n] = time.Unix(c.Created, 0)
		}
	}
	return out, nil
}

// Reap tears down slots whose runner has exited (ephemeral runners exit after their one job) or is missing, and
// slots whose runner has outlived max_runtime. Slots that are starting are never touched.
func (g *Guard) Reap(ctx context.Context) {
	slots, err := g.List(ctx)
	if err != nil {
		g.log.Error("reap: list", "err", err)
		return
	}
	var starts map[int]time.Time
	if g.cfg.MaxRuntime.Duration > 0 || g.cfg.memBudget > 0 {
		if starts, err = g.runnerStarts(ctx); err != nil {
			g.log.Error("reap: list", "err", err)
			starts = nil
		}
	}
	reaped := map[int]bool{}
	for _, s := range slots {
		if s.Starting {
			continue
		}
		reason := "runner_exited"
		if s.RunnerState == "running" || s.RunnerState == "created" || s.RunnerState == "restarting" {
			t, ok := starts[s.Slot]
			if g.cfg.MaxRuntime.Duration <= 0 || !ok || time.Since(t) <= g.cfg.MaxRuntime.Duration {
				continue
			}
			reason = "max_runtime"
		}
		g.mu.Lock()
		if !g.starting[s.Slot] {
			g.log.Info("reaping slot", "slot", s.Slot, "runner", s.RunnerName, "runner_state", s.RunnerState, "reason", reason)
			reaped[s.Slot] = true
			if err := g.teardown(ctx, s.Slot, true, !g.takeKeep(s.Slot)); err != nil {
				g.log.Error("reap: teardown", "slot", s.Slot, "err", err)
			}
		}
		g.mu.Unlock()
	}
	g.memoryBackstop(ctx, slots, starts, reaped)
}

// ReapLoop runs Reap until ctx ends.
func (g *Guard) ReapLoop(ctx context.Context) {
	t := time.NewTicker(g.cfg.ReapInterval.Duration)
	defer t.Stop()
	for {
		g.Reap(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// slotKey identifies a slot across pools.
type slotKey struct {
	instance string
	slot     int
}

// memoryBySlot measures the working set of every running runner-scaling container on the host, all pools together
// (the label filter is on the key only), summed per (instance, slot).
func (g *Guard) memoryBySlot(ctx context.Context) (map[slotKey]int64, error) {
	cs, err := g.d.ContainerList(ctx, labelInstance)
	if err != nil {
		return nil, err
	}
	out := map[slotKey]int64{}
	for _, c := range cs {
		n, err := strconv.Atoi(c.Labels[labelSlot])
		if err != nil {
			continue
		}
		if c.State == "created" || c.State == "restarting" {
			// Not running yet, so nothing to measure; the slot still exists and will count as at least the reserve.
			if _, ok := out[slotKey{c.Labels[labelInstance], n}]; !ok {
				out[slotKey{c.Labels[labelInstance], n}] = 0
			}
			continue
		}
		if c.State != "running" {
			continue
		}
		m, err := g.d.ContainerMemory(ctx, c.ID)
		if errors.Is(err, ErrNotFound) {
			continue // gone since the list
		}
		if err != nil {
			return nil, err
		}
		out[slotKey{c.Labels[labelInstance], n}] += m
	}
	return out, nil
}

// admit refuses a start that would not fit the shared memory budget. Slots count as at least memory_reserve so
// fresh ones are not seen as empty. Stats failures fail open.
func (g *Guard) admit(ctx context.Context, slot int) error {
	if g.cfg.memBudget <= 0 {
		return nil
	}
	by, err := g.memoryBySlot(ctx)
	if err != nil {
		g.log.Warn("memory budget: cannot read stats, admitting", "slot", slot, "err", err)
		return nil
	}
	// This guard's own slots that are still coming up may have no containers yet.
	g.mu.Lock()
	for n := range g.starting {
		if _, ok := by[slotKey{g.cfg.Prefix, n}]; !ok && n != slot {
			by[slotKey{g.cfg.Prefix, n}] = 0
		}
	}
	g.mu.Unlock()
	var total int64
	for _, m := range by {
		total += max(m, g.cfg.memReserve)
	}
	if total+g.cfg.memReserve > g.cfg.memBudget {
		return fmt.Errorf("%w: memory budget: %s in use of %s, need %s reserve", ErrNotReady,
			fmtBytes(total), fmtBytes(g.cfg.memBudget), fmtBytes(g.cfg.memReserve))
	}
	return nil
}

// memoryBackstop reaps this guard's newest running slot once measured use has been over memory_budget on 3
// consecutive reap ticks. It makes no stats calls unless a budget is set and a slot of this guard is running.
func (g *Guard) memoryBackstop(ctx context.Context, slots []api.Slot, starts map[int]time.Time, reaped map[int]bool) {
	if g.cfg.memBudget <= 0 {
		return
	}
	var cand []api.Slot
	for _, s := range slots {
		if !s.Starting && !reaped[s.Slot] && s.RunnerState == "running" {
			cand = append(cand, s)
		}
	}
	if len(cand) == 0 {
		g.overMem = 0
		return
	}
	by, err := g.memoryBySlot(ctx)
	if err != nil {
		g.log.Warn("memory budget: cannot read stats", "err", err)
		return
	}
	var total int64
	for _, m := range by {
		total += m
	}
	if total <= g.cfg.memBudget {
		g.overMem = 0
		return
	}
	if g.overMem++; g.overMem < 3 {
		return
	}
	g.overMem = 0
	victim := cand[0]
	for _, s := range cand[1:] {
		if starts[s.Slot].After(starts[victim.Slot]) {
			victim = s
		}
	}
	g.mu.Lock()
	if !g.starting[victim.Slot] {
		g.log.Info("reaping slot", "slot", victim.Slot, "runner", victim.RunnerName, "runner_state", victim.RunnerState,
			"reason", "memory_budget", "in_use", fmtBytes(total), "budget", fmtBytes(g.cfg.memBudget))
		if err := g.teardown(ctx, victim.Slot, true, true); err != nil {
			g.log.Error("reap: teardown", "slot", victim.Slot, "err", err)
		}
	}
	g.mu.Unlock()
}

// fmtBytes renders sizes like 6.1G, 512M.
func fmtBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return strings.TrimSuffix(strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64), ".0") + "G"
	default:
		return strconv.FormatInt((n+(1<<20)-1)>>20, 10) + "M"
	}
}
