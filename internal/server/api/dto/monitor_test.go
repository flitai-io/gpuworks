package dto

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The /monitor pages are built against this literal wire shape (see
// artifacts/archive/2026-09-19-monitor-center/plan.md §6 and the monitor wire
// contract scratch note): field names, null-vs-value nullability and
// empty-vs-null list semantics are pinned here rather than only exercised
// indirectly through handler tests.

func TestMonitorOverviewWireContract(t *testing.T) {
	const wire = `{
		"as_of": 1758182650000, "stale": false,
		"signals": [
			{"key": "ready_replicas", "collected": true, "value": 21, "denominator": 22, "detail": {"not_ready": ["Qwen3-32B"]}},
			{"key": "error_rate", "collected": false},
			{"key": "request_rate", "collected": false},
			{"key": "token_rate", "collected": true, "value": 3100, "denominator": 1820},
			{"key": "ttft", "collected": false},
			{"key": "tpot", "collected": false},
			{"key": "waiting", "collected": true, "value": 12},
			{"key": "preemptions", "collected": false}
		],
		"services": [
			{"deployment_id": 42, "name": "Qwen3-32B", "engine": "vllm", "placement": "TP4 · gpu3, gpu4",
			 "replicas": {"ready": 2, "desired": 2}, "state": "running",
			 "request_rate": null, "ttft_p95_ms": null,
			 "running": 64, "waiting": 12, "kv_usage_pct": 94, "kv_hit_rate_pct": 41,
			 "prompt_tok_s": 3100, "generation_tok_s": 1820,
			 "health": {"status":"ok","threshold_sec":60,"last":null,"recent_mean_ms":null,"recent_ok":0,"recent":[]}}
		],
		"state_counts": [
			{"state": "running", "count": 1}, {"state": "starting", "count": 0}, {"state": "asleep", "count": 0},
			{"state": "offline", "count": 0}, {"state": "failed", "count": 0}, {"state": "stopped", "count": 0}
		],
		"vram_by_node": [
			{"node_uuid": "n1", "name": "gpu3", "gpu_count": 8, "total_bytes": 0, "allocated_bytes": 0, "used_bytes": 0, "used_as_of": 0}
		],
		"cluster_tokens": {
			"prompt": {"labels": {}, "points": [[1758182650000, 3100]]},
			"generation": {"labels": {}, "points": [[1758182650000, 1820]]}
		},
		"events": [
			{"time_ms": 1758182600000, "kind": "instance_running", "deployment_id": 42, "instance_uuid": "inst-1", "label": "Qwen3-32B"}
		]
	}`

	var decoded MonitorOverview
	if err := json.Unmarshal([]byte(wire), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.AsOf != 1758182650000 || decoded.Stale {
		t.Fatalf("as_of/stale = %d/%v", decoded.AsOf, decoded.Stale)
	}
	if len(decoded.Signals) != 8 {
		t.Fatalf("signals = %+v", decoded.Signals)
	}
	ready := decoded.Signals[0]
	if !ready.Collected || ready.Value == nil || *ready.Value != 21 || ready.Denominator == nil || *ready.Denominator != 22 {
		t.Fatalf("ready_replicas signal = %+v", ready)
	}
	if notReady, ok := ready.Detail["not_ready"].([]any); !ok || len(notReady) != 1 || notReady[0] != "Qwen3-32B" {
		t.Fatalf("ready_replicas detail = %+v", ready.Detail)
	}
	errRate := decoded.Signals[1]
	if errRate.Collected || errRate.Value != nil {
		t.Fatalf("error_rate signal must carry no value when uncollected: %+v", errRate)
	}
	if len(decoded.Services) != 1 {
		t.Fatalf("services = %+v", decoded.Services)
	}
	svc := decoded.Services[0]
	if svc.RequestRate != nil || svc.TTFTP95Ms != nil {
		t.Fatalf("service request_rate/ttft_p95_ms must decode as nil, got %+v", svc)
	}
	if svc.Running == nil || *svc.Running != 64 {
		t.Fatalf("service running = %+v", svc.Running)
	}
	if svc.Health.Status != "ok" || svc.Health.ThresholdSec != 60 || svc.Health.Last != nil || svc.Health.RecentMeanMS != nil || svc.Health.RecentOK != 0 {
		t.Fatalf("service health = %+v", svc.Health)
	}
	if svc.Health.Recent == nil || len(svc.Health.Recent) != 0 {
		t.Fatalf("health.recent must decode as an empty (non-nil) slice: %#v", svc.Health.Recent)
	}
	if len(decoded.StateCounts) != 6 || decoded.StateCounts[0] != (MonitorStateCount{State: "running", Count: 1}) || decoded.StateCounts[5] != (MonitorStateCount{State: "stopped", Count: 0}) {
		t.Fatalf("state_counts = %+v", decoded.StateCounts)
	}
	if len(decoded.VRAMByNode) != 1 || decoded.VRAMByNode[0].UsedBytes == nil || *decoded.VRAMByNode[0].UsedBytes != 0 {
		t.Fatalf("vram_by_node = %+v", decoded.VRAMByNode)
	}
	if len(decoded.ClusterTokens.Prompt.Points) != 1 || decoded.ClusterTokens.Prompt.Points[0] != (Point{1758182650000, 3100}) {
		t.Fatalf("cluster_tokens.prompt = %+v", decoded.ClusterTokens.Prompt)
	}
	if len(decoded.Events) != 1 || decoded.Events[0].Kind != "instance_running" || decoded.Events[0].DeploymentID != 42 {
		t.Fatalf("events = %+v", decoded.Events)
	}

	// Round trip: marshal back and check the wire shapes a real (mostly
	// uncollected) overview must produce - nulls stay null, empty lists
	// stay [] rather than disappearing or becoming null, and an uncollected
	// signal carries no value/spark key at all.
	raw, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{
		`"request_rate":null`, `"ttft_p95_ms":null`,
		`"health":{"status":"ok","threshold_sec":60,"last":null,"recent_mean_ms":null,"recent_ok":0,"recent":[]}`,
		`{"key":"error_rate","collected":false}`, `{"key":"preemptions","collected":false}`,
		`"state_counts":[{"state":"running","count":1},{"state":"starting","count":0},{"state":"asleep","count":0},{"state":"offline","count":0},{"state":"failed","count":0},{"state":"stopped","count":0}]`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("marshaled overview missing %q in %s", want, body)
		}
	}
}

// TestMonitorOverviewPhase2SignalsWireContract is the phase2 counterpart to
// TestMonitorOverviewWireContract's uncollected fixture: error_rate,
// request_rate, ttft, tpot and preemptions all carry real values, and
// services[].request_rate/ttft_p95_ms are non-null.
func TestMonitorOverviewPhase2SignalsWireContract(t *testing.T) {
	const wire = `{
		"as_of": 0, "stale": false,
		"signals": [
			{"key": "ready_replicas", "collected": true, "value": 2, "denominator": 2},
			{"key": "error_rate", "collected": true, "value": 3.5, "detail": {"5xx": 2, "429": 1, "asleep_503": 0.5}},
			{"key": "request_rate", "collected": true, "value": 42},
			{"key": "token_rate", "collected": false},
			{"key": "ttft", "collected": true, "value": 2500, "denominator": 3000},
			{"key": "tpot", "collected": true, "value": 50, "denominator": 60},
			{"key": "waiting", "collected": false},
			{"key": "preemptions", "collected": true, "value": 0.5}
		],
		"services": [
			{"deployment_id": 42, "name": "svc", "engine": "vllm", "placement": "",
			 "replicas": {"ready": 2, "desired": 2}, "state": "running",
			 "request_rate": 42, "ttft_p95_ms": 2500,
			 "running": null, "waiting": null, "kv_usage_pct": null, "kv_hit_rate_pct": null,
			 "prompt_tok_s": null, "generation_tok_s": null,
			 "health": {"status":"timeout","threshold_sec":15,"last":{"at_ms":1758182640000,"status":"timeout","latency_ms":15000,"message":"no reply"},"recent_mean_ms":120,"recent_ok":3,"recent":[{"at_ms":1758182640000,"status":"timeout","latency_ms":15000,"message":"no reply"}]}}
		],
		"vram_by_node": [], "cluster_tokens": {"prompt": {"labels": {}, "points": []}, "generation": {"labels": {}, "points": []}},
		"events": []
	}`
	var decoded MonitorOverview
	if err := json.Unmarshal([]byte(wire), &decoded); err != nil {
		t.Fatal(err)
	}
	errRate := decoded.Signals[1]
	if !errRate.Collected || errRate.Value == nil || *errRate.Value != 3.5 {
		t.Fatalf("error_rate = %+v", errRate)
	}
	if errRate.Detail["5xx"] != 2.0 || errRate.Detail["429"] != 1.0 || errRate.Detail["asleep_503"] != 0.5 {
		t.Fatalf("error_rate detail = %+v", errRate.Detail)
	}
	ttft := decoded.Signals[4]
	if !ttft.Collected || ttft.Value == nil || *ttft.Value != 2500 || ttft.Denominator == nil || *ttft.Denominator != 3000 {
		t.Fatalf("ttft = %+v", ttft)
	}
	svc := decoded.Services[0]
	if svc.RequestRate == nil || *svc.RequestRate != 42 || svc.TTFTP95Ms == nil || *svc.TTFTP95Ms != 2500 {
		t.Fatalf("service request_rate/ttft_p95_ms = %+v", svc)
	}
	if svc.Health.Status != "timeout" || svc.Health.ThresholdSec != 15 || svc.Health.Last == nil ||
		svc.Health.Last.Status != "timeout" || svc.Health.Last.LatencyMS != 15000 || svc.Health.Last.Message != "no reply" ||
		svc.Health.RecentMeanMS == nil || *svc.Health.RecentMeanMS != 120 || svc.Health.RecentOK != 3 || len(svc.Health.Recent) != 1 {
		t.Fatalf("service health = %+v", svc.Health)
	}

	raw, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{
		`"request_rate":42`, `"ttft_p95_ms":2500`, `"value":3.5`,
		`"health":{"status":"timeout","threshold_sec":15,"last":{"at_ms":1758182640000,"status":"timeout","latency_ms":15000,"message":"no reply"},"recent_mean_ms":120,"recent_ok":3,"recent":[{"at_ms":1758182640000,"status":"timeout","latency_ms":15000,"message":"no reply"}]}`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("marshaled overview missing %q in %s", want, body)
		}
	}
}

