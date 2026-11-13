package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// metricsFor answers MetricsLatest as the metrics store does for the host
// series of one node: only that node's rows, only samples at or after since.
func metricsFor(nodeUUID string, totalBytes, usedBytes float64) func(string, map[string]string, int64, ...int64) ([]MetricLatest, error) {
	return func(metric string, matchers map[string]string, sinceMS int64, _ ...int64) ([]MetricLatest, error) {
		if matchers["node"] != nodeUUID {
			return nil, nil
		}
		var value float64
		switch metric {
		case "gpuworks_node_mem_total_bytes":
			value = totalBytes
		case "gpuworks_node_mem_used_bytes":
			value = usedBytes
		default:
			return nil, nil
		}
		now := time.Now().UnixMilli()
		if now < sinceMS {
			return nil, nil
		}
		return []MetricLatest{{Labels: map[string]string{"node": nodeUUID}, Value: value, TimeMS: now}}, nil
	}
}

func jsonOfUint(id uint) string {
	raw, _ := json.Marshal(id)
	return string(raw)
}

func errorBody(t *testing.T, body string) (code string, params map[string]any) {
	t.Helper()
	var out struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	return out.Code, out.Params
}

func TestWakeRefusedWhileAProcessOutsideHoldsTheCard(t *testing.T) {
	// The card is free of co-tenants; only the foreign process holds it.
	f := newSleepFixture(t)
	sleeper := f.deployment(t, &store.Deployment{Name: "sleeper", ServedName: "sleeper", Engine: "vllm", Sleepable: true, DesiredSleep: 1})
	f.instance(t, sleeper, "sleeper-1", 1)
	wf := wakeWaitFixture{sleepFixture: f, sleeper: sleeper}
	wf.holdOutside(t)

	check := func(t *testing.T, status int, body string) {
		t.Helper()
		code, params := errorBody(t, body)
		if status != http.StatusConflict || code != "WAKE_GPU_HELD_OUTSIDE" {
			t.Fatalf("%d %s, want 409 WAKE_GPU_HELD_OUTSIDE", status, body)
		}
		refused, _ := params["refused"].([]any)
		if len(refused) != 1 {
			t.Fatalf("refused = %v", params["refused"])
		}
		first, _ := refused[0].(map[string]any)
		if first["held_outside"] != true || first["node"] != "sleep-node" {
			t.Fatalf("refused[0] = %v", first)
		}
		if got, _ := json.Marshal(first["instances"]); string(got) != `["sleeper-1"]` {
			t.Fatalf("refused[0].instances = %s", got)
		}
		if got, _ := json.Marshal(first["gpu_indexes"]); string(got) != `[0]` {
			t.Fatalf("refused[0].gpu_indexes = %s", got)
		}
		// How much the foreign process takes is unknown, so no shortfall is
		// claimed, and the shortfall rows say the card is held.
		if first["shortfall_gib"] != "0.0" {
			t.Fatalf("shortfall_gib = %v, want 0.0 for an unknowable shortfall", first["shortfall_gib"])
		}
		shortfalls, _ := params["shortfalls"].([]any)
		if len(shortfalls) != 1 || shortfalls[0].(map[string]any)["held_outside"] != true {
			t.Fatalf("shortfalls = %v", params["shortfalls"])
		}
	}
	t.Run("deployment", func(t *testing.T) {
		status, body := wf.deploymentCall(t, sleeper.ID, "")
		check(t, status, body)
		// ?partial=true wakes what fits; nothing does, so it is the same refusal.
		status, body = wf.deploymentCall(t, sleeper.ID, "?partial=true")
		check(t, status, body)
	})
	t.Run("instance", func(t *testing.T) {
		rec := instanceRequest(f.h.wakeInstance, "sleeper-1", "")
		check(t, rec.Code, strings.TrimSpace(rec.Body.String()))
	})
	t.Run("waiting is still allowed", func(t *testing.T) {
		status, body := wf.deploymentCall(t, sleeper.ID, "?wait=true")
		if want := `{"status":"waiting","woken":[],"waiting":["sleeper-1"],"yielded":[],"yielded_held":[]}`; status != http.StatusAccepted || body != want {
			t.Fatalf("%d %s", status, body)
		}
	})
	if sleepIntent(t, f.st, "sleeper-1") != 1 {
		t.Fatal("a refused wake woke the sleeper")
	}
}

