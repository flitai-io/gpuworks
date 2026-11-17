package api

import (
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

const (
	listDefaultPageSize = 50
	listMaxPageSize     = 200
)

// listQuery is a paged list request: GET /deployments and GET /nodes return
// one page when the request names a page or a page size, and every row (the
// original response, kept for scripts and programmatic clients) otherwise.
type listQuery struct {
	paged    bool
	page     int
	pageSize int
	// search is lowercased; a row matches when one of its names contains it.
	search string
	// states are the requested states; empty matches every state.
	states map[string]bool
}

// parseListQuery reads page, page_size, q and state. A malformed page or an
// unknown state is refused rather than ignored: a filter that silently matches
// everything reads as an answer.
func parseListQuery(r *http.Request, knownStates []string) (listQuery, error) {
	values := r.URL.Query()
	q := listQuery{page: 1, pageSize: listDefaultPageSize}
	if raw, ok := listParam(values, "page"); ok {
		q.paged = true
		page, err := strconv.Atoi(raw)
		if err != nil || page < 1 {
			return q, listPageError("page", raw)
		}
		q.page = page
	}
	if raw, ok := listParam(values, "page_size"); ok {
		q.paged = true
		size, err := strconv.Atoi(raw)
		if err != nil || size < 1 || size > listMaxPageSize {
			return q, listPageError("page_size", raw)
		}
		q.pageSize = size
	}
	q.search = strings.ToLower(strings.TrimSpace(values.Get("q")))
	if raw, ok := listParam(values, "state"); ok {
		q.states = map[string]bool{}
		for _, state := range strings.Split(raw, ",") {
			state = strings.TrimSpace(state)
			if !slices.Contains(knownStates, state) {
				return q, listFilterError("state", state)
			}
			q.states[state] = true
		}
	}
	return q, nil
}

func listParam(values map[string][]string, name string) (string, bool) {
	raw, ok := values[name]
	if !ok || len(raw) == 0 {
		return "", false
	}
	return strings.TrimSpace(raw[0]), true
}

func (q listQuery) matchesState(state string) bool {
	return len(q.states) == 0 || q.states[state]
}

// matchesSearch reports whether any of names contains the search text.
func (q listQuery) matchesSearch(names ...string) bool {
	if q.search == "" {
		return true
	}
	for _, name := range names {
		if strings.Contains(strings.ToLower(name), q.search) {
			return true
		}
	}
	return false
}

// pageOf cuts the matched rows to the requested page and converts only that
// page's rows to their wire form. A page past the end is empty, not null.
func pageOf[R, T any](rows []R, q listQuery, convert func([]R) []T) dto.ListPage[T] {
	start, end := q.window(len(rows))
	items := convert(rows[start:end])
	if items == nil {
		items = []T{}
	}
	return dto.ListPage[T]{Items: items, Page: q.page, PageSize: q.pageSize, Total: len(rows)}
}

// window is the [start, end) slice of total rows the requested page covers;
// a page past the end is empty.
func (q listQuery) window(total int) (int, int) {
	start := min(q.offset(), total)
	return start, start + min(q.pageSize, total-start)
}

// offset is the number of rows before the requested page. Any page number is
// accepted, so the product saturates at math.MaxInt instead of overflowing
// into a negative offset: a page no list can reach is simply past the end.
func (q listQuery) offset() int {
	if q.page-1 > math.MaxInt/q.pageSize {
		return math.MaxInt
	}
	return (q.page - 1) * q.pageSize
}

func listPageError(name, value string) error {
	return productError(ErrorListPageInvalid,
		fmt.Sprintf("%s must be a whole number (page from 1, page_size from 1 to %d), got %q", name, listMaxPageSize, value),
		map[string]any{"name": name, "value": value, "max_page_size": listMaxPageSize})
}

func listFilterError(name, value string) error {
	return productError(ErrorListFilterInvalid, fmt.Sprintf("unknown %s filter %q", name, value),
		map[string]any{"name": name, "value": value})
}
