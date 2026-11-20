package guard

// The Docker policy proxy: a second, independent check between the guard and the Docker daemon.
//
// The guard builds every container from guard.yaml, so a bug in it should not be able to hand out root on the host.
// But a guard that holds the Docker socket is root-equivalent: anything that can talk to it can ask for a privileged
// container with / mounted. Path and method filters cannot see that, because it is the body of an ordinary create call.
// This proxy therefore reads the body. It loads the same guard.yaml and rebuilds, with the guard's own spec builders,
// the one container each request is allowed to be. A create that differs in any field is refused. The guard no longer
// holds the socket: it talks to this proxy, which is a separate process in a separate container with its own
// minimal privileges, and shares nothing with the guard but the read-only config.
//
// What it enforces:
//   - an exact allowlist of method + path + query (the guard's fifteen Engine API calls and nothing else);
//   - containers/create bodies identical to what the guard's templates produce for that slot, field for field;
//   - networks, volumes, images, exec commands only for the guard's own names, ranges, images and commands;
//   - containers it starts, inspects, measures or removes must carry the runner-scaling label (removal: this pool's).
//
// What it does not do: it trusts guard.yaml (a config that asks for Privileged, as guard.yaml does for the
// privileged pool, is honoured), it does not look inside the images, and it cannot stop a malicious job from using
// what the templates give it. Keep guard.yaml read-only to everything that is not the proxy and the guard.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MacLeodTech/runner-scaling/internal/jit"
)

// maxPolicyBody bounds any request body the proxy will read. A JIT config is a few KB.
const maxPolicyBody = 256 << 10

