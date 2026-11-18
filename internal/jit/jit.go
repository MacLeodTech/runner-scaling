// Package jit validates GitHub Actions just-in-time runner configs before the guard hands one to a runner.
//
// An encoded JIT config is base64 of a JSON object that maps runner file names (".runner", ".credentials",
// ".credentials_rsaparams") to base64 file contents. Nothing in it is signed, and the runner trusts whatever servers
// it names, so a scaler that has been taken over could hand the guard a config of its own making: one pointing at
// a server it runs, or a genuine one from a GitHub org it controls. Either way its jobs would run on this host.
//
// What ties a config to one org is the Actions service tenant URL (https://pipelines….actions.githubusercontent.com/
// <tenant>/). .runner's ServerUrl and the OAuth token URL in .credentials both live under it, and the shared broker
// (ServerUrlV2) only accepts runners holding a token that tenant issued. So the guard insists that:
//
//   - the config decodes and stays under a size limit,
//   - .runner's ServerUrl is under the pinned tenant URL,
//   - every URL anywhere in the config is https and either under the tenant URL or on a shared host (the broker),
//   - .runner names the runner the scaler asked for, and its work folder is the one the guard mounts.
package jit

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// MaxEncodedSize bounds the encoded config. Real ones are about 4 KiB.
const MaxEncodedSize = 64 << 10

// DefaultSharedHosts serve every org and identify runners by their tenant-issued credentials.
var DefaultSharedHosts = []string{"broker.actions.githubusercontent.com"}

// ErrNoTenant is returned when no tenant URL is pinned. The message carries the tenant URL the config named, so an
// administrator can check it belongs to their org and pin it.
var ErrNoTenant = errors.New("no tenant_url pinned")

// Expect is what a config must contain to be accepted.
type Expect struct {
	TenantURL   string   // e.g. https://pipelinesghubeus22.actions.githubusercontent.com/<tenant>/
	RunnerName  string   // the name the scaler asked GitHub for
	WorkFolder  string   // optional: if non-empty, .runner's WorkFolder must equal it when present
	SharedHosts []string // hosts allowed with any path; default DefaultSharedHosts
}

// Validate returns nil if encoded is a JIT config that satisfies e.
func Validate(encoded string, e Expect) error {
	if encoded == "" {
		return errors.New("empty jit config")
	}
	if len(encoded) > MaxEncodedSize {
		return fmt.Errorf("jit config is %d bytes, limit %d", len(encoded), MaxEncodedSize)
	}
	docs, err := decode(encoded)
	if err != nil {
		return err
	}
	runner, ok := docs[".runner"].(map[string]any)
	if !ok {
		return errors.New("jit config has no .runner object")
	}
	server, _ := lookup(runner, "ServerUrl").(string)
	if e.TenantURL == "" {
		return fmt.Errorf("%w: this config's ServerUrl is %q; if that is your org's tenant, set tenant_url to it", ErrNoTenant, server)
	}
	tenant, err := parseTenant(e.TenantURL)
	if err != nil {
		return err
	}
	if !tenant.contains(server) {
		return fmt.Errorf(".runner ServerUrl %q is not under the pinned tenant %q", server, e.TenantURL)
	}
	shared := e.SharedHosts
	if len(shared) == 0 {
		shared = DefaultSharedHosts
	}
	for _, name := range sortedKeys(docs) {
		if err := checkURLs(docs[name], tenant, shared, name); err != nil {
			return err
		}
	}
	if name, _ := lookup(runner, "AgentName").(string); name != e.RunnerName {
		return fmt.Errorf(".runner AgentName is %q, want %q", name, e.RunnerName)
	}
	if e.WorkFolder != "" {
		if wf, ok := lookup(runner, "WorkFolder").(string); ok && wf != "" && wf != e.WorkFolder {
			return fmt.Errorf(".runner WorkFolder is %q, want %q", wf, e.WorkFolder)
		}
	}
	return nil
}

// decode returns each file of the config as parsed JSON. Every file the runner writes from a JIT config is JSON;
// anything else is refused.
func decode(encoded string) (map[string]any, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("jit config is not base64: %w", err)
	}
	var files map[string]string
	if err := json.Unmarshal(raw, &files); err != nil {
		return nil, fmt.Errorf("jit config is not a JSON object of strings: %w", err)
	}
	docs := make(map[string]any, len(files))
	for name, b64 := range files {
		content, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("jit file %q is not base64: %w", name, err)
		}
		var doc any
		if err := json.Unmarshal(content, &doc); err != nil {
			return nil, fmt.Errorf("jit file %q is not JSON: %w", name, err)
		}
		docs[name] = doc
	}
	return docs, nil
}

