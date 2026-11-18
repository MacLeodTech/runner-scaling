package guard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MacLeodTech/runner-scaling/internal/api"
)

// fakeDocker keeps containers in memory.
type fakeDocker struct {
	mu          sync.Mutex
	containers  map[string]*fakeContainer // by name
	volumes     map[string]bool
	networks    map[string]string
	execs       [][]string
	pulled      []string
	dindHealth  string
	wipes       []ContainerSpec
	wipeExit    int              // exit code of one-shot wipers
	mem         map[string]int64 // by container name
	memErr      error
	memCalls    int
	cpu         map[string]int64 // cumulative CPU ns by container name
	statCalls   int
	execGate    chan struct{} // if set, Exec blocks until it is closed (to hold a teardown mid-prune)
	execEntered chan struct{} // if set, receives one value when Exec is reached
}

type fakeContainer struct {
	spec    ContainerSpec
	state   string
	created time.Time
}

func newFake() *fakeDocker {
	return &fakeDocker{containers: map[string]*fakeContainer{}, volumes: map[string]bool{}, networks: map[string]string{}, dindHealth: "healthy"}
}

func (f *fakeDocker) ImageExists(context.Context, string) (bool, error) { return false, nil }
func (f *fakeDocker) ImagePull(_ context.Context, r string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulled = append(f.pulled, r)
	return nil
}
func (f *fakeDocker) NetworkEnsure(_ context.Context, n, s string, _ map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.networks[n] = s
	return nil
}
func (f *fakeDocker) VolumeCreate(_ context.Context, n string, _ map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumes[n] = true
	return nil
}
func (f *fakeDocker) VolumeRemove(_ context.Context, n string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.volumes, n)
	return nil
}
func (f *fakeDocker) ContainerCreate(_ context.Context, n string, s ContainerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.containers[n] != nil {
		return "", fmt.Errorf("name %s in use", n)
	}
	f.containers[n] = &fakeContainer{spec: s, state: "created", created: time.Now()}
	return n, nil
}
func (f *fakeDocker) ContainerStart(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	c.state = "running"
	if c.spec.Labels[labelRole] == "wipe" { // one-shot: done at once
		c.state = "exited"
		f.wipes = append(f.wipes, c.spec)
	}
	return nil
}
func (f *fakeDocker) ContainerInspect(_ context.Context, id string) (*ContainerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	if c == nil {
		return nil, ErrNotFound
	}
	st := &ContainerState{Status: c.state, MemoryLimit: c.spec.HostConfig.Memory}
	if c.spec.Labels[labelRole] == "wipe" {
		st.ExitCode = f.wipeExit
	}
	if c.spec.Healthcheck != nil {
		st.Health = f.dindHealth
	}
	return st, nil
}
func (f *fakeDocker) ContainerRemove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, id)
	return nil
}
func (f *fakeDocker) ContainerList(_ context.Context, label string) ([]ContainerSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, v, hasV := strings.Cut(label, "=")
	var out []ContainerSummary
	for n, c := range f.containers {
		got, present := c.spec.Labels[k]
		if present && (!hasV || got == v) {
			out = append(out, ContainerSummary{ID: n, Names: []string{"/" + n}, State: c.state, Created: c.created.Unix(), Labels: c.spec.Labels})
		}
	}
	return out, nil
}
func (f *fakeDocker) ContainerMemory(_ context.Context, id string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.memCalls++
	if f.memErr != nil {
		return 0, f.memErr
	}
	return f.mem[id], nil
}
func (f *fakeDocker) ContainerStats(_ context.Context, id string) (ContainerUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statCalls++
	return ContainerUsage{MemBytes: f.mem[id], CPUNanos: f.cpu[id], PIDs: 3, NetRx: 10, NetTx: 20, BlkRead: 1, BlkWrite: 2}, nil
}
func (f *fakeDocker) Exec(_ context.Context, _ string, cmd []string) (int, error) {
	if f.execEntered != nil {
		f.execEntered <- struct{}{}
	}
	if f.execGate != nil {
		<-f.execGate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs = append(f.execs, cmd)
	return 0, nil
}

const testConfig = `
github_url: https://github.com/example-org
tenant_url: https://pipelines.actions.githubusercontent.com/OURS/
prefix: ci
max_slots: 3
subnet: 172.41.{slot}.0/24
work_dir: /runner/work
start_timeout: 5s
runner:
  image: example/runner@sha256:aaaa
  command: [/actions-runner/run.sh]
  env: {GOCACHE: /cache/go-build, RUNNER_ALLOW_RUNASROOT: "1"}
  binds: ["/srv/runners/{slot}/cache:/cache"]
  cap_drop: [ALL]
  security_opt: [no-new-privileges:true]
  memory: 3G
  cpus: 2
  pids: 1024
  labels: {com.example.keep: "yes"}
dind:
  image: docker@sha256:bbbb
  privileged: true
  binds: ["/srv/runners/{slot}/docker:/var/lib/docker"]
  memory: 2G
  healthcheck: {test: [CMD, docker, info], interval: 1s, retries: 5}
  prune_on_stop:
    - [docker, builder, prune, -f]
`

func newGuard(t *testing.T) (*Guard, *fakeDocker) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "guard.yaml")
	if err := os.WriteFile(p, []byte(testConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	f := newFake()
	return New(cfg, f, slog.New(slog.NewTextHandler(io.Discard, nil))), f
}

// jitFor makes a JIT config for a runner. org selects the tenant: the guard's own ("https://github.com/example-org")
// or another one.
func jitFor(name, org string) string {
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	tenant := "OURS"
	if org != "https://github.com/example-org" {
		tenant = "THEIRS"
	}
	runner := fmt.Sprintf(`{"AgentName":%q,"ServerUrl":"https://pipelines.actions.githubusercontent.com/%s/","ServerUrlV2":"https://broker.actions.githubusercontent.com/","WorkFolder":"/runner/work"}`, name, tenant)
	files, _ := json.Marshal(map[string]string{".runner": enc(runner), ".credentials": enc(`{"scheme":"OAuth"}`)})
	return enc(string(files))
}

func do(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var b io.Reader
	if s, ok := body.(string); ok {
		b = strings.NewReader(s)
	} else if body != nil {
		j, _ := json.Marshal(body)
		b = bytes.NewReader(j)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, b))
	return rec
}

