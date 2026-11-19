// Command runners queries a running runner-scaling deployment: pools, slots, jobs and memory. It is read-only and
// talks only to the scaler's dashboard (GET /api/state and GET /metrics), so it needs the dashboard URL and token and
// nothing else: no Docker socket, no GitHub credential.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/MacLeodTech/runner-scaling/internal/api"
	"github.com/MacLeodTech/runner-scaling/internal/dashboard"
)

var version = "dev"

const usage = `runners: query a runner-scaling deployment (read-only)

usage: runners [flags] <command>

commands:
  status    pools at a glance: slots, queued and running jobs (default)
  slots     every slot with its resource use
  jobs      queued, running and recent jobs        (-n N limits recent, default 10)
  memory    memory used against the budget, per pool and per slot
  watch     status, refreshed until interrupted    (-i seconds, default 5)
  host      runner containers on THIS host with their runtime and privilege, and the firewall gate and fence checks
            (runs the docker CLI; needs docker access)
  completion bash|zsh   print a shell completion script
  version

flags:
  --url URL          dashboard base URL (env RUNNERS_URL, default http://localhost:8787)
  --token-file FILE  file holding the dashboard token (env RUNNERS_TOKEN_FILE); or set RUNNERS_TOKEN;
                     default ~/.config/runners/token if it exists
  --json             machine-readable output where the command has one
  --gate-file FILE   host: the guards' require_file; reported MISSING when absent (env RUNNERS_GATE_FILE)
  --fence CIDR       host: the firewall's FENCE_SRC; a container outside it is reported UNFENCED (env RUNNERS_FENCE)
`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "runners:", err)
		os.Exit(1)
	}
}

type opts struct {
	url, tokenFile string
	gateFile       string
	fence          string
	json           bool
	n              int
	interval       int
}

func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("runners", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o opts
	fs.StringVar(&o.url, "url", env("RUNNERS_URL", "http://localhost:8787"), "")
	fs.StringVar(&o.tokenFile, "token-file", os.Getenv("RUNNERS_TOKEN_FILE"), "")
	fs.StringVar(&o.gateFile, "gate-file", os.Getenv("RUNNERS_GATE_FILE"), "")
	fs.StringVar(&o.fence, "fence", os.Getenv("RUNNERS_FENCE"), "")
	fs.BoolVar(&o.json, "json", false, "")
	fs.IntVar(&o.n, "n", 10, "")
	fs.IntVar(&o.interval, "i", 5, "")
	// flags may come before or after the command
	var rest []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprint(out, usage)
				return nil
			}
			return fmt.Errorf("%w\n\n%s", err, usage)
		}
		args = fs.Args()
		if len(args) > 0 {
			rest = append(rest, args[0])
			args = args[1:]
		}
	}
	cmd := "status"
	if len(rest) > 0 {
		cmd = rest[0]
	}
	if cmd == "completion" {
		if len(rest) != 2 {
			return fmt.Errorf("usage: runners completion bash|zsh")
		}
		return printCompletion(out, rest[1])
	}
	if len(rest) > 1 {
		return fmt.Errorf("unexpected argument %q\n\n%s", rest[1], usage)
	}
	if cmd == "version" {
		fmt.Fprintln(out, "runners", version)
		return nil
	}
	if cmd == "help" {
		fmt.Fprint(out, usage)
		return nil
	}
	c, err := newClient(o)
	if err != nil {
		return err
	}
	switch cmd {
	case "status":
		st, err := c.state()
		if err != nil {
			return err
		}
		if o.json {
			return writeJSON(out, st)
		}
		printStatus(out, st)
	case "slots":
		st, err := c.state()
		if err != nil {
			return err
		}
		if o.json {
			return writeJSON(out, pools(st))
		}
		printSlots(out, st)
	case "jobs":
		st, err := c.state()
		if err != nil {
			return err
		}
		if o.json {
			return writeJSON(out, st)
		}
		printJobs(out, st, o.n)
	case "memory":
		m, err := c.metrics()
		if err != nil {
			return err
		}
		st, err := c.state()
		if err != nil {
			return err
		}
		if o.json {
			return writeJSON(out, map[string]any{"budget_bytes": m["runner_scaling_memory_budget_bytes"], "used_bytes": m["runner_scaling_memory_used_bytes"], "pools": pools(st)})
		}
		printMemory(out, st, m)
	case "host":
		return printHost(out, o)
	case "watch":
		for {
			st, err := c.state()
			if err != nil {
				return err
			}
			fmt.Fprint(out, "\033[H\033[2J")
			printStatus(out, st)
			time.Sleep(time.Duration(max(o.interval, 1)) * time.Second)
		}
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
	return nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func writeJSON(w io.Writer, v any) error {
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	return e.Encode(v)
}

// ---- client ----

type client struct {
	base  *url.URL
	token string
	http  *http.Client
}

func newClient(o opts) (*client, error) {
	u, err := url.Parse(strings.TrimRight(o.url, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("bad --url %q (want e.g. http://localhost:8787)", o.url)
	}
	tok := strings.TrimSpace(os.Getenv("RUNNERS_TOKEN"))
	if o.tokenFile == "" && tok == "" {
		if dir, err := os.UserConfigDir(); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "runners", "token")); err == nil {
				o.tokenFile = filepath.Join(dir, "runners", "token")
			}
		}
	}
	if o.tokenFile != "" {
		b, err := os.ReadFile(o.tokenFile)
		if err != nil {
			return nil, fmt.Errorf("token file: %w", err)
		}
		if w := tokenFileWarning(o.tokenFile); w != "" {
			fmt.Fprintln(warnOut, "runners: warning:", w)
		}
		tok = strings.TrimSpace(string(b))
	}
	return &client{base: u, token: tok, http: &http.Client{Timeout: 10 * time.Second}}, nil
}

