package onboard

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"syscall"
)

// Host is what init needs to know about this machine.
type Host struct {
	MemTotalGB int
	DockerGID  int
	Taken      []netip.Prefix
}

// Detect reads the host's memory, the Docker socket's group and the address ranges already in use.
func (d Doctor) Detect(ctx context.Context) (Host, error) {
	if d.DockerHost == "" {
		d.DockerHost = "/var/run/docker.sock"
	}
	h := Host{}
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return h, err
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "MemTotal:") {
			var kb int
			fmt.Sscanf(strings.TrimPrefix(l, "MemTotal:"), "%d", &kb)
			h.MemTotalGB = kb >> 20
		}
	}
	fi, err := os.Stat(d.DockerHost)
	if err != nil {
		return h, fmt.Errorf("docker socket: %w", err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		h.DockerGID = int(st.Gid)
	}
	h.Taken = d.usedNetworks(ctx)
	return h, nil
}
