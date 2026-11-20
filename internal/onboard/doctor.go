// Package onboard holds the first-run helpers: `doctor` (is this host and this directory ready?) and `init`
// (write the files and, optionally, create the GitHub App). Nothing here runs at job time.
package onboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/MacLeodTech/runner-scaling/internal/ghapi"
	"github.com/MacLeodTech/runner-scaling/internal/guard"
)

// Level is how bad a finding is.
type Level int

const (
	Pass Level = iota
	Warn
	Fail
	Skip
)

func (l Level) String() string { return [...]string{"ok", "WARN", "FAIL", "skip"}[l] }

// Check is one finding. Fix says what to do about a Warn or Fail.
type Check struct {
	Name   string `json:"name"`
	Level  Level  `json:"-"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

func mk(l Level, name, detail, fix string) Check {
	return Check{Name: name, Level: l, Status: l.String(), Detail: detail, Fix: fix}
}

// Doctor inspects the host and, if Dir holds a stack, its files.
type Doctor struct {
	Dir         string // the stack directory (compose.yaml, .env, guard*.yaml, secrets/); "" = host checks only
	DockerHost  string // unix socket path; default /var/run/docker.sock
	Offline     bool   // skip the checks that need the internet
	WantSysbox  bool   // the Sysbox runtime is required
	GitHub      bool   // also ask GitHub about the App (needs the key and ids in Dir)
	APIBase     string // https://api.github.com
	HTTP        *http.Client
	ExpectOwner int // uid that must own the key file (65532, the scaler's)
}

const (
	minAPI       = "1.44"
	scalerUID    = 65532
	cgroup2Magic = 0x63677270
)

// Run performs every check and returns them in order. It never changes anything.
func (d Doctor) Run(ctx context.Context) []Check {
	if d.DockerHost == "" {
		d.DockerHost = "/var/run/docker.sock"
	}
	if d.APIBase == "" {
		d.APIBase = "https://api.github.com"
	}
	if d.HTTP == nil {
		d.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	if d.ExpectOwner == 0 {
		d.ExpectOwner = scalerUID
	}
	var out []Check
	out = append(out, d.docker(ctx)...)
	out = append(out, d.kernel()...)
	out = append(out, d.resources()...)
	if d.Offline {
		out = append(out, mk(Skip, "internet", "offline mode", ""))
	} else {
		out = append(out, d.internet(ctx)...)
	}
	if d.Dir != "" {
		out = append(out, d.stack(ctx)...)
	}
	return out
}

// ---- Docker ----

func (d Doctor) dockerClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", d.DockerHost)
		}}}
}

func (d Doctor) dget(ctx context.Context, path string, v any) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://docker"+path, nil)
	resp, err := d.dockerClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return json.Unmarshal(b, v)
}

type dockerInfo struct {
	SecurityOptions []string
	CgroupVersion   string
	DockerRootDir   string
	Runtimes        map[string]json.RawMessage
	MemTotal        int64
	NCPU            int
}

func (d Doctor) docker(ctx context.Context) []Check {
	var out []Check
	var ver struct{ Version, ApiVersion string }
	if err := d.dget(ctx, "/version", &ver); err != nil {
		hint := "Is Docker installed and running? `systemctl status docker`"
		if errors.Is(err, os.ErrPermission) || strings.Contains(err.Error(), "permission denied") {
			hint = "Your user cannot use the Docker socket: add it to the docker group (`sudo usermod -aG docker $USER`, then log in again) or run with sudo"
		}
		return []Check{mk(Fail, "docker", err.Error(), hint)}
	}
	if apiLess(ver.ApiVersion, minAPI) {
		out = append(out, mk(Fail, "docker version", fmt.Sprintf("Docker %s speaks API %s; need %s or newer", ver.Version, ver.ApiVersion, minAPI), "Upgrade Docker Engine to 25 or newer"))
	} else {
		out = append(out, mk(Pass, "docker version", fmt.Sprintf("Docker %s (API %s)", ver.Version, ver.ApiVersion), ""))
	}
	var info dockerInfo
	if err := d.dget(ctx, "/info", &info); err != nil {
		return append(out, mk(Warn, "docker info", err.Error(), ""))
	}
	rootless := false
	for _, o := range info.SecurityOptions {
		if strings.Contains(o, "rootless") {
			rootless = true
		}
	}
	if rootless {
		out = append(out, mk(Fail, "rootful docker", "rootless Docker detected", "The guard needs the real rootful socket; rootless Docker is untested"))
	} else {
		out = append(out, mk(Pass, "rootful docker", "", ""))
	}
	if fi, err := os.Stat(d.DockerHost); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			out = append(out, mk(Pass, "docker socket group", fmt.Sprintf("gid %d: put DOCKER_GID=%d in .env", st.Gid, st.Gid), ""))
		}
	}
	if err := exec.CommandContext(ctx, "docker", "compose", "version", "--short").Run(); err != nil {
		out = append(out, mk(Fail, "docker compose", "`docker compose version` failed", "Install Docker Compose v2 (the docker-compose-plugin package)"))
	} else {
		out = append(out, mk(Pass, "docker compose", "", ""))
	}
	_, hasSysbox := info.Runtimes["sysbox-runc"]
	switch {
	case hasSysbox:
		out = append(out, mk(Pass, "sysbox runtime", "sysbox-runc is registered", ""))
	case d.WantSysbox:
		out = append(out, mk(Fail, "sysbox runtime", "sysbox-runc is not registered with Docker", "Install Sysbox: docs/SYSBOX.md"))
	default:
		out = append(out, mk(Skip, "sysbox runtime", "not installed (only needed for a pool with Docker in jobs)", ""))
	}
	if info.DockerRootDir != "" {
		var fs syscall.Statfs_t
		if err := syscall.Statfs(info.DockerRootDir, &fs); err == nil {
			gb := int64(fs.Bavail) * int64(fs.Bsize) >> 30
			switch {
			case gb < 15:
				out = append(out, mk(Fail, "disk", fmt.Sprintf("%d GB free under %s", gb, info.DockerRootDir), "The runner image alone is about 2.4 GB; free space or move Docker's data-root"))
			case gb < 30:
				out = append(out, mk(Warn, "disk", fmt.Sprintf("%d GB free under %s", gb, info.DockerRootDir), "30 GB or more is comfortable"))
			default:
				out = append(out, mk(Pass, "disk", fmt.Sprintf("%d GB free under %s", gb, info.DockerRootDir), ""))
			}
		}
	}
	return out
}

func apiLess(have, want string) bool {
	h, w := strings.Split(have, "."), strings.Split(want, ".")
	for i := 0; i < 2; i++ {
		a, b := 0, 0
		if i < len(h) {
			a, _ = strconv.Atoi(h[i])
		}
		if i < len(w) {
			b, _ = strconv.Atoi(w[i])
		}
		if a != b {
			return a < b
		}
	}
	return false
}

// ---- kernel and resources ----

func (d Doctor) kernel() []Check {
	var out []Check
	var fs syscall.Statfs_t
	if err := syscall.Statfs("/sys/fs/cgroup", &fs); err == nil && int64(fs.Type) == cgroup2Magic {
		out = append(out, mk(Pass, "cgroup v2", "", ""))
	} else {
		out = append(out, mk(Warn, "cgroup v2", "not in use", "Only memory_high needs it; the memory budget works on v1"))
	}
	// nf_tables: a module under /sys/module (loaded or built in) or listed in /proc/modules.
	nft := false
	if _, err := os.Stat("/sys/module/nf_tables"); err == nil {
		nft = true
	}
	if b, err := os.ReadFile("/proc/modules"); err == nil && strings.Contains(string(b), "nf_tables ") {
		nft = true
	}
	if nft {
		out = append(out, mk(Pass, "nf_tables", "the firewall sidecar can fence the runners", ""))
	} else {
		out = append(out, mk(Warn, "nf_tables", "not found in this kernel (it may be loaded on demand)", "Check with `sudo nft list tables`. With only legacy iptables use examples/firewall-iptables.sh"))
	}
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); err == nil && strings.TrimSpace(string(b)) == "0" {
		out = append(out, mk(Warn, "ip_forward", "net.ipv4.ip_forward is 0", "Docker normally turns it on; runners need it to reach the internet"))
	}
	return out
}

func (d Doctor) resources() []Check {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil
	}
	var kb int64
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "MemTotal:") {
			fmt.Sscanf(strings.TrimPrefix(l, "MemTotal:"), "%d", &kb)
		}
	}
	gb := kb >> 20
	switch {
	case gb < 4:
		return []Check{mk(Fail, "memory", fmt.Sprintf("%d GB", gb), "A runner may use 3 GB; this host is too small")}
	case gb < 8:
		return []Check{mk(Warn, "memory", fmt.Sprintf("%d GB", gb), "Set max_slots: 1 and memory_budget to about half of RAM")}
	}
	return []Check{mk(Pass, "memory", fmt.Sprintf("%d GB", gb), "")}
}

// ---- internet ----

func (d Doctor) internet(ctx context.Context) []Check {
	var out []Check
	for _, h := range []string{"api.github.com", "ghcr.io", "broker.actions.githubusercontent.com"} {
		req, _ := http.NewRequestWithContext(ctx, "HEAD", "https://"+h, nil)
		resp, err := d.HTTP.Do(req)
		if err != nil {
			out = append(out, mk(Fail, "reach "+h, err.Error(), "Allow outbound HTTPS (443) from this host to "+h))
			continue
		}
		resp.Body.Close()
		out = append(out, mk(Pass, "reach "+h, resp.Status, ""))
		if h == "api.github.com" {
			if t, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
				skew := time.Since(t)
				if skew < 0 {
					skew = -skew
				}
				if skew > time.Minute {
					out = append(out, mk(Fail, "clock", fmt.Sprintf("this host is %s off GitHub's clock", skew.Round(time.Second)), "Enable time sync (`timedatectl set-ntp true`); GitHub rejects the App's tokens when the clock is off"))
				} else {
					out = append(out, mk(Pass, "clock", fmt.Sprintf("within %s of GitHub", skew.Round(time.Second)), ""))
				}
			}
		}
	}
	return out
}

// ---- the stack directory ----

var fenceRE = regexp.MustCompile(`(?m)^\s*FENCE_SRC:\s*([0-9a-fA-F:./]+)`)

func (d Doctor) stack(ctx context.Context) []Check {
	var out []Check
	env, err := ReadEnv(filepath.Join(d.Dir, ".env"))
	if err != nil {
		return []Check{mk(Fail, ".env", err.Error(), "Run `runner-scaling init`, or copy examples/env.example to .env")}
	}
	for _, k := range []string{"GITHUB_APP_CLIENT_ID", "GITHUB_APP_INSTALLATION_ID", "DOCKER_GID"} {
		if v := env[k]; v == "" || strings.Contains(v, "xxxx") || v == "12345678" {
			out = append(out, mk(Fail, ".env "+k, "empty or still the example value", "Set it in "+filepath.Join(d.Dir, ".env")))
		}
	}
	if fi, err := os.Stat(d.DockerHost); err == nil && env["DOCKER_GID"] != "" {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && strconv.Itoa(int(st.Gid)) != env["DOCKER_GID"] {
			out = append(out, mk(Fail, "DOCKER_GID", fmt.Sprintf(".env says %s but the socket's group is %d", env["DOCKER_GID"], st.Gid), fmt.Sprintf("Set DOCKER_GID=%d in .env", st.Gid)))
		} else {
			out = append(out, mk(Pass, "DOCKER_GID", "matches the Docker socket", ""))
		}
	}
	keyPath := filepath.Join(d.Dir, "secrets", "github_app.pem")
	key, kerr := os.Stat(keyPath)
	switch {
	case kerr != nil:
		out = append(out, mk(Fail, "app key", kerr.Error(), "Put the App's private key at "+keyPath+" (`runner-scaling init --create-app` makes one)"))
	default:
		st, _ := key.Sys().(*syscall.Stat_t)
		perm := key.Mode().Perm()
		switch {
		case st != nil && int(st.Uid) != d.ExpectOwner:
			out = append(out, mk(Fail, "app key owner", fmt.Sprintf("owned by uid %d; the scaler runs as %d and cannot read it", st.Uid, d.ExpectOwner), fmt.Sprintf("sudo chown %d:%d %s", d.ExpectOwner, d.ExpectOwner, keyPath)))
		case perm&0o077 != 0:
			out = append(out, mk(Fail, "app key mode", fmt.Sprintf("mode %04o lets others read the key", perm), "sudo chmod 0400 "+keyPath))
		default:
			out = append(out, mk(Pass, "app key file", "owned by the scaler's uid, mode 0400", ""))
		}
		if b, err := os.ReadFile(keyPath); err == nil {
			if _, err := ghapi.ParseKey(b); err != nil {
				out = append(out, mk(Fail, "app key", err.Error(), "Download a fresh private key from the App's settings page"))
			}
		}
	}
	fence := ""
	if b, err := os.ReadFile(filepath.Join(d.Dir, "compose.yaml")); err == nil {
		if m := fenceRE.FindSubmatch(b); m != nil {
			fence = string(m[1])
		}
	}
	cfgs, _ := filepath.Glob(filepath.Join(d.Dir, "guard*.yaml"))
	if len(cfgs) == 0 {
		out = append(out, mk(Fail, "guard config", "no guard*.yaml in "+d.Dir, "Run `runner-scaling init`"))
	}
	taken := d.usedNetworks(ctx)
	for _, p := range cfgs {
		out = append(out, d.guardConfig(p, env, fence, taken)...)
	}
	if d.GitHub {
		out = append(out, d.github(ctx, env, keyPath)...)
	}
	return out
}

func (d Doctor) guardConfig(path string, env map[string]string, fence string, taken []netip.Prefix) []Check {
	name := filepath.Base(path)
	for k, v := range env { // the compose file passes .env through; the config may reference these
		if _, ok := os.LookupEnv(k); !ok {
			os.Setenv(k, v)
			defer os.Unsetenv(k)
		}
	}
	if _, ok := os.LookupEnv("USERDIR"); !ok {
		os.Setenv("USERDIR", "/srv")
		defer os.Unsetenv("USERDIR")
	}
	cfg, err := guard.LoadConfig(path)
	if err != nil {
		return []Check{mk(Fail, name, err.Error(), "Fix the key it names; `runner-scaling guard --check --config "+path+"` repeats this check")}
	}
	out := []Check{mk(Pass, name, fmt.Sprintf("valid: prefix %s, %d slots", cfg.Prefix, cfg.MaxSlots), "")}
	if strings.Contains(cfg.GitHubURL, "YOUR-ORG") {
		out = append(out, mk(Fail, name+" github_url", "still the example value", "Set github_url: https://github.com/<your-org>"))
	}
	if cfg.TenantURL == "" {
		out = append(out, mk(Warn, name+" tenant_url", "not pinned", "Expected before the first job: after it, the guard logs the URL to pin (QUICKSTART step 6, or `runner-scaling pin-tenant`)"))
	}
	if cfg.Subnet != "" {
		var fp netip.Prefix
		if fence != "" {
			fp, _ = netip.ParsePrefix(fence)
		}
	slots:
		for slot := 1; slot <= cfg.MaxSlots; slot++ {
			sub := strings.ReplaceAll(cfg.Subnet, "{slot}", strconv.Itoa(slot))
			p, err := netip.ParsePrefix(sub)
			if err != nil {
				continue
			}
			if fp.IsValid() && !(fp.Contains(p.Addr()) && fp.Bits() <= p.Bits()) {
				out = append(out, mk(Fail, name+" subnet", fmt.Sprintf("%s (slot %d) is outside FENCE_SRC %s: that runner would not be fenced", sub, slot, fence), "Make FENCE_SRC contain every pool's subnet"))
				break
			}
			for _, t := range taken {
				if t.Overlaps(p) {
					out = append(out, mk(Fail, name+" subnet", fmt.Sprintf("%s (slot %d) overlaps %s, already in use on this host", sub, slot, t), "Pick another range (init chooses a free one) and change subnet and FENCE_SRC together"))
					break slots
				}
			}
		}
	} else {
		out = append(out, mk(Warn, name+" subnet", "empty: Docker picks the ranges, so the fence cannot know them", "Set a fixed subnet inside FENCE_SRC"))
	}
	return out
}

// usedNetworks lists the address ranges this host already uses: interfaces, and Docker's networks that are not
// runner-scaling's own (those are re-created by the guard).
func (d Doctor) usedNetworks(ctx context.Context) []netip.Prefix {
	var out []netip.Prefix
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr().Is4() && !p.Addr().IsLoopback() {
			out = append(out, p.Masked())
		}
	}
	var nets []struct {
		Name   string
		Labels map[string]string
		IPAM   struct{ Config []struct{ Subnet string } }
	}
	if d.dget(ctx, "/networks", &nets) == nil {
		for _, n := range nets {
			if _, ours := n.Labels["io.github.runner-scaling.instance"]; ours {
				continue
			}
			for _, c := range n.IPAM.Config {
				if p, err := netip.ParsePrefix(c.Subnet); err == nil && p.Addr().Is4() {
					out = append(out, p.Masked())
				}
			}
		}
	}
	return out
}

// ReadEnv parses a .env file (KEY=VALUE lines, # comments).
func ReadEnv(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if k, v, ok := strings.Cut(l, "="); ok {
			m[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return m, nil
}

// ---- GitHub (optional) ----

func (d Doctor) github(ctx context.Context, env map[string]string, keyPath string) []Check {
	b, err := os.ReadFile(keyPath)
	if err != nil {
		return []Check{mk(Skip, "github app", "no readable key (run as the key's owner, or with sudo)", "")}
	}
	key, err := ghapi.ParseKey(b)
	if err != nil {
		return []Check{mk(Fail, "github app", err.Error(), "")}
	}
	id, _ := strconv.ParseInt(env["GITHUB_APP_INSTALLATION_ID"], 10, 64)
	a := &ghapi.AppAuth{Base: d.APIBase, ClientID: env["GITHUB_APP_CLIENT_ID"], InstallationID: id, Key: key}
	jwt, err := a.JWT()
	if err != nil {
		return []Check{mk(Fail, "github app", err.Error(), "")}
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(d.APIBase, "/")+"/app/installations/"+strconv.FormatInt(id, 10), nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return []Check{mk(Fail, "github app", err.Error(), "")}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return []Check{mk(Fail, "github app", "GitHub answered "+resp.Status+" for installation "+env["GITHUB_APP_INSTALLATION_ID"], "Check the Client ID, installation ID and that this key belongs to the App")}
	}
	var inst struct {
		Account     struct{ Login string }
		Permissions map[string]string
	}
	json.NewDecoder(resp.Body).Decode(&inst)
	out := []Check{mk(Pass, "github app", "authenticates; installed on "+inst.Account.Login, "")}
	if inst.Permissions["organization_self_hosted_runners"] != "write" {
		out = append(out, mk(Fail, "app permission", "Self-hosted runners is not read and write", "App settings > Permissions > Organization permissions > Self-hosted runners: Read and write, then accept the change on the installation"))
	}
	var extra []string
	for k, v := range inst.Permissions {
		if k != "organization_self_hosted_runners" && k != "metadata" {
			extra = append(extra, k+"="+v)
		}
	}
	if len(extra) > 0 {
		out = append(out, mk(Warn, "app permissions", "more than needed: "+strings.Join(extra, ", "), "Remove everything except Self-hosted runners; a leaked key can then do less"))
	}
	if _, err := exec.LookPath("gh"); err == nil && inst.Account.Login != "" {
		out = append(out, d.runnerGroup(ctx, inst.Account.Login))
	}
	return out
}

// runnerGroup uses the user's own gh login (not the App) to check the runner group is not open to public repos.
func (d Doctor) runnerGroup(ctx context.Context, org string) Check {
	b, err := exec.CommandContext(ctx, "gh", "api", "orgs/"+org+"/actions/runner-groups", "--jq",
		`.runner_groups[] | select(.name=="Default") | "\(.visibility) \(.allows_public_repositories)"`).Output()
	if err != nil {
		return mk(Skip, "runner group", "gh could not read it (not logged in, or not an org admin)", "Check by hand: QUICKSTART step 2")
	}
	switch strings.TrimSpace(string(b)) {
	case "selected false":
		return mk(Pass, "runner group", "Default group: selected repositories, public repositories blocked", "")
	case "":
		return mk(Skip, "runner group", "no Default group found", "")
	}
	return mk(Fail, "runner group", "the Default group is `"+strings.TrimSpace(string(b))+"`", "Org settings > Actions > Runner groups > Default: Selected repositories, and Allow public repositories off")
}
