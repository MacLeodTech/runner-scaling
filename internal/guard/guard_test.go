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
	mu         sync.Mutex
	containers map[string]*fakeContainer // by name
	volumes    map[string]bool
	networks   map[string]string
	execs      [][]string
	pulled     []string
	dindHealth string
	wipes      []ContainerSpec
	wipeExit   int              // exit code of one-shot wipers
	mem        map[string]int64 // by container name
	memErr     error
	memCalls   int
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
	st := &ContainerState{Status: c.state}
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
func (f *fakeDocker) Exec(_ context.Context, _ string, cmd []string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs = append(f.execs, cmd)
	return 0, nil
}

const testConfig = `
github_url: https://github.com/MacLeodTech
tenant_url: https://pipelines.actions.githubusercontent.com/OURS/
prefix: homelab
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

// jitFor makes a JIT config for a runner. org selects the tenant: the guard's own ("https://github.com/MacLeodTech")
// or another one.
func jitFor(name, org string) string {
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	tenant := "OURS"
	if org != "https://github.com/MacLeodTech" {
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
	rec := do(t, h, "PUT", "/v1/slots/2", api.StartRequest{RunnerName: "homelab-2-abcd12", JITConfig: jitFor("homelab-2-abcd12", "https://github.com/MacLeodTech")})
	if rec.Code != http.StatusCreated {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	r, d := f.containers["homelab-runner-2"], f.containers["homelab-dind-2"]
	if r == nil || d == nil {
		t.Fatalf("containers: %v", f.containers)
	}
	if r.spec.HostConfig.Privileged || !d.spec.HostConfig.Privileged {
		t.Error("only dind may be privileged")
	}
	if !slices.Contains(r.spec.Env, "DOCKER_HOST=tcp://homelab-dind-2:2375") || !slices.Contains(d.spec.Env, "DOCKER_TLS_CERTDIR=") {
		t.Errorf("env: runner %v dind %v", r.spec.Env, d.spec.Env)
	}
	if !slices.Contains(r.spec.HostConfig.Binds, "/srv/runners/2/cache:/cache") || !slices.Contains(r.spec.HostConfig.Binds, "homelab-work-2:/runner/work") {
		t.Errorf("runner binds: %v", r.spec.HostConfig.Binds)
	}
	if !slices.Contains(d.spec.HostConfig.Binds, "homelab-work-2:/runner/work") {
		t.Errorf("dind binds: %v", d.spec.HostConfig.Binds)
	}
	if f.networks["homelab-slot-2"] != "172.41.2.0/24" || !f.volumes["homelab-work-2"] {
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
	rec = do(t, h, "PUT", "/v1/slots/2", api.StartRequest{RunnerName: "homelab-2-zzzz99", JITConfig: jitFor("homelab-2-zzzz99", "https://github.com/MacLeodTech")})
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
	good := jitFor("homelab-1-abcd12", "https://github.com/MacLeodTech")
	cases := []struct {
		name, method, path string
		body               any
		want               int
	}{
		{"slot 0", "PUT", "/v1/slots/0", api.StartRequest{RunnerName: "homelab-0-abcd12", JITConfig: good}, 400},
		{"slot over max", "PUT", "/v1/slots/4", api.StartRequest{RunnerName: "homelab-4-abcd12", JITConfig: good}, 400},
		{"slot not a number", "PUT", "/v1/slots/1x", api.StartRequest{RunnerName: "homelab-1-abcd12", JITConfig: good}, 400},
		{"name for another slot", "PUT", "/v1/slots/1", api.StartRequest{RunnerName: "homelab-2-abcd12", JITConfig: jitFor("homelab-2-abcd12", "https://github.com/MacLeodTech")}, 400},
		{"name with wrong prefix", "PUT", "/v1/slots/1", api.StartRequest{RunnerName: "evil-1-abcd12", JITConfig: jitFor("evil-1-abcd12", "https://github.com/MacLeodTech")}, 400},
		{"jit names another runner", "PUT", "/v1/slots/1", api.StartRequest{RunnerName: "homelab-1-abcd12", JITConfig: jitFor("homelab-1-other1", "https://github.com/MacLeodTech")}, 400},
		{"jit for another org", "PUT", "/v1/slots/1", api.StartRequest{RunnerName: "homelab-1-abcd12", JITConfig: jitFor("homelab-1-abcd12", "https://github.com/evil")}, 400},
		{"unknown field", "PUT", "/v1/slots/1", `{"runner_name":"homelab-1-abcd12","jit_config":"x","image":"evil"}`, 400},
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
	err := g.Start(context.Background(), 1, api.StartRequest{RunnerName: "homelab-1-abcd12", JITConfig: jitFor("homelab-1-abcd12", "https://github.com/MacLeodTech")})
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
		name := fmt.Sprintf("homelab-%d-abcd12", n)
		if err := g.Start(ctx, n, api.StartRequest{RunnerName: name, JITConfig: jitFor(name, "https://github.com/MacLeodTech")}); err != nil {
			t.Fatal(err)
		}
	}
	f.containers["homelab-runner-1"].state = "exited"
	g.Reap(ctx)
	if f.containers["homelab-dind-1"] != nil || f.containers["homelab-runner-1"] != nil {
		t.Error("slot 1 not reaped")
	}
	if f.containers["homelab-runner-2"] == nil {
		t.Error("slot 2 reaped while running")
	}
	slots, _ := g.List(ctx)
	if len(slots) != 1 || slots[0].Slot != 2 || slots[0].RunnerName != "homelab-2-abcd12" {
		t.Errorf("slots: %+v", slots)
	}
}

func TestConfigRejects(t *testing.T) {
	base := testConfig
	cases := map[string][2]string{
		"unknown key":       {"prefix: homelab", "prefix: homelab\nimage_override: x"},
		"bad prefix":        {"prefix: homelab", "prefix: Home_Lab"},
		"http github url":   {"https://github.com/MacLeodTech", "http://github.com/MacLeodTech"},
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

func TestExampleConfigLoads(t *testing.T) {
	if _, err := LoadConfig("../../examples/guard.yaml"); err != nil {
		t.Fatal(err)
	}
}

func TestWorkBindIsWipedThroughDind(t *testing.T) {
	g, f := newGuard(t)
	g.cfg.WorkBind = "/big/runners/{slot}/work"
	if err := g.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := g.Start(ctx, 1, api.StartRequest{RunnerName: "homelab-1-abcd12", JITConfig: jitFor("homelab-1-abcd12", "https://github.com/MacLeodTech")}); err != nil {
		t.Fatal(err)
	}
	if len(f.volumes) != 0 {
		t.Errorf("created a volume in bind mode: %v", f.volumes)
	}
	if b := f.containers["homelab-runner-1"].spec.HostConfig.Binds; !slices.Contains(b, "/big/runners/1/work:/runner/work") {
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
	req := api.StartRequest{RunnerName: "homelab-1-abcd12", JITConfig: jitFor("homelab-1-abcd12", "https://github.com/MacLeodTech")}
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
	if err := g.Start(ctx, 2, api.StartRequest{RunnerName: "homelab-2-abcd12", JITConfig: jitFor("homelab-2-abcd12", "https://github.com/MacLeodTech")}); err != nil {
		t.Fatal(err)
	}
	if len(f.wipes) != 1 {
		t.Fatalf("wipes: %d", len(f.wipes))
	}
	w := f.wipes[0]
	if w.HostConfig.NetworkMode != "none" || w.HostConfig.Privileged || len(w.HostConfig.Binds) != 1 || w.HostConfig.Binds[0] != "/big/lite/2/work:/runner/work" {
		t.Errorf("wiper spec: %+v", w.HostConfig)
	}
	r := f.containers["homelab-runner-2"]
	if r == nil || slices.ContainsFunc(r.spec.Env, func(e string) bool { return strings.HasPrefix(e, "DOCKER_HOST=") }) {
		t.Fatalf("runner: %+v", r)
	}
	if r.spec.HostConfig.CgroupParent != "runner-scaling.slice" || w.HostConfig.CgroupParent != "runner-scaling.slice" {
		t.Errorf("cgroup parent not applied")
	}
	if f.containers["homelab-wipe-2"] != nil {
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
			f.containers["homelab-runner-2"] = &fakeContainer{state: "running", spec: ContainerSpec{Labels: g.labels(2, "runner", "homelab-2-abcd12")}}
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
			name := "homelab-1-abcd12"
			if err := g.Start(ctx, 1, api.StartRequest{RunnerName: name, JITConfig: jitFor(name, "https://github.com/MacLeodTech")}); err != nil {
				t.Fatal(err)
			}
			f.containers["homelab-runner-1"].created = time.Now().Add(-c.age)
			if c.starting {
				g.mu.Lock()
				g.starting[1] = true
				g.mu.Unlock()
			}
			g.Reap(ctx)
			if gone := f.containers["homelab-runner-1"] == nil; gone != c.reaped {
				t.Errorf("reaped=%v, want %v", gone, c.reaped)
			}
		})
	}
}

func TestMaxRuntimeValidation(t *testing.T) {
	for in, ok := range map[string]bool{"max_runtime: 0s": true, "max_runtime: 6h": true, "max_runtime: 1m": true, "max_runtime: 30s": false, "max_runtime: -1h": false} {
		p := filepath.Join(t.TempDir(), "guard.yaml")
		os.WriteFile(p, []byte(strings.Replace(testConfig, "prefix: homelab", "prefix: homelab\n"+in, 1)), 0o600)
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
	name := fmt.Sprintf("homelab-%d-abcd12", n)
	return g.Start(context.Background(), n, api.StartRequest{RunnerName: name, JITConfig: jitFor(name, "https://github.com/MacLeodTech")})
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
		{"exactly fits", map[string]int64{"homelab-runner-2": 7 * gib}, false, true},
		{"one byte over", map[string]int64{"homelab-runner-2": 7*gib + 1}, false, false},
		{"runner+dind summed", map[string]int64{"homelab-runner-2": 4 * gib, "homelab-dind-2": 4 * gib}, false, false},
		{"small slot counts as reserve", map[string]int64{"homelab-runner-2": 1 << 20}, false, true},
		{"reserve floor denies", nil, false, true},
		{"other pool counts", map[string]int64{"lite-runner-1": 7*gib + 1}, true, false},
		{"other pool fits", map[string]int64{"lite-runner-1": 7 * gib}, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, f := memGuard(t, "8G")
			for n, v := range c.usage {
				pool, slot := "homelab", "2"
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
		f.containers["homelab-runner-1"].created = time.Now().Add(-time.Hour)
		f.containers["homelab-runner-2"].created = time.Now().Add(-time.Minute)
		return g, f
	}
	alive := func(f *fakeDocker, n int) bool { return f.containers[fmt.Sprintf("homelab-runner-%d", n)] != nil }

	t.Run("kills newest after 3 ticks and resets", func(t *testing.T) {
		g, f := setup(t)
		f.mem["homelab-runner-1"] = 5 * gib
		f.mem["homelab-runner-2"] = 5 * gib
		g.Reap(ctx)
		g.Reap(ctx)
		if !alive(f, 1) || !alive(f, 2) {
			t.Fatal("killed before 3 ticks")
		}
		g.Reap(ctx)
		if !alive(f, 1) || alive(f, 2) {
			t.Fatalf("want only newest (2) killed: %v", f.containers)
		}
		if f.containers["homelab-dind-2"] != nil {
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
		f.mem["homelab-runner-1"] = 9 * gib
		g.Reap(ctx)
		g.Reap(ctx)
		f.mem["homelab-runner-1"] = gib
		g.Reap(ctx)
		f.mem["homelab-runner-1"] = 9 * gib
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
		f.mem["homelab-runner-1"] = 9 * gib
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
		{"budget only", "prefix: homelab", "prefix: homelab\nmemory_budget: 8G", true},
		{"budget and reserve", "prefix: homelab", "prefix: homelab\nmemory_budget: 8G\nmemory_reserve: 2G", true},
		{"reserve equals budget", "prefix: homelab", "prefix: homelab\nmemory_budget: 8G\nmemory_reserve: 8G", true},
		{"one slot over budget", "prefix: homelab", "prefix: homelab\nmemory_budget: 4G", false},
		{"reserve over budget", "prefix: homelab", "prefix: homelab\nmemory_budget: 1G\nmemory_reserve: 2G", false},
		{"default reserve over small budget", "prefix: homelab", "prefix: homelab\nmemory_budget: 512M", false},
		{"bad budget", "prefix: homelab", "prefix: homelab\nmemory_budget: lots", false},
		{"bad reserve", "prefix: homelab", "prefix: homelab\nmemory_budget: 8G\nmemory_reserve: x", false},
		{"reserve without budget", "prefix: homelab", "prefix: homelab\nmemory_reserve: 1G", false},
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
	if got := f.containers["homelab-runner-1"].spec.HostConfig.CPUShares; got != 512 {
		t.Errorf("runner CpuShares %d", got)
	}
	if got := f.containers["homelab-dind-1"].spec.HostConfig.CPUShares; got != 0 {
		t.Errorf("dind CpuShares %d", got)
	}
	b, _ := json.Marshal(f.containers["homelab-runner-1"].spec.HostConfig)
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
	if err := g.KeepCache(ctx, 2, "homelab-2-abcd12"); err != nil {
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
	}{{1, "homelab-1-zzzzzz", ErrConflict}, {1, "", ErrConflict}, {3, "homelab-3-abcd12", ErrConflict}, {9, "homelab-9-abcd12", ErrBadRequest}} {
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
	f.containers["homelab-runner-1"].state = "exited"
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
	f.containers["homelab-runner-2"] = &fakeContainer{state: "running", spec: ContainerSpec{Labels: g.labels(2, "runner", "homelab-2-abcd12")}}
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
