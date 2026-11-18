// Command runner-scaling runs ephemeral, autoscaled GitHub Actions runners on one Docker host without giving the
// internet-facing part of the system the Docker socket.
//
//	runner-scaling guard  --config guard.yaml   holds the Docker socket; builds containers only from its config
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
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

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
	fmt.Fprintln(os.Stderr, "usage: runner-scaling guard|scaler|health|version [flags]")
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

	cfg, err := guard.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if *check {
		fmt.Printf("%s: ok (prefix %s, %d slots, tenant pinned: %v)\n", *cfgPath, cfg.Prefix, cfg.MaxSlots, cfg.TenantURL != "")
		return nil
	}
	g := guard.New(cfg, guard.NewSocketDocker(*dockerSock), log)

	pctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	err = g.EnsureImages(pctx)
	cancel()
	if err != nil {
		return err
	}
	go g.ReapLoop(ctx)

	_ = os.Remove(*socket)
	ln, err := net.Listen("unix", *socket)
	if err != nil {
		return err
	}
	m, err := strconv.ParseUint(*mode, 8, 32)
	if err != nil {
		return fmt.Errorf("socket-mode: %w", err)
	}
	if *gid >= 0 {
		if err := os.Chown(*socket, -1, *gid); err != nil {
			return fmt.Errorf("chown socket: %w", err)
		}
	}
	if err := os.Chmod(*socket, os.FileMode(m)); err != nil {
		return err
	}
	srv := &http.Server{Handler: g.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); srv.Shutdown(context.Background()) }()
	log.Info("guard listening", "socket", *socket, "prefix", cfg.Prefix, "max_slots", cfg.MaxSlots, "version", version)
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func runHealth(args []string) error {
	fs := flag.NewFlagSet("health", flag.ExitOnError)
	socket := fs.String("socket", env("GUARD_SOCKET", defaultSocket), "guard API socket")
	fs.Parse(args)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := scaler.NewGuardClient(*socket).Info(ctx)
	return err
}

func runScaler(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("scaler", flag.ExitOnError)
	socket := fs.String("socket", env("GUARD_SOCKET", defaultSocket), "guard API socket")
	name := fs.String("name", env("SCALE_SET_NAME", ""), "scale set name (default: the guard's prefix)")
	labels := fs.String("labels", env("SCALE_SET_LABELS", ""), "comma-separated labels workflows target (default: the name)")
	group := fs.String("runner-group", env("RUNNER_GROUP", scaleset.DefaultRunnerGroup), "runner group")
	minR := fs.Int("min-runners", atoi(env("MIN_RUNNERS", "0")), "runners kept warm")
	maxR := fs.Int("max-runners", atoi(env("MAX_RUNNERS", "0")), "runner limit (default and ceiling: the guard's max_slots)")
	del := fs.Bool("delete-scale-set", false, "delete the scale set from GitHub and exit (stop the guard's runners first)")
	fs.Parse(args)

	gc := scaler.NewGuardClient(*socket)
	var info, err = gc.Info(ctx)
	for err != nil { // the guard may still be pulling images
		log.Info("waiting for guard", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
		info, err = gc.Info(ctx)
	}
	if *name == "" {
		*name = info.Prefix
	}
	if *maxR <= 0 || *maxR > info.MaxSlots {
		*maxR = info.MaxSlots
	}
	if *minR < 0 || *minR > *maxR {
		return fmt.Errorf("min-runners must be 0..%d", *maxR)
	}

	gh, err := githubClient(info.GitHubURL)
	if err != nil {
		return err
	}
	if *del {
		return deleteScaleSet(ctx, log, gh, *name, *group)
	}
	ss, err := ensureScaleSet(ctx, gh, *name, *group, splitLabels(*labels, *name))
	if err != nil {
		return err
	}
	gh.SetSystemInfo(systemInfo(ss.ID))
	var got []string
	for _, l := range ss.Labels {
		got = append(got, l.Name)
	}
	log.Info("scale set ready", "name", ss.Name, "id", ss.ID, "labels", got, "min", *minR, "max", *maxR)

	host, _ := os.Hostname()
	var session *scaleset.MessageSessionClient
	for {
		session, err = gh.MessageSessionClient(ctx, ss.ID, info.Prefix+"-"+host)
		if err == nil {
			break
		}
		if !errors.Is(err, scaleset.ConflictError) {
			return err
		}
		// A previous scaler's session hasn't expired yet.
		log.Info("another session is open for this scale set; retrying in 30s")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
	defer session.Close(context.WithoutCancel(ctx))

	l, err := listener.New(session, listener.Config{ScaleSetID: ss.ID, MaxRunners: *maxR, Logger: log.WithGroup("listener")})
	if err != nil {
		return err
	}
	s := &scaler.Scaler{
		Log: log, Guard: gc, GitHub: gh, Session: session, ScaleSetID: ss.ID, Info: info, Min: *minR, Max: *maxR,
	}
	// Deliberately no scale set deletion on exit: queued jobs keep waiting for it while the scaler restarts.
	return l.Run(ctx, s)
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

func sameLabels(a, b []scaleset.Label) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, l := range a {
		seen[strings.ToLower(l.Name)] = true
	}
	for _, l := range b {
		if !seen[strings.ToLower(l.Name)] {
			return false
		}
	}
	return true
}
