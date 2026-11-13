package aihelper

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func mustCall(t *testing.T, env ToolEnv, tool, args string) string {
	t.Helper()
	out, err := callBuiltin(tool, env, args)
	if err != nil {
		t.Fatalf("%s %s: %v", tool, args, err)
	}
	return out
}

func callErr(t *testing.T, env ToolEnv, tool, args string) string {
	t.Helper()
	out, err := callBuiltin(tool, env, args)
	if err == nil {
		t.Fatalf("%s %s answered %q, want an error", tool, args, out)
	}
	return err.Error()
}

func lines(s string) []string { return strings.Split(s, "\n") }

func assertIssued(t *testing.T, f *fakeAPI, want ...string) {
	t.Helper()
	got := f.issued()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("reads issued:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestAPIToolsWithoutAReaderSayItIsUnavailable(t *testing.T) {
	for tool, args := range map[string]string{"search_api": `{"query":"x"}`, "call_api": `{"path":"/api/v1/nodes"}`, "search_metrics": `{"query":"x"}`, "cluster_overview": `{}`} {
		if got := callErr(t, apiEnv{}, tool, args); got != errNoAPI.Error() {
			t.Fatalf("%s without an API: %q", tool, got)
		}
	}
}

func TestSearchAPIRanksPathMatchesFirst(t *testing.T) {
	out := mustCall(t, apiEnv{newFakeAPI()}, "search_api", `{"query":"gpu"}`)
	ls := lines(out)
	if !strings.HasPrefix(ls[0], "GET /api/v1/gpus — 集群 GPU 清单") {
		t.Fatalf("first hit %q, want the route whose path matches", ls[0])
	}
	if len(ls) < 2 {
		t.Fatalf("no description-only matches listed: %q", out)
	}
	for _, l := range ls[1:] {
		route := strings.Fields(l)[1]
		if strings.Contains(route, "gpu") {
			t.Fatalf("path match %q ranked after the first", l)
		}
		if !strings.Contains(strings.ToLower(RouteDoc(route)), "gpu") {
			t.Fatalf("%q listed without matching", l)
		}
	}
}

func TestSearchAPIClipsLongDescriptions(t *testing.T) {
	const route = "/api/v1/gpus"
	previous := routeDocs[route]
	t.Cleanup(func() { routeDocs[route] = previous })
	routeDocs[route] = strings.Repeat("GPU capacity. ", 100)
	out := searchAPI([]string{route}, "gpu")
	const prefix = "GET /api/v1/gpus — "
	if !strings.HasSuffix(out, "…") || len(out) > len(prefix)+600+len("…") {
		t.Fatalf("long description not cut: %d bytes", len(out))
	}
}

func TestSearchAPIFindsDiagnosticFields(t *testing.T) {
	cases := []struct {
		query string
		route string
	}{
		{"foreign_processes", "/api/v1/gpus"},
		{"retry_exhausted", "/api/v1/deployments/{id}"},
		{"client_closed", "/api/v1/monitor/deployments/{id}/requests"},
		{"cached_nodes", "/api/v1/models"},
		{"NVLink", "/api/v1/ai/models/{id}/fit"},
	}
	var routes []string
	for _, c := range cases {
		routes = append(routes, c.route)
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			out := searchAPI(routes, c.query)
			for _, line := range lines(out) {
				if strings.HasPrefix(line, "GET "+c.route+" — ") {
					return
				}
			}
			t.Fatalf("cannot discover %s by %q: %s", c.route, c.query, out)
		})
	}
}

func TestSearchAPIShowsGPUAllocationFilterValues(t *testing.T) {
	out := mustCall(t, apiEnv{newFakeAPI()}, "search_api", `{"query":"allocation_state"}`)
	for _, line := range lines(out) {
		if !strings.HasPrefix(line, "GET /api/v1/gpus — ") {
			continue
		}
		for _, state := range []string{"available", "partial", "allocated", "unusable", "unknown"} {
			if !regexp.MustCompile(`\b` + state + `\b`).MatchString(line) {
				t.Errorf("assistant cannot choose allocation_state=%s from the search result: %s", state, line)
			}
		}
		return
	}
	t.Fatalf("GPU allocation filter not discoverable: %s", out)
}

