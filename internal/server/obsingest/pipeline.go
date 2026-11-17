// Package obsingest is the server's metrics, log and request store
// ingestion: the queues that keep agent telemetry off the hub's read loops,
// the single writer per store, the deployment-label backfill and the
// quantiles folded from histogram buckets at ingest. It imports no store,
// api, hub or planning: the app adapts its stores to those.
package obsingest

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/selflog"
	"gpuworks.ai/gpuworks/internal/server/logstore"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
	"gpuworks.ai/gpuworks/internal/server/uxstore"
)

// Pipeline is the metrics, log and request stores and the ingestion that
// writes them.
type Pipeline struct {
	tsdb tsdb.Store
	logs logstore.Store
	// ux keeps the gateway's per-request records (ux.db).
	ux *uxstore.Store
	// onSamples sees every ingest batch on the ingest goroutine, before it
	// is written (the long-term GPU statistics count its card samples).
	onSamples func([]tsdb.Sample)

	// logQueue decouples log ingest from the hub's ctrl-stream read loops:
	// IngestLogs only enqueues, and RunLogs is the sole appender. A
	// store stall (lock wait, checkpoint) then delays one batcher goroutine
	// instead of stalling every node's heartbeat behind it.
	logQueue   chan []logstore.Entry
	logDropped atomic.Int64
	logTaken   atomic.Int64

	// metricsQueue decouples metrics ingest the same way logQueue does (see
	// metrics.go): IngestMetrics only converts and enqueues, and RunMetrics is
	// the sole tsdb writer. A tsdb stall (the batched
	// Retain sweep, a checkpoint) then delays one batcher goroutine instead of
	// stalling the hub ctrl read loop that every node's heartbeat rides.
	metricsQueue          chan []tsdb.Sample
	metricsDropped        atomic.Int64
	metricsTaken          atomic.Int64
	lastMetricsDropWarnMS atomic.Int64

	// resolveDeployment backfills the "deployment" label for old agents that
	// send an "instance" label but no "deployment" label (see metrics.go's
	// withDeploymentLabel). The app wires it from store.InstanceByUUID
	// (WrapDeploymentLookup); it is a func so this package stays free of
	// store types.
	//
	// Returns (id, found, cacheable). found is whether instanceUUID resolved
	// to a deployment. cacheable says whether that answer - positive or a
	// confirmed "no such instance" - should be trusted for deploymentLookupTTL:
	// a transient lookup error is not cacheable, since caching it as "unknown"
	// would split that instance's samples across two series for the rest of
	// the TTL window instead of just the one affected batch.
	resolveDeployment func(instanceUUID string) (id uint, found bool, cacheable bool)
	now               func() time.Time

	deployMu    sync.Mutex
	deployCache map[string]deploymentLookup
	// deployCacheCap overrides deploymentCacheCap for tests; zero means use
	// the constant.
	deployCacheCap int
	// deployEvicted counts deployCache entries dropped by the over-capacity
	// fallback (evictDeployCacheIfOverCapacity), not routine TTL sweeps: real
	// data loss under sustained over-capacity load, reported on the
	// RunMetrics pipeline-health log line so it is observable instead
	// of only inferrable from silence.
	deployEvicted atomic.Int64

	// bucketMu/bucketState hold the quantile-at-ingest bucket TTL map (see
	// quantile.go); bucketStateCap overrides bucketStateCapDefault
	// for tests.
	bucketMu       sync.Mutex
	bucketState    map[string]*bucketVector
	bucketStateCap int
	// bucketEvicted is bucketState's counterpart to deployEvicted.
	bucketEvicted atomic.Int64
}

const (
	// logIngestQueueChunks bounds the backlog: each chunk is one agent TLogs
	// message (~4-64 lines). Overflow drops whole chunks, counted — better a
	// lost diagnostic line than a blocked control stream.
	logIngestQueueChunks = 256
	// logIngestFlushRows / logIngestFlushEvery shape the batcher's commits.
	// 100ms keeps the deliberate ingest-visibility cost of batching at half
	// the previous 200ms while still amortizing a commit across ~200 rows at
	// fleet ingest.
	logIngestFlushRows  = 1000
	logIngestFlushEvery = 100 * time.Millisecond
	// Checkpoint policy: a wal_checkpoint(PASSIVE) is a write-lock copy of
	// the WAL into the main database — running it on the batcher's timer
	// stalled ingest for its whole duration and showed up as seconds-deep
	// freshness spikes on the fleet host. It now needs both a quiet interval
	// AND enough rows written since the last copy to be worth the pause; the
	// periodic ceiling bounds the WAL when ingest is slow instead of fast.
	logIngestCheckpointEvery = 30 * time.Second
	logIngestCheckpointRows  = 50_000
)

