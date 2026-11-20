package onboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MacLeodTech/runner-scaling/internal/ghapi"
)

// AppFlow creates the GitHub App through GitHub's "App manifest" flow: a local page posts a manifest to GitHub, you
// press one button there, GitHub redirects back with a one-time code, and the code is exchanged for the App's id,
// Client ID and private key. Then it waits for you to install the App and reads the installation id.
type AppFlow struct {
	Org     string
	Name    string // App name, e.g. runner-scaler
	Listen  string // 127.0.0.1:0 by default
	Web     string // https://github.com
	API     string // https://api.github.com
	HTTP    *http.Client
	Timeout time.Duration
	// Announce is told the URL to open once the local page is ready.
	Announce func(url string)
}

// AppResult is what the flow produces.
type AppResult struct {
	AppID          int64
	Slug           string
	ClientID       string
	PEM            []byte
	InstallationID int64
	Account        string
}

func (f *AppFlow) manifest(redirect string) map[string]any {
	return map[string]any{
		"name":            f.Name,
		"url":             "https://github.com/MacLeodTech/runner-scaling",
		"redirect_url":    redirect,
		"public":          false,
		"hook_attributes": map[string]any{"url": "https://example.invalid/unused", "active": false},
		"default_permissions": map[string]string{
			"organization_self_hosted_runners": "write", // the only permission the scaler needs
		},
		"default_events": []string{},
	}
}

// Run blocks until the App exists and is installed, or ctx/timeout ends.
func (f *AppFlow) Run(ctx context.Context) (*AppResult, error) {
	if f.Org == "" {
		return nil, errors.New("an organization is required")
	}
	if f.Listen == "" {
		f.Listen = "127.0.0.1:0"
	}
	if f.Web == "" {
		f.Web = "https://github.com"
	}
	if f.API == "" {
		f.API = "https://api.github.com"
	}
	if f.HTTP == nil {
		f.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if f.Timeout == 0 {
		f.Timeout = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, f.Timeout)
	defer cancel()

	ln, err := net.Listen("tcp", f.Listen)
	if err != nil {
		return nil, err
	}
	defer ln.Close()
	base := "http://" + ln.Addr().String()
	st := make([]byte, 16)
	rand.Read(st)
	state := hex.EncodeToString(st)
	mj, _ := json.Marshal(f.manifest(base + "/callback"))
	target := fmt.Sprintf("%s/organizations/%s/settings/apps/new?state=%s", f.Web, url.PathEscape(f.Org), state)

	got := make(chan *AppResult, 1)
	fail := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><meta charset=utf-8><title>Create the GitHub App</title>
<body style="font:16px system-ui;max-width:40em;margin:3em auto">
<h1>Create the runner-scaling GitHub App</h1>
<p>This sends the App's settings to GitHub (organization <b>%s</b>). On the next page, review them and press
<b>Create GitHub App</b>. The only permission is <i>Self-hosted runners: read and write</i>.</p>
<form id=f method=post action="%s"><input type=hidden name=manifest value="%s"><button>Continue to GitHub</button></form>
</body>`, html.EscapeString(f.Org), html.EscapeString(target), html.EscapeString(string(mj)))
	})
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		res, err := f.convert(r.Context(), r.URL.Query().Get("code"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			select {
			case fail <- err:
			default:
			}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		inst := fmt.Sprintf("%s/apps/%s/installations/new", f.Web, url.PathEscape(res.Slug))
		fmt.Fprintf(w, `<!doctype html><meta charset=utf-8><body style="font:16px system-ui;max-width:40em;margin:3em auto">
<h1>App created</h1><p>One more step: <a href="%s">install it on <b>%s</b></a> (choose <i>All repositories</i>: it only manages runners).
Then go back to your terminal.</p></body>`, html.EscapeString(inst), html.EscapeString(f.Org))
		select {
		case got <- res:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()
	if f.Announce != nil {
		f.Announce(base + "/")
	}

	var res *AppResult
	select {
	case res = <-got:
	case err := <-fail:
		return nil, err
	case <-ctx.Done():
		return nil, errors.New("timed out waiting for the App to be created on GitHub")
	}
	key, err := ghapi.ParseKey(res.PEM)
	if err != nil {
		return nil, err
	}
	a := &ghapi.AppAuth{ClientID: res.ClientID, Key: key}
	for {
		id, acct, err := f.findInstallation(ctx, a)
		if err == nil && id != 0 {
			res.InstallationID, res.Account = id, acct
			return res, nil
		}
		select {
		case <-ctx.Done():
			return res, errors.New("the App was created but not installed in time: install it, then run init again with --client-id/--installation-id/--key-file")
		case <-time.After(3 * time.Second):
		}
	}
}

// convert trades the one-time code for the App's credentials.
func (f *AppFlow) convert(ctx context.Context, code string) (*AppResult, error) {
	if code == "" {
		return nil, errors.New("no code in the redirect")
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(f.API, "/")+"/app-manifests/"+url.PathEscape(code)+"/conversions", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("GitHub refused the code: %s", resp.Status)
	}
	var v struct {
		ID       int64
		Slug     string
		ClientID string `json:"client_id"`
		PEM      string `json:"pem"`
	}
	if err := json.Unmarshal(b, &v); err != nil || v.PEM == "" || v.ClientID == "" {
		return nil, errors.New("GitHub's answer had no key or Client ID")
	}
	return &AppResult{AppID: v.ID, Slug: v.Slug, ClientID: v.ClientID, PEM: []byte(v.PEM)}, nil
}

// findInstallation returns the App's installation on the organization, if there is one.
func (f *AppFlow) findInstallation(ctx context.Context, a *ghapi.AppAuth) (int64, string, error) {
	jwt, err := a.JWT()
	if err != nil {
		return 0, "", err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(f.API, "/")+"/app/installations", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	var list []struct {
		ID      int64
		Account struct{ Login string }
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&list); err != nil {
		return 0, "", err
	}
	for _, i := range list {
		if strings.EqualFold(i.Account.Login, f.Org) {
			return i.ID, i.Account.Login, nil
		}
	}
	return 0, "", nil
}