func TestSearchAPINeverListsDeniedOrUndocumentedRoutes(t *testing.T) {
	f := newFakeAPI()
	// Every one of these is a route of the fake router; none may be offered.
	for _, q := range []string{"ssh terminal apikeys events", "undocumented thing"} {
		if out := mustCall(t, apiEnv{f}, "search_api", `{"query":"`+q+`"}`); out != "no endpoint matches; try other words (nodes, gpus, models, deployments, instances, logs, metrics, images, storages, networks)" {
			t.Fatalf("query %q answered %q", q, out)
		}
	}
	// With no words every readable, documented route is listed - and still
	// none of the others.
	out := mustCall(t, apiEnv{f}, "search_api", `{"query":""}`)
	var listed []string
	for _, l := range lines(out) {
		listed = append(listed, strings.Fields(l)[1])
	}
	want := "/api/v1/benchmarks/{id}/report /api/v1/dashboard/summary /api/v1/deployments /api/v1/deployments/{id} /api/v1/gpus /api/v1/logs/search /api/v1/monitor/metrics /api/v1/nodes /api/v1/nodes/facets /api/v1/nodes/{uuid}"
	if strings.Join(listed, " ") != want {
		t.Fatalf("listed %v\nwant   %s", listed, want)
	}
	if len(f.issued()) != 0 {
		t.Fatalf("search_api read the API: %v", f.issued())
	}
}

func TestSearchAPICapsItsAnswer(t *testing.T) {
	f := newFakeAPI()
	ok := func(w http.ResponseWriter, _ *http.Request) {
		writeFake(w, 200, map[string]any{"items": []any{}, "total": 0})
	}
	for _, p := range []string{"/api/v1/models", "/api/v1/images", "/api/v1/benchmarks", "/api/v1/dgpus", "/api/v1/engines", "/api/v1/inference-templates"} {
		f.handle(p, ok)
	}
	out := mustCall(t, apiEnv{f}, "search_api", `{"query":""}`)
	ls := lines(out)
	if len(ls) != 13 || ls[12] != "(4 more match; use more specific words)" {
		t.Fatalf("%d lines, last %q", len(ls), ls[len(ls)-1])
	}
}

func TestCallAPIAppliesJQ(t *testing.T) {
	f := newFakeAPI()
	env := apiEnv{f}
	if out := mustCall(t, env, "call_api", `{"path":"/api/v1/nodes","jq":".total"}`); out != "2" {
		t.Fatalf("one output = %q", out)
	}
	// Several outputs come back as one array.
	if out := mustCall(t, env, "call_api", `{"path":"/api/v1/nodes","jq":".items[] | {name, state}"}`); out != `[{"name":"gpu-a","state":"online"},{"name":"gpu-b","state":"offline"}]` {
		t.Fatalf("several outputs = %q", out)
	}
	// The filter sees the redacted answer, never the secret.
	if out := mustCall(t, env, "call_api", `{"path":"/api/v1/nodes/`+uuidA+`","jq":".token"}`); out != `"[redacted]"` {
		t.Fatalf("token through jq = %q", out)
	}
}

