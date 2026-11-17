package logstore

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// rowSelect is the projection every query uses; m is the partition alias.
const rowSelect = "m.id, m.ts, m.instance, m.node, m.level, m.source, m.line"

// scanEntries reads rows in whatever order the query produced.
func scanEntries(rows *sql.Rows, err error) ([]Entry, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.TimeMS, &e.Instance, &e.Node, &e.Level, &e.Source, &e.Line); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// partQuery is the per-partition SQL for one compiled filter.
type partQuery struct {
	from    string // meta table clause: "logs_YYYYMMDD m"
	ftsName string // set when a MATCH is compiled
	where   []string
	args    []any
	// tailWhere, when set, is where for a page read (selectSQL) with the
	// unselective terms kept off their indexes; see streamsFromTail.
	tailWhere []string
}

// streamsFromTail reports whether a filter's newest-first page is best read
// by walking the partition from its newest row. With nothing narrowing it but
// the source most lines carry and an open-ended time range, nearly every row
// matches, yet SQLite would otherwise pick the (source, ts) index for those
// terms and sort the whole day's match set by id before taking a page: the
// console's default first screen cost a sort of every line logged that day
// (hundreds of milliseconds at fleet scale, growing all day). An instance or
// node filter, a rare source or a closed time range is selective, so those
// keep their indexes.
func streamsFromTail(f Filter) bool {
	return f.Query == "" && f.Instance == "" && len(f.Instances) == 0 && f.Node == "" &&
		f.ToMS == 0 && (f.Source == "" || f.Source == SourceInstance)
}

// tailTerm keeps a term off any index: SQLite does not use an index for a
// column under a unary plus.
func tailTerm(term string) string { return "+" + term }

// compile builds the per-partition query for the filter. The FTS5 match
// argument leads the args (it feeds the table-valued function).
func compile(day string, f Filter) partQuery {
	meta := partitionPrefix + day
	q := partQuery{from: meta + " m"}
	if f.Query != "" {
		q.ftsName = meta + "_fts"
		q.args = append(q.args, f.Query)
	}
	if f.Instance != "" {
		// Prefix match on purpose: the UI only ever shows 8-char short IDs, so
		// a short ID typed into a filter must find the instance. The escape
		// keeps user % and _ literal; the (instance, ts, id) index still serves
		// the prefix.
		q.where = append(q.where, `m.instance LIKE ? ESCAPE '\'`)
		q.args = append(q.args, likeEscape(f.Instance)+"%")
	} else if len(f.Instances) > 0 {
		// Exact match against the full set - unlike Instance, callers here
		// (the service page's merged replica stream) always pass complete
		// UUIDs, so a prefix LIKE per entry would be both slower and looser
		// than it needs to be. Filter.validate has already rejected this
		// combined with Instance and capped its length, so the placeholder
		// count below is bounded by MaxInstances.
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(f.Instances)), ",")
		q.where = append(q.where, "m.instance IN ("+placeholders+")")
		for _, inst := range f.Instances {
			q.args = append(q.args, inst)
		}
	}
	if f.Node != "" {
		q.where = append(q.where, "m.node = ?")
		q.args = append(q.args, f.Node)
	}
	if f.Source != "" {
		q.where = append(q.where, "m.source = ?")
		q.args = append(q.args, f.Source)
	}
	if f.Level != "" {
		q.where = append(q.where,
			fmt.Sprintf(`(CASE m.level WHEN 'debug' THEN 0 WHEN 'info' THEN 1 WHEN 'warn' THEN 2 WHEN 'error' THEN 3 WHEN 'fatal' THEN 4 ELSE -1 END) >= %d`, rankOf(f.Level)))
	}
	if f.FromMS > 0 {
		q.where = append(q.where, "m.ts >= ?")
		q.args = append(q.args, f.FromMS)
	}
	if f.ToMS > 0 {
		q.where = append(q.where, "m.ts <= ?")
		q.args = append(q.args, f.ToMS)
	}
	if f.CursorTS > 0 {
		// Day-aware keyset in arrival order: the cursor's own partition keeps
		// only rows inserted before it (id < cursor), older partitions are
		// taken whole (their day start precedes the cursor's day).
		dayStart, err := partitionStartMS(partitionDay(f.CursorTS))
		if err != nil {
			dayStart = f.CursorTS
		}
		q.where = append(q.where, "(m.ts < ? OR m.id < ?)")
		q.args = append(q.args, dayStart, f.CursorID)
	}
	if streamsFromTail(f) {
		q.tailWhere = make([]string, len(q.where))
		for i, term := range q.where {
			if term == "m.source = ?" || term == "m.ts >= ?" {
				term = tailTerm(term)
			}
			q.tailWhere[i] = term
		}
	}
	return q
}

