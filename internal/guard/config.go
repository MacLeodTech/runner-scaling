package guard

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/MacLeodTech/runner-scaling/internal/jit"
)

// Config is the guard's whole world: everything about the containers it creates comes from here, never from a
// request. It is meant to be a read-only file owned by the host's administrator.
//
// Strings may contain {slot}, replaced by the slot number (1..max_slots), and {prefix}.
type Config struct {
	// GitHubURL is the org or repo URL the scale set is registered at. The guard tells the scaler; it can't check it
	// against a JIT config, which doesn't carry it. TenantURL is what binds configs to the org.
	GitHubURL string `yaml:"github_url"`
	// TenantURL is the org's Actions service URL (https://pipelines….actions.githubusercontent.com/<tenant>/). Every
	// JIT config must point under it. Leave it empty on first start: the guard then refuses configs and logs the
	// tenant URL the first one named, for you to check and pin.
	TenantURL string `yaml:"tenant_url"`
	// SharedHosts may appear in JIT configs with any path. Defaults to broker.actions.githubusercontent.com.
	SharedHosts []string `yaml:"shared_hosts"`
	// Prefix names everything the guard creates (containers, networks, volumes) and is the start of every runner
	// name. Lowercase letters, digits and dashes.
	Prefix string `yaml:"prefix"`
	// MaxSlots is the most runners that may exist at once.
	MaxSlots int `yaml:"max_slots"`
	// Subnet is the per-slot network, e.g. 172.41.{slot}.0/24. Empty lets Docker pick.
	Subnet string `yaml:"subnet"`
	// WorkDir is where the runner keeps job workspaces. It is a fresh Docker volume per runner, mounted at the same
	// path in the runner and its dind sidecar so that bind mounts from container jobs resolve. Default /runner/work.
	// With dind it must not be under /tmp: docker:dind mounts a tmpfs over /tmp, hiding the shared directory.
	WorkDir string `yaml:"work_dir"`
	// WorkBind, if set, is a host directory ({slot} required) used for work_dir instead of a Docker volume, for when
	// Docker's own disk is small. The guard empties it before each runner starts and again when the slot is torn
	// down: through the slot's dind, or without dind in a one-shot container (runner image, no network).
	WorkBind string `yaml:"work_bind"`
	// CacheDirs are host directories ({slot} required) holding warm caches that consecutive jobs on a slot share
	// (toolchain, package and image layer caches). A job can plant code in them that a later job runs, so the guard
	// empties them whenever a slot is torn down, unless the scaler has vouched for the job that ran there
	// (POST /v1/slots/{n}/keep-cache). They are also emptied at startup and before a slot whose last wipe failed
	// is reused.
	CacheDirs []string `yaml:"cache_dirs"`
	// CgroupParent puts every container the guard creates under one cgroup, so one memory limit can cover all
	// runners together (several guards may share it). With Docker's systemd cgroup driver this is a slice, e.g.
	// runner-scaling.slice; set its limit on the host: systemctl set-property runner-scaling.slice MemoryMax=8G.
	// Optional: memory_budget below needs no host setup.
	CgroupParent string `yaml:"cgroup_parent"`
	// MemoryBudget, if set (e.g. 8G), caps the total memory working set of all runner-scaling containers on the host,
	// across every pool: give every guard on the host the same value. The guard refuses new slots that would not fit
	// and reaps its newest slot if measured use stays over budget. Needs no host or systemd setup.
	MemoryBudget string `yaml:"memory_budget"`
	// MemoryReserve is the assumed minimum footprint of one slot (runner and dind together) for admission, so slots
	// that have just started and not ramped up are not counted as nearly empty. Default 1G with memory_budget.
	MemoryReserve string `yaml:"memory_reserve"`
	// MemoryRamp is how long after a container is created its slot counts at least MemoryRampShare of the
	// container's memory limit, instead of the (smaller) measured use, when admitting new slots. A job that has just
	// started looks light and then grows, so without this a burst of jobs is admitted against memory they have yet to
	// use. Default 2m with memory_budget; 0 turns it off. Measured use takes over once the ramp is over, so settled
	// light jobs pack tightly.
	MemoryRamp *Duration `yaml:"memory_ramp"`
	// MemoryRampShare is the fraction of a young container's memory limit to assume, 0 < share <= 1. Default 0.5.
	MemoryRampShare float64 `yaml:"memory_ramp_share"`
	// ModuleBlocklist lists kernel modules the host must not autoload on a job's behalf. A container with CAP_NET_ADMIN
	// (which any job with a dind can start) that creates a link of a type the host hasn't loaded, e.g. `ip link add type
	// ifb`, makes the kernel load that module on the HOST, with plain runc and under Sysbox alike. At startup the guard
	// writes `install <module> /bin/false` for each entry to /etc/modprobe.d/50-runner-scaling-<prefix>.conf on the host,
	// from a one-shot container with no network, so the autoload fails instead. Only name modules the host never uses.
	// Omitted: the host is not touched. An explicit empty list (`module_blocklist: []`) removes the guard's own file (it
	// never touches files it did not write).
	ModuleBlocklist []string `yaml:"module_blocklist"`
	// RequireFile, if set, must exist before the guard starts any slot: a gate for something that must happen first,
	// such as a host firewall that fences the slot subnets. Put it on a tmpfs so it is gone after a reboot.
	RequireFile string `yaml:"require_file"`
	// StartTimeout bounds how long a slot may take to come up (dind health included).
	StartTimeout Duration `yaml:"start_timeout"`
	// MaxRuntime, if set, is how long a runner container may run before the reaper removes it (0 = no limit).
	MaxRuntime Duration `yaml:"max_runtime"`
	// ReapInterval is how often exited runners are cleaned up.
	ReapInterval Duration `yaml:"reap_interval"`

	memBudget, memReserve int64 // parsed MemoryBudget and MemoryReserve
	memRamp               time.Duration
	memRampShare          float64

	Runner Template  `yaml:"runner"`
	Dind   *Template `yaml:"dind"` // optional: omit for runners without a private Docker daemon
}

