// Command runner-scaling runs ephemeral, autoscaled GitHub Actions runners on one Docker host without giving the
// internet-facing part of the system the Docker socket.
//
//	runner-scaling init --org ACME              first run: create the GitHub App, write the files for this host
//	runner-scaling doctor [--dir D]             check this host and a stack directory; prints the fix for each problem
//	runner-scaling pin-tenant [--dir D]         after the first refused job: pin the tenant URL the guard saw
//	runner-scaling guard  --config guard.yaml   holds the Docker socket; builds containers only from its config
//	runner-scaling docker-policy --config ...  optional: sits between the guard and the Docker socket and refuses any
//	                                            Docker call that is not exactly one the guard's config produces
//	runner-scaling firewall --src ...           fences the runner networks (nftables) and keeps the gate file while it holds
//	runner-scaling scaler                       talks to GitHub; asks the guard for slots over a unix socket
//	runner-scaling health --socket ...          exits 0 if the guard answers (for container healthchecks)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/MacLeodTech/runner-scaling/internal/api"
	"github.com/MacLeodTech/runner-scaling/internal/dashboard"
	"github.com/MacLeodTech/runner-scaling/internal/fence"
	"github.com/MacLeodTech/runner-scaling/internal/ghapi"
	"github.com/MacLeodTech/runner-scaling/internal/guard"
	"github.com/MacLeodTech/runner-scaling/internal/scaler"
)

var version = "dev" // set with -ldflags "-X main.version=..."

