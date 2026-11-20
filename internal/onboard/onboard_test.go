package onboard

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MacLeodTech/runner-scaling/internal/guard"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

func pfx(s ...string) (out []netip.Prefix) {
	for _, v := range s {
		out = append(out, netip.MustParsePrefix(v))
	}
	return
}

func TestChooseFenceAvoidsUsedRanges(t *testing.T) {
	if got, _ := ChooseFence(nil); got != "172.30.0.0/16" {
		t.Fatalf("free host: %s", got)
	}
	if got, _ := ChooseFence(pfx("172.30.12.0/24")); got != "172.31.0.0/16" {
		t.Fatalf("172.30 in use: %s", got)
	}
	if got, _ := ChooseFence(pfx("172.16.0.0/12")); got != "" {
		t.Fatalf("whole block in use should fail, got %s", got)
	}
	if _, err := ChooseFence(pfx("172.16.0.0/12")); err == nil {
		t.Fatal("no error when nothing is free")
	}
}

func TestSize(t *testing.T) {
	for gb, want := range map[int][2]string{2: {"1", "3G"}, 8: {"2", "4G"}, 16: {"4", "8G"}, 128: {"8", "64G"}} {
		s, b := Size(gb)
		if strconv.Itoa(s) != want[0] || b != want[1] {
			t.Errorf("%d GB -> %d slots %s, want %v", gb, s, b, want)
		}
	}
}