// Template describes one container.
type Template struct {
	Image      string            `yaml:"image"`
	User       string            `yaml:"user"`
	Command    []string          `yaml:"command"` // runner only: overrides the image's entrypoint (see defaults)
	Env        map[string]string `yaml:"env"`
	Binds      []string          `yaml:"binds"` // host:container[:opts], {slot} allowed in the host part
	Privileged bool              `yaml:"privileged"`
	// Runtime, if set, is the name of a container runtime registered with the Docker daemon (its `runtimes` setting),
	// for example sysbox-runc (lets dind run without privileged) or runsc (gVisor). Empty means the daemon's default.
	Runtime     string   `yaml:"runtime"`
	CapDrop     []string `yaml:"cap_drop"`
	CapAdd      []string `yaml:"cap_add"`
	SecurityOpt []string `yaml:"security_opt"`
	DNS         []string `yaml:"dns"`
	Memory      string   `yaml:"memory"` // e.g. 3G; a hard limit: a container that passes it is OOM-killed
	// MemoryHigh, if set (e.g. 2560M), is a soft limit below Memory (cgroup v2 memory.high): a container that passes it
	// is slowed down by the kernel's reclaim instead of being killed. Docker has no setting for this, so the guard
	// writes it after the container has started, from a one-shot container that mounts the host's /sys/fs/cgroup for
	// that moment only. Optional; if the write fails the job runs with the hard limit alone.
	MemoryHigh string            `yaml:"memory_high"`
	CPUs       float64           `yaml:"cpus"`
	CPUShares  int64             `yaml:"cpu_shares"` // relative CPU weight against other host workloads (Docker default 1024), 2..262144
	Pids       int64             `yaml:"pids"`
	Health     *Health           `yaml:"healthcheck"`
	Log        map[string]string `yaml:"log_opts"` // json-file options
	Labels     map[string]string `yaml:"labels"`   // extra container labels; the guard's own io.github.runner-scaling.* labels win
	// PruneOnStop runs these commands in the container before it is removed (dind only), e.g. to trim build cache.
	PruneOnStop [][]string `yaml:"prune_on_stop"`
}

type Health struct {
	Test        []string `yaml:"test"`
	Interval    Duration `yaml:"interval"`
	Timeout     Duration `yaml:"timeout"`
	Retries     int      `yaml:"retries"`
	StartPeriod Duration `yaml:"start_period"`
}