const defaultSocket = "/run/runner-scaling/guard.sock"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))

	var err error
	switch os.Args[1] {
	case "guard":
		err = runGuard(ctx, log, os.Args[2:])
	case "docker-policy":
		err = runDockerPolicy(ctx, log, os.Args[2:])
	case "doctor":
		err = runDoctor(ctx, os.Args[2:])
	case "init":
		err = runInit(ctx, os.Args[2:])
	case "pin-tenant":
		err = runPinTenant(ctx, os.Args[2:])
	case "firewall":
		err = runFirewall(ctx, log, os.Args[2:])
	case "scaler":
		err = runScaler(ctx, log, os.Args[2:])
	case "health":
		err = runHealth(os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		usage()
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: runner-scaling init|doctor|pin-tenant|guard|docker-policy|scaler|firewall|health|version [flags]")
	os.Exit(2)
}

func logLevel() slog.Level {
	var l slog.Level
	_ = l.UnmarshalText([]byte(env("LOG_LEVEL", "info")))
	return l
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

// secret reads NAME, or the file named by NAME_FILE.
func secret(name string) (string, error) {
	if f := os.Getenv(name + "_FILE"); f != "" {
		b, err := os.ReadFile(f)
		return strings.TrimSpace(string(b)), err
	}
	return os.Getenv(name), nil
}

func runGuard(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("guard", flag.ExitOnError)
	cfgPath := fs.String("config", env("GUARD_CONFIG", "/etc/runner-scaling/guard.yaml"), "guard config file")
	socket := fs.String("socket", env("GUARD_SOCKET", defaultSocket), "unix socket to serve the guard API on")
	mode := fs.String("socket-mode", env("GUARD_SOCKET_MODE", "0660"), "permissions of the API socket")
	gid := fs.Int("socket-gid", -1, "group to give the API socket (needs CAP_CHOWN unless running as that group)")
	dockerSock := fs.String("docker", env("DOCKER_SOCKET", "/var/run/docker.sock"), "Docker daemon socket")
	check := fs.Bool("check", false, "validate the config and exit")
	fs.Parse(args)

	cfgs, err := guard.LoadConfigs(*cfgPath)
	if err != nil {
		return err
	}
	if *check {
		for _, c := range cfgs {
			fmt.Printf("%s: ok (pool %s, prefix %s, %d slots, tenant pinned: %v)\n", *cfgPath, c.Key(), c.Prefix, c.MaxSlots, c.TenantURL != "")
		}
		return nil
	}
	pools := guard.NewPools(cfgs, guard.NewSocketDocker(*dockerSock), log)
	if err := pools.Setup(ctx); err != nil {
		return err
	}

	ln, err := listenUnix(*socket, *mode, *gid)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: pools.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second}
	go func() { <-ctx.Done(); srv.Shutdown(context.Background()) }()
	log.Info("guard listening", "socket", *socket, "pools", pools.Keys(), "version", version)
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// listenUnix creates a unix socket closed to everyone but its owner, then opens it to its group.
func listenUnix(socket, mode string, gid int) (net.Listener, error) {
	m, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return nil, fmt.Errorf("socket-mode: %w", err)
	}
	_ = os.Remove(socket)
	oldMask := syscall.Umask(0o177)
	ln, err := net.Listen("unix", socket)
	syscall.Umask(oldMask)
	if err != nil {
		return nil, err
	}
	if gid >= 0 {
		if err := os.Chown(socket, -1, gid); err != nil {
			ln.Close()
			return nil, fmt.Errorf("chown socket: %w", err)
		}
	}
	if err := os.Chmod(socket, os.FileMode(m)); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// runDockerPolicy serves the Docker policy proxy: the guard's Docker socket, minus everything the guard never asks for.
func runDockerPolicy(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("docker-policy", flag.ExitOnError)
	cfgPath := fs.String("config", env("GUARD_CONFIG", "/etc/runner-scaling/guard.yaml"), "the guard's config file (read-only)")
	listen := fs.String("listen", env("POLICY_SOCKET", "/run/docker-policy/docker.sock"), "unix socket the guard connects to")
	mode := fs.String("socket-mode", env("POLICY_SOCKET_MODE", "0660"), "permissions of that socket")
	gid := fs.Int("socket-gid", -1, "group to give that socket")
	dockerSock := fs.String("docker", env("DOCKER_SOCKET", "/var/run/docker.sock"), "the real Docker daemon socket")
	check := fs.Bool("check", false, "validate the config and exit")
	fs.Parse(args)

	cfgs, err := guard.LoadConfigs(*cfgPath)
	if err != nil {
		return err
	}
	if *check {
		for _, c := range cfgs {
			fmt.Printf("%s: ok (policy for pool %s, prefix %s, %d slots)\n", *cfgPath, c.Key(), c.Prefix, c.MaxSlots)
		}
		return nil
	}
	ln, err := listenUnix(*listen, *mode, *gid)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler: guard.NewProxy(cfgs, *dockerSock, log), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second,
	}
	go func() { <-ctx.Done(); srv.Shutdown(context.Background()) }()
	log.Info("docker policy listening", "socket", *listen, "docker", *dockerSock, "pools", len(cfgs), "version", version)
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func runHealth(args []string) error {
	fs := flag.NewFlagSet("health", flag.ExitOnError)
	socket := fs.String("socket", env("GUARD_SOCKET", defaultSocket), "guard API socket")
	policy := fs.String("docker-policy-socket", "", "check the Docker policy proxy on this socket instead of the guard")
	fs.Parse(args)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if *policy != "" {
		return guard.PingDocker(ctx, *policy)
	}
	pools, err := scaler.NewGuardClient(*socket).Pools(ctx)
	if err == nil && len(pools) == 0 {
		err = errors.New("guard runs no pools")
	}
	return err
}

func runScaler(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("scaler", flag.ExitOnError)
	socket := fs.String("socket", env("GUARD_SOCKET", defaultSocket), "guard API socket")
	name := fs.String("name", env("SCALE_SET_NAME", ""), "scale set name (single-pool guard only; default: the pool's prefix)")
	labels := fs.String("labels", env("SCALE_SET_LABELS", ""), "comma-separated labels (single-pool guard only; default: the pool's labels, else the name)")
	group := fs.String("runner-group", env("RUNNER_GROUP", scaleset.DefaultRunnerGroup), "runner group")
	minR := fs.Int("min-runners", atoi(env("MIN_RUNNERS", "0")), "runners kept warm per pool")
	maxR := fs.Int("max-runners", atoi(env("MAX_RUNNERS", "0")), "runner limit per pool (default and ceiling: the pool's max_slots)")
	del := fs.Bool("delete-scale-set", false, "delete the scale sets from GitHub and exit (stop the guard's runners first)")
	fs.Parse(args)

	gc := scaler.NewGuardClient(*socket)
	infos, err := gc.Pools(ctx)
	for err != nil || len(infos) == 0 { // the guard may still be pulling images
		log.Info("waiting for guard", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
		infos, err = gc.Pools(ctx)
	}
	if len(infos) > 1 && (*name != "" || *labels != "") {
		return errors.New("--name and --labels (SCALE_SET_NAME, SCALE_SET_LABELS) only apply to a guard with one pool: set `labels:` per pool in guard.yaml")
	}

	type pool struct {
		info     api.Info
		gc       *scaler.GuardClient
		gh       *scaleset.Client
		name     string
		labels   []string
		min, max int
		s        *scaler.Scaler
	}
	var pools []*pool
	for _, info := range infos {
		p := &pool{info: info, gc: gc.Pool(info.Name), name: *name, min: *minR, max: *maxR}
		if p.name == "" {
			p.name = info.Prefix
		}
		if len(infos) == 1 {
			p.labels = splitLabels(*labels, p.name)
			if *labels == "" && len(info.Labels) > 0 {
				p.labels = info.Labels
			}
		} else {
			p.labels = info.Labels
			if len(p.labels) == 0 {
				p.labels = []string{info.Name}
			}
		}
		if p.max <= 0 || p.max > info.MaxSlots {
			p.max = info.MaxSlots
		}
		if p.min < 0 || p.min > p.max {
			return fmt.Errorf("pool %s: min-runners must be 0..%d", info.Name, p.max)
		}
		if p.gh, err = githubClient(info.GitHubURL); err != nil {
			return err
		}
		pools = append(pools, p)
	}
	if *del {
		for _, p := range pools {
			if err := deleteScaleSet(ctx, log, p.gh, p.name, *group); err != nil {
				return err
			}
		}
		return nil
	}
	if len(pools) == 1 {
		pools[0].gc = gc // a single-pool guard still answers on the unprefixed paths
	}

	for _, p := range pools {
		ss, err := ensureScaleSet(ctx, p.gh, p.name, *group, p.labels)
		if err != nil {
			return fmt.Errorf("pool %s: %w", p.info.Name, err)
		}
		p.gh.SetSystemInfo(systemInfo(ss.ID))
		var got []string
		for _, l := range ss.Labels {
			got = append(got, l.Name)
		}
		log.Info("scale set ready", "pool", p.info.Name, "name", ss.Name, "id", ss.ID, "labels", got, "min", p.min, "max", p.max)
		p.s = &scaler.Scaler{
			Log: log.With("pool", p.info.Name), Guard: p.gc, GitHub: p.gh, ScaleSetID: ss.ID, Info: p.info, Min: p.min, Max: p.max,
			TrustedRefs: trustedRefs(),
		}
	}

	if addr := env("DASHBOARD_ADDR", ""); addr != "" {
		token, err := secret("DASHBOARD_TOKEN")
		if err != nil {
			return err
		}
		var servers []*dashboard.Server
		for i, p := range pools {
			p.s.Dash = dashboard.NewTracker()
			d := &dashboard.Server{Name: p.name, Version: version, Info: p.info, Guard: p.gc, Tracker: p.s.Dash, Token: token}
			if i == 0 {
				d.Peers = dashboardPeers()
			}
			servers = append(servers, d)
		}
		dash := servers[0]
		dash.Locals = servers[1:]
		if uf := env("DASHBOARD_USERS_FILE", ""); uf != "" { // opt-in accounts: login for browsers, the token stays for machines
			accts, fresh, err := dashboard.OpenAccounts(uf)
			if err != nil {
				return fmt.Errorf("DASHBOARD_USERS_FILE: %w", err)
			}
			if fresh {
				log.Warn("dashboard accounts created with the default login admin:admin; it must change its password at first login", "file", uf)
			}
			// An admin password kept in a file (DASHBOARD_ADMIN_PASSWORD[_FILE]) is applied at every start, so it wins over
			// a change made in the page and a rotated secret takes effect on restart. Unset or empty: accounts as before.
			if pw, err := secret("DASHBOARD_ADMIN_PASSWORD"); err != nil {
				return fmt.Errorf("DASHBOARD_ADMIN_PASSWORD: %w", err)
			} else if pw != "" {
				changed, err := accts.SyncAdminPassword(pw)
				if err != nil {
					return fmt.Errorf("DASHBOARD_ADMIN_PASSWORD: %w", err)
				}
				log.Info("dashboard admin password is managed by DASHBOARD_ADMIN_PASSWORD", "updated", changed)
			}
			dash.Auth = dashboard.NewAuth(accts, log.WithGroup("auth"))
			// The audit trail lives next to the users file unless told otherwise; "off" turns it off.
			switch af := env("DASHBOARD_AUDIT_FILE", filepath.Join(filepath.Dir(uf), "audit.jsonl")); af {
			case "off":
				log.Info("dashboard audit log is off")
			default:
				max, err := parseSize(env("DASHBOARD_AUDIT_MAX", ""))
				if err != nil {
					return fmt.Errorf("DASHBOARD_AUDIT_MAX: %w", err)
				}
				dash.Auth.Audit = dashboard.NewAudit(af, max, log.WithGroup("audit"))
				log.Info("dashboard audit log", "file", af)
			}
		}
		if gh, err := githubAPI(); err != nil {
			return fmt.Errorf("job detail from GitHub: %w", err)
		} else if gh != nil {
			poll, err := time.ParseDuration(env("DASHBOARD_GITHUB_POLL", "20s"))
			if err != nil || poll < 5*time.Second {
				return fmt.Errorf("DASHBOARD_GITHUB_POLL: want a duration of at least 5s, got %q", env("DASHBOARD_GITHUB_POLL", ""))
			}
			stall, err := time.ParseDuration(env("DASHBOARD_STALL_AFTER", "10m"))
			if err != nil || stall <= 0 {
				return fmt.Errorf("DASHBOARD_STALL_AFTER: want a duration like 10m, got %q", env("DASHBOARD_STALL_AFTER", ""))
			}
			for _, d := range servers { // one poller per pool: each follows its own tracker
				en := &dashboard.Enricher{Src: gh, Tracker: d.Tracker, Every: poll, FailureLogs: env("DASHBOARD_FAILURE_LOGS", "off") == "on", Log: log.WithGroup("github")}
				d.GitHub, d.StallAfter = en, stall
				go en.Loop(ctx)
			}
			log.Info("job detail from GitHub", "poll", poll, "stall_after", stall, "failure_logs", env("DASHBOARD_FAILURE_LOGS", "off") == "on")
		}
		ds := &http.Server{Addr: addr, ReadHeaderTimeout: 10 * time.Second, Handler: dash.Handler()}
		go func() {
			if err := ds.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				log.Error("dashboard", "err", err)
			}
		}()
		go func() { <-ctx.Done(); ds.Close() }()
		log.Info("dashboard listening", "addr", addr, "token", token != "")
	}
	// Deliberately no scale set deletion on exit: queued jobs keep waiting for it while the scaler restarts.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, len(pools))
	for _, p := range pools {
		go func() { errc <- runPool(ctx, log, p.gh, p.s, p.info, p.max) }()
	}
	var first error
	for range pools {
		if err := <-errc; err != nil && first == nil && !errors.Is(err, context.Canceled) {
			first = err
		}
		cancel() // one pool stopping stops them all: the supervisor restarts the scaler as a whole
	}
	return first
}

// runPool opens the pool's message session and feeds its scaler until ctx ends.
func runPool(ctx context.Context, log *slog.Logger, gh *scaleset.Client, s *scaler.Scaler, info api.Info, maxR int) error {
	host, _ := os.Hostname()
	var session *scaleset.MessageSessionClient
	var err error
	for {
		session, err = gh.MessageSessionClient(ctx, s.ScaleSetID, info.Prefix+"-"+host)
		if err == nil {
			break
		}
		if !errors.Is(err, scaleset.ConflictError) {
			return err
		}
		// A previous scaler's session hasn't expired yet.
		log.Info("another session is open for this scale set; retrying in 30s", "pool", info.Name)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
	defer session.Close(context.WithoutCancel(ctx))
	s.Session = session
	l, err := listener.New(session, listener.Config{ScaleSetID: s.ScaleSetID, MaxRunners: maxR, Logger: log.With("pool", info.Name).WithGroup("listener")})
	if err != nil {
		return err
	}
	return l.Run(ctx, s)
}

// githubAPI builds the client behind the dashboard's job detail (steps, who triggered a run, failure logs). It
// prefers a dedicated read-only token (DASHBOARD_GITHUB_TOKEN[_FILE], e.g. a fine-grained token with Actions: read),
// then the GitHub App, then the personal access token. DASHBOARD_GITHUB=off turns it off. It returns nil when off.
func githubAPI() (*ghapi.Client, error) {
	if env("DASHBOARD_GITHUB", "auto") == "off" {
		return nil, nil
	}
	base := env("GITHUB_API_URL", "")
	if tok, err := secret("DASHBOARD_GITHUB_TOKEN"); err != nil {
		return nil, err
	} else if tok != "" {
		return ghapi.New(base, ghapi.TokenAuth(tok)), nil
	}
	key, err := secret("GITHUB_APP_PRIVATE_KEY")
	if err != nil {
		return nil, err
	}
	if id, _ := strconv.ParseInt(os.Getenv("GITHUB_APP_INSTALLATION_ID"), 10, 64); key != "" && os.Getenv("GITHUB_APP_CLIENT_ID") != "" && id != 0 {
		k, err := ghapi.ParseKey([]byte(key))
		if err != nil {
			return nil, err
		}
		return ghapi.New(base, &ghapi.AppAuth{Base: base, ClientID: os.Getenv("GITHUB_APP_CLIENT_ID"), InstallationID: id, Key: k}), nil
	}
	tok, err := secret("GITHUB_TOKEN")
	if err != nil {
		return nil, err
	}
	if tok == "" {
		return nil, nil
	}
	return ghapi.New(base, ghapi.TokenAuth(tok)), nil
}

// parseSize reads a byte count with an optional K, M or G suffix ("5M"). Empty means 0 (use the default).
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(strings.TrimSuffix(strings.TrimSpace(strings.ToUpper(s)), "B")))
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'K':
		mult, s = 1<<10, s[:len(s)-1]
	case 'M':
		mult, s = 1<<20, s[:len(s)-1]
	case 'G':
		mult, s = 1<<30, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("want a size like 5M, got %q", s)
	}
	return n * mult, nil
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func splitLabels(s, def string) []string {
	var out []string
	for _, l := range strings.Split(s, ",") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		out = []string{def}
	}
	return out
}

