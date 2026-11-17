package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

func decodeInstancePage(t *testing.T, body []byte) dto.ListPage[dto.Instance] {
	t.Helper()
	var page dto.ListPage[dto.Instance]
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return page
}

func instanceUUIDs(items []dto.Instance) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.UUID)
	}
	return out
}

// GET /instances with page parameters returns one page of the full list,
// newest first, each row byte for byte the row the full list shows; without
// them it still returns every instance (scripts and the assistant read it so).
func TestInstancePageSlicesTheListNewestFirst(t *testing.T) {
	f := newListFixture(t, 3)
	var full []dto.Instance
	if err := unmarshalList(f.get(t, f.handler.listInstances, "/api/v1/instances"), &full); err != nil {
		t.Fatal(err)
	}
	// The fixture seeds 6 instances plus one per extra deployment.
	if len(full) != 9 {
		t.Fatalf("full list has %d instances, want 9", len(full))
	}
	page := decodeInstancePage(t, f.get(t, f.handler.listInstances, "/api/v1/instances?page=2&page_size=4"))
	if page.Page != 2 || page.PageSize != 4 || page.Total != 9 || len(page.Items) != 4 {
		t.Fatalf("page = page %d size %d total %d items %d, want 2/4/9/4", page.Page, page.PageSize, page.Total, len(page.Items))
	}
	if !reflect.DeepEqual(page.Items, full[4:8]) {
		t.Fatalf("page 2 = %v, want %v", instanceUUIDs(page.Items), instanceUUIDs(full[4:8]))
	}
	past := decodeInstancePage(t, f.get(t, f.handler.listInstances, "/api/v1/instances?page=9&page_size=4"))
	if past.Total != 9 || past.Items == nil || len(past.Items) != 0 {
		t.Fatalf("a page past the end = %+v, want no rows and the total", past)
	}
}

// q matches the instance UUID, its deployment's name or its node's name,
// ignoring case; state matches the instance's own state.
func TestInstancePageSearchAndState(t *testing.T) {
	f := newListFixture(t, 12)
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"q=INST-EXTRA-1", []string{"inst-extra-1", "inst-extra-10", "inst-extra-11"}},
		{"q=multi", []string{"inst-multi-0", "inst-multi-1"}},
		// The node's name: list-b runs one rank of multi and the replica
		// scheduled away from list-a.
		{"q=list-b", []string{"inst-multi-1", "inst-scheduled-away"}},
		{"q=nothing-like-it", []string{}},
		{"state=stopped", []string{"inst-scheduled-away-stopped"}},
		{"q=scheduled-away&state=running", []string{"inst-scheduled-away"}},
		{"q=scheduled-away&state=running,stopped", []string{"inst-scheduled-away", "inst-scheduled-away-stopped"}},
	} {
		page := decodeInstancePage(t, f.get(t, f.handler.listInstances, "/api/v1/instances?page=1&page_size=200&"+tc.query))
		got := instanceUUIDs(page.Items)
		sort.Strings(got)
		if page.Total != len(tc.want) || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s matched %v (total %d), want %v", tc.query, got, page.Total, tc.want)
		}
	}
}

func TestInstancePageRejectsMalformedQueries(t *testing.T) {
	f := newListFixture(t, 0)
	for _, query := range []string{"page=0", "page_size=201", "page=x", "state=asleep", "page=1&state=running,bogus"} {
		rec := httptest.NewRecorder()
		f.handler.listInstances(rec, httptest.NewRequest(http.MethodGet, "/api/v1/instances?"+query, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400: %s", query, rec.Code, rec.Body.String())
		}
	}
}

// A page number so large that (page-1)*page_size overflows is a page past the
// end: no rows, the true total, the page echoed back, never a panic.
func TestInstancePageHugePageIsEmpty(t *testing.T) {
	f := newListFixture(t, 3)
	for _, query := range []string{
		"page=9223372036854775807&page_size=200",
		"page=9223372036854775807&page_size=1",
		"page=4611686018427387905&page_size=2",
	} {
		rec := httptest.NewRecorder()
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("%s panicked: %v", query, p)
				}
			}()
			f.handler.listInstances(rec, httptest.NewRequest(http.MethodGet, "/api/v1/instances?"+query, nil))
		}()
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", query, rec.Code, rec.Body.String())
		}
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
			t.Fatal(err)
		}
		wantPage := strings.TrimPrefix(strings.Split(query, "&")[0], "page=")
		if string(wire["items"]) != "[]" || string(wire["total"]) != "9" || string(wire["page"]) != wantPage {
			t.Errorf("%s = %s, want items [], total 9 and page %s", query, rec.Body.String(), wantPage)
		}
	}
}

// Without page parameters q and state still filter, the whole match comes
// back in the plain list envelope, and an unknown state is refused.
func TestInstanceListFiltersWithoutPaging(t *testing.T) {
	f := newListFixture(t, 0)
	body := f.get(t, f.handler.listInstances, "/api/v1/instances?state=stopped")
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(wire))
	for k := range wire {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"items", "total"}) || string(wire["total"]) != "1" {
		t.Fatalf("state=stopped without paging = %s, want {items,total} with total 1", body)
	}
	var items []map[string]any
	if err := json.Unmarshal(wire["items"], &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0]["uuid"] != "inst-scheduled-away-stopped" || items[0]["state"] != "stopped" {
		t.Fatalf("state=stopped items = %s", wire["items"])
	}
	var multi []dto.Instance
	if err := unmarshalList(f.get(t, f.handler.listInstances, "/api/v1/instances?q=multi"), &multi); err != nil {
		t.Fatal(err)
	}
	if got := instanceUUIDs(multi); !reflect.DeepEqual(got, []string{"inst-multi-1", "inst-multi-0"}) && !reflect.DeepEqual(got, []string{"inst-multi-0", "inst-multi-1"}) {
		t.Fatalf("q=multi without paging matched %v, want the two multi ranks", got)
	}

	rec := httptest.NewRecorder()
	f.handler.listInstances(rec, httptest.NewRequest(http.MethodGet, "/api/v1/instances?state=foo", nil))
	want := `{"code":"LIST_FILTER_INVALID","error":"unknown state filter \"foo\"","params":{"name":"state","value":"foo"}}`
	if rec.Code != http.StatusBadRequest || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("state=foo = %d %s, want 400 %s", rec.Code, rec.Body.String(), want)
	}
}

// The window never overflows: a page beyond what an int can address is past
// the end of any list.
func TestListWindowHugePage(t *testing.T) {
	for _, tc := range []struct {
		page, size, total, start, end int
	}{
		{1, 50, 0, 0, 0},
		{2, 4, 9, 4, 8},
		{3, 4, 9, 8, 9},
		{4, 4, 9, 9, 9},
		{1<<63 - 1, 200, 9, 9, 9},
		{1<<63 - 1, 1, 9, 9, 9},
		{1 << 62, 2, 9, 9, 9},
		{1<<63 - 1, 200, 1<<63 - 1, 1<<63 - 1, 1<<63 - 1},
	} {
		q := listQuery{paged: true, page: tc.page, pageSize: tc.size}
		start, end := q.window(tc.total)
		if start != tc.start || end != tc.end {
			t.Errorf("page %d size %d of %d = [%d,%d), want [%d,%d)", tc.page, tc.size, tc.total, start, end, tc.start, tc.end)
		}
	}
}
