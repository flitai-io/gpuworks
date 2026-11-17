package app

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/internal/server/analytics"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/statsdb"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// analyticsAPI serves usage analytics to the API (api.Analytics).
type analyticsAPI struct {
	ops     *opsStats
	store   *store.Store
	obs     *observability
	dataDir string
	queries *analytics.Service
}

func newAnalyticsAPI(ops *opsStats, st *store.Store, obs *observability, dataDir string) *analyticsAPI {
	return &analyticsAPI{ops: ops, store: st, obs: obs, dataDir: dataDir, queries: &analytics.Service{
		Store: st, Stats: ops.stats, Retention: ops.Retention, LiveSinceMS: ops.liveSinceMS,
		Now: func() time.Time { return ops.now() },
	}}
}

func analyticsQuery(q api.AnalyticsQuery) analytics.Query {
	return analytics.Query{FromMS: q.FromMS, ToMS: q.ToMS, Granularity: q.Granularity}
}

func (a *analyticsAPI) Usage(ctx context.Context, q api.AnalyticsQuery) (dto.UsageAnalytics, error) {
	f := analytics.UsageFilter{DeploymentID: q.DeploymentID, APIKeyID: q.APIKeyID, KeylessOnly: q.Keyless}
	switch q.Source {
	case "api":
		f.Source = 1 + uint8(uxrecord.SourceAPI)
	case "playground":
		f.Source = 1 + uint8(uxrecord.SourcePlayground)
	case "batch":
		f.Source = 1 + uint8(uxrecord.SourceBatch)
	case "evaluation":
		f.Source = 1 + uint8(uxrecord.SourceEvaluation)
	}
	return a.queries.Usage(ctx, analyticsQuery(q), f)
}

func (a *analyticsAPI) Quality(ctx context.Context, q api.AnalyticsQuery) (dto.QualityAnalytics, error) {
	return a.queries.Quality(ctx, analyticsQuery(q), q.DeploymentID)
}

func (a *analyticsAPI) GPUs(ctx context.Context, q api.AnalyticsQuery) (dto.GPUAnalytics, error) {
	return a.queries.GPUs(ctx, analytics.GPUQuery{Query: analyticsQuery(q), GroupBy: q.GroupBy, Metric: q.Metric,
		NodeUUIDs: q.NodeUUIDs, GPUModel: q.GPUModel})
}

func (a *analyticsAPI) GPUTime(ctx context.Context, q api.AnalyticsQuery) (dto.GPUTimeAnalytics, error) {
	return a.queries.GPUTime(ctx, analyticsQuery(q), q.DeploymentID)
}

func (a *analyticsAPI) KeyUsage(ctx context.Context) (dto.KeyUsageList, error) {
	return a.queries.KeyUsage(ctx)
}

func retentionDTO(r store.DataRetention) dto.DataRetention {
	return dto.DataRetention{
		MetricsHours: r.MetricsHours, LogsHours: r.LogsHours,
		RequestLogDays: r.RequestLogDays, RequestLogMaxMB: r.RequestLogMaxMB,
		UsageHourlyDays: r.UsageHourlyDays, UsageDailyDays: r.UsageDailyDays,
		StatsHourlyDays: r.StatsHourlyDays, StatsDailyDays: r.StatsDailyDays,
		Quality: r.Quality, GPU: r.GPU, GPUTime: r.GPUTime, TimeZone: r.TimeZone,
	}
}

func (a *analyticsAPI) Retention() dto.DataRetentionView {
	limits := make(map[string]dto.DataRetentionLimit, len(store.DataRetentionLimits))
	for name, l := range store.DataRetentionLimits {
		limits[name] = dto.DataRetentionLimit{Min: l.Min, Max: l.Max}
	}
	return dto.DataRetentionView{Value: retentionDTO(a.ops.Retention()), Limits: limits, ServerTimeZone: a.ops.fallback.TimeZone}
}

