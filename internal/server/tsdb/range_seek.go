package tsdb

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// seekBucketsMinStepMS is the step from which QueryRange reads one sample per
// bucket instead of every sample in the window. Agents report every 10 s, so
// a 30 s bucket already holds about three samples; measured at 200 nodes the
// seek path was 1.4x faster at 30 s, 2.8x at 60 s and 14x at 300 s, and 1.9x
// slower at 10 s, where every bucket holds a single sample. A var so tests
// can drive both paths over the same data.
var seekBucketsMinStepMS int64 = 30_000

// querySeekSeriesSQL lists the candidate series. Matchers other than the
// indexed columns are applied in Go before any sample is read, so a
// node-and-card query seeks only that card's buckets.
const querySeekSeriesSQL = `SELECT sr.id, sr.labels_json FROM series sr WHERE sr.metric = ?%s ORDER BY sr.id`

// querySeekBucketsSQL returns the newest sample of every bucket of every
// listed series: b walks the bucket starts covering [startMS, endMS] (aligned
// to multiples of the step, as the scan path buckets), and the correlated
// subquery seeks the (series_id, ts_ms) key backwards from the bucket's end,
// so the cost follows the points returned rather than the samples stored.
// Empty buckets are dropped in SQL. ids is a JSON array of series ids.
const querySeekBucketsSQL = `
	WITH RECURSIVE b(t) AS (SELECT ? UNION ALL SELECT t + ? FROM b WHERE t + ? <= ?)
	SELECT id, t, v FROM (
		SELECT ids.value AS id, b.t AS t, (
			SELECT sm.value FROM samples sm
			WHERE sm.series_id = ids.value AND sm.ts_ms >= max(b.t, ?) AND sm.ts_ms <= min(b.t + ? - 1, ?)
			ORDER BY sm.ts_ms DESC LIMIT 1
		) AS v
		FROM json_each(?) ids, b
	) WHERE v IS NOT NULL
	ORDER BY id, t`

// queryRangeBySeek answers QueryRange with one key seek per bucket. It
// returns what the scan path returns: matching series in id order, each with
// the last value of every non-empty bucket, and a series with no samples in
// the window still listed with empty Points. Both statements run in one read
// transaction so the series list and their samples come from one snapshot.
func (s *sqliteStore) queryRangeBySeek(metric string, matchers map[string]string, startMS, endMS, stepMS int64) ([]Series, error) {
	tx, err := s.reader.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	clause, extraArgs, rest := indexedMatcherClause("sr.", matchers)
	rows, err := tx.Query(fmt.Sprintf(querySeekSeriesSQL, clause), append([]any{metric}, extraArgs...)...)
	if err != nil {
		return nil, err
	}
	out := make([]Series, 0, 4)
	index := make(map[int64]int)
	var ids strings.Builder
	ids.WriteByte('[')
	for rows.Next() {
		var id int64
		var lj string
		if err := rows.Scan(&id, &lj); err != nil {
			rows.Close()
			return nil, err
		}
		labels := map[string]string{}
		if err := json.Unmarshal([]byte(lj), &labels); err != nil {
			rows.Close()
			return nil, err
		}
		if !matchesAll(labels, rest) {
			continue
		}
		if len(index) > 0 {
			ids.WriteByte(',')
		}
		ids.WriteString(strconv.FormatInt(id, 10))
		index[id] = len(out)
		out = append(out, Series{Labels: labels, Points: make([]Point, 0, 4)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids.WriteByte(']')
	if len(out) == 0 {
		return out, nil
	}

	first := startMS / stepMS * stepMS
	rows, err = tx.Query(querySeekBucketsSQL, first, stepMS, stepMS, endMS, startMS, stepMS, endMS, ids.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, t int64
		var v float64
		if err := rows.Scan(&id, &t, &v); err != nil {
			return nil, err
		}
		i := index[id]
		out[i].Points = append(out[i].Points, Point{TimeMS: t, Value: v})
	}
	return out, rows.Err()
}