// warnOut receives warnings; a variable so tests can capture it.
var warnOut io.Writer = os.Stderr

// tokenFileWarning returns a message when the token file can be read or written by anyone but its owner.
func tokenFileWarning(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if m := fi.Mode().Perm(); m&0o077 != 0 {
		return fmt.Sprintf("token file %s is mode %04o, readable by other users; run: chmod 600 %s", path, m, path)
	}
	return ""
}

func (c *client) get(path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, c.base.String()+path, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token) // header, never the URL: tokens in URLs end up in logs
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the dashboard at %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("dashboard refused the token (HTTP %d): set RUNNERS_TOKEN or --token-file", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("dashboard answered HTTP %d for %s", resp.StatusCode, path)
	}
	return b, nil
}

func (c *client) state() (dashboard.State, error) {
	var st dashboard.State
	b, err := c.get("/api/state")
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

// metrics returns the unlabelled and summed samples of GET /metrics, keyed by metric name.
func (c *client) metrics() (map[string]float64, error) {
	b, err := c.get("/metrics")
	if err != nil {
		return nil, err
	}
	return parseMetrics(string(b)), nil
}

func parseMetrics(text string) map[string]float64 {
	m := map[string]float64{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || l[0] == '#' {
			continue
		}
		i := strings.LastIndexByte(l, ' ')
		if i < 0 {
			continue
		}
		v, err := strconv.ParseFloat(l[i+1:], 64)
		if err != nil {
			continue
		}
		name := l[:i]
		if j := strings.IndexByte(name, '{'); j >= 0 {
			name = name[:j]
		}
		m[name] += v
	}
	return m
}

// ---- rendering ----

// pools flattens the state and its peers into one list, the state itself first.
func pools(st dashboard.State) []dashboard.State {
	own := st
	own.Peers = nil
	return append([]dashboard.State{own}, st.Peers...)
}

func printStatus(w io.Writer, st dashboard.State) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "POOL\tSLOTS\tQUEUED\tRUNNING\tNOTE")
	for _, p := range pools(st) {
		used := 0
		for _, s := range p.Slots {
			if s.RunnerName != "" || s.Starting {
				used++
			}
		}
		note := ""
		switch {
		case p.PeerErr != "":
			note = "unreachable: " + p.PeerErr
		case p.GuardErr != "":
			note = "guard: " + p.GuardErr
		}
		fmt.Fprintf(tw, "%s\t%d/%d\t%d\t%d\t%s\n", p.Name, used, p.MaxSlots, len(p.Queued), len(p.Running), note)
	}
	tw.Flush()
	fmt.Fprintf(w, "\n%s %s, as of %s\n", st.Name, st.Version, st.Now.Local().Format("15:04:05"))
}

