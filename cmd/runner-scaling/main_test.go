package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/MacLeodTech/runner-scaling/internal/dashboard"
)

func TestEnv(t *testing.T) {
	t.Setenv("RS_SET", "v")
	t.Setenv("RS_EMPTY", "")
	if got := env("RS_SET", "d"); got != "v" {
		t.Errorf("set: %q", got)
	}
	if got := env("RS_EMPTY", "d"); got != "d" {
		t.Errorf("an empty value counts as unset: %q", got)
	}
	if got := env("RS_UNSET_XYZ", "d"); got != "d" {
		t.Errorf("unset: %q", got)
	}
}

func TestSecret(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "tok")
	if err := os.WriteFile(f, []byte("  from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RS_TOK", "from-env")
	t.Setenv("RS_TOK_FILE", "")
	if got, err := secret("RS_TOK"); err != nil || got != "from-env" {
		t.Errorf("env only: %q, %v", got, err)
	}
	t.Setenv("RS_TOK_FILE", f)
	if got, err := secret("RS_TOK"); err != nil || got != "from-file" {
		t.Errorf("the file wins and is trimmed: %q, %v", got, err)
	}
	t.Setenv("RS_TOK_FILE", filepath.Join(dir, "missing"))
	if _, err := secret("RS_TOK"); err == nil {
		t.Error("an unreadable secret file must be an error, not a silent empty credential")
	}
}

func TestSplitLabels(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"a,b", []string{"a", "b"}},
		{" a , b ,, ", []string{"a", "b"}},
		{"", []string{"def"}},
		{" , ", []string{"def"}},
	}
	for _, c := range cases {
		if got := splitLabels(c.in, "def"); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %v want %v", c.in, got, c.want)
		}
	}
}

func labels(names ...string) []scaleset.Label {
	var out []scaleset.Label
	for _, n := range names {
		out = append(out, scaleset.Label{Type: "User", Name: n})
	}
	return out
}