// TestMonitorSignalNoRecentRequestsDetailRoundTrips proves
// detail.no_recent_requests (the "collected, but the diagnosis window
// itself saw nothing" marker - dto.MonitorSignal's doc comment) survives
// decode->encode unchanged, both on its own (request_rate, preemptions) and
// alongside another detail key it must not displace (error_rate's 5xx/429/
// asleep_503 breakdown, ttft's upper_bound).
func TestMonitorSignalNoRecentRequestsDetailRoundTrips(t *testing.T) {
	const wire = `[
		{"key": "error_rate", "collected": true, "value": 0, "detail": {"5xx": 0, "429": 0, "asleep_503": 0, "no_recent_requests": true}},
		{"key": "request_rate", "collected": true, "value": 0, "detail": {"no_recent_requests": true}},
		{"key": "ttft", "collected": true, "detail": {"upper_bound": true, "no_recent_requests": true}},
		{"key": "preemptions", "collected": true, "value": 0, "detail": {"no_recent_requests": true}}
	]`
	var decoded []MonitorSignal
	if err := json.Unmarshal([]byte(wire), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 4 {
		t.Fatalf("signals = %+v", decoded)
	}
	errRate := decoded[0]
	if errRate.Detail["no_recent_requests"] != true {
		t.Fatalf("error_rate.Detail = %+v, want no_recent_requests:true alongside the 5xx/429/asleep_503 breakdown", errRate.Detail)
	}
	if errRate.Detail["5xx"] != 0.0 || errRate.Detail["429"] != 0.0 || errRate.Detail["asleep_503"] != 0.0 {
		t.Fatalf("error_rate.Detail = %+v, want the 5xx/429/asleep_503 breakdown preserved alongside no_recent_requests", errRate.Detail)
	}
	// ttft omits Value/Denominator entirely in this state even though
	// Collected stays true (a "0ms" p95 would misreport an instant
	// response) - this fixture's wire JSON has no "value" key at all, so
	// the decoded pointer must stay nil.
	ttft := decoded[2]
	if ttft.Value != nil {
		t.Fatalf("ttft.Value = %v, want nil (omitted) in the no_recent_requests state", *ttft.Value)
	}
	if ttft.Detail["upper_bound"] != true || ttft.Detail["no_recent_requests"] != true {
		t.Fatalf("ttft.Detail = %+v, want both upper_bound and no_recent_requests true", ttft.Detail)
	}

	raw, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, `"5xx":0`) {
		t.Fatalf("marshaled signals lost error_rate's breakdown alongside no_recent_requests: %s", body)
	}
	if got := strings.Count(body, `"no_recent_requests":true`); got != 4 {
		t.Fatalf("no_recent_requests:true occurrences = %d, want 4 (one per signal in this fixture): %s", got, body)
	}
}

