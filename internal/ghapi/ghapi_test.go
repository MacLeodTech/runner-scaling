package ghapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The fixtures are real GitHub responses (scrubbed), so this checks the types against the live shape.
func TestJobsAndRunDecodeRealResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth header: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("X-RateLimit-Remaining", "4000")
		w.Header().Set("X-RateLimit-Limit", "5000")
		switch {
		case strings.HasSuffix(r.URL.Path, "/jobs"):
			w.Write(fixture(t, "jobs.json"))
		default:
			w.Write(fixture(t, "run.json"))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, TokenAuth("tok"))
	jobs, err := c.Jobs(context.Background(), "example", "repo", 44552049822)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs: %v %v", jobs, err)
	}
	j := jobs[0]
	if j.ID == 0 || j.Name == "" || j.RunnerName == "" || j.Status != "completed" || j.Conclusion == "" || j.StartedAt.IsZero() || len(j.Steps) == 0 || j.Steps[0].Name == "" || j.Steps[0].StartedAt.IsZero() {
		t.Fatalf("job not decoded: %+v", j)
	}
	r, err := c.Run(context.Background(), "example", "repo", 44552049822)
	if err != nil || r.DisplayTitle == "" || r.Actor.Login != "octocat" || r.HTMLURL == "" || r.Event == "" {
		t.Fatalf("run: %+v %v", r, err)
	}
	if st := c.Status(); st.Remaining != 4000 || st.Limit != 5000 || st.Error != "" || st.Requests != 2 {
		t.Fatalf("status: %+v", st)
	}
}

func TestETagSecondCallIsNotModified(t *testing.T) {
	var hits, conditional int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Header.Get("If-None-Match") == `"v1"` {
			atomic.AddInt32(&conditional, 1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Write(fixture(t, "jobs.json"))
	}))
	defer srv.Close()
	c := New(srv.URL, TokenAuth("tok"))
	for i := 0; i < 3; i++ {
		jobs, err := c.Jobs(context.Background(), "o", "r", 1)
		if err != nil || len(jobs) != 2 {
			t.Fatalf("call %d: %v %v", i, jobs, err)
		}
	}
	if st := c.Status(); conditional != 2 || st.NotModified != 2 {
		t.Fatalf("conditional=%d status=%+v", conditional, st)
	}
}

func TestDeniedIsReportedAndFlagged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, TokenAuth("tok"))
	_, err := c.Jobs(context.Background(), "o", "r", 1)
	var ae *APIError
	if !errors.As(err, &ae) || !ae.Denied() {
		t.Fatalf("want a denied APIError, got %v", err)
	}
	if st := c.Status(); !strings.Contains(st.Error, "Actions: read") || st.Errors != 1 {
		t.Fatalf("status: %+v", st)
	}
}

func TestStopsWhenRateLimitIsLow(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("X-RateLimit-Remaining", "10")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Reset", "9999999999")
		w.Write(fixture(t, "jobs.json"))
	}))
	defer srv.Close()
	c := New(srv.URL, TokenAuth("tok"))
	if _, err := c.Jobs(context.Background(), "o", "r", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Jobs(context.Background(), "o", "r", 1); !errors.Is(err, ErrRateLow) || hits != 1 {
		t.Fatalf("err=%v hits=%d", err, hits)
	}
}

func TestLogTailFollowsRedirectWithoutCredential(t *testing.T) {
	var sawAuthOnBlob bool
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuthOnBlob = r.Header.Get("Authorization") != ""
		w.Write([]byte("2026-11-19T16:01:59.1234567Z first\n2026-11-19T16:02:00.0000000Z \x1b[31mred\x1b[0m line\n2026-11-19T16:02:01.0000000Z last\n\n"))
	}))
	defer blob.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, blob.URL+"/signed", http.StatusFound)
	}))
	defer api.Close()
	c := New(api.URL, TokenAuth("tok"))
	got, err := c.LogTail(context.Background(), "o", "r", 7, 2)
	if err != nil || len(got) != 2 || got[0] != "red line" || got[1] != "last" {
		t.Fatalf("got %q err %v", got, err)
	}
	if sawAuthOnBlob {
		t.Fatal("the credential was sent to the signed log URL")
	}
}

func TestAppAuthMintsAndCachesInstallationToken(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pk := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	parsed, err := ParseKey(pk)
	if err != nil {
		t.Fatal(err)
	}
	var mints int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/42/access_tokens" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") || strings.Count(r.Header.Get("Authorization"), ".") != 2 {
			t.Errorf("bad mint request: %s %s %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		atomic.AddInt32(&mints, 1)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"token":"inst-tok","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
	}))
	defer srv.Close()
	a := &AppAuth{Base: srv.URL, ClientID: "Iv1.x", InstallationID: 42, Key: parsed}
	for i := 0; i < 3; i++ {
		if tok, err := a.Token(context.Background()); err != nil || tok != "inst-tok" {
			t.Fatalf("token %q %v", tok, err)
		}
	}
	if mints != 1 {
		t.Fatalf("minted %d times, want 1 (cached)", mints)
	}
	// near expiry: a fresh one is minted
	a.expires = time.Now().Add(time.Minute)
	a.Token(context.Background())
	if mints != 2 {
		t.Fatalf("minted %d times after near-expiry, want 2", mints)
	}
}

func TestParseKeyRejectsGarbage(t *testing.T) {
	if _, err := ParseKey([]byte("nope")); err == nil {
		t.Fatal("want an error")
	}
}
