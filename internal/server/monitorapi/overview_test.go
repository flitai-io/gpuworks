package monitorapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/healthcheck"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/monitor"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

func inventoryFunc(inv monitor.Inventory) func(context.Context) (monitor.Inventory, error) {
	return func(context.Context) (monitor.Inventory, error) { return inv, nil }
}

func TestMonitorEventKind(t *testing.T) {
	for _, tc := range []struct {
		name     string
		data     string
		wantKind string
		wantOK   bool
	}{
		{"running", `{"state":"running"}`, "instance_running", true},
		{"failed", `{"state":"failed"}`, "instance_failed", true},
		{"stopped", `{"state":"stopped"}`, "instance_stopped", true},
		{"progress state ignored", `{"state":"pulling"}`, "", false},
		{"empty data", "", "", false},
		{"malformed json", "{not json", "", false},
		{"no state field", `{"detail":"x"}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, ok := monitorEventKind(tc.data)
			if ok != tc.wantOK || kind != tc.wantKind {
				t.Fatalf("monitorEventKind(%q) = (%q, %v), want (%q, %v)", tc.data, kind, ok, tc.wantKind, tc.wantOK)
			}
		})
	}
}

func TestSignalsDTOFixedOrderAndSpark(t *testing.T) {
	signals := monitor.ClusterSignals{
		ReadyReplicas: 1, DesiredReplicas: 2,
		HasWaiting: true, WaitingP95: 5, WaitingSeries: []tsdb.Point{{TimeMS: 0, Value: 1}, {TimeMS: 1000, Value: 5}},
		HasTokens: true, InputTokensPerSec: 10, OutputTokensPerSec: 4,
		PromptTokensPerSecSeries: []tsdb.Point{{TimeMS: 0, Value: 2}, {TimeMS: 1000, Value: 10}},
	}
	out := signalsDTO(signals)
	if len(out) != 8 {
		t.Fatalf("signals = %+v, want 8", out)
	}
	wantKeys := []string{"ready_replicas", "error_rate", "request_rate", "token_rate", "ttft", "tpot", "waiting", "preemptions"}
	for i, k := range wantKeys {
		if out[i].Key != k {
			t.Fatalf("signals[%d].Key = %q, want %q (fixed order)", i, out[i].Key, k)
		}
	}

	ready := out[0]
	if ready.Value == nil || *ready.Value != 1 || ready.Denominator == nil || *ready.Denominator != 2 {
		t.Fatalf("ready_replicas = %+v, want 1/2", ready)
	}
	if ready.Spark != nil {
		t.Fatalf("ready_replicas.Spark = %+v, want nil (no natural series to draw from)", ready.Spark)
	}

	tokenRate := out[3]
	if len(tokenRate.Spark) == 0 {
		t.Fatal("token_rate.Spark must be non-empty when collected")
	}

	waiting := out[6]
	if len(waiting.Spark) == 0 {
		t.Fatal("waiting.Spark must be non-empty when collected")
	}

	for _, uncollectedIdx := range []int{1, 2, 4, 5, 7} {
		s := out[uncollectedIdx]
		if s.Collected {
			t.Fatalf("signals[%d] (%s) must be Collected=false", uncollectedIdx, s.Key)
		}
		if s.Value != nil || s.Spark != nil {
			t.Fatalf("uncollected signal %+v must carry no value/spark", s)
		}
	}
}

// TestVramNodesDTONullsUsedFieldsWhenNotCollected pins the wire's
// null-vs-value rule for vram_by_node: a node with no recent "actual usage"
// sample (HasUsedBytes false) must render used_bytes and used_as_of as
// null, never a zero-valued 0.
func TestVramNodesDTONullsUsedFieldsWhenNotCollected(t *testing.T) {
	nodes := []monitor.VRAMNode{
		{NodeUUID: "n1", NodeName: "gpu1", TotalBytes: 100, AllocatedBytes: 40},
		{NodeUUID: "n2", NodeName: "gpu2", TotalBytes: 200, AllocatedBytes: 80, HasUsedBytes: true, UsedBytes: 55, UsedAtMS: 1234},
	}
	out := vramNodesDTO(nodes, map[string]int{"n1": 8, "n2": 4})
	if len(out) != 2 {
		t.Fatalf("out = %+v, want 2 rows", out)
	}
	uncollected := out[0]
	if uncollected.UsedBytes != nil || uncollected.UsedAsOf != nil {
		t.Fatalf("uncollected node = %+v, want used_bytes/used_as_of nil", uncollected)
	}
	if uncollected.GPUCount != 8 || uncollected.TotalBytes != 100 || uncollected.AllocatedBytes != 40 {
		t.Fatalf("uncollected node other fields = %+v", uncollected)
	}
	collected := out[1]
	if collected.UsedBytes == nil || *collected.UsedBytes != 55 || collected.UsedAsOf == nil || *collected.UsedAsOf != 1234 {
		t.Fatalf("collected node = %+v, want used_bytes=55 used_as_of=1234", collected)
	}
}

// TestSignalsDTOPhase2SignalsCollected proves error_rate/request_rate/ttft/
// tpot/preemptions actually populate once the corresponding
// monitor.ClusterSignals fields are collected - the phase2 counterpart to
// TestSignalsDTOFixedOrderAndSpark's phase1 "always uncollected" case.
func TestSignalsDTOPhase2SignalsCollected(t *testing.T) {
	signals := monitor.ClusterSignals{
		// 5xx alone (0.5%) stays under 2 (2%); the
		// combined total (2.0%) reaches it - this fixture is deliberately
		// the "warn from the total, not crit from 5xx alone" case; see
		// TestSignalsDTOErrorRateCritWhen5xxAloneCrossesBound for the other.
		HasErrRate: true, ErrRate5xxPct: 0.5, ErrRate429Pct: 1, ErrRateAsleepPct: 0.5, ErrRateTotalPct: 2,
		ErrorRateSeries: []tsdb.Point{{TimeMS: 0, Value: 2}},
		HasRequestRate:  true, RequestsPerSec: 42,
		RequestsPerSecSeries: []tsdb.Point{{TimeMS: 0, Value: 42}},
		// 500ms above 3000 so ttft's severity stays
		// "warn" regardless of the configured threshold's exact value.
		HasTTFT: true, TTFTP95Ms: 3000 + 500, TTFTP99Ms: 3000 + 1000,
		TTFTP95Series: []tsdb.Point{{TimeMS: 0, Value: 3000 + 500}},
		HasTPOT:       true, TPOTP95Ms: 150, TPOTP99Ms: 150 + 50,
		TPOTP95Series:  []tsdb.Point{{TimeMS: 0, Value: 150}},
		HasPreemptions: true, PreemptionsPerSec: 0.5,
		PreemptionsPerSecSeries: []tsdb.Point{{TimeMS: 0, Value: 0.5}},
	}
	out := signalsDTO(signals)
	byKey := map[string]int{}
	for i, s := range out {
		byKey[s.Key] = i
	}

	errRate := out[byKey["error_rate"]]
	if !errRate.Collected || errRate.Value == nil || *errRate.Value != 2.0 {
		t.Fatalf("error_rate = %+v, want collected/2.0", errRate)
	}
	if errRate.Detail["5xx"] != 0.5 || errRate.Detail["429"] != 1.0 || errRate.Detail["asleep_503"] != 0.5 {
		t.Fatalf("error_rate.Detail = %+v", errRate.Detail)
	}
	if len(errRate.Spark) == 0 {
		t.Error("error_rate.Spark must be non-empty when collected")
	}

	reqRate := out[byKey["request_rate"]]
	if !reqRate.Collected || reqRate.Value == nil || *reqRate.Value != 42 {
		t.Fatalf("request_rate = %+v, want collected/42", reqRate)
	}
	if len(reqRate.Spark) == 0 {
		t.Error("request_rate.Spark must be non-empty when collected")
	}

	ttft := out[byKey["ttft"]]
	wantTTFT, wantTTFTDenom := 3500.0, 4000.0
	if !ttft.Collected || ttft.Value == nil || *ttft.Value != wantTTFT || ttft.Denominator == nil || *ttft.Denominator != wantTTFTDenom {
		t.Fatalf("ttft = %+v, want collected value=%v denominator=%v", ttft, wantTTFT, wantTTFTDenom)
	}
	if ttft.Detail["upper_bound"] != true {
		t.Errorf("ttft.Detail = %+v, want upper_bound:true", ttft.Detail)
	}
	if len(ttft.Spark) == 0 {
		t.Error("ttft.Spark must be non-empty when collected")
	}

	tpot := out[byKey["tpot"]]
	if !tpot.Collected || tpot.Value == nil || *tpot.Value != 150 {
		t.Fatalf("tpot = %+v, want collected/%v", tpot, 150)
	}
	if tpot.Detail["upper_bound"] != true {
		t.Errorf("tpot.Detail = %+v, want upper_bound:true", tpot.Detail)
	}
	if len(tpot.Spark) == 0 {
		t.Error("tpot.Spark must be non-empty when collected")
	}

	preempt := out[byKey["preemptions"]]
	if !preempt.Collected || preempt.Value == nil || *preempt.Value != 0.5 {
		t.Fatalf("preemptions = %+v, want collected/0.5", preempt)
	}
	if len(preempt.Spark) == 0 {
		t.Error("preemptions.Spark must be non-empty when collected")
	}
}

// TestSignalsDTOTTFTTPOTNoUpperBoundDetailWhenUncollected proves the
// upper_bound flag never appears when the signal itself was not collected.
func TestSignalsDTOTTFTTPOTNoUpperBoundDetailWhenUncollected(t *testing.T) {
	out := signalsDTO(monitor.ClusterSignals{})
	for _, s := range out {
		if s.Key == "ttft" || s.Key == "tpot" {
			if s.Detail != nil {
				t.Fatalf("%s.Detail = %+v, want nil when uncollected", s.Key, s.Detail)
			}
		}
	}
}

// TestSignalsDTONoRecentRequestsThreeStates is the wire-level table test for
// the "quiet window, not a platform gap" state (dto.MonitorSignal's doc
// comment): request_rate, error_rate, preemptions, waiting and token_rate
// all report Collected=true with a real (zero) Value and
// detail.no_recent_requests=true once ProxyNoRecent is set; ttft/tpot use
// their own TTFTNoRecent/TPOTNoRecent instead and additionally omit
// Value/Denominator even though Collected stays true. Each signal is
// covered in its three states: never collected, collected+recent (no
// no_recent_requests marker) and collected+idle-window (the new state).
func TestSignalsDTONoRecentRequestsThreeStates(t *testing.T) {
	get := func(t *testing.T, out []dto.MonitorSignal, key string) dto.MonitorSignal {
		t.Helper()
		for _, s := range out {
			if s.Key == key {
				return s
			}
		}
		t.Fatalf("signal %q not found in %+v", key, out)
		return dto.MonitorSignal{}
	}

	cases := []struct {
		name          string
		key           string
		signals       monitor.ClusterSignals
		wantCollected bool
		wantValue     *float64 // nil means "want a nil pointer on the wire"
		wantNoRecent  bool
	}{
		{name: "request_rate never collected", key: "request_rate", signals: monitor.ClusterSignals{},
			wantCollected: false, wantValue: nil},
		{name: "request_rate collected recent", key: "request_rate",
			signals:       monitor.ClusterSignals{HasRequestRate: true, RequestsPerSec: 12},
			wantCollected: true, wantValue: floatPtr(12)},
		{name: "request_rate collected idle window", key: "request_rate",
			signals:       monitor.ClusterSignals{HasRequestRate: true, HasErrRate: true, ProxyNoRecent: true},
			wantCollected: true, wantValue: floatPtr(0), wantNoRecent: true},

		{name: "error_rate never collected", key: "error_rate", signals: monitor.ClusterSignals{},
			wantCollected: false, wantValue: nil},
		{name: "error_rate collected recent", key: "error_rate",
			signals:       monitor.ClusterSignals{HasErrRate: true, ErrRate5xxPct: 0.3, ErrRateTotalPct: 0.5},
			wantCollected: true, wantValue: floatPtr(0.5)},
		{name: "error_rate collected idle window", key: "error_rate",
			signals:       monitor.ClusterSignals{HasErrRate: true, HasRequestRate: true, ProxyNoRecent: true},
			wantCollected: true, wantValue: floatPtr(0), wantNoRecent: true},

		{name: "preemptions never collected", key: "preemptions", signals: monitor.ClusterSignals{},
			wantCollected: false, wantValue: nil},
		{name: "preemptions collected recent", key: "preemptions",
			signals:       monitor.ClusterSignals{HasPreemptions: true, PreemptionsPerSec: 0.05},
			wantCollected: true, wantValue: floatPtr(round1(0.05))},
		{name: "preemptions collected idle window", key: "preemptions",
			signals:       monitor.ClusterSignals{HasPreemptions: true, ProxyNoRecent: true},
			wantCollected: true, wantValue: floatPtr(0), wantNoRecent: true},

		{name: "waiting never collected", key: "waiting", signals: monitor.ClusterSignals{},
			wantCollected: false, wantValue: nil},
		{name: "waiting collected recent", key: "waiting",
			signals:       monitor.ClusterSignals{HasWaiting: true, WaitingP95: 5},
			wantCollected: true, wantValue: floatPtr(5)},
		{name: "waiting collected idle window", key: "waiting",
			signals:       monitor.ClusterSignals{HasWaiting: true, ProxyNoRecent: true},
			wantCollected: true, wantValue: floatPtr(0), wantNoRecent: true},

		{name: "token_rate never collected", key: "token_rate", signals: monitor.ClusterSignals{},
			wantCollected: false, wantValue: nil},
		{name: "token_rate collected recent", key: "token_rate",
			signals:       monitor.ClusterSignals{HasTokens: true, InputTokensPerSec: 10, OutputTokensPerSec: 4},
			wantCollected: true, wantValue: floatPtr(10)},
		{name: "token_rate collected idle window", key: "token_rate",
			signals:       monitor.ClusterSignals{HasTokens: true, ProxyNoRecent: true},
			wantCollected: true, wantValue: floatPtr(0), wantNoRecent: true},

		{name: "ttft never collected", key: "ttft", signals: monitor.ClusterSignals{},
			wantCollected: false, wantValue: nil},
		{name: "ttft collected recent", key: "ttft",
			signals:       monitor.ClusterSignals{HasTTFT: true, TTFTP95Ms: 250},
			wantCollected: true, wantValue: floatPtr(250)},
		{name: "ttft collected idle window", key: "ttft",
			signals:       monitor.ClusterSignals{HasTTFT: true, TTFTNoRecent: true},
			wantCollected: true, wantValue: nil, wantNoRecent: true},

		{name: "tpot never collected", key: "tpot", signals: monitor.ClusterSignals{},
			wantCollected: false, wantValue: nil},
		{name: "tpot collected recent", key: "tpot",
			signals:       monitor.ClusterSignals{HasTPOT: true, TPOTP95Ms: 40},
			wantCollected: true, wantValue: floatPtr(40)},
		{name: "tpot collected idle window", key: "tpot",
			signals:       monitor.ClusterSignals{HasTPOT: true, TPOTNoRecent: true},
			wantCollected: true, wantValue: nil, wantNoRecent: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := get(t, signalsDTO(tc.signals), tc.key)
			if got.Collected != tc.wantCollected {
				t.Fatalf("%s.Collected = %v, want %v", tc.key, got.Collected, tc.wantCollected)
			}
			switch {
			case tc.wantValue == nil && got.Value != nil:
				t.Fatalf("%s.Value = %v, want nil", tc.key, *got.Value)
			case tc.wantValue != nil && (got.Value == nil || *got.Value != *tc.wantValue):
				t.Fatalf("%s.Value = %v, want %v", tc.key, got.Value, *tc.wantValue)
			}
			gotNoRecent, _ := got.Detail["no_recent_requests"].(bool)
			if gotNoRecent != tc.wantNoRecent {
				t.Fatalf("%s.Detail[no_recent_requests] = %v (detail=%+v), want %v", tc.key, gotNoRecent, got.Detail, tc.wantNoRecent)
			}
			if !tc.wantNoRecent && got.Detail != nil && got.Detail["no_recent_requests"] != nil {
				t.Fatalf("%s.Detail = %+v, want no no_recent_requests key at all when not set", tc.key, got.Detail)
			}
		})
	}
}

// TestSignalsDTOErrorRateValueRounded proves the headline value is rounded
// to one decimal, the same precision the detail percentages already use.
func TestSignalsDTOErrorRateValueRounded(t *testing.T) {
	signals := monitor.ClusterSignals{HasErrRate: true, ErrRateTotalPct: 3.4599999999999995}
	out := signalsDTO(signals)
	for _, s := range out {
		if s.Key == "error_rate" {
			if s.Value == nil || *s.Value != 3.5 {
				t.Fatalf("error_rate.Value = %v, want 3.5 (rounded)", s.Value)
			}
		}
	}
}

// TestSignalsDTORoundsValuesToOneDecimal proves request_rate/token_rate/
// ttft/tpot/waiting/preemptions each round their value/denominator to one
// decimal (round1), the same precision error_rate already uses - a real
// server showed a raw float64 like 2349.9999999999995 on the wire before
// this rounding was added.
func TestSignalsDTORoundsValuesToOneDecimal(t *testing.T) {
	signals := monitor.ClusterSignals{
		HasRequestRate: true, RequestsPerSec: 41.999999999999993,
		HasTokens: true, InputTokensPerSec: 10.049999999999999, OutputTokensPerSec: 4.05000000000001,
		HasTTFT: true, TTFTP95Ms: 2349.9999999999995, TTFTP99Ms: 3000.0400000000004,
		HasTPOT: true, TPOTP95Ms: 149.94999999999999, TPOTP99Ms: 200.05000000000001,
		HasWaiting: true, WaitingP95: 5.05000000000001,
		HasPreemptions: true, PreemptionsPerSec: 0.44999999999999996,
	}
	out := signalsDTO(signals)
	byKey := map[string]dto.MonitorSignal{}
	for _, s := range out {
		byKey[s.Key] = s
	}

	if v := byKey["request_rate"].Value; v == nil || *v != 42.0 {
		t.Fatalf("request_rate.Value = %v, want 42.0 (rounded)", v)
	}
	if v, d := byKey["token_rate"].Value, byKey["token_rate"].Denominator; v == nil || *v != 10.0 || d == nil || *d != 4.1 {
		t.Fatalf("token_rate = value=%v denominator=%v, want 10.0/4.1 (rounded)", v, d)
	}
	if v, d := byKey["ttft"].Value, byKey["ttft"].Denominator; v == nil || *v != 2350.0 || d == nil || *d != 3000.0 {
		t.Fatalf("ttft = value=%v denominator=%v, want 2350.0/3000.0 (rounded)", v, d)
	}
	if v, d := byKey["tpot"].Value, byKey["tpot"].Denominator; v == nil || *v != 150.0 || d == nil || *d != 200.1 {
		t.Fatalf("tpot = value=%v denominator=%v, want 150.0/200.1 (rounded)", v, d)
	}
	if v := byKey["waiting"].Value; v == nil || *v != 5.1 {
		t.Fatalf("waiting.Value = %v, want 5.1 (rounded)", v)
	}
	if v := byKey["preemptions"].Value; v == nil || *v != 0.5 {
		t.Fatalf("preemptions.Value = %v, want 0.5 (rounded)", v)
	}
}

// TestServiceRowDTORoundsAllNumericFields proves every numeric field the
// cluster overview's per-service row carries - not just request_rate and
// ttft_p95_ms - is rounded to one decimal (round1), the same precision the
// signal row and the deployment page's replica rows
// (replicaRowsDTO/TestReplicaRowsDTORoundsValues, if present) already use: a
// raw float64 aggregation like 41.999999999999993 must never reach the
// wire as-is.
func TestServiceRowDTORoundsAllNumericFields(t *testing.T) {
	row := monitor.ServiceRow{
		DeploymentID:   1,
		HasRequestRate: true, RequestsPerSec: 41.999999999999993,
		HasTTFT: true, TTFTP95Sec: 2.3499999999999996,
		HasRunning: true, RunningAvg: 12.049999999999999,
		HasWaiting: true, WaitingP95: 5.05000000000001,
		HasKVUsage: true, KVUsageMax: 0.940000000000001,
		HasKVHitRate: true, KVHitRate: 0.409999999999999,
		HasTokens: true, PromptTokensPerSec: 3100.049999999999999, GenTokensPerSec: 1820.05000000000001,
	}
	got := serviceRowDTO(row, "", "running", dto.DeploymentHealth{Status: "ok"})
	if got.RequestRate == nil || *got.RequestRate != 42.0 {
		t.Fatalf("RequestRate = %v, want 42.0 (rounded)", got.RequestRate)
	}
	if got.TTFTP95Ms == nil || *got.TTFTP95Ms != 2350.0 {
		t.Fatalf("TTFTP95Ms = %v, want 2350.0 (rounded)", got.TTFTP95Ms)
	}
	if got.Running == nil || *got.Running != 12.0 {
		t.Fatalf("Running = %v, want 12.0 (rounded)", got.Running)
	}
	if got.Waiting == nil || *got.Waiting != 5.1 {
		t.Fatalf("Waiting = %v, want 5.1 (rounded)", got.Waiting)
	}
	if got.KVUsagePct == nil || *got.KVUsagePct != 94.0 {
		t.Fatalf("KVUsagePct = %v, want 94.0 (rounded, x100)", got.KVUsagePct)
	}
	if got.KVHitRatePct == nil || *got.KVHitRatePct != 41.0 {
		t.Fatalf("KVHitRatePct = %v, want 41.0 (rounded, x100)", got.KVHitRatePct)
	}
	if got.PromptTokS == nil || *got.PromptTokS != 3100.1 {
		t.Fatalf("PromptTokS = %v, want 3100.1 (rounded)", got.PromptTokS)
	}
	if got.GenerationTokS == nil || *got.GenerationTokS != 1820.1 {
		t.Fatalf("GenerationTokS = %v, want 1820.1 (rounded)", got.GenerationTokS)
	}
}

// TestDeploymentEventsInRangePagesUntilOutOfRange proves the pagination
// logic: it must keep paging while pages are full and still inside range,
// and stop as soon as it sees an entry older than sinceMS - without relying
// on a single flat page-size cap to happen to be big enough.
func TestDeploymentEventsInRangePagesUntilOutOfRange(t *testing.T) {
	// Three pages of eventPageSize+1 total entries, IDs descending
	// (newest first, matching store.DeploymentTimeline's own order), all
	// within range except the very last (oldest) one.
	total := eventPageSize*2 + 5
	all := make([]EventRef, total)
	for i := 0; i < total; i++ {
		all[i] = EventRef{ID: uint(total - i), Kind: "state", Data: `{"state":"running"}`, CreatedAtMS: int64(total - i)}
	}
	all[total-1].CreatedAtMS = -1 // the oldest entry falls before sinceMS=0

	var calls int
	svc := New(Deps{
		Now: func() time.Time { return baseTime },
		DeploymentEvents: func(_ context.Context, _ uint, before uint, limit int) ([]EventRef, error) {
			calls++
			start := 0
			if before != 0 {
				for i, e := range all {
					if e.ID == before {
						start = i + 1
						break
					}
				}
			}
			end := start + limit
			if end > len(all) {
				end = len(all)
			}
			if start >= len(all) {
				return nil, nil
			}
			return all[start:end], nil
		},
	})

	got, err := svc.deploymentEventsInRange(context.Background(), 1, 0)
	if err != nil {
		t.Fatalf("deploymentEventsInRange: %v", err)
	}
	if len(got) != total-1 {
		t.Fatalf("got %d events, want %d (every entry at/after sinceMS=0, excluding the one before it)", len(got), total-1)
	}
	if calls < 2 {
		t.Fatalf("calls = %d, want at least 2 pages read", calls)
	}
}

func TestDeploymentEventsInRangeStopsOnFirstPageWhenAllInRange(t *testing.T) {
	all := []EventRef{{ID: 3, CreatedAtMS: 300}, {ID: 2, CreatedAtMS: 200}, {ID: 1, CreatedAtMS: 100}}
	var calls int
	svc := New(Deps{
		DeploymentEvents: func(_ context.Context, _ uint, before uint, limit int) ([]EventRef, error) {
			calls++
			return all, nil
		},
	})
	got, err := svc.deploymentEventsInRange(context.Background(), 1, 0)
	if err != nil {
		t.Fatalf("deploymentEventsInRange: %v", err)
	}
	if len(got) != 3 || calls != 1 {
		t.Fatalf("got %d events in %d calls, want 3 events in 1 call (page smaller than eventPageSize stops the loop)", len(got), calls)
	}
}

// ---- Service-level integration (real tsdb, injected Inventory) ----

func newOverviewTestService(t *testing.T, inv monitor.Inventory, now time.Time) (*Service, tsdb.Store) {
	t.Helper()
	store := newTestStore(t)
	svc := New(Deps{
		TSDB:      store,
		Inventory: inventoryFunc(inv),
		Now:       func() time.Time { return now },
		NotFound:  errNotFound,
	})
	return svc, store
}

var errNotFound = errors.New("not found")

func TestServiceOverviewEndToEnd(t *testing.T) {
	inv := monitor.Inventory{
		Deployments: []monitor.DeploymentRef{{ID: 1, Name: "svc", Engine: "vllm", ReplicasDesired: 1}},
		Instances:   []monitor.InstanceRef{{UUID: "i1", DeploymentID: 1, State: proto.InstRunning}},
	}
	svc, store := newOverviewTestService(t, inv, baseTime)
	writeSample(t, store, "gpuworks_infer_requests_running", map[string]string{"instance": "i1", "deployment": "1"}, 4, baseTime)

	out, err := svc.Overview(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(out.Services) != 1 || out.Services[0].Running == nil || *out.Services[0].Running != 4 {
		t.Fatalf("services = %+v", out.Services)
	}
	// The deployment does not have the health check on.
	if h := out.Services[0].Health; h.Status != "off" || h.Recent == nil {
		t.Fatalf("health = %+v, want off with an empty recent list", h)
	}
}

// The services table carries each deployment's health check and puts the
// ones whose check failed or timed out first, then the ones with nothing to
// check, the healthy, and last the unchecked; ties go by name.
func TestServiceOverviewOrdersServicesByHealth(t *testing.T) {
	inv := monitor.Inventory{Deployments: []monitor.DeploymentRef{
		{ID: 1, Name: "a-healthy", HealthCheck: true, HealthCheckThresholdSec: 60},
		{ID: 2, Name: "b-unchecked"},
		{ID: 3, Name: "c-timeout", HealthCheck: true, HealthCheckThresholdSec: 30},
		{ID: 4, Name: "d-failed", HealthCheck: true, HealthCheckThresholdSec: 60},
		{ID: 5, Name: "e-failed", HealthCheck: true, HealthCheckThresholdSec: 60},
		{ID: 6, Name: "f-asleep", HealthCheck: true, HealthCheckThresholdSec: 60},
	}}
	found := map[uint]healthcheck.Health{
		1: {Status: healthcheck.StatusOK, Recent: []healthcheck.Probe{{AtMS: 1, Status: healthcheck.StatusOK, LatencyMS: 80, InstanceUUID: "i1"}}},
		3: {Status: healthcheck.StatusTimeout, Recent: []healthcheck.Probe{{AtMS: 1, Status: healthcheck.StatusTimeout, LatencyMS: 30000}}},
		4: {Status: healthcheck.StatusFailed},
		5: {Status: healthcheck.StatusFailed},
		6: {Status: healthcheck.StatusNoReplica},
	}
	svc := New(Deps{
		TSDB: newTestStore(t), Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }, NotFound: errNotFound,
		Health: func(id uint) healthcheck.Health { return found[id] },
	})
	out, err := svc.Overview(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	var order []string
	for _, row := range out.Services {
		order = append(order, row.Name+":"+row.Health.Status)
	}
	want := []string{"d-failed:failed", "e-failed:failed", "c-timeout:timeout", "f-asleep:no_replica", "a-healthy:ok", "b-unchecked:off"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("services = %v, want %v", order, want)
	}
	if h := out.Services[1+1].Health; h.ThresholdSec != 30 || h.Last == nil || h.Last.LatencyMS != 30000 {
		t.Fatalf("c-timeout health = %+v", h)
	}
	if h := out.Services[4].Health; h.RecentMeanMS == nil || *h.RecentMeanMS != 80 || h.Last.InstanceUUID != "i1" {
		t.Fatalf("a-healthy health = %+v", h)
	}
}

// TestServiceOverviewRequestRateThreeStatesAgainstRealTSDB is
// TestSignalsDTONoRecentRequestsThreeStates' end-to-end counterpart against
// a real tsdb.Store (rather than a hand-built monitor.ClusterSignals): it
// proves BuildCluster's own widened-range query - not just signalsDTO's
// pure reduction - produces the right collected/value/detail across
// request_rate's three states (never collected, collected+recent,
// collected+idle-window).
func TestServiceOverviewRequestRateThreeStatesAgainstRealTSDB(t *testing.T) {
	inv := monitor.Inventory{
		Deployments: []monitor.DeploymentRef{{ID: 1, Name: "svc", Engine: "vllm", ReplicasDesired: 1}},
		Instances:   []monitor.InstanceRef{{UUID: "i1", DeploymentID: 1, State: proto.InstRunning}},
	}

	for _, tc := range []struct {
		name      string
		sampleAge time.Duration // how long before "now" the sample sits; < 0 means "write nothing"
	}{
		{name: "never collected", sampleAge: -1},
		{name: "collected recent", sampleAge: 30 * time.Second},
		{name: "collected idle window", sampleAge: 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, store := newOverviewTestService(t, inv, baseTime)
			if tc.sampleAge >= 0 {
				at := baseTime.Add(-tc.sampleAge)
				labels := map[string]string{"deployment": "1", "class": "2xx"}
				writeSample(t, store, "gpuworks_proxy_requests_total", labels, 90, at.Add(-10*time.Second))
				writeSample(t, store, "gpuworks_proxy_requests_total", labels, 100, at)
			}

			out, err := svc.Overview(context.Background(), time.Hour)
			if err != nil {
				t.Fatalf("Overview: %v", err)
			}
			var reqRate dto.MonitorSignal
			for _, s := range out.Signals {
				if s.Key == "request_rate" {
					reqRate = s
				}
			}

			collected := tc.sampleAge >= 0
			recentInWindow := collected && tc.sampleAge <= monitor.DiagnosisWindow
			wantNoRecent := collected && !recentInWindow

			if reqRate.Collected != collected {
				t.Fatalf("request_rate.Collected = %v, want %v", reqRate.Collected, collected)
			}
			gotNoRecent, _ := reqRate.Detail["no_recent_requests"].(bool)
			if gotNoRecent != wantNoRecent {
				t.Fatalf("request_rate.Detail = %+v, want no_recent_requests=%v", reqRate.Detail, wantNoRecent)
			}
			switch {
			case recentInWindow:
				if reqRate.Value == nil || *reqRate.Value <= 0 {
					t.Fatalf("request_rate.Value = %v, want > 0 for recent traffic", reqRate.Value)
				}
			case wantNoRecent:
				if reqRate.Value == nil || *reqRate.Value != 0 {
					t.Fatalf("request_rate.Value = %v, want exactly 0 in the quiet-window state", reqRate.Value)
				}
			case !collected:
				if reqRate.Value != nil {
					t.Fatalf("request_rate.Value = %v, want nil when never collected", *reqRate.Value)
				}
			}

			if len(out.Services) != 1 {
				t.Fatalf("Services = %+v, want 1", out.Services)
			}
			row := out.Services[0]
			if collected {
				if row.RequestRate == nil {
					t.Fatalf("services[0].RequestRate = nil, want a non-null value (this deployment's own proxy series exist in range)")
				}
				if wantNoRecent && *row.RequestRate != 0 {
					t.Fatalf("services[0].RequestRate = %v, want exactly 0 in the quiet-window state", *row.RequestRate)
				}
			} else if row.RequestRate != nil {
				t.Fatalf("services[0].RequestRate = %v, want nil when never collected", *row.RequestRate)
			}
		})
	}
}

func TestServiceDeploymentNotFound(t *testing.T) {
	svc, _ := newOverviewTestService(t, monitor.Inventory{}, baseTime)
	_, err := svc.Deployment(context.Background(), 42, time.Hour)
	if !errors.Is(err, errNotFound) {
		t.Fatalf("err = %v, want the injected NotFound sentinel", err)
	}
}

// TestOverviewSurvivesLeaderCancellation proves that, at the Service level,
// the caller that ends up leading a shared computation must not cancel it
// for concurrent waiters (or itself) just because its own request context
// is canceled mid-flight - Overview's compute closure runs under
// context.WithoutCancel specifically so this cannot happen.
func TestOverviewSurvivesLeaderCancellation(t *testing.T) {
	inv := monitor.Inventory{Deployments: []monitor.DeploymentRef{{ID: 1, Name: "svc"}}}
	real := newTestStore(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	blocking := &blockingStore{Store: real, onFirstQuery: func() {
		once.Do(func() {
			close(started)
			<-release
		})
	}}
	svc := New(Deps{TSDB: blocking, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }})

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	type result struct {
		err error
	}
	leaderDone := make(chan result, 1)
	go func() {
		_, err := svc.Overview(leaderCtx, time.Hour)
		leaderDone <- result{err}
	}()

	<-started // the leader is now blocked inside its first tsdb query

	waiterDone := make(chan result, 1)
	go func() {
		_, err := svc.Overview(context.Background(), time.Hour)
		waiterDone <- result{err}
	}()

	// Give the waiter goroutine a moment to reach Cached.Get and observe the
	// leader's in-flight entry before we cancel the leader's own context.
	time.Sleep(50 * time.Millisecond)
	cancelLeader()
	close(release)

	leaderResult := <-leaderDone
	waiterResult := <-waiterDone
	if leaderResult.err != nil {
		t.Fatalf("leader (whose own ctx was canceled mid-flight) err = %v, want nil - compute must run under context.WithoutCancel", leaderResult.err)
	}
	if waiterResult.err != nil {
		t.Fatalf("waiter err = %v, want nil - the leader's cancellation must not propagate to it", waiterResult.err)
	}
}

// blockingStore wraps a real tsdb.Store and calls onFirstQuery before
// delegating to it - a hook point tests use to pause a computation
// mid-flight in a deterministic, race-free way.
type blockingStore struct {
	tsdb.Store
	onFirstQuery func()
}

func (b *blockingStore) QueryRange(metric string, matchers map[string]string, startMS, endMS, stepMS int64) ([]tsdb.Series, error) {
	if b.onFirstQuery != nil {
		b.onFirstQuery()
	}
	return b.Store.QueryRange(metric, matchers, startMS, endMS, stepMS)
}

// request_rate's denominator is the fleet's average requests in flight; it
// is absent when no deployment reports the gauge and a true zero for a
// quiet fleet.
func TestSignalsDTORequestRateCarriesInflight(t *testing.T) {
	find := func(out []dto.MonitorSignal) dto.MonitorSignal {
		for _, s := range out {
			if s.Key == "request_rate" {
				return s
			}
		}
		t.Fatal("no request_rate signal")
		return dto.MonitorSignal{}
	}
	got := find(signalsDTO(monitor.ClusterSignals{HasRequestRate: true, RequestsPerSec: 5.04, HasInflight: true, InflightAvg: 19.96}))
	if got.Value == nil || *got.Value != 5 || got.Denominator == nil || *got.Denominator != 20 {
		t.Fatalf("request_rate = %+v, want value 5 and denominator 20", got)
	}
	encoded, _ := json.Marshal(got)
	if !strings.Contains(string(encoded), `"value":5,"denominator":20`) {
		t.Fatalf("wire = %s", encoded)
	}
	if got := find(signalsDTO(monitor.ClusterSignals{HasRequestRate: true, RequestsPerSec: 3})); got.Denominator != nil {
		t.Fatalf("no in-flight gauge: denominator = %v, want absent", *got.Denominator)
	}
	if got := find(signalsDTO(monitor.ClusterSignals{HasRequestRate: true, ProxyNoRecent: true})); got.Denominator == nil || *got.Denominator != 0 {
		t.Fatalf("quiet fleet: denominator = %v, want 0", got.Denominator)
	}
	if got := find(signalsDTO(monitor.ClusterSignals{HasInflight: true, InflightAvg: 7})); got.Denominator != nil || got.Value != nil {
		t.Fatalf("uncollected request_rate = %+v, want no value or denominator", got)
	}
}
