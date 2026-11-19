// Package ghapi is a small read-only client for the parts of the GitHub REST API the dashboard uses to show what a
// job is doing: its run, its steps and, for failures, the tail of its log. It uses conditional requests (ETag) so a
// poll that finds nothing new costs no rate limit, and it stops polling when the limit runs low.
package ghapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Step is one step of a job.
type Step struct {
	Number      int       `json:"number"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`     // queued, in_progress, completed
	Conclusion  string    `json:"conclusion"` // success, failure, skipped, cancelled, ...
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
}

// Job is one job of a workflow run.
type Job struct {
	ID              int64     `json:"id"`
	RunID           int64     `json:"run_id"`
	RunAttempt      int       `json:"run_attempt"`
	Name            string    `json:"name"`
	Status          string    `json:"status"`
	Conclusion      string    `json:"conclusion"`
	HTMLURL         string    `json:"html_url"`
	HeadSHA         string    `json:"head_sha"`
	HeadBranch      string    `json:"head_branch"`
	RunnerName      string    `json:"runner_name"`
	RunnerGroupName string    `json:"runner_group_name"`
	Labels          []string  `json:"labels"`
	CreatedAt       time.Time `json:"created_at"`
	StartedAt       time.Time `json:"started_at"`
	CompletedAt     time.Time `json:"completed_at"`
	Steps           []Step    `json:"steps"`
}

// Run is a workflow run.
type Run struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	DisplayTitle string    `json:"display_title"`
	Event        string    `json:"event"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	HeadBranch   string    `json:"head_branch"`
	HeadSHA      string    `json:"head_sha"`
	HTMLURL      string    `json:"html_url"`
	RunAttempt   int       `json:"run_attempt"`
	RunStartedAt time.Time `json:"run_started_at"`
	Actor        struct {
		Login string `json:"login"`
	} `json:"actor"`
	TriggeringActor struct {
		Login string `json:"login"`
	} `json:"triggering_actor"`
	PullRequests []struct {
		Number int `json:"number"`
	} `json:"pull_requests"`
}

// Status is what the client knows about its own health, for the dashboard.
type Status struct {
	Enabled     bool      `json:"enabled"`
	Error       string    `json:"error,omitempty"` // last problem; empty when the last poll worked
	Remaining   int       `json:"rate_remaining"`
	Limit       int       `json:"rate_limit"`
	Reset       time.Time `json:"rate_reset,omitempty"`
	LastOK      time.Time `json:"last_ok,omitempty"`
	Requests    int64     `json:"requests"`
	NotModified int64     `json:"not_modified"`
	Errors      int64     `json:"errors"`
}

// APIError is a non-success answer from GitHub.
type APIError struct {
	Status int
	Path   string
	Msg    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("GitHub %d for %s: %s", e.Status, e.Path, e.Msg)
}

// Denied reports whether the credential may not read this resource (a missing permission looks like a 403 or 404).
func (e *APIError) Denied() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden || e.Status == http.StatusNotFound
}

// ErrRateLow is returned instead of calling GitHub when few requests remain in the window.
var ErrRateLow = errors.New("GitHub rate limit nearly used up; waiting for it to reset")

// minRemaining is how many requests are always left alone for the scale set's own use.
const minRemaining = 200

type cached struct {
	etag string
	body []byte
}

// Client talks to the GitHub REST API. Safe for concurrent use.
type Client struct {
	Base string // https://api.github.com
	Auth Auth
	HC   *http.Client
	Now  func() time.Time

	mu     sync.Mutex
	etags  map[string]cached
	status Status
}

// New returns a client for base (default https://api.github.com).
func New(base string, auth Auth) *Client {
	if base == "" {
		base = "https://api.github.com"
	}
	return &Client{Base: strings.TrimRight(base, "/"), Auth: auth, HC: &http.Client{Timeout: 20 * time.Second}, Now: time.Now,
		etags: map[string]cached{}, status: Status{Enabled: true}}
}

// Status returns a copy of the client's health.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// get returns the body for path, using a stored ETag when there is one.
func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	c.mu.Lock()
	if c.status.Limit > 0 && c.status.Remaining < minRemaining && c.Now().Before(c.status.Reset) {
		c.mu.Unlock()
		return nil, ErrRateLow
	}
	prev, have := c.etags[path]
	c.mu.Unlock()

	tok, err := c.Auth.Token(ctx)
	if err != nil {
		c.fail(err)
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if have {
		req.Header.Set("If-None-Match", prev.etag)
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		c.fail(err)
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.Requests++
	if v, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining")); err == nil {
		c.status.Remaining = v
		c.status.Limit, _ = strconv.Atoi(resp.Header.Get("X-RateLimit-Limit"))
		if r, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			c.status.Reset = time.Unix(r, 0)
		}
	}
	switch {
	case resp.StatusCode == http.StatusNotModified && have:
		c.status.NotModified++
		c.status.LastOK, c.status.Error = c.Now(), ""
		return prev.body, nil
	case resp.StatusCode == http.StatusOK:
		if et := resp.Header.Get("ETag"); et != "" {
			c.etags[path] = cached{et, body}
		}
		c.status.LastOK, c.status.Error = c.Now(), ""
		return body, nil
	}
	var m struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &m)
	e := &APIError{Status: resp.StatusCode, Path: path, Msg: m.Message}
	c.status.Errors++
	if e.Denied() {
		c.status.Error = "the GitHub credential cannot read workflow runs (grant the App or token Actions: read on the repos): " + e.Error()
	} else {
		c.status.Error = e.Error()
	}
	return nil, e
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	c.status.Errors++
	c.status.Error = err.Error()
	c.mu.Unlock()
}

func runPath(owner, repo string, runID int64) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/actions/runs/" + strconv.FormatInt(runID, 10)
}

// Run fetches one workflow run.
func (c *Client) Run(ctx context.Context, owner, repo string, runID int64) (*Run, error) {
	b, err := c.get(ctx, runPath(owner, repo, runID))
	if err != nil {
		return nil, err
	}
	var r Run
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Jobs fetches the jobs of the latest attempt of a run, with their steps (up to 100 jobs).
func (c *Client) Jobs(ctx context.Context, owner, repo string, runID int64) ([]Job, error) {
	b, err := c.get(ctx, runPath(owner, repo, runID)+"/jobs?filter=latest&per_page=100")
	if err != nil {
		return nil, err
	}
	var r struct {
		Jobs []Job `json:"jobs"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return r.Jobs, nil
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
var stamp = regexp.MustCompile(`^\d{4}-\d\d-\d\dT[\d:.]+Z `)

