// Package api is the wire format between the scaler and the guard. The guard serves it on a unix socket that
// only the scaler can reach. It is deliberately tiny: the scaler can say which slot to start with which JIT config,
// which slot to stop, and ask what exists. It can never describe a container.
package api

// Info is GET /v1/info.
type Info struct {
	Prefix    string `json:"prefix"`
	MaxSlots  int    `json:"max_slots"`
	WorkDir   string `json:"work_dir"`
	GitHubURL string `json:"github_url"`
	// MemoryBudget is the guard's memory_budget in bytes, 0 when none is set.
	MemoryBudget int64 `json:"memory_budget,omitempty"`
}

// Counters is GET /v1/counters: monotonic totals since the guard started, by name (denied_total,
// admission_refused_total, starts_total, start_failures_total, reaped_<reason>_total).
type Counters map[string]int64

// Slot is one entry of GET /v1/slots.
type Slot struct {
	Slot        int    `json:"slot"`
	RunnerName  string `json:"runner_name,omitempty"`
	RunnerState string `json:"runner_state,omitempty"` // Docker state: running, exited, ...
	DindState   string `json:"dind_state,omitempty"`
	Starting    bool   `json:"starting,omitempty"`
	// Stats is the slot's resource use, summed over its runner and dind containers. Only set when asked for
	// (GET /v1/slots?stats=1).
	Stats *SlotStats `json:"stats,omitempty"`
}

// SlotStats is one sample of a slot's resource use.
type SlotStats struct {
	MemBytes      int64   `json:"mem_bytes"`   // working set (usage minus inactive file cache)
	CPUPercent    float64 `json:"cpu_percent"` // 100 = one core fully busy; 0 until two samples exist
	PIDs          int64   `json:"pids"`
	NetRxBytes    int64   `json:"net_rx_bytes"` // cumulative since the container started
	NetTxBytes    int64   `json:"net_tx_bytes"`
	BlkReadBytes  int64   `json:"blk_read_bytes"` // cumulative
	BlkWriteBytes int64   `json:"blk_write_bytes"`
}

// StartRequest is the body of PUT /v1/slots/{n}.
type StartRequest struct {
	RunnerName string `json:"runner_name"`
	JITConfig  string `json:"jit_config"`
}

// KeepCacheRequest is the body of POST /v1/slots/{n}/keep-cache: the scaler vouches that the runner on this slot is
// running a trusted job, so the slot's warm caches (cache_dirs) survive its teardown. Without it they are emptied.
type KeepCacheRequest struct {
	RunnerName string `json:"runner_name"`
}

// Error is the body of every non-2xx response.
type Error struct {
	Error string `json:"error"`
}
