package uxstore

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// ErrBadQuery wraps every query validation failure (a missing deployment, an
// unknown grid kind, a malformed cursor), so an API can answer 400.
var ErrBadQuery = errors.New("uxstore: bad query")

// Request page sizes.
const (
	DefaultRequestLimit = 50
	MaxRequestLimit     = 500
)

// firstTokenSQL is a request's time to its first token in SQL, as
// uxrecord.FirstTokenOf works it out: the earlier of the first reasoning and
// the first answer, -1 when neither came.
const firstTokenSQL = "(CASE WHEN r.first_think_ms >= 0 AND (r.first_answer_ms < 0 OR r.first_think_ms < r.first_answer_ms) THEN r.first_think_ms ELSE r.first_answer_ms END)"

// RequestQuery selects a page of a deployment's request log, newest first.
type RequestQuery struct {
	// DeploymentID is required.
	DeploymentID uint
	// FromMS and ToMS bound ReceivedMS, inclusive; <= 0 is open.
	FromMS, ToMS int64
	// APIKeyID narrows to one caller; 0 is any.
	APIKeyID uint
	// InstanceUUID narrows to one replica; "" is any.
	InstanceUUID string
	// PriorityClass narrows to the requests run at one class; "" is any.
	PriorityClass string
	// Results keeps the requests of these results (uxrecord.ResultOf);
	// empty is every result.
	Results []uxrecord.Result
	// MinEndMS and MaxEndMS bound the end-to-end time (EndMS), inclusive;
	// <= 0 is open.
	MinEndMS, MaxEndMS int32
	// MinTTFTMS and MaxTTFTMS bound the time to the first token, the earlier
	// of the first reasoning and the first answer, inclusive; <= 0 is open.
	// Either keeps only streamed requests that had a first token.
	MinTTFTMS, MaxTTFTMS int32
	// Limit is the page size: <= 0 is DefaultRequestLimit, capped at
	// MaxRequestLimit.
	Limit int
	// Cursor continues after the page that returned it; "" starts at the
	// newest request.
	Cursor string
}

// cursor is the keyset position of the last row of a page: its ReceivedMS,
// its day partition and its sequence in that partition.
type cursor struct {
	ts  int64
	day string
	seq int64
}

func (c cursor) encode() string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(fmt.Sprintf("1.%d.%s.%d", c.ts, c.day, c.seq)))
}

func decodeCursor(s string) (cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: cursor: %v", ErrBadQuery, err)
	}
	parts := strings.Split(string(raw), ".")
	if len(parts) != 4 || parts[0] != "1" {
		return cursor{}, fmt.Errorf("%w: cursor format", ErrBadQuery)
	}
	ts, errTS := strconv.ParseInt(parts[1], 10, 64)
	seq, errSeq := strconv.ParseInt(parts[3], 10, 64)
	if errTS != nil || errSeq != nil || ts <= 0 || dayOf(ts) != parts[2] {
		return cursor{}, fmt.Errorf("%w: cursor values", ErrBadQuery)
	}
	return cursor{ts: ts, day: parts[2], seq: seq}, nil
}

const reqColumns = `r.seq, r.id, r.ts, r.deployment_id, r.instance_uuid, r.api_key_id, r.user_id, r.source,
	r.path, r.stream, r.body_bytes, r.images, r.dispatch_ms, r.first_byte_ms, r.first_think_ms,
	r.first_answer_ms, r.last_chunk_ms, r.end_ms, r.events, r.think_events, r.max_gap_ms, r.status,
	r.outcome, r.finish_reason, r.usage, r.prompt_tokens, r.completion_tokens, r.cached_tokens,
	r.reasoning_tokens, r.engine_queue_ms, r.engine_prefill_ms, r.engine_decode_ms, r.weight, r.trace_id,
	r.pd_prefill_ms, r.pd_prefill_queue_ms, r.pd_prefill_compute_ms, r.pd_prefill_instance, r.pd_fallback,
	r.priority_class`