func (q partQuery) selectSQL(limit int, orderNewestFirst bool) string {
	order := "ASC"
	if orderNewestFirst {
		order = "DESC"
	}
	// Arrival order: the rowid is the insert sequence, so newest-first pages
	// stream from the tail of each partition instead of sorting the whole
	// match set by ts. Wall-clock ts stays a display/filter column;
	// same-second interleaving follows arrival.
	orderColumn := "m.id"
	if q.ftsName != "" {
		// FTS5 consumes this rowid order directly. Keep ORDER BY on the
		// outer SELECT: SQLite may discard a subquery's order during JOIN
		// flattening, breaking newest-first pagination as well as latency.
		orderColumn = "f.rowid"
	}
	where := q.where
	if q.tailWhere != nil {
		where = q.tailWhere
	}
	sqlStr := "SELECT " + rowSelect + " FROM " + q.matchedFrom()
	if len(where) > 0 {
		sqlStr += " WHERE " + strings.Join(where, " AND ")
	}
	return fmt.Sprintf("%s ORDER BY %s %s LIMIT %d", sqlStr, orderColumn, order, limit)
}

// matchedFrom keeps FTS as the outer loop. An ordinary JOIN lets SQLite
// choose the source/time index first and repeat MATCH for every log row;
// even a rare keyword then costs a scan of the entire time range.
func (q partQuery) matchedFrom() string {
	if q.ftsName != "" {
		return fmt.Sprintf("%s(?) f CROSS JOIN %s ON m.id = f.rowid", q.ftsName, q.from)
	}
	return q.from
}

func (q partQuery) countSQL(limit int64) string {
	sqlStr := "SELECT 1 FROM " + q.matchedFrom()
	if len(q.where) > 0 {
		sqlStr += " WHERE " + strings.Join(q.where, " AND ")
	}
	// Bound the rows visited inside the aggregate, not just the reported
	// total after an entire partition has already been counted.
	return fmt.Sprintf("SELECT count(*) FROM (%s LIMIT %d)", sqlStr, limit)
}

// likeEscape makes a user string literal for LIKE: %, _ and the escape char
// itself are escaped.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// errFTSSyntax marks a query SQLite's FTS5 rejected as invalid user input, so
// the caller can retry the input as a quoted phrase instead of surfacing a
// syntax error to the UI.
var errFTSSyntax = errors.New("invalid full-text query")

// ftsQueryError converts the SQLite driver's error for invalid FTS5 input into
// errFTSSyntax. The driver (glebarez/go-sqlite) reports both shapes only as
// SQLITE_ERROR text, so they are recognized here, on the error of the query
// this package ran (external text: SQLite): a parser error ("fts5: syntax
// error near ...") and the unknown-column error from `word:` column-filter
// syntax on a query that has no such column ("no such column: x").
func ftsQueryError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "fts5: syntax error") || strings.Contains(msg, "no such column: ") {
		return fmt.Errorf("%w: %w", errFTSSyntax, err)
	}
	return err
}