func TestCallAPIJQFailureReturnsTheBodyAndTheStructure(t *testing.T) {
	env := apiEnv{newFakeAPI()}
	outline := `The answer's structure: {"items":[{"created_at":"string","gpu_count":"number","gpus":["{…}","×2"],"has_ssh_credential":"bool","hostname":"string","labels":"{…}","last_seen_at":"string","name":"string","scheduling":"string","state":"string","uuid":"string"},"×2"],"total":"number"}`
	for _, jq := range []string{".items[", ".total | keys"} {
		out := mustCall(t, env, "call_api", `{"path":"/api/v1/nodes","jq":"`+jq+`"}`)
		if !strings.Contains(out, `"name":"gpu-a"`) || !strings.Contains(out, "filter did not apply") || !strings.Contains(out, outline) || strings.Contains(out, "jq:") {
			t.Fatalf("jq %s => %s", jq, out)
		}
	}
	if !strings.Contains(callAPITool().Description, "`.items[].points[-1][1]`") || !strings.Contains(callAPITool().Description, "no items") {
		t.Fatal("call_api no longer shows the single-object and metric-point filters")
	}
}

// The list filter from the tool's own examples, pointed at one node, does not
// match that body. The read still succeeded: the step must carry the node,
// and must not be a tool error a caller can classify by the text "jq:".
func TestCallAPIJQMismatchReturnsTheBody(t *testing.T) {
	env := apiEnv{newFakeAPI()}
	args := `{"path":"/api/v1/nodes/` + uuidA + `","jq":".items[] | {name, state}"}`
	out := mustCall(t, env, "call_api", args)
	if !strings.Contains(out, "gpu-a") || !strings.Contains(out, "cannot iterate over: null") || !strings.Contains(out, "The answer's structure:") {
		t.Fatalf("mismatch = %s", out)
	}
	if strings.Contains(out, "jq:") {
		t.Fatalf("mismatch is still tagged jq: %s", out)
	}
	tool, ok := LookupTool("call_api")
	if !ok {
		t.Fatal("no call_api")
	}
	_, errText := tool.Run(t.Context(), env, json.RawMessage(args))
	if errText != "" {
		t.Fatalf("step error = %q", errText)
	}
}

// logLines serves n log lines from /api/v1/logs/search.
func logLines(f *fakeAPI, n int, line string) {
	f.handle("/api/v1/logs/search", func(w http.ResponseWriter, _ *http.Request) {
		items := make([]any, n)
		for i := range items {
			items[i] = map[string]any{"id": i + 1, "instance": "inst-1", "node": uuidA, "level": "info", "source": "container", "time_ms": fakeNow.UnixMilli(), "line": line}
		}
		writeFake(w, 200, map[string]any{"items": items})
	})
}

func TestCallAPICutsArraysToTheLimit(t *testing.T) {
	f := newFakeAPI()
	logLines(f, 250, "ok")
	env := apiEnv{f}
	ids := func(out string) []string {
		var raw []any
		if err := json.Unmarshal([]byte(out), &raw); err != nil {
			t.Fatalf("%q: %v", out, err)
		}
		got := make([]string, len(raw))
		for i, x := range raw {
			got[i] = fmt.Sprint(x)
		}
		return got
	}
	more := func(n int) string {
		return fmt.Sprintf("…%d more items (raise limit, page with the endpoint's own parameters, or filter with jq)", n)
	}
	for _, c := range []struct {
		args       string
		kept, more int
	}{
		{`{"path":"/api/v1/logs/search","jq":"[.items[].id]"}`, 20, 230},
		{`{"path":"/api/v1/logs/search","jq":"[.items[].id]","limit":3}`, 3, 247},
		{`{"path":"/api/v1/logs/search","jq":"[.items[].id]","limit":0}`, 20, 230},
		{`{"path":"/api/v1/logs/search","jq":"[.items[].id]","limit":5000}`, 200, 50},
	} {
		got := ids(mustCall(t, env, "call_api", c.args))
		if len(got) != c.kept+1 || got[0] != "1" || got[c.kept-1] != fmt.Sprint(c.kept) || got[c.kept] != more(c.more) {
			t.Fatalf("%s: %d entries, last %q", c.args, len(got), got[len(got)-1])
		}
	}
	// jq runs on the whole answer, before the cut.
	if out := mustCall(t, env, "call_api", `{"path":"/api/v1/logs/search","jq":".items | length"}`); out != "250" {
		t.Fatalf("jq saw %s items", out)
	}
}