func printSlots(w io.Writer, st dashboard.State) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "POOL\tSLOT\tRUNNER\tSTATE\tMEM\tCPU\tPIDS")
	for _, p := range pools(st) {
		for _, s := range p.Slots {
			state := orDash(s.RunnerState)
			if s.Starting {
				state = "starting"
			}
			mem, cpu, pids := "-", "-", "-"
			if s.Stats != nil {
				mem, cpu, pids = human(s.Stats.MemBytes), fmt.Sprintf("%.0f%%", s.Stats.CPUPercent), strconv.FormatInt(s.Stats.PIDs, 10)
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\n", p.Name, s.Slot, orDash(s.RunnerName), state, mem, cpu, pids)
		}
	}
	tw.Flush()
}

func printJobs(w io.Writer, st dashboard.State, n int) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "POOL\tSTATE\tREPO\tJOB\tREF\tAGE")
	now := st.Now
	row := func(pool string, j dashboard.Job) {
		t := j.Queued
		if !j.Started.IsZero() {
			t = j.Started
		}
		if !j.Finished.IsZero() {
			t = j.Finished
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", pool, j.State, j.Repo, j.Name, j.Ref, age(now, t))
	}
	for _, p := range pools(st) {
		for _, j := range p.Queued {
			row(p.Name, j)
		}
		for _, j := range p.Running {
			row(p.Name, j)
		}
		for i, j := range p.Recent {
			if i >= n {
				break
			}
			row(p.Name, j)
		}
	}
	tw.Flush()
}