func TestStartBuildsSpecFromTemplate(t *testing.T) {
	g, f := newGuard(t)
	h := g.Handler()
	rec := do(t, h, "PUT", "/v1/slots/2", api.StartRequest{RunnerName: "ci-2-abcd12", JITConfig: jitFor("ci-2-abcd12", "https://github.com/example-org")})
	if rec.Code != http.StatusCreated {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	r, d := f.containers["ci-runner-2"], f.containers["ci-dind-2"]
	if r == nil || d == nil {
		t.Fatalf("containers: %v", f.containers)
	}
	if r.spec.HostConfig.Privileged || !d.spec.HostConfig.Privileged {
		t.Error("only dind may be privileged")
	}
	if !slices.Contains(r.spec.Env, "DOCKER_HOST=tcp://ci-dind-2:2375") || !slices.Contains(d.spec.Env, "DOCKER_TLS_CERTDIR=") {
		t.Errorf("env: runner %v dind %v", r.spec.Env, d.spec.Env)
	}
	if !slices.Contains(r.spec.HostConfig.Binds, "/srv/runners/2/cache:/cache") || !slices.Contains(r.spec.HostConfig.Binds, "ci-work-2:/runner/work") {
		t.Errorf("runner binds: %v", r.spec.HostConfig.Binds)
	}
	if !slices.Contains(d.spec.HostConfig.Binds, "ci-work-2:/runner/work") {
		t.Errorf("dind binds: %v", d.spec.HostConfig.Binds)
	}
	if f.networks["ci-slot-2"] != "172.41.2.0/24" || !f.volumes["ci-work-2"] {
		t.Errorf("network/volume: %v %v", f.networks, f.volumes)
	}
	if r.spec.HostConfig.Memory != 3<<30 || *r.spec.HostConfig.PidsLimit != 1024 || r.spec.HostConfig.NanoCPUs != 2e9 {
		t.Errorf("limits: %+v", r.spec.HostConfig)
	}
	if r.spec.Labels["com.example.keep"] != "yes" || r.spec.Labels[labelSlot] != "2" {
		t.Errorf("labels: %v", r.spec.Labels)
	}
	if got := r.spec.Cmd; len(got) != 1 || got[0] != "/actions-runner/run.sh" {
		t.Errorf("cmd: %v", got)
	}

	// Same slot again: conflict. Then stop, which prunes and removes everything.
	rec = do(t, h, "PUT", "/v1/slots/2", api.StartRequest{RunnerName: "ci-2-zzzz99", JITConfig: jitFor("ci-2-zzzz99", "https://github.com/example-org")})
	if rec.Code != http.StatusConflict {
		t.Fatalf("second start: %d", rec.Code)
	}
	if rec := do(t, h, "DELETE", "/v1/slots/2", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("stop: %d %s", rec.Code, rec.Body)
	}
	if len(f.containers) != 0 || len(f.volumes) != 0 || len(f.execs) != 1 {
		t.Errorf("after stop: containers %v volumes %v execs %v", f.containers, f.volumes, f.execs)
	}
}

func TestRefusals(t *testing.T) {
	g, f := newGuard(t)
	h := g.Handler()
	good := jitFor("ci-1-abcd12", "https://github.com/example-org")
	cases := []struct {
		name, method, path string
		body               any
		want               int
	}{
		{"slot 0", "PUT", "/v1/slots/0", api.StartRequest{RunnerName: "ci-0-abcd12", JITConfig: good}, 400},
		{"slot over max", "PUT", "/v1/slots/4", api.StartRequest{RunnerName: "ci-4-abcd12", JITConfig: good}, 400},
		{"slot not a number", "PUT", "/v1/slots/1x", api.StartRequest{RunnerName: "ci-1-abcd12", JITConfig: good}, 400},
		{"name for another slot", "PUT", "/v1/slots/1", api.StartRequest{RunnerName: "ci-2-abcd12", JITConfig: jitFor("ci-2-abcd12", "https://github.com/example-org")}, 400},
		{"name with wrong prefix", "PUT", "/v1/slots/1", api.StartRequest{RunnerName: "evil-1-abcd12", JITConfig: jitFor("evil-1-abcd12", "https://github.com/example-org")}, 400},
		{"jit names another runner", "PUT", "/v1/slots/1", api.StartRequest{RunnerName: "ci-1-abcd12", JITConfig: jitFor("ci-1-other1", "https://github.com/example-org")}, 400},
		{"jit for another org", "PUT", "/v1/slots/1", api.StartRequest{RunnerName: "ci-1-abcd12", JITConfig: jitFor("ci-1-abcd12", "https://github.com/evil")}, 400},
		{"unknown field", "PUT", "/v1/slots/1", `{"runner_name":"ci-1-abcd12","jit_config":"x","image":"evil"}`, 400},
		{"not json", "PUT", "/v1/slots/1", `nope`, 400},
		{"stop over max", "DELETE", "/v1/slots/9", nil, 400},
		{"raw docker api", "POST", "/containers/create", `{}`, 404},
		{"wrong method", "POST", "/v1/slots/1", `{}`, 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := do(t, h, c.method, c.path, c.body)
			if rec.Code != c.want && !(c.want == 404 && rec.Code == 405) {
				t.Fatalf("got %d, want %d: %s", rec.Code, c.want, rec.Body)
			}
		})
	}
	if len(f.containers) != 0 {
		t.Fatalf("refused requests created containers: %v", f.containers)
	}
}

func TestUnhealthyDindTearsDown(t *testing.T) {
	g, f := newGuard(t)
	f.dindHealth = "unhealthy"
	err := g.Start(context.Background(), 1, api.StartRequest{RunnerName: "ci-1-abcd12", JITConfig: jitFor("ci-1-abcd12", "https://github.com/example-org")})
	if err == nil {
		t.Fatal("start succeeded with unhealthy dind")
	}
	if len(f.containers) != 0 || len(f.volumes) != 0 {
		t.Fatalf("left behind: %v %v", f.containers, f.volumes)
	}
}

func TestReapRemovesExitedRunners(t *testing.T) {
	g, f := newGuard(t)
	ctx := context.Background()
	for _, n := range []int{1, 2} {
		name := fmt.Sprintf("ci-%d-abcd12", n)
		if err := g.Start(ctx, n, api.StartRequest{RunnerName: name, JITConfig: jitFor(name, "https://github.com/example-org")}); err != nil {
			t.Fatal(err)
		}
	}
	f.containers["ci-runner-1"].state = "exited"
	g.Reap(ctx)
	if f.containers["ci-dind-1"] != nil || f.containers["ci-runner-1"] != nil {
		t.Error("slot 1 not reaped")
	}
	if f.containers["ci-runner-2"] == nil {
		t.Error("slot 2 reaped while running")
	}
	slots, _ := g.List(ctx)
	if len(slots) != 1 || slots[0].Slot != 2 || slots[0].RunnerName != "ci-2-abcd12" {
		t.Errorf("slots: %+v", slots)
	}
}

func TestConfigRejects(t *testing.T) {
	base := testConfig
	cases := map[string][2]string{
		"unknown key":       {"prefix: ci", "prefix: ci\nimage_override: x"},
		"bad prefix":        {"prefix: ci", "prefix: Home_Lab"},
		"http github url":   {"https://github.com/example-org", "http://github.com/example-org"},
		"too many slots":    {"max_slots: 3", "max_slots: 900"},
		"privileged runner": {"  cap_drop: [ALL]", "  privileged: true\n  cap_drop: [ALL]"},
		"socket bind":       {`"/srv/runners/{slot}/cache:/cache"`, `"/var/run/docker.sock:/var/run/docker.sock"`},
		"root bind":         {`"/srv/runners/{slot}/cache:/cache"`, `"/:/host"`},
		"bad subnet":        {"172.41.{slot}.0/24", "300.41.{slot}.0/24"},
		"relative workdir":  {"work_dir: /runner/work", "work_dir: tmp"},
		"reserved label":    {`com.example.keep: "yes"`, `io.github.runner-scaling.slot: "9"`},
		"http tenant":       {"https://pipelines.actions", "http://pipelines.actions"},
		"work_dir in /tmp":  {"work_dir: /runner/work", "work_dir: /tmp/runner"},
		"bad cgroup parent": {"work_dir: /runner/work", "work_dir: /runner/work\ncgroup_parent: ../evil"},
		"bad runtime":       {"  privileged: true\n  binds: [\"/srv/runners/{slot}/docker", "  privileged: true\n  runtime: \"../x y\"\n  binds: [\"/srv/runners/{slot}/docker"},
		"work_bind no slot": {"work_dir: /runner/work", "work_dir: /runner/work\nwork_bind: /big/work"},
	}
	for name, sub := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "guard.yaml")
			os.WriteFile(p, []byte(strings.Replace(base, sub[0], sub[1], 1)), 0o600)
			if _, err := LoadConfig(p); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]int64{"512m": 512 << 20, "3G": 3 << 30, "1.5g": 3 << 29, "2GiB": 2 << 30, "100": 100} {
		if got, err := ParseBytes(in); err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v", in, got, err)
		}
	}
}