func TestCallAPICutsLongStrings(t *testing.T) {
	f := newFakeAPI()
	logLines(f, 1, strings.Repeat("é", 400))
	out := mustCall(t, apiEnv{f}, "call_api", `{"path":"/api/v1/logs/search","jq":".items[0].line"}`)
	if out != `"`+strings.Repeat("é", 300)+`…[100 more chars]"` {
		t.Fatalf("cut string = %q", out)
	}
}

// An answer past the cap is cut with a note that says how to narrow the
// read, and the whole result - note included - stays within what the runner
// feeds back uncut.
func TestCallAPICapsTheAnswerWithAHint(t *testing.T) {
	f := newFakeAPI()
	logLines(f, 200, strings.Repeat("x", 250))
	env := apiEnv{f}
	out := mustCall(t, env, "call_api", `{"path":"/api/v1/logs/search","limit":200}`)
	if len(out) > maxAnswerChars || len(out) > maxToolResultChars {
		t.Fatalf("answer is %d bytes", len(out))
	}
	note := out[strings.LastIndex(out, "\n[truncated: "):]
	want := `. Select fields with jq or lower limit. The answer's structure: {"items":[{"id":"number","instance":"string","level":"string","line":"string","node":"string","source":"string","time_ms":"string"},"×200"]}]`
	if !strings.HasSuffix(note, want) || !strings.Contains(note, " chars shown.") {
		t.Fatalf("note = %q", note)
	}
	var shown, total int
	if _, err := fmt.Sscanf(note, "\n[truncated: %d of %d chars shown.", &shown, &total); err != nil {
		t.Fatalf("note %q: %v", note, err)
	}
	if body := strings.TrimSuffix(out[:len(out)-len(note)], "…"); len(body) != shown || total <= maxAnswerChars {
		t.Fatalf("note says %d of %d shown; %d shown", shown, total, len(body))
	}
	// Through the runner the note survives.
	if fed := truncateToolResult(out); fed != out {
		t.Fatal("the runner cut call_api's answer again")
	}
	// With jq the note does not repeat the structure.
	out = mustCall(t, env, "call_api", `{"path":"/api/v1/logs/search","limit":200,"jq":".items"}`)
	if !strings.HasSuffix(out, " chars shown. Select fields with jq or lower limit.]") || len(out) > maxAnswerChars {
		t.Fatalf("jq answer tail = %q", out[max(0, len(out)-120):])
	}
}

func TestCallAPIRedactsSecretsOnly(t *testing.T) {
	f := newFakeAPI()
	f.handle("/api/v1/benchmarks/{id}", func(w http.ResponseWriter, _ *http.Request) {
		writeFake(w, 200, map[string]any{
			"id": 7, "prompt_tokens": 512, "has_ssh_credential": true, "token_prefix": "gw_", "hf_token": "", "tokenizer_ref": "Qwen/Qwen3-8B",
			"endpoint": map[string]any{"api_key": "sk-live", "password": "hunter2"}, "secrets": []any{map[string]any{"client_secret": "s3"}},
		})
	})
	out := mustCall(t, apiEnv{f}, "call_api", `{"path":"/api/v1/benchmarks/7"}`)
	want := `{"endpoint":{"api_key":"[redacted]","password":"[redacted]"},"has_ssh_credential":true,"hf_token":"","id":7,"prompt_tokens":512,"secrets":[{"client_secret":"[redacted]"}],"token_prefix":"gw_","tokenizer_ref":"Qwen/Qwen3-8B"}`
	if out != want {
		t.Fatalf("got  %s\nwant %s", out, want)
	}
	// A node's agent token, the field the real detail carries for admins.
	out = mustCall(t, apiEnv{f}, "call_api", `{"path":"/api/v1/nodes/`+uuidA+`","jq":"{token, has_ssh_credential}"}`)
	if out != `{"has_ssh_credential":true,"token":"[redacted]"}` || strings.Contains(out, "nt_secret") {
		t.Fatalf("node detail = %s", out)
	}
}