// Search returns the newest-arrival-first page for the filter, walking
// partitions from the newest day backwards and stopping as soon as the page
// is full.
//
// Missing time bounds include all retained partitions, for both keyword
// searches and tails. Search, totals and Histogram use the caller's range;
// the console supplies its default 24-hour window explicitly.
func (s *sqliteStore) Search(filter Filter) (Result, error) {
	if err := filter.validate(); err != nil {
		return Result{}, err
	}
	res, err := s.searchOnce(filter)
	if errors.Is(err, errFTSSyntax) && filter.Query != "" {
		filter.Query = quotePhrase(filter.Query)
		return s.searchOnce(filter)
	}
	return res, err
}

func (s *sqliteStore) searchOnce(filter Filter) (_ Result, err error) {
	defer func() { err = ftsQueryError(err) }()
	if filter.Limit <= 0 {
		filter.Limit = DefaultLimit
	}
	if filter.Limit > MaxLimit {
		filter.Limit = MaxLimit
	}
	days, err := listPartitions(s.reader)
	if err != nil {
		return Result{}, err
	}
	days = restrictPartitions(days, filter)

	// probeLimit asks for one row past filter.Limit so Result.Truncated can
	// be set here, once, instead of every caller re-deriving it by probing
	// the store with its own limit+1 (the api layer used to do exactly
	// that, which silently broke once filter.Limit was itself close to the
	// old hard 1000 cap - a limit+1 probe could get clamped right back down
	// to 1000, losing the signal at that boundary).
	probeLimit := filter.Limit + 1
	res := Result{Entries: make([]Entry, 0, filter.Limit)}
	for _, day := range days {
		pq := compile(day, filter)
		page, err := scanEntries(s.reader.Query(pq.selectSQL(probeLimit-len(res.Entries), true), pq.args...))
		if err != nil {
			return Result{}, err
		}
		res.Entries = append(res.Entries, page...)
		if len(res.Entries) >= probeLimit {
			break
		}
	}
	if len(res.Entries) > filter.Limit {
		res.Entries = res.Entries[:filter.Limit]
		res.Truncated = true
	}

	if filter.WithTotal {
		total, capped := int64(0), false
		for _, day := range days {
			pq := compile(day, filter)
			var n int64
			if err := s.reader.QueryRow(pq.countSQL(TotalCap-total), pq.args...).Scan(&n); err != nil {
				return Result{}, err
			}
			total += n
			if total >= TotalCap {
				total = TotalCap
				capped = true
				break
			}
		}
		res.Total, res.TotalCapped = total, capped
	}
	return res, nil
}

// Tail returns the most recent entries of an instance, oldest-first.
func (s *sqliteStore) Tail(instance string, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 100
	}
	res, err := s.Search(Filter{Instance: instance, Limit: limit})
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(res.Entries)-1; i < j; i, j = i+1, j-1 {
		res.Entries[i], res.Entries[j] = res.Entries[j], res.Entries[i]
	}
	return res.Entries, nil
}

