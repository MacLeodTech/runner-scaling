package guard

import (
	"maps"
	"sync"
)

// Counter names, as served by GET /v1/counters. All are totals since the guard process started.
const (
	CounterDenied                  = "denied_total"                    // bad or refused requests and unknown endpoints: the API was misused. Alert on any.
	CounterNotReady                = "not_ready_total"                 // starts refused with 503: memory budget full, or the firewall gate missing
	CounterConflicts               = "conflicts_total"                 // starts or stops refused because the slot was in use or in transition
	CounterAdmissionRefused        = "admission_refused_total"         // starts held back by the memory budget
	CounterStarts                  = "starts_total"                    // slots started
	CounterStartFailures           = "start_failures_total"            // starts that failed after being admitted
	CounterGateWithdrawn           = "gate_withdrawn_total"            // times require_file vanished (firewall rules lost): running slots may be unfenced. Alert on any.
	CounterModuleBlocklistFailures = "module_blocklist_failures_total" // times module_blocklist could not be applied at startup: jobs can still make the host load those modules. Alert on any.
	CounterReapedPrefix            = "reaped_"                         // + reason + "_total": runner_exited, max_runtime, memory_budget
)

// counters is a small set of monotonic counters, safe for concurrent use.
type counters struct {
	mu sync.Mutex
	m  map[string]int64
}

func (c *counters) add(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]int64{}
	}
	c.m[name]++
}

func (c *counters) snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.m)
}

// Counters returns the guard's counters. Names that have not happened yet are present with value 0, so a scrape
// sees them from the start and rate() works from the first event.
func (g *Guard) Counters() map[string]int64 {
	out := map[string]int64{
		CounterDenied: 0, CounterNotReady: 0, CounterConflicts: 0, CounterAdmissionRefused: 0, CounterStarts: 0, CounterStartFailures: 0, CounterGateWithdrawn: 0,
		CounterReapedPrefix + "runner_exited_total": 0, CounterReapedPrefix + "max_runtime_total": 0,
		CounterReapedPrefix + "memory_budget_total": 0,
	}
	maps.Copy(out, g.count.snapshot())
	return out
}