// Open opens the metrics.db, logs.db and ux.db under the data dir, with the
// queues that keep ingest off its callers: RunLogs and RunMetrics write them.
func Open(dataDir string) (*Pipeline, error) {
	td, err := tsdb.OpenSQLite(filepath.Join(dataDir, "metrics.db"))
	if err != nil {
		return nil, err
	}
	ls, err := logstore.OpenSQLite(filepath.Join(dataDir, "logs.db"))
	if err != nil {
		td.Close()
		return nil, err
	}
	ux, err := uxstore.Open(filepath.Join(dataDir, "ux.db"), uxstore.Options{})
	if err != nil {
		ls.Close()
		td.Close()
		return nil, err
	}
	p := New(Stores{TSDB: td, Logs: ls, UX: ux})
	p.logQueue = make(chan []logstore.Entry, logIngestQueueChunks)
	p.metricsQueue = make(chan []tsdb.Sample, metricsIngestQueueBatches)
	return p, nil
}

// Stores are the stores a Pipeline writes; any may be nil when the caller
// writes none of its kind.
type Stores struct {
	TSDB tsdb.Store
	Logs logstore.Store
	UX   *uxstore.Store
}

// New is a Pipeline over stores that writes each batch on its caller's
// goroutine: there are no queues, and RunLogs and RunMetrics return at once.
// Open's Pipeline is the one that keeps ingest off its callers.
func New(s Stores) *Pipeline {
	return &Pipeline{
		tsdb: s.TSDB, logs: s.Logs, ux: s.UX,
		now:         time.Now,
		deployCache: map[string]deploymentLookup{},
		bucketState: map[string]*bucketVector{},
	}
}

// TSDB is the metrics store.
func (o *Pipeline) TSDB() tsdb.Store { return o.tsdb }

// Logs is the log store.
func (o *Pipeline) Logs() logstore.Store { return o.logs }

// UX is the gateway's per-request record store (ux.db).
func (o *Pipeline) UX() *uxstore.Store { return o.ux }

// SetDeploymentResolver sets how an instance's deployment is found for a
// metric sample that names only the instance (WrapDeploymentLookup). Call it
// before RunMetrics.
func (o *Pipeline) SetDeploymentResolver(resolve func(instanceUUID string) (id uint, found bool, cacheable bool)) {
	o.resolveDeployment = resolve
}

// OnSamples sets what sees every metrics batch on the ingest goroutine before
// it is written. Call it before RunMetrics.
func (o *Pipeline) OnSamples(see func([]tsdb.Sample)) { o.onSamples = see }

// IngestLogs converts an agent log batch and hands it to the ingest queue;
// it never touches the store and can never block the caller. Lines from
// agents that predate severity classification arrive with an empty level and
// are stored as such; an empty source means instance (container) logs.
func (o *Pipeline) IngestLogs(nodeUUID string, lines []proto.LogLine) {
	if len(lines) == 0 {
		return
	}
	entries := logEntries(nodeUUID, lines)
	if o.logQueue == nil {
		// Constructions without the batcher (tests) keep the old synchronous
		// behavior rather than silently dropping everything.
		_ = o.logs.Append(entries)
		return
	}
	select {
	case o.logQueue <- entries:
	default:
		o.logDropped.Add(int64(len(entries)))
	}
}

// IngestLogsWait is IngestLogs for a caller that can afford to wait: an
// agent's telemetry stream, whose wait reaches the agent as flow control and
// leaves the lines in its buffer. It drops nothing unless ctx ends first.
func (o *Pipeline) IngestLogsWait(ctx context.Context, nodeUUID string, lines []proto.LogLine) error {
	if len(lines) == 0 {
		return nil
	}
	entries := logEntries(nodeUUID, lines)
	if o.logQueue == nil {
		return o.logs.Append(entries)
	}
	select {
	case o.logQueue <- entries:
		return nil
	case <-ctx.Done():
		o.logDropped.Add(int64(len(entries)))
		return ctx.Err()
	}
}