func scanRequest(rows *sql.Rows, seq *int64) (uxrecord.Request, error) {
	var r uxrecord.Request
	var source, outcome int
	var trace []byte
	var pd uxrecord.PDStage
	err := rows.Scan(seq, &r.ID, &r.ReceivedMS, &r.DeploymentID, &r.InstanceUUID, &r.APIKeyID, &r.UserID,
		&source, &r.Path, &r.Stream, &r.BodyBytes, &r.Images, &r.DispatchMS, &r.FirstByteMS,
		&r.FirstThinkMS, &r.FirstAnswerMS, &r.LastChunkMS, &r.EndMS, &r.Events, &r.ThinkEvents,
		&r.MaxGapMS, &r.Status, &outcome, &r.FinishReason, &r.Usage, &r.PromptTokens,
		&r.CompletionTokens, &r.CachedTokens, &r.ReasoningTokens, &r.EngineQueueMS,
		&r.EnginePrefillMS, &r.EngineDecodeMS, &r.Weight, &trace,
		&pd.PrefillMS, &pd.PrefillQueueMS, &pd.PrefillComputeMS, &pd.PrefillInstance, &pd.Fallback,
		&r.PriorityClass)
	r.Source, r.Outcome = uxrecord.Source(source), uxrecord.Outcome(outcome)
	if len(trace) == len(r.Trace.Trace) {
		copy(r.Trace.Trace[:], trace)
	}
	// A request with any PD fact was disaggregated: a prefill instance, a
	// fallback, or a stage the gateway waited for.
	if pd.PrefillInstance != "" || pd.Fallback != "" || pd.PrefillMS >= 0 {
		r.PD = &pd
	}
	return r, err
}

// resultSQL is uxrecord.ResultOf as WHERE terms over a req_ table aliased r,
// one per result; TestResultFilterMatchesGoRule holds them to it.
var resultSQL = map[uxrecord.Result]string{
	uxrecord.ResultError:        "(r.outcome = 2 OR r.status < 200 OR r.status >= 300)",
	uxrecord.ResultClientClosed: "(r.outcome = 1 AND r.status BETWEEN 200 AND 299)",
	uxrecord.ResultInterrupted:  "(r.outcome = 3 AND r.status BETWEEN 200 AND 299)",
	uxrecord.ResultLength:       "(r.outcome = 0 AND r.status BETWEEN 200 AND 299 AND r.finish_reason = 'length')",
	uxrecord.ResultCompleted:    "(r.outcome = 0 AND r.status BETWEEN 200 AND 299 AND r.finish_reason != 'length')",
}

// requestSQL is the per-partition page query. The deployment (or key)
// equality and the ts range ride the (deployment_id, ts, seq) or
// (api_key_id, ts, seq) index from its tail in order with no sort; the other
// filters are checked on the rows the index walk reaches, and the walk stops
// at LIMIT.
func requestSQL(day string, q RequestQuery, cur *cursor, limit int) (string, []any) {
	where := []string{"r.deployment_id = ?"}
	args := []any{q.DeploymentID}
	if q.APIKeyID != 0 {
		where = append(where, "r.api_key_id = ?")
		args = append(args, q.APIKeyID)
	}
	if q.PriorityClass != "" {
		where = append(where, "r.priority_class = ?")
		args = append(args, q.PriorityClass)
	}
	if q.FromMS > 0 {
		where = append(where, "r.ts >= ?")
		args = append(args, q.FromMS)
	}
	if q.ToMS > 0 {
		where = append(where, "r.ts <= ?")
		args = append(args, q.ToMS)
	}
	if cur != nil && cur.day == day {
		where = append(where, "(r.ts, r.seq) < (?, ?)")
		args = append(args, cur.ts, cur.seq)
	}
	if q.InstanceUUID != "" {
		where = append(where, "r.instance_uuid = ?")
		args = append(args, q.InstanceUUID)
	}
	if q.MinEndMS > 0 {
		where = append(where, "r.end_ms >= ?")
		args = append(args, q.MinEndMS)
	}
	if q.MaxEndMS > 0 {
		where = append(where, "r.end_ms >= 0 AND r.end_ms <= ?")
		args = append(args, q.MaxEndMS)
	}
	if q.MinTTFTMS > 0 || q.MaxTTFTMS > 0 {
		where = append(where, "r.stream = 1 AND "+firstTokenSQL+" >= ?")
		args = append(args, max(q.MinTTFTMS, 0))
	}
	if q.MaxTTFTMS > 0 {
		where = append(where, firstTokenSQL+" <= ?")
		args = append(args, q.MaxTTFTMS)
	}
	if len(q.Results) > 0 {
		terms := make([]string, 0, len(q.Results))
		for _, result := range q.Results {
			terms = append(terms, resultSQL[result])
		}
		where = append(where, "("+strings.Join(terms, " OR ")+")")
	}
	args = append(args, limit)
	return fmt.Sprintf(`SELECT %s FROM %s r WHERE %s ORDER BY r.ts DESC, r.seq DESC LIMIT ?`,
		reqColumns, reqPrefix+day, strings.Join(where, " AND ")), args
}