var (
	hexIDRE      = regexp.MustCompile(`^[0-9a-f]{12,64}$`)
	memHighArgRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+=[0-9]+$`)
)

// Policy decides whether one Docker API request is one the guard may make.
type Policy struct {
	g      *Guard // only for its pure spec builders: it has no Docker client
	images map[string]bool
	pulls  map[string]bool // canonical "fromImage|tag" of each image pull
	nameRE *regexp.Regexp  // prefix-(runner|dind|wipe)-N

	owner ownerLookup

	execMu sync.Mutex
	execs  map[string]string // exec id -> container it was created in
}

// ownerLookup returns the runner-scaling instance label of a container, found=false if there is no such container
// or it carries no such label.
type ownerLookup func(ctx context.Context, ref string) (instance string, found bool, err error)

// NewPolicy builds the policy for cfg. owner answers ownership questions from the daemon.
func NewPolicy(cfg *Config, owner ownerLookup) *Policy {
	p := &Policy{
		g:      New(cfg, nil, slog.New(slog.DiscardHandler)),
		images: map[string]bool{}, pulls: map[string]bool{},
		nameRE: regexp.MustCompile(`^` + regexp.QuoteMeta(cfg.Prefix) + `-(runner|dind|wipe)-([0-9]+)$`),
		owner:  owner, execs: map[string]string{},
	}
	ts := []*Template{&cfg.Runner}
	if cfg.Dind != nil {
		ts = append(ts, cfg.Dind)
	}
	for _, t := range ts {
		p.images[t.Image] = true
		p.pulls[pullKey(pullQuery(t.Image))] = true
	}
	return p
}

func pullKey(q url.Values) string { return q.Get("fromImage") + "|" + q.Get("tag") }

// Denial is a refused request: Why goes to the log and to the caller.
type Denial struct{ Why string }

func (d *Denial) Error() string { return d.Why }

func deny(format string, a ...any) error { return &Denial{fmt.Sprintf(format, a...)} }

// Decision is what to send to the daemon: the checked, canonical request, never the caller's own bytes.
type Decision struct {
	Method, Path string
	Query        url.Values
	Body         []byte // nil for none
	TrackExec    bool   // remember the exec id in the response
	execOf       string
}

// Check decides one request. path is the URL path without the API version prefix.
func (p *Policy) Check(ctx context.Context, method, rawPath string, q url.Values, body []byte) (*Decision, error) {
	if method == "GET" && rawPath == "/_ping" {
		return &Decision{Method: method, Path: "/_ping", Query: url.Values{}}, onlyQuery(q) // for the proxy's own healthcheck
	}
	if !strings.HasPrefix(rawPath, "/"+apiVersion+"/") {
		return nil, deny("only API %s is allowed", apiVersion)
	}
	path := strings.TrimPrefix(rawPath, "/"+apiVersion)
	if strings.Contains(path, "//") || strings.Contains(path, "..") || strings.ContainsAny(path, "%?#\\") {
		return nil, deny("malformed path")
	}
	d := &Decision{Method: method, Path: path, Query: q}
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")

	switch {
	// ---- images
	case method == "GET" && segs[0] == "images" && len(segs) >= 3 && segs[len(segs)-1] == "json":
		ref := strings.Join(segs[1:len(segs)-1], "/")
		if !p.images[ref] {
			return nil, deny("image %q is not one of the configured images", ref)
		}
		return d, onlyQuery(q)
	case method == "POST" && path == "/images/create":
		for k := range q {
			if k != "fromImage" && k != "tag" {
				return nil, deny("unexpected query %q on image pull", k)
			}
		}
		if !p.pulls[pullKey(q)] {
			return nil, deny("pull of %q:%q is not a configured image", q.Get("fromImage"), q.Get("tag"))
		}
		return d, requireNoBody(body)

	// ---- networks and volumes
	case method == "GET" && len(segs) == 2 && segs[0] == "networks":
		if _, ok := p.slotOf(segs[1], "-slot-"); !ok {
			return nil, deny("network %q is not one of this pool's", segs[1])
		}
		return d, onlyQuery(q)
	case method == "POST" && path == "/networks/create":
		return p.checkCreateNamed(d, q, body, "network", func(name string) (any, error) {
			slot, ok := p.slotOf(name, "-slot-")
			if !ok {
				return nil, deny("network %q is not one of this pool's", name)
			}
			return networkCreateBody(name, p.g.slotSubnet(slot), p.g.labels(slot, "network", "")), nil
		}, "Name")
	case method == "POST" && path == "/volumes/create":
		if p.g.cfg.WorkBind != "" {
			return nil, deny("this pool uses work_bind, not work volumes")
		}
		return p.checkCreateNamed(d, q, body, "volume", func(name string) (any, error) {
			slot, ok := p.slotOf(name, "-work-")
			if !ok {
				return nil, deny("volume %q is not one of this pool's", name)
			}
			return volumeCreateBody(name, p.g.labels(slot, "work", "")), nil
		}, "Name")
	case method == "DELETE" && len(segs) == 2 && segs[0] == "volumes":
		if _, ok := p.slotOf(segs[1], "-work-"); !ok {
			return nil, deny("volume %q is not one of this pool's", segs[1])
		}
		if !queryIs(q, url.Values{"force": {"1"}}) {
			return nil, deny("unexpected query on volume removal")
		}
		return d, requireNoBody(body)

	// ---- containers
	case method == "GET" && path == "/containers/json":
		return d, p.checkList(q)
	case method == "POST" && path == "/containers/create":
		return p.checkContainerCreate(ctx, d, q, body)
	case len(segs) >= 3 && segs[0] == "containers":
		return p.checkContainerOp(ctx, d, method, segs, q, body)

	// ---- exec
	case method == "POST" && len(segs) == 3 && segs[0] == "exec" && segs[2] == "start":
		if err := p.knownExec(segs[1]); err != nil {
			return nil, err
		}
		if err := bodyIs(body, map[string]any{"Detach": true}); err != nil {
			return nil, deny("exec start: %v", err)
		}
		d.Body = canonical(map[string]any{"Detach": true})
		return d, onlyQuery(q)
	case method == "GET" && len(segs) == 3 && segs[0] == "exec" && segs[2] == "json":
		if err := p.knownExec(segs[1]); err != nil {
			return nil, err
		}
		return d, onlyQuery(q)
	}
	return nil, deny("%s %s is not an API call the guard makes", method, path)
}

// slotOf reads N out of <prefix><infix>N with N in 1..max_slots.
func (p *Policy) slotOf(name, infix string) (int, bool) {
	rest, ok := strings.CutPrefix(name, p.g.cfg.Prefix+infix)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || strconv.Itoa(n) != rest || n < 1 || n > p.g.cfg.MaxSlots {
		return 0, false
	}
	return n, true
}

func onlyQuery(q url.Values) error {
	if len(q) != 0 {
		return deny("unexpected query parameters")
	}
	return nil
}

func queryIs(q, want url.Values) bool {
	if len(q) != len(want) {
		return false
	}
	for k, v := range want {
		if !slices.Equal(q[k], v) {
			return false
		}
	}
	return true
}

func requireNoBody(b []byte) error {
	if len(b) != 0 {
		return deny("unexpected request body")
	}
	return nil
}

// canonical is the one JSON encoding the proxy ever forwards.
func canonical(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// decodeBody parses a JSON object, keeping numbers exact, rejecting anything after it.
func decodeBody(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, errors.New("body is not a JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the JSON body")
	}
	return m, nil
}

// normalise round-trips a value through JSON so an expected spec and a received body compare equal field for field.
func normalise(v any) (map[string]any, error) {
	return decodeBody(canonical(v))
}

func bodyIs(body []byte, want any) error {
	got, err := decodeBody(body)
	if err != nil {
		return err
	}
	exp, _ := normalise(want)
	if !reflect.DeepEqual(got, exp) {
		return errors.New("body is not the one the guard sends")
	}
	return nil
}

// checkCreateNamed handles a create whose whole body is derived from one name field.
func (p *Policy) checkCreateNamed(d *Decision, q url.Values, body []byte, what string, build func(name string) (any, error), nameKey string) (*Decision, error) {
	if err := onlyQuery(q); err != nil {
		return nil, err
	}
	got, err := decodeBody(body)
	if err != nil {
		return nil, deny("%s create: %v", what, err)
	}
	name, _ := got[nameKey].(string)
	want, err := build(name)
	if err != nil {
		return nil, err
	}
	exp, _ := normalise(want)
	if !reflect.DeepEqual(got, exp) {
		return nil, deny("%s create for %q differs from what the guard builds", what, name)
	}
	d.Body = canonical(exp)
	return d, nil
}

// checkList allows the two container listings the guard makes: this pool's, and every pool's (for the shared budget).
func (p *Policy) checkList(q url.Values) error {
	if !queryIs(q, url.Values{"all": {"1"}, "filters": {q.Get("filters")}}) {
		return deny("unexpected query on container list")
	}
	var f map[string][]string
	if err := json.Unmarshal([]byte(q.Get("filters")), &f); err != nil || len(f) != 1 || len(f["label"]) != 1 {
		return deny("container list filter must be one label")
	}
	if l := f["label"][0]; l != labelInstance && l != labelInstance+"="+p.g.cfg.Prefix {
		return deny("container list filter %q is not the guard's", l)
	}
	return nil
}

var (
	statsQuery  = url.Values{"stream": {"false"}, "one-shot": {"true"}}
	removeQuery = url.Values{"force": {"1"}, "v": {"1"}}
)

// checkContainerOp handles /containers/{ref}/{json,stats,start,exec} and DELETE /containers/{ref}.
func (p *Policy) checkContainerOp(ctx context.Context, d *Decision, method string, segs []string, q url.Values, body []byte) (*Decision, error) {
	if len(segs) != 3 {
		return nil, deny("%s %s is not an API call the guard makes", method, d.Path)
	}
	ref, op := segs[1], segs[2]
	switch {
	case method == "GET" && op == "json":
		return d, firstErr(onlyQuery(q), p.mustOwn(ctx, ref, false), requireNoBody(body))
	case method == "GET" && op == "stats":
		if !queryIs(q, statsQuery) {
			return nil, deny("unexpected query on stats")
		}
		return d, firstErr(p.mustOwn(ctx, ref, false), requireNoBody(body))
	case method == "POST" && op == "start":
		return d, firstErr(onlyQuery(q), p.mustOwn(ctx, ref, true), requireNoBody(body))
	case method == "POST" && op == "exec":
		return p.checkExecCreate(ctx, d, ref, q, body)
	}
	return nil, deny("%s %s is not an API call the guard makes", method, d.Path)
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// mustOwn requires ref (a name or id) to be a runner-scaling container: of any pool to read, of this pool to change.
// A container that does not exist is let through only for reads and removal, so the daemon's own 404 reaches the guard.
func (p *Policy) mustOwn(ctx context.Context, ref string, write bool) error {
	if !hexIDRE.MatchString(ref) {
		if _, ok := p.nameOK(ref); !ok {
			return deny("container %q is not one of this pool's", ref)
		}
	}
	inst, found, err := p.owner(ctx, ref)
	if err != nil {
		return deny("cannot check ownership of %q: %v", ref, err)
	}
	if !found {
		return nil // nothing there: the daemon answers 404
	}
	if inst == "" {
		return deny("container %q is not a runner-scaling container", ref)
	}
	if write && inst != p.g.cfg.Prefix {
		return deny("container %q belongs to pool %q, not %q", ref, inst, p.g.cfg.Prefix)
	}
	return nil
}

// nameOK reports whether name is one of this pool's container names, and which kind.
func (p *Policy) nameOK(name string) (kind string, ok bool) {
	m := p.nameRE.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	n, _ := strconv.Atoi(m[2])
	if strconv.Itoa(n) != m[2] || n > p.g.cfg.MaxSlots || (n == 0 && m[1] != "wipe") {
		return "", false
	}
	return m[1], true
}

// ---- container removal is DELETE /containers/{ref}, which has only two segments.

func (p *Policy) checkContainerDelete(ctx context.Context, d *Decision, ref string, q url.Values, body []byte) (*Decision, error) {
	if !queryIs(q, removeQuery) {
		return nil, deny("unexpected query on container removal")
	}
	return d, firstErr(p.mustOwn(ctx, ref, true), requireNoBody(body))
}

// ---- exec

func (p *Policy) checkExecCreate(ctx context.Context, d *Decision, ref string, q url.Values, body []byte) (*Decision, error) {
	if p.g.cfg.Dind == nil {
		return nil, deny("this pool has no dind to exec in")
	}
	if kind, ok := p.nameOK(ref); !ok || kind != "dind" {
		return nil, deny("exec is only allowed in this pool's dind containers, not %q", ref)
	}
	if err := firstErr(onlyQuery(q), p.mustOwn(ctx, ref, true)); err != nil {
		return nil, err
	}
	got, err := decodeBody(body)
	if err != nil {
		return nil, deny("exec: %v", err)
	}
	cmds := [][]string{p.g.wipeWorkCmd()}
	cmds = append(cmds, p.g.cfg.Dind.PruneOnStop...)
	for _, c := range cmds {
		exp, _ := normalise(map[string]any{"Cmd": c})
		if reflect.DeepEqual(got, exp) {
			d.Body = canonical(exp)
			d.TrackExec = true
			d.execOf = ref
			return d, nil
		}
	}
	return nil, deny("exec command is not one the guard runs")
}

func (p *Policy) knownExec(id string) error {
	p.execMu.Lock()
	defer p.execMu.Unlock()
	if _, ok := p.execs[id]; !ok {
		return deny("exec %q was not created through this proxy", truncate(id, 16))
	}
	return nil
}

func (p *Policy) rememberExec(id, container string) {
	p.execMu.Lock()
	defer p.execMu.Unlock()
	if len(p.execs) > 4096 { // execs finish within seconds; this only guards against unbounded growth
		clear(p.execs)
	}
	p.execs[id] = container
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ---- container create

func (p *Policy) checkContainerCreate(ctx context.Context, d *Decision, q url.Values, body []byte) (*Decision, error) {
	if len(q) != 1 || len(q["name"]) != 1 {
		return nil, deny("container create needs exactly ?name=")
	}
	name := q.Get("name")
	kind, ok := p.nameOK(name)
	if !ok {
		return nil, deny("container name %q is not one of this pool's", name)
	}
	n, _ := strconv.Atoi(name[strings.LastIndex(name, "-")+1:])
	got, err := decodeBody(body)
	if err != nil {
		return nil, deny("container create: %v", err)
	}

	var want ContainerSpec
	switch kind {
	case "dind":
		if p.g.cfg.Dind == nil {
			return nil, deny("this pool has no dind")
		}
		want = p.g.dindSpec(n)
	case "runner":
		jitCfg, runnerName, err := runnerFields(got)
		if err != nil {
			return nil, deny("runner create: %v", err)
		}
		m := p.g.nameRE.FindStringSubmatch(runnerName)
		if m == nil || m[1] != strconv.Itoa(n) {
			return nil, deny("runner name %q does not fit slot %d", runnerName, n)
		}
		if err := jit.Validate(jitCfg, jit.Expect{
			TenantURL: p.g.cfg.TenantURL, RunnerName: runnerName, WorkFolder: p.g.cfg.WorkDir, SharedHosts: p.g.cfg.SharedHosts,
		}); err != nil {
			return nil, deny("runner create: JIT config refused (%v); after changing guard.yaml restart this proxy as well as the guard", err)
		}
		want = p.g.runnerSpec(n, runnerName, jitCfg)
	case "wipe":
		binds, cmd, err := helperFields(got)
		if err != nil {
			return nil, deny("helper create: %v", err)
		}
		if err := p.allowedHelper(ctx, n, binds, cmd); err != nil {
			return nil, err
		}
		want = p.g.wipeSpec(n, cmd, binds)
	}
	exp, err := normalise(want)
	if err != nil {
		return nil, deny("internal: %v", err)
	}
	if diff := diffKeys("", got, exp); diff != "" {
		return nil, deny("%s create for %q differs from the guard's template at %s", kind, name, diff)
	}
	d.Body = canonical(exp)
	return d, nil
}

// runnerFields pulls out the two things that legitimately vary per runner; everything else is compared in full.
func runnerFields(got map[string]any) (jitCfg, runnerName string, err error) {
	runnerName, _ = got["Hostname"].(string)
	env, _ := got["Env"].([]any)
	for _, e := range env {
		if s, ok := e.(string); ok {
			if v, ok := strings.CutPrefix(s, jitEnv+"="); ok {
				if jitCfg != "" {
					return "", "", errors.New("more than one JIT config")
				}
				jitCfg = v
			}
		}
	}
	if jitCfg == "" || runnerName == "" {
		return "", "", errors.New("missing JIT config or hostname")
	}
	return jitCfg, runnerName, nil
}

func stringList(v any) ([]string, bool) {
	l, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, len(l))
	for i, e := range l {
		if out[i], ok = e.(string); !ok {
			return nil, false
		}
	}
	return out, true
}

// helperFields pulls out the two things that vary per helper container: its mounts and its command.
func helperFields(got map[string]any) (binds, cmd []string, err error) {
	hc, _ := got["HostConfig"].(map[string]any)
	var ok bool
	if binds, ok = stringList(hc["Binds"]); !ok {
		return nil, nil, errors.New("Binds missing")
	}
	if cmd, ok = stringList(got["Entrypoint"]); !ok {
		return nil, nil, errors.New("Entrypoint missing")
	}
	return binds, cmd, nil
}

// allowedHelper checks a helper container's mounts and command against the closed list of things the guard runs in one:
// emptying a slot's work_bind or cache directories, setting memory.high, and writing the module blocklist.
func (p *Policy) allowedHelper(ctx context.Context, slot int, binds, cmd []string) error {
	g := p.g
	same := func(b, c []string) bool { return slices.Equal(binds, b) && slices.Equal(cmd, c) }
	switch {
	case slot == 0:
		if g.cfg.ModuleBlocklist != nil && same([]string{modprobeBind}, g.modprobeCmd()) {
			return nil
		}
		return deny("helper for slot 0 is only allowed to write the module blocklist")
	case g.cfg.WorkBind != "" && same([]string{g.slotWork(slot)}, g.wipeWorkCmd()):
		return nil
	case len(g.cfg.CacheDirs) > 0 && func() bool { b, c := g.cacheWipeCmd(slot); return same(b, c) }():
		return nil
	case len(g.cfg.CacheDirs) > 0 && g.cfg.cacheMax > 0 && func() bool { b, c := g.cacheTrimCmd(slot); return same(b, c) }():
		return nil
	case slices.Equal(binds, []string{cgroupBind}) && g.anyMemoryHigh():
		// memory_high: sh -c <script> sh id=bytes ...; every id must be one of this pool's containers.
		if len(cmd) < 5 || !slices.Equal(cmd[:4], []string{"sh", "-c", memoryHighScript, "sh"}) {
			return deny("helper with the cgroup tree mounted may only run the memory_high script")
		}
		for _, a := range cmd[4:] {
			if !memHighArgRE.MatchString(a) {
				return deny("memory_high argument %q is malformed", a)
			}
			id, _, _ := strings.Cut(a, "=")
			if inst, found, _ := p.owner(ctx, id); !found || inst != g.cfg.Prefix {
				return deny("memory_high target %q is not a container of this pool", id)
			}
		}
		return nil
	}
	return deny("helper container mounts or command are not ones the guard uses")
}

func (g *Guard) anyMemoryHigh() bool {
	return g.cfg.Runner.MemoryHigh != "" || (g.cfg.Dind != nil && g.cfg.Dind.MemoryHigh != "")
}

// diffKeys returns the path of the first difference between a received body and the expected one, "" if equal.
func diffKeys(path string, got, want any) string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return path
		}
		for _, k := range slices.Sorted(mapKeys(g)) {
			if _, ok := w[k]; !ok {
				return path + "/" + k + " (unexpected field)"
			}
		}
		for _, k := range slices.Sorted(mapKeys(w)) {
			gv, ok := g[k]
			if !ok {
				return path + "/" + k + " (missing)"
			}
			if d := diffKeys(path+"/"+k, gv, w[k]); d != "" {
				return d
			}
		}
		return ""
	default:
		if reflect.DeepEqual(got, want) {
			return ""
		}
		if path == "" {
			return "/"
		}
		return path
	}
}

func mapKeys[V any](m map[string]V) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// ---- the HTTP side

// Proxy is an http.Handler that checks each request with a Policy and forwards the allowed ones to the daemon.
type Proxy struct {
	pol    *Policy
	log    *slog.Logger
	daemon *http.Transport
	rp     *httputil.ReverseProxy
}

// NewProxy serves pol in front of the Docker socket at dockerSock.
func NewProxy(cfg *Config, dockerSock string, log *slog.Logger) *Proxy {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", dockerSock)
		},
		ResponseHeaderTimeout: 15 * time.Minute, // image pulls and removals can be slow
	}
	px := &Proxy{log: log, daemon: tr}
	px.pol = NewPolicy(cfg, px.lookupOwner)
	px.rp = &httputil.ReverseProxy{
		Transport: tr,
		Rewrite:   func(*httputil.ProxyRequest) {}, // the request is already built by serve
		ModifyResponse: func(resp *http.Response) error {
			dec, _ := resp.Request.Context().Value(decisionKey{}).(*Decision)
			if dec == nil || !dec.TrackExec || resp.StatusCode != http.StatusCreated {
				return nil
			}
			b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(b))
			var out struct {
				ID string `json:"Id"`
			}
			if err == nil && json.Unmarshal(b, &out) == nil && hexIDRE.MatchString(out.ID) {
				px.pol.rememberExec(out.ID, dec.execOf)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Error("docker daemon unreachable", "err", err)
			writeDenial(w, http.StatusBadGateway, "docker daemon unreachable")
		},
	}
	return px
}

type decisionKey struct{}

// lookupOwner asks the daemon for a container's runner-scaling instance label.
func (px *Proxy) lookupOwner(ctx context.Context, ref string) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://docker/"+apiVersion+"/containers/"+url.PathEscape(ref)+"/json", nil)
	if err != nil {
		return "", false, err
	}
	resp, err := px.daemon.RoundTrip(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("daemon answered %d", resp.StatusCode)
	}
	var out struct {
		Config struct{ Labels map[string]string }
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return "", false, err
	}
	return out.Config.Labels[labelInstance], true, nil
}

func writeDenial(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"message": "runner-scaling docker policy: " + msg})
}

func (px *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPolicyBody))
	if err != nil {
		writeDenial(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	if _, err := url.ParseQuery(r.URL.RawQuery); err != nil {
		writeDenial(w, http.StatusBadRequest, "malformed query")
		return
	}
	// Hijacked or upgraded connections (attach, exec start with a tty) are never allowed.
	if r.Header.Get("Upgrade") != "" {
		px.refuse(w, r, "connection upgrades are not allowed")
		return
	}
	path := r.URL.Path
	if r.URL.EscapedPath() != path {
		px.refuse(w, r, "encoded path")
		return
	}
	var dec *Decision
	// DELETE /containers/{ref} has two segments; every other container call goes through Check.
	if r.Method == "DELETE" {
		if ref, ok := strings.CutPrefix(path, "/"+apiVersion+"/containers/"); ok && ref != "" && !strings.Contains(ref, "/") {
			dec, err = px.pol.checkContainerDelete(r.Context(), &Decision{Method: r.Method, Path: "/containers/" + ref, Query: r.URL.Query()}, ref, r.URL.Query(), body)
		} else {
			dec, err = px.pol.Check(r.Context(), r.Method, path, r.URL.Query(), body)
		}
	} else {
		dec, err = px.pol.Check(r.Context(), r.Method, path, r.URL.Query(), body)
	}
	if err != nil {
		px.refuse(w, r, err.Error())
		return
	}
	out := r.Clone(context.WithValue(r.Context(), decisionKey{}, dec))
	out.URL = &url.URL{Scheme: "http", Host: "docker", Path: versioned(dec.Path), RawQuery: dec.Query.Encode()}
	out.RequestURI = ""
	out.Host = "docker"
	out.Header = http.Header{}
	out.Body, out.ContentLength = http.NoBody, 0
	if dec.Body != nil {
		out.Header.Set("Content-Type", "application/json")
		out.Body, out.ContentLength = io.NopCloser(bytes.NewReader(dec.Body)), int64(len(dec.Body))
	}
	px.rp.ServeHTTP(w, out)
}

func (px *Proxy) refuse(w http.ResponseWriter, r *http.Request, why string) {
	px.log.Warn("DENY", "method", r.Method, "path", truncate(r.URL.Path, 200), "why", why)
	writeDenial(w, http.StatusForbidden, why)
}

// versioned prefixes an API path with the version, except /_ping, which has none.
func versioned(p string) string {
	if p == "/_ping" {
		return p
	}
	return "/" + apiVersion + p
}

// PingDocker asks a Docker API socket (the policy proxy's, for its healthcheck) whether it is up.
func PingDocker(ctx context.Context, socket string) error {
	d := NewSocketDocker(socket)
	req, err := http.NewRequestWithContext(ctx, "GET", "http://docker/_ping", nil)
	if err != nil {
		return err
	}
	resp, err := d.c.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker policy answered %d", resp.StatusCode)
	}
	return nil
}