func TestExampleConfigsLoad(t *testing.T) {
	main, err := LoadConfig("../../examples/guard.yaml")
	if err != nil {
		t.Fatal(err)
	}
	lite, err := LoadConfig("../../examples/guard-lite.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if main.Dind == nil || lite.Dind != nil {
		t.Errorf("main pool should have dind and lite should not: %v, %v", main.Dind != nil, lite.Dind != nil)
	}
	// Two pools on one host: they must not share names or overlap, and must share the memory budget.
	if main.Prefix == lite.Prefix || main.Subnet == lite.Subnet {
		t.Errorf("pools collide: %q/%q, %q/%q", main.Prefix, lite.Prefix, main.Subnet, lite.Subnet)
	}
	if main.MemoryBudget != lite.MemoryBudget || main.RequireFile != lite.RequireFile {
		t.Errorf("pools disagree on the shared budget or gate")
	}
}

func TestWorkBindIsWipedThroughDind(t *testing.T) {
	g, f := newGuard(t)
	g.cfg.WorkBind = "/big/runners/{slot}/work"
	if err := g.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := g.Start(ctx, 1, api.StartRequest{RunnerName: "ci-1-abcd12", JITConfig: jitFor("ci-1-abcd12", "https://github.com/example-org")}); err != nil {
		t.Fatal(err)
	}
	if len(f.volumes) != 0 {
		t.Errorf("created a volume in bind mode: %v", f.volumes)
	}
	if b := f.containers["ci-runner-1"].spec.HostConfig.Binds; !slices.Contains(b, "/big/runners/1/work:/runner/work") {
		t.Errorf("runner binds: %v", b)
	}
	if len(f.execs) != 1 || f.execs[0][len(f.execs[0])-1] != "/runner/work" {
		t.Fatalf("no wipe before start: %v", f.execs)
	}
	g.Stop(ctx, 1)
	if len(f.execs) != 3 { // wipe, then wipe + prune at teardown
		t.Errorf("execs: %v", f.execs)
	}
}

func TestRequireFileGatesStarts(t *testing.T) {
	g, f := newGuard(t)
	g.cfg.RequireFile = filepath.Join(t.TempDir(), "firewall-ok")
	req := api.StartRequest{RunnerName: "ci-1-abcd12", JITConfig: jitFor("ci-1-abcd12", "https://github.com/example-org")}
	if rec := do(t, g.Handler(), "PUT", "/v1/slots/1", req); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("started without the gate file: %d", rec.Code)
	}
	if len(f.containers) != 0 {
		t.Fatal("containers created")
	}
	os.WriteFile(g.cfg.RequireFile, nil, 0o600)
	if rec := do(t, g.Handler(), "PUT", "/v1/slots/1", req); rec.Code != http.StatusCreated {
		t.Fatalf("gate file present: %d %s", rec.Code, rec.Body)
	}
}

func TestRuntimeIsPassedToDocker(t *testing.T) {
	g, f := newGuard(t)
	g.cfg.Dind.Runtime = "sysbox-runc"
	g.cfg.Dind.Privileged = false
	if err := g.Start(context.Background(), 1, api.StartRequest{RunnerName: "ci-1-abcd12", JITConfig: jitFor("ci-1-abcd12", "https://github.com/example-org")}); err != nil {
		t.Fatal(err)
	}
	if got := f.containers["ci-dind-1"].spec.HostConfig.Runtime; got != "sysbox-runc" {
		t.Errorf("dind runtime %q", got)
	}
	if got := f.containers["ci-runner-1"].spec.HostConfig.Runtime; got != "" {
		t.Errorf("runner should use the default runtime, got %q", got)
	}
}

func TestGateWithdrawnIsCountedOnceAndRunnersSurvive(t *testing.T) {
	g, f := newGuard(t)
	ctx := context.Background()
	g.cfg.RequireFile = filepath.Join(t.TempDir(), "firewall-ok")
	os.WriteFile(g.cfg.RequireFile, nil, 0o600)
	if err := g.Start(ctx, 1, api.StartRequest{RunnerName: "ci-1-abcd12", JITConfig: jitFor("ci-1-abcd12", "https://github.com/example-org")}); err != nil {
		t.Fatal(err)
	}
	g.Reap(ctx)
	if n := g.Counters()[CounterGateWithdrawn]; n != 0 {
		t.Fatalf("counted with the gate in place: %d", n)
	}
	os.Remove(g.cfg.RequireFile)
	g.Reap(ctx)
	g.Reap(ctx)
	if n := g.Counters()[CounterGateWithdrawn]; n != 1 {
		t.Fatalf("one outage should count once, got %d", n)
	}
	if f.containers["ci-runner-1"] == nil {
		t.Fatal("a running job was killed because the gate went away")
	}
	os.WriteFile(g.cfg.RequireFile, nil, 0o600)
	g.Reap(ctx)
	os.Remove(g.cfg.RequireFile)
	g.Reap(ctx)
	if n := g.Counters()[CounterGateWithdrawn]; n != 2 {
		t.Fatalf("second outage should count: %d", n)
	}
}

func TestConfigExpandsBracedEnvOnly(t *testing.T) {
	t.Setenv("RS_TEST_DIR", "/big")
	cfg := strings.Replace(testConfig, `"/srv/runners/{slot}/cache:/cache"`, `"${RS_TEST_DIR}/{slot}/cache:/cache"`, 1)
	cfg = strings.Replace(cfg, "command: [/actions-runner/run.sh]", `command: [/bin/sh, -c, 'mkdir -p "$TMPDIR" && exec run.sh "$@"', run]`, 1)
	p := filepath.Join(t.TempDir(), "guard.yaml")
	os.WriteFile(p, []byte(cfg), 0o600)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Runner.Binds[0] != "/big/{slot}/cache:/cache" || !strings.Contains(c.Runner.Command[2], `"$TMPDIR"`) {
		t.Errorf("binds %v command %v", c.Runner.Binds, c.Runner.Command)
	}
	os.WriteFile(p, []byte(strings.Replace(cfg, "${RS_TEST_DIR}", "${RS_TEST_UNSET_VAR}", 1)), 0o600)
	if _, err := LoadConfig(p); err == nil {
		t.Error("unset variable accepted")
	}
}

func TestNoDindPoolWipesInOneShotContainer(t *testing.T) {
	g, f := newGuard(t)
	g.cfg.Dind = nil
	g.cfg.WorkBind = "/big/lite/{slot}/work"
	g.cfg.CgroupParent = "runner-scaling.slice"
	if err := g.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := g.Start(ctx, 2, api.StartRequest{RunnerName: "ci-2-abcd12", JITConfig: jitFor("ci-2-abcd12", "https://github.com/example-org")}); err != nil {
		t.Fatal(err)
	}
	if len(f.wipes) != 1 {
		t.Fatalf("wipes: %d", len(f.wipes))
	}
	w := f.wipes[0]
	if w.HostConfig.NetworkMode != "none" || w.HostConfig.Privileged || len(w.HostConfig.Binds) != 1 || w.HostConfig.Binds[0] != "/big/lite/2/work:/runner/work" {
		t.Errorf("wiper spec: %+v", w.HostConfig)
	}
	r := f.containers["ci-runner-2"]
	if r == nil || slices.ContainsFunc(r.spec.Env, func(e string) bool { return strings.HasPrefix(e, "DOCKER_HOST=") }) {
		t.Fatalf("runner: %+v", r)
	}
	if r.spec.HostConfig.CgroupParent != "runner-scaling.slice" || w.HostConfig.CgroupParent != "runner-scaling.slice" {
		t.Errorf("cgroup parent not applied")
	}
	if f.containers["ci-wipe-2"] != nil {
		t.Error("wiper left behind")
	}
	g.Stop(ctx, 2)
	if len(f.wipes) != 2 || len(f.containers) != 0 {
		t.Errorf("after stop: wipes %d containers %v", len(f.wipes), f.containers)
	}
}