func systemInfo(id int) scaleset.SystemInfo {
	return scaleset.SystemInfo{System: "runner-scaling", Subsystem: "scaler", Version: version, CommitSHA: "NA", ScaleSetID: id}
}

// githubClient prefers a GitHub App (GITHUB_APP_CLIENT_ID, GITHUB_APP_INSTALLATION_ID, GITHUB_APP_PRIVATE_KEY[_FILE])
// and falls back to a personal access token (GITHUB_TOKEN[_FILE]).
func githubClient(url string) (*scaleset.Client, error) {
	key, err := secret("GITHUB_APP_PRIVATE_KEY")
	if err != nil {
		return nil, err
	}
	app := scaleset.GitHubAppAuth{ClientID: os.Getenv("GITHUB_APP_CLIENT_ID"), PrivateKey: key}
	app.InstallationID, _ = strconv.ParseInt(os.Getenv("GITHUB_APP_INSTALLATION_ID"), 10, 64)
	if app.Validate() == nil {
		return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{GitHubConfigURL: url, GitHubAppAuth: app, SystemInfo: systemInfo(0)})
	}
	tok, err := secret("GITHUB_TOKEN")
	if err != nil {
		return nil, err
	}
	if tok == "" {
		return nil, errors.New("no GitHub credentials: set GITHUB_APP_* or GITHUB_TOKEN (or their _FILE variants)")
	}
	return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{GitHubConfigURL: url, PersonalAccessToken: tok, SystemInfo: systemInfo(0)})
}

