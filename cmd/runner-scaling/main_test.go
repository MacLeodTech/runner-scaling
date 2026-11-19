package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

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