func TestCallAPIShowsTimesWithTheirAge(t *testing.T) {
	atFakeNow(t)
	f := newFakeAPI()
	env := apiEnv{f}
	out := mustCall(t, env, "call_api", `{"path":"/api/v1/nodes/`+uuidB+`","jq":"{last_seen_at, created_at}"}`)
	if out != `{"created_at":"2026-10-07T12:00:00Z (4.0d ago)","last_seen_at":"2026-10-11T10:30:00Z (1.5h ago)"}` {
		t.Fatalf("node times = %s", out)
	}
	// Times are aged after jq: a field jq keeps by name is shown with its
	// age, and jq itself still compares time_ms as the number it is.
	out = mustCall(t, env, "call_api", `{"path":"/api/v1/logs/search","jq":"{time_ms: .items[0].time_ms}"}`)
	if out != `{"time_ms":"2026-10-11T11:55:00Z (5m ago)"}` {
		t.Fatalf("log time = %s", out)
	}
	out = mustCall(t, env, "call_api", `{"path":"/api/v1/logs/search","jq":"[.items[] | select(.time_ms > 1791719670000)] | length"}`)
	if out != `1` {
		t.Fatalf("numeric time filter = %s", out)
	}
	// Durations named *_ms are small numbers and stay as they are.
	f.handle("/api/v1/benchmarks/{id}", func(w http.ResponseWriter, _ *http.Request) {
		writeFake(w, 200, map[string]any{"ttft_ms": 350, "first_token_ms": 1234.5, "e2e_ms": 0, "finished_at": nil})
	})
	if out := mustCall(t, env, "call_api", `{"path":"/api/v1/benchmarks/7"}`); out != `{"e2e_ms":0,"finished_at":null,"first_token_ms":1234.5,"ttft_ms":350}` {
		t.Fatalf("durations = %s", out)
	}
}

func TestCallAPIResolvesNodeNames(t *testing.T) {
	t.Run("in the path", func(t *testing.T) {
		f := newFakeAPI()
		out := mustCall(t, apiEnv{f}, "call_api", `{"path":"/api/v1/nodes/gpu-b","jq":".uuid"}`)
		if out != `"`+uuidB+`"` {
			t.Fatalf("answer %s", out)
		}
		assertIssued(t, f, "/api/v1/nodes?page=1&page_size=50&q=gpu-b", "/api/v1/nodes/"+uuidB)
	})
	t.Run("by hostname", func(t *testing.T) {
		f := newFakeAPI()
		mustCall(t, apiEnv{f}, "call_api", `{"path":"/api/v1/nodes/host-a","jq":".name"}`)
		assertIssued(t, f, "/api/v1/nodes?page=1&page_size=50&q=host-a", "/api/v1/nodes/"+uuidA)
	})
	t.Run("in the node query parameter", func(t *testing.T) {
		f := newFakeAPI()
		out := mustCall(t, apiEnv{f}, "call_api", `{"path":"/api/v1/logs/search","query":{"node":"gpu-a","level":"error"},"jq":"[.items[].line]"}`)
		if out != `["NCCL WARN socket timeout"]` {
			t.Fatalf("answer %s", out)
		}
		assertIssued(t, f, "/api/v1/nodes?page=1&page_size=50&q=gpu-a", "/api/v1/logs/search?level=error&node="+uuidA)
	})
	t.Run("a uuid or server is not looked up", func(t *testing.T) {
		f := newFakeAPI()
		mustCall(t, apiEnv{f}, "call_api", `{"path":"/api/v1/nodes/`+uuidA+`","jq":".name"}`)
		mustCall(t, apiEnv{f}, "call_api", `{"path":"/api/v1/logs/search","query":{"node":"server"}}`)
		assertIssued(t, f, "/api/v1/nodes/"+uuidA, "/api/v1/logs/search?node=server")
	})
	t.Run("an unknown name", func(t *testing.T) {
		f := newFakeAPI()
		if got := callErr(t, apiEnv{f}, "call_api", `{"path":"/api/v1/nodes/gpu-z"}`); got != `no node is named "gpu-z"` {
			t.Fatalf("error %q", got)
		}
		if got := callErr(t, apiEnv{f}, "call_api", `{"path":"/api/v1/logs/search","query":{"node":"gpu"}}`); got != `no node is named "gpu"` {
			t.Fatalf("a name matching only part of node names: %q", got)
		}
		assertIssued(t, f, "/api/v1/nodes?page=1&page_size=50&q=gpu-z", "/api/v1/nodes?page=1&page_size=50&q=gpu")
	})
}

