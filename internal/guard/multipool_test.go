package guard

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MacLeodTech/runner-scaling/internal/api"
)

const multiConfig = `
github_url: https://github.com/example-org
tenant_url: https://pipelines.actions.githubusercontent.com/OURS/
prefix: ci
max_slots: 2
start_timeout: 5s
runner:
  image: example/runner@sha256:aaaa
  cap_drop: [ALL]
  security_opt: [no-new-privileges:true]
  memory: 3G
pools:
  lite:
    subnet: 172.41.1{slot}.0/24
  box:
    max_slots: 3
    subnet: 172.41.2{slot}.0/24
    labels: [homelab-box, box]
    dind:
      image: docker@sha256:bbbb
      runtime: sysbox-runc
      memory: 2G
      healthcheck: {test: [CMD, docker, info], interval: 1s, retries: 5}
`

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "guard.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigsMultiPool(t *testing.T) {
	cs, err := LoadConfigs(writeCfg(t, multiConfig))
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("got %d pools", len(cs))
	}
	box, lite := cs[0], cs[1] // sorted by name
	if box.Name != "box" || lite.Name != "lite" {
		t.Fatalf("names %q %q", box.Name, lite.Name)
	}
	if box.Prefix != "ci-box" || lite.Prefix != "ci-lite" {
		t.Errorf("prefixes %q %q", box.Prefix, lite.Prefix)
	}
	if box.MaxSlots != 3 || lite.MaxSlots != 2 {
		t.Errorf("max_slots %d %d: the pool's own value wins, else the shared one", box.MaxSlots, lite.MaxSlots)
	}
	if strings.Join(box.Labels, ",") != "homelab-box,box" || lite.Labels[len(lite.Labels)-1] != "lite" || lite.Labels[0] != "self-hosted" {
		t.Errorf("labels %v %v", box.Labels, lite.Labels)
	}
	if box.Dind == nil || lite.Dind != nil {
		t.Error("dind belongs to box only")
	}
	if box.Runner.Image != lite.Runner.Image || box.Runner.Image == "" {
		t.Error("the shared runner template reaches both pools")
	}
}

func TestLoadConfigsSinglePoolUnchanged(t *testing.T) {
	cs, err := LoadConfigs(writeCfg(t, testConfig))
	if err != nil || len(cs) != 1 {
		t.Fatalf("%v %d", err, len(cs))
	}
	if cs[0].Name != "" || cs[0].Prefix != "ci" || cs[0].Key() != "ci" {
		t.Errorf("name %q prefix %q key %q", cs[0].Name, cs[0].Prefix, cs[0].Key())
	}
	if _, err := LoadConfig(writeCfg(t, multiConfig)); err == nil {
		t.Error("LoadConfig must refuse a two-pool file rather than pick one")
	}
}

func TestLoadConfigsRefusesCollisions(t *testing.T) {
	for name, tc := range map[string]struct{ edit, want string }{
		"same prefix":    {"  box:\n    max_slots: 3", "  box:\n    prefix: ci-lite\n    max_slots: 3"},
		"same label":     {"labels: [homelab-box, box]", "labels: [LITE]"},
		"same subnet":    {"172.41.2{slot}.0/24", "172.41.1{slot}.0/24"},
		"bad pool name":  {"  box:", "  Box_1:"},
		"no pools":       {"pools:\n", "pools: {}\nxx:\n"},
		"unknown key":    {"    max_slots: 3", "    max_slotz: 3"},
		"shared hostdir": {"    subnet: 172.41.2{slot}.0/24", "    subnet: 172.41.2{slot}.0/24\n    work_bind: /srv/w/{slot}\n  lite2:\n    labels: [l2]\n    subnet: 172.41.3{slot}.0/24\n    work_bind: /srv/w/{slot}"},
	} {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(multiConfig, tc.edit, tc.want, 1)
			if body == multiConfig {
				t.Fatal("edit did not apply")
			}
			if name == "shared hostdir" { // lite also needs the same bind
				body = strings.Replace(body, "  lite:\n    subnet: 172.41.1{slot}.0/24", "  lite:\n    subnet: 172.41.1{slot}.0/24\n    work_bind: /srv/w/{slot}", 1)
			}
			if _, err := LoadConfigs(writeCfg(t, body)); err == nil {
				t.Fatalf("expected an error for %s", name)
			}
		})
	}
}

