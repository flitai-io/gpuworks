// Package monitor exercises internal/server/monitor end to end against a real
// (SQLite-backed) tsdb.Store, in process and fully offline: no docker, no
// HTTP server, no real store.Store. Inventory is a literal fixture standing in
// for the store-to-monitor adapter (internal/server/app/monitor_adapter.go,
// which has its own tests); TSDB is real, so every window computation and
// quantile-gauge reduction this package exercises runs the exact code the
// product runs against the exact SQL tsdb issues.
package monitor

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	svmonitor "gpuworks.ai/gpuworks/internal/server/monitor"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// newStore opens a real SQLite-backed tsdb.Store in a temp file, matching
// internal/server/monitor's own test convention: a hand-rolled fake tsdb
// here could silently drift from the real QueryRange/QueryLatest SQL
// (bucket alignment, indexed instance/deployment matchers) - CLAUDE.md's
// "test fixtures must faithfully reproduce real behavior" rule.
func newStore(t *testing.T) tsdb.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "monitor-integration.db")
	store, err := tsdb.OpenSQLite(path)
	if err != nil {
		t.Fatalf("open test tsdb: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// countingStore wraps a real tsdb.Store and counts every QueryRange/
// QueryLatest call it forwards, so a test can assert the monitor overview's
// fixed, O(metrics)-not-O(deployments) query budget (plan
// artifacts/archive/2026-09-19-monitor-center/plan.md §8 second phase capacity
// requirement) actually holds at runtime, rather than trusting
// window_fetch.go's doc comments that say so.
type countingStore struct {
	tsdb.Store
	rangeCalls  int
	latestCalls int
}

func (c *countingStore) QueryRange(metric string, matchers map[string]string, startMS, endMS, stepMS int64) ([]tsdb.Series, error) {
	c.rangeCalls++
	return c.Store.QueryRange(metric, matchers, startMS, endMS, stepMS)
}

func (c *countingStore) QueryLatest(metric string, matchers map[string]string, sinceMS int64, asOfMS ...int64) ([]tsdb.LatestPoint, error) {
	c.latestCalls++
	return c.Store.QueryLatest(metric, matchers, sinceMS, asOfMS...)
}

func (c *countingStore) totalCalls() int { return c.rangeCalls + c.latestCalls }

// depKey formats a deployment id the way every tsdb series' "deployment"
// label carries it (monitor/window_fetch.go's depKeyString).
func depKey(id uint) string { return strconv.FormatUint(uint64(id), 10) }

// gaugeHoldTicks/counterHoldTicks/holdStep pick how many WindowStep-spaced
// samples writeGaugeHold/writeCounterHold write: a series spanning 110s, so
// a window over it has several buckets rather than a single point. A counter
// needs one extra raw point because Rate() produces one fewer output point
// than its input.
const (
	gaugeHoldTicks   = 12
	counterHoldTicks = 13
	holdStep         = 10 * time.Second
)

// writeGaugeHold writes a constant-value gauge series ending at now, held
// continuously for (gaugeHoldTicks-1)*holdStep (110s).
func writeGaugeHold(t *testing.T, store tsdb.Store, metric string, labels map[string]string, value float64, now time.Time) {
	t.Helper()
	writeTicks(t, store, metric, labels, now, gaugeHoldTicks, func(int) float64 { return value })
}

func writeTicks(t *testing.T, store tsdb.Store, metric string, labels map[string]string, now time.Time, count int, valueAt func(i int) float64) {
	t.Helper()
	start := now.Add(-time.Duration(count-1) * holdStep)
	samples := make([]tsdb.Sample, count)
	for i := 0; i < count; i++ {
		samples[i] = tsdb.Sample{Metric: metric, Labels: labels, Value: valueAt(i), TimeMS: start.Add(time.Duration(i) * holdStep).UnixMilli()}
	}
	if err := store.WriteBatch(samples); err != nil {
		t.Fatalf("write %s: %v", metric, err)
	}
}

// singleInstanceInventory builds a one-node ("n1"), one-GPU (index 0)
// Inventory for one deployment: `ready` running instances against
// `desired` configured replicas. Every GPU/node-scoped metric this package
// seeds is labeled against this same n1/gpu-0 fixture.
func singleInstanceInventory(depID uint, desired, ready int) svmonitor.Inventory {
	var instances []svmonitor.InstanceRef
	for i := 0; i < ready; i++ {
		instances = append(instances, svmonitor.InstanceRef{
			UUID:         "inst-" + strconv.FormatUint(uint64(depID), 10) + "-" + strconv.Itoa(i),
			DeploymentID: depID,
			NodeUUID:     "n1",
			State:        proto.InstRunning,
			GPUIndexes:   []int{0},
		})
	}
	return svmonitor.Inventory{
		Deployments: []svmonitor.DeploymentRef{{
			ID: depID, Name: "svc-" + strconv.FormatUint(uint64(depID), 10), Engine: "vllm", ReplicasDesired: desired,
		}},
		Instances: instances,
		Nodes:     []svmonitor.NodeRef{{UUID: "n1", Name: "node-1", State: "online"}},
		GPUs:      []svmonitor.GPURef{{NodeUUID: "n1", Index: 0, MemoryTotalBytes: 80 << 30, Healthy: true}},
	}
}
