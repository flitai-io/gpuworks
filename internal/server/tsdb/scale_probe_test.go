package tsdb

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// The scale probe measures metrics.db at fleet shape: ingest throughput, a
// one-node chart, a fleet-wide range scan, and WriteBatch latency while such
// a scan is running. It is a load measurement, gated like the other probes,
// and runs on an idle host on request:
//
//	GPUWORKS_TEST_PERF=1 go test -count=1 -v ./internal/server/tsdb/ -run '^TestScaleProbe$'
//
// GPUWORKS_TSDB_PROBE_NODES (default 1000) and GPUWORKS_TSDB_PROBE_HOURS
// (default 6) size it. Every node has 8 cards reporting the agent's GPU
// metrics every 10 s, so the series count is the fleet's; only the history
// is shorter than a retention window, and only utilization carries it. Ingest
// is written tick by tick in batches of the server's ingest flush size, the
// order real ingest commits.
func TestScaleProbe(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_PERF") != "1" {
		t.Skip("tsdb load probe; set GPUWORKS_TEST_PERF=1 on an idle host to run")
	}
	nodes := probeEnvInt(t, "GPUWORKS_TSDB_PROBE_NODES", 1000)
	hours := probeEnvInt(t, "GPUWORKS_TSDB_PROBE_HOURS", 6)
	const (
		gpusPerNode = 8
		intervalMS  = 10_000
		flushRows   = 2000
	)
	metrics := []string{
		metricUtil,
		"gpuworks_gpu_memory_used_bytes",
		"gpuworks_gpu_power_watts",
		"gpuworks_gpu_temperature_celsius",
	}
	dbPath := filepath.Join(t.TempDir(), "metrics.db")
	st, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	series := make([]map[string]string, 0, nodes*gpusPerNode)
	for n := 0; n < nodes; n++ {
		for g := 0; g < gpusPerNode; g++ {
			series = append(series, map[string]string{
				"node":     fmt.Sprintf("node-%04d", n),
				"gpu":      strconv.Itoa(g),
				"gpu_uuid": fmt.Sprintf("GPU-%04d-%d", n, g),
			})
		}
	}
	endMS := int64(1_800_000_000_000)
	startMS := endMS - int64(hours)*3_600_000
	tickBatch := func(ts int64) []Sample {
		out := make([]Sample, 0, len(series)*len(metrics))
		for _, m := range metrics {
			for i, labels := range series {
				out = append(out, Sample{Metric: m, Labels: labels, Value: float64((ts/intervalMS + int64(i)) % 100), TimeMS: ts})
			}
		}
		return out
	}

	// Series rows come from WriteBatch so each version under test fills its
	// own columns; the utilization history is then bulk-inserted in SQL,
	// which is not what is being measured and is far faster than ingest.
	if err := writeAll(st, tickBatch(startMS), flushRows); err != nil {
		t.Fatalf("seed series: %v", err)
	}
	seedStart := time.Now()
	if _, err := st.(*sqliteStore).db.Exec(`
		WITH RECURSIVE ticks(ts) AS (SELECT ? UNION ALL SELECT ts + ? FROM ticks WHERE ts + ? <= ?)
		INSERT OR REPLACE INTO samples(series_id, ts_ms, value)
		SELECT sr.id, ticks.ts, (ticks.ts / ? + sr.id) % 100 FROM series sr, ticks WHERE sr.metric = ?`,
		startMS+intervalMS, intervalMS, intervalMS, endMS, intervalMS, metricUtil); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	t.Logf("fleet: %d nodes, %d series, %dh of 10s %s history (seeded in %v)",
		nodes, len(series)*len(metrics), hours, metricUtil, time.Since(seedStart).Round(time.Second))
	if fi, err := os.Stat(dbPath); err == nil {
		t.Logf("%-40s %.0f MB", "metrics.db size", float64(fi.Size())/(1<<20))
	}

	// Ingest: ten full-fleet ticks after the history, in flush-size commits.
	const ingestTicks = 10
	written := 0
	ingestStart := time.Now()
	for i := 1; i <= ingestTicks; i++ {
		batch := tickBatch(endMS + int64(i)*intervalMS)
		if err := writeAll(st, batch, flushRows); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		written += len(batch)
	}
	ingest := time.Since(ingestStart)
	t.Logf("%-40s %d samples in %v (%.0f samples/s)", "ingest", written, ingest.Round(time.Millisecond), float64(written)/ingest.Seconds())
	endMS += ingestTicks * intervalMS

	probe := func(name string, fn func() int) {
		var d [5]time.Duration
		points := 0
		for i := range d {
			s := time.Now()
			points = fn()
			d[i] = time.Since(s)
		}
		slices.Sort(d[:])
		t.Logf("%-40s %v (median of 5, %d points)", name, d[2].Round(100*time.Microsecond), points)
	}
	query := func(matchers map[string]string, fromMS, stepMS int64) int {
		got, err := st.QueryRange(metricUtil, matchers, fromMS, endMS, stepMS)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		points := 0
		for _, s := range got {
			points += len(s.Points)
		}
		return points
	}
	oneNode := map[string]string{"node": fmt.Sprintf("node-%04d", nodes/2)}
	probe("one node, last 1h, step 60s", func() int { return query(oneNode, endMS-3_600_000, 60_000) })
	probe(fmt.Sprintf("one node, last %dh, step 300s", hours), func() int { return query(oneNode, startMS, 300_000) })
	probe(fmt.Sprintf("fleet, last %dh, step 60s", hours), func() int { return query(nil, startMS, 60_000) })
	probe(fmt.Sprintf("fleet, last %dh, step 300s", hours), func() int { return query(nil, startMS, 300_000) })

	// WriteBatch latency while fleet scans run back to back: the shape of a
	// dashboard holding a long range query while agents keep reporting.
	var stop atomic.Bool
	var scans atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			query(nil, startMS, 300_000)
			scans.Add(1)
		}
	}()
	time.Sleep(50 * time.Millisecond)
	var lat []time.Duration
	ts := endMS + intervalMS
	for len(lat) < 20 {
		batch := tickBatch(ts)
		batch = batch[:min(flushRows, len(batch))]
		s := time.Now()
		if err := st.WriteBatch(batch); err != nil {
			t.Fatalf("write under scan: %v", err)
		}
		lat = append(lat, time.Since(s))
		ts += intervalMS
	}
	stop.Store(true)
	<-done
	slices.Sort(lat)
	t.Logf("%-40s p50 %v  p95 %v  max %v (%d writes, %d scans)", "write 2000 samples during fleet scans",
		lat[len(lat)/2].Round(100*time.Microsecond), lat[len(lat)*95/100].Round(100*time.Microsecond),
		lat[len(lat)-1].Round(100*time.Microsecond), len(lat), scans.Load())
}

func writeAll(st Store, batch []Sample, flushRows int) error {
	for len(batch) > 0 {
		n := min(flushRows, len(batch))
		if err := st.WriteBatch(batch[:n]); err != nil {
			return err
		}
		batch = batch[n:]
	}
	return nil
}

func probeEnvInt(t *testing.T, key string, def int) int {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		t.Fatalf("%s=%q: want a positive integer", key, v)
	}
	return n
}