// LogTail returns the last n lines of a finished job's log, without colour codes or timestamps. The log endpoint
// answers with a redirect to a signed URL, which is fetched without the credential. Output is capped, and GitHub has
// already masked secrets in it.
func (c *Client) LogTail(ctx context.Context, owner, repo string, jobID int64, n int) ([]string, error) {
	c.mu.Lock()
	low := c.status.Limit > 0 && c.status.Remaining < minRemaining && c.Now().Before(c.status.Reset)
	c.mu.Unlock()
	if low {
		return nil, ErrRateLow
	}
	tok, err := c.Auth.Token(ctx)
	if err != nil {
		return nil, err
	}
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/actions/jobs/" + strconv.FormatInt(jobID, 10) + "/logs"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	noRedirect := *c.HC
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirect.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	c.mu.Lock()
	c.status.Requests++
	c.mu.Unlock()
	body := resp.Body
	if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusTemporaryRedirect {
		loc, err := resp.Location()
		if err != nil {
			return nil, err
		}
		r2, err := http.NewRequestWithContext(ctx, http.MethodGet, loc.String(), nil) // signed URL: no credential
		if err != nil {
			return nil, err
		}
		resp2, err := c.HC.Do(r2)
		if err != nil {
			return nil, err
		}
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusOK {
			return nil, &APIError{Status: resp2.StatusCode, Path: path, Msg: "log download failed"}
		}
		body = resp2.Body
	} else if resp.StatusCode != http.StatusOK {
		return nil, &APIError{Status: resp.StatusCode, Path: path, Msg: "log not available"}
	}
	raw, _ := io.ReadAll(io.LimitReader(body, 8<<20))
	return tail(string(raw), n), nil
}

func tail(s string, n int) []string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = stamp.ReplaceAllString(ansi.ReplaceAllString(l, ""), "")
		if len(l) > 400 {
			l = l[:400] + "…"
		}
		out = append(out, l)
	}
	return out
}
