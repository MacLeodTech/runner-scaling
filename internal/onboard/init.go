package onboard

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/MacLeodTech/runner-scaling/examples"
)

// Plan is what `init` will write, decided from the host and the answers; nothing is touched until Write.
type Plan struct {
	Files   map[string]File // path relative to the stack directory
	Fence   string          // the range every pool's subnet sits in
	Slots   int
	Budget  string
	Notes   []string
	ChownTo int // uid the key should end up owned by (the scaler's), set when we are root
}

// File is one file to write.
type File struct {
	Data []byte
	Mode os.FileMode
}

// PlanInput is everything Plan needs; the caller fills in what it detected.
type PlanInput struct {
	Org            string
	ClientID       string
	InstallationID int64
	Key            []byte // PEM
	DockerGID      int
	MemTotalGB     int
	Taken          []netip.Prefix // ranges already in use on the host
	Pools          []string       // "lite", "sysbox"; empty means lite only
	Version        string         // this binary's version (v0.12.0), to pin the image tag; "dev" keeps the example's
	Root           bool
}

var (
	orgRE     = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	versionRE = regexp.MustCompile(`^v(\d+)\.(\d+)\.\d+`)
)

// ChooseFence picks a /16 inside 172.16.0.0/12 that nothing on the host uses, preferring 172.30 (the examples').
func ChooseFence(taken []netip.Prefix) (string, error) {
	order := []int{30, 31, 29, 28, 27, 26, 25, 24, 23, 22, 21, 20, 19, 18, 17, 16}
	for _, b := range order {
		cand := netip.MustParsePrefix(fmt.Sprintf("172.%d.0.0/16", b))
		free := true
		for _, t := range taken {
			if t.Overlaps(cand) {
				free = false
				break
			}
		}
		if free {
			return cand.String(), nil
		}
	}
	return "", fmt.Errorf("no free /16 inside 172.16.0.0/12: every candidate overlaps a range this host uses")
}

// Size picks max_slots and memory_budget for the host: the runners may use about half the RAM, a slot's working
// set is about 2 GB, never fewer than one slot, never more than 8.
func Size(memGB int) (slots int, budget string) {
	b := memGB / 2
	if b < 3 {
		b = 3
	}
	slots = b / 2
	if slots < 1 {
		slots = 1
	}
	if slots > 8 {
		slots = 8
	}
	return slots, fmt.Sprintf("%dG", b)
}

func replaceOnce(s, old, new, what string) (string, error) {
	if !strings.Contains(s, old) {
		return "", fmt.Errorf("init: the example %s changed (no %q); this is a bug in init", what, old)
	}
	return strings.Replace(s, old, new, 1), nil
}

// KnownPools are the pools init can write, in the order they appear in guard-pools.yaml.
var KnownPools = []string{"lite", "sysbox"}

// poolBlock cuts the lines between "# >>> pool NAME" and "# <<< pool NAME" (markers included) out of s, or returns
// just that block when keep is true.
func poolBlock(s, name string) (before, block, after string, err error) {
	start, end := "  # >>> pool "+name+"\n", "  # <<< pool "+name+"\n"
	i, j := strings.Index(s, start), strings.Index(s, end)
	if i < 0 || j < i {
		return "", "", "", fmt.Errorf("init: guard-pools.yaml has no %q pool block; this is a bug in init", name)
	}
	return s[:i], s[i : j+len(end)], s[j+len(end):], nil
}

