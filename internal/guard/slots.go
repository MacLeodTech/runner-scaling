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

	// jitEnv carries the JIT config into the runner container. The runner reads it exactly as it reads --jitconfig
	// (its ACTIONS_RUNNER_INPUT_<FLAG> convention), so it never appears in the process's argv, which any user on the
	// host can read from /proc/<pid>/cmdline. The environment is readable only by the process's owner and root.
	jitEnv = "ACTIONS_RUNNER_INPUT_JITCONFIG"
)

// entrypointWrapper replaces the image's own entrypoint with the configured command, unchanged.
const entrypointWrapper = `exec "$@"`

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

	admitMu    sync.Mutex     // held across the memory-budget check and marking a slot as starting
	afterAdmit func()         // test seam: runs after the budget check passes, before the slot is claimed; nil in production
	mu         sync.Mutex     // guards starting only; never held across Docker calls that can take long
	starting   map[int]bool   // slots in transition: being started or being torn down (List shows both as Starting)
	keepMu     sync.Mutex     // guards keepCache and cacheDirty
	keepCache  map[int]string // slot -> runner whose caches the scaler vouched for; consumed at teardown
	cacheDirty map[int]bool   // slots whose caches could not be emptied
	cpuMu      sync.Mutex
	cpuPrev    map[string]cpuSample // container id -> last CPU reading, for the dashboard's CPU %
	overMem    int                  // consecutive reap ticks over memory_budget; only touched by Reap
	gateDown   bool                 // require_file was missing at the last reap tick; only touched by Reap
	gateSince  time.Time            // when it went missing; only touched by Reap
	nameRE     *regexp.Regexp
	count      counters
	hostAvail  func() (int64, error) // the host's available memory; a test seam, /proc/meminfo in production
}

func New(cfg *Config, d Docker, log *slog.Logger) *Guard {
	return &Guard{
		cfg: cfg, d: d, log: log,
		starting: map[int]bool{}, keepCache: map[int]string{}, cacheDirty: map[int]bool{},
		nameRE:    regexp.MustCompile(`^` + regexp.QuoteMeta(cfg.Prefix) + `-([0-9]+)-[a-z0-9]{4,16}$`),
		hostAvail: func() (int64, error) { return readMemAvailable("/proc/meminfo") },
	}
}

func (g *Guard) Info() api.Info {
	return api.Info{Prefix: g.cfg.Prefix, MaxSlots: g.cfg.MaxSlots, WorkDir: g.cfg.WorkDir, GitHubURL: g.cfg.GitHubURL, MemoryBudget: g.cfg.memBudget}
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

type cpuSample struct {
	nanos int64
	at    time.Time
}

// ListWithStats is List plus each slot's resource use, summed over its containers. CPU % is the change since the
// previous call, so the first sample of a container reads 0. A container that can't be sampled is left out.
func (g *Guard) ListWithStats(ctx context.Context) ([]api.Slot, error) {
	slots, err := g.List(ctx)
	if err != nil {
		return nil, err
	}
	cs, err := g.d.ContainerList(ctx, labelInstance+"="+g.cfg.Prefix)
	if err != nil {
		return nil, err
	}
	idx := map[int]int{}
	for i := range slots {
		idx[slots[i].Slot] = i
	}
	type res struct {
		slot int
		id   string
		u    ContainerUsage
		at   time.Time
	}
	var mu sync.Mutex
	var got []res
	var wg sync.WaitGroup
	for _, c := range cs {
		n, err := strconv.Atoi(c.Labels[labelSlot])
		if err != nil || c.State != "running" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, err := g.d.ContainerStats(ctx, c.ID)
			if err != nil {
				return
			}
			mu.Lock()
			got = append(got, res{n, c.ID, u, time.Now()})
			mu.Unlock()
		}()
	}
	wg.Wait()
	g.cpuMu.Lock()
	defer g.cpuMu.Unlock()
	if g.cpuPrev == nil {
		g.cpuPrev = map[string]cpuSample{}
	}
	seen := map[string]bool{}
	for _, r := range got {
		i, ok := idx[r.slot]
		if !ok {
			continue
		}
		st := slots[i].Stats
		if st == nil {
			st = &api.SlotStats{}
			slots[i].Stats = st
		}
		st.MemBytes += r.u.MemBytes
		st.PIDs += r.u.PIDs
		st.NetRxBytes += r.u.NetRx
		st.NetTxBytes += r.u.NetTx
		st.BlkReadBytes += r.u.BlkRead
		st.BlkWriteBytes += r.u.BlkWrite
		if p, ok := g.cpuPrev[r.id]; ok && r.at.After(p.at) && r.u.CPUNanos >= p.nanos {
			st.CPUPercent += float64(r.u.CPUNanos-p.nanos) / float64(r.at.Sub(p.at).Nanoseconds()) * 100
		}
		g.cpuPrev[r.id] = cpuSample{r.u.CPUNanos, r.at}
		seen[r.id] = true
	}
	for id := range g.cpuPrev { // forget containers that are gone
		if !seen[id] {
			delete(g.cpuPrev, id)
		}
	}
	return slots, nil
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
	defer g.release(slot)

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
		g.count.add(CounterStartFailures)
		g.log.Error("slot start failed, tearing down", "slot", slot, "err", err)
		g.teardown(context.WithoutCancel(ctx), slot, false, false) // no job ran: the caches are as they were
		return err
	}
	g.count.add(CounterStarts)
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