func TestCallAPIMergesAQueryWrittenIntoThePath(t *testing.T) {
	f := newFakeAPI()
	mustCall(t, apiEnv{f}, "call_api", `{"path":"/api/v1/logs/search?q=NCCL&level=warn","query":{"level":"error","limit":50,"with_total":true}}`)
	assertIssued(t, f, "/api/v1/logs/search?level=error&limit=50&q=NCCL&with_total=true")
}

func TestCallAPIRefusesWhatItMayNotRead(t *testing.T) {
	f := newFakeAPI()
	env := apiEnv{f}
	for args, want := range map[string]string{
		`{"path":"/api/v2/nodes"}`:                  `path must start with /api/v1/, got "/api/v2/nodes"`,
		`{"path":""}`:                               `path must start with /api/v1/, got ""`,
		`{"path":"/api/v1/nodes/gpu-a/ssh"}`:        "/api/v1/nodes/{uuid}/ssh is not readable by the assistant (SSH access settings)",
		`{"path":"/api/v1/apikeys"}`:                "/api/v1/apikeys is not readable by the assistant (API keys)",
		`{"path":"/api/v1/nodes/gpu-a/whatever"}`:   "no GET endpoint /api/v1/nodes/gpu-a/whatever; find one with search_api",
		`{"path":"/api/v1/logs/search?level=%zz"}`:  `query in path: invalid URL escape "%zz"`,
		`{"path":"/api/v1/nodes","limit":"twenty"}`: "arguments are not valid JSON for this tool (json: cannot unmarshal string into Go struct field .limit of type int); resend the call with a JSON object",
		`{"path":"/api/v1/nodes"`:                   "arguments are not valid JSON for this tool (unexpected end of JSON input); resend the call with a JSON object",
	} {
		if got := callErr(t, env, "call_api", args); got != want {
			t.Fatalf("%s: got %q\nwant %q", args, got, want)
		}
	}
	assertIssued(t, f)
}

// Models write an endpoint many ways; each is read as the /api/v1 path it
// means rather than refused.
func TestCallAPIReadsThePathAModelMeant(t *testing.T) {
	for _, path := range []string{"api/v1/nodes", "/nodes", "nodes", "/api/v1/nodes/", "http://gpuworks.local:8800/api/v1/nodes"} {
		f := newFakeAPI()
		mustCall(t, apiEnv{f}, "call_api", `{"path":"`+path+`","jq":".total"}`)
		assertIssued(t, f, "/api/v1/nodes")
	}
}

// get_gpu_topology takes a node's name as well as its uuid.
func TestGPUTopologyAcceptsANodeName(t *testing.T) {
	f := newFakeAPI()
	env := &topologyEnv{apiEnv: apiEnv{f}}
	mustCall(t, env, "get_gpu_topology", `{"node_uuid":"gpu-b"}`)
	if env.asked != uuidB {
		t.Fatalf("topology asked for %q, want %q", env.asked, uuidB)
	}
	mustCall(t, env, "get_gpu_topology", `{"node_uuid":"`+uuidA+`"}`)
	if env.asked != uuidA {
		t.Fatalf("topology asked for %q, want %q", env.asked, uuidA)
	}
	if got := callErr(t, env, "get_gpu_topology", `{"node_uuid":"no-such-node"}`); got != `no node is named "no-such-node"` {
		t.Fatalf("unknown name = %q", got)
	}
}