func runnerGroupID(ctx context.Context, gh *scaleset.Client, group string) (int, error) {
	if group == scaleset.DefaultRunnerGroup {
		return 1, nil
	}
	g, err := gh.GetRunnerGroupByName(ctx, group)
	if err != nil {
		return 0, fmt.Errorf("runner group %q: %w", group, err)
	}
	return g.ID, nil
}

func deleteScaleSet(ctx context.Context, log *slog.Logger, gh *scaleset.Client, name, group string) error {
	groupID, err := runnerGroupID(ctx, gh, group)
	if err != nil {
		return err
	}
	ss, err := gh.GetRunnerScaleSet(ctx, groupID, name)
	if err != nil {
		return err
	}
	if ss == nil {
		log.Info("no such scale set", "name", name)
		return nil
	}
	if err := gh.DeleteRunnerScaleSet(ctx, ss.ID); err != nil {
		return err
	}
	log.Info("scale set deleted", "name", name, "id", ss.ID)
	return nil
}

// ensureScaleSet finds the scale set by name or creates it, and keeps its labels in step with the flags.
func ensureScaleSet(ctx context.Context, gh *scaleset.Client, name, group string, labels []string) (*scaleset.RunnerScaleSet, error) {
	groupID, err := runnerGroupID(ctx, gh, group)
	if err != nil {
		return nil, err
	}
	want := make([]scaleset.Label, len(labels))
	for i, l := range labels {
		want[i] = scaleset.Label{Name: l}
	}
	spec := &scaleset.RunnerScaleSet{Name: name, RunnerGroupID: groupID, Labels: want, RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true}}
	ss, err := gh.GetRunnerScaleSet(ctx, groupID, name)
	if err != nil {
		return nil, err
	}
	if ss == nil {
		return gh.CreateRunnerScaleSet(ctx, spec)
	}
	if sameLabels(ss.Labels, want) {
		return ss, nil
	}
	if _, err := gh.UpdateRunnerScaleSet(ctx, ss.ID, spec); err != nil {
		return nil, fmt.Errorf("update labels: %w", err)
	}
	if got, err := gh.GetRunnerScaleSet(ctx, groupID, name); err == nil && got != nil && sameLabels(got.Labels, want) {
		return got, nil
	}
	// The service has been seen to accept the update without changing the labels. Recreate the scale set instead:
	// this only happens when the labels change, and runners already started for the old one finish their jobs.
	slog.Warn("scale set labels did not update; recreating it", "name", name, "id", ss.ID)
	if err := gh.DeleteRunnerScaleSet(ctx, ss.ID); err != nil {
		return nil, fmt.Errorf("recreate scale set: delete: %w", err)
	}
	return gh.CreateRunnerScaleSet(ctx, spec)
}

