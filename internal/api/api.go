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
}

// Slot is one entry of GET /v1/slots.
type Slot struct {
	Slot        int    `json:"slot"`
	RunnerName  string `json:"runner_name,omitempty"`
	RunnerState string `json:"runner_state,omitempty"` // Docker state: running, exited, ...
	DindState   string `json:"dind_state,omitempty"`
	Starting    bool   `json:"starting,omitempty"`
}

// StartRequest is the body of PUT /v1/slots/{n}.
type StartRequest struct {
	RunnerName string `json:"runner_name"`
	JITConfig  string `json:"jit_config"`
}

// Error is the body of every non-2xx response.
type Error struct {
	Error string `json:"error"`
}