// Context assembles before + match + after around (ts,id) for one instance.
// Window rows come from the match's own day partition; a window that would
// cross midnight truncates there (documented behavior).
//
// matchIndex is the index of the anchor row inside the returned slice, or -1
// when the anchor id is not in that partition. The anchor can legitimately be
// missing: retention prunes the boundary partition between a search and a
// click on one of its rows, and a caller may pass a ts from a different day
// than the id's. Such a window is still worth returning - the neighbouring
// lines are real - so the anchor's absence is reported by the index rather
// than by dropping the window or failing the read. -1 is deliberately chosen
// over len(rows): every bounds-checked read (0 <= i && i < len(rows)) rejects
// it, whereas len(rows) is a valid index once after rows exist and silently
// pointed callers at a neighbour before this contract existed.
func (s *sqliteStore) Context(instance, node string, ts, id int64, before, after int) ([]Entry, int, error) {
	if before < 0 || after < 0 {
		return nil, 0, fmt.Errorf("negative context window")
	}
	day := partitionDay(ts)
	name := partitionPrefix + day
	// Scope: same instance when the row has one (agent/server rows have
	// none), otherwise the same node, so an infrastructure warning still gets
	// a meaningful neighborhood.
	scope, sargs := "m.instance = ?", []any{instance}
	if instance == "" && node != "" {
		scope, sargs = "m.node = ?", []any{node}
	}
	beforeRows, err := scanEntries(s.reader.Query(
		fmt.Sprintf(`SELECT %s FROM %s m WHERE %s AND (m.ts < ? OR (m.ts = ? AND m.id < ?)) ORDER BY m.ts DESC, m.id DESC LIMIT ?`,
			rowSelect, name, scope),
		append(sargs, ts, ts, id, before)...))
	if err != nil {
		return nil, 0, err
	}
	match, err := scanEntries(s.reader.Query(
		fmt.Sprintf(`SELECT %s FROM %s m WHERE m.id = ?`, rowSelect, name), id))
	if err != nil {
		return nil, 0, err
	}
	afterRows, err := scanEntries(s.reader.Query(
		fmt.Sprintf(`SELECT %s FROM %s m WHERE %s AND (m.ts > ? OR (m.ts = ? AND m.id > ?)) ORDER BY m.ts ASC, m.id ASC LIMIT ?`,
			rowSelect, name, scope),
		append(sargs, ts, ts, id, after)...))
	if err != nil {
		return nil, 0, err
	}
	out := make([]Entry, 0, len(beforeRows)+1+len(afterRows))
	for i := len(beforeRows) - 1; i >= 0; i-- {
		out = append(out, beforeRows[i])
	}
	// The anchor is matched by primary key, so match holds zero or one row.
	// Deriving the index from len(out) before appending it made a missing
	// anchor report the position of the first after row - or len(out), one
	// past the end, when there were none.
	matchIndex := -1
	if len(match) == 1 {
		matchIndex = len(out)
	}
	out = append(out, match...)
	out = append(out, afterRows...)
	return out, matchIndex, nil
}

// Histogram counts rows per time bucket and severity. Bucket boundaries are
// unix-ms multiples of bucketMS (UTC-aligned; for wall-clock-aligned buckets
// the caller passes an already aligned bucketMS).
func (s *sqliteStore) Histogram(filter Filter, bucketMS int64) ([]Bucket, error) {
	if err := filter.validate(); err != nil {
		return nil, err
	}
	buckets, err := s.histogramOnce(filter, bucketMS)
	if errors.Is(err, errFTSSyntax) && filter.Query != "" {
		filter.Query = quotePhrase(filter.Query)
		return s.histogramOnce(filter, bucketMS)
	}
	return buckets, err
}

func (s *sqliteStore) histogramOnce(filter Filter, bucketMS int64) (_ []Bucket, err error) {
	defer func() { err = ftsQueryError(err) }()
	if bucketMS <= 0 {
		bucketMS = 60_000
	}
	days, err := listPartitions(s.reader)
	if err != nil {
		return nil, err
	}
	days = restrictPartitions(days, filter)
	merged := map[int64]map[string]int64{}
	for _, day := range days {
		pq := compile(day, filter)
		sqlStr := fmt.Sprintf(
			`SELECT (m.ts / %d) * %d AS bucket, m.level, count(*) FROM %s`,
			bucketMS, bucketMS, pq.matchedFrom())
		if len(pq.where) > 0 {
			sqlStr += " WHERE " + strings.Join(pq.where, " AND ")
		}
		sqlStr += " GROUP BY bucket, m.level"
		rows, err := s.reader.Query(sqlStr, pq.args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var start int64
			var level string
			var n int64
			if err := rows.Scan(&start, &level, &n); err != nil {
				rows.Close()
				return nil, err
			}
			if merged[start] == nil {
				merged[start] = map[string]int64{}
			}
			merged[start][level] += n
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	out := make([]Bucket, 0, len(merged))
	for start, counts := range merged {
		out = append(out, Bucket{StartMS: start, Counts: counts})
	}
	// Newest-first to match every other store read; the UI reverses for display.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].StartMS > out[j-1].StartMS; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}
