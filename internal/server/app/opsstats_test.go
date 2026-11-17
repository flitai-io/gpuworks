package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/rollup"
	"gpuworks.ai/gpuworks/internal/server/statsdb"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

func newTestOpsStats(t *testing.T, now *time.Time) (*opsStats, *store.Store, *observability) {
	t.Helper()
	dir := t.TempDir()
	st := telemetryTestStore(t)
	obs, err := openObservability(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(obs.Close)
	ops, err := newOpsStats(dir, 72*time.Hour, st, obs, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ops.now = func() time.Time { return *now }
	t.Cleanup(func() { _ = ops.stats.Close() })
	return ops, st, obs
}

func finished(dep, key uint, endedAt time.Time, prompt, cached int32) uxrecord.Request {
	return uxrecord.Request{
		ReceivedMS: endedAt.Add(-5 * time.Second).UnixMilli(), DeploymentID: dep, APIKeyID: key, UserID: 1, Source: uxrecord.SourceAPI,
		Stream: true, Status: 200, Outcome: uxrecord.OutcomeOK, FinishReason: "stop",
		FirstThinkMS: uxrecord.Unknown, FirstAnswerMS: 400, LastChunkMS: 4900, EndMS: 5000,
		Usage: true, PromptTokens: prompt, CachedTokens: cached, CompletionTokens: 100, ReasoningTokens: uxrecord.Unknown,
	}
}

func usageRows(t *testing.T, st *store.Store) []store.UsageHourly {
	t.Helper()
	var rows []store.UsageHourly
	if err := st.DB().Order("hour_ms").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	return rows
}

// A first start seeds the data retention from the configuration and starts
// live accumulation; requests become hour rows on a flush and day rows once
// their day has ended; the setting changes the stores at once.
func TestOpsStatsAccumulatesFlushesRollsUpAndRetains(t *testing.T) {
	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, shanghai)
	ops, st, obs := newTestOpsStats(t, &now)
	ctx := context.Background()
	if r := ops.Retention(); r.MetricsHours != 72 || r.UsageDailyDays != 400 || r.TimeZone == "" {
		t.Fatalf("seeded retention = %+v", r)
	}
	r := ops.Retention()
	r.TimeZone = "Asia/Shanghai"
	if err := ops.SetRetention(r); err != nil {
		t.Fatal(err)
	}

	ops.observeRequest(finished(17, 3, now, 1000, 800))
	ops.observeRequest(finished(17, 3, now, 2000, -1))
	ops.observeRequest(finished(17, 0, now, 10, 0))
	if err := ops.flushUsage(ctx); err != nil {
		t.Fatal(err)
	}
	rows := usageRows(t, st)
	if len(rows) != 2 || rows[0].HourMS != rollup.HourMS(now) {
		t.Fatalf("hour rows = %+v", rows)
	}
	var key3 store.UsageHourly
	for _, row := range rows {
		if row.APIKeyID == 3 {
			key3 = row
		}
	}
	if key3.Requests != 2 || key3.PromptTokens != 3000 || key3.CachedTokens != 800 || key3.CacheReportedPrompt != 1000 {
		t.Fatalf("key 3 = %+v", key3)
	}
	// Nothing new: no write.
	if err := ops.flushUsage(ctx); err != nil || len(usageRows(t, st)) != 2 {
		t.Fatal("an empty flush wrote")
	}

	// The card samples of an ingest batch, not the instance or other series.
	ops.observeSamples([]tsdb.Sample{
		{Metric: "gpuworks_gpu_utilization", Labels: map[string]string{"node": "n1", "gpu": "0", "gpu_uuid": "GPU-1"}, Value: 50, TimeMS: now.UnixMilli()},
		{Metric: "gpuworks_gpu_utilization", Labels: map[string]string{"node": "n1", "gpu": "0", "gpu_uuid": "GPU-1", "instance": "i"}, Value: 99, TimeMS: now.UnixMilli()},
		{Metric: "gpuworks_gpu_utilization", Labels: map[string]string{"node": "n1", "gpu": "x"}, Value: 99, TimeMS: now.UnixMilli()},
		{Metric: "gpuworks_node_cpu_busy_percent", Labels: map[string]string{"node": "n1"}, Value: 99, TimeMS: now.UnixMilli()},
	})
	if err := ops.flushStats(ctx); err != nil {
		t.Fatal(err)
	}
	// The card's hour, and its node's and its model's.
	if h, _, err := ops.stats.RowCount(ctx, statsdb.KindGPU); err != nil || h != 3 {
		t.Fatalf("gpu rows = %d %v", h, err)
	}
	if h, _, _ := ops.stats.RowCount(ctx, statsdb.KindQuality); h != 1 {
		t.Fatalf("quality rows = %d", h)
	}
	// GPU utilization switched off: its samples are not counted.
	r.GPU = false
	if err := ops.SetRetention(r); err != nil {
		t.Fatal(err)
	}
	ops.observeSamples([]tsdb.Sample{{Metric: "gpuworks_gpu_power_watts", Labels: map[string]string{"node": "n2", "gpu": "0"}, Value: 300, TimeMS: now.UnixMilli()}})
	if len(ops.gpus.Take()) != 0 {
		t.Fatal("a switched-off statistic counted")
	}

	// The day has not ended: nothing rolls up. Past its end and the grace,
	// it does, once.
	ops.rollupDays(ctx)
	if rolled, _ := st.UsageDayRolled(ctx, 20261022); rolled {
		t.Fatal("rolled up an unfinished day")
	}
	now = time.Date(2026, 10, 23, 0, 6, 0, 0, shanghai)
	ops.rollupDays(ctx)
	if rolled, _ := st.UsageDayRolled(ctx, 20261022); !rolled {
		t.Fatal("yesterday did not roll up")
	}
	if rolled, _ := ops.stats.RolledDay(ctx, 20261022); !rolled {
		t.Fatal("yesterday's statistics did not roll up")
	}
	var days []store.UsageDaily
	if err := st.DB().Find(&days).Error; err != nil || len(days) != 2 {
		t.Fatalf("day rows = %+v %v", days, err)
	}
	if ops.rolledThrough != 20261022 {
		t.Fatalf("rolled through %d", ops.rolledThrough)
	}

	// The request log's bounds follow the setting at once.
	r.RequestLogDays, r.RequestLogMaxMB = 3, 1024
	if err := ops.SetRetention(r); err != nil {
		t.Fatal(err)
	}
	if age, bytes := obs.UX().Limits(); age != 3*24*time.Hour || bytes != 1024<<20 {
		t.Fatalf("request log limits = %v %d", age, bytes)
	}
	// An invalid value is refused and changes nothing.
	bad := r
	bad.UsageHourlyDays = 1
	var invalid *store.DataRetentionError
	if err := ops.SetRetention(bad); !errors.As(err, &invalid) || invalid.Field != "usage_hourly_days" || ops.Retention() != r {
		t.Fatalf("invalid setting = %v", err)
	}

	// Retention: 8 days later the hours are past 7 days; the days stay.
	r.UsageHourlyDays, r.StatsHourlyDays = 7, 7
	if err := ops.SetRetention(r); err != nil {
		t.Fatal(err)
	}
	now = now.AddDate(0, 0, 8)
	ops.retain(ctx)
	if left := usageRows(t, st); len(left) != 0 {
		t.Fatalf("hours left = %+v", left)
	}
	if err := st.DB().Find(&days).Error; err != nil || len(days) != 2 {
		t.Fatalf("days after retention = %d", len(days))
	}
	ops.mu.Lock()
	last := ops.last
	ops.mu.Unlock()
	if last.retainAt.IsZero() || last.retainErr != "" || last.flushRows == 0 {
		t.Fatalf("passes = %+v", last)
	}
}

// The card time of a minute: whole cards count whole, a dGPU its memory
// share, a card of an offline node or owned elsewhere not at all.
func TestOpsStatsSamplesCardTime(t *testing.T) {
	now := time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
	ops, st, _ := newTestOpsStats(t, &now)
	node, err := st.RegisterNode("n1", store.HashToken("n1"), "n1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Update("state", store.NodeStateOnline).Error; err != nil {
		t.Fatal(err)
	}
	for _, g := range []store.GPU{
		{NodeID: node.ID, Ident: "GPU-a", Name: "RTX 6000D", MemoryTotal: 84 << 30},
		{NodeID: node.ID, Ident: "GPU-b", Index: 1, Name: "RTX 6000D", MemoryTotal: 84 << 30},
		{NodeID: node.ID, Ident: "GPU-c", Index: 2, Name: "RTX 6000D", MemoryTotal: 84 << 30, Ownership: "passthrough"},
	} {
		g := g
		if err := st.DB().Create(&g).Error; err != nil {
			t.Fatal(err)
		}
	}
	whole, _ := st.CreateDeployment(&store.Deployment{Name: "whole", ServedName: "whole", State: store.DeployActive})
	dgpu, _ := st.CreateDeployment(&store.Deployment{Name: "dgpu", ServedName: "dgpu", State: store.DeployActive, VRAMLimitBytes: 21 << 30})
	for _, in := range []store.Instance{
		{UUID: "w", DeploymentID: whole.ID, NodeID: node.ID, State: "running", GPUIndexes: "[0]", GPUIdents: `["GPU-a"]`},
		{UUID: "d", DeploymentID: dgpu.ID, NodeID: node.ID, State: "running", GPUIndexes: "[1]", GPUIdents: `["GPU-b"]`},
	} {
		in := in
		if _, err := st.CreateInstance(&in); err != nil {
			t.Fatal(err)
		}
	}
	if err := ops.sampleGPUTime(now, time.Minute); err != nil {
		t.Fatal(err)
	}
	times, capacity := ops.gpuTimes.Take()
	hour := rollup.HourMS(now)
	if w := times[rollup.GPUTimeKey{HourMS: hour, DeploymentID: whole.ID, GPUModel: "RTX 6000D"}]; w.GPUSeconds != 60 || w.MaxInstances != 1 {
		t.Fatalf("whole = %+v", w)
	}
	if d := times[rollup.GPUTimeKey{HourMS: hour, DeploymentID: dgpu.ID, GPUModel: "RTX 6000D"}]; d.GPUSeconds != 15 || d.WholeGPUSeconds != 60 {
		t.Fatalf("dgpu = %+v", d)
	}
	if c := capacity[rollup.CapacityKey{HourMS: hour, GPUModel: "RTX 6000D"}]; c.TotalGPUSeconds != 120 || c.SchedulableGPUSeconds != 120 {
		t.Fatalf("capacity = %+v", c)
	}
	// Switched off: no read at all.
	r := ops.Retention()
	r.GPUTime = false
	if err := ops.SetRetention(r); err != nil {
		t.Fatal(err)
	}
	if err := ops.sampleGPUTime(now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if tt, _ := ops.gpuTimes.Take(); len(tt) != 0 {
		t.Fatal("a switched-off statistic sampled")
	}
}

// The fill out of the request log: requests that ended before live
// accumulation began, once; requests after it are left to live
// accumulation.
func TestOpsStatsBackfillsFromTheRequestLogOnce(t *testing.T) {
	now := time.Date(2026, 10, 22, 6, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	st := telemetryTestStore(t)
	obs, err := openObservability(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(obs.Close)
	// History the old release wrote: two days back and this morning.
	for _, at := range []time.Time{now.Add(-50 * time.Hour), now.Add(-2 * time.Hour), now.Add(-time.Hour)} {
		// ux.db weighs its own rows (1 unless it sampled); the weights
		// themselves are rollup's test.
		obs.UX().Observe(finished(17, 3, at, 1000, 500))
	}
	if err := obs.UX().Sync(); err != nil {
		t.Fatal(err)
	}
	ops, err := newOpsStats(dir, 72*time.Hour, st, obs, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ops.stats.Close() })
	ops.now = func() time.Time { return now }
	// A request the new release counts live, already in the log too.
	later := finished(17, 3, now.Add(time.Minute), 7, 0)
	obs.UX().Observe(later)
	_ = obs.UX().Sync()
	ctx := context.Background()
	ops.liveSinceMS = now.UnixMilli()
	ops.backfill(ctx)
	var total store.UsageCounts
	for _, row := range usageRows(t, st) {
		total.Requests += row.Requests
		total.PromptTokens += row.PromptTokens
	}
	if total.Requests != 3 || total.PromptTokens != 3000 {
		t.Fatalf("filled = %+v", total)
	}
	state, err := st.UsageRollupState(now)
	if err != nil || !state.BackfillDone {
		t.Fatalf("state = %+v %v", state, err)
	}
	if q, ok, _ := ops.stats.BackfillState(ctx, statsdb.KindQuality); !ok || !q.Done {
		t.Fatalf("quality fill = %+v", q)
	}
	// The finished days the fill touched are rolled up.
	if rolled, _ := st.UsageDayRolled(ctx, rollup.DayOf(now.Add(-50*time.Hour), ops.location())); !rolled {
		t.Fatal("a filled day was not rolled up")
	}
	// Again: nothing more.
	ops.backfill(ctx)
	var again int64
	for _, row := range usageRows(t, st) {
		again += row.Requests
	}
	if again != 3 {
		t.Fatalf("a second fill added: %d", again)
	}
}

// A card held outside the platform counts as schedulable capacity only when
// the scheduler may place a partition on it by memory (ADR 0068): the
// holders' memory known, the node's agent keeping them reported beside a
// partition, a partition backend ready on the card, and a node whose
// placement policy lets automatic placement share it. Counting any other
// held card would report capacity the scheduler never uses.
func TestOpsStatsCountsAHeldCardAsSchedulableOnlyWhenAPartitionMayShareIt(t *testing.T) {
	for _, tc := range []struct {
		name        string
		safety      int
		ready       bool
		allow       bool
		schedulable float64
	}{
		{"current agent with a ready backend", proto.PartitionSafetyForeignTracked, true, true, 60},
		{"node does not allow sharing", proto.PartitionSafetyForeignTracked, true, false, 0},
		{"agent before foreign tracking", 1, true, true, 0},
		{"no ready partition backend", proto.PartitionSafetyForeignTracked, false, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
			ops, st, _ := newTestOpsStats(t, &now)
			node, err := st.RegisterNode("n1", store.HashToken("n1"), "n1")
			if err != nil {
				t.Fatal(err)
			}
			updates := map[string]any{"state": store.NodeStateOnline, "gpu_vendor": "nvidia", "arch": "amd64", "os": "linux",
				"runtime_driver": "docker", "partition_safety_protocol": tc.safety, "placement_share_with_foreign": tc.allow}
			if tc.ready {
				index := 0
				probes, _ := json.Marshal([]proto.PartitionProbeResult{{Backend: accelerator.BackendNVIDIALogical, GPUIndex: &index, Status: proto.PartitionProbePassed}})
				assets, _ := json.Marshal([]proto.AssetReport{{Backend: accelerator.BackendNVIDIALogical, Root: "/assets", Files: []string{"libvgpu.so"}}})
				updates["partition_probes"], updates["partition_assets"] = string(probes), string(assets)
			}
			if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Updates(updates).Error; err != nil {
				t.Fatal(err)
			}
			held, _ := json.Marshal([]proto.CardProcess{{PID: 41235, Name: "python3", MemoryBytes: 62 << 30}})
			card := store.GPU{NodeID: node.ID, Index: 0, Ident: "GPU-h", UUID: "GPU-h", Name: "H20", Vendor: "nvidia", ArchID: "sm_90",
				ComputeCapability: "9.0", MemoryTotal: 140 << 30, ForeignProcesses: string(held)}
			if err := st.DB().Create(&card).Error; err != nil {
				t.Fatal(err)
			}
			if err := ops.sampleGPUTime(now, time.Minute); err != nil {
				t.Fatal(err)
			}
			_, capacity := ops.gpuTimes.Take()
			if c := capacity[rollup.CapacityKey{HourMS: rollup.HourMS(now), GPUModel: "H20"}]; c.TotalGPUSeconds != 60 || c.SchedulableGPUSeconds != tc.schedulable {
				t.Fatalf("capacity = %+v, want %v schedulable seconds", c, tc.schedulable)
			}
		})
	}
}
