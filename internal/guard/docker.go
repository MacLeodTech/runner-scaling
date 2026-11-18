package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Docker is the handful of Docker Engine API calls the guard makes. It is an interface so tests can fake it.
type Docker interface {
	ImageExists(ctx context.Context, ref string) (bool, error)
	ImagePull(ctx context.Context, ref string) error
	NetworkEnsure(ctx context.Context, name, subnet string, labels map[string]string) error
	VolumeCreate(ctx context.Context, name string, labels map[string]string) error
	VolumeRemove(ctx context.Context, name string) error
	ContainerCreate(ctx context.Context, name string, spec ContainerSpec) (string, error)
	ContainerStart(ctx context.Context, id string) error
	ContainerInspect(ctx context.Context, id string) (*ContainerState, error)
	ContainerRemove(ctx context.Context, id string) error
	ContainerList(ctx context.Context, label string) ([]ContainerSummary, error)
	Exec(ctx context.Context, id string, cmd []string) (int, error)
	// ContainerMemory returns a container's memory working set in bytes (usage minus inactive file cache).
	ContainerMemory(ctx context.Context, id string) (int64, error)
	// ContainerStats returns one sample of a container's resource use, for the dashboard.
	ContainerStats(ctx context.Context, id string) (ContainerUsage, error)
}

// ContainerSpec is the body of POST /containers/create. Only fields the guard sets are listed.
type ContainerSpec struct {
	Image            string            `json:"Image"`
	Hostname         string            `json:"Hostname,omitempty"`
	User             string            `json:"User,omitempty"`
	Env              []string          `json:"Env,omitempty"`
	Entrypoint       []string          `json:"Entrypoint,omitempty"`
	Cmd              []string          `json:"Cmd,omitempty"`
	Labels           map[string]string `json:"Labels,omitempty"`
	Healthcheck      *HealthConfig     `json:"Healthcheck,omitempty"`
	HostConfig       HostConfig        `json:"HostConfig"`
	NetworkingConfig *NetworkingConfig `json:"NetworkingConfig,omitempty"`
}

type HealthConfig struct {
	Test        []string      `json:"Test"`
	Interval    time.Duration `json:"Interval,omitempty"`
	Timeout     time.Duration `json:"Timeout,omitempty"`
	Retries     int           `json:"Retries,omitempty"`
	StartPeriod time.Duration `json:"StartPeriod,omitempty"`
}

type HostConfig struct {
	Binds         []string       `json:"Binds,omitempty"`
	NetworkMode   string         `json:"NetworkMode,omitempty"`
	RestartPolicy map[string]any `json:"RestartPolicy,omitempty"`
	Privileged    bool           `json:"Privileged"`
	CapDrop       []string       `json:"CapDrop,omitempty"`
	CapAdd        []string       `json:"CapAdd,omitempty"`
	SecurityOpt   []string       `json:"SecurityOpt,omitempty"`
	DNS           []string       `json:"Dns,omitempty"`
	Memory        int64          `json:"Memory,omitempty"`
	NanoCPUs      int64          `json:"NanoCpus,omitempty"`
	CPUShares     int64          `json:"CpuShares,omitempty"`
	PidsLimit     *int64         `json:"PidsLimit,omitempty"`
	CgroupParent  string         `json:"CgroupParent,omitempty"`
	LogConfig     *LogConfig     `json:"LogConfig,omitempty"`
}

type LogConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config,omitempty"`
}

type NetworkingConfig struct {
	EndpointsConfig map[string]struct{} `json:"EndpointsConfig"`
}

type ContainerState struct {
	Status   string // created, running, exited, dead, ...
	ExitCode int
	Health   string // "" when there is no healthcheck, else starting / healthy / unhealthy
}

type ContainerSummary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	State   string            `json:"State"`
	Created int64             `json:"Created"` // unix seconds
	Labels  map[string]string `json:"Labels"`
}

// ErrNotFound is returned for 404s.
var ErrNotFound = errors.New("not found")

// apiVersion is the oldest Engine API the guard needs (Docker 25).
const apiVersion = "v1.44"