func TestSameLabels(t *testing.T) {
	cases := []struct {
		name string
		a, b []scaleset.Label
		want bool
	}{
		{"same", labels("a", "b"), labels("a", "b"), true},
		{"order is irrelevant", labels("a", "b"), labels("b", "a"), true},
		{"case is irrelevant", labels("Self-Hosted"), labels("self-hosted"), true},
		{"extra label", labels("a"), labels("a", "b"), false},
		{"different label", labels("a", "b"), labels("a", "c"), false},
		{"both empty", nil, nil, true},
		// Same length and every name of b is in a, but a has "y" and b does not: the sets differ, so the scale set
		// must be recreated.
		{"duplicate hides a difference", labels("x", "y"), labels("x", "x"), false},
		{"duplicate hides a difference, reversed", labels("x", "x"), labels("x", "y"), false},
	}
	for _, c := range cases {
		if got := sameLabels(c.a, c.b); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestTrustedRefs(t *testing.T) {
	// The unset case needs the variable absent, not empty; t.Setenv then os.Unsetenv restores it afterwards.
	t.Setenv("CACHE_TRUSTED_REFS", "")
	os.Unsetenv("CACHE_TRUSTED_REFS")
	if got := trustedRefs(); !reflect.DeepEqual(got, []string{"refs/heads/main"}) {
		t.Errorf("default: %v", got)
	}
	for in, want := range map[string][]string{
		"refs/heads/main, refs/heads/release": {"refs/heads/main", "refs/heads/release"},
		"none":                                nil,
		"":                                    nil, // set but empty: trust nothing
		"none,refs/heads/x":                   {"refs/heads/x"},
	} {
		t.Setenv("CACHE_TRUSTED_REFS", in)
		if got := trustedRefs(); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %v want %v", in, got, want)
		}
	}
}

func TestDashboardPeers(t *testing.T) {
	t.Setenv("DASHBOARD_PEERS", "lite=http://a:8080, sysbox=http://b:8080,broken,=http://c,nourl=,")
	want := []dashboard.Peer{{Name: "lite", URL: "http://a:8080"}, {Name: "sysbox", URL: "http://b:8080"}}
	if got := dashboardPeers(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	t.Setenv("DASHBOARD_PEERS", "")
	if got := dashboardPeers(); len(got) != 0 {
		t.Errorf("none configured: %v", got)
	}
}

func TestRunHealthFailsWithoutGuard(t *testing.T) {
	if err := runHealth([]string{"-socket", filepath.Join(t.TempDir(), "none.sock")}); err == nil {
		t.Error("health must fail when no guard answers on the socket")
	}
}

// fakeGitHub answers the few GitHub and Actions-service calls ensureScaleSet makes. The scaleset client treats a
// non-github.com host as GitHub Enterprise Server, so the API lives under /api/v3 on the test server.
type fakeGitHub struct {
	t           *testing.T
	mu          sync.Mutex
	sets        map[int]scaleset.RunnerScaleSet
	nextID      int
	ignorePatch bool // accept a PATCH but leave the labels alone, as the real service has been seen to do
	calls       []string
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, *scaleset.Client) {
	f := &fakeGitHub{t: t, sets: map[int]scaleset.RunnerScaleSet{}, nextID: 100}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		reply := func(v any) { json.NewEncoder(w).Encode(v) }
		switch p := r.URL.Path; {
		case p == "/api/v3/orgs/acme/actions/runners/registration-token":
			w.WriteHeader(http.StatusCreated)
			reply(map[string]any{"token": "reg", "expires_at": time.Now().Add(time.Hour)})
		case p == "/api/v3/actions/runner-registration":
			// The client only reads the expiry from this token, without verifying it.
			claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix())))
			hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
			reply(map[string]string{"url": srv.URL + "/actions", "token": hdr + "." + claims + ".sig"})
		case strings.HasPrefix(p, "/actions/_apis/runtime/runnerscalesets"):
			f.scaleSets(w, r, strings.TrimPrefix(p, "/actions/_apis/runtime/runnerscalesets"))
		default:
			f.t.Errorf("unexpected request %s %s", r.Method, p)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	gh, err := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL: srv.URL + "/acme", PersonalAccessToken: "pat", SystemInfo: systemInfo(0)})
	if err != nil {
		t.Fatal(err)
	}
	return f, gh
}

func (f *fakeGitHub) scaleSets(w http.ResponseWriter, r *http.Request, rest string) {
	f.calls = append(f.calls, r.Method)
	reply := func(v any) { json.NewEncoder(w).Encode(v) }
	id, _ := strconv.Atoi(strings.TrimPrefix(rest, "/"))
	switch r.Method {
	case http.MethodGet:
		var out []scaleset.RunnerScaleSet
		for _, s := range f.sets {
			if s.Name == r.URL.Query().Get("name") {
				out = append(out, s)
			}
		}
		reply(map[string]any{"count": len(out), "value": out})
	case http.MethodPost:
		var s scaleset.RunnerScaleSet
		json.NewDecoder(r.Body).Decode(&s)
		f.nextID++
		s.ID = f.nextID
		f.sets[s.ID] = s
		reply(s)
	case http.MethodPatch:
		var s scaleset.RunnerScaleSet
		json.NewDecoder(r.Body).Decode(&s)
		cur := f.sets[id]
		if !f.ignorePatch {
			cur.Labels = s.Labels
			f.sets[id] = cur
		}
		reply(cur)
	case http.MethodDelete:
		delete(f.sets, id)
		w.WriteHeader(http.StatusNoContent)
	}
}

func labelNames(ls []scaleset.Label) []string {
	var n []string
	for _, l := range ls {
		n = append(n, l.Name)
	}
	sort.Strings(n)
	return n
}