type topologyEnv struct {
	apiEnv
	asked string
}

func (e *topologyEnv) GPUTopology(uuid string) (any, error) {
	e.asked = uuid
	return map[string]any{"fast_cliques": [][]int{{0, 1}}}, nil
}

func TestCallAPISurfacesHTTPErrorsWithTheirBody(t *testing.T) {
	f := newFakeAPI()
	got := callErr(t, apiEnv{f}, "call_api", `{"path":"/api/v1/nodes","query":{"state":"broken"}}`)
	if got != `HTTP 400: {"code":"LIST_FILTER_INVALID","error":"unknown state filter \"broken\"","params":{"name":"state","value":"broken"}}`+"\n" {
		t.Fatalf("error = %q", got)
	}
	got = callErr(t, apiEnv{f}, "call_api", `{"path":"/api/v1/deployments/99"}`)
	if got != `HTTP 404: {"code":"DEPLOYMENT_NOT_FOUND","error":"deployment not found"}`+"\n" {
		t.Fatalf("error = %q", got)
	}
}

func TestCallAPIExplainsAnEmptyAnswer(t *testing.T) {
	f := newFakeAPI()
	const hint = "\n(empty. If you expected data, check the parameter values against search_api's description: an unknown filter value answers empty, not an error.)"
	for args, want := range map[string]string{
		`{"path":"/api/v1/logs/search","query":{"q":"no such line"}}`:                   `{"items":[]}`,
		`{"path":"/api/v1/nodes","query":{"state":"connecting","page":1}}`:              `{"all":2,"items":[],"labels":[],"page":1,"page_size":50,"total":0}`,
		`{"path":"/api/v1/nodes","jq":"[.items[] | select(.state == \"connecting\")]"}`: `[]`,
		`{"path":"/api/v1/nodes","jq":".items[] | select(.state == \"connecting\")"}`:   `[]`,
	} {
		if got := mustCall(t, apiEnv{f}, "call_api", args); got != want+hint {
			t.Fatalf("%s: %q", args, got)
		}
	}
}

func TestCallAPIReturnsATextAnswerClipped(t *testing.T) {
	out := mustCall(t, apiEnv{newFakeAPI()}, "call_api", `{"path":"/api/v1/benchmarks/3/report"}`)
	if !strings.HasPrefix(out, "# Benchmark report\nrow\nrow\n") || !strings.HasSuffix(out, "…") || len(out) > maxAnswerChars {
		t.Fatalf("%d bytes, head %q tail %q", len(out), out[:30], out[len(out)-10:])
	}
	if truncateToolResult(out) != out {
		t.Fatal("the runner cut the text answer again")
	}
}

// A filter naming a field the answer lacks runs - jq reads it as null - and
// the answer says which field is not there (on gpu3 the model read a
// model's presence from its detail, which carries storages instead, and
// concluded the weights were nowhere).
func TestCallAPINamesFieldsAFilterReadAsNull(t *testing.T) {
	f := newFakeAPI()
	out := mustCall(t, apiEnv{f}, "call_api", `{"path":"/api/v1/nodes/`+uuidA+`","jq":"{name, presence, gpu_count}"}`)
	if !strings.Contains(out, `"presence":null`) || !strings.Contains(out, `(note: The answer has no field "presence".`) || !strings.Contains(out, "null above may only mean the field name is wrong") {
		t.Fatalf("answer = %s", out)
	}
	clean := mustCall(t, apiEnv{f}, "call_api", `{"path":"/api/v1/nodes/`+uuidA+`","jq":"{name, gpu_count}"}`)
	if strings.Contains(clean, "note:") {
		t.Fatalf("a filter naming only real fields got a note: %s", clean)
	}
}