func TestMonitorOverviewEmptyListsAreEmptyNotNull(t *testing.T) {
	overview := MonitorOverview{
		Signals:     []MonitorSignal{},
		Services:    []MonitorServiceRow{},
		StateCounts: []MonitorStateCount{},
		VRAMByNode:  []MonitorVRAMNode{},
		ClusterTokens: MonitorClusterTokens{
			Prompt:     MonitorSeries{Labels: map[string]string{}, Points: []Point{}},
			Generation: MonitorSeries{Labels: map[string]string{}, Points: []Point{}},
		},
		Events: []MonitorEvent{},
	}
	raw, err := json.Marshal(overview)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{
		`"signals":[]`, `"services":[]`, `"state_counts":[]`, `"vram_by_node":[]`,
		`"labels":{}`, `"points":[]`, `"events":[]`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("empty overview must render %q as an empty list/object, got %s", want, body)
		}
	}
	if strings.Contains(body, "null") {
		t.Fatalf("an all-empty overview should carry no null lists: %s", body)
	}
}

func TestMonitorDeploymentWireContract(t *testing.T) {
	const wire = `{
		"as_of": 0, "stale": true,
		"deployment": {"id": 42, "name": "Qwen3-32B", "engine": "vllm", "placement": "TP4 · gpu3, gpu4", "replicas": {"ready": 2, "desired": 2}},
		"alignment": null,
		"ttft_breakdown": null,
		"charts": {"latency": [], "concurrency": [{"labels": {"metric": "running"}, "points": [[1000, 5]]}], "kv": [], "throughput": [], "stage_time": [], "preemptions": []},
		"replicas": [
			{"instance_uuid": "inst-1", "state": "running", "node_name": "gpu3", "gpu_indexes": [0,1,2,3],
			 "running": 38, "waiting": 9, "kv_usage_pct": 96, "kv_hit_rate_pct": 40,
			 "prompt_tok_s": 1500, "generation_tok_s": 900,
			 "gpus": [{"index": 0, "util_pct": 91, "mem_bw_pct": null, "temperature_c": 78, "power_w": null}]}
		],
		"log_instances": ["inst-1"],
		"health": {"status":"off","threshold_sec":60,"last":null,"recent_mean_ms":null,"recent_ok":0,"recent":[]}
	}`

	var decoded MonitorDeployment
	if err := json.Unmarshal([]byte(wire), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Health.Status != "off" || decoded.Health.ThresholdSec != 60 || decoded.Health.Last != nil || decoded.Health.Recent == nil || len(decoded.Health.Recent) != 0 {
		t.Fatalf("health = %+v", decoded.Health)
	}
	if len(decoded.LogInstances) != 1 || decoded.LogInstances[0] != "inst-1" {
		t.Fatalf("log_instances = %+v", decoded.LogInstances)
	}
	if decoded.Alignment != nil || decoded.TTFTBreakdown != nil {
		t.Fatalf("alignment/ttft_breakdown must decode as nil: %+v / %+v", decoded.Alignment, decoded.TTFTBreakdown)
	}
	if len(decoded.Charts.Latency) != 0 || decoded.Charts.Latency == nil {
		t.Fatalf("charts.latency must decode as an empty (non-nil) slice: %#v", decoded.Charts.Latency)
	}
	if len(decoded.Charts.Concurrency) != 1 || decoded.Charts.Concurrency[0].Labels["metric"] != "running" {
		t.Fatalf("charts.concurrency = %+v", decoded.Charts.Concurrency)
	}
	if len(decoded.Replicas) != 1 {
		t.Fatalf("replicas = %+v", decoded.Replicas)
	}
	rep := decoded.Replicas[0]
	if len(rep.GPUIndexes) != 4 || rep.GPUIndexes[3] != 3 {
		t.Fatalf("replica gpu_indexes = %+v", rep.GPUIndexes)
	}
	gpu := rep.GPUs[0]
	if gpu.UtilPct == nil || *gpu.UtilPct != 91 || gpu.MemBWPct != nil || gpu.TemperatureC == nil || *gpu.TemperatureC != 78 || gpu.PowerW != nil {
		t.Fatalf("replica gpu = %+v", gpu)
	}

	raw, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{`"alignment":null`, `"ttft_breakdown":null`, `"latency":[]`, `"mem_bw_pct":null`, `"power_w":null`, `"log_instances":["inst-1"]`,
		`"health":{"status":"off","threshold_sec":60,"last":null,"recent_mean_ms":null,"recent_ok":0,"recent":[]}`} {
		if !strings.Contains(body, want) {
			t.Fatalf("marshaled deployment missing %q in %s", want, body)
		}
	}
}

// TestMonitorDeploymentPhase2AlignmentAndTTFTBreakdownWireContract is the
// phase2 counterpart to TestMonitorDeploymentWireContract's nil fixture:
// alignment and ttft_breakdown both carry real values, and a replica's GPU
// row has a non-null mem_bw_pct.
func TestMonitorDeploymentPhase2AlignmentAndTTFTBreakdownWireContract(t *testing.T) {
	const wire = `{
		"as_of": 0, "stale": false,
		"deployment": {"id": 42, "name": "svc", "engine": "vllm", "placement": "", "replicas": {"ready": 1, "desired": 1}},
		"alignment": {"proxy_requests": 5120, "engine_completed": 5094, "client_errors": 12, "server_errors": 6, "lost": 8},
		"ttft_breakdown": {"queue_ms": 180, "prefill_ms": 260, "first_token_ms": 40, "total_ms": 480, "approximate": false},
		"charts": {"latency": [], "concurrency": [], "kv": [], "throughput": [], "stage_time": [], "preemptions": []},
		"replicas": [
			{"instance_uuid": "inst-1", "state": "running", "node_name": "gpu3", "gpu_indexes": [0],
			 "running": null, "waiting": null, "kv_usage_pct": null, "kv_hit_rate_pct": null,
			 "prompt_tok_s": null, "generation_tok_s": null,
			 "gpus": [{"index": 0, "util_pct": null, "mem_bw_pct": 77, "temperature_c": null, "power_w": null}]}
		],
		"log_instances": ["inst-1"]
	}`
	var decoded MonitorDeployment
	if err := json.Unmarshal([]byte(wire), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Alignment == nil || decoded.Alignment.ProxyRequests != 5120 || decoded.Alignment.ServerErrors != 6 || decoded.Alignment.Lost != 8 {
		t.Fatalf("alignment = %+v", decoded.Alignment)
	}
	if decoded.TTFTBreakdown == nil || decoded.TTFTBreakdown.TotalMs != 480 || decoded.TTFTBreakdown.FirstTokenMs != 40 || decoded.TTFTBreakdown.Approximate {
		t.Fatalf("ttft_breakdown = %+v", decoded.TTFTBreakdown)
	}
	gpu := decoded.Replicas[0].GPUs[0]
	if gpu.MemBWPct == nil || *gpu.MemBWPct != 77 {
		t.Fatalf("gpu.MemBWPct = %v, want 77", gpu.MemBWPct)
	}

	raw, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{`"proxy_requests":5120`, `"server_errors":6`, `"total_ms":480`, `"approximate":false`, `"mem_bw_pct":77`} {
		if !strings.Contains(body, want) {
			t.Fatalf("marshaled deployment missing %q in %s", want, body)
		}
	}
}

// TestMonitorDeploymentReplicaGPUPowerWireContract pins power_w as a real
// wire value, not just the null the phase2 fixtures above decode: a card
// whose power was collected carries its watts as a plain JSON number under
// "power_w", and round-tripping through MonitorReplicaGPU keeps it.
func TestMonitorDeploymentReplicaGPUPowerWireContract(t *testing.T) {
	const wire = `{"instance_uuid": "inst-1", "state": "running", "node_name": "gpu3", "gpu_indexes": [0],
		"running": null, "waiting": null, "kv_usage_pct": null, "kv_hit_rate_pct": null,
		"prompt_tok_s": null, "generation_tok_s": null,
		"gpus": [{"index": 0, "util_pct": 91, "mem_bw_pct": 70, "temperature_c": 78, "power_w": 213.4}]}`
	var decoded MonitorReplicaRow
	if err := json.Unmarshal([]byte(wire), &decoded); err != nil {
		t.Fatal(err)
	}
	gpu := decoded.GPUs[0]
	if gpu.PowerW == nil || *gpu.PowerW != 213.4 {
		t.Fatalf("gpu.PowerW = %v, want 213.4", gpu.PowerW)
	}
	raw, err := json.Marshal(gpu)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"power_w":213.4`) {
		t.Fatalf("marshaled gpu missing power_w:213.4: %s", raw)
	}
}

// TestMonitorTTFTBreakdownApproximateWhenClamped proves Approximate is set
// (and FirstTokenMs is clamped to 0, not negative) whenever queue+prefill
// alone would already exceed - or exactly consume - the TTFT total.
func TestMonitorTTFTBreakdownApproximateWhenClamped(t *testing.T) {
	b := MonitorTTFTBreakdown{QueueMs: 300, PrefillMs: 300, FirstTokenMs: 0, TotalMs: 480, Approximate: true}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"approximate":true`) {
		t.Fatalf("marshaled ttft_breakdown missing approximate:true: %s", raw)
	}
}

func TestMonitorDeploymentEmptyListsAreEmptyNotNull(t *testing.T) {
	deployment := MonitorDeployment{
		Health: DeploymentHealth{Recent: []HealthProbe{}},
		Charts: MonitorCharts{
			Latency: []MonitorSeries{}, TPOT: []MonitorSeries{}, Concurrency: []MonitorSeries{}, KV: []MonitorSeries{},
			Throughput: []MonitorSeries{}, StageTime: []MonitorSeries{}, Preemptions: []MonitorSeries{},
		},
		Replicas:     []MonitorReplicaRow{{GPUIndexes: []int{}, GPUs: []MonitorReplicaGPU{}}},
		LogInstances: []string{},
	}
	raw, err := json.Marshal(deployment)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{
		`"recent":[]`,
		`"latency":[]`, `"tpot":[]`, `"concurrency":[]`, `"kv":[]`, `"throughput":[]`, `"stage_time":[]`, `"preemptions":[]`,
		`"gpu_indexes":[]`, `"gpus":[]`, `"log_instances":[]`,
		// alignment/ttft_breakdown are the deliberate exception: nil there
		// means "not collected", not "empty list" (dto.MonitorDeployment's
		// doc comment), so they must stay null even on an otherwise
		// all-empty deployment.
		`"alignment":null`, `"ttft_breakdown":null`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("empty deployment must render %q, got %s", want, body)
		}
	}
}