func printMemory(w io.Writer, st dashboard.State, m map[string]float64) {
	if b := m["runner_scaling_memory_budget_bytes"]; b > 0 {
		u := m["runner_scaling_memory_used_bytes"]
		fmt.Fprintf(w, "budget %s, used %s (%.0f%%)\n\n", human(int64(b)), human(int64(u)), 100*u/b)
	} else {
		fmt.Fprintln(w, "no memory budget set")
		fmt.Fprintln(w)
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "POOL\tSLOT\tMEM")
	for _, p := range pools(st) {
		slots := append([]api.Slot(nil), p.Slots...)
		sort.Slice(slots, func(i, j int) bool { return slots[i].Slot < slots[j].Slot })
		for _, s := range slots {
			if s.Stats != nil {
				fmt.Fprintf(tw, "%s\t%d\t%s\n", p.Name, s.Slot, human(s.Stats.MemBytes))
			}
		}
	}
	tw.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func age(now, t time.Time) string {
	if t.IsZero() || now.IsZero() {
		return "-"
	}
	d := now.Sub(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func human(b int64) string {
	const k = 1024
	if b < k {
		return fmt.Sprintf("%dB", b)
	}
	f, u := float64(b), "KMGT"
	i := -1
	for f >= k && i < len(u)-1 {
		f /= k
		i++
	}
	return fmt.Sprintf("%.1f%ciB", f, u[i])
}

// hostRow is one runner-scaling container as Docker reports it.
type hostRow struct {
	Name       string   `json:"name"`
	Runtime    string   `json:"runtime"`
	Privileged bool     `json:"privileged"`
	State      string   `json:"state"`
	Image      string   `json:"image"`
	Addresses  []string `json:"addresses,omitempty"`
	Unfenced   bool     `json:"unfenced,omitempty"`
}

// hostReport is the JSON form of the host command.
type hostReport struct {
	Gate       string    `json:"gate,omitempty"` // "present", "MISSING", or empty when no gate file was given
	Containers []hostRow `json:"containers"`
}

// gateStatus reports whether the firewall gate file exists. Empty path: not checked.
func gateStatus(path string) string {
	if path == "" {
		return ""
	}
	if _, err := os.Stat(path); err != nil {
		return "MISSING"
	}
	return "present"
}

// outsideFence reports whether a running container has an address outside the fenced range. A container with no
// address (stopped) is not flagged.
func outsideFence(fence *net.IPNet, addrs []string) bool {
	if fence == nil {
		return false
	}
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && !fence.Contains(ip) {
			return true
		}
	}
	return false
}

// printHost lists the containers the guards started (found by their label) and shows how each one is isolated. It runs
// the docker CLI, so unlike the other commands it needs access to Docker on this host. With --gate-file and --fence
// it also checks that the firewall gate exists and that every container sits inside the fenced range.
func printHost(w io.Writer, o opts) error {
	var fence *net.IPNet
	if o.fence != "" {
		_, f, err := net.ParseCIDR(o.fence)
		if err != nil {
			return fmt.Errorf("bad --fence %q: %w", o.fence, err)
		}
		fence = f
	}
	ids, err := exec.Command("docker", "ps", "-aq", "--filter", "label=io.github.runner-scaling.instance").Output()
	if err != nil {
		return fmt.Errorf("docker ps: %w (this command needs Docker access on this host)", err)
	}
	rep := hostReport{Gate: gateStatus(o.gateFile), Containers: []hostRow{}}
	if f := strings.Fields(string(ids)); len(f) > 0 {
		out, err := exec.Command("docker", append([]string{"inspect"}, f...)...).Output()
		if err != nil {
			return fmt.Errorf("docker inspect: %w", err)
		}
		var raw []struct {
			Name       string
			State      struct{ Status string }
			Config     struct{ Image string }
			HostConfig struct {
				Runtime    string
				Privileged bool
			}
			NetworkSettings struct {
				Networks map[string]struct{ IPAddress string }
			}
		}
		if err := json.Unmarshal(out, &raw); err != nil {
			return err
		}
		for _, r := range raw {
			rt := r.HostConfig.Runtime
			if rt == "" {
				rt = "runc"
			}
			var addrs []string
			for _, n := range r.NetworkSettings.Networks {
				if n.IPAddress != "" {
					addrs = append(addrs, n.IPAddress)
				}
			}
			sort.Strings(addrs)
			rep.Containers = append(rep.Containers, hostRow{strings.TrimPrefix(r.Name, "/"), rt, r.HostConfig.Privileged,
				r.State.Status, r.Config.Image, addrs, outsideFence(fence, addrs)})
		}
		sort.Slice(rep.Containers, func(i, j int) bool { return rep.Containers[i].Name < rep.Containers[j].Name })
	}
	if o.json {
		return writeJSON(w, rep)
	}
	return renderHost(w, rep, fence != nil)
}

func renderHost(w io.Writer, rep hostReport, fenceChecked bool) error {
	if rep.Gate != "" {
		fmt.Fprintf(w, "firewall gate: %s\n\n", rep.Gate)
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	hdr := "CONTAINER\tSTATE\tRUNTIME\tPRIVILEGED"
	if fenceChecked {
		hdr += "\tFENCE"
	}
	fmt.Fprintln(tw, hdr)
	for _, r := range rep.Containers {
		warn := ""
		if r.Privileged {
			warn = "  <- root on the host if a job starts a container against it"
		}
		fence := ""
		if fenceChecked {
			fence = "\tinside"
			if r.Unfenced {
				fence = "\tUNFENCED"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%v%s%s\n", r.Name, r.State, r.Runtime, r.Privileged, fence, warn)
	}
	return tw.Flush()
}

const bashCompletion = `_runners() {
  local cur prev
  cur="${COMP_WORDS[COMP_CWORD]}"
  prev="${COMP_WORDS[COMP_CWORD-1]}"
  case "$prev" in
    --token-file|--gate-file) COMPREPLY=($(compgen -f -- "$cur")); return ;;
    completion) COMPREPLY=($(compgen -W "bash zsh" -- "$cur")); return ;;
  esac
  COMPREPLY=($(compgen -W "status slots jobs memory watch host completion version help --url --token-file --json --gate-file --fence -n -i" -- "$cur"))
}
complete -F _runners runners
`

const zshCompletion = `#compdef runners
_runners() {
  local -a cmds
  cmds=(
    'status:pools at a glance'
    'slots:every slot with its resource use'
    'jobs:queued, running and recent jobs'
    'memory:memory used against the budget'
    'watch:status, refreshed until interrupted'
    'host:runner containers on this host'
    'completion:print a shell completion script'
    'version:print the version'
  )
  _arguments \
    '--url[dashboard base URL]:url' \
    '--token-file[file holding the dashboard token]:file:_files' \
    '--json[machine-readable output]' \
    '--gate-file[firewall gate file (host)]:file:_files' \
    '--fence[fenced CIDR (host)]:cidr' \
    '-n[recent jobs to show]:count' \
    '-i[watch interval in seconds]:seconds' \
    '1:command:->cmd' \
    '2:shell:(bash zsh)'
  [[ $state == cmd ]] && _describe command cmds
}
_runners "$@"
`

func printCompletion(w io.Writer, shell string) error {
	switch shell {
	case "bash":
		_, err := io.WriteString(w, bashCompletion)
		return err
	case "zsh":
		_, err := io.WriteString(w, zshCompletion)
		return err
	}
	return fmt.Errorf("unknown shell %q (want bash or zsh)", shell)
}