func TestSleepL1RefusedWhenHostMemoryCannotHoldTheWeights(t *testing.T) {
	const gib = 1 << 30
	newFixture := func(t *testing.T, latest func(string, map[string]string, int64, ...int64) ([]MetricLatest, error)) (sleepFixture, *store.Deployment) {
		t.Helper()
		f := newSleepFixture(t)
		model, err := f.st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/weights", Revision: "main", Name: "weights", WeightBytes: 14 * gib})
		if err != nil || model.ID != 1 {
			t.Fatalf("model = %+v %v (the fixture's deployments point at model 1)", model, err)
		}
		f.h.d.MetricsLatest = latest
		dep := f.deployment(t, &store.Deployment{Name: "s", ServedName: "s", Engine: "vllm", Sleepable: true})
		f.instance(t, dep, "inst-1", 0)
		return f, dep
	}

	t.Run("deployment L1 is refused, L2 is not", func(t *testing.T) {
		f, dep := newFixture(t, nil)
		f.h.d.MetricsLatest = metricsFor(f.node.UUID, 64*gib, 60*gib)
		for _, query := range []string{"?level=1", ""} {
			status, body := f.call(t, f.h.sleepDeployment, "/api/v1/deployments/x/sleep"+query, dep.ID)
			if status != http.StatusConflict || body["code"] != "SLEEP_HOST_MEMORY_INSUFFICIENT" {
				t.Fatalf("sleep%s = %d %v, want 409 SLEEP_HOST_MEMORY_INSUFFICIENT", query, status, body)
			}
			params := body["params"].(map[string]any)
			if params["node"] != "sleep-node" || params["need_gib"] != "14.0" || params["available_gib"] != "4.0" {
				t.Fatalf("params = %v", params)
			}
			if got, _ := f.st.GetDeployment(dep.ID); got.DesiredSleep != 0 || sleepIntent(t, f.st, "inst-1") != 0 {
				t.Fatal("a refused L1 sleep changed the order")
			}
		}
		status, body := f.call(t, f.h.sleepDeployment, "/api/v1/deployments/x/sleep?level=2", dep.ID)
		if status != http.StatusOK || body["status"] != "sleeping" || body["level"] != float64(2) {
			t.Fatalf("L2 sleep = %d %v", status, body)
		}
		if got, _ := f.st.GetDeployment(dep.ID); got.DesiredSleep != 2 {
			t.Fatalf("desired level = %d", got.DesiredSleep)
		}
	})

	t.Run("enough host memory allows L1", func(t *testing.T) {
		f, dep := newFixture(t, nil)
		f.h.d.MetricsLatest = metricsFor(f.node.UUID, 64*gib, 10*gib)
		status, body := f.call(t, f.h.sleepDeployment, "/api/v1/deployments/x/sleep?level=1", dep.ID)
		if status != http.StatusOK || body["level"] != float64(1) {
			t.Fatalf("L1 sleep = %d %v", status, body)
		}
	})

	t.Run("no observation does not refuse", func(t *testing.T) {
		f, dep := newFixture(t, func(string, map[string]string, int64, ...int64) ([]MetricLatest, error) { return nil, nil })
		if status, body := f.call(t, f.h.sleepDeployment, "/api/v1/deployments/x/sleep?level=1", dep.ID); status != http.StatusOK {
			t.Fatalf("no metric rows: %d %v", status, body)
		}
	})

	t.Run("another node's observation does not count", func(t *testing.T) {
		f, dep := newFixture(t, nil)
		f.h.d.MetricsLatest = metricsFor("some-other-node", 64*gib, 63*gib)
		if status, body := f.call(t, f.h.sleepDeployment, "/api/v1/deployments/x/sleep?level=1", dep.ID); status != http.StatusOK {
			t.Fatalf("%d %v", status, body)
		}
	})

	t.Run("a stale observation does not count", func(t *testing.T) {
		f, dep := newFixture(t, nil)
		fresh := metricsFor(f.node.UUID, 64*gib, 63*gib)
		f.h.d.MetricsLatest = func(metric string, matchers map[string]string, since int64, asOf ...int64) ([]MetricLatest, error) {
			// The samples are ten minutes old; the store applies the cutoff.
			if time.Now().Add(-10*time.Minute).UnixMilli() < since {
				return nil, nil
			}
			return fresh(metric, matchers, since, asOf...)
		}
		if status, body := f.call(t, f.h.sleepDeployment, "/api/v1/deployments/x/sleep?level=1", dep.ID); status != http.StatusOK {
			t.Fatalf("%d %v", status, body)
		}
	})

	t.Run("a model with unknown weights does not refuse", func(t *testing.T) {
		f, dep := newFixture(t, nil)
		f.h.d.MetricsLatest = metricsFor(f.node.UUID, 64*gib, 63*gib)
		if err := f.st.DB().Model(&store.Model{}).Where("id = 1").Update("weight_bytes", 0).Error; err != nil {
			t.Fatal(err)
		}
		if status, body := f.call(t, f.h.sleepDeployment, "/api/v1/deployments/x/sleep?level=1", dep.ID); status != http.StatusOK {
			t.Fatalf("%d %v", status, body)
		}
	})

	t.Run("an instance's L1 sleep is checked too", func(t *testing.T) {
		f, _ := newFixture(t, nil)
		f.h.d.MetricsLatest = metricsFor(f.node.UUID, 64*gib, 60*gib)
		rec := instanceRequest(f.h.sleepInstance, "inst-1", "?level=1")
		if code, _ := errorBody(t, rec.Body.String()); rec.Code != http.StatusConflict || code != "SLEEP_HOST_MEMORY_INSUFFICIENT" {
			t.Fatalf("instance L1 = %d %s", rec.Code, rec.Body)
		}
		if sleepIntent(t, f.st, "inst-1") != 0 {
			t.Fatal("a refused sleep changed the intent")
		}
		if rec = instanceRequest(f.h.sleepInstance, "inst-1", "?level=2"); rec.Code != http.StatusOK {
			t.Fatalf("instance L2 = %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("a yielding wake gives way at L2 when the host cannot hold the weights", func(t *testing.T) {
		f := newSleepFixture(t)
		if _, err := f.st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/weights", Revision: "main", Name: "weights", WeightBytes: 14 * gib}); err != nil {
			t.Fatal(err)
		}
		f.h.d.MetricsLatest = metricsFor(f.node.UUID, 64*gib, 60*gib)
		sleeper := f.deployment(t, &store.Deployment{Name: "sleeper", ServedName: "sleeper", Engine: "vllm", Sleepable: true, DesiredSleep: 2})
		f.instance(t, sleeper, "sleeper-1", 2)
		newcomer := f.deployment(t, &store.Deployment{Name: "newcomer", ServedName: "newcomer", Engine: "vllm", Sleepable: true})
		f.instance(t, newcomer, "newcomer-1", 0)
		wf := wakeWaitFixture{sleepFixture: f, sleeper: sleeper, newcomer: newcomer}
		status, body := wf.deploymentCall(t, sleeper.ID, "?yield=true")
		if want := `{"status":"waiting","woken":[],"waiting":["sleeper-1"],"yielded":["newcomer"],"yielded_held":["newcomer"]}`; status != http.StatusAccepted || body != want {
			t.Fatalf("%d %s", status, body)
		}
		if got := sleepIntent(t, f.st, "newcomer-1"); got != 2 {
			t.Fatalf("co-tenant's level = %d, want 2: L1 does not fit the host", got)
		}
	})
}