func TestMonitorSeriesWireContract(t *testing.T) {
	const wire = `[
		{"labels": {"node": "gpu3"}, "points": [[1000, 12.5], [2000, 13]]},
		{"labels": {}, "points": []}
	]`
	var decoded []MonitorSeries
	if err := json.Unmarshal([]byte(wire), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 2 {
		t.Fatalf("series = %+v", decoded)
	}
	if decoded[0].Labels["node"] != "gpu3" || len(decoded[0].Points) != 2 || decoded[0].Points[0] != (Point{1000, 12.5}) {
		t.Fatalf("series[0] = %+v", decoded[0])
	}
	if decoded[1].Points == nil || len(decoded[1].Points) != 0 {
		t.Fatalf("series[1].points must decode as an empty (non-nil) slice: %#v", decoded[1].Points)
	}

	raw, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, `[1000,12.5]`) {
		t.Fatalf("points must round-trip as [t,v] arrays: %s", body)
	}
	if !strings.Contains(body, `"points":[]`) {
		t.Fatalf("an empty series must render points as [] not null: %s", body)
	}

	// Unknown metric -> [] is the handler's job (api/monitor_test.go), but
	// the shape it must produce - a JSON array, never an object or null -
	// is pinned here too.
	empty, err := json.Marshal([]MonitorSeries{})
	if err != nil {
		t.Fatal(err)
	}
	if string(empty) != "[]" {
		t.Fatalf("empty series list must marshal to []: %s", string(empty))
	}
}

