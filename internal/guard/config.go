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

	Runner Template  `yaml:"runner"`
	Dind   *Template `yaml:"dind"` // optional: omit for runners without a private Docker daemon
}

// Template describes one container.
type Template struct {
	Image       string            `yaml:"image"`
	User        string            `yaml:"user"`
	Command     []string          `yaml:"command"` // runner only: overrides the image's entrypoint (see defaults)
	Env         map[string]string `yaml:"env"`
	Binds       []string          `yaml:"binds"` // host:container[:opts], {slot} allowed in the host part
	Privileged  bool              `yaml:"privileged"`
	CapDrop     []string          `yaml:"cap_drop"`
	CapAdd      []string          `yaml:"cap_add"`
	SecurityOpt []string          `yaml:"security_opt"`
	DNS         []string          `yaml:"dns"`
	Memory      string            `yaml:"memory"` // e.g. 3G
	CPUs        float64           `yaml:"cpus"`
	CPUShares   int64             `yaml:"cpu_shares"` // relative CPU weight against other host workloads (Docker default 1024), 2..262144
	Pids        int64             `yaml:"pids"`
	Health      *Health           `yaml:"healthcheck"`
	Log         map[string]string `yaml:"log_opts"` // json-file options
	Labels      map[string]string `yaml:"labels"`   // extra container labels; the guard's own io.github.runner-scaling.* labels win
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
		// The official runner image. The JIT config arrives in RUNNER_SCALING_JITCONFIG and is moved off the
		// environment before the runner starts, so job steps don't inherit it.
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
	if c.MaxRuntime.Duration < 0 || (c.MaxRuntime.Duration > 0 && c.MaxRuntime.Duration < time.Minute) {
		errs = append(errs, errors.New("max_runtime must be 0 (disabled) or at least 1m"))
	}
	c.memBudget, c.memReserve = 0, 0
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
	} else if c.MemoryReserve != "" {
		errs = append(errs, errors.New("memory_reserve needs memory_budget"))
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
	if t.Memory != "" {
		if _, err := ParseBytes(t.Memory); err != nil {
			errs = append(errs, fmt.Errorf("%s: memory: %w", name, err))
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