// SocketDocker talks to the Docker daemon over its unix socket.
type SocketDocker struct {
	c *http.Client
}

func NewSocketDocker(socket string) *SocketDocker {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	return &SocketDocker{c: &http.Client{Transport: tr}}
}

func (d *SocketDocker) do(ctx context.Context, method, p string, q url.Values, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	u := "http://docker/" + apiVersion + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return resp.StatusCode, ErrNotFound
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, fmt.Errorf("docker %s %s: %d %s", method, p, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
	}
	// Drain streaming responses (image pull) so the call returns when the work is done.
	_, err = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, err
}

func (d *SocketDocker) ImageExists(ctx context.Context, ref string) (bool, error) {
	_, err := d.do(ctx, http.MethodGet, "/images/"+ref+"/json", nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (d *SocketDocker) ImagePull(ctx context.Context, ref string) error {
	q := url.Values{}
	if name, digest, ok := strings.Cut(ref, "@"); ok {
		// name may carry a tag too (docker:28-dind@sha256:...); the digest wins.
		if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
			name = name[:i]
		}
		q.Set("fromImage", name)
		q.Set("tag", digest)
	} else {
		q.Set("fromImage", ref)
	}
	_, err := d.do(ctx, http.MethodPost, "/images/create", q, nil, nil)
	return err
}

func (d *SocketDocker) NetworkEnsure(ctx context.Context, name, subnet string, labels map[string]string) error {
	var existing struct {
		IPAM struct {
			Config []struct{ Subnet string }
		}
	}
	if _, err := d.do(ctx, http.MethodGet, "/networks/"+name, nil, nil, &existing); err == nil {
		// A network with the right name but another subnet would sit outside
		// the firewall fence, so refuse it rather than reuse it.
		if subnet != "" {
			for _, c := range existing.IPAM.Config {
				if c.Subnet == subnet {
					return nil
				}
			}
			return fmt.Errorf("network %q exists but is not on subnet %s; remove it so it can be recreated", name, subnet)
		}
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	body := map[string]any{"Name": name, "Driver": "bridge", "Labels": labels, "CheckDuplicate": true}
	if subnet != "" {
		body["IPAM"] = map[string]any{"Config": []map[string]string{{"Subnet": subnet}}}
	}
	_, err := d.do(ctx, http.MethodPost, "/networks/create", nil, body, nil)
	return err
}

func (d *SocketDocker) VolumeCreate(ctx context.Context, name string, labels map[string]string) error {
	_, err := d.do(ctx, http.MethodPost, "/volumes/create", nil, map[string]any{"Name": name, "Labels": labels}, nil)
	return err
}

func (d *SocketDocker) VolumeRemove(ctx context.Context, name string) error {
	_, err := d.do(ctx, http.MethodDelete, "/volumes/"+name, url.Values{"force": {"1"}}, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (d *SocketDocker) ContainerCreate(ctx context.Context, name string, spec ContainerSpec) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	_, err := d.do(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, spec, &out)
	return out.ID, err
}

func (d *SocketDocker) ContainerStart(ctx context.Context, id string) error {
	_, err := d.do(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil)
	return err
}

func (d *SocketDocker) ContainerInspect(ctx context.Context, id string) (*ContainerState, error) {
	var out struct {
		State struct {
			Status   string
			ExitCode int
			Health   *struct{ Status string }
		}
	}
	if _, err := d.do(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &out); err != nil {
		return nil, err
	}
	st := &ContainerState{Status: out.State.Status, ExitCode: out.State.ExitCode}
	if out.State.Health != nil {
		st.Health = out.State.Health.Status
	}
	return st, nil
}

func (d *SocketDocker) ContainerRemove(ctx context.Context, id string) error {
	_, err := d.do(ctx, http.MethodDelete, "/containers/"+id, url.Values{"force": {"1"}, "v": {"1"}}, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (d *SocketDocker) ContainerList(ctx context.Context, label string) ([]ContainerSummary, error) {
	f, _ := json.Marshal(map[string][]string{"label": {label}})
	var out []ContainerSummary
	_, err := d.do(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"1"}, "filters": {string(f)}}, nil, &out)
	return out, err
}

// ContainerMemory reads one stats sample: memory_stats.usage minus the inactive file cache (inactive_file on
// cgroup v2, total_inactive_file on v1), the same working set `docker stats` shows.
func (d *SocketDocker) ContainerMemory(ctx context.Context, id string) (int64, error) {
	var out struct {
		MemoryStats struct {
			Usage int64            `json:"usage"`
			Stats map[string]int64 `json:"stats"`
		} `json:"memory_stats"`
	}
	q := url.Values{"stream": {"false"}, "one-shot": {"true"}}
	if _, err := d.do(ctx, http.MethodGet, "/containers/"+id+"/stats", q, nil, &out); err != nil {
		return 0, err
	}
	use := out.MemoryStats.Usage
	if v, ok := out.MemoryStats.Stats["inactive_file"]; ok {
		use -= v
	} else if v, ok := out.MemoryStats.Stats["total_inactive_file"]; ok {
		use -= v
	}
	return max(use, 0), nil
}

// ContainerUsage is one stats sample. CPUNanos is cumulative CPU time, so a rate needs two samples.
type ContainerUsage struct {
	MemBytes, CPUNanos, PIDs, NetRx, NetTx, BlkRead, BlkWrite int64
}

// ContainerStats reads one stats sample (memory as in ContainerMemory).
func (d *SocketDocker) ContainerStats(ctx context.Context, id string) (ContainerUsage, error) {
	var out struct {
		MemoryStats struct {
			Usage int64            `json:"usage"`
			Stats map[string]int64 `json:"stats"`
		} `json:"memory_stats"`
		CPUStats struct {
			CPUUsage struct {
				Total int64 `json:"total_usage"`
			} `json:"cpu_usage"`
		} `json:"cpu_stats"`
		PidsStats struct {
			Current int64 `json:"current"`
		} `json:"pids_stats"`
		Networks map[string]struct {
			Rx int64 `json:"rx_bytes"`
			Tx int64 `json:"tx_bytes"`
		} `json:"networks"`
		BlkioStats struct {
			IO []struct {
				Op    string `json:"op"`
				Value int64  `json:"value"`
			} `json:"io_service_bytes_recursive"`
		} `json:"blkio_stats"`
	}
	q := url.Values{"stream": {"false"}, "one-shot": {"true"}}
	if _, err := d.do(ctx, http.MethodGet, "/containers/"+id+"/stats", q, nil, &out); err != nil {
		return ContainerUsage{}, err
	}
	u := ContainerUsage{MemBytes: out.MemoryStats.Usage, CPUNanos: out.CPUStats.CPUUsage.Total, PIDs: out.PidsStats.Current}
	if v, ok := out.MemoryStats.Stats["inactive_file"]; ok {
		u.MemBytes -= v
	} else if v, ok := out.MemoryStats.Stats["total_inactive_file"]; ok {
		u.MemBytes -= v
	}
	u.MemBytes = max(u.MemBytes, 0)
	for _, n := range out.Networks {
		u.NetRx += n.Rx
		u.NetTx += n.Tx
	}
	for _, b := range out.BlkioStats.IO {
		switch strings.ToLower(b.Op) {
		case "read":
			u.BlkRead += b.Value
		case "write":
			u.BlkWrite += b.Value
		}
	}
	return u, nil
}

// Exec runs cmd detached and polls until it finishes, returning its exit code.
func (d *SocketDocker) Exec(ctx context.Context, id string, cmd []string) (int, error) {
	var created struct {
		ID string `json:"Id"`
	}
	if _, err := d.do(ctx, http.MethodPost, "/containers/"+id+"/exec", nil, map[string]any{"Cmd": cmd}, &created); err != nil {
		return -1, err
	}
	if _, err := d.do(ctx, http.MethodPost, "/exec/"+created.ID+"/start", nil, map[string]any{"Detach": true}, nil); err != nil {
		return -1, err
	}
	for {
		var st struct {
			Running  bool
			ExitCode int
		}
		if _, err := d.do(ctx, http.MethodGet, "/exec/"+created.ID+"/json", nil, nil, &st); err != nil {
			return -1, err
		}
		if !st.Running {
			return st.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