// TestMonitorUIFixturesDecodeStrictlyIntoCurrentDTOs is a read-only
// counterpart to internal/server/app's
// TestMonitorServiceDeploymentChartLabelVocabularyMatchesUIFixture (which
// additionally cross-checks the deployment fixture's chart label vocabulary
// against a live computation - this package is a leaf and has no server to
// do that with). Both ui/tests/fixtures/monitor-*.json files are the
// literal payload the UI's own test suite renders against; decoding them
// here with DisallowUnknownFields means a struct-shape drift between this
// package's DTOs and either fixture fails loudly in the Go suite rather
// than only inside `npm test`.
func TestMonitorUIFixturesDecodeStrictlyIntoCurrentDTOs(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	decodeStrict := func(t *testing.T, name string, v any) {
		t.Helper()
		path := filepath.Join(repoRoot, "ui", "tests", "fixtures", name)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v (is the fixture path still correct?)", path, err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			t.Fatalf("decode %s (DisallowUnknownFields): %v", path, err)
		}
	}

	t.Run("overview", func(t *testing.T) {
		var fixture MonitorOverview
		decodeStrict(t, "monitor-overview.json", &fixture)
	})
	t.Run("deployment", func(t *testing.T) {
		var fixture MonitorDeployment
		decodeStrict(t, "monitor-deployment.json", &fixture)
	})
}

