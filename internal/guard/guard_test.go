package guard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

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
}

type fakeContainer struct {
	spec  ContainerSpec
	state string
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
	f.containers[n] = &fakeContainer{spec: s, state: "created"}
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
	k, v, _ := strings.Cut(label, "=")
	var out []ContainerSummary
	for n, c := range f.containers {
		if c.spec.Labels[k] == v {
			out = append(out, ContainerSummary{ID: n, Names: []string{"/" + n}, State: c.state, Labels: c.spec.Labels})
		}
	}
	return out, nil
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