// NewPlan builds the files for the stack: the compose file, guard.yaml with the chosen pools, .env and the key.
func NewPlan(in PlanInput) (*Plan, error) {
	if !orgRE.MatchString(in.Org) {
		return nil, fmt.Errorf("%q does not look like a GitHub organization name", in.Org)
	}
	if in.ClientID == "" || in.InstallationID == 0 || len(in.Key) == 0 {
		return nil, fmt.Errorf("the App's Client ID, installation id and key are required")
	}
	if in.DockerGID <= 0 {
		return nil, fmt.Errorf("the Docker socket's group id is unknown")
	}
	want := map[string]bool{}
	for _, n := range in.Pools {
		if !slices.Contains(KnownPools, n) {
			return nil, fmt.Errorf("unknown pool %q (init can write: %s)", n, strings.Join(KnownPools, ", "))
		}
		want[n] = true
	}
	if len(want) == 0 {
		want["lite"] = true
	}
	fence, err := ChooseFence(in.Taken)
	if err != nil {
		return nil, err
	}
	slots, budget := Size(in.MemTotalGB)
	var notes []string
	if want["sysbox"] { // one slot there is a runner and a daemon: 3G + 3G, which the budget must be able to hold
		if b, _ := strconv.Atoi(strings.TrimSuffix(budget, "G")); b < 6 {
			budget = "6G"
			notes = append(notes, fmt.Sprintf("a Sysbox pool needs 6 GB per job and this host has %d GB: raised memory_budget to 6G, so expect one Docker job at a time and little room for anything else", in.MemTotalGB))
		}
	}
	rd := func(n string) string { b, _ := examples.FS.ReadFile(n); return string(b) }
	prefix := strings.TrimSuffix(fence, ".0.0/16") // 172.30
	p := &Plan{Files: map[string]File{}, Fence: fence, Slots: slots, Budget: budget, Notes: notes}

	g := rd("guard-pools.yaml")
	for _, r := range [][3]string{
		{"github_url: https://github.com/YOUR-ORG", "github_url: https://github.com/" + in.Org, "github_url"},
		{"memory_budget: 8G", "memory_budget: " + budget, "memory_budget"},
	} {
		if g, err = replaceOnce(g, r[0], r[1], "guard-pools.yaml"); err != nil {
			return nil, err
		}
	}
	for _, n := range KnownPools {
		before, block, after, err := poolBlock(g, n)
		if err != nil {
			return nil, err
		}
		if !want[n] {
			g = before + after
			continue
		}
		// lite gets the sized slot count; a Docker pool is heavier (two containers a slot), so half as many.
		n2 := slots
		if n == "sysbox" {
			n2 = max(1, slots/2)
		}
		re := regexp.MustCompile(`(?m)^    max_slots: \d+`)
		if !re.MatchString(block) {
			return nil, fmt.Errorf("init: the %s block of guard-pools.yaml has no max_slots; this is a bug in init", n)
		}
		g = before + re.ReplaceAllLiteralString(block, "    max_slots: "+strconv.Itoa(n2)) + after
	}
	g = strings.ReplaceAll(g, "172.30.", prefix+".")
	p.Files["guard.yaml"] = File{[]byte(g), 0o644}

	c := rd("compose.yaml")
	if c, err = replaceOnce(c, "FENCE_SRC: 172.30.0.0/16", "FENCE_SRC: "+fence, "compose.yaml"); err != nil {
		return nil, err
	}
	if m := versionRE.FindStringSubmatch(in.Version); m != nil {
		c = strings.ReplaceAll(c, "runner-scaling:0.13", "runner-scaling:"+m[1]+"."+m[2])
	}
	p.Files["compose.yaml"] = File{[]byte(c), 0o644}

	e := rd("env.example")
	for _, r := range [][2]string{
		{`(?m)^GITHUB_APP_CLIENT_ID=.*$`, "GITHUB_APP_CLIENT_ID=" + in.ClientID},
		{`(?m)^GITHUB_APP_INSTALLATION_ID=.*$`, "GITHUB_APP_INSTALLATION_ID=" + strconv.FormatInt(in.InstallationID, 10)},
		{`(?m)^DOCKER_GID=.*$`, "DOCKER_GID=" + strconv.Itoa(in.DockerGID)},
	} {
		re := regexp.MustCompile(r[0])
		if !re.MatchString(e) {
			return nil, fmt.Errorf("init: env.example has no %s line; this is a bug in init", r[0])
		}
		e = re.ReplaceAllLiteralString(e, r[1])
	}
	p.Files[".env"] = File{[]byte(e), 0o600}
	p.Files["secrets/github_app.pem"] = File{in.Key, 0o400}
	if in.Root {
		p.ChownTo = scalerUID
	}
	p.Notes = append(p.Notes,
		fmt.Sprintf("pools: %s", strings.Join(slices.DeleteFunc(slices.Clone(KnownPools), func(n string) bool { return !want[n] }), ", ")),
		fmt.Sprintf("fenced range %s (nothing on this host uses it); every pool's subnet sits inside it", fence),
		fmt.Sprintf("%d slot(s), %s memory budget for %d GB of RAM", slots, budget, in.MemTotalGB))
	return p, nil
}

// Conflicts lists planned files that already exist under dir.
func (p *Plan) Conflicts(dir string) []string {
	var out []string
	for name := range p.Files {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Write creates the files. The key is written 0400 and, when running as root, handed to the scaler's uid; as a normal
// user the returned note says what to run.
func (p *Plan) Write(dir string, force bool) ([]string, error) {
	if c := p.Conflicts(dir); len(c) > 0 && !force {
		return nil, fmt.Errorf("already exist in %s: %s (use --force to overwrite)", dir, strings.Join(c, ", "))
	}
	var notes []string
	if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o700); err != nil {
		return nil, err
	}
	for name, f := range p.Files {
		path := filepath.Join(dir, name)
		if force {
			os.Remove(path) // a read-only (0400) key cannot be overwritten in place
		}
		if err := os.WriteFile(path, f.Data, f.Mode); err != nil {
			return nil, err
		}
		if err := os.Chmod(path, f.Mode); err != nil {
			return nil, err
		}
	}
	key := filepath.Join(dir, "secrets", "github_app.pem")
	if p.ChownTo != 0 {
		if err := os.Chown(key, p.ChownTo, p.ChownTo); err != nil {
			return nil, err
		}
		os.Chmod(filepath.Join(dir, "secrets"), 0o755)
	} else {
		notes = append(notes, fmt.Sprintf("The scaler container runs as uid %d and must be able to read the key:\n    sudo chown %d:%d %s && sudo chmod 0400 %s", scalerUID, scalerUID, scalerUID, key, key))
	}
	return notes, nil
}