func TestWipeStaleAtStartup(t *testing.T) {
	for _, dind := range []bool{false, true} {
		t.Run(fmt.Sprintf("dind=%v", dind), func(t *testing.T) {
			g, f := newGuard(t)
			if !dind {
				g.cfg.Dind = nil
			}
			g.cfg.WorkBind = "/big/runners/{slot}/work"
			ctx := context.Background()
			// slot 2 is in use and must be left alone.
			f.containers["ci-runner-2"] = &fakeContainer{state: "running", spec: ContainerSpec{Labels: g.labels(2, "runner", "ci-2-abcd12")}}
			g.WipeStale(ctx)
			var got []string
			for _, w := range f.wipes {
				got = append(got, w.HostConfig.Binds[0])
				if w.HostConfig.NetworkMode != "none" || w.HostConfig.Privileged {
					t.Errorf("wiper spec: %+v", w.HostConfig)
				}
			}
			slices.Sort(got)
			want := []string{"/big/runners/1/work:/runner/work", "/big/runners/3/work:/runner/work"}
			if !slices.Equal(got, want) {
				t.Errorf("wiped %v, want %v", got, want)
			}
			if len(f.containers) != 1 {
				t.Errorf("wipers left behind: %v", f.containers)
			}
		})
	}
	t.Run("no work_bind", func(t *testing.T) {
		g, f := newGuard(t)
		g.WipeStale(context.Background())
		if len(f.wipes) != 0 {
			t.Errorf("wipes: %d", len(f.wipes))
		}
	})
}

func TestReapMaxRuntime(t *testing.T) {
	cases := []struct {
		name     string
		max      time.Duration
		age      time.Duration
		starting bool
		reaped   bool
	}{
		{"disabled", 0, 100 * time.Hour, false, false},
		{"within limit", time.Hour, 10 * time.Minute, false, false},
		{"over limit", time.Hour, 2 * time.Hour, false, true},
		{"over limit but starting", time.Hour, 2 * time.Hour, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, f := newGuard(t)
			g.cfg.MaxRuntime.Duration = c.max
			ctx := context.Background()
			name := "ci-1-abcd12"
			if err := g.Start(ctx, 1, api.StartRequest{RunnerName: name, JITConfig: jitFor(name, "https://github.com/example-org")}); err != nil {
				t.Fatal(err)
			}
			f.containers["ci-runner-1"].created = time.Now().Add(-c.age)
			if c.starting {
				g.mu.Lock()
				g.starting[1] = true
				g.mu.Unlock()
			}
			g.Reap(ctx)
			if gone := f.containers["ci-runner-1"] == nil; gone != c.reaped {
				t.Errorf("reaped=%v, want %v", gone, c.reaped)
			}
		})
	}
}

func TestMaxRuntimeValidation(t *testing.T) {
	for in, ok := range map[string]bool{"max_runtime: 0s": true, "max_runtime: 6h": true, "max_runtime: 1m": true, "max_runtime: 30s": false, "max_runtime: -1h": false} {
		p := filepath.Join(t.TempDir(), "guard.yaml")
		os.WriteFile(p, []byte(strings.Replace(testConfig, "prefix: ci", "prefix: ci\n"+in, 1)), 0o600)
		if _, err := LoadConfig(p); (err == nil) != ok {
			t.Errorf("%s: err=%v, want ok=%v", in, err, ok)
		}
	}
}

const gib = int64(1) << 30

func memGuard(t *testing.T, budget string) (*Guard, *fakeDocker) {
	t.Helper()
	g, f := newGuard(t)
	g.cfg.MemoryBudget, g.cfg.MemoryReserve = budget, "1G"
	if err := g.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	f.mem = map[string]int64{}
	return g, f
}

func startSlot(t *testing.T, g *Guard, n int) error {
	name := fmt.Sprintf("ci-%d-abcd12", n)
	return g.Start(context.Background(), n, api.StartRequest{RunnerName: name, JITConfig: jitFor(name, "https://github.com/example-org")})
}

func TestMemoryAdmission(t *testing.T) {
	// Budget 8G, reserve 1G: admit while total+1G <= 8G.
	cases := []struct {
		name  string
		usage map[string]int64 // container name -> bytes
		other bool             // put usage on another pool's slot
		ok    bool
	}{
		{"empty", nil, false, true},
		{"exactly fits", map[string]int64{"ci-runner-2": 7 * gib}, false, true},
		{"one byte over", map[string]int64{"ci-runner-2": 7*gib + 1}, false, false},
		{"runner+dind summed", map[string]int64{"ci-runner-2": 4 * gib, "ci-dind-2": 4 * gib}, false, false},
		{"small slot counts as reserve", map[string]int64{"ci-runner-2": 1 << 20}, false, true},
		{"reserve floor denies", nil, false, true},
		{"other pool counts", map[string]int64{"lite-runner-1": 7*gib + 1}, true, false},
		{"other pool fits", map[string]int64{"lite-runner-1": 7 * gib}, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, f := memGuard(t, "8G")
			for n, v := range c.usage {
				pool, slot := "ci", "2"
				if c.other {
					pool, slot = "lite", "1"
				}
				f.containers[n] = &fakeContainer{state: "running", spec: ContainerSpec{Labels: map[string]string{labelInstance: pool, labelSlot: slot, labelRole: "runner"}}}
				f.mem[n] = v
			}
			err := startSlot(t, g, 1)
			if (err == nil) != c.ok {
				t.Fatalf("err=%v, want ok=%v", err, c.ok)
			}
			if err != nil {
				if !errors.Is(err, ErrNotReady) || !strings.Contains(err.Error(), "of 8G, need 1G reserve") {
					t.Errorf("err: %v", err)
				}
				if len(f.containers) != len(c.usage) {
					t.Errorf("containers created on deny: %v", f.containers)
				}
			}
		})
	}
}

func TestMemoryAdmissionReserveFloor(t *testing.T) {
	// Seven idle slots of other pools each count as 1G: 7G + 1G reserve fits 8G, an eighth does not.
	g, f := memGuard(t, "8G")
	for i := 1; i <= 8; i++ {
		f.containers[fmt.Sprintf("lite-runner-%d", i)] = &fakeContainer{state: "running", spec: ContainerSpec{Labels: map[string]string{labelInstance: "lite", labelSlot: strconv.Itoa(i), labelRole: "runner"}}}
		if i == 7 {
			if err := startSlot(t, g, 1); err != nil {
				t.Fatalf("7 idle slots: %v", err)
			}
			g.Stop(context.Background(), 1)
		}
	}
	if err := startSlot(t, g, 1); !errors.Is(err, ErrNotReady) {
		t.Fatalf("8 idle slots: %v", err)
	}
}

func TestMemoryStatsErrorFailsOpen(t *testing.T) {
	g, f := memGuard(t, "8G")
	f.containers["lite-runner-1"] = &fakeContainer{state: "running", spec: ContainerSpec{Labels: map[string]string{labelInstance: "lite", labelSlot: "1", labelRole: "runner"}}}
	f.memErr = errors.New("stats broke")
	if err := startSlot(t, g, 1); err != nil {
		t.Fatalf("blocked on stats error: %v", err)
	}
}

