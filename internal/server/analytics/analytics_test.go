package analytics

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/rollup"
	"gpuworks.ai/gpuworks/internal/server/statsdb"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

type fixture struct {
	svc   *Service
	st    *store.Store
	stats *statsdb.Store
	loc   *time.Location
	now   time.Time
	dep   uint
	key   uint
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	stats, err := statsdb.Open(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stats.Close() })
	loc, _ := time.LoadLocation("Asia/Shanghai")
	retention := store.DefaultDataRetention(72*time.Hour, "Asia/Shanghai")
	f := &fixture{st: st, stats: stats, loc: loc, now: time.Date(2026, 10, 22, 22, 0, 0, 0, loc)}
	f.svc = &Service{Store: st, Stats: stats, Retention: func() store.DataRetention { return retention },
		LiveSinceMS: time.Date(2026, 10, 21, 0, 0, 0, 0, loc).UnixMilli(), Now: func() time.Time { return f.now }}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "glm", ServedName: "nvidia/GLM", State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	f.dep = dep.ID
	if _, err := st.CreateAPIKey("customer", 0); err != nil {
		t.Fatal(err)
	}
	keys, _ := st.ListAPIKeys()
	f.key = keys[0].ID
	return f
}

func (f *fixture) at(day, hour int) int64 {
	return time.Date(2026, 10, day, hour, 0, 0, 0, f.loc).UnixMilli()
}