// slotSubnet is the slot's network range, "" when Docker picks one.
func (g *Guard) slotSubnet(slot int) string {
	if g.cfg.Subnet == "" {
		return ""
	}
	return g.cfg.Expand(g.cfg.Subnet, slot)
}

// slotWork is the work_dir bind of a slot: a per-slot volume, or work_bind.
func (g *Guard) slotWork(slot int) string {
	if g.cfg.WorkBind != "" {
		return g.cfg.Expand(g.cfg.WorkBind, slot) + ":" + g.cfg.WorkDir
	}
	_, _, _, vol := g.names(slot)
	return vol + ":" + g.cfg.WorkDir
}

// dindSpec is the one container spec the guard creates for a slot's Docker sidecar. Pure: no Docker calls.
func (g *Guard) dindSpec(slot int) ContainerSpec {
	_, dindName, netName, _ := g.names(slot)
	spec := g.spec(g.cfg.Dind, slot, netName, g.slotWork(slot), map[string]string{"DOCKER_TLS_CERTDIR": ""})
	spec.Hostname = dindName
	spec.Labels = g.labels(slot, "dind", "")
	return spec
}

// runnerSpec is the one container spec the guard creates for a slot's runner. Pure: no Docker calls.
func (g *Guard) runnerSpec(slot int, runnerName, jit string) ContainerSpec {
	_, dindName, netName, _ := g.names(slot)
	extraEnv := map[string]string{jitEnv: jit, "RUNNER_SCALING_SLOT": strconv.Itoa(slot)}
	if g.cfg.Dind != nil {
		extraEnv["DOCKER_HOST"] = "tcp://" + dindName + ":2375"
	}
	spec := g.spec(&g.cfg.Runner, slot, netName, g.slotWork(slot), extraEnv)
	spec.Hostname = runnerName
	spec.Labels = g.labels(slot, "runner", runnerName)
	spec.Entrypoint = []string{"/bin/sh", "-c", entrypointWrapper, "runner-scaling"}
	spec.Cmd = g.cfg.Runner.Command
	return spec
}