// sameLabels reports whether a and b name the same set of labels, ignoring order and case.
func sameLabels(a, b []scaleset.Label) bool {
	set := func(ls []scaleset.Label) map[string]bool {
		m := make(map[string]bool, len(ls))
		for _, l := range ls {
			m[strings.ToLower(l.Name)] = true
		}
		return m
	}
	sa, sb := set(a), set(b)
	if len(sa) != len(sb) {
		return false
	}
	for n := range sa {
		if !sb[n] {
			return false
		}
	}
	return true
}

// trustedRefs reads CACHE_TRUSTED_REFS: comma-separated git refs whose jobs may keep the slot's warm caches. Default
// refs/heads/main; "none" (or any list without a ref) makes the guard wipe caches after every job.
func trustedRefs() []string {
	v, ok := os.LookupEnv("CACHE_TRUSTED_REFS")
	if !ok {
		v = "refs/heads/main"
	}
	var out []string
	for _, r := range strings.Split(v, ",") {
		if r = strings.TrimSpace(r); r != "" && r != "none" {
			out = append(out, r)
		}
	}
	return out
}

// dashboardPeers parses DASHBOARD_PEERS, "name=http://host:port,name2=http://...": other scalers whose state this
// dashboard folds into its own page. They must use the same DASHBOARD_TOKEN.
func dashboardPeers() []dashboard.Peer {
	var out []dashboard.Peer
	for _, p := range strings.Split(env("DASHBOARD_PEERS", ""), ",") {
		if name, url, ok := strings.Cut(strings.TrimSpace(p), "="); ok && name != "" && url != "" {
			out = append(out, dashboard.Peer{Name: name, URL: url})
		}
	}
	return out
}

