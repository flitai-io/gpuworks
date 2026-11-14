package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
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
