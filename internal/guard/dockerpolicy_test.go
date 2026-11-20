package guard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MacLeodTech/runner-scaling/internal/api"
)

// fakeDaemon is just enough of the Docker Engine API for the guard to run a slot start to finish.
type fakeDaemon struct {
	mu     sync.Mutex
	calls  []string
	ctrs   map[string]*daemonCtr // by name
	byID   map[string]*daemonCtr
	execID int
}

type daemonCtr struct {
	id, name string
	labels   map[string]string
	status   string
	health   bool
}

func newFakeDaemon() *fakeDaemon {
	return &fakeDaemon{ctrs: map[string]*daemonCtr{}, byID: map[string]*daemonCtr{}}
}

func (d *fakeDaemon) find(ref string) *daemonCtr {
	if c := d.ctrs[ref]; c != nil {
		return c
	}
	return d.byID[ref]
}

func (d *fakeDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, r.Method+" "+r.URL.Path)
	path := strings.TrimPrefix(r.URL.Path, "/"+apiVersion)
	b, _ := io.ReadAll(r.Body)
	out := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case r.URL.Path == "/_ping":
		w.Write([]byte("OK"))
	case path == "/containers/create":
		var spec ContainerSpec
		json.Unmarshal(b, &spec)
		name := r.URL.Query().Get("name")
		sum := sha256.Sum256([]byte(name))
		c := &daemonCtr{id: hex.EncodeToString(sum[:]), name: name, labels: spec.Labels, status: "created", health: spec.Healthcheck != nil}
		d.ctrs[name], d.byID[c.id] = c, c
		out(201, map[string]string{"Id": c.id})
	case len(segs) == 3 && segs[0] == "containers" && segs[2] == "start":
		c := d.find(segs[1])
		if c == nil {
			out(404, map[string]string{"message": "no such container"})
			return
		}
		c.status = "running"
		if strings.Contains(c.name, "-wipe-") {
			c.status = "exited"
		}
		w.WriteHeader(204)
	case len(segs) == 3 && segs[0] == "containers" && segs[2] == "json":
		c := d.find(segs[1])
		if c == nil {
			out(404, map[string]string{"message": "no such container"})
			return
		}
		st := map[string]any{"Status": c.status, "ExitCode": 0}
		if c.health {
			st["Health"] = map[string]string{"Status": "healthy"}
		}
		out(200, map[string]any{"State": st, "Config": map[string]any{"Labels": c.labels}, "HostConfig": map[string]any{"Memory": 0}})
	case len(segs) == 3 && segs[0] == "containers" && segs[2] == "stats":
		out(200, map[string]any{})
	case len(segs) == 3 && segs[0] == "containers" && segs[2] == "exec":
		d.execID++
		out(201, map[string]string{"Id": strings.Repeat("e", 60) + string(rune('a'+d.execID%6)) + "000"})
	case len(segs) == 3 && segs[0] == "exec" && segs[2] == "start":
		w.WriteHeader(200)
	case len(segs) == 3 && segs[0] == "exec" && segs[2] == "json":
		out(200, map[string]any{"Running": false, "ExitCode": 0})
	case r.Method == "GET" && path == "/containers/json":
		var all []map[string]any
		for _, c := range d.ctrs {
			all = append(all, map[string]any{"Id": c.id, "Names": []string{"/" + c.name}, "Labels": c.labels, "State": c.status, "Created": 1})
		}
		out(200, all)
	case r.Method == "DELETE" && len(segs) == 2 && segs[0] == "containers":
		if c := d.find(segs[1]); c != nil {
			delete(d.ctrs, c.name)
			delete(d.byID, c.id)
			w.WriteHeader(204)
			return
		}
		out(404, map[string]string{"message": "no such container"})
	case r.Method == "GET" && len(segs) == 2 && segs[0] == "networks":
		out(404, map[string]string{"message": "no such network"})
	case r.Method == "POST" && path == "/networks/create", r.Method == "POST" && path == "/volumes/create":
		out(201, map[string]string{"Id": "x"})
	case r.Method == "DELETE" && segs[0] == "volumes":
		w.WriteHeader(204)
	case r.Method == "GET" && segs[0] == "images":
		out(200, map[string]any{})
	case r.Method == "POST" && path == "/images/create":
		w.Write([]byte("{}"))
	default:
		out(404, map[string]string{"message": "unhandled " + r.Method + " " + r.URL.Path})
	}
}