// Requests returns a page of the request log, ordered by ReceivedMS then
// insertion, newest first, and the cursor of the next page ("" when this is
// the last). Partitions are read newest first and the walk stops as soon as
// the page is full.
func (s *Store) Requests(ctx context.Context, q RequestQuery) ([]uxrecord.Request, string, error) {
	if q.DeploymentID == 0 {
		return nil, "", fmt.Errorf("%w: deployment is required", ErrBadQuery)
	}
	for _, result := range q.Results {
		if _, known := resultSQL[result]; !known {
			return nil, "", fmt.Errorf("%w: unknown result %q", ErrBadQuery, result)
		}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultRequestLimit
	}
	limit = min(limit, MaxRequestLimit)
	var cur *cursor
	if q.Cursor != "" {
		c, err := decodeCursor(q.Cursor)
		if err != nil {
			return nil, "", err
		}
		cur = &c
	}
	days, err := s.days(ctx, q.FromMS, q.ToMS)
	if err != nil {
		return nil, "", err
	}
	out := make([]uxrecord.Request, 0, limit)
	var lastSeq int64
	for _, day := range days {
		if cur != nil && day > cur.day {
			continue // only rows older than the cursor are wanted
		}
		// One row past the page tells whether another page exists.
		query, args := requestSQL(day, q, cur, limit+1-len(out))
		rows, err := s.reader.QueryContext(ctx, query, args...)
		if isNoSuchTable(err) {
			continue // dropped by retention since the listing
		}
		if err != nil {
			return nil, "", err
		}
		for rows.Next() {
			if len(out) == limit {
				rows.Close()
				last := out[len(out)-1]
				return out, cursor{ts: last.ReceivedMS, day: dayOf(last.ReceivedMS), seq: lastSeq}.encode(), nil
			}
			r, err := scanRequest(rows, &lastSeq)
			if err != nil {
				rows.Close()
				return nil, "", err
			}
			out = append(out, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, "", err
		}
	}
	return out, "", nil
}

// GridQuery selects one request-shape distribution.
type GridQuery struct {
	// DeploymentID narrows to one deployment; 0 is every deployment.
	DeploymentID uint
	// Kind is uxrecord.GridIO, uxrecord.GridUncachedTTFT or
	// uxrecord.GridSegments.
	Kind string
	// FromMS is floored to its 5-minute bucket; buckets starting after ToMS
	// are excluded. <= 0 is open.
	FromMS, ToMS int64
}

// Grid sums a distribution's cells over the buckets in range, ordered by
// (x, y). A deployment's query is served by the (deployment_id, kind,
// bucket_ms) index; the all-deployments query is a primary-key range scan of
// the time window across every deployment's cells, which is acceptable for
// cluster-wide views since the cells per bucket stay in the hundreds per
// deployment.
func (s *Store) Grid(ctx context.Context, q GridQuery) ([]uxrecord.Cell, error) {
	if q.Kind != uxrecord.GridIO && q.Kind != uxrecord.GridUncachedTTFT && q.Kind != uxrecord.GridSegments {
		return nil, fmt.Errorf("%w: unknown grid kind %q", ErrBadQuery, q.Kind)
	}
	where, args := bucketRange(q.DeploymentID, q.FromMS, q.ToMS)
	where = append(where, "kind = ?")
	args = append(args, q.Kind)
	sums := map[[2]int]int64{}
	err := s.eachDay(ctx, q.FromMS, q.ToMS, func(day string) (string, []any) {
		return fmt.Sprintf(`SELECT x, y, SUM(n) FROM %s WHERE %s GROUP BY x, y`,
			gridPrefix+day, strings.Join(where, " AND ")), args
	}, func(rows *sql.Rows) error {
		var x, y int
		var n int64
		if err := rows.Scan(&x, &y, &n); err != nil {
			return err
		}
		sums[[2]int{x, y}] += n
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]uxrecord.Cell, 0, len(sums))
	for k, n := range sums {
		out = append(out, uxrecord.Cell{X: k[0], Y: k[1], N: n})
	}
	slices.SortFunc(out, func(a, b uxrecord.Cell) int {
		if a.X != b.X {
			return a.X - b.X
		}
		return a.Y - b.Y
	})
	return out, nil
}

// CoverageStats counts the requests in a window: all of them, those whose
// reply carried usage (the ones in the io grid), and those that also carried
// a cache figure.
type CoverageStats struct {
	Total, WithUsage, WithCache int64
}

// Coverage counts every request (sampling does not apply) of a deployment,
// or of all deployments when deploymentID is 0, over the buckets in range
// (the same bucket rule as GridQuery).
func (s *Store) Coverage(ctx context.Context, deploymentID uint, fromMS, toMS int64) (CoverageStats, error) {
	where, args := bucketRange(deploymentID, fromMS, toMS)
	var out CoverageStats
	err := s.eachDay(ctx, fromMS, toMS, func(day string) (string, []any) {
		return fmt.Sprintf(`SELECT COALESCE(SUM(total), 0), COALESCE(SUM(with_usage), 0),
			COALESCE(SUM(with_cache), 0) FROM %s WHERE %s`, cntPrefix+day, strings.Join(where, " AND ")), args
	}, func(rows *sql.Rows) error {
		var c CoverageStats
		if err := rows.Scan(&c.Total, &c.WithUsage, &c.WithCache); err != nil {
			return err
		}
		out.Total += c.Total
		out.WithUsage += c.WithUsage
		out.WithCache += c.WithCache
		return nil
	})
	return out, err
}

// RequestsByDeployment counts every request (sampling does not apply) per
// deployment over the buckets in range (the same bucket rule as GridQuery),
// for the daily usage report.
func (s *Store) RequestsByDeployment(ctx context.Context, fromMS, toMS int64) (map[uint]int64, error) {
	where, args := bucketRange(0, fromMS, toMS)
	out := map[uint]int64{}
	err := s.eachDay(ctx, fromMS, toMS, func(day string) (string, []any) {
		return fmt.Sprintf(`SELECT deployment_id, COALESCE(SUM(total), 0) FROM %s WHERE %s GROUP BY deployment_id`,
			cntPrefix+day, strings.Join(where, " AND ")), args
	}, func(rows *sql.Rows) error {
		var id uint
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return err
		}
		out[id] += n
		return nil
	})
	return out, err
}