// Duration parses "30s"-style strings in YAML.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

var runtimeRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// moduleRE is what a kernel module name may look like; it also keeps shell and config syntax out of the host file.
var moduleRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

var prefixRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${NAME} with the guard's environment variable NAME. Only the braced form is expanded, so shell
// snippets in commands ("$TMPDIR", "$@") pass through untouched. An unset variable is an error.
func expandEnv(b []byte) ([]byte, error) {
	var missing []string
	out := envRef.ReplaceAllFunc(b, func(m []byte) []byte {
		name := string(envRef.FindSubmatch(m)[1])
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
		}
		return []byte(v)
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("unset environment variables: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// LoadConfig reads and validates a config file. ${NAME} is replaced from the environment. Unknown keys are errors.
func LoadConfig(file string) (*Config, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	raw, err = expandEnv(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	for _, n := range c.Notices() {
		fmt.Fprintf(os.Stderr, "notice: %s\n", n)
	}
	return &c, nil
}

// Validate fills defaults and checks the config. The guard refuses to start on a bad config.
func (c *Config) Validate() error {
	if c.WorkDir == "" {
		c.WorkDir = "/runner/work"
	}
	if c.StartTimeout.Duration == 0 {
		c.StartTimeout.Duration = 2 * time.Minute
	}
	if c.ReapInterval.Duration == 0 {
		c.ReapInterval.Duration = 5 * time.Second
	}
	if len(c.SharedHosts) == 0 {
		c.SharedHosts = jit.DefaultSharedHosts
	}
	if len(c.Runner.Command) == 0 {
		// The official runner image. The JIT config arrives in ACTIONS_RUNNER_INPUT_JITCONFIG, which the runner
		// reads like --jitconfig, so it is never on the command line.
		c.Runner.Command = []string{"/home/runner/run.sh"}
	}

	var errs []error
	u := strings.TrimRight(c.GitHubURL, "/")
	if !strings.HasPrefix(u, "https://") || strings.Count(strings.TrimPrefix(u, "https://"), "/") < 1 {
		errs = append(errs, errors.New("github_url must be https://<host>/<org>[/<repo>]"))
	}
	if c.TenantURL != "" && (!strings.HasPrefix(c.TenantURL, "https://") || strings.Count(strings.Trim(strings.TrimPrefix(c.TenantURL, "https://"), "/"), "/") < 1) {
		errs = append(errs, errors.New("tenant_url must be https://<host>/<tenant>/"))
	}
	if !prefixRE.MatchString(c.Prefix) {
		errs = append(errs, errors.New("prefix must be 1-31 lowercase letters, digits or dashes"))
	}
	if c.MaxSlots < 1 || c.MaxSlots > 250 {
		errs = append(errs, errors.New("max_slots must be 1..250"))
	}
	if !path.IsAbs(c.WorkDir) || path.Clean(c.WorkDir) != c.WorkDir || c.WorkDir == "/" {
		errs = append(errs, errors.New("work_dir must be a clean absolute path other than /"))
	}
	if c.Dind != nil && (c.WorkDir == "/tmp" || strings.HasPrefix(c.WorkDir, "/tmp/")) {
		// docker:dind's entrypoint mounts a tmpfs over /tmp, which would hide the shared work dir from the daemon,
		// so bind mounts from container jobs would silently point at an empty directory.
		errs = append(errs, errors.New("work_dir must not be under /tmp when dind is used (dind mounts a tmpfs over /tmp)"))
	}
	if c.WorkBind != "" {
		if h := c.Expand(c.WorkBind, 1); !path.IsAbs(h) || path.Clean(h) == "/" || strings.Contains(h, ":") || !strings.Contains(c.WorkBind, "{slot}") {
			errs = append(errs, errors.New("work_bind must be an absolute host path containing {slot}"))
		}
	}
	for _, d := range c.CacheDirs {
		if h := c.Expand(d, 1); !path.IsAbs(h) || path.Clean(h) != h || h == "/" || strings.Contains(h, ":") || !strings.Contains(d, "{slot}") {
			errs = append(errs, fmt.Errorf("cache_dirs entry %q must be a clean absolute host path containing {slot}", d))
		}
	}
	if c.MaxRuntime.Duration < 0 || (c.MaxRuntime.Duration > 0 && c.MaxRuntime.Duration < time.Minute) {
		errs = append(errs, errors.New("max_runtime must be 0 (disabled) or at least 1m"))
	}
	c.memBudget, c.memReserve, c.memRamp, c.memRampShare = 0, 0, 0, 0
	if c.MemoryBudget != "" {
		b, err := ParseBytes(c.MemoryBudget)
		if err != nil {
			errs = append(errs, fmt.Errorf("memory_budget: %w", err))
		}
		r := int64(1 << 30)
		if c.MemoryReserve != "" {
			if r, err = ParseBytes(c.MemoryReserve); err != nil {
				errs = append(errs, fmt.Errorf("memory_reserve: %w", err))
			}
		}
		if b > 0 && r > b {
			errs = append(errs, errors.New("memory_reserve must not exceed memory_budget"))
		}
		c.memBudget, c.memReserve = b, r
		c.memRamp, c.memRampShare = 2*time.Minute, 0.5
		if c.MemoryRamp != nil {
			c.memRamp = c.MemoryRamp.Duration
		}
		if c.MemoryRampShare != 0 {
			c.memRampShare = c.MemoryRampShare
		}
		if c.memRamp < 0 || c.memRamp > time.Hour {
			errs = append(errs, errors.New("memory_ramp must be between 0 (off) and 1h"))
		}
		if c.memRampShare <= 0 || c.memRampShare > 1 {
			errs = append(errs, errors.New("memory_ramp_share must be above 0 and at most 1"))
		}
		if b > 0 {
			errs = append(errs, c.budgetFit(b)...)
		}
	} else if c.MemoryReserve != "" || c.MemoryRamp != nil || c.MemoryRampShare != 0 {
		errs = append(errs, errors.New("memory_reserve, memory_ramp and memory_ramp_share need memory_budget"))
	}
	seenMod := map[string]bool{}
	for _, m := range c.ModuleBlocklist {
		switch {
		case !moduleRE.MatchString(m):
			errs = append(errs, fmt.Errorf("module_blocklist: %q is not a module name (lowercase letters, digits, _ and -)", m))
		case seenMod[strings.ReplaceAll(m, "-", "_")]:
			errs = append(errs, fmt.Errorf("module_blocklist: %q listed twice (the kernel treats - and _ alike)", m))
		default:
			seenMod[strings.ReplaceAll(m, "-", "_")] = true
		}
	}
	if c.CgroupParent != "" && !regexp.MustCompile(`^([A-Za-z0-9_-]+\.slice|/[A-Za-z0-9_./-]+)$`).MatchString(c.CgroupParent) {
		errs = append(errs, errors.New("cgroup_parent must be a systemd slice (name.slice) or an absolute cgroup path"))
	}
	if c.Subnet != "" {
		for s := 1; s <= c.MaxSlots; s++ {
			if _, _, err := net.ParseCIDR(c.Expand(c.Subnet, s)); err != nil {
				errs = append(errs, fmt.Errorf("subnet for slot %d: %w", s, err))
				break
			}
		}
	}
	errs = append(errs, c.Runner.validate("runner")...)
	if c.Runner.Privileged {
		errs = append(errs, errors.New("runner: privileged is not allowed; give Docker to jobs through the dind sidecar"))
	}
	if len(c.Runner.PruneOnStop) > 0 {
		errs = append(errs, errors.New("runner: prune_on_stop is only for dind"))
	}
	if c.Dind != nil {
		errs = append(errs, c.Dind.validate("dind")...)
	}
	return errors.Join(errs...)
}

func (t *Template) validate(name string) []error {
	var errs []error
	if t.Image == "" {
		errs = append(errs, fmt.Errorf("%s: image is required", name))
	} else if !strings.Contains(t.Image, "@sha256:") {
		// Not fatal, but the whole point is that nothing about these containers changes behind your back.
		fmt.Fprintf(os.Stderr, "warning: %s image %q is not pinned by digest\n", name, t.Image)
	}
	if t.Runtime != "" && !runtimeRE.MatchString(t.Runtime) {
		errs = append(errs, fmt.Errorf("%s: runtime %q is not a plausible runtime name", name, t.Runtime))
	}
	if t.Memory != "" {
		if _, err := ParseBytes(t.Memory); err != nil {
			errs = append(errs, fmt.Errorf("%s: memory: %w", name, err))
		}
	}
	if t.MemoryHigh != "" {
		high, err := ParseBytes(t.MemoryHigh)
		limit, lerr := ParseBytes(t.Memory)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: memory_high: %w", name, err))
		case t.Memory == "" || lerr != nil:
			errs = append(errs, fmt.Errorf("%s: memory_high needs a valid memory limit above it", name))
		case high <= 0 || high >= limit:
			errs = append(errs, fmt.Errorf("%s: memory_high (%s) must be above 0 and below memory (%s)", name, t.MemoryHigh, t.Memory))
		}
	}
	if t.CPUShares != 0 && (t.CPUShares < 2 || t.CPUShares > 262144) {
		errs = append(errs, fmt.Errorf("%s: cpu_shares must be 2..262144", name))
	}
	for k := range t.Labels {
		if strings.HasPrefix(k, "io.github.runner-scaling.") {
			errs = append(errs, fmt.Errorf("%s: label %q is reserved for the guard", name, k))
		}
	}
	for _, b := range t.Binds {
		host, ctr, ok := strings.Cut(b, ":")
		ctr, _, _ = strings.Cut(ctr, ":")
		switch {
		case !ok || !path.IsAbs(host) || !path.IsAbs(ctr):
			errs = append(errs, fmt.Errorf("%s: bind %q must be /host/path:/container/path", name, b))
		case strings.Contains(host, "docker.sock") || path.Clean(host) == "/":
			errs = append(errs, fmt.Errorf("%s: bind %q would hand the job the host", name, b))
		}
	}
	return errs
}

// Expand substitutes {slot} and {prefix}.
func (c *Config) Expand(s string, slot int) string {
	return strings.NewReplacer("{slot}", strconv.Itoa(slot), "{prefix}", c.Prefix).Replace(s)
}

// ParseBytes parses sizes like 512m, 3G, 1.5g, or a plain byte count.
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	mult := float64(1)
	for suf, m := range map[string]float64{"k": 1 << 10, "m": 1 << 20, "g": 1 << 30} {
		if strings.HasSuffix(s, suf) || strings.HasSuffix(s, suf+"b") || strings.HasSuffix(s, suf+"ib") {
			s = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(s, "ib"), "b"), suf)
			mult = m
			break
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0, fmt.Errorf("bad size")
	}
	return int64(f * mult), nil
}

// slotLimit is the most memory one slot's containers may use together, from their memory limits (0 if unlimited).
func (c *Config) slotLimit() (limit int64, unlimited bool) {
	ts := []*Template{&c.Runner}
	if c.Dind != nil {
		ts = append(ts, c.Dind)
	}
	for _, t := range ts {
		if t.Memory == "" {
			return 0, true
		}
		n, _ := ParseBytes(t.Memory) // validated by Template.validate
		limit += n
	}
	return limit, false
}

// budgetFit checks the per-container memory limits against memory_budget. One slot that may outgrow the whole
// budget could never run to completion (the backstop would reap it), so that is an error.
func (c *Config) budgetFit(budget int64) []error {
	if limit, unlimited := c.slotLimit(); !unlimited && limit > budget {
		return []error{fmt.Errorf("memory limits of one slot add up to %s, more than memory_budget %s: that slot could never finish", fmtBytes(limit), fmtBytes(budget))}
	}
	return nil
}

// Notices returns advice about a valid config, for the guard to print at startup. All slots together may exceed
// memory_budget: that is the point of sharing it, with the backstop as the safety net, so it is only a notice.
func (c *Config) Notices() []string {
	if c.memBudget <= 0 {
		return nil
	}
	limit, unlimited := c.slotLimit()
	switch {
	case unlimited:
		return []string{"memory_budget is set but a container has no memory limit: one runaway job can use the whole budget before the guard reacts"}
	case limit*int64(c.MaxSlots) > c.memBudget:
		return []string{fmt.Sprintf("%d slots may use up to %s together against memory_budget %s: the guard holds back new slots and reaps the newest runner if use stays over the budget",
			c.MaxSlots, fmtBytes(limit*int64(c.MaxSlots)), fmtBytes(c.memBudget))}
	}
	return nil
}