func TestMemoryDisabledMakesNoStatsCalls(t *testing.T) {
	g, f := newGuard(t)
	if err := startSlot(t, g, 1); err != nil {
		t.Fatal(err)
	}
	g.Reap(context.Background())
	if f.memCalls != 0 {
		t.Errorf("stats calls with no budget: %d", f.memCalls)
	}
}

func TestMemoryBackstop(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*Guard, *fakeDocker) {
		g, f := memGuard(t, "8G")
		for _, n := range []int{1, 2} {
			if err := startSlot(t, g, n); err != nil {
				t.Fatal(err)
			}
		}
		f.containers["ci-runner-1"].created = time.Now().Add(-time.Hour)
		f.containers["ci-runner-2"].created = time.Now().Add(-time.Minute)
		return g, f
	}
	alive := func(f *fakeDocker, n int) bool { return f.containers[fmt.Sprintf("ci-runner-%d", n)] != nil }

	t.Run("kills newest after 3 ticks and resets", func(t *testing.T) {
		g, f := setup(t)
		f.mem["ci-runner-1"] = 5 * gib
		f.mem["ci-runner-2"] = 5 * gib
		g.Reap(ctx)
		g.Reap(ctx)
		if !alive(f, 1) || !alive(f, 2) {
			t.Fatal("killed before 3 ticks")
		}
		g.Reap(ctx)
		if !alive(f, 1) || alive(f, 2) {
			t.Fatalf("want only newest (2) killed: %v", f.containers)
		}
		if f.containers["ci-dind-2"] != nil {
			t.Error("dind of killed slot left behind")
		}
		g.Reap(ctx) // 10G was only slot 1's 5G + ... still counted? slot 2 gone from fake list but mem map stays
		g.Reap(ctx)
		if !alive(f, 1) {
			t.Fatal("counter not reset: second kill after 2 ticks")
		}
	})
	t.Run("under-budget tick resets", func(t *testing.T) {
		g, f := setup(t)
		f.mem["ci-runner-1"] = 9 * gib
		g.Reap(ctx)
		g.Reap(ctx)
		f.mem["ci-runner-1"] = gib
		g.Reap(ctx)
		f.mem["ci-runner-1"] = 9 * gib
		g.Reap(ctx)
		g.Reap(ctx)
		if !alive(f, 1) || !alive(f, 2) {
			t.Fatal("killed despite reset")
		}
		g.Reap(ctx)
		if alive(f, 2) {
			t.Fatal("not killed after 3 consecutive")
		}
	})
	t.Run("ignores starting slots", func(t *testing.T) {
		g, f := setup(t)
		f.mem["ci-runner-1"] = 9 * gib
		g.mu.Lock()
		g.starting[2] = true
		g.mu.Unlock()
		for range 3 {
			g.Reap(ctx)
		}
		if !alive(f, 2) || alive(f, 1) {
			t.Fatalf("want slot 1 killed, starting slot 2 kept: %v", f.containers)
		}
	})
	t.Run("no running slots does nothing", func(t *testing.T) {
		g, f := memGuard(t, "8G")
		f.containers["lite-runner-1"] = &fakeContainer{state: "running", spec: ContainerSpec{Labels: map[string]string{labelInstance: "lite", labelSlot: "1", labelRole: "runner"}}}
		f.mem["lite-runner-1"] = 20 * gib
		for range 5 {
			g.Reap(ctx)
		}
		if f.memCalls != 0 || f.containers["lite-runner-1"] == nil {
			t.Errorf("calls %d containers %v", f.memCalls, f.containers)
		}
	})
}

func TestMemoryAndCPUConfigValidation(t *testing.T) {
	cases := []struct {
		name, old, new string
		ok             bool
	}{
		{"budget only", "prefix: ci", "prefix: ci\nmemory_budget: 8G", true},
		{"budget and reserve", "prefix: ci", "prefix: ci\nmemory_budget: 8G\nmemory_reserve: 2G", true},
		{"reserve equals budget", "prefix: ci", "prefix: ci\nmemory_budget: 8G\nmemory_reserve: 8G", true},
		{"one slot over budget", "prefix: ci", "prefix: ci\nmemory_budget: 4G", false},
		{"reserve over budget", "prefix: ci", "prefix: ci\nmemory_budget: 1G\nmemory_reserve: 2G", false},
		{"default reserve over small budget", "prefix: ci", "prefix: ci\nmemory_budget: 512M", false},
		{"bad budget", "prefix: ci", "prefix: ci\nmemory_budget: lots", false},
		{"bad reserve", "prefix: ci", "prefix: ci\nmemory_budget: 8G\nmemory_reserve: x", false},
		{"reserve without budget", "prefix: ci", "prefix: ci\nmemory_reserve: 1G", false},
		{"ramp off", "prefix: ci", "prefix: ci\nmemory_budget: 8G\nmemory_ramp: 0s", true},
		{"ramp and share", "prefix: ci", "prefix: ci\nmemory_budget: 8G\nmemory_ramp: 5m\nmemory_ramp_share: 0.75", true},
		{"ramp negative", "prefix: ci", "prefix: ci\nmemory_budget: 8G\nmemory_ramp: -1m", false},
		{"ramp too long", "prefix: ci", "prefix: ci\nmemory_budget: 8G\nmemory_ramp: 2h", false},
		{"share over 1", "prefix: ci", "prefix: ci\nmemory_budget: 8G\nmemory_ramp_share: 1.5", false},
		{"share negative", "prefix: ci", "prefix: ci\nmemory_budget: 8G\nmemory_ramp_share: -0.5", false},
		{"ramp without budget", "prefix: ci", "prefix: ci\nmemory_ramp: 1m", false},
		{"cpu_shares ok", "  cpus: 2", "  cpus: 2\n  cpu_shares: 512", true},
		{"cpu_shares 1", "  cpus: 2", "  cpus: 2\n  cpu_shares: 1", false},
		{"cpu_shares huge", "  cpus: 2", "  cpus: 2\n  cpu_shares: 262145", false},
		{"cpu_shares negative", "  cpus: 2", "  cpus: 2\n  cpu_shares: -5", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "guard.yaml")
			os.WriteFile(p, []byte(strings.Replace(testConfig, c.old, c.new, 1)), 0o600)
			if _, err := LoadConfig(p); (err == nil) != c.ok {
				t.Fatalf("err=%v, want ok=%v", err, c.ok)
			}
		})
	}
	g, _ := memGuard(t, "8G")
	if g.cfg.memBudget != 8*gib || g.cfg.memReserve != gib {
		t.Errorf("parsed %d %d", g.cfg.memBudget, g.cfg.memReserve)
	}
}

func TestCPUSharesInHostConfig(t *testing.T) {
	g, f := newGuard(t)
	g.cfg.Runner.CPUShares = 512
	if err := g.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := startSlot(t, g, 1); err != nil {
		t.Fatal(err)
	}
	if got := f.containers["ci-runner-1"].spec.HostConfig.CPUShares; got != 512 {
		t.Errorf("runner CpuShares %d", got)
	}
	if got := f.containers["ci-dind-1"].spec.HostConfig.CPUShares; got != 0 {
		t.Errorf("dind CpuShares %d", got)
	}
	b, _ := json.Marshal(f.containers["ci-runner-1"].spec.HostConfig)
	if !strings.Contains(string(b), `"CpuShares":512`) {
		t.Errorf("json: %s", b)
	}
}