func logEntries(nodeUUID string, lines []proto.LogLine) []logstore.Entry {
	entries := make([]logstore.Entry, 0, len(lines))
	for _, l := range lines {
		source := l.Source
		if source == "" {
			source = logstore.SourceInstance
		}
		entries = append(entries, logstore.Entry{
			Instance: l.Instance, Node: nodeUUID, TimeMS: l.TimeMS,
			Level: l.Level, Source: source, Line: l.Line,
		})
	}
	return entries
}

// RunLogs is the single logstore appender until ctx ends. It batches queued chunks
// into commits of up to logIngestFlushRows or one flush interval, whichever
// comes first, and flushes what remains when ctx ends.
func (o *Pipeline) RunLogs(ctx context.Context) {
	if o.logQueue == nil {
		return
	}
	// Low-frequency pipeline observability: at fleet scale this one line
	// distinguishes "agents stopped sending" (taken stalls) from "batcher
	// wedged" (queue pinned full, dropping) from "store slow" (queue deep,
	// taken climbing).
	observe := time.NewTicker(5 * time.Second)
	defer observe.Stop()
	batch := make([]logstore.Entry, 0, logIngestFlushRows)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := o.logs.Append(batch); err != nil {
			// The rows are lost either way; the store's own error is the
			// actionable signal. batch is reused only after Append returned.
			_ = err
		}
		batch = batch[:0]
	}
	ticker := time.NewTicker(logIngestFlushEvery)
	defer ticker.Stop()
	checkpoint := time.NewTicker(logIngestCheckpointEvery)
	defer checkpoint.Stop()
	rowsSinceCheckpoint := 0
	committed := func(rows int) { rowsSinceCheckpoint += rows }
	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case es := <-o.logQueue:
					batch = append(batch, es...)
				default:
					flush()
					return
				}
			}
		case es := <-o.logQueue:
			o.logTaken.Add(int64(len(es)))
			batch = append(batch, es...)
			if len(batch) >= logIngestFlushRows {
				n := len(batch)
				flush()
				committed(n)
			}
		case <-ticker.C:
			if len(batch) > 0 {
				n := len(batch)
				flush()
				committed(n)
			}
		case <-observe.C:
			slog.Info("log ingest pipeline",
				"taken", o.logTaken.Load(), "dropped", o.logDropped.Load(),
				"queued", len(o.logQueue))
		case <-checkpoint.C:
			// WAL hygiene, gated: a passive checkpoint is a write-lock copy
			// that stalls this loop for its whole duration, so it only runs
			// once enough new rows make the pause worthwhile. Best-effort —
			// a busy checkpoint (readers pinning frames) retries later.
			if rowsSinceCheckpoint >= logIngestCheckpointRows {
				if _, _, _, err := o.logs.Checkpoint(); err == nil {
					rowsSinceCheckpoint = 0
				}
			}
		}
	}
}

// Close closes the stores.
func (o *Pipeline) Close() {
	if o.tsdb != nil {
		o.tsdb.Close()
	}
	if o.logs != nil {
		o.logs.Close()
	}
	if o.ux != nil {
		o.ux.Close()
	}
}

// RunSelfLogs drains the server's own error-level slog records into the
// logstore under source=server. Best-effort: append errors are swallowed —
// the records already reached stderr and the rotated file.
func (o *Pipeline) RunSelfLogs(ctx context.Context, c *selflog.Collector) {
	if c == nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			o.drainSelfLogs(c)
			return
		case <-ticker.C:
			o.drainSelfLogs(c)
		}
	}
}

// drainSelfLogs moves one queued batch into the store. The server's own logs
// bypass the agent entirely: there is no TLogs hop, the writer is local.
func (o *Pipeline) drainSelfLogs(c *selflog.Collector) {
	recs := c.Drain(256)
	if len(recs) == 0 {
		return
	}
	entries := make([]logstore.Entry, 0, len(recs))
	for _, r := range recs {
		entries = append(entries, logstore.Entry{
			TimeMS: r.TimeMS, Level: r.Level,
			Source: logstore.SourceServer, Line: r.Line,
		})
	}
	_ = o.logs.Append(entries)
}