func (a *analyticsAPI) SetRetention(in dto.DataRetention) (dto.DataRetentionView, error) {
	err := a.ops.SetRetention(store.DataRetention{
		MetricsHours: in.MetricsHours, LogsHours: in.LogsHours,
		RequestLogDays: in.RequestLogDays, RequestLogMaxMB: in.RequestLogMaxMB,
		UsageHourlyDays: in.UsageHourlyDays, UsageDailyDays: in.UsageDailyDays,
		StatsHourlyDays: in.StatsHourlyDays, StatsDailyDays: in.StatsDailyDays,
		Quality: in.Quality, GPU: in.GPU, GPUTime: in.GPUTime, TimeZone: in.TimeZone,
	})
	if err != nil {
		return dto.DataRetentionView{}, err
	}
	return a.Retention(), nil
}

func fileSize(path string) *int64 {
	var total int64
	found := false
	for _, suffix := range []string{"", "-wal"} {
		if info, err := os.Stat(path + suffix); err == nil {
			total += info.Size()
			found = true
		}
	}
	if !found {
		return nil
	}
	return &total
}

func ptrTo[T any](v T) *T { return &v }

func (a *analyticsAPI) RetentionStatus(ctx context.Context) (dto.DataRetentionStatus, error) {
	r := a.ops.Retention()
	now := a.ops.now()
	out := dto.DataRetentionStatus{LiveSinceMS: a.ops.liveSinceMS}
	out.Stores = append(out.Stores,
		dto.DataRetentionStore{Kind: "metrics", File: "metrics.db", FileBytes: fileSize(filepath.Join(a.dataDir, "metrics.db")),
			EarliestMS: ptrTo(now.Add(-time.Duration(r.MetricsHours) * time.Hour).UnixMilli())},
		dto.DataRetentionStore{Kind: "logs", File: "logs.db", FileBytes: fileSize(filepath.Join(a.dataDir, "logs.db")),
			EarliestMS: ptrTo(now.Add(-time.Duration(r.LogsHours) * time.Hour).UnixMilli())},
	)
	requests := dto.DataRetentionStore{Kind: "requests", File: "ux.db", FileBytes: fileSize(filepath.Join(a.dataDir, "ux.db"))}
	if a.obs != nil && a.obs.UX() != nil {
		if start, ok, err := a.obs.UX().EarliestDayMS(); err != nil {
			return out, err
		} else if ok {
			requests.EarliestMS = &start
		}
	}
	out.Stores = append(out.Stores, requests)
	hourly, daily, err := a.store.UsageRowCounts(ctx)
	if err != nil {
		return out, err
	}
	out.Stores = append(out.Stores, dto.DataRetentionStore{Kind: "usage", File: "main", HourlyRows: &hourly, DailyRows: &daily})
	statsBytes := a.ops.stats.SizeBytes()
	for _, kind := range statsdb.Kinds() {
		h, d, err := a.ops.stats.RowCount(ctx, kind)
		if err != nil {
			return out, err
		}
		out.Stores = append(out.Stores, dto.DataRetentionStore{Kind: string(kind), File: "stats.db", FileBytes: ptrTo(statsBytes), HourlyRows: &h, DailyRows: &d})
	}
	state, err := a.store.UsageRollupState(now)
	if err != nil {
		return out, err
	}
	quality, _, err := a.ops.stats.BackfillState(ctx, statsdb.KindQuality)
	if err != nil {
		return out, err
	}
	out.Backfill = dto.StatisticsBackfill{UsageDone: state.BackfillDone, UsageThroughMS: state.BackfillThroughMS,
		QualityDone: quality.Done, QualityThroughMS: quality.ThroughMS}
	a.ops.mu.Lock()
	last := a.ops.last
	a.ops.mu.Unlock()
	out.Retain = dto.DataRetentionPass{AtMS: passMS(last.retainAt), Error: last.retainErr}
	out.Rollup = dto.DataRetentionPass{AtMS: passMS(last.rollupAt), Error: last.rollupErr}
	out.UsageFlush = dto.UsageFlush{AtMS: passMS(last.flushAt), Rows: last.flushRows, MS: last.flushMS, Error: last.flushErr}
	return out, nil
}

func passMS(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
