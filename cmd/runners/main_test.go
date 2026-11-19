package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MacLeodTech/runner-scaling/internal/api"
	"github.com/MacLeodTech/runner-scaling/internal/dashboard"
)

func fake(t *testing.T, token string) *httptest.Server {
	t.Helper()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	st := dashboard.State{Name: "homelab", Version: "v9", Now: now, MaxSlots: 3,
		Slots: []api.Slot{{Slot: 1, RunnerName: "r1", RunnerState: "running", Stats: &api.SlotStats{MemBytes: 3 << 30, CPUPercent: 120, PIDs: 40}}, {Slot: 2}},
		Peers: []dashboard.State{{Name: "lite", MaxSlots: 8}, {Name: "sysbox", PeerErr: "timeout"}}}
	st.Running = []dashboard.Job{{Repo: "o/r", Name: "build", Ref: "main", State: "running", Started: now.Add(-90 * time.Second)}}
	st.Recent = []dashboard.Job{{Repo: "o/r", Name: "old1", State: "success", Finished: now.Add(-3 * time.Hour)}, {Repo: "o/r", Name: "old2", State: "failure", Finished: now.Add(-4 * time.Hour)}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		if r.URL.RawQuery != "" {
			t.Errorf("token or query leaked into the URL: %q", r.URL.RawQuery)
		}
		switch r.URL.Path {
		case "/api/state":
			json.NewEncoder(w).Encode(st)
		case "/metrics":
			w.Write([]byte("# HELP x\nrunner_scaling_memory_budget_bytes 8589934592\nrunner_scaling_memory_used_bytes 4294967296\nrunner_scaling_slots{state=\"a\"} 1\nrunner_scaling_slots{state=\"b\"} 2\n"))
		default:
			http.NotFound(w, r)
		}
	}))
}

func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var b bytes.Buffer
	err := run(args, &b)
	return b.String(), err
}

func TestCommands(t *testing.T) {
	srv := fake(t, "s3cret")
	defer srv.Close()
	tf := filepath.Join(t.TempDir(), "tok")
	os.WriteFile(tf, []byte("s3cret\n"), 0o600)
	base := []string{"--url", srv.URL, "--token-file", tf}

	cases := []struct {
		args []string
		want []string
	}{
		{nil, []string{"homelab", "1/3", "lite", "0/8", "unreachable: timeout"}},
		{[]string{"slots"}, []string{"r1", "3.0GiB", "120%", "40"}},
		{[]string{"jobs", "-n", "1"}, []string{"build", "1m", "old1", "3h"}},
		{[]string{"memory"}, []string{"budget 8.0GiB, used 4.0GiB (50%)"}},
		{[]string{"status", "--json"}, []string{`"name": "homelab"`}},
	}
	for _, c := range cases {
		out, err := runCLI(t, append(append([]string{}, base...), c.args...)...)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("%v: output lacks %q:\n%s", c.args, w, out)
			}
		}
	}
	if out, _ := runCLI(t, append(base, "jobs", "-n", "1")...); strings.Contains(out, "old2") {
		t.Errorf("-n 1 should hide old2:\n%s", out)
	}
	// flags after the command work too
	if _, err := runCLI(t, "jobs", "--url", srv.URL, "--token-file", tf); err != nil {
		t.Errorf("flags after command: %v", err)
	}
}

func TestErrors(t *testing.T) {
	srv := fake(t, "s3cret")
	defer srv.Close()
	t.Setenv("RUNNERS_TOKEN", "")
	if _, err := runCLI(t, "--url", srv.URL); err == nil || !strings.Contains(err.Error(), "refused the token") {
		t.Errorf("missing token: %v", err)
	}
	t.Setenv("RUNNERS_TOKEN", "s3cret")
	if _, err := runCLI(t, "--url", srv.URL); err != nil {
		t.Errorf("token from env: %v", err)
	}
	if _, err := runCLI(t, "--url", "http://127.0.0.1:1"); err == nil || !strings.Contains(err.Error(), "cannot reach") {
		t.Errorf("unreachable: %v", err)
	}
	if _, err := runCLI(t, "--url", "nonsense"); err == nil {
		t.Error("bad url accepted")
	}
	if _, err := runCLI(t, "--url", srv.URL, "bogus"); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("unknown command: %v", err)
	}
	if _, err := runCLI(t, "a", "b"); err == nil {
		t.Error("extra argument accepted")
	}
	if out, err := runCLI(t, "version"); err != nil || !strings.HasPrefix(out, "runners ") {
		t.Errorf("version: %q %v", out, err)
	}
}

