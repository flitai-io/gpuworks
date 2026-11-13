package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

func TestDashboardCurrentMetricsUseExistingOnlineHardwareAndFreshSamples(t *testing.T) {
	for _, scenario := range []string{"empty", "offline", "stale", "partial", "zero"} {
		t.Run(scenario, func(t *testing.T) {
			st, err := store.Open(":memory:", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if scenario != "empty" {
				state := store.NodeStateOnline
				if scenario == "offline" {
					state = store.NodeStateOffline
				}
				n := store.Node{UUID: "live", State: state, GPUCount: 1, GPUs: []store.GPU{{Index: 3}}}
				if err := st.DB().Create(&n).Error; err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().UnixMilli()
			value := 12.0
			if scenario == "zero" {
				value = 0
			}
			stamp := now
			if scenario == "stale" {
				stamp -= int64(5 * time.Hour / time.Millisecond)
			}
			queries := 0
			h := apiHandlers{d: Deps{Store: st, MetricsLatest: func(metric string, matchers map[string]string, since int64, asOf ...int64) ([]MetricLatest, error) {
				queries++
				if len(asOf) != 1 || asOf[0]-since != int64(2*time.Minute/time.Millisecond) {
					t.Fatalf("current query bounds = %d, %v", since, asOf)
				}
				points := []MetricLatest{{Labels: map[string]string{"node": "live", "gpu": "3"}, TimeMS: stamp, Value: value}, {Labels: map[string]string{"node": "removed", "gpu": "3"}, TimeMS: now, Value: 97}}
				if metric != "gpuworks_node_mem_used_bytes" {
					points = append(points, MetricLatest{Labels: map[string]string{"node": "live", "gpu": "7"}, TimeMS: now, Value: 99})
				}
				out := []MetricLatest{}
				for _, p := range points {
					if p.TimeMS >= since && p.TimeMS <= asOf[0] {
						out = append(out, p)
					}
				}
				return out, nil
			}}}
			rec := httptest.NewRecorder()
			h.dashboardSummary(rec, httptest.NewRequest(http.MethodGet, "/api/v1/dashboard/summary", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
				t.Fatal(err)
			}
			var metrics map[string]*float64
			if err := json.Unmarshal(wire["current_metrics"], &metrics); err != nil {
				t.Fatalf("current_metrics missing or invalid: %s", rec.Body.String())
			}
			for _, key := range []string{"utilization_pct", "temperature_c", "power_watts", "memory_used_bytes", "vram_used_bytes", "vram_total_bytes"} {
				got, present := metrics[key]
				if !present {
					t.Fatalf("missing field %s", key)
				}
				if scenario == "partial" || scenario == "zero" {
					if got == nil || *got != value {
						t.Errorf("%s = %v, want %v", key, got, value)
					}
				} else if got != nil {
					t.Errorf("%s = %v, want no measurement", key, *got)
				}
			}
			if queries > 6 {
				t.Fatalf("queries %d scale with hardware", queries)
			}
		})
	}
}

func TestDashboardCurrentMetricsDeduplicateResourcesAndRejectInvalidSamples(t *testing.T) {
	now := time.Now().UnixMilli()
	nodes := []store.Node{
		{UUID: "live", State: store.NodeStateOnline, GPUs: []store.GPU{{Index: 3}, {Index: 4}, {Index: 5}, {Index: 6}}},
		{UUID: "cpu-only", State: store.NodeStateOnline},
		{UUID: "offline", State: store.NodeStateOffline, GPUs: []store.GPU{{Index: 3}}},
	}
	h := apiHandlers{d: Deps{MetricsLatest: func(metric string, _ map[string]string, _ int64, _ ...int64) ([]MetricLatest, error) {
		point := func(node, gpu string, stamp int64, value float64) MetricLatest {
			return MetricLatest{Labels: map[string]string{"node": node, "gpu": gpu}, TimeMS: stamp, Value: value}
		}
		if metric == "gpuworks_node_mem_used_bytes" {
			return []MetricLatest{point("live", "", now, 10), point("cpu-only", "", now, 20), point("offline", "", now, 99)}, nil
		}
		// Latest returns one observation per label set, including old hardware
		// labels for the same GPU. Never return multiple samples of one series.
		oldLabels := point("live", "3", now-1000, 99)
		oldLabels.Labels["gpu_uuid"] = "replaced-card"
		instance := point("live", "3", now, 99)
		instance.Labels["instance"] = "serving"
		return []MetricLatest{oldLabels, instance, point("live", "3", now, 10), point("live", "4", now, 20), point("live", "5", now, math.NaN()), point("live", "6", now, math.Inf(1)), point("live", "7", now, 99), point("offline", "3", now, 99), point("removed", "3", now, 99)}, nil
	}}}
	got := h.dashboardCurrentMetrics(nodes)
	for key, pair := range map[string]struct {
		got  *float64
		want float64
	}{
		"utilization": {got.UtilizationPct, 15}, "temperature": {got.TemperatureC, 20}, "power": {got.PowerWatts, 30}, "memory": {got.MemoryUsedBytes, 30},
	} {
		if pair.got == nil || *pair.got != pair.want {
			t.Errorf("%s = %v, want %v", key, pair.got, pair.want)
		}
	}
}

func TestDashboardCurrentMetricsKeepCurrentSampleBehindFutureSamples(t *testing.T) {
	db, err := tsdb.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	labels := map[string]string{"node": "live", "gpu": "3", "gpu_uuid": "gpu-live", "vendor": "nvidia"}
	now := time.Now().UnixMilli()
	var cutoff int64
	calls := 0
	h := apiHandlers{d: Deps{MetricsLatest: func(metric string, matchers map[string]string, since int64, asOf ...int64) ([]MetricLatest, error) {
		calls++
		if len(asOf) != 1 {
			t.Fatalf("missing upper bound: %v", asOf)
		}
		if calls == 1 {
			cutoff = asOf[0]
		}
		if asOf[0] != cutoff {
			t.Fatalf("snapshot cutoff changed from %d to %d", cutoff, asOf[0])
		}
		// Arrive after the dashboard captures its cutoff, before each SQLite read.
		// The next millisecond sample models ordinary concurrent ingestion; the
		// far-future sample models agent clock skew on this exact label set.
		if err := db.WriteBatch([]tsdb.Sample{
			{Metric: metric, Labels: labels, TimeMS: now - 1000, Value: 20},
			{Metric: metric, Labels: labels, TimeMS: cutoff + 1, Value: 98},
			{Metric: metric, Labels: labels, TimeMS: cutoff + 60_000, Value: 99},
		}); err != nil {
			t.Fatal(err)
		}
		rows, err := db.QueryLatest(metric, matchers, since, asOf...)
		out := []MetricLatest{}
		for _, row := range rows {
			out = append(out, MetricLatest{Labels: row.Labels, TimeMS: row.TimeMS, Value: row.Value})
		}
		return out, err
	}}}
	got := h.dashboardCurrentMetrics([]store.Node{{UUID: "live", State: store.NodeStateOnline, GPUs: []store.GPU{{Index: 3}}}})
	for _, value := range []*float64{got.UtilizationPct, got.TemperatureC, got.PowerWatts, got.MemoryUsedBytes, got.VRAMUsedBytes, got.VRAMTotalBytes} {
		if value == nil || *value != 20 {
			t.Fatalf("valid current sample lost behind later arrivals: %+v", got)
		}
	}
	if calls != 6 {
		t.Fatalf("query count = %d, want 6", calls)
	}
}

func TestDashboardSummaryRetainsInventoryWhenMetricsFail(t *testing.T) {
	for _, all := range []bool{false, true} {
		name := "temperature"
		if all {
			name = "all"
		}
		t.Run(name, func(t *testing.T) {
			n := fleetFixture(t)
			model, err := n.store.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/m", Revision: "main", Name: "org/m"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := n.store.CreateDeployment(&store.Deployment{Name: "serving", ModelID: model.ID, Replicas: 1, Sleepable: true}); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			h := apiHandlers{d: Deps{Store: n.store, Log: slog.New(slog.NewJSONHandler(&logs, nil))}}
			read := func() dto.DashboardSummary {
				t.Helper()
				rec := httptest.NewRecorder()
				h.dashboardSummary(rec, httptest.NewRequest(http.MethodGet, "/api/v1/dashboard/summary", nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("healthy inventory status=%d body=%s", rec.Code, rec.Body.String())
				}
				var got dto.DashboardSummary
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				return got
			}
			before := read()
			calls := 0
			h.d.MetricsLatest = func(metric string, _ map[string]string, _ int64, asOf ...int64) ([]MetricLatest, error) {
				calls++
				if all || metric == "gpuworks_gpu_temperature_celsius" {
					return nil, errors.New("metric storage unavailable")
				}
				return []MetricLatest{{Labels: map[string]string{"node": "node-a", "gpu": "0"}, TimeMS: asOf[0], Value: 20}}, nil
			}
			got := read()
			if got.Nodes != before.Nodes || got.Deployments != before.Deployments || got.Deployments.Total != 1 || !reflect.DeepEqual(got.ByVendor, before.ByVendor) || !reflect.DeepEqual(got.ByLabel, before.ByLabel) {
				t.Fatalf("metrics failure changed inventory: before=%+v after=%+v", before, got)
			}
			if got.CurrentMetrics.TemperatureC != nil {
				t.Fatal("failed temperature must be null")
			}
			for _, value := range []*float64{got.CurrentMetrics.UtilizationPct, got.CurrentMetrics.PowerWatts, got.CurrentMetrics.MemoryUsedBytes, got.CurrentMetrics.VRAMUsedBytes, got.CurrentMetrics.VRAMTotalBytes} {
				if all && value != nil || !all && (value == nil || *value != 20) {
					t.Fatalf("incorrect partial metrics: %+v", got.CurrentMetrics)
				}
			}
			wantErrors := 1
			if all {
				wantErrors = 6
			}
			if calls != 6 || strings.Count(logs.String(), "metric storage unavailable") != wantErrors {
				t.Fatalf("calls=%d logs=%s", calls, logs.String())
			}
			// Inventory keeps updating even while metrics remain unavailable.
			if err := n.store.DB().Create(&store.Node{UUID: "new-node", State: store.NodeStateOnline}).Error; err != nil {
				t.Fatal(err)
			}
			if updated := read(); updated.Nodes.Total != before.Nodes.Total+1 || updated.Nodes.Online != before.Nodes.Online+1 {
				t.Fatalf("inventory froze: %+v", updated.Nodes)
			}
		})
	}
}

// VRAM used and total are summed over the same cards: a card with only one
// of the two samples is left out of both, so the ratio stays a real one.
func TestDashboardCurrentMetricsPairVRAMUsedAndTotalPerCard(t *testing.T) {
	now := time.Now().UnixMilli()
	nodes := []store.Node{{UUID: "live", State: store.NodeStateOnline, GPUs: []store.GPU{{Index: 0}, {Index: 1}, {Index: 2}}}}
	point := func(gpu string, value float64) MetricLatest {
		return MetricLatest{Labels: map[string]string{"node": "live", "gpu": gpu}, TimeMS: now, Value: value}
	}
	h := apiHandlers{d: Deps{MetricsLatest: func(metric string, _ map[string]string, _ int64, _ ...int64) ([]MetricLatest, error) {
		switch metric {
		case "gpuworks_gpu_memory_used_bytes":
			// Card 2 has a used sample but no total one.
			return []MetricLatest{point("0", 10), point("1", 20), point("2", 70)}, nil
		case "gpuworks_gpu_memory_total_bytes":
			return []MetricLatest{point("0", 40), point("1", 60)}, nil
		}
		return nil, nil
	}}}
	got := h.dashboardCurrentMetrics(nodes)
	if got.VRAMUsedBytes == nil || got.VRAMTotalBytes == nil || *got.VRAMUsedBytes != 30 || *got.VRAMTotalBytes != 100 {
		t.Fatalf("vram used/total = %v/%v, want 30/100 over cards 0 and 1", got.VRAMUsedBytes, got.VRAMTotalBytes)
	}
	none := (&apiHandlers{d: Deps{MetricsLatest: func(metric string, _ map[string]string, _ int64, _ ...int64) ([]MetricLatest, error) {
		if metric == "gpuworks_gpu_memory_used_bytes" {
			return []MetricLatest{point("0", 10)}, nil
		}
		if metric == "gpuworks_gpu_memory_total_bytes" {
			return []MetricLatest{point("1", 40)}, nil
		}
		return nil, nil
	}}}).dashboardCurrentMetrics(nodes)
	if none.VRAMUsedBytes != nil || none.VRAMTotalBytes != nil {
		t.Fatalf("no card with both samples: vram = %v/%v, want null", none.VRAMUsedBytes, none.VRAMTotalBytes)
	}
}