// The generated files must be accepted by the guard's own validation, fenced consistently, and carry the answers.
func TestPlanProducesValidFiles(t *testing.T) {
	p, err := NewPlan(PlanInput{Org: "acme", ClientID: "Iv1.abc", InstallationID: 42, Key: testKey(t), DockerGID: 988,
		MemTotalGB: 16, Taken: pfx("172.30.0.0/24"), Version: "v0.12.3"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	notes, err := p.Write(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "chown 65532") {
		t.Errorf("a non-root init must say how to hand over the key: %v", notes)
	}
	t.Setenv("USERDIR", "/srv")
	cfgs, err := guard.LoadConfigs(filepath.Join(dir, "guard.yaml"))
	if err != nil || len(cfgs) != 1 {
		t.Fatalf("generated guard config rejected (%d pools): %v", len(cfgs), err)
	}
	cfg := cfgs[0]
	if cfg.Name != "lite" || cfg.Dind != nil {
		t.Errorf("default is the lite pool alone: %q dind %v", cfg.Name, cfg.Dind != nil)
	}
	if cfg.GitHubURL != "https://github.com/acme" || cfg.MaxSlots != 4 {
		t.Errorf("config %+v", cfg)
	}
	if !strings.HasPrefix(cfg.Subnet, "172.31.") {
		t.Errorf("subnet %q should follow the chosen free range 172.31", cfg.Subnet)
	}
	compose, _ := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if !strings.Contains(string(compose), "FENCE_SRC: 172.31.0.0/16") || !strings.Contains(string(compose), "runner-scaling:0.12") ||
		strings.Contains(string(compose), "172.30.") {
		t.Errorf("compose.yaml not rewritten consistently")
	}
	env, _ := ReadEnv(filepath.Join(dir, ".env"))
	if env["GITHUB_APP_CLIENT_ID"] != "Iv1.abc" || env["GITHUB_APP_INSTALLATION_ID"] != "42" || env["DOCKER_GID"] != "988" {
		t.Errorf("env %v", env)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "secrets/github_app.pem")); fi.Mode().Perm() != 0o400 {
		t.Errorf("key mode %v", fi.Mode())
	}
	if fi, _ := os.Stat(filepath.Join(dir, ".env")); fi.Mode().Perm() != 0o600 {
		t.Errorf(".env mode %v", fi.Mode())
	}
	if _, err := p.Write(dir, false); err == nil {
		t.Error("second init overwrote existing files without --force")
	}
	if _, err := p.Write(dir, true); err != nil {
		t.Errorf("--force: %v", err)
	}
}

// Both pools: one guard.yaml, the Sysbox pool sized smaller, subnets inside the fence, and doctor wants Sysbox for it.
func TestPlanTwoPools(t *testing.T) {
	p, err := NewPlan(PlanInput{Org: "acme", ClientID: "Iv1.abc", InstallationID: 42, Key: testKey(t), DockerGID: 988,
		MemTotalGB: 16, Taken: pfx("172.30.0.0/24"), Pools: []string{"lite", "sysbox"}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := p.Write(dir, false); err != nil {
		t.Fatal(err)
	}
	t.Setenv("USERDIR", "/srv")
	cfgs, err := guard.LoadConfigs(filepath.Join(dir, "guard.yaml"))
	if err != nil || len(cfgs) != 2 {
		t.Fatalf("%d pools: %v", len(cfgs), err)
	}
	lite, sbx := cfgs[0], cfgs[1] // sorted by name
	if sbx.Name != "sysbox" || sbx.Dind == nil || sbx.Dind.Runtime != "sysbox-runc" || lite.Dind != nil {
		t.Errorf("pools: %q %q", sbx.Name, lite.Name)
	}
	if sbx.MaxSlots != 2 || lite.MaxSlots != 4 {
		t.Errorf("slots lite %d sysbox %d, want 4 and 2", lite.MaxSlots, sbx.MaxSlots)
	}
	if !(Doctor{Dir: dir}).stackWantsSysbox() {
		t.Error("doctor should require Sysbox for a stack with a sysbox pool")
	}
	// A pool the examples do not have, and a bad name, are refused.
	for _, bad := range []string{"privileged", "x y"} {
		if _, err := NewPlan(PlanInput{Org: "acme", ClientID: "c", InstallationID: 1, Key: []byte("k"), DockerGID: 1, MemTotalGB: 8, Pools: []string{bad}}); err == nil {
			t.Errorf("pool %q accepted", bad)
		}
	}
}

func TestPlanRejectsBadInput(t *testing.T) {
	ok := PlanInput{Org: "acme", ClientID: "Iv1", InstallationID: 1, Key: []byte("k"), DockerGID: 1, MemTotalGB: 8}
	for name, mut := range map[string]func(*PlanInput){
		"org":    func(i *PlanInput) { i.Org = "a/b" },
		"empty":  func(i *PlanInput) { i.Org = "" },
		"client": func(i *PlanInput) { i.ClientID = "" },
		"inst":   func(i *PlanInput) { i.InstallationID = 0 },
		"gid":    func(i *PlanInput) { i.DockerGID = 0 },
	} {
		in := ok
		mut(&in)
		if _, err := NewPlan(in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestApiLess(t *testing.T) {
	for _, c := range []struct {
		have string
		less bool
	}{{"1.43", true}, {"1.44", false}, {"1.52", false}, {"1.9", true}, {"2.0", false}} {
		if apiLess(c.have, "1.44") != c.less {
			t.Errorf("%s", c.have)
		}
	}
}

// A fake GitHub: converts the manifest code, lists installations after "install".
func TestAppFlow(t *testing.T) {
	key := testKey(t)
	var installed atomic.Bool
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/app-manifests/") && strings.HasSuffix(r.URL.Path, "/conversions"):
			if strings.Contains(r.URL.Path, "bad") {
				http.Error(w, "no", 404)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": 7, "slug": "runner-scaler", "client_id": "Iv1.new", "pem": string(key)})
		case r.URL.Path == "/app/installations":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				http.Error(w, "no jwt", 401)
				return
			}
			if !installed.Load() {
				w.Write([]byte("[]"))
				return
			}
			w.Write([]byte(`[{"id":99,"account":{"login":"Other"}},{"id":123,"account":{"login":"Acme"}}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()

	f := &AppFlow{Org: "acme", Name: "runner-scaler", API: gh.URL, Web: "https://github.example", Timeout: 20 * time.Second}
	var local string
	ready := make(chan struct{})
	f.Announce = func(u string) { local = u; close(ready) }
	type out struct {
		r   *AppResult
		err error
	}
	done := make(chan out, 1)
	go func() { r, err := f.Run(context.Background()); done <- out{r, err} }()
	<-ready

	// The landing page carries the manifest and the state GitHub must echo back.
	resp, err := http.Get(local)
	if err != nil {
		t.Fatal(err)
	}
	buf := new(strings.Builder)
	b := make([]byte, 1<<16)
	n, _ := resp.Body.Read(b)
	buf.Write(b[:n])
	resp.Body.Close()
	page := buf.String()
	if !strings.Contains(page, "organization_self_hosted_runners") || strings.Contains(page, "pull_requests") {
		t.Fatalf("manifest page: %s", page)
	}
	i := strings.Index(page, "state=")
	state := page[i+6 : i+6+32]

	// Wrong state is refused and does not end the flow.
	if r, _ := http.Get(local + "callback?code=abc&state=nope"); r.StatusCode != 400 {
		t.Fatalf("wrong state: %d", r.StatusCode)
	}
	if r, _ := http.Get(local + "callback?code=abc&state=" + url.QueryEscape(state)); r.StatusCode != 200 {
		t.Fatalf("callback: %d", r.StatusCode)
	}
	installed.Store(true)
	select {
	case o := <-done:
		if o.err != nil {
			t.Fatal(o.err)
		}
		if o.r.ClientID != "Iv1.new" || o.r.InstallationID != 123 || o.r.Account != "Acme" || len(o.r.PEM) == 0 {
			t.Fatalf("%+v", o.r)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("flow did not finish")
	}
}

func TestAppFlowBadCode(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 404) }))
	defer gh.Close()
	f := &AppFlow{Org: "acme", Name: "x", API: gh.URL, Timeout: 5 * time.Second}
	ready := make(chan string, 1)
	f.Announce = func(u string) { ready <- u }
	done := make(chan error, 1)
	go func() { _, err := f.Run(context.Background()); done <- err }()
	local := <-ready
	resp, _ := http.Get(local)
	b := make([]byte, 1<<16)
	n, _ := resp.Body.Read(b)
	resp.Body.Close()
	page := string(b[:n])
	i := strings.Index(page, "state=")
	http.Get(local + "callback?code=x&state=" + page[i+6:i+6+32])
	if err := <-done; err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err = %v", err)
	}
}

func TestFindAndSetTenant(t *testing.T) {
	const u = "https://pipelines.actions.githubusercontent.com/AbC123/"
	log := `guard-1 | time=x level=WARN msg=DENY err="jit: no tenant_url pinned: this config's ServerUrl is \"` + u + `\"; if that is your org's tenant, set tenant_url to it"`
	got, err := FindTenant(log + "\n" + log)
	if err != nil || got != u {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := FindTenant("nothing here"); err == nil {
		t.Error("no refusal in the log accepted")
	}
	other := strings.Replace(log, "AbC123", "Other9", 1)
	if _, err := FindTenant(log + other); err == nil {
		t.Error("two different tenants must not be guessed between")
	}
	if _, err := FindTenant(strings.Replace(log, "actions.githubusercontent.com", "evil.example", 1)); err == nil {
		t.Error("a non-GitHub host was accepted")
	}
	cfg := "github_url: https://github.com/acme\n# tenant_url: https://pipelines….actions.githubusercontent.com/<tenant>/   # pin it\nprefix: x\n"
	out, err := SetTenant(cfg, u)
	if err != nil || !strings.Contains(out, "\ntenant_url: "+u+"\n") || strings.Contains(out, "<tenant>") {
		t.Fatalf("%q %v", out, err)
	}
	out, _ = SetTenant("github_url: a\nprefix: x\n", u)
	if !strings.Contains(out, "tenant_url: "+u) {
		t.Errorf("no line to replace: %q", out)
	}
	if _, err := SetTenant(cfg, "http://pipelines.actions.githubusercontent.com/x/"); err == nil {
		t.Error("http accepted")
	}
}

// fakeDocker answers the three calls doctor makes.
func fakeDocker(t *testing.T, api string, runtimes []string, subnets ...string) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"Version": "29.0.0", "ApiVersion": api})
	})
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		rt := map[string]any{"runc": map[string]any{}}
		for _, r := range runtimes {
			rt[r] = map[string]any{}
		}
		json.NewEncoder(w).Encode(map[string]any{"SecurityOptions": []string{"name=seccomp"}, "DockerRootDir": t.TempDir(), "Runtimes": rt})
	})
	mux.HandleFunc("/networks", func(w http.ResponseWriter, _ *http.Request) {
		var nets []map[string]any
		for i, s := range subnets {
			nets = append(nets, map[string]any{"Name": "n" + strconv.Itoa(i), "IPAM": map[string]any{"Config": []map[string]string{{"Subnet": s}}}})
		}
		json.NewEncoder(w).Encode(nets)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func find(cs []Check, name string) *Check {
	for i := range cs {
		if strings.Contains(cs[i].Name, name) {
			return &cs[i]
		}
	}
	return nil
}

func TestDoctorStackFindings(t *testing.T) {
	t.Setenv("USERDIR", "/srv")
	p, err := NewPlan(PlanInput{Org: "acme", ClientID: "Iv1.abc", InstallationID: 42, Key: testKey(t), DockerGID: 988, MemTotalGB: 16, Version: "v0.12.0"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := p.Write(dir, false); err != nil {
		t.Fatal(err)
	}
	// A Docker network already sits on the fenced range.
	d := Doctor{Dir: dir, Offline: true, DockerHost: fakeDocker(t, "1.43", nil, "172.30.12.0/24"), WantSysbox: true}
	cs := d.Run(context.Background())
	for name, want := range map[string]Level{
		"docker version":  Fail, // API too old
		"sysbox runtime":  Fail, // required, absent
		"app key owner":   Fail, // owned by the test user, not 65532
		"subnet":          Fail, // overlaps the existing network
		"tenant_url":      Warn,
		"guard.yaml pool lite": Pass,
	} {
		c := find(cs, name)
		if c == nil || c.Level != want {
			t.Errorf("%s: %+v, want level %v", name, c, want)
		}
	}
	if c := find(cs, "subnet"); c != nil && !strings.Contains(c.Detail, "172.30.12.0/24") {
		t.Errorf("the conflicting range should be named: %q", c.Detail)
	}
	// Good Docker, key owner accepted as the test user's uid, fence matches.
	d = Doctor{Dir: dir, Offline: true, DockerHost: fakeDocker(t, "1.52", []string{"sysbox-runc"}), WantSysbox: true, ExpectOwner: os.Getuid()}
	cs = d.Run(context.Background())
	for _, name := range []string{"docker version", "sysbox runtime", "app key file", "guard.yaml pool lite"} {
		if c := find(cs, name); c == nil || c.Level != Pass {
			t.Errorf("%s: %+v", name, c)
		}
	}
	if c := find(cs, "subnet"); c != nil {
		t.Errorf("unexpected subnet finding: %+v", c)
	}
	// A key readable by everyone is refused.
	os.Chmod(filepath.Join(dir, "secrets/github_app.pem"), 0o644)
	if c := find(d.Run(context.Background()), "app key mode"); c == nil || c.Level != Fail {
		t.Errorf("world-readable key: %+v", c)
	}
	// The example values left in place are refused.
	os.WriteFile(filepath.Join(dir, ".env"), []byte("GITHUB_APP_CLIENT_ID=Iv1.xxxxxxxxxxxxxxxx\nGITHUB_APP_INSTALLATION_ID=12345678\nDOCKER_GID=\n"), 0o600)
	n := 0
	for _, c := range d.Run(context.Background()) {
		if strings.HasPrefix(c.Name, ".env ") && c.Level == Fail {
			n++
		}
	}
	if n != 3 {
		t.Errorf("example .env values flagged %d times, want 3", n)
	}
}

func TestDoctorNoDocker(t *testing.T) {
	cs := Doctor{Offline: true, DockerHost: filepath.Join(t.TempDir(), "none.sock")}.Run(context.Background())
	if c := find(cs, "docker"); c == nil || c.Level != Fail || c.Fix == "" {
		t.Fatalf("%+v", c)
	}
}