func (d *fakeDaemon) saw(call string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.calls {
		if c == call {
			return true
		}
	}
	return false
}

// policyRig is a real Guard talking to a real Proxy in front of a fake daemon.
type policyRig struct {
	g      *Guard
	px     *Proxy
	d      *fakeDaemon
	logs   *bytes.Buffer
	sock   string // the proxy's socket
	client *http.Client
}

func newPolicyRig(t *testing.T, mutate func(*Config)) *policyRig {
	t.Helper()
	dir, err := os.MkdirTemp("", "rs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	cfgPath := filepath.Join(dir, "guard.yaml")
	if err := os.WriteFile(cfgPath, []byte(testConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(cfg)
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	}

	daemon := newFakeDaemon()
	dsock := filepath.Join(dir, "d.sock")
	dln, err := net.Listen("unix", dsock)
	if err != nil {
		t.Fatal(err)
	}
	dsrv := &http.Server{Handler: daemon}
	go dsrv.Serve(dln)
	t.Cleanup(func() { dsrv.Close() })

	logs := &bytes.Buffer{}
	px := NewProxy(cfg, dsock, slog.New(slog.NewTextHandler(logs, nil)))
	psock := filepath.Join(dir, "p.sock")
	pln, err := net.Listen("unix", psock)
	if err != nil {
		t.Fatal(err)
	}
	psrv := &http.Server{Handler: px}
	go psrv.Serve(pln)
	t.Cleanup(func() { psrv.Close() })

	g := New(cfg, NewSocketDocker(psock), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &policyRig{g: g, px: px, d: daemon, logs: logs, sock: psock, client: NewSocketDocker(psock).c}
}

func (r *policyRig) denies(t *testing.T) string {
	t.Helper()
	var out []string
	for _, l := range strings.Split(r.logs.String(), "\n") {
		if strings.Contains(l, "DENY") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// raw sends one request straight to the proxy, as a compromised guard would.
func (r *policyRig) raw(t *testing.T, method, path string, body any) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://docker"+path, rd)
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (r *policyRig) start(t *testing.T, slot int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	name := "ci-" + string(rune('0'+slot)) + "-abcd"
	if err := r.g.Start(ctx, slot, api.StartRequest{RunnerName: name, JITConfig: jitFor(name, "https://github.com/example-org")}); err != nil {
		t.Fatalf("start through the policy proxy: %v\nDENY lines:\n%s", err, r.denies(t))
	}
}

// Everything the guard does in normal operation must pass the policy: this is the test that keeps the policy and the
// guard's spec builders from drifting apart.
func TestPolicyAllowsEverythingTheGuardDoes(t *testing.T) {
	variants := map[string]func(*Config){
		"privileged dind, volumes": nil,
		"work_bind and caches and memory_high and blocklist": func(c *Config) {
			c.WorkBind = "/srv/rs/{slot}/work"
			c.CacheDirs = []string{"/srv/rs/{slot}/cache", "/srv/rs/{slot}/docker"}
			c.CacheMax = "10G"
			c.Runner.MemoryHigh, c.Dind.MemoryHigh = "2560M", "1G"
			c.ModuleBlocklist = []string{"ifb", "dummy"}
		},
		"no dind": func(c *Config) {
			c.Dind = nil
			c.Runner.Env = map[string]string{"A": "b"}
			c.WorkBind = "/srv/rs/{slot}/work"
		},
	}
	for name, mutate := range variants {
		t.Run(name, func(t *testing.T) {
			r := newPolicyRig(t, mutate)
			ctx := context.Background()
			if err := r.g.EnsureImages(ctx); err != nil {
				t.Fatal(err)
			}
			if err := r.g.ApplyModuleBlocklist(ctx); err != nil {
				t.Fatalf("blocklist: %v\n%s", err, r.denies(t))
			}
			r.g.WipeStale(ctx)
			r.start(t, 1)
			r.start(t, 2)
			if _, err := r.g.ListWithStats(ctx); err != nil {
				t.Fatalf("list: %v\n%s", err, r.denies(t))
			}
			if err := r.g.Stop(ctx, 1); err != nil {
				t.Fatalf("stop: %v\n%s", err, r.denies(t))
			}
			r.g.Reap(ctx)
			if d := r.denies(t); d != "" {
				t.Fatalf("the policy refused calls the guard makes:\n%s", d)
			}
			if !r.d.saw("POST /" + apiVersion + "/containers/create") {
				t.Fatal("nothing reached the daemon")
			}
		})
	}
}

func TestPolicyRefusesWhatTheGuardNeverSends(t *testing.T) {
	r := newPolicyRig(t, func(c *Config) { c.Dind.Privileged = false })
	r.start(t, 1)
	good := r.g.dindSpec(2)

	clone := func(f func(*ContainerSpec)) ContainerSpec {
		b, _ := json.Marshal(good)
		var s ContainerSpec
		json.Unmarshal(b, &s)
		f(&s)
		return s
	}
	// bodies that start from a valid spec and change one thing
	tampered := map[string]ContainerSpec{
		"privileged":     clone(func(s *ContainerSpec) { s.HostConfig.Privileged = true }),
		"host root bind": clone(func(s *ContainerSpec) { s.HostConfig.Binds = append(s.HostConfig.Binds, "/:/host") }),
		"docker socket":  clone(func(s *ContainerSpec) { s.HostConfig.Binds = append(s.HostConfig.Binds, "/var/run/docker.sock:/s") }),
		"host network":   clone(func(s *ContainerSpec) { s.HostConfig.NetworkMode = "host" }),
		"added cap":      clone(func(s *ContainerSpec) { s.HostConfig.CapAdd = []string{"SYS_ADMIN"} }),
		"other image":    clone(func(s *ContainerSpec) { s.Image = "evil/image" }),
		"other command":  clone(func(s *ContainerSpec) { s.Cmd = []string{"sh"} }),
		"no memory cap":  clone(func(s *ContainerSpec) { s.HostConfig.Memory = 0 }),
		"other labels":   clone(func(s *ContainerSpec) { s.Labels["x"] = "y" }),
	}
	for what, spec := range tampered {
		if code, body := r.raw(t, "POST", "/"+apiVersion+"/containers/create?name=ci-dind-2", spec); code != 403 {
			t.Errorf("%s: got %d %s, want 403", what, code, body)
		}
	}
	// an extra field Docker would honour but the guard never sends
	extra := map[string]any{}
	b, _ := json.Marshal(good)
	json.Unmarshal(b, &extra)
	extra["PidMode"] = "host"
	if code, _ := r.raw(t, "POST", "/"+apiVersion+"/containers/create?name=ci-dind-2", extra); code != 403 {
		t.Errorf("unknown top-level field: got %d", code)
	}
	extra = map[string]any{}
	json.Unmarshal(b, &extra)
	extra["HostConfig"].(map[string]any)["PidMode"] = "host"
	if code, _ := r.raw(t, "POST", "/"+apiVersion+"/containers/create?name=ci-dind-2", extra); code != 403 {
		t.Errorf("unknown HostConfig field: got %d", code)
	}
	// the untampered spec is accepted
	if code, body := r.raw(t, "POST", "/"+apiVersion+"/containers/create?name=ci-dind-2", good); code != 201 {
		t.Errorf("the guard's own spec was refused: %d %s", code, body)
	}

	v := "/" + apiVersion
	type call struct {
		m, p string
		body any
	}
	for what, c := range map[string]call{
		"wrong name":            {"POST", v + "/containers/create?name=sshd", good},
		"slot out of range":     {"POST", v + "/containers/create?name=ci-dind-9", good},
		"no name":               {"POST", v + "/containers/create", good},
		"extra query":           {"POST", v + "/containers/create?name=ci-dind-2&platform=x", good},
		"daemon info":           {"GET", v + "/info", nil},
		"list all containers":   {"GET", v + "/containers/json?all=1", nil},
		"list by other label":   {"GET", v + "/containers/json?all=1&filters=%7B%22label%22%3A%5B%22x%22%5D%7D", nil},
		"archive":               {"GET", v + "/containers/ci-dind-1/archive?path=/", nil},
		"attach":                {"POST", v + "/containers/ci-dind-1/attach", nil},
		"restart":               {"POST", v + "/containers/ci-dind-1/restart", nil},
		"other image pull":      {"POST", v + "/images/create?fromImage=evil/x&tag=latest", nil},
		"build":                 {"POST", v + "/build", nil},
		"other network":         {"POST", v + "/networks/create", map[string]any{"Name": "host2", "Driver": "host"}},
		"network wrong subnet":  {"POST", v + "/networks/create", networkCreateBody("ci-slot-2", "10.0.0.0/8", r.g.labels(2, "network", ""))},
		"other volume":          {"POST", v + "/volumes/create", map[string]any{"Name": "pwn", "Driverlabels": 1}},
		"remove other volume":   {"DELETE", v + "/volumes/some-db?force=1", nil},
		"exec arbitrary":        {"POST", v + "/containers/ci-dind-1/exec", map[string]any{"Cmd": []string{"sh", "-c", "id"}}},
		"exec privileged":       {"POST", v + "/containers/ci-dind-1/exec", map[string]any{"Cmd": []string{"docker", "builder", "prune", "-f"}, "Privileged": true}},
		"exec in runner":        {"POST", v + "/containers/ci-runner-1/exec", map[string]any{"Cmd": []string{"docker", "builder", "prune", "-f"}}},
		"exec start unknown id": {"POST", v + "/exec/" + strings.Repeat("a", 64) + "/start", map[string]any{"Detach": true}},
		"other API version":     {"GET", "/v1.30/containers/json", nil},
		"unversioned":           {"GET", "/containers/json", nil},
		"path traversal":        {"GET", v + "/containers/ci-dind-1/../../info", nil},
		"double slash":          {"GET", v + "//info", nil},
		"encoded slash":         {"GET", v + "/containers/ci%2Fdind-1/json", nil},
	} {
		if code, body := r.raw(t, c.m, c.p, c.body); code != 403 && code != 400 {
			t.Errorf("%s: %s %s got %d %s, want a refusal", what, c.m, c.p, code, body)
		}
	}
}

// A container the guard did not create, and one from another pool, must not be reachable through the proxy.
func TestPolicyOwnership(t *testing.T) {
	r := newPolicyRig(t, nil)
	r.d.mu.Lock()
	for name, labels := range map[string]map[string]string{
		"ci-runner-3": nil,                           // our name, no label: someone else's
		"ci-dind-3":   {labelInstance: "other-pool"}, // our name, another pool's label
	} {
		c := &daemonCtr{id: strings.Repeat(name[len(name)-1:], 64), name: name, labels: labels, status: "running"}
		r.d.ctrs[name], r.d.byID[c.id] = c, c
	}
	r.d.mu.Unlock()
	v := "/" + apiVersion
	for _, c := range []struct{ m, p string }{
		{"DELETE", v + "/containers/ci-runner-3?force=1&v=1"},
		{"DELETE", v + "/containers/ci-dind-3?force=1&v=1"},
		{"POST", v + "/containers/ci-dind-3/start"},
		{"GET", v + "/containers/ci-runner-3/json"},
	} {
		if code, body := r.raw(t, c.m, c.p, nil); code != 403 && !(c.m == "GET" && strings.Contains(c.p, "dind")) {
			t.Errorf("%s %s: got %d %s, want 403", c.m, c.p, code, body)
		}
	}
	// reads of another pool's labelled container are allowed (the shared memory budget needs them); writes are not
	if code, _ := r.raw(t, "GET", v+"/containers/ci-dind-3/json", nil); code != 200 {
		t.Errorf("read of another pool's container: got %d", code)
	}
	if r.d.saw("DELETE /v1.44/containers/ci-runner-3") || r.d.saw("DELETE /v1.44/containers/ci-dind-3") {
		t.Error("a foreign container was removed")
	}
}

func TestPolicyRunnerCreate(t *testing.T) {
	r := newPolicyRig(t, nil)
	v := "/" + apiVersion
	name := "ci-1-abcd"
	good := jitFor(name, "https://github.com/example-org")
	post := func(s ContainerSpec) int {
		code, _ := r.raw(t, "POST", v+"/containers/create?name=ci-runner-1", s)
		return code
	}
	if code := post(r.g.runnerSpec(1, name, good)); code != 201 {
		t.Fatalf("valid runner spec refused: %d\n%s", code, r.denies(t))
	}
	r.raw(t, "DELETE", v+"/containers/ci-runner-1?force=1&v=1", nil)

	// the JIT config of another tenant
	if post(r.g.runnerSpec(1, name, jitFor(name, "https://github.com/other"))) != 403 {
		t.Error("JIT config for another tenant was accepted")
	}
	// a runner name for a different slot
	if post(r.g.runnerSpec(1, "ci-2-abcd", jitFor("ci-2-abcd", "https://github.com/example-org"))) != 403 {
		t.Error("runner name of another slot was accepted")
	}
	// runner spec with an extra bind
	s := r.g.runnerSpec(1, name, good)
	s.HostConfig.Binds = append(s.HostConfig.Binds, "/etc:/etc")
	if post(s) != 403 {
		t.Error("extra bind accepted")
	}
	// a second JIT variable smuggled in
	s = r.g.runnerSpec(1, name, good)
	s.Env = append(s.Env, jitEnv+"=other")
	if post(s) != 403 {
		t.Error("duplicate JIT variable accepted")
	}
}

func TestPolicyHelperContainers(t *testing.T) {
	r := newPolicyRig(t, func(c *Config) {
		c.CacheDirs = []string{"/srv/rs/{slot}/cache"}
		c.Runner.MemoryHigh = "2G"
	})
	r.start(t, 1)
	v := "/" + apiVersion
	post := func(slot int, cmd, binds []string) int {
		code, _ := r.raw(t, "POST", v+"/containers/create?name=ci-wipe-"+string(rune('0'+slot)), r.g.wipeSpec(slot, cmd, binds))
		return code
	}
	cb, cc := r.g.cacheWipeCmd(1)
	if post(1, cc, cb) != 201 {
		t.Fatalf("the guard's own cache wipe was refused\n%s", r.denies(t))
	}
	r.raw(t, "DELETE", v+"/containers/ci-wipe-1?force=1&v=1", nil)

	for what, c := range map[string]struct {
		slot  int
		cmd   []string
		binds []string
	}{
		"mount the host root":        {1, []string{"sh", "-c", "id"}, []string{"/:/host"}},
		"cache command, other bind":  {1, cc, []string{"/etc:/wipe/0"}},
		"cache bind, other command":  {1, []string{"sh", "-c", "cat /wipe/0/x"}, cb},
		"cgroup tree, other command": {1, []string{"sh", "-c", "echo pwn > /cgroup/x", "sh"}, []string{cgroupBind}},
		"modprobe without config":    {0, r.g.modprobeCmd(), []string{modprobeBind}},
		"cgroup, unowned target":     {1, []string{"sh", "-c", memoryHighScript, "sh", strings.Repeat("c", 64) + "=1"}, []string{cgroupBind}},
		"slot 7":                     {7, cc, cb},
	} {
		if post(c.slot, c.cmd, c.binds) != 403 {
			t.Errorf("%s: helper accepted", what)
		}
	}
}

func TestPolicyPing(t *testing.T) {
	r := newPolicyRig(t, nil)
	if err := PingDocker(context.Background(), r.sock); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyBodyLimit(t *testing.T) {
	r := newPolicyRig(t, nil)
	big := strings.Repeat("a", maxPolicyBody+1)
	req := httptest.NewRequest("POST", "/"+apiVersion+"/containers/create?name=ci-dind-1", strings.NewReader(big))
	rec := httptest.NewRecorder()
	r.px.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("got %d", rec.Code)
	}
}