// Latency sums a deployment's latency breakdown over the buckets in range
// (the same bucket rule as GridQuery). Days written before the breakdown
// existed hold no sums and add nothing.
func (s *Store) Latency(ctx context.Context, deploymentID uint, fromMS, toMS int64) (uxrecord.LatencySums, error) {
	if deploymentID == 0 {
		return uxrecord.LatencySums{}, fmt.Errorf("%w: deployment is required", ErrBadQuery)
	}
	where, args := bucketRange(deploymentID, fromMS, toMS)
	out := uxrecord.LatencySums{SegmentMS: map[uxrecord.SegmentKind]float64{}}
	err := s.eachDay(ctx, fromMS, toMS, func(day string) (string, []any) {
		return fmt.Sprintf(`SELECT COALESCE(SUM(requests), 0), COALESCE(SUM(e2e_ms), 0), COALESCE(SUM(gateway_ms), 0),
			COALESCE(SUM(preprocess_ms), 0), COALESCE(SUM(queue_ms), 0), COALESCE(SUM(prefill_ms), 0), COALESCE(SUM(waiting_ms), 0),
			COALESCE(SUM(thinking_ms), 0), COALESCE(SUM(answering_ms), 0), COALESCE(SUM(tail_ms), 0),
			COALESCE(SUM(prefill_speed_tokens), 0), COALESCE(SUM(prefill_speed_ms), 0),
			COALESCE(SUM(thinking_speed_tokens), 0), COALESCE(SUM(thinking_speed_ms), 0),
			COALESCE(SUM(answering_speed_tokens), 0), COALESCE(SUM(answering_speed_ms), 0),
			COALESCE(SUM(decode_speed_tokens), 0), COALESCE(SUM(decode_speed_ms), 0), COALESCE(SUM(pd_prefill_ms), 0)
			FROM %s WHERE %s`, latPrefix+day, strings.Join(where, " AND ")), args
	}, func(rows *sql.Rows) error {
		var day uxrecord.LatencySums
		var gateway, preprocess, queue, prefill, waiting, thinking, answering, tail, pdPrefill float64
		if err := rows.Scan(&day.Requests, &day.EndToEndMS, &gateway, &preprocess, &queue, &prefill, &waiting, &thinking,
			&answering, &tail, &day.PrefillTokens, &day.PrefillMS, &day.ThinkingTokens, &day.ThinkingMS,
			&day.AnsweringTokens, &day.AnsweringMS, &day.DecodeTokens, &day.DecodeMS, &pdPrefill); err != nil {
			return err
		}
		day.SegmentMS = map[uxrecord.SegmentKind]float64{
			uxrecord.SegmentGateway: gateway, uxrecord.SegmentPreprocess: preprocess, uxrecord.SegmentQueue: queue, uxrecord.SegmentPrefill: prefill,
			uxrecord.SegmentWaiting: waiting, uxrecord.SegmentThinking: thinking,
			uxrecord.SegmentAnswering: answering, uxrecord.SegmentTail: tail, uxrecord.SegmentPDPrefill: pdPrefill,
		}
		out.Merge(day)
		return nil
	})
	return out, err
}

