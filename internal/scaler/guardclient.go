package scaler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/MacLeodTech/runner-scaling/internal/api"
)

// Guard is what the scaler needs from the guard.
type Guard interface {
	Info(ctx context.Context) (api.Info, error)
	Slots(ctx context.Context) ([]api.Slot, error)
	Start(ctx context.Context, slot int, req api.StartRequest) error
	Stop(ctx context.Context, slot int) error
	KeepCache(ctx context.Context, slot int, runner string) error
}

// GuardClient talks to the guard over its unix socket.
type GuardClient struct{ c *http.Client }

func NewGuardClient(socket string) *GuardClient {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &GuardClient{c: &http.Client{Transport: tr}} // each call gets its own deadline, see do
}

const (
	quickTimeout = 30 * time.Second // reads and keep-cache: the guard answers at once
	slowTimeout  = 10 * time.Minute // Start waits for dind to become healthy; Stop wipes work dirs and prunes images
)

func (g *GuardClient) do(ctx context.Context, method, path string, body, out any, want int) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	timeout := quickTimeout
	if method == http.MethodPut || method == http.MethodDelete {
		timeout = slowTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://guard"+path, rd)
	if err != nil {
		return err
	}
	resp, err := g.c.Do(req)
	if err != nil {
		return fmt.Errorf("guard: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		var e api.Error
		_ = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&e)
		return fmt.Errorf("guard %s %s: %d %s", method, path, resp.StatusCode, e.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (g *GuardClient) Info(ctx context.Context) (api.Info, error) {
	var i api.Info
	return i, g.do(ctx, http.MethodGet, "/v1/info", nil, &i, http.StatusOK)
}

func (g *GuardClient) Slots(ctx context.Context) ([]api.Slot, error) {
	var s []api.Slot
	return s, g.do(ctx, http.MethodGet, "/v1/slots", nil, &s, http.StatusOK)
}

// SlotStats is Slots with each slot's resource use (CPU, memory, pids, network and disk I/O).
func (g *GuardClient) SlotStats(ctx context.Context) ([]api.Slot, error) {
	var s []api.Slot
	return s, g.do(ctx, http.MethodGet, "/v1/slots?stats=1", nil, &s, http.StatusOK)
}

// Counters returns the guard's counters (denials, refusals, reaps, starts).
func (g *GuardClient) Counters(ctx context.Context) (api.Counters, error) {
	var c api.Counters
	return c, g.do(ctx, http.MethodGet, "/v1/counters", nil, &c, http.StatusOK)
}

func (g *GuardClient) Start(ctx context.Context, slot int, req api.StartRequest) error {
	return g.do(ctx, http.MethodPut, "/v1/slots/"+strconv.Itoa(slot), req, nil, http.StatusCreated)
}

func (g *GuardClient) Stop(ctx context.Context, slot int) error {
	return g.do(ctx, http.MethodDelete, "/v1/slots/"+strconv.Itoa(slot), nil, nil, http.StatusNoContent)
}

func (g *GuardClient) KeepCache(ctx context.Context, slot int, runner string) error {
	return g.do(ctx, http.MethodPost, "/v1/slots/"+strconv.Itoa(slot)+"/keep-cache", api.KeepCacheRequest{RunnerName: runner}, nil, http.StatusNoContent)
}
