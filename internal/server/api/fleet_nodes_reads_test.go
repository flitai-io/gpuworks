package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// With the server's node snapshot on, the pages a console polls - the node
// list, its facets, the dashboard, storage usage - read the fleet once
// between changes, not once per request; and a change made through the API is
// on the very next page.
func TestNodePagesShareTheFleetSnapshot(t *testing.T) {
	r := newAutoRig(t, 3)
	r.st.EnableFleetNodeSnapshot(nil)
	reads := countWholeNodeReads(t, r.st)
	paths := []string{"/api/v1/nodes?page=1&page_size=20", "/api/v1/nodes/facets", "/api/v1/dashboard/summary", "/api/v1/storages/usage"}
	get := func(path string) []byte {
		t.Helper()
		rec := r.do(r.session, http.MethodGet, path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body)
		}
		return rec.Body.Bytes()
	}
	for _, path := range paths {
		get(path)
	}
	reads.Store(0)
	for range 3 {
		for _, path := range paths {
			get(path)
		}
	}
	if n := reads.Load(); n != 0 {
		t.Fatalf("12 requests with nothing changed read the whole fleet %d times, want 0", n)
	}

	if rec := r.do(r.session, http.MethodPatch, "/api/v1/nodes/au-2/name", `{"name":"renamed"}`); rec.Code != http.StatusOK {
		t.Fatalf("rename = %d %s", rec.Code, rec.Body)
	}
	var page dto.NodePage
	if err := json.Unmarshal(get("/api/v1/nodes?page=1&page_size=20"), &page); err != nil {
		t.Fatal(err)
	}
	for _, n := range page.Items {
		if n.UUID == "au-2" && n.Name != "renamed" {
			t.Fatalf("the page after a rename shows %q", n.Name)
		}
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("the page after a rename read the fleet %d times, want 1", n)
	}
}
