package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

func decodeDeploymentPage(t *testing.T, body []byte) dto.DeploymentPage {
	t.Helper()
	var page dto.DeploymentPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return page
}

func pageNames(page dto.DeploymentPage) []string {
	names := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		names = append(names, item.Name)
	}
	return names
}

// The page envelope and a list row, as literal wire JSON: the row carries the
// list's columns and none of the launch configuration.
func TestDeploymentPageWireJSON(t *testing.T) {
	ready := 1
	page := dto.DeploymentPage{Page: 2, PageSize: 1, Total: 3, Items: []dto.DeploymentListItem{{
		ID: 7, Name: "qwen", TemplateID: 3, TemplateDeviated: true, Engine: "vllm", Image: "vllm/vllm-openai:v0.10.1",
		ModelID: 2, ModelName: "Qwen/Qwen3-8B", ServedName: "qwen3", NodeUUID: "n-1", NodeName: "gpu3", NodeNames: []string{"gpu3", "gpu2"},
		DesiredState: "active", RuntimeState: "running", State: "running", Sleepable: true, Replicas: 2, ReadyReplicas: &ready,
		Health:     dto.DeploymentHealth{Status: "timeout", ThresholdSec: 60, Last: &dto.HealthProbe{AtMS: 9, Status: "timeout", LatencyMS: 60000}, Recent: []dto.HealthProbe{{AtMS: 9, Status: "timeout", LatencyMS: 60000}}},
		Operations: []dto.DeploymentOperation{{Type: "openai.chat", Endpoint: "/v1/chat/completions", Renderer: "chat"}},
		CreatedAt:  time.Date(2026, 10, 3, 1, 2, 3, 0, time.UTC),
	}}}
	got, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"items":[{"id":7,"name":"qwen","template_id":3,"template_deviated":true,"engine":"vllm","image":"vllm/vllm-openai:v0.10.1","model_id":2,"model_name":"Qwen/Qwen3-8B","served_name":"qwen3","node_uuid":"n-1","node_name":"gpu3","node_names":["gpu3","gpu2"],"desired_state":"active","runtime_state":"running","state":"running","sleepable":true,"replicas":2,"ready_replicas":1,"health":{"status":"timeout","threshold_sec":60,"last":{"at_ms":9,"status":"timeout","latency_ms":60000},"recent_mean_ms":null,"recent_ok":0,"recent":[{"at_ms":9,"status":"timeout","latency_ms":60000}]},"operations":[{"type":"openai.chat","endpoint":"/v1/chat/completions","renderer":"chat"}],"created_at":"2026-10-03T01:02:03Z"}],"page":2,"page_size":1,"total":3}`
	if string(got) != want {
		t.Fatalf("wire JSON\n got: %s\nwant: %s", got, want)
	}
	minimal, err := json.Marshal(dto.DeploymentListItem{ID: 1, Name: "bare", Operations: []dto.DeploymentOperation{}})
	if err != nil {
		t.Fatal(err)
	}
	const wantMinimal = `{"id":1,"name":"bare","template_deviated":false,"engine":"","model_id":0,"model_name":"","served_name":"","node_uuid":"","desired_state":"","runtime_state":"","state":"","sleepable":false,"replicas":0,"health":{"status":"","threshold_sec":0,"last":null,"recent_mean_ms":null,"recent_ok":0,"recent":null},"operations":[],"created_at":"0001-01-01T00:00:00Z"}`
	if string(minimal) != wantMinimal {
		t.Fatalf("optional fields\n got: %s\nwant: %s", minimal, wantMinimal)
	}
	var back dto.DeploymentPage
	if err := json.Unmarshal(got, &back); err != nil || !reflect.DeepEqual(back, page) {
		t.Fatalf("round trip = %+v err=%v", back, err)
	}
}

// A page holds the rows the full list holds at the same positions, each the
// list projection of that deployment's DTO, and nothing heavier.
func TestDeploymentPageSlicesTheListNewestFirst(t *testing.T) {
	f := newListFixture(t, 3)
	deps, err := f.st.ListDeployments()
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 10 {
		t.Fatalf("fixture seeds %d deployments, want 10", len(deps))
	}
	body := f.get(t, f.handler.listDeployments, "/api/v1/deployments?page=2&page_size=4")
	page := decodeDeploymentPage(t, body)
	if page.Page != 2 || page.PageSize != 4 || page.Total != 10 || len(page.Items) != 4 {
		t.Fatalf("page = page %d size %d total %d items %d, want 2/4/10/4", page.Page, page.PageSize, page.Total, len(page.Items))
	}
	for i, item := range page.Items {
		full := f.handler.deploymentDTO(&deps[4+i], false)
		want := deploymentListItem(full, deploymentPreparationSummary(&deps[4+i], full.ModelProgress))
		if !reflect.DeepEqual(item, want) {
			t.Errorf("row %d = %+v, want %+v", i, item, want)
		}
	}
	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	for _, heavy := range []string{"quick_params", "extra_args", "catalog_selection_snapshot", "instances", "model_progress"} {
		for _, item := range raw.Items {
			if _, ok := item[heavy]; ok {
				t.Errorf("a list row carries %s", heavy)
			}
		}
	}

	last := decodeDeploymentPage(t, f.get(t, f.handler.listDeployments, "/api/v1/deployments?page=3&page_size=4"))
	if last.Total != 10 || len(last.Items) != 2 || last.Items[1].ID != deps[9].ID {
		t.Fatalf("last page = %+v, want the oldest deployments on the last page", pageNames(last))
	}
	past := decodeDeploymentPage(t, f.get(t, f.handler.listDeployments, "/api/v1/deployments?page=9&page_size=4"))
	if past.Total != 10 || past.Items == nil || len(past.Items) != 0 {
		t.Fatalf("a page past the end = %+v, want no rows and the total", past)
	}
	// A page whose offset overflows an int is past the end too.
	huge := decodeDeploymentPage(t, f.get(t, f.handler.listDeployments, "/api/v1/deployments?page=9223372036854775807&page_size=200"))
	if huge.Total != 10 || huge.Items == nil || len(huge.Items) != 0 || huge.Page != 1<<63-1 {
		t.Fatalf("a huge page = %+v, want no rows and the total", huge)
	}
	defaults := decodeDeploymentPage(t, f.get(t, f.handler.listDeployments, "/api/v1/deployments?page=1"))
	if defaults.PageSize != listDefaultPageSize || len(defaults.Items) != 10 {
		t.Fatalf("page without a size = size %d, %d rows", defaults.PageSize, len(defaults.Items))
	}
}

// q matches the name, the served name or the model's name, ignoring case.
func TestDeploymentPageSearch(t *testing.T) {
	f := newListFixture(t, 12)
	for _, tc := range []struct {
		q    string
		want []string
	}{
		{q: "EXTRA-1", want: []string{"extra-1", "extra-10", "extra-11"}},
		{q: "node-gone", want: []string{"node-gone"}},
		// Only deployments whose model still exists carry its name.
		{q: "org/doomed", want: []string{}},
		{q: "nothing-like-it", want: []string{}},
	} {
		page := decodeDeploymentPage(t, f.get(t, f.handler.listDeployments, "/api/v1/deployments?page=1&q="+tc.q))
		got := pageNames(page)
		sort.Strings(got)
		if page.Total != len(tc.want) || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("q=%s matched %v (total %d), want %v", tc.q, got, page.Total, tc.want)
		}
	}
	served := decodeDeploymentPage(t, f.get(t, f.handler.listDeployments, "/api/v1/deployments?page=1&page_size=200&q=org/list"))
	if served.Total != 17 {
		t.Errorf("q=org/list (model name) matched %d, want the 17 deployments of that model", served.Total)
	}
}

// state filters by the state the list shows: each filtered page holds exactly
// the deployments the full list reports in that state.
func TestDeploymentPageStateFilterAgreesWithTheFullList(t *testing.T) {
	f := newListFixture(t, 0)
	var full []dto.Deployment
	if err := unmarshalList(f.get(t, f.handler.listDeployments, "/api/v1/deployments"), &full); err != nil {
		t.Fatal(err)
	}
	byState := map[string][]string{}
	for _, d := range full {
		byState[d.RuntimeState] = append(byState[d.RuntimeState], d.Name)
	}
	if len(byState) < 3 {
		t.Fatalf("the fixture covers only states %v; the filter is not exercised", byState)
	}
	for state, want := range byState {
		page := decodeDeploymentPage(t, f.get(t, f.handler.listDeployments, "/api/v1/deployments?page=1&state="+state))
		got := pageNames(page)
		sort.Strings(got)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("state=%s matched %v, want %v", state, got, want)
		}
	}
	both := decodeDeploymentPage(t, f.get(t, f.handler.listDeployments, "/api/v1/deployments?page=1&state=running,failed"))
	if both.Total != len(byState["running"])+len(byState["failed"]) {
		t.Errorf("state=running,failed matched %d, want %d", both.Total, len(byState["running"])+len(byState["failed"]))
	}

	// A delete in flight outranks the runtime, as the UI shows it.
	deps, _ := f.st.ListDeployments()
	if err := f.st.DB().Model(&deps[0]).Update("desired_state", "deleting").Error; err != nil {
		t.Fatal(err)
	}
	deleting := decodeDeploymentPage(t, f.get(t, f.handler.listDeployments, "/api/v1/deployments?page=1&state=deleting"))
	if deleting.Total != 1 || deleting.Items[0].ID != deps[0].ID {
		t.Errorf("state=deleting matched %v, want %s", pageNames(deleting), deps[0].Name)
	}
	// So a deployment being deleted is never offered as one that can serve.
	for _, item := range decodeDeploymentPage(t, f.get(t, f.handler.listDeployments, "/api/v1/deployments?page=1&state=running")).Items {
		if item.ID == deps[0].ID {
			t.Errorf("state=running still lists %s, which is being deleted", deps[0].Name)
		}
	}
}

// sort orders the whole filtered list before paging, ascending or with "-"
// descending; ties and an absent sort keep the natural (newest first) order.
func TestDeploymentPageSortsBeforePaging(t *testing.T) {
	f := newListFixture(t, 3)
	var full []dto.Deployment
	if err := unmarshalList(f.get(t, f.handler.listDeployments, "/api/v1/deployments"), &full); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(full))
	for _, d := range full {
		names = append(names, d.Name)
	}
	ascending := append([]string(nil), names...)
	sort.Slice(ascending, func(i, j int) bool { return strings.ToLower(ascending[i]) < strings.ToLower(ascending[j]) })
	var paged []string
	for page := 1; page <= 3; page++ {
		paged = append(paged, pageNames(decodeDeploymentPage(t, f.get(t, f.handler.listDeployments, fmt.Sprintf("/api/v1/deployments?page=%d&page_size=4&sort=name", page))))...)
	}
	if !reflect.DeepEqual(paged, ascending) {
		t.Fatalf("sort=name across pages = %v, want %v", paged, ascending)
	}
	desc := pageNames(decodeDeploymentPage(t, f.get(t, f.handler.listDeployments, "/api/v1/deployments?page=1&page_size=200&sort=-name")))
	if len(desc) != len(ascending) || desc[0] != ascending[len(ascending)-1] || desc[len(desc)-1] != ascending[0] {
		t.Fatalf("sort=-name = %v, want the reverse of %v", desc, ascending)
	}
	natural := pageNames(decodeDeploymentPage(t, f.get(t, f.handler.listDeployments, "/api/v1/deployments?page=1&page_size=200&sort=")))
	if !reflect.DeepEqual(natural, names) {
		t.Fatalf("an empty sort = %v, want the natural order %v", natural, names)
	}
	// Sorting by state groups the rows by the state the list shows.
	byState := decodeDeploymentPage(t, f.get(t, f.handler.listDeployments, "/api/v1/deployments?page=1&page_size=200&sort=state"))
	for i := 1; i < len(byState.Items); i++ {
		prev, cur := byState.Items[i-1], byState.Items[i]
		if effective(prev) > effective(cur) {
			t.Fatalf("sort=state: %s (%s) before %s (%s)", prev.Name, effective(prev), cur.Name, effective(cur))
		}
	}
}

// effective is a list row's state as the list orders it: a delete in flight
// outranks the runtime.
func effective(d dto.DeploymentListItem) string {
	if d.DesiredState == "deleting" {
		return "deleting"
	}
	return d.RuntimeState
}

// GET /nodes sorts the same way: ascending or "-" descending, before paging.
// The fixture's nodes have no display name, so their hostname names them;
// its list-gone node is deleted.
func TestNodePageSorts(t *testing.T) {
	f := newListFixture(t, 0)
	read := func(query string) []string {
		var page dto.NodePage
		if err := json.Unmarshal(f.get(t, f.handler.listNodes, "/api/v1/nodes?"+query), &page); err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(page.Items))
		for _, n := range page.Items {
			out = append(out, n.Hostname)
		}
		return out
	}
	if got := read("page=1&sort=name"); !reflect.DeepEqual(got, []string{"list-a", "list-b"}) {
		t.Fatalf("sort=name = %v", got)
	}
	if got := read("page=1&sort=-name"); !reflect.DeepEqual(got, []string{"list-b", "list-a"}) {
		t.Fatalf("sort=-name = %v", got)
	}
	if got := read("page=2&page_size=1&sort=-name"); !reflect.DeepEqual(got, []string{"list-a"}) {
		t.Fatalf("page 2 of sort=-name = %v", got)
	}
}

func TestListQueryRejectsMalformedPagesAndFilters(t *testing.T) {
	f := newListFixture(t, 0)
	for _, tc := range []struct {
		serve http.HandlerFunc
		query string
		code  string
	}{
		{f.handler.listDeployments, "page=0", ErrorListPageInvalid},
		{f.handler.listDeployments, "page=two", ErrorListPageInvalid},
		{f.handler.listDeployments, "page=1&page_size=0", ErrorListPageInvalid},
		{f.handler.listDeployments, "page=1&page_size=201", ErrorListPageInvalid},
		{f.handler.listDeployments, "page=1&state=waking", ErrorListFilterInvalid},
		{f.handler.listDeployments, "page=1&state=running,", ErrorListFilterInvalid},
		{f.handler.listDeployments, "page=1&sort=gpu_count", ErrorListFilterInvalid},
		{f.handler.listDeployments, "page=1&sort=--name", ErrorListFilterInvalid},
		{f.handler.listNodes, "page=1&sort=model", ErrorListFilterInvalid},
		{f.handler.listNodes, "page=1&state=running", ErrorListFilterInvalid},
		{f.handler.listNodes, "page=1&label=pool", ErrorListFilterInvalid},
		{f.handler.listNodes, "page=1&label==a", ErrorListFilterInvalid},
		{f.handler.listNodes, "page=1&label=gpuworks.ai/vendor=nosuchvendor", ErrorListFilterInvalid},
		{f.handler.listNodes, "page=1&label=gpuworks.ai/misspelled=x", ErrorListFilterInvalid},
		{f.handler.nodeFacets, "state=asleep", ErrorListFilterInvalid},
		// The template list pages the same way; a bad page is no longer
		// quietly replaced by the default.
		{f.handler.listInferenceTemplates, "page=0", ErrorListPageInvalid},
		{f.handler.listInferenceTemplates, "page=1&page_size=500", ErrorListPageInvalid},
		{f.handler.listInferenceTemplates, "page_size=x", ErrorListPageInvalid},
		{f.handler.nodeFacets, "label=gpuworks.ai/vendor=nosuchvendor", ErrorListFilterInvalid},
	} {
		rec := httptest.NewRecorder()
		tc.serve(rec, httptest.NewRequest(http.MethodGet, "/x?"+tc.query, nil))
		var body struct {
			Code   string         `json:"code"`
			Error  string         `json:"error"`
			Params map[string]any `json:"params"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusBadRequest || body.Code != tc.code || body.Error == "" || body.Params["name"] == nil {
			t.Errorf("%s = %d %s, want 400 %s with params", tc.query, rec.Code, rec.Body.String(), tc.code)
		}
	}
}

