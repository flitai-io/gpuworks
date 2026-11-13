package uxstore

import (
	"database/sql"
	"fmt"
	"slices"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tracecontext"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// Grid kinds as stored in memory; the tables keep the uxrecord kind strings.
const (
	kindIO uint8 = iota
	kindUT
	kindSeg
)

var kindNames = [...]string{kindIO: uxrecord.GridIO, kindUT: uxrecord.GridUncachedTTFT, kindSeg: uxrecord.GridSegments}

type gridKey struct {
	bucket int64
	dep    uint
	kind   uint8
	x, y   int
}

type cntKey struct {
	bucket int64
	dep    uint
}

type cntVal struct{ total, withUsage, withCache int64 }

// writerState is owned by the writer goroutine (and by Open before it
// starts); nothing else touches it.
type writerState struct {
	sampler sampler
	// ready memoizes days whose tables exist, so a flush runs no DDL for
	// them. Filled from the file at Open, extended after a commit that
	// created a day, pruned by retention.
	ready map[string]struct{}

	rows []uxrecord.Request
	grid map[gridKey]int64
	cnt  map[cntKey]cntVal
	lat  map[cntKey]*uxrecord.LatencySums

	lastRetain time.Time
	// finalErr is the error of the flush Close ran.
	finalErr error
}

// initWriter loads the days already in the file, giving each the tables
// added since it was created (every statement is IF NOT EXISTS) and its
// tables the columns added since (completePartition).
func (s *Store) initWriter() error {
	days, err := listDays(s.writer)
	if err != nil {
		return fmt.Errorf("list partitions: %w", err)
	}
	for _, d := range days {
		for _, stmt := range partitionDDL(d) {
			if _, err := s.writer.Exec(stmt); err != nil {
				return fmt.Errorf("complete partition %s: %w", d, err)
			}
		}
		if err := completePartition(s.writer, d); err != nil {
			return fmt.Errorf("complete partition %s: %w", d, err)
		}
	}
	s.w = writerState{
		sampler:    sampler{budget: s.opts.RowsPerSecond},
		ready:      make(map[string]struct{}, len(days)),
		grid:       map[gridKey]int64{},
		cnt:        map[cntKey]cntVal{},
		lat:        map[cntKey]*uxrecord.LatencySums{},
		lastRetain: s.opts.Now(),
	}
	for _, d := range days {
		s.w.ready[d] = struct{}{}
	}
	s.partitions.Store(int64(len(days)))
	return nil
}

// run is the writer goroutine.
func (s *Store) run() {
	defer close(s.done)
	ticker := time.NewTicker(s.opts.FlushEvery)
	defer ticker.Stop()
	for {
		select {
		case r := <-s.queue:
			s.handle(r)
			if len(s.w.rows) >= maxPendingRows {
				_ = s.flush()
			}
		case <-ticker.C:
			_ = s.flush()
			s.maybeRetain()
		case req := <-s.ctl:
			s.drainQueue()
			err := s.flush()
			if err == nil && req.retain {
				err = s.retain()
			}
			req.done <- err
		case <-s.stop:
			s.drainQueue()
			s.w.finalErr = s.flush()
			return
		}
	}
}

// drainQueue handles whatever is queued right now without waiting for more.
func (s *Store) drainQueue() {
	for {
		select {
		case r := <-s.queue:
			s.handle(r)
		default:
			return
		}
	}
}

// handle adds a record to the grids and the coverage counts (every record)
// and, if the sampler keeps it, to the pending request rows.
func (s *Store) handle(r uxrecord.Request) {
	if s.opts.beforeRecord != nil {
		s.opts.beforeRecord()
	}
	w := &s.w
	bucket := floorBucket(r.ReceivedMS)
	cells := uxrecord.GridCells(&r)
	if cells.HasIO {
		w.grid[gridKey{bucket, r.DeploymentID, kindIO, cells.IO.X, cells.IO.Y}]++
	}
	if cells.HasUncachedTT {
		w.grid[gridKey{bucket, r.DeploymentID, kindUT, cells.UncachedTTFT.X, cells.UncachedTTFT.Y}]++
	}
	ck := cntKey{bucket, r.DeploymentID}
	c := w.cnt[ck]
	c.total++
	if r.Usage {
		c.withUsage++
		if r.CachedTokens >= 0 {
			c.withCache++
		}
	}
	w.cnt[ck] = c
	if sums, segCells, ok := uxrecord.LatencyOf(r); ok {
		for _, cell := range segCells {
			w.grid[gridKey{bucket, r.DeploymentID, kindSeg, cell.X, cell.Y}] += cell.N
		}
		acc := w.lat[ck]
		if acc == nil {
			acc = &uxrecord.LatencySums{}
			w.lat[ck] = acc
		}
		acc.Merge(sums)
	}

	keep, weight := w.sampler.keep(&r, s.opts.Now().Unix())
	if !keep {
		s.sampledAway.Add(1)
		return
	}
	r.Weight = weight
	w.rows = append(w.rows, r)
}

// flush writes everything pending in one transaction. The pending data is
// discarded either way; a failed transaction is counted, not retried.
func (s *Store) flush() error {
	w := &s.w
	if len(w.rows) == 0 && len(w.grid) == 0 && len(w.cnt) == 0 && len(w.lat) == 0 {
		return nil
	}
	created, err := s.writeBatch()
	if err != nil {
		s.flushErrors.Add(1)
	} else {
		s.rowsWritten.Add(uint64(len(w.rows)))
		for _, d := range created {
			w.ready[d] = struct{}{}
		}
		s.partitions.Add(int64(len(created)))
	}
	w.rows = w.rows[:0]
	clear(w.grid)
	clear(w.cnt)
	clear(w.lat)
	return err
}

const insertReqSQL = `INSERT INTO %s (id, ts, deployment_id, instance_uuid, api_key_id, user_id, source, path, stream,
	body_bytes, images, dispatch_ms, first_byte_ms, first_think_ms, first_answer_ms, last_chunk_ms, end_ms,
	events, think_events, max_gap_ms, status, outcome, finish_reason, usage, prompt_tokens, completion_tokens,
	cached_tokens, reasoning_tokens, engine_queue_ms, engine_prefill_ms, engine_decode_ms, weight, trace_id,
	pd_prefill_ms, pd_prefill_queue_ms, pd_prefill_compute_ms, pd_prefill_instance, pd_fallback)
	VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

const upsertGridSQL = `INSERT INTO %s (bucket_ms, deployment_id, kind, x, y, n) VALUES (?,?,?,?,?,?)
	ON CONFLICT (bucket_ms, deployment_id, kind, x, y) DO UPDATE SET n = n + excluded.n`

const upsertCntSQL = `INSERT INTO %s (bucket_ms, deployment_id, total, with_usage, with_cache) VALUES (?,?,?,?,?)
	ON CONFLICT (deployment_id, bucket_ms) DO UPDATE SET total = total + excluded.total,
	with_usage = with_usage + excluded.with_usage, with_cache = with_cache + excluded.with_cache`

const upsertLatSQL = `INSERT INTO %s (bucket_ms, deployment_id, requests, e2e_ms, gateway_ms, preprocess_ms, queue_ms, prefill_ms,
	waiting_ms, thinking_ms, answering_ms, tail_ms, prefill_speed_tokens, prefill_speed_ms, thinking_speed_tokens,
	thinking_speed_ms, answering_speed_tokens, answering_speed_ms, decode_speed_tokens, decode_speed_ms, pd_prefill_ms)
	VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT (deployment_id, bucket_ms) DO UPDATE SET requests = requests + excluded.requests,
	e2e_ms = e2e_ms + excluded.e2e_ms, gateway_ms = gateway_ms + excluded.gateway_ms,
	preprocess_ms = preprocess_ms + excluded.preprocess_ms,
	queue_ms = queue_ms + excluded.queue_ms, prefill_ms = prefill_ms + excluded.prefill_ms,
	waiting_ms = waiting_ms + excluded.waiting_ms, thinking_ms = thinking_ms + excluded.thinking_ms,
	answering_ms = answering_ms + excluded.answering_ms, tail_ms = tail_ms + excluded.tail_ms,
	prefill_speed_tokens = prefill_speed_tokens + excluded.prefill_speed_tokens,
	prefill_speed_ms = prefill_speed_ms + excluded.prefill_speed_ms,
	thinking_speed_tokens = thinking_speed_tokens + excluded.thinking_speed_tokens,
	thinking_speed_ms = thinking_speed_ms + excluded.thinking_speed_ms,
	answering_speed_tokens = answering_speed_tokens + excluded.answering_speed_tokens,
	answering_speed_ms = answering_speed_ms + excluded.answering_speed_ms,
	decode_speed_tokens = decode_speed_tokens + excluded.decode_speed_tokens,
	decode_speed_ms = decode_speed_ms + excluded.decode_speed_ms,
	pd_prefill_ms = pd_prefill_ms + excluded.pd_prefill_ms`

// writeBatch is one transaction covering the request rows, the grid upserts,
// the coverage upserts and the latency sums. It returns the days whose tables
// it created.
func (s *Store) writeBatch() (created []string, err error) {
	w := &s.w
	days := map[string]struct{}{}
	for i := range w.rows {
		days[dayOf(w.rows[i].ReceivedMS)] = struct{}{}
	}
	for k := range w.cnt { // every grid key has a coverage key of the same bucket
		days[dayOf(k.bucket)] = struct{}{}
	}
	order := make([]string, 0, len(days))
	for d := range days {
		order = append(order, d)
	}
	slices.Sort(order)

	tx, err := s.writer.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }() // Commit/statement errors take precedence; a committed transaction is already closed.
	for _, d := range order {
		made, err := s.ensurePartition(tx, d)
		if err != nil {
			return nil, err
		}
		if made {
			created = append(created, d)
		}
	}

	stmts := newStmtCache(tx)
	defer stmts.close()
	for i := range w.rows {
		r := &w.rows[i]
		pd := pdColumns(r.PD)
		st, err := stmts.get(insertReqSQL, reqPrefix+dayOf(r.ReceivedMS))
		if err != nil {
			return nil, err
		}
		if _, err := st.Exec(r.ID, r.ReceivedMS, r.DeploymentID, r.InstanceUUID, r.APIKeyID, r.UserID,
			int(r.Source), r.Path, r.Stream, r.BodyBytes, r.Images, r.DispatchMS, r.FirstByteMS,
			r.FirstThinkMS, r.FirstAnswerMS, r.LastChunkMS, r.EndMS, r.Events, r.ThinkEvents, r.MaxGapMS,
			r.Status, int(r.Outcome), r.FinishReason, r.Usage, r.PromptTokens, r.CompletionTokens,
			r.CachedTokens, r.ReasoningTokens, r.EngineQueueMS, r.EnginePrefillMS, r.EngineDecodeMS,
			r.Weight, traceBlob(r.Trace.Trace), pd.PrefillMS, pd.PrefillQueueMS, pd.PrefillComputeMS,
			pd.PrefillInstance, pd.Fallback); err != nil {
			return nil, fmt.Errorf("insert request: %w", err)
		}
	}
	for k, n := range w.grid {
		st, err := stmts.get(upsertGridSQL, gridPrefix+dayOf(k.bucket))
		if err != nil {
			return nil, err
		}
		if _, err := st.Exec(k.bucket, k.dep, kindNames[k.kind], k.x, k.y, n); err != nil {
			return nil, fmt.Errorf("upsert grid: %w", err)
		}
	}
	for k, c := range w.cnt {
		st, err := stmts.get(upsertCntSQL, cntPrefix+dayOf(k.bucket))
		if err != nil {
			return nil, err
		}
		if _, err := st.Exec(k.bucket, k.dep, c.total, c.withUsage, c.withCache); err != nil {
			return nil, fmt.Errorf("upsert coverage: %w", err)
		}
	}
	for k, l := range w.lat {
		st, err := stmts.get(upsertLatSQL, latPrefix+dayOf(k.bucket))
		if err != nil {
			return nil, err
		}
		seg := l.SegmentMS
		if _, err := st.Exec(k.bucket, k.dep, l.Requests, l.EndToEndMS,
			seg[uxrecord.SegmentGateway], seg[uxrecord.SegmentPreprocess], seg[uxrecord.SegmentQueue], seg[uxrecord.SegmentPrefill],
			seg[uxrecord.SegmentWaiting], seg[uxrecord.SegmentThinking], seg[uxrecord.SegmentAnswering],
			seg[uxrecord.SegmentTail], l.PrefillTokens, l.PrefillMS, l.ThinkingTokens, l.ThinkingMS,
			l.AnsweringTokens, l.AnsweringMS, l.DecodeTokens, l.DecodeMS, seg[uxrecord.SegmentPDPrefill]); err != nil {
			return nil, fmt.Errorf("upsert latency: %w", err)
		}
	}
	if err := stmts.close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return created, nil
}

// stmtCache prepares one statement per (SQL, table) inside a transaction.
type stmtCache struct {
	tx *sql.Tx
	m  map[string]*sql.Stmt
}

func newStmtCache(tx *sql.Tx) *stmtCache { return &stmtCache{tx: tx, m: map[string]*sql.Stmt{}} }

func (c *stmtCache) get(format, table string) (*sql.Stmt, error) {
	key := format + "\x00" + table
	if st, ok := c.m[key]; ok {
		return st, nil
	}
	st, err := c.tx.Prepare(fmt.Sprintf(format, table))
	if err != nil {
		return nil, fmt.Errorf("prepare %s: %w", table, err)
	}
	c.m[key] = st
	return st, nil
}

// close closes every statement; safe to call twice.
func (c *stmtCache) close() error {
	var first error
	for k, st := range c.m {
		if err := st.Close(); err != nil && first == nil {
			first = err
		}
		delete(c.m, k)
	}
	return first
}

// traceBlob is a trace id as the trace_id column holds it: 16 bytes, or
// empty for none.
func traceBlob(t tracecontext.TraceID) []byte {
	if t.IsZero() {
		return []byte{}
	}
	return t[:]
}

// pdColumns is a record's PD stage as the request table's columns hold it:
// -1 and empty for a request that had none.
func pdColumns(pd *uxrecord.PDStage) uxrecord.PDStage {
	if pd == nil {
		return uxrecord.PDStage{PrefillMS: uxrecord.Unknown, PrefillQueueMS: uxrecord.Unknown, PrefillComputeMS: uxrecord.Unknown}
	}
	return *pd
}