func TestHuman(t *testing.T) {
	for in, want := range map[int64]string{0: "0B", 1023: "1023B", 1024: "1.0KiB", 3 << 30: "3.0GiB"} {
		if got := human(in); got != want {
			t.Errorf("human(%d) = %s, want %s", in, got, want)
		}
	}
}

func TestDefaultTokenFile(t *testing.T) {
	srv := fake(t, "s3cret")
	defer srv.Close()
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("RUNNERS_TOKEN", "")
	t.Setenv("RUNNERS_TOKEN_FILE", "")
	if _, err := runCLI(t, "--url", srv.URL); err == nil {
		t.Fatal("no token anywhere should be refused")
	}
	os.MkdirAll(filepath.Join(cfg, "runners"), 0o700)
	os.WriteFile(filepath.Join(cfg, "runners", "token"), []byte("s3cret\n"), 0o600)
	if _, err := runCLI(t, "--url", srv.URL); err != nil {
		t.Fatalf("default token file not used: %v", err)
	}
}

func TestTokenFileWarning(t *testing.T) {
	f := filepath.Join(t.TempDir(), "tok")
	os.WriteFile(f, []byte("x"), 0o644)
	os.Chmod(f, 0o644)
	if w := tokenFileWarning(f); !strings.Contains(w, "mode 600") {
		t.Errorf("0644 not flagged: %q", w)
	}
	os.Chmod(f, 0o600)
	if w := tokenFileWarning(f); w != "" {
		t.Errorf("0600 flagged: %q", w)
	}
	if w := tokenFileWarning(f + "-missing"); w != "" {
		t.Errorf("missing file flagged: %q", w)
	}
}

func TestTokenFileWarningPrinted(t *testing.T) {
	srv := fake(t, "s3cret")
	defer srv.Close()
	var warn bytes.Buffer
	old := warnOut
	warnOut = &warn
	defer func() { warnOut = old }()
	f := filepath.Join(t.TempDir(), "tok")
	os.WriteFile(f, []byte("s3cret\n"), 0o600)
	os.Chmod(f, 0o640)
	if _, err := runCLI(t, "--url", srv.URL, "--token-file", f); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warn.String(), "warning") || strings.Contains(warn.String(), "s3cret") {
		t.Errorf("warning output: %q", warn.String())
	}
}

func TestCompletion(t *testing.T) {
	for _, sh := range []string{"bash", "zsh"} {
		out, err := runCLI(t, "completion", sh)
		if err != nil || !strings.Contains(out, "runners") || !strings.Contains(out, "--gate-file") {
			t.Errorf("%s: %q %v", sh, out, err)
		}
	}
	if _, err := runCLI(t, "completion", "fish"); err == nil {
		t.Error("unknown shell accepted")
	}
	if _, err := runCLI(t, "completion"); err == nil {
		t.Error("missing shell accepted")
	}
}

func TestHostChecks(t *testing.T) {
	_, fence, _ := net.ParseCIDR("172.41.0.0/16")
	if outsideFence(fence, []string{"172.41.3.2"}) {
		t.Error("inside address flagged")
	}
	if !outsideFence(fence, []string{"172.41.3.2", "172.17.0.5"}) {
		t.Error("outside address not flagged")
	}
	if outsideFence(nil, []string{"172.17.0.5"}) {
		t.Error("no fence given must not flag")
	}
	gate := filepath.Join(t.TempDir(), "ok")
	if gateStatus("") != "" || gateStatus(gate) != "MISSING" {
		t.Error("gate status")
	}
	// a file we are not allowed to look at is "unknown", not "MISSING"
	locked := filepath.Join(t.TempDir(), "locked")
	os.Mkdir(locked, 0o000)
	defer os.Chmod(locked, 0o700)
	if os.Getuid() != 0 {
		if got := gateStatus(filepath.Join(locked, "ok")); !strings.HasPrefix(got, "unknown") {
			t.Errorf("unreadable gate path reported as %q", got)
		}
	}
	os.WriteFile(gate, nil, 0o600)
	if gateStatus(gate) != "present" {
		t.Error("gate present not seen")
	}
	var b bytes.Buffer
	renderHost(&b, hostReport{Gate: "MISSING", Containers: []hostRow{{Name: "a", Runtime: "runc", Unfenced: true}}}, true)
	if !strings.Contains(b.String(), "MISSING") || !strings.Contains(b.String(), "UNFENCED") {
		t.Errorf("render: %s", b.String())
	}
}