// wipeSpec is the spec of every one-shot helper container (wipes, memory_high, module blocklist): the runner image as
// root with no network, no capabilities beyond file deletion, and only the given command and binds. Pure.
func (g *Guard) wipeSpec(slot int, cmd, binds []string) ContainerSpec {
	pids := int64(64)
	return ContainerSpec{
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
}

func (g *Guard) start(ctx context.Context, slot int, req api.StartRequest) error {
	runnerName, dindName, netName, volName := g.names(slot)
	if err := g.d.NetworkEnsure(ctx, netName, g.slotSubnet(slot), g.labels(slot, "network", "")); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	if g.cfg.WorkBind == "" {
		if err := g.d.VolumeCreate(ctx, volName, g.labels(slot, "work", "")); err != nil {
			return fmt.Errorf("work volume: %w", err)
		}
	}

	high := map[string]*Template{} // container id -> template, for memory_high
	if g.cfg.Dind != nil {
		id, err := g.run(ctx, dindName, g.dindSpec(slot))
		if err != nil {
			return fmt.Errorf("dind: %w", err)
		}
		high[id] = g.cfg.Dind
		if err := g.waitHealthy(ctx, dindName); err != nil {
			return fmt.Errorf("dind: %w", err)
		}
	}

	if g.cfg.WorkBind != "" {
		if err := g.wipeWork(ctx, slot); err != nil {
			return err
		}
	}

	id, err := g.run(ctx, runnerName, g.runnerSpec(slot, req.RunnerName, req.JITConfig))
	if err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	high[id] = &g.cfg.Runner
	g.applyMemoryHigh(ctx, slot, high)
	return nil
}

// wipeCmd empties the directory given as $0, including dotfiles, without removing it.
var wipeCmd = []string{"sh", "-c", `find "$0" -mindepth 1 -delete`}

// wipeWork empties a slot's work_bind directory: inside its dind if there is one (it mounts the same directory),
// otherwise in a one-shot container from the runner image with nothing but that directory and no network.
func (g *Guard) wipeWorkCmd() []string { return append(slices.Clone(wipeCmd), g.cfg.WorkDir) }

func (g *Guard) wipeWork(ctx context.Context, slot int) error {
	cmd := g.wipeWorkCmd()
	var code int
	var err error
	if g.cfg.Dind != nil {
		_, dindName, _, _ := g.names(slot)
		code, err = g.d.Exec(ctx, dindName, cmd)
	} else {
		code, err = g.wipeOneShot(ctx, slot, cmd, []string{g.slotWork(slot)})
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
	spec := g.wipeSpec(slot, cmd, binds)
	defer g.d.ContainerRemove(context.WithoutCancel(ctx), name)
	if _, err := g.run(ctx, name, spec); err != nil {
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

// run creates and starts a container and returns its id.
func (g *Guard) run(ctx context.Context, name string, spec ContainerSpec) (string, error) {
	id, err := g.d.ContainerCreate(ctx, name, spec)
	if err != nil {
		return "", err
	}
	return id, g.d.ContainerStart(ctx, id)
}

// memoryHighScript sets memory.high on the cgroup of each container given as id=bytes. The container's cgroup has
// the id in its name (docker-<id>.scope with Docker's systemd cgroup driver, <id> with cgroupfs), wherever the
// cgroup_parent puts it. It fails unless every container was found and written.
const memoryHighScript = `rc=0
for a in "$@"; do
  id=${a%%=*}; v=${a#*=}; found=0
  for d in $(find /cgroup -maxdepth 6 -type d \( -name "docker-$id.scope" -o -name "$id" \)); do
    echo "$v" > "$d/memory.high" && found=1
  done
  [ "$found" = 1 ] || { echo "no writable cgroup for $id" >&2; rc=1; }
done
exit $rc`

// cgroupSafeRE limits what reaches the script's find patterns: no glob characters.
var cgroupSafeRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// applyMemoryHigh sets the soft memory limit (memory_high) of the given containers, in one one-shot container with
// no network that has the host's cgroup tree mounted. This is best effort: Docker has no setting for it, and the
// hard memory limit protects the host either way, so a failure is logged and the job runs without the throttle.
// targets maps each container id to the template that created it.
func (g *Guard) applyMemoryHigh(ctx context.Context, slot int, targets map[string]*Template) {
	args := []string{"sh", "-c", memoryHighScript, "sh"}
	for _, id := range slices.Sorted(maps.Keys(targets)) {
		t := targets[id]
		if t.MemoryHigh == "" {
			continue
		}
		high, err := ParseBytes(t.MemoryHigh) // validated at load
		if err != nil || !cgroupSafeRE.MatchString(id) {
			g.log.Warn("memory_high skipped", "slot", slot, "container", id)
			continue
		}
		args = append(args, id+"="+strconv.FormatInt(high, 10))
	}
	if len(args) == 4 {
		return
	}
	code, err := g.wipeOneShot(ctx, slot, args, []string{cgroupBind})
	if err == nil && code != 0 {
		err = fmt.Errorf("exit %d", code)
	}
	if err != nil {
		g.log.Warn("memory_high not applied: the hard memory limit still holds", "slot", slot, "err", err)
		return
	}
	g.log.Info("memory_high applied", "slot", slot)
}

// modprobeScript writes the host's modprobe rules file from the one-shot container: $1 is the file name, the rest are
// module names. With no modules it removes the file, but only if it carries the guard's marker line. The file is
// replaced atomically and left alone when it already says the same.
const modprobeScript = `f="/modprobe.d/$1"; shift
marker="# managed by the runner-scaling guard: do not edit"
if [ "$#" = 0 ]; then
  if [ -f "$f" ] && [ "$(head -n1 "$f")" = "$marker" ]; then rm -f "$f" && echo removed; else echo nothing; fi
  exit 0
fi
tmp="$f.tmp"
{ echo "$marker"; for m in "$@"; do echo "install $m /bin/false"; done; } > "$tmp" || exit 1
if [ -f "$f" ] && cmp -s "$tmp" "$f"; then rm -f "$tmp"; echo unchanged; else mv "$tmp" "$f" && echo written; fi`

// ApplyModuleBlocklist makes the host's modprobe rules match module_blocklist (see Config.ModuleBlocklist). It runs at
// startup, in a one-shot container with no network that has the host's /etc/modprobe.d mounted. A failure is logged
// and counted, not fatal: the pool still works, it just doesn't have this protection. It does not unload modules that
// are already loaded. With the option omitted it does nothing; with an explicit empty list it removes the guard's own file.
func (g *Guard) modprobeFile() string { return "50-runner-scaling-" + g.cfg.Prefix + ".conf" }

// modprobeBind and cgroupBind are the only host paths, besides the slots' own directories, a helper container may mount.
const (
	modprobeBind = "/etc/modprobe.d:/modprobe.d"
	cgroupBind   = "/sys/fs/cgroup:/cgroup"
)

func (g *Guard) modprobeCmd() []string {
	return append([]string{"sh", "-c", modprobeScript, "sh", g.modprobeFile()}, g.cfg.ModuleBlocklist...)
}

func (g *Guard) ApplyModuleBlocklist(ctx context.Context) error {
	if g.cfg.ModuleBlocklist == nil {
		return nil // not configured: the host is not touched at all
	}
	file := g.modprobeFile()
	code, err := g.wipeOneShot(ctx, 0, g.modprobeCmd(), []string{modprobeBind})
	if err == nil && code != 0 {
		err = fmt.Errorf("exit %d", code)
	}
	if err != nil {
		g.count.add(CounterModuleBlocklistFailures)
		g.log.Error("module_blocklist NOT applied: jobs can make the host kernel load these modules", "file", "/etc/modprobe.d/"+file, "err", err)
		return err
	}
	g.log.Info("module_blocklist applied", "file", "/etc/modprobe.d/"+file, "modules", len(g.cfg.ModuleBlocklist))
	return nil
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
		Runtime:       t.Runtime,
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
	if !g.claim(slot) {
		return fmt.Errorf("%w: slot %d is starting or stopping", ErrConflict, slot)
	}
	defer g.release(slot)
	return g.teardown(ctx, slot, true, !g.takeKeep(slot))
}

// claim marks a slot as in transition so nothing else starts, stops or reaps it, and reports whether it was free.
// The slow work (wipes, prunes) then runs without g.mu, so it can't stall Start and List for the other slots.
func (g *Guard) claim(slot int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.starting[slot] {
		return false
	}
	g.starting[slot] = true
	return true
}

func (g *Guard) release(slot int) {
	g.mu.Lock()
	delete(g.starting, slot)
	g.mu.Unlock()
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

// cacheMounts is the slot's cache_dirs as bind specs, with the in-container target of each.
func (g *Guard) cacheMounts(slot int) (binds, targets []string) {
	for i, d := range g.cfg.CacheDirs {
		t := "/wipe/" + strconv.Itoa(i)
		binds = append(binds, g.cfg.Expand(d, slot)+":"+t)
		targets = append(targets, t)
	}
	return binds, targets
}

func (g *Guard) cacheWipeCmd(slot int) (binds, cmd []string) {
	binds, targets := g.cacheMounts(slot)
	return binds, append([]string{"sh", "-c", `for d in "$@"; do find "$d" -mindepth 1 -delete || exit 1; done`, "wipe"}, targets...)
}

func (g *Guard) cacheTrimCmd(slot int) (binds, cmd []string) {
	binds, targets := g.cacheMounts(slot)
	script := `max="$1"; shift; used=$(du -sk "$@" | awk '{t+=$1} END{print t+0}'); [ "$used" -le "$max" ] && exit 0; for d in "$@"; do find "$d" -mindepth 1 -delete || exit 1; done`
	return binds, append([]string{"sh", "-c", script, "trim", strconv.FormatInt(g.cfg.cacheMax/1024, 10)}, targets...)
}

// wipeCaches empties every cache_dirs directory of the slot in a one-shot container with nothing else mounted and
// no network. The slot's containers must be gone (dind's image store is among them).
func (g *Guard) wipeCaches(ctx context.Context, slot int) error {
	if len(g.cfg.CacheDirs) == 0 {
		return nil
	}
	binds, cmd := g.cacheWipeCmd(slot)
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

// trimCaches enforces cache_max on a slot whose caches were kept: if the cache_dirs together hold more than the
// limit, they are emptied (same one-shot container as wipeCaches), so growth is bounded without ever mixing
// kept and discarded entries.
func (g *Guard) trimCaches(ctx context.Context, slot int) error {
	if g.cfg.cacheMax <= 0 || len(g.cfg.CacheDirs) == 0 {
		return nil
	}
	binds, cmd := g.cacheTrimCmd(slot)
	wctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	code, err := g.wipeOneShot(wctx, slot, cmd, binds)
	if err == nil && code != 0 {
		err = fmt.Errorf("exit %d", code)
	}
	if err != nil {
		// A half-done trim leaves a partly deleted cache: treat it like a failed wipe and start the slot clean.
		g.keepMu.Lock()
		g.cacheDirty[slot] = true
		g.keepMu.Unlock()
		return err
	}
	return nil
}

// dindStoreWiped reports whether the slot's dind data will be emptied right after teardown: caches are being wiped and
// one of dind's binds is a cache_dirs entry (typically /var/lib/docker).
func (g *Guard) dindStoreWiped(slot int, wipeCaches bool) bool {
	if !wipeCaches || g.cfg.Dind == nil {
		return false
	}
	for _, b := range g.cfg.Dind.Binds {
		host, _, _ := strings.Cut(g.cfg.Expand(b, slot), ":")
		for _, d := range g.cfg.CacheDirs {
			if g.cfg.Expand(d, slot) == host {
				return true
			}
		}
	}
	return false
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
			// Pruning a store that is about to be emptied is wasted time with the slot held.
			if prune && !g.dindStoreWiped(slot, wipeCaches) {
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
	} else if err := g.trimCaches(ctx, slot); err != nil {
		g.log.Warn("cache trim failed", "slot", slot, "err", err) // the caches are intact (and trusted): not an error
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
		if !g.claim(slot) {
			continue
		}
		busy, err := g.slotHasContainers(ctx, slot)
		if err == nil && !busy {
			if g.cfg.WorkBind != "" {
				wctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				var code int
				code, err = g.wipeOneShot(wctx, slot, g.wipeWorkCmd(), []string{g.slotWork(slot)})
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
		g.release(slot)
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
	gateLost := g.watchGate(slots)
	reaped := map[int]bool{}
	var wg sync.WaitGroup // teardowns can take minutes (prune, wipes): run them side by side, not one after another
	for _, s := range slots {
		if s.Starting {
			continue
		}
		reason := "runner_exited"
		if s.RunnerState == "running" || s.RunnerState == "created" || s.RunnerState == "restarting" {
			t, ok := starts[s.Slot]
			switch {
			case gateLost:
				reason = "gate_lost"
			case g.cfg.MaxRuntime.Duration <= 0 || !ok || time.Since(t) <= g.cfg.MaxRuntime.Duration:
				continue
			default:
				reason = "max_runtime"
			}
		}
		if g.claim(s.Slot) {
			g.log.Info("reaping slot", "slot", s.Slot, "runner", s.RunnerName, "runner_state", s.RunnerState, "reason", reason)
			g.count.add(CounterReapedPrefix + reason + "_total")
			reaped[s.Slot] = true
			wg.Go(func() {
				if err := g.teardown(ctx, s.Slot, true, !g.takeKeep(s.Slot)); err != nil {
					g.log.Error("reap: teardown", "slot", s.Slot, "err", err)
				}
				g.release(s.Slot)
			})
		}
	}
	wg.Wait()
	g.memoryBackstop(ctx, slots, starts, reaped)
}

// watchGate notices when require_file disappears while slots are running. The gate only stops new slots from
// starting; a runner that is already up keeps running, and may be unfenced if the rules that the file vouches for
// were flushed. The guard cannot repair that (it has no network and no firewall), so it makes it loud: one ERROR
// line and one count per outage, and an INFO line when the file is back. It touches a running job only with
// gate_loss: stop, once the file has been gone for gate_grace; it then returns true and Reap removes the running slots.
func (g *Guard) watchGate(slots []api.Slot) (stop bool) {
	if g.cfg.RequireFile == "" {
		return false
	}
	if _, err := os.Stat(g.cfg.RequireFile); err == nil {
		if g.gateDown {
			g.gateDown = false
			g.log.Info("gate restored", "file", g.cfg.RequireFile)
		}
		return false
	}
	if !g.gateDown {
		g.gateDown = true
		g.gateSince = time.Now()
		g.count.add(CounterGateWithdrawn)
		g.log.Error("GATE WITHDRAWN: require_file is gone, new slots are refused and running slots may be unfenced",
			"file", g.cfg.RequireFile, "running_slots", len(slots), "gate_loss", g.gateLossMode())
	}
	if g.cfg.GateLoss != "stop" || len(slots) == 0 {
		return false
	}
	grace := 10 * time.Second
	if g.cfg.GateGrace != nil {
		grace = g.cfg.GateGrace.Duration
	}
	return time.Since(g.gateSince) >= grace
}

func (g *Guard) gateLossMode() string {
	if g.cfg.GateLoss == "stop" {
		return "stop"
	}
	return "warn"
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

// slotLoad is one slot's memory as the admission check sees it.
type slotLoad struct {
	used  int64 // measured working set of its running containers
	young int64 // memory limits of its containers that are still inside the ramp window (see memory_ramp)
}

// memoryBySlot measures the working set of every running runner-scaling container on the host, all pools together
// (the label filter is on the key only), summed per (instance, slot).
func (g *Guard) memoryBySlot(ctx context.Context) (map[slotKey]int64, error) {
	loads, err := g.slotLoads(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[slotKey]int64, len(loads))
	for k, l := range loads {
		out[k] = l.used
	}
	return out, nil
}

// slotLoads is memoryBySlot plus, for containers created less than memory_ramp ago, their memory limits.
func (g *Guard) slotLoads(ctx context.Context) (map[slotKey]slotLoad, error) {
	cs, err := g.d.ContainerList(ctx, labelInstance)
	if err != nil {
		return nil, err
	}
	out := map[slotKey]slotLoad{}
	for _, c := range cs {
		n, err := strconv.Atoi(c.Labels[labelSlot])
		if err != nil {
			continue
		}
		k := slotKey{c.Labels[labelInstance], n}
		if c.State != "created" && c.State != "restarting" && c.State != "running" {
			continue
		}
		l := out[k] // the slot exists from here on, so it counts as at least the reserve
		if g.cfg.memRamp > 0 && c.Created > 0 && time.Since(time.Unix(c.Created, 0)) < g.cfg.memRamp {
			// A limit we can't read is left out: the slot then counts as measured, or as the reserve.
			if st, err := g.d.ContainerInspect(ctx, c.ID); err == nil {
				l.young += st.MemoryLimit
			}
		}
		if c.State == "running" { // created or restarting: nothing to measure yet
			m, err := g.d.ContainerMemory(ctx, c.ID)
			if errors.Is(err, ErrNotFound) {
				continue // gone since the list
			}
			if err != nil {
				return nil, err
			}
			l.used += m
		}
		out[k] = l
	}
	return out, nil
}

// admit refuses a start that would not fit the shared memory budget. Slots count as at least memory_reserve so
// fresh ones are not seen as empty. Stats failures fail open.
func (g *Guard) admit(ctx context.Context, slot int) error {
	if g.cfg.memBudget <= 0 {
		return nil
	}
	by, err := g.slotLoads(ctx)
	if err != nil {
		g.log.Warn("memory budget: cannot read stats, admitting", "slot", slot, "err", err)
		return nil
	}
	// This guard's own slots that are still coming up may have no containers yet.
	g.mu.Lock()
	for n := range g.starting {
		if _, ok := by[slotKey{g.cfg.Prefix, n}]; !ok && n != slot {
			by[slotKey{g.cfg.Prefix, n}] = slotLoad{}
		}
	}
	g.mu.Unlock()
	// A slot counts as its measured use, but at least the reserve, and while its containers are young at least a
	// share of their limits: they look light now and will grow.
	var total, measured int64
	for _, l := range by {
		total += max(l.used, g.cfg.memReserve, int64(float64(l.young)*g.cfg.memRampShare))
		measured += l.used
	}
	budget, avail, limited := g.effectiveBudget(measured)
	if total+g.cfg.memReserve > budget {
		g.count.add(CounterAdmissionRefused)
		if limited {
			return fmt.Errorf("%w: memory budget: %s in use of %s (the host has %s free and keeps %s for other services), need %s reserve", ErrNotReady,
				fmtBytes(total), fmtBytes(budget), fmtBytes(avail), fmtBytes(g.cfg.memHeadroom), fmtBytes(g.cfg.memReserve))
		}
		return fmt.Errorf("%w: memory budget: %s in use of %s, need %s reserve", ErrNotReady,
			fmtBytes(total), fmtBytes(g.cfg.memBudget), fmtBytes(g.cfg.memReserve))
	}
	return nil
}

// effectiveBudget is memory_budget, lowered when memory_host_headroom is set and the host cannot spare that much: the
// host's available memory plus what the runners already use (they are part of what is no longer available), minus the
// headroom. limited reports that the host, not the configured budget, set the figure; avail is the host's available
// memory. An unreadable /proc/meminfo fails open to the configured budget.
func (g *Guard) effectiveBudget(runnersUsed int64) (budget, avail int64, limited bool) {
	budget = g.cfg.memBudget
	if g.cfg.memHeadroom <= 0 || g.hostAvail == nil {
		return budget, 0, false
	}
	avail, err := g.hostAvail()
	if err != nil {
		g.log.Warn("memory budget: cannot read host memory, using memory_budget alone", "err", err)
		return budget, 0, false
	}
	spare := max(avail+runnersUsed-g.cfg.memHeadroom, 0)
	if spare < budget {
		return spare, avail, true
	}
	return budget, avail, false
}

// hostShort reports that the host's available memory is below half the headroom: real pressure from outside the
// runners, not just a smaller budget.
func (g *Guard) hostShort() bool {
	if g.cfg.memHeadroom <= 0 || g.hostAvail == nil {
		return false
	}
	avail, err := g.hostAvail()
	return err == nil && avail < g.cfg.memHeadroom/2
}

// readMemAvailable returns MemAvailable from a /proc/meminfo-style file, in bytes. A container sees the host's figure.
func readMemAvailable(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return parseMemAvailable(string(b))
}

func parseMemAvailable(s string) (int64, error) {
	for _, line := range strings.Split(s, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemAvailable:" {
			kb, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil || kb < 0 {
				return 0, fmt.Errorf("MemAvailable %q is not a size", f[1])
			}
			return kb * 1024, nil
		}
	}
	return 0, errors.New("no MemAvailable in meminfo")
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
	short := g.hostShort()
	if total <= g.cfg.memBudget && !short {
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
	if g.claim(victim.Slot) {
		g.count.add(CounterReapedPrefix + "memory_budget_total")
		g.log.Info("reaping slot", "slot", victim.Slot, "runner", victim.RunnerName, "runner_state", victim.RunnerState,
			"reason", "memory_budget", "host_short", short, "in_use", fmtBytes(total), "budget", fmtBytes(g.cfg.memBudget))
		if err := g.teardown(ctx, victim.Slot, true, true); err != nil {
			g.log.Error("reap: teardown", "slot", victim.Slot, "err", err)
		}
		g.release(victim.Slot)
	}
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