// Two starts racing on one guard must not both be admitted against the same free memory.
func TestMemoryAdmissionConcurrentStarts(t *testing.T) {
	// 1.5G budget, 1G reserve, nothing running: three concurrent starts. Once the first is marked starting it counts
	// as at least the reserve (1G), so the next would need 1G + 1G > 1.5G and must wait. Without admitMu all three
	// would see an empty host and be admitted.
	g, _ := memGuard(t, "8G")
	g.cfg.memBudget = 3 * gib / 2
	// Hold each start at a barrier placed between the budget check and claiming the slot, until all three have reached
	// it (or 300ms pass, which is what happens when admitMu correctly lets only one in at a time). Without the lock all
	// three would have passed the check against the same empty host and would all be admitted.
	var arrived atomic.Int32
	g.afterAdmit = func() {
		arrived.Add(1)
		for deadline := time.Now().Add(300 * time.Millisecond); arrived.Load() < 3 && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
	}
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(n int) { defer wg.Done(); errs[n] = startSlot(t, g, n+1) }(i)
	}
	wg.Wait()
	ok, denied := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrNotReady):
			denied++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || denied != 2 {
		t.Fatalf("admitted %d, denied %d; want 1 and 2 (errs=%v)", ok, denied, errs)
	}
}

// Another pool's slot that has containers but none running yet (it is mid-start) still counts as the reserve.
func TestMemoryAdmissionCountsCreatedContainers(t *testing.T) {
	g, f := memGuard(t, "8G")
	g.cfg.memBudget = 2 * gib
	f.containers["lite-dind-1"] = &fakeContainer{state: "created", spec: ContainerSpec{Labels: map[string]string{labelInstance: "lite", labelSlot: "1", labelRole: "dind"}}}
	if err := startSlot(t, g, 1); err != nil {
		t.Fatalf("1G in use of 2G, 1G reserve fits: %v", err)
	}
	g.Stop(context.Background(), 1)
	f.containers["lite-dind-2"] = &fakeContainer{state: "created", spec: ContainerSpec{Labels: map[string]string{labelInstance: "lite", labelSlot: "2", labelRole: "dind"}}}
	if err := startSlot(t, g, 1); !errors.Is(err, ErrNotReady) {
		t.Fatalf("two mid-start slots (2G) must deny: %v", err)
	}
}

func cacheGuard(t *testing.T) (*Guard, *fakeDocker) {
	t.Helper()
	g, f := newGuard(t)
	g.cfg.CacheDirs = []string{"/srv/rs/{slot}/cache", "/srv/rs/{slot}/docker"}
	if err := g.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return g, f
}

// cacheWipes returns the host paths each cache wipe covered.
func cacheWipes(f *fakeDocker) [][]string {
	var out [][]string
	for _, w := range f.wipes {
		if len(w.HostConfig.Binds) > 0 && strings.Contains(w.HostConfig.Binds[0], ":/wipe/") {
			var hosts []string
			for _, b := range w.HostConfig.Binds {
				hosts = append(hosts, strings.SplitN(b, ":", 2)[0])
			}
			out = append(out, hosts)
		}
	}
	return out
}

