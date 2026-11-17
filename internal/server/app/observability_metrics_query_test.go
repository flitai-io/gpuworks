package app

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/obsingest"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// TestQueryMetricsAnswersWireSeriesWithPointPairs runs queryMetrics over a
// real tsdb: each stored series becomes a dto.MonitorSeries whose points are
// [unix ms, value] pairs (last value per step bucket), a series with no
// sample in the window keeps "points": [], and the store's errors surface.
func TestQueryMetricsAnswersWireSeriesWithPointPairs(t *testing.T) {
	td, err := tsdb.OpenSQLite(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { td.Close() })
	if err := td.WriteBatch([]tsdb.Sample{
		{Metric: "gpu_util", Labels: map[string]string{"node": "gpu3", "gpu": "0"}, Value: 10, TimeMS: 1000},
		{Metric: "gpu_util", Labels: map[string]string{"node": "gpu3", "gpu": "0"}, Value: 12.5, TimeMS: 1500},
		{Metric: "gpu_util", Labels: map[string]string{"node": "gpu3", "gpu": "0"}, Value: 13, TimeMS: 2000},
		{Metric: "gpu_util", Labels: map[string]string{"node": "gpu4", "gpu": "0"}, Value: 99, TimeMS: 90000},
	}); err != nil {
		t.Fatal(err)
	}
	o := &observability{obsingest.New(obsingest.Stores{TSDB: td})}

	series, err := o.queryMetrics("gpu_util", map[string]string{"node": "gpu3"}, 0, 5000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(dto.NewList(series))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"items":[{"labels":{"gpu":"0","node":"gpu3"},"points":[[1000,12.5],[2000,13]]}],"total":1}`
	if string(raw) != want {
		t.Fatalf("metrics wire JSON\n got %s\nwant %s", raw, want)
	}

	// gpu4's only sample is outside the window: the series is listed with
	// an empty points array, not null.
	series, err = o.queryMetrics("gpu_util", map[string]string{"node": "gpu4"}, 0, 5000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(dto.NewList(series))
	if string(raw) != `{"items":[{"labels":{"gpu":"0","node":"gpu4"},"points":[]}],"total":1}` {
		t.Fatalf("empty-window wire JSON = %s", raw)
	}

	series, err = o.queryMetrics("no_such_metric", nil, 0, 5000, 1000)
	if err != nil || series == nil || len(series) != 0 {
		t.Fatalf("unknown metric = %#v err=%v, want empty non-nil", series, err)
	}

	if _, err := o.queryMetrics("gpu_util", nil, 0, 5000, 0); err == nil {
		t.Fatal("a store error (non-positive step) must surface")
	}
}