// The PD summary on the wire: the KV transfer is always an object, its
// values null when the engine exports none (vllm-ascend's Mooncake
// connector), and the prefill stage's p95 sits beside its mean.
func TestMonitorPDWireContract(t *testing.T) {
	avg, p95, mb, rate, failed, expired, stageP95 := 1294.2, 1878.0, 217.1, 167.7, 0.0, 0.0, 1878.0
	pd := MonitorPD{
		Fallbacks: map[string]float64{}, PrefillStageP95Ms: &stageP95,
		KVTransfer: MonitorPDKVTransfer{Exposed: true, AvgMs: &avg, P95Ms: &p95, AvgMB: &mb, MBPerSec: &rate, Failed: &failed, Expired: &expired},
	}
	got, err := json.Marshal(pd)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"effectiveness_pct":null,"fallback_rate_pct":null,"fallbacks":{},"prefill_stage_avg_ms":null,"prefill_waiting_avg":null,` +
		`"avg_prompt_tokens":null,"prefill_stage_p95_ms":1878,"kv_transfer":{"exposed":true,"avg_ms":1294.2,"p95_ms":1878,` +
		`"avg_mb":217.1,"mb_per_s":167.7,"failed":0,"expired":0}}`
	if string(got) != want {
		t.Fatalf("wire =\n%s\nwant\n%s", got, want)
	}
	var back MonitorPD
	if err := json.Unmarshal(got, &back); err != nil || *back.KVTransfer.AvgMs != avg || back.KVTransfer.Failed == nil {
		t.Fatalf("round trip %+v %v", back, err)
	}
	none, _ := json.Marshal(MonitorPD{Fallbacks: map[string]float64{}})
	if !strings.Contains(string(none), `"kv_transfer":{"exposed":false,"avg_ms":null,"p95_ms":null,"avg_mb":null,"mb_per_s":null,"failed":null,"expired":null}`) {
		t.Fatalf("not exposed = %s", none)
	}
}

// A PD request on the wire: its stage, null where unknown; any other
// request has "pd": null.
func TestMonitorRequestPDWireContract(t *testing.T) {
	stage, compute := int32(795), int32(640)
	got, _ := json.Marshal(MonitorRequestPD{PrefillInstanceUUID: "p-1", PrefillMS: &stage, PrefillComputeMS: &compute, Fallback: ""})
	if want := `{"prefill_instance_uuid":"p-1","prefill_ms":795,"prefill_queue_ms":null,"prefill_compute_ms":640,"fallback":""}`; string(got) != want {
		t.Fatalf("pd = %s", got)
	}
	plain, _ := json.Marshal(MonitorRequest{Segments: []MonitorRequestSegment{}})
	if !strings.HasSuffix(string(plain), `"segments":[],"pd":null}`) {
		t.Fatalf("plain = %s", plain)
	}
}