// Filtering and paging read the same rows the full list reads: the query
// count stays fixed however many deployments there are.
func TestDeploymentPageQueryCountDoesNotGrowWithTheFleet(t *testing.T) {
	count := func(extra int) int64 {
		f := newListFixture(t, extra)
		var queries atomic.Int64
		tally := func(*gorm.DB) { queries.Add(1) }
		if err := f.st.DB().Callback().Query().Before("gorm:query").Register("test:count_query", tally); err != nil {
			t.Fatal(err)
		}
		if err := f.st.DB().Callback().Row().Before("gorm:row").Register("test:count_row", tally); err != nil {
			t.Fatal(err)
		}
		f.get(t, f.handler.listDeployments, "/api/v1/deployments?page=1&page_size=5&state=running,failed,pending&q=e")
		return queries.Load()
	}
	small, large := count(0), count(200)
	if small == 0 || small != large {
		t.Fatalf("paged GET /deployments: %d queries for 7 rows, %d for 207", small, large)
	}
}

// Every paged list answers with the same envelope: the inference templates
// default to page 1 of 50, as the other lists do.
func TestInferenceTemplatePageUsesTheCommonEnvelope(t *testing.T) {
	f := newListFixture(t, 0)
	var page map[string]json.RawMessage
	if err := json.Unmarshal(f.get(t, f.handler.listInferenceTemplates, "/api/v1/inference-templates"), &page); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(page))
	for key := range page {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"items", "page", "page_size", "total"}) {
		t.Fatalf("envelope keys = %v", keys)
	}
	if string(page["items"]) != "[]" || string(page["page"]) != "1" || string(page["page_size"]) != "50" || string(page["total"]) != "0" {
		t.Fatalf("empty template page = %s %s %s %s", page["items"], page["page"], page["page_size"], page["total"])
	}
}