func TestEnsureScaleSet(t *testing.T) {
	ctx := context.Background()
	want := []string{"homelab-sysbox", "self-hosted"}

	t.Run("creates a missing scale set", func(t *testing.T) {
		f, gh := newFakeGitHub(t)
		ss, err := ensureScaleSet(ctx, gh, "homelab-sysbox", scaleset.DefaultRunnerGroup, []string{"self-hosted", "homelab-sysbox"})
		if err != nil {
			t.Fatal(err)
		}
		if len(f.sets) != 1 || ss.Name != "homelab-sysbox" || !reflect.DeepEqual(labelNames(ss.Labels), want) {
			t.Errorf("got %+v, sets %+v", ss, f.sets)
		}
		if !ss.RunnerSetting.DisableUpdate {
			t.Error("the runner must be created with DisableUpdate set")
		}
	})

	t.Run("leaves matching labels alone", func(t *testing.T) {
		f, gh := newFakeGitHub(t)
		first, err := ensureScaleSet(ctx, gh, "homelab-sysbox", scaleset.DefaultRunnerGroup, []string{"self-hosted", "homelab-sysbox"})
		if err != nil {
			t.Fatal(err)
		}
		f.calls = nil
		// same set, different order and case: no write calls
		again, err := ensureScaleSet(ctx, gh, "homelab-sysbox", scaleset.DefaultRunnerGroup, []string{"HOMELAB-SYSBOX", "self-hosted"})
		if err != nil {
			t.Fatal(err)
		}
		if again.ID != first.ID || len(f.sets) != 1 {
			t.Errorf("id %d -> %d, sets %d", first.ID, again.ID, len(f.sets))
		}
		for _, c := range f.calls {
			if c != http.MethodGet {
				t.Errorf("unexpected %s call for unchanged labels", c)
			}
		}
	})

	t.Run("updates changed labels in place", func(t *testing.T) {
		f, gh := newFakeGitHub(t)
		first, _ := ensureScaleSet(ctx, gh, "homelab-sysbox", scaleset.DefaultRunnerGroup, []string{"self-hosted", "old"})
		got, err := ensureScaleSet(ctx, gh, "homelab-sysbox", scaleset.DefaultRunnerGroup, []string{"self-hosted", "homelab-sysbox"})
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != first.ID || !reflect.DeepEqual(labelNames(got.Labels), want) || len(f.sets) != 1 {
			t.Errorf("got %+v, sets %+v", got, f.sets)
		}
	})

	t.Run("recreates the set when GitHub ignores the label update", func(t *testing.T) {
		f, gh := newFakeGitHub(t)
		f.ignorePatch = true
		first, _ := ensureScaleSet(ctx, gh, "homelab-sysbox", scaleset.DefaultRunnerGroup, []string{"self-hosted", "old"})
		got, err := ensureScaleSet(ctx, gh, "homelab-sysbox", scaleset.DefaultRunnerGroup, []string{"self-hosted", "homelab-sysbox"})
		if err != nil {
			t.Fatal(err)
		}
		if got.ID == first.ID || len(f.sets) != 1 || !reflect.DeepEqual(labelNames(f.sets[got.ID].Labels), want) {
			t.Errorf("not recreated: first %d, got %+v, sets %+v", first.ID, got, f.sets)
		}
	})

	t.Run("a duplicate label cannot hide a change", func(t *testing.T) {
		f, gh := newFakeGitHub(t)
		first, _ := ensureScaleSet(ctx, gh, "x", scaleset.DefaultRunnerGroup, []string{"self-hosted", "x"})
		got, err := ensureScaleSet(ctx, gh, "x", scaleset.DefaultRunnerGroup, []string{"x", "x"})
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != first.ID || !reflect.DeepEqual(labelNames(f.sets[got.ID].Labels), []string{"x", "x"}) {
			t.Errorf("labels not updated: %+v", f.sets)
		}
	})
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"": 0, "1024": 1024, "5M": 5 << 20, "5MB": 5 << 20, "2k": 2 << 10, "1G": 1 << 30, " 3m ": 3 << 20} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"x", "0", "-5M", "M"} {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) accepted", in)
		}
	}
}