func TestCachesWipedAtTeardownUnlessVouched(t *testing.T) {
	ctx := context.Background()
	g, f := cacheGuard(t)
	if err := startSlot(t, g, 1); err != nil {
		t.Fatal(err)
	}
	if err := g.Stop(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got, want := cacheWipes(f), [][]string{{"/srv/rs/1/cache", "/srv/rs/1/docker"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unvouched stop wiped %v, want %v", got, want)
	}
	for _, w := range f.wipes {
		if w.HostConfig.NetworkMode != "none" || w.HostConfig.Privileged {
			t.Errorf("wiper spec: %+v", w.HostConfig)
		}
	}

	f.wipes = nil
	if err := startSlot(t, g, 2); err != nil {
		t.Fatal(err)
	}
	if err := g.KeepCache(ctx, 2, "ci-2-abcd12"); err != nil {
		t.Fatal(err)
	}
	if err := g.Stop(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if w := cacheWipes(f); len(w) != 0 {
		t.Fatalf("vouched stop wiped %v", w)
	}

	// The vouch is for one runner only: the next one on the slot starts without it.
	if err := startSlot(t, g, 2); err != nil {
		t.Fatal(err)
	}
	if err := g.Stop(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if w := cacheWipes(f); len(w) != 1 {
		t.Fatalf("second runner's stop wiped %v, want one wipe", w)
	}
}

func TestKeepCacheRejectsWrongRunner(t *testing.T) {
	g, _ := cacheGuard(t)
	ctx := context.Background()
	if err := startSlot(t, g, 1); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		slot   int
		runner string
		want   error
	}{{1, "ci-1-zzzzzz", ErrConflict}, {1, "", ErrConflict}, {3, "ci-3-abcd12", ErrConflict}, {9, "ci-9-abcd12", ErrBadRequest}} {
		if err := g.KeepCache(ctx, c.slot, c.runner); !errors.Is(err, c.want) {
			t.Errorf("KeepCache(%d,%q) = %v, want %v", c.slot, c.runner, err, c.want)
		}
	}
}

func TestReapWipesCachesOfExitedRunner(t *testing.T) {
	g, f := cacheGuard(t)
	if err := startSlot(t, g, 1); err != nil {
		t.Fatal(err)
	}
	f.containers["ci-runner-1"].state = "exited"
	g.Reap(context.Background())
	if w := cacheWipes(f); len(w) != 1 {
		t.Fatalf("reap wiped %v, want one wipe", w)
	}
}

func TestFailedCacheWipeBlocksReuse(t *testing.T) {
	g, f := cacheGuard(t)
	ctx := context.Background()
	if err := startSlot(t, g, 1); err != nil {
		t.Fatal(err)
	}
	f.wipeExit = 1
	if err := g.Stop(ctx, 1); err == nil {
		t.Fatal("stop should report the failed wipe")
	}
	if err := startSlot(t, g, 1); !errors.Is(err, ErrNotReady) {
		t.Fatalf("start after failed wipe = %v, want ErrNotReady", err)
	}
	f.wipeExit = 0
	if err := startSlot(t, g, 1); err != nil {
		t.Fatalf("start after wipe works again: %v", err)
	}
}

func TestWipeStaleEmptiesCaches(t *testing.T) {
	g, f := cacheGuard(t)
	f.containers["ci-runner-2"] = &fakeContainer{state: "running", spec: ContainerSpec{Labels: g.labels(2, "runner", "ci-2-abcd12")}}
	g.WipeStale(context.Background())
	if got := len(cacheWipes(f)); got != 2 { // slots 1 and 3; slot 2 is in use
		t.Fatalf("startup wiped caches of %d slots, want 2", got)
	}
}

func TestCacheDirsValidation(t *testing.T) {
	for _, d := range []string{"relative/{slot}", "/srv/cache", "/srv/{slot}/../x", "/srv/a:b/{slot}", "/"} {
		g, _ := newGuard(t)
		g.cfg.CacheDirs = []string{d}
		if err := g.cfg.Validate(); err == nil {
			t.Errorf("cache_dirs %q accepted", d)
		}
	}
}

func TestListWithStatsSumsContainersAndComputesCPU(t *testing.T) {
	g, f := memGuard(t, "8G")
	f.cpu = map[string]int64{}
	if err := startSlot(t, g, 1); err != nil {
		t.Fatal(err)
	}
	f.mem["ci-runner-1"], f.mem["ci-dind-1"] = 100<<20, 50<<20
	if plain, _ := g.List(context.Background()); plain[0].Stats != nil || f.statCalls != 0 {
		t.Fatalf("List must not sample stats: %+v calls=%d", plain[0].Stats, f.statCalls)
	}
	first, err := g.ListWithStats(context.Background())
	if err != nil || len(first) != 1 || first[0].Stats == nil {
		t.Fatalf("%+v %v", first, err)
	}
	st := first[0].Stats
	if st.MemBytes != 150<<20 || st.PIDs != 6 || st.NetRxBytes != 20 || st.NetTxBytes != 40 || st.BlkWriteBytes != 4 {
		t.Errorf("not summed over runner+dind: %+v", st)
	}
	if st.CPUPercent != 0 {
		t.Errorf("first sample has no rate: %v", st.CPUPercent)
	}
	time.Sleep(20 * time.Millisecond)
	f.cpu["ci-runner-1"] = 10_000_000 // 10ms of CPU in ~20ms+ wall: a sizeable fraction of a core
	second, _ := g.ListWithStats(context.Background())
	if c := second[0].Stats.CPUPercent; c <= 5 || c > 60 {
		t.Errorf("cpu%% = %v", c)
	}
}

func TestSlowStopDoesNotBlockOtherSlots(t *testing.T) {
	g, f := newGuard(t)
	ctx := context.Background()
	org := "https://github.com/example-org"
	for _, n := range []int{1, 2} {
		name := fmt.Sprintf("ci-%d-abcd12", n)
		if err := g.Start(ctx, n, api.StartRequest{RunnerName: name, JITConfig: jitFor(name, org)}); err != nil {
			t.Fatal(err)
		}
	}
	f.execGate, f.execEntered = make(chan struct{}), make(chan struct{}, 1)
	stopped := make(chan error, 1)
	go func() { stopped <- g.Stop(ctx, 1) }()
	<-f.execEntered // slot 1's teardown is now stuck in its prune

	done := make(chan struct{})
	go func() {
		defer close(done)
		slots, err := g.List(ctx)
		if err != nil || len(slots) != 2 || !slots[0].Starting || slots[1].Starting {
			t.Errorf("List during stop: %+v, %v", slots, err)
		}
		name := "ci-3-abcd12"
		if err := g.Start(ctx, 3, api.StartRequest{RunnerName: name, JITConfig: jitFor(name, org)}); err != nil {
			t.Errorf("Start of another slot during stop: %v", err)
		}
		if err := g.Stop(ctx, 1); !errors.Is(err, ErrConflict) {
			t.Errorf("second Stop of a stopping slot: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("List/Start/Stop blocked behind a slow teardown")
	}
	close(f.execGate)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if slots, _ := g.List(ctx); len(slots) != 2 || slots[0].Slot != 2 {
		t.Errorf("after stop: %+v", slots)
	}
}

func TestMemoryRampCountsYoungSlotsAtAShareOfTheirLimit(t *testing.T) {
	// Two slots of another pool, each with an 8G limit but only 100M in use. Budget 8G, reserve 1G.
	// Young: each counts max(100M, 1G, 8G*0.5) = 4G, 8G in all, so no room for another. Settled: 1G each, plenty.
	cases := []struct {
		name  string
		age   time.Duration
		extra string // appended to the config
		ok    bool
	}{
		{"young slots reserve their ramp share", 10 * time.Second, "", false},
		{"settled slots count as measured", 10 * time.Minute, "", true},
		{"ramp off", 10 * time.Second, "memory_ramp: 0s", true},
		{"small share", 10 * time.Second, "memory_ramp_share: 0.25", true}, // 2G each, 4G in all
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, f := newGuard(t)
			g.cfg.MemoryBudget, g.cfg.MemoryReserve = "8G", "1G"
			if strings.Contains(c.extra, "memory_ramp: 0s") {
				g.cfg.MemoryRamp = &Duration{}
			}
			if strings.Contains(c.extra, "memory_ramp_share") {
				g.cfg.MemoryRampShare = 0.25
			}
			if err := g.cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			f.mem = map[string]int64{}
			for _, n := range []string{"1", "2"} {
				name := "lite-runner-" + n
				f.containers[name] = &fakeContainer{
					state: "running", created: time.Now().Add(-c.age),
					spec: ContainerSpec{
						Labels:     map[string]string{labelInstance: "lite", labelSlot: n, labelRole: "runner"},
						HostConfig: HostConfig{Memory: 8 * gib},
					},
				}
				f.mem[name] = 100 << 20
			}
			err := startSlot(t, g, 1)
			if (err == nil) != c.ok {
				t.Fatalf("err=%v, want ok=%v", err, c.ok)
			}
			if err != nil && !errors.Is(err, ErrNotReady) {
				t.Errorf("err: %v", err)
			}
		})
	}
}

func TestMemoryRampDefaults(t *testing.T) {
	g, _ := memGuard(t, "8G")
	if g.cfg.memRamp != 2*time.Minute || g.cfg.memRampShare != 0.5 {
		t.Errorf("defaults: ramp %v share %v", g.cfg.memRamp, g.cfg.memRampShare)
	}
}

func TestMemoryHighIsAppliedFromAOneShotContainer(t *testing.T) {
	g, f := newGuard(t)
	g.cfg.Runner.MemoryHigh, g.cfg.Dind.MemoryHigh = "2560M", "1G"
	if err := g.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := startSlot(t, g, 1); err != nil {
		t.Fatal(err)
	}
	if len(f.wipes) != 1 {
		t.Fatalf("want one one-shot container, got %d", len(f.wipes))
	}
	w := f.wipes[0]
	if w.HostConfig.NetworkMode != "none" || !slices.Contains(w.HostConfig.Binds, "/sys/fs/cgroup:/cgroup") {
		t.Errorf("one-shot should have no network and mount the cgroup tree: %+v", w.HostConfig)
	}
	args := strings.Join(w.Entrypoint, " ")
	for _, want := range []string{"ci-runner-1=" + strconv.FormatInt(2560<<20, 10), "ci-dind-1=" + strconv.FormatInt(1<<30, 10)} {
		if !strings.Contains(args, want) {
			t.Errorf("entrypoint %q lacks %q", args, want)
		}
	}
	if f.containers["ci-wipe-1"] != nil {
		t.Error("one-shot container left behind")
	}
}

func TestMemoryHighFailureDoesNotFailTheStart(t *testing.T) {
	g, f := newGuard(t)
	g.cfg.Runner.MemoryHigh = "2G"
	if err := g.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	f.wipeExit = 1 // the one-shot reports it could not write
	if err := startSlot(t, g, 1); err != nil {
		t.Fatalf("start failed because memory_high was not applied: %v", err)
	}
	if f.containers["ci-runner-1"] == nil {
		t.Error("runner not started")
	}
}

func TestNoMemoryHighNoOneShot(t *testing.T) {
	g, f := newGuard(t)
	if err := startSlot(t, g, 1); err != nil {
		t.Fatal(err)
	}
	if len(f.wipes) != 0 {
		t.Errorf("one-shot container started without memory_high: %d", len(f.wipes))
	}
}

func TestMemoryHighValidation(t *testing.T) {
	cases := []struct {
		name, old, new string
		ok             bool
	}{
		{"below memory", "  memory: 3G", "  memory: 3G\n  memory_high: 2G", true},
		{"equal to memory", "  memory: 3G", "  memory: 3G\n  memory_high: 3G", false},
		{"above memory", "  memory: 3G", "  memory: 3G\n  memory_high: 4G", false},
		{"not a size", "  memory: 3G", "  memory: 3G\n  memory_high: lots", false},
		{"zero", "  memory: 3G", "  memory: 3G\n  memory_high: 0", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "guard.yaml")
			if err := os.WriteFile(p, []byte(strings.Replace(testConfig, c.old, c.new, 1)), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(p); (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok = %v", err, c.ok)
			}
		})
	}
}

func TestCountersTrackWhatHappened(t *testing.T) {
	g, f := memGuard(t, "8G")
	h := g.Handler()
	counters := func() map[string]int64 {
		rec := do(t, h, "GET", "/v1/counters", nil)
		if rec.Code != 200 {
			t.Fatalf("counters: %d", rec.Code)
		}
		var m map[string]int64
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	for _, k := range []string{CounterDenied, CounterNotReady, CounterConflicts, CounterAdmissionRefused, CounterStarts, CounterStartFailures, "reaped_memory_budget_total"} {
		if v, ok := counters()[k]; !ok || v != 0 {
			t.Fatalf("%s should start at 0, got %v (present: %v)", k, v, ok)
		}
	}

	if err := startSlot(t, g, 1); err != nil { // a start
		t.Fatal(err)
	}
	if err := startSlot(t, g, 1); !errors.Is(err, ErrConflict) { // the slot is in use
		t.Fatalf("second start: %v", err)
	}
	do(t, h, "PUT", "/v1/slots/1", `{"runner_name":"x","jit_config":"y"}`) // refused: bad runner name
	do(t, h, "GET", "/v1/nothing", nil)                                    // unknown endpoint
	f.containers["ci-runner-2"] = &fakeContainer{state: "running", spec: ContainerSpec{Labels: map[string]string{labelInstance: "ci", labelSlot: "2", labelRole: "runner"}}}
	f.mem["ci-runner-2"] = 8 * gib
	if err := startSlot(t, g, 3); !errors.Is(err, ErrNotReady) { // memory budget full
		t.Fatalf("start over budget: %v", err)
	}
	do(t, h, "PUT", "/v1/slots/3", fmt.Sprintf(`{"runner_name":"ci-3-abcd12","jit_config":%q}`, jitFor("ci-3-abcd12", "https://github.com/example-org"))) // 503 through the API
	f.containers["ci-runner-1"].state = "exited"
	g.Reap(context.Background()) // a runner that exited

	c := counters()
	want := map[string]int64{
		CounterStarts: 1, CounterDenied: 2, CounterConflicts: 0, CounterAdmissionRefused: 2, CounterNotReady: 1,
		"reaped_runner_exited_total": 1,
	}
	for k, v := range want {
		if c[k] != v {
			t.Errorf("%s = %d, want %d (all: %v)", k, c[k], v, c)
		}
	}
}

func TestInfoCarriesTheMemoryBudget(t *testing.T) {
	g, _ := memGuard(t, "8G")
	if got := g.Info().MemoryBudget; got != 8*gib {
		t.Errorf("MemoryBudget = %d", got)
	}
	g2, _ := newGuard(t)
	if g2.Info().MemoryBudget != 0 {
		t.Error("budget reported without one set")
	}
}

func TestModuleBlocklistIsWrittenFromANetworklessOneShot(t *testing.T) {
	g, f := newGuard(t)
	g.cfg.ModuleBlocklist = []string{"ifb", "bonding", "nf-foo"}
	if err := g.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := g.ApplyModuleBlocklist(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.wipes) != 1 {
		t.Fatalf("want one one-shot container, got %d", len(f.wipes))
	}
	w := f.wipes[0]
	if w.HostConfig.NetworkMode != "none" || w.HostConfig.Privileged || !slices.Equal(w.HostConfig.Binds, []string{"/etc/modprobe.d:/modprobe.d"}) {
		t.Errorf("one-shot must have no network, not be privileged, and mount only /etc/modprobe.d: %+v", w.HostConfig)
	}
	args := strings.Join(w.Entrypoint, " ")
	for _, want := range []string{"50-runner-scaling-ci.conf", "ifb bonding nf-foo"} {
		if !strings.Contains(args, want) {
			t.Errorf("entrypoint %q lacks %q", args, want)
		}
	}
	if f.containers["ci-wipe-0"] != nil {
		t.Error("one-shot container left behind")
	}
}

func TestModuleBlocklistOmittedTouchesNothing(t *testing.T) {
	g, f := newGuard(t)
	if err := g.ApplyModuleBlocklist(context.Background()); err != nil || len(f.wipes) != 0 {
		t.Fatalf("omitted option must not start a container: err=%v wipes=%d", err, len(f.wipes))
	}
	g.cfg.ModuleBlocklist = []string{} // explicit empty list: clean up the guard's own file
	if err := g.ApplyModuleBlocklist(context.Background()); err != nil || len(f.wipes) != 1 {
		t.Fatalf("explicit empty list should run the cleanup: err=%v wipes=%d", err, len(f.wipes))
	}
}

func TestModuleBlocklistFailureIsCountedNotFatal(t *testing.T) {
	g, f := newGuard(t)
	g.cfg.ModuleBlocklist = []string{"ifb"}
	f.wipeExit = 1
	if err := g.ApplyModuleBlocklist(context.Background()); err == nil {
		t.Fatal("a failed write must be reported")
	}
	if got := g.Counters()[CounterModuleBlocklistFailures]; got != 1 {
		t.Errorf("failure counter = %d, want 1", got)
	}
	if err := startSlot(t, g, 1); err != nil {
		t.Errorf("a failed blocklist must not stop slots from starting: %v", err)
	}
}

func TestModuleBlocklistValidation(t *testing.T) {
	for _, tc := range []struct {
		mods []string
		ok   bool
	}{
		{[]string{"ifb", "bonding", "nf_conntrack", "8021q"}, true},
		{nil, true},
		{[]string{}, true},
		{[]string{"Ifb"}, false},
		{[]string{"ifb;rm -rf /"}, false},
		{[]string{"a b"}, false},
		{[]string{""}, false},
		{[]string{"../x"}, false},
		{[]string{"ifb", "ifb"}, false},
		{[]string{"nf-x", "nf_x"}, false},
	} {
		g, _ := newGuard(t)
		g.cfg.ModuleBlocklist = tc.mods
		if err := g.cfg.Validate(); (err == nil) != tc.ok {
			t.Errorf("%q: ok=%v, err=%v", tc.mods, tc.ok, err)
		}
	}
}

// The script runs for real here (sh against a temp dir) because it is what actually edits the host's modprobe rules.
func TestModprobeScriptBehaviour(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		script := strings.ReplaceAll(modprobeScript, "/modprobe.d", dir)
		out, err := exec.Command("sh", append([]string{"-c", script, "sh"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("script failed: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	file := filepath.Join(dir, "50-x.conf")
	if got := run("50-x.conf", "ifb", "bonding"); got != "written" {
		t.Fatalf("first run: %q", got)
	}
	b, _ := os.ReadFile(file)
	want := "# managed by the runner-scaling guard: do not edit\ninstall ifb /bin/false\ninstall bonding /bin/false\n"
	if string(b) != want {
		t.Fatalf("file content:\n%q\nwant\n%q", b, want)
	}
	if got := run("50-x.conf", "ifb", "bonding"); got != "unchanged" {
		t.Errorf("same list twice: %q", got)
	}
	if got := run("50-x.conf", "ifb"); got != "written" {
		t.Errorf("changed list: %q", got)
	}
	if _, err := os.Stat(file + ".tmp"); err == nil {
		t.Error("temp file left behind")
	}
	if got := run("50-x.conf"); got != "removed" {
		t.Errorf("empty list should remove the managed file: %q", got)
	}
	// a file the guard did not write must never be removed or replaced by an empty list
	foreign := filepath.Join(dir, "60-mine.conf")
	os.WriteFile(foreign, []byte("blacklist foo\n"), 0o644)
	if got := run("60-mine.conf"); got != "nothing" {
		t.Errorf("foreign file with empty list: %q", got)
	}
	if b, _ := os.ReadFile(foreign); string(b) != "blacklist foo\n" {
		t.Error("foreign file was modified")
	}
}