// bucketRange is the WHERE terms of a bucketed query.
func bucketRange(deploymentID uint, fromMS, toMS int64) ([]string, []any) {
	where := []string{"bucket_ms >= ?"}
	args := []any{max(0, floorBucket(fromMS))}
	if toMS > 0 {
		where = append(where, "bucket_ms <= ?")
		args = append(args, toMS)
	}
	if deploymentID != 0 {
		where = append(where, "deployment_id = ?")
		args = append(args, deploymentID)
	}
	return where, args
}

// eachDay runs a per-partition query over the days in range and hands every
// row to scan.
func (s *Store) eachDay(ctx context.Context, fromMS, toMS int64, build func(day string) (string, []any), scan func(*sql.Rows) error) error {
	days, err := s.days(ctx, floorBucket(fromMS), toMS)
	if err != nil {
		return err
	}
	for _, day := range days {
		query, args := build(day)
		rows, err := s.reader.QueryContext(ctx, query, args...)
		if isNoSuchTable(err) {
			continue
		}
		if err != nil {
			return err
		}
		for rows.Next() {
			if err := scan(rows); err != nil {
				rows.Close()
				return err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// days lists the partitions overlapping [fromMS, toMS], newest first.
func (s *Store) days(ctx context.Context, fromMS, toMS int64) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	all, err := listDays(s.reader)
	if err != nil {
		return nil, err
	}
	return daysInRange(all, fromMS, toMS), nil
}

// isNoSuchTable reports SQLite refusing a query on a day partition that
// retention dropped after it was listed. The driver (glebarez/go-sqlite) has
// only the generic SQLITE_ERROR code for it, so the text is read here, at the
// query this package ran (external text: SQLite "no such table: <name>").
func isNoSuchTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}
