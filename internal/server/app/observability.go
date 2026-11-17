package app

import (
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/server/logstore"
	"gpuworks.ai/gpuworks/internal/server/obsingest"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// observability is the server's metrics, log and request stores and their
// ingestion (obsingest), with the reads the API, the automation and the AI
// assistant make of them in the shapes they take.
type observability struct{ *obsingest.Pipeline }

// openObservability opens the metrics.db, logs.db and ux.db under the data
// dir.
func openObservability(dataDir string) (*observability, error) {
	p, err := obsingest.Open(dataDir)
	if err != nil {
		return nil, err
	}
	return &observability{p}, nil
}

// proxyMetricsSink adapts an Accountant flush batch to the metrics ingest
// path, the same one every agent-reported metric batch already goes through.
// It always runs on the Accountant's own single writer goroutine (see
// proxy.Accountant), never on a request-handling goroutine.
func proxyMetricsSink(obs *observability) func([]tsdb.Sample) {
	return func(samples []tsdb.Sample) {
		batch := make([]proto.MetricSample, 0, len(samples))
		for _, s := range samples {
			batch = append(batch, proto.MetricSample{Metric: s.Metric, Labels: s.Labels, Value: s.Value, TimeMS: s.TimeMS})
		}
		obs.IngestMetrics("server", batch)
	}
}

// queryMetrics answers a range query as wire series: one per label set, each
// point a [unix ms, value] pair.
func (o *observability) queryMetrics(metric string, matchers map[string]string, startMS, endMS, stepMS int64) ([]dto.MonitorSeries, error) {
	series, err := o.TSDB().QueryRange(metric, matchers, startMS, endMS, stepMS)
	if err != nil {
		return nil, err
	}
	out := make([]dto.MonitorSeries, 0, len(series))
	for _, s := range series {
		points := make([]dto.Point, 0, len(s.Points))
		for _, p := range s.Points {
			points = append(points, dto.Point{float64(p.TimeMS), p.Value})
		}
		labels := s.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		out = append(out, dto.MonitorSeries{Labels: labels, Points: points})
	}
	return out, nil
}

// hostMemory reads a node's host memory from the metrics store's newest
// samples by planning.RecentHostMemory, the reader an operator's L1 sleep uses
// too.
func (o *observability) hostMemory() planning.HostMemory {
	return planning.RecentHostMemory(func(metric, node string, sinceMS int64) (float64, bool) {
		points, err := o.TSDB().QueryLatest(metric, map[string]string{"node": node}, sinceMS)
		if err != nil || len(points) == 0 {
			return 0, false
		}
		return points[0].Value, true
	}, time.Now)
}

func (o *observability) latestMetrics(metric string, matchers map[string]string, sinceMS int64, asOfMS ...int64) ([]api.MetricLatest, error) {
	points, err := o.TSDB().QueryLatest(metric, matchers, sinceMS, asOfMS...)
	if err != nil {
		return nil, err
	}
	out := make([]api.MetricLatest, 0, len(points))
	for _, point := range points {
		out = append(out, api.MetricLatest{Labels: point.Labels, Value: point.Value, TimeMS: point.TimeMS})
	}
	return out, nil
}

// logEntryDTO converts a store row into the api wire shape.
func logEntryDTO(e logstore.Entry) api.LogEntry {
	return api.LogEntry{
		ID: e.ID, Instance: e.Instance, Node: e.Node, Level: e.Level,
		Source: e.Source, TimeMS: e.TimeMS, Line: e.Line,
	}
}

func (o *observability) searchLogs(f api.LogsFilter) (api.LogsPage, error) {
	res, err := o.Logs().Search(logstore.Filter{
		Query: f.Query, Instance: f.Instance, Instances: f.Instances, Node: f.Node, Level: f.Level,
		Source: f.Source, FromMS: f.FromMS, ToMS: f.ToMS,
		CursorTS: f.CursorTS, CursorID: f.CursorID,
		Limit: f.Limit, WithTotal: f.WithTotal,
	})
	if err != nil {
		return api.LogsPage{}, err
	}
	page := api.LogsPage{Total: res.Total, TotalCapped: res.TotalCapped, Truncated: res.Truncated, Items: make([]api.LogEntry, 0, len(res.Entries))}
	for _, e := range res.Entries {
		page.Items = append(page.Items, logEntryDTO(e))
	}
	return page, nil
}

func (o *observability) contextLogs(instance, node string, ts, id int64, before, after int) (api.LogsWindow, error) {
	rows, idx, err := o.Logs().Context(instance, node, ts, id, before, after)
	if err != nil {
		return api.LogsWindow{}, err
	}
	window := api.LogsWindow{Items: make([]api.LogEntry, 0, len(rows)), MatchIndex: idx}
	for _, e := range rows {
		window.Items = append(window.Items, logEntryDTO(e))
	}
	return window, nil
}

func (o *observability) histogramLogs(f api.LogsFilter, bucketMS int64) ([]api.LogsBucket, error) {
	buckets, err := o.Logs().Histogram(logstore.Filter{
		Query: f.Query, Instance: f.Instance, Instances: f.Instances, Node: f.Node, Level: f.Level,
		Source: f.Source, FromMS: f.FromMS, ToMS: f.ToMS,
	}, bucketMS)
	if err != nil {
		return nil, err
	}
	out := make([]api.LogsBucket, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, api.LogsBucket{StartMS: b.StartMS, Counts: b.Counts})
	}
	return out, nil
}

// agentRejectDTOs answers the hub's recent handshake refusals on the wire.
func agentRejectDTOs(events []hub.RejectEvent) []dto.AgentReject {
	out := make([]dto.AgentReject, 0, len(events))
	for _, e := range events {
		out = append(out, dto.AgentReject{TokenPrefix: e.TokenPrefix, Reason: e.Reason, Remote: e.Remote, At: e.At})
	}
	return out
}