// runFirewall keeps the runner networks fenced off private address space: an nftables table of its own, read back
// every few seconds and re-applied if anything changed it, and a gate file the guards wait for.
func runFirewall(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("firewall", flag.ExitOnError)
	src := fs.String("src", env("FENCE_SRC", ""), "comma-separated IPv4 runner networks to fence (must contain every guard's subnet), e.g. 172.30.0.0/16")
	src6 := fs.String("src6", env("FENCE_SRC6", ""), "comma-separated IPv6 runner networks to fence (only if you enabled IPv6 on them)")
	also := fs.String("also-block", env("FENCE_ALSO_BLOCK", ""), "extra destination networks to block, IPv4 and IPv6, beyond the built-in private/reserved ranges")
	gate := fs.String("gate", env("GATE", "/gate/firewall-ok"), "file present only while the fence is verified in place (guards' require_file)")
	every := fs.Duration("interval", 3*time.Second, "how often the kernel's ruleset is read back")
	metrics := fs.String("metrics", env("FENCE_METRICS", ""), "serve Prometheus metrics on this address, e.g. 127.0.0.1:9102 (host network: pick a port you own)")
	remove := fs.Bool("remove", false, "delete the fence table and exit")
	check := fs.Bool("check", false, "verify the installed fence against these flags and exit 0 if it is in place")
	fs.Parse(args)

	if *remove {
		return fence.Remove()
	}
	spec := fence.Spec{}
	var err error
	if spec.Src4, err = prefixes(*src, true); err != nil {
		return err
	}
	if spec.Src6, err = prefixes(*src6, false); err != nil {
		return err
	}
	if extra, err := prefixes(*also, true); err != nil {
		return err
	} else if len(extra) > 0 {
		spec.Block4 = append(append(spec.Block4, fence.DefaultBlock4...), extra...)
	}
	if extra, err := prefixes(*also, false); err != nil {
		return err
	} else if len(extra) > 0 {
		spec.Block6 = append(append(spec.Block6, fence.DefaultBlock6...), extra...)
	}
	if err := spec.Normalize(); err != nil {
		return err
	}
	if *check {
		st, err := spec.Verify()
		if err != nil {
			return err
		}
		if !st.OK {
			return errors.New(st.Problem)
		}
		fmt.Println("fence in place")
		return nil
	}
	r := &fence.Runner{Spec: spec, Gate: *gate, Interval: *every, Log: log}
	if *metrics != "" {
		go func() {
			if err := r.Metrics(ctx, *metrics); err != nil {
				log.Error("metrics", "err", err)
			}
		}()
	}
	log.Info("firewall starting", "src", *src, "src6", *src6, "gate", *gate, "interval", every.String(), "version", version)
	return r.Run(ctx)
}

// prefixes parses a comma-separated list of networks, keeping only the IPv4 (v4=true) or IPv6 ones.
func prefixes(s string, v4 bool) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", f, err)
		}
		if p.Addr().Is4() == v4 {
			out = append(out, p)
		}
	}
	return out, nil
}