func TestPoolsHandlerRoutes(t *testing.T) {
	cs, err := LoadConfigs(writeCfg(t, multiConfig))
	if err != nil {
		t.Fatal(err)
	}
	p := NewPools(cs, newFake(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := p.Handler()

	rec := do(t, h, "GET", "/v1/pools", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"name":"box"`) || !strings.Contains(rec.Body.String(), `"homelab-box"`) {
		t.Fatalf("pools: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/v1/pools/lite/info", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"prefix":"ci-lite"`) {
		t.Errorf("lite info: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/v1/pools/box/slots", nil); rec.Code != 200 {
		t.Errorf("box slots: %d %s", rec.Code, rec.Body)
	}
	for _, path := range []string{"/v1/pools/nope/info", "/v1/info", "/v1/slots", "/v1/pools/lite/../box/info"} {
		if rec := do(t, h, "GET", path, nil); rec.Code != 404 && rec.Code != 307 {
			t.Errorf("%s: got %d, want a refusal with several pools", path, rec.Code)
		}
	}
	// A guard with one pool still answers where old scalers look.
	one, _ := LoadConfigs(writeCfg(t, testConfig))
	h1 := NewPools(one, newFake(), slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()
	if rec := do(t, h1, "GET", "/v1/info", nil); rec.Code != 200 {
		t.Errorf("single pool /v1/info: %d", rec.Code)
	}
	if rec := do(t, h1, "GET", "/v1/pools", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"name":"ci"`) {
		t.Errorf("single pool /v1/pools: %d %s", rec.Code, rec.Body)
	}
}

// Two pools through one policy proxy: each pool's guard works, and neither can use the other's names or its template.
func TestPolicyTwoPools(t *testing.T) {
	cs, err := LoadConfigs(writeCfg(t, multiConfig))
	if err != nil {
		t.Fatal(err)
	}
	box, lite := cs[0], cs[1]

	dir, _ := os.MkdirTemp("", "rs")
	t.Cleanup(func() { os.RemoveAll(dir) })
	daemon := newFakeDaemon()
	dsock := filepath.Join(dir, "d.sock")
	dln, _ := net.Listen("unix", dsock)
	dsrv := &http.Server{Handler: daemon}
	go dsrv.Serve(dln)
	t.Cleanup(func() { dsrv.Close() })

	px := NewProxy(cs, dsock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	psock := filepath.Join(dir, "p.sock")
	pln, _ := net.Listen("unix", psock)
	psrv := &http.Server{Handler: px}
	go psrv.Serve(pln)
	t.Cleanup(func() { psrv.Close() })

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	gBox := New(box, NewSocketDocker(psock), quiet)
	gLite := New(lite, NewSocketDocker(psock), quiet)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, g := range []*Guard{gBox, gLite} {
		name := g.cfg.Prefix + "-1-abcd"
		if err := g.Start(ctx, 1, api.StartRequest{RunnerName: name, JITConfig: jitFor(name, "https://github.com/example-org")}); err != nil {
			t.Fatalf("pool %s through the shared policy: %v", g.cfg.Name, err)
		}
	}

	// lite's guard, hijacked, asks for a container with box's name and box's Sysbox sidecar spec: refused.
	raw := policyRig{client: NewSocketDocker(psock).c}
	st, _ := raw.raw(t, "POST", "/v1.43/containers/create?name=ci-box-dind-2", map[string]any{
		"Image": box.Dind.Image, "HostConfig": map[string]any{"Runtime": "sysbox-runc", "Privileged": true},
	})
	if st != 403 {
		t.Errorf("forged dind create: %d, want 403", st)
	}
	// A name that belongs to neither pool.
	if st, _ := raw.raw(t, "POST", "/v1.43/containers/create?name=ci-other-runner-1", map[string]any{"Image": lite.Runner.Image}); st != 403 {
		t.Errorf("foreign name: %d, want 403", st)
	}
	// Removing the other pool's container by name is allowed only to a policy that owns it, and the daemon is asked once.
	for _, g := range []*Guard{gBox, gLite} {
		if err := g.Stop(ctx, 1); err != nil {
			t.Errorf("stop %s: %v", g.cfg.Prefix, err)
		}
	}
}
