package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// The scaler and the guard are separate processes (often separate image versions during an update), so the JSON field
// names are a contract. These tests pin them: renaming a field is a breaking change that must fail here.

func TestWireFieldNames(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"info", Info{Prefix: "p", MaxSlots: 3, WorkDir: "/w", GitHubURL: "https://x", MemoryBudget: 7},
			`{"prefix":"p","max_slots":3,"work_dir":"/w","github_url":"https://x","memory_budget":7}`},
		{"info without budget", Info{Prefix: "p"},
			`{"prefix":"p","max_slots":0,"work_dir":"","github_url":""}`},
		{"slot idle", Slot{Slot: 2}, `{"slot":2}`},
		{"slot running", Slot{Slot: 1, RunnerName: "r", RunnerState: "running", DindState: "running", Starting: true},
			`{"slot":1,"runner_name":"r","runner_state":"running","dind_state":"running","starting":true}`},
		{"stats", SlotStats{MemBytes: 1, CPUPercent: 2.5, PIDs: 3, NetRxBytes: 4, NetTxBytes: 5, BlkReadBytes: 6, BlkWriteBytes: 7},
			`{"mem_bytes":1,"cpu_percent":2.5,"pids":3,"net_rx_bytes":4,"net_tx_bytes":5,"blk_read_bytes":6,"blk_write_bytes":7}`},
		{"start request", StartRequest{RunnerName: "r", JITConfig: "j"}, `{"runner_name":"r","jit_config":"j"}`},
		{"keep cache", KeepCacheRequest{RunnerName: "r"}, `{"runner_name":"r"}`},
		{"error", Error{Error: "no"}, `{"error":"no"}`},
		{"counters", Counters{"starts_total": 4}, `{"starts_total":4}`},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.v)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if string(b) != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, b, c.want)
		}
	}
}

func TestStatsOnlyWhenAsked(t *testing.T) {
	b, _ := json.Marshal(Slot{Slot: 1})
	if strings.Contains(string(b), "stats") {
		t.Errorf("a slot without stats must not carry the key: %s", b)
	}
	b, _ = json.Marshal(Slot{Slot: 1, Stats: &SlotStats{}})
	if !strings.Contains(string(b), `"stats":{`) {
		t.Errorf("a slot with stats must carry them, even zero ones: %s", b)
	}
}

// An older scaler or guard must keep working against a newer peer: unknown fields are ignored on decode by default,
// and missing optional ones decode to zero values.
func TestDecodeToleratesMissingAndExtraFields(t *testing.T) {
	var i Info
	if err := json.Unmarshal([]byte(`{"prefix":"p","max_slots":2,"future_field":true}`), &i); err != nil {
		t.Fatal(err)
	}
	if i.Prefix != "p" || i.MaxSlots != 2 || i.MemoryBudget != 0 {
		t.Errorf("got %+v", i)
	}
}