// Yesterday is rolled up, today is not: a day range reads yesterday's day row
// and today's hours, an hour range the hours.
func TestUsageAcrossRolledAndUnrolledDays(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := rollup.Usage{Requests: 1, PromptTokens: 1000, CompletionTokens: 10, CachedTokens: 900, CacheReported: 1, CacheReportedPrompt: 1000}
	key := func(ms int64, keyID uint, src uxrecord.Source) rollup.UsageKey {
		return rollup.UsageKey{HourMS: ms, APIKeyID: keyID, UserID: 1, DeploymentID: f.dep, Source: src}
	}
	if err := f.st.AddUsage(ctx, map[rollup.UsageKey]rollup.Usage{
		key(f.at(21, 9), f.key, uxrecord.SourceAPI):    u,
		key(f.at(21, 10), f.key, uxrecord.SourceAPI):   u,
		key(f.at(22, 9), f.key, uxrecord.SourceAPI):    u,
		key(f.at(22, 9), 0, uxrecord.SourcePlayground): u,
		key(f.at(22, 10), 9999, uxrecord.SourceAPI):    u, // a key deleted before keys were kept
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.RollupUsageDay(ctx, 20261021, f.loc); err != nil {
		t.Fatal(err)
	}
	days, err := f.svc.Usage(ctx, Query{FromMS: f.at(20, 0), ToMS: f.at(23, 0)}, UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if days.Range.Granularity != Day || len(days.Range.Buckets) != 3 || days.Range.Buckets[1] != f.at(21, 0) {
		t.Fatalf("range = %+v", days.Range)
	}
	if days.Series[0].Requests != 0 || days.Series[1].Requests != 2 || days.Series[2].Requests != 3 || days.Totals.PromptTokens != 5000 {
		t.Fatalf("series = %+v totals %+v", days.Series, days.Totals)
	}
	var customer, playground, deleted int
	for _, r := range days.Rows {
		switch {
		case r.APIKeyID == f.key:
			customer++
			if r.APIKeyName != "customer" || r.Requests != 3 || r.FirstMS != f.at(21, 0) || r.LastMS != f.at(22, 0) || r.ServedName != "nvidia/GLM" || r.Source != "api" {
				t.Fatalf("customer row = %+v", r)
			}
		case r.Source == "playground":
			playground++
		case r.APIKeyDeleted:
			deleted++
		}
	}
	if customer != 1 || playground != 1 || deleted != 1 {
		t.Fatalf("rows = %+v", days.Rows)
	}
	hours, err := f.svc.Usage(ctx, Query{FromMS: f.at(22, 0), ToMS: f.at(23, 0), Granularity: Hour}, UsageFilter{APIKeyID: f.key})
	if err != nil || len(hours.Series) != 24 || hours.Series[9].Requests != 1 || hours.Totals.Requests != 1 {
		t.Fatalf("hours = %+v %v", hours.Series, err)
	}
	// Revoked keys keep their name and say so.
	keys, _ := f.st.ListAPIKeys()
	if err := f.st.RevokeAPIKey(keys[0].Prefix); err != nil {
		t.Fatal(err)
	}
	again, _ := f.svc.Usage(ctx, Query{FromMS: f.at(22, 0), ToMS: f.at(23, 0)}, UsageFilter{APIKeyID: f.key})
	if len(again.Rows) != 1 || !again.Rows[0].APIKeyRevoked || again.Rows[0].APIKeyName != "customer" {
		t.Fatalf("revoked = %+v", again.Rows)
	}
	keyUsage, err := f.svc.KeyUsage(ctx)
	if err != nil || len(keyUsage.Items) != 1 || keyUsage.Items[0].Today.Requests != 1 || keyUsage.Items[0].Last7Days.Requests != 3 {
		t.Fatalf("key usage = %+v %v", keyUsage, err)
	}
}

func TestQueryRangesAreChecked(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		q    Query
		code string
	}{
		{Query{FromMS: 0, ToMS: 1}, messages.ErrorAnalyticsRangeInvalid},
		{Query{FromMS: f.at(22, 0), ToMS: f.at(21, 0)}, messages.ErrorAnalyticsRangeInvalid},
		{Query{FromMS: f.at(1, 0), ToMS: f.at(22, 0), Granularity: Hour}, messages.ErrorAnalyticsRangeTooLong},
		{Query{FromMS: f.at(21, 0), ToMS: f.at(22, 0), Granularity: "week"}, messages.ErrorAnalyticsValueInvalid},
	} {
		var qe *messages.ProductError
		if _, err := f.svc.Usage(ctx, tc.q, UsageFilter{}); !errors.As(err, &qe) || qe.Code() != tc.code || qe.Params()["field"] == "" {
			t.Errorf("%+v: %v", tc.q, err)
		}
	}
	// No granularity: hours up to two days, days beyond.
	if r, _ := f.svc.Usage(ctx, Query{FromMS: f.at(21, 0), ToMS: f.at(23, 0)}, UsageFilter{}); r.Range.Granularity != Hour || len(r.Range.Buckets) != 48 {
		t.Fatalf("2 days = %s %d", r.Range.Granularity, len(r.Range.Buckets))
	}
	if r, _ := f.svc.Usage(ctx, Query{FromMS: f.at(1, 0), ToMS: f.at(23, 0)}, UsageFilter{}); r.Range.Granularity != Day || len(r.Range.Buckets) != 22 {
		t.Fatalf("22 days = %s %d", r.Range.Granularity, len(r.Range.Buckets))
	}
	for _, bad := range []GPUQuery{{Query: Query{FromMS: f.at(21, 0), ToMS: f.at(22, 0)}, GroupBy: "rack"}, {Query: Query{FromMS: f.at(21, 0), ToMS: f.at(22, 0)}, Metric: "fan"}} {
		var qe *messages.ProductError
		if _, err := f.svc.GPUs(ctx, bad); !errors.As(err, &qe) || qe.Code() != messages.ErrorAnalyticsValueInvalid {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

func TestQualityGPUAndCardTime(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var q rollup.Quality
	q.Requests, q.Completed, q.Error = 10, 9, 1
	for i := 0; i < 9; i++ {
		q.TTFT.Observe(300)
		q.EndToEnd.Observe(5000)
	}
	q.OutputTokens, q.DecodeMS = 1000, 20000
	if err := f.stats.AddQuality(ctx, map[rollup.QualityKey]rollup.Quality{
		{HourMS: f.at(21, 9), DeploymentID: f.dep}: q, {HourMS: f.at(22, 9), DeploymentID: f.dep}: q,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.stats.RollupDay(ctx, 20261021, f.loc); err != nil {
		t.Fatal(err)
	}
	quality, err := f.svc.Quality(ctx, Query{FromMS: f.at(21, 0), ToMS: f.at(23, 0), Granularity: Day}, 0)
	if err != nil || len(quality.Series) != 2 || quality.Series[0].Requests != 10 || quality.Series[1].Requests != 10 {
		t.Fatalf("quality = %+v %v", quality.Series, err)
	}
	if quality.Totals.TTFTP50MS == nil || *quality.Totals.TTFTP50MS < 270 || *quality.Totals.TTFTP50MS > 330 ||
		quality.Totals.OutputTokensPerSec == nil || *quality.Totals.OutputTokensPerSec != 50 {
		t.Fatalf("totals = %+v", quality.Totals)
	}
	if len(quality.Rows) != 1 || quality.Rows[0].DeploymentName != "glm" || quality.Rows[0].Error != 2 {
		t.Fatalf("rows = %+v", quality.Rows)
	}
	// Rows from before classes count under the empty class.
	if len(quality.Classes) != 1 || quality.Classes[0].PriorityClass != "" || quality.Classes[0].Requests != 20 {
		t.Fatalf("classes = %+v", quality.Classes)
	}
	// A class's own rows split out, most requests first, while the
	// deployment's row still adds them all up.
	var low rollup.Quality
	low.Requests, low.Completed = 30, 30
	low.TTFT.Observe(9000)
	if err := f.stats.AddQuality(ctx, map[rollup.QualityKey]rollup.Quality{{HourMS: f.at(22, 10), DeploymentID: f.dep, PriorityClass: "low"}: low}); err != nil {
		t.Fatal(err)
	}
	split, err := f.svc.Quality(ctx, Query{FromMS: f.at(22, 0), ToMS: f.at(23, 0)}, 0)
	if err != nil || len(split.Classes) != 2 || split.Classes[0].PriorityClass != "low" || split.Classes[0].Requests != 30 ||
		split.Classes[1].PriorityClass != "" || split.Classes[1].Requests != 10 || len(split.Rows) != 1 || split.Rows[0].Requests != 40 {
		t.Fatalf("split = %+v rows %+v %v", split.Classes, split.Rows, err)
	}
	empty, _ := f.svc.Quality(ctx, Query{FromMS: f.at(10, 0), ToMS: f.at(11, 0)}, 0)
	if empty.Totals.TTFTP50MS != nil || empty.Totals.OutputTokensPerSec != nil || len(empty.Rows) != 0 {
		t.Fatalf("empty = %+v", empty)
	}

	node, _ := f.st.RegisterNode("node-uuid", store.HashToken("n"), "host-a")
	cardA := rollup.GPUKey{NodeUUID: node.UUID, GPUIndex: 0, GPUUUID: "GPU-a"}
	cardB := rollup.GPUKey{NodeUUID: node.UUID, GPUIndex: 1, GPUUUID: "GPU-b"}
	hour := func(k rollup.GPUKey, ms int64) rollup.GPUKey { k.HourMS = ms; return k }
	if err := f.stats.AddGPUs(ctx, map[rollup.GPUKey]rollup.GPU{
		hour(cardA, f.at(22, 9)):  {UtilSum: 80, UtilN: 2, UtilMax: 50, PowerSum: 400, PowerN: 2, TempSum: 100, TempN: 2, TempMax: 55},
		hour(cardB, f.at(22, 9)):  {UtilSum: 0, UtilN: 2, PowerSum: 100, PowerN: 2},
		hour(cardA, f.at(22, 10)): {UtilSum: 120, UtilN: 2, UtilMax: 70},
	}, map[rollup.GPUKey]string{cardA: "RTX 6000D", cardB: "RTX 6000D"}); err != nil {
		t.Fatal(err)
	}
	gq := GPUQuery{Query: Query{FromMS: f.at(22, 0), ToMS: f.at(23, 0), Granularity: Hour}, GroupBy: "gpu", Metric: MetricPower}
	gpus, err := f.svc.GPUs(ctx, gq)
	if err != nil || len(gpus.Cards) != 2 || len(gpus.Heat) != 2 {
		t.Fatalf("gpus = %+v %v", gpus, err)
	}
	a := gpus.Cards[0]
	if a.GPUUUID != "GPU-a" || *a.UtilAvg != 50 || *a.UtilMax != 70 || a.NodeName != "host-a" || a.Present || a.GPUModel != "RTX 6000D" {
		t.Fatalf("card a = %+v", a)
	}
	if gpus.Heat[0].Label != "host-a #0" || *gpus.Heat[0].Values[9] != 200 || gpus.Heat[0].Values[10] != nil {
		t.Fatalf("power heat = %+v", gpus.Heat[0])
	}
	byModel, _ := f.svc.GPUs(ctx, GPUQuery{Query: gq.Query})
	if len(byModel.Heat) != 1 || byModel.Heat[0].Label != "RTX 6000D" || *byModel.Heat[0].Values[9] != 20 || byModel.Metric != MetricUtil {
		t.Fatalf("by model = %+v", byModel.Heat)
	}

	if err := f.stats.AddGPUTime(ctx, map[rollup.GPUTimeKey]rollup.GPUTime{
		{HourMS: f.at(22, 9), DeploymentID: f.dep, GPUModel: "RTX 6000D"}: {GPUSeconds: 4 * 3600, WholeGPUSeconds: 4 * 3600, InstanceSeconds: 3600, MaxInstances: 1},
	}, map[rollup.CapacityKey]rollup.Capacity{{HourMS: f.at(22, 9), GPUModel: "RTX 6000D"}: {TotalGPUSeconds: 8 * 3600, SchedulableGPUSeconds: 8 * 3600}}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.AddUsage(ctx, map[rollup.UsageKey]rollup.Usage{{HourMS: f.at(22, 9), APIKeyID: f.key, UserID: 1, DeploymentID: f.dep}: {Requests: 1, PromptTokens: 3e6, CompletionTokens: 1e6}}); err != nil {
		t.Fatal(err)
	}
	gt, err := f.svc.GPUTime(ctx, Query{FromMS: f.at(22, 0), ToMS: f.at(23, 0), Granularity: Hour}, 0)
	if err != nil || len(gt.Rows) != 1 || len(gt.Series) != 1 || len(gt.Capacity) != 1 {
		t.Fatalf("gpu time = %+v %v", gt, err)
	}
	row := gt.Rows[0]
	if row.GPUHours != 4 || row.MaxInstances != 1 || *row.GPUHoursPerMillionOutput != 4 || *row.GPUHoursPerMillionAllTokens != 1 || gt.Series[0].GPUHours[9] != 4 {
		t.Fatalf("row = %+v", row)
	}
	// 1 instance-hour over the 22 hours that have passed.
	if row.AvgInstances < 0.045 || row.AvgInstances > 0.046 {
		t.Fatalf("avg instances = %v", row.AvgInstances)
	}
	if c := gt.Capacity[0]; c.TotalGPUHours != 8 || c.AllocatedGPUHours != 4 {
		t.Fatalf("capacity = %+v", c)
	}
}

// The card time's tokens per deployment are what the usage's groups add up
// to, over hours and over days rolled up or not, with or without a
// deployment filter.
func TestUsageByDeploymentMatchesTheUsageGroups(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	other, err := f.st.CreateDeployment(&store.Deployment{Name: "qwen", ServedName: "Qwen/Qwen3", State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	delta := map[rollup.UsageKey]rollup.Usage{}
	for i, at := range []int64{f.at(20, 9), f.at(21, 3), f.at(21, 23), f.at(22, 1), f.at(22, 14)} {
		for _, dep := range []uint{f.dep, other.ID} {
			for _, src := range []uxrecord.Source{uxrecord.SourceAPI, uxrecord.SourcePlayground} {
				for _, key := range []uint{f.key, 0, 4242} {
					n := int64(i+1) * int64(dep) * int64(key+1)
					delta[rollup.UsageKey{HourMS: at, APIKeyID: key, UserID: 1, DeploymentID: dep, Source: src}] =
						rollup.Usage{Requests: 1, PromptTokens: 1000 * n, CompletionTokens: 10 * n, ReasoningTokens: n}
				}
			}
		}
	}
	if err := f.st.AddUsage(ctx, delta); err != nil {
		t.Fatal(err)
	}
	for _, d := range []rollup.Day{20261020, 20261021} { // the 22nd is today, unrolled
		if err := f.st.RollupUsageDay(ctx, d, f.loc); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []Query{
		{FromMS: f.at(20, 0), ToMS: f.at(23, 0), Granularity: Day},
		{FromMS: f.at(21, 12), ToMS: f.at(22, 15), Granularity: Day},
		{FromMS: f.at(21, 0), ToMS: f.at(22, 15), Granularity: Hour},
	} {
		for _, filter := range []UsageFilter{{}, {DeploymentID: other.ID}} {
			sp, err := f.svc.resolve(q)
			if err != nil {
				t.Fatal(err)
			}
			groups, err := f.svc.usageData(ctx, sp, filter)
			if err != nil {
				t.Fatal(err)
			}
			want := map[uint]store.UsageCounts{}
			for k, acc := range groups.groups {
				cur := want[k.dep]
				addCounts(&cur, acc.UsageCounts)
				want[k.dep] = cur
			}
			got, err := f.svc.usageByDeployment(ctx, sp, filter)
			if err != nil {
				t.Fatal(err)
			}
			if len(want) == 0 || fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("%+v %+v:\n got %v\nwant %v", q, filter, got, want)
			}
		}
	}
}