type tenantURL struct{ host, path string } // path always ends in "/"

func parseTenant(s string) (tenantURL, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || len(strings.Trim(u.Path, "/")) == 0 {
		return tenantURL{}, fmt.Errorf("tenant_url %q must be https://<host>/<tenant>/", s)
	}
	p := u.EscapedPath()
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return tenantURL{host: strings.ToLower(u.Host), path: p}, nil
}

// contains reports whether raw is an https URL on the tenant's host (and port) whose path is the tenant's or below
// it. Encoded or dot segments are refused rather than interpreted.
func (t tenantURL) contains(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || strings.ToLower(u.Host) != t.host {
		return false
	}
	p := u.EscapedPath()
	segs := strings.Split(p, "/")
	if strings.Contains(p, "%") || slices.Contains(segs, "..") || slices.Contains(segs, ".") {
		return false
	}
	return p+"/" == t.path || strings.HasPrefix(p, t.path)
}

// lookup finds a key case-insensitively (the runner's serializer has used both camelCase and PascalCase).
func lookup(m map[string]any, key string) any {
	if v, ok := m[key]; ok {
		return v
	}
	for _, k := range sortedKeys(m) {
		if strings.EqualFold(k, key) {
			return m[k]
		}
	}
	return nil
}

// checkURLs walks a decoded JSON document and rejects any string that looks like a URL (has "://") unless it is
// https and either under the tenant or on a shared host.
func checkURLs(v any, tenant tenantURL, shared []string, where string) error {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(t) {
			if err := checkURLs(t[k], tenant, shared, where+"."+k); err != nil {
				return err
			}
		}
	case []any:
		for i, child := range t {
			if err := checkURLs(child, tenant, shared, fmt.Sprintf("%s[%d]", where, i)); err != nil {
				return err
			}
		}
	case string:
		if !strings.Contains(t, "://") {
			return nil
		}
		u, err := url.Parse(t)
		if err != nil {
			return fmt.Errorf("%s: unparseable URL %q", where, t)
		}
		if u.Scheme != "https" || u.User != nil {
			return fmt.Errorf("%s: URL %q must be https without credentials", where, t)
		}
		if tenant.contains(t) || (u.Port() == "" && HostAllowed(u.Hostname(), shared)) {
			return nil
		}
		return fmt.Errorf("%s: URL %q is neither under the tenant nor on a shared host", where, t)
	}
	return nil
}

// HostAllowed reports whether host matches one of the patterns (exact, or ".suffix" for any subdomain).
func HostAllowed(host string, patterns []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	for _, p := range patterns {
		p = strings.ToLower(p)
		if strings.HasPrefix(p, ".") {
			if strings.HasSuffix(host, p) && len(host) > len(p) {
				return true
			}
		} else if host == p {
			return true
		}
	}
	return false
}

// Shape describes a config without its secrets: each file's top-level keys, plus the scheme and host of every URL.
// The guard logs it when it refuses a config, so a format change on GitHub's side can be diagnosed.
func Shape(encoded string) string {
	docs, err := decode(encoded)
	if err != nil {
		return err.Error()
	}
	var parts []string
	for _, name := range sortedKeys(docs) {
		desc := name + ":"
		if m, ok := docs[name].(map[string]any); ok {
			desc += " keys=" + strings.Join(sortedKeys(m), ",")
		}
		var hosts []string
		collectHosts(docs[name], &hosts)
		if len(hosts) > 0 {
			desc += " url_hosts=" + strings.Join(hosts, ",")
		}
		parts = append(parts, desc)
	}
	return strings.Join(parts, "; ")
}

func collectHosts(v any, out *[]string) {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(t) {
			collectHosts(t[k], out)
		}
	case []any:
		for _, c := range t {
			collectHosts(c, out)
		}
	case string:
		if strings.Contains(t, "://") {
			if u, err := url.Parse(t); err == nil {
				*out = append(*out, u.Scheme+"://"+u.Host)
			}
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for s := range m {
		k = append(k, s)
	}
	slices.Sort(k)
	return k
}
