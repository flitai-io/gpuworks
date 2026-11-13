package aihelper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The platform API tools: the assistant reads what the UI reads, through the
// asking user's APIReader. search_api finds endpoints in the API reference,
// call_api reads one (with jq and limits to keep answers small),
// search_metrics finds stored series, and cluster_overview answers "how is
// the cluster" in one call.

var errNoAPI = errors.New("the platform API is not available to this request")

const (
	searchAPIResults     = 12
	searchMetricsResults = 30
)

func searchAPITool() Tool {
	return Tool{
		Name: "search_api",
		Description: "Search the platform's read-only HTTP API (GET endpoints) by keywords - English path words (nodes, gpus, models, deployments, instances, logs, metrics, images, storages, networks, templates, benchmarks, monitor) or words from the descriptions. " +
			"Returns matching endpoints with what they answer and their query parameters. Read one with call_api.",
		Schema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"keywords, e.g. \"node disk usage\" or \"deployment events\""}},"required":["query"]}`),
		Call: func(ctx context.Context, env ToolEnv, args json.RawMessage) (string, error) {
			var a struct {
				Query string `json:"query"`
			}
			if err := strictArgs(args, &a); err != nil {
				return "", err
			}
			api := env.API()
			if api == nil {
				return "", errNoAPI
			}
			return searchAPI(api.Routes(), a.Query), nil
		},
	}
}

func searchAPI(routes []string, query string) string {
	terms := searchTerms(query)
	type hit struct {
		route string
		score int
	}
	var hits []hit
	for _, route := range routes {
		doc := RouteDoc(route)
		if !Readable(route) || doc == "" {
			continue
		}
		path, text := strings.ToLower(route), strings.ToLower(doc)
		score := 0
		for _, t := range terms {
			if strings.Contains(path, t) {
				score += 3
			}
			if strings.Contains(text, t) {
				score++
			}
		}
		if score > 0 || len(terms) == 0 {
			hits = append(hits, hit{route, score})
		}
	}
	if len(hits) == 0 {
		return "no endpoint matches; try other words (nodes, gpus, models, deployments, instances, logs, metrics, images, storages, networks)"
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].route < hits[j].route
	})
	var b strings.Builder
	for i, h := range hits {
		if i == searchAPIResults {
			fmt.Fprintf(&b, "(%d more match; use more specific words)\n", len(hits)-searchAPIResults)
			break
		}
		fmt.Fprintf(&b, "GET %s — %s\n", h.route, clip(RouteDoc(h.route), 600))
	}
	return strings.TrimSpace(b.String())
}

var termSplit = regexp.MustCompile(`[\s,，、/]+`)

func searchTerms(query string) []string {
	var out []string
	for _, t := range termSplit.Split(strings.ToLower(query), -1) {
		if t = strings.Trim(t, "\"'`?？"); len(t) > 1 {
			out = append(out, t)
		}
	}
	return out
}

func callAPITool() Tool {
	return Tool{
		Name: "call_api",
		Description: "Read one GET endpoint of the platform API (find it with search_api) and get its JSON. " +
			"Collections answer {\"items\": [...], \"total\": N}. A single object has no items: filter `.name`, not `.items[].name`. " +
			"Metric points are [unix ms, value] arrays: `.items[].points[-1][1]`. " +
			"Arrays are cut to `limit` items (default 20, max 200) and strings to 300 characters, each cut marked; " +
			"answers over 12000 characters are truncated. Use `jq` to keep only what you need - e.g. '.items[] | {name, state}', " +
			"'[.items[] | select(.state != \"running\")] | length', '.items | group_by(.state) | map({state: .[0].state, n: length})'. " +
			"Path parameters go in the path (a node's name works where its uuid is expected); query parameters in `query`. Times are shown as UTC with their age.",
		Schema: json.RawMessage(`{"type":"object","properties":{` +
			`"path":{"type":"string","description":"e.g. /api/v1/nodes or /api/v1/deployments/12"},` +
			`"query":{"type":"object","description":"query parameters, e.g. {\"level\":\"error\",\"limit\":50}"},` +
			`"jq":{"type":"string","description":"jq filter applied before the answer is cut"},` +
			`"limit":{"type":"integer","description":"items kept per array, default 20"}},"required":["path"]}`),
		Call: callAPI,
	}
}

func callAPI(ctx context.Context, env ToolEnv, args json.RawMessage) (string, error) {
	var a struct {
		Path  string         `json:"path"`
		Query map[string]any `json:"query"`
		JQ    string         `json:"jq"`
		Limit int            `json:"limit"`
	}
	if err := strictArgs(args, &a); err != nil {
		return "", err
	}
	api := env.API()
	if api == nil {
		return "", errNoAPI
	}
	path, query, err := splitPathQuery(a.Path, a.Query)
	if err != nil {
		return "", err
	}
	pattern := api.Route(path)
	if pattern == "" {
		return "", fmt.Errorf("no GET endpoint %s; find one with search_api", path)
	}
	if !Readable(pattern) {
		return "", fmt.Errorf("%s is not readable by the assistant (%s)", pattern, deniedRoutes[pattern])
	}
	if path, query, err = resolveNodeNames(ctx, api, pattern, path, query); err != nil {
		return "", err
	}
	status, body, err := api.Get(ctx, path, query)
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", fmt.Errorf("HTTP %d: %s", status, clip(string(body), 1500))
	}
	var data any
	if err := json.Unmarshal(body, &data); err != nil {
		return clipToFit(string(body), maxAnswerChars), nil //nolint:nilerr // Non-JSON API responses are successful text answers, including reports.
	}
	// Secrets go before jq can reach them; times are aged after it, so a
	// filter still compares time_ms as the number it is.
	data = redact(data)
	limit := a.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	limit = min(limit, maxListLimit)
	// A filter naming a field the answer lacks still runs - jq reads it as
	// null - and a null reads to a model like missing data; say which.
	// A filter that does not run at all still returns this body: the GET
	// succeeded, and a failed step only sends the model back for another try.
	note := ""
	if a.JQ != "" {
		out, err := runJQ(ctx, a.JQ, data)
		if err != nil {
			note = fmt.Sprintf("\n(filter did not apply: %v.%s The answer's structure: %s)", err, fieldHints(a.JQ, data), outlineText(data))
		} else {
			if hints := fieldHints(a.JQ, data); hints != "" {
				note = "\n(note:" + hints + " jq reads a missing field as null, so a null above may only mean the field name is wrong.)"
			}
			data = out
		}
	}
	data = humanize(data)
	if isEmptyAnswer(data) {
		return compact(data, "") + "\n(empty. If you expected data, check the parameter values against search_api's description: an unknown filter value answers empty, not an error.)" + note, nil
	}
	hint := "Select fields with jq or lower limit."
	if a.JQ == "" {
		hint += " The answer's structure: " + outlineText(data)
	}
	return clipToFit(compact(shape(data, limit), hint), maxAnswerChars-len(note)) + note, nil
}

// apiPath reads the ways a model writes an endpoint as its path: a full URL,
// "api/v1/nodes", "/nodes" or "nodes", with or without a trailing slash, are
// all "/api/v1/nodes".
func apiPath(p string) string {
	if u, err := url.Parse(p); err == nil && u.Scheme != "" && u.Host != "" {
		p = u.Path
	}
	p = strings.TrimRight(p, "/")
	switch {
	case p == "":
		return p
	case strings.HasPrefix(p, "/api/v1/"):
	case strings.HasPrefix(p, "api/v1/"):
		p = "/" + p
	case strings.HasPrefix(p, "/api/"):
		// another API version: leave it to be refused as is
	case strings.HasPrefix(p, "/"):
		p = "/api/v1" + p
	default:
		p = "/api/v1/" + p
	}
	return p
}

// splitPathQuery accepts a query written into the path as well as in query.
func splitPathQuery(raw string, params map[string]any) (string, url.Values, error) {
	path, rawQuery, _ := strings.Cut(strings.TrimSpace(raw), "?")
	path = apiPath(path)
	if !strings.HasPrefix(path, "/api/v1/") {
		return "", nil, fmt.Errorf("path must start with /api/v1/, got %q", raw)
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", nil, fmt.Errorf("query in path: %v", err)
	}
	for k, v := range params {
		switch t := v.(type) {
		case []any:
			for _, x := range t {
				query.Add(k, queryValue(x))
			}
		default:
			query.Set(k, queryValue(t))
		}
	}
	return path, query, nil
}

func queryValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

var nodeUUID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// resolveNodeNames lets a node's name stand for its uuid: in the path of a
// /nodes/{uuid} route and in node= query parameters.
func resolveNodeNames(ctx context.Context, api APIReader, pattern, path string, query url.Values) (string, url.Values, error) {
	if strings.HasPrefix(pattern, "/api/v1/nodes/{uuid}") {
		parts := strings.Split(path, "/")
		if len(parts) > 4 && !nodeUUID.MatchString(parts[4]) {
			uuid, err := nodeUUIDByName(ctx, api, parts[4])
			if err != nil {
				return "", nil, err
			}
			parts[4] = uuid
			path = strings.Join(parts, "/")
		}
	}
	for _, key := range []string{"node", "node_uuid"} {
		if v := query.Get(key); v != "" && v != "server" && !nodeUUID.MatchString(v) {
			uuid, err := nodeUUIDByName(ctx, api, v)
			if err != nil {
				return "", nil, err
			}
			query.Set(key, uuid)
		}
	}
	return path, query, nil
}

func nodeUUIDByName(ctx context.Context, api APIReader, name string) (string, error) {
	status, body, err := api.Get(ctx, "/api/v1/nodes", url.Values{"q": {name}, "page": {"1"}, "page_size": {"50"}})
	if err != nil {
		return "", err
	}
	if status != 200 {
		return "", fmt.Errorf("looking up node %q: HTTP %d", name, status)
	}
	var page struct {
		Items []struct {
			UUID     string `json:"uuid"`
			Name     string `json:"name"`
			Hostname string `json:"hostname"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return "", err
	}
	for _, n := range page.Items {
		if n.Name == name || n.Hostname == name {
			return n.UUID, nil
		}
	}
	return "", fmt.Errorf("no node is named %q", name)
}

func searchMetricsTool() Tool {
	return Tool{
		Name: "search_metrics",
		Description: "Search the catalog of stored time series (GPU, dGPU, node, network, inference engine, gateway, KV cache disk, benchmark) by keywords. " +
			"Returns each series' name, what it measures, unit and labels. Read one with call_api GET /api/v1/metrics/query " +
			"{metric, node (name or uuid), range like \"1h\", step like \"5m\"}; it answers one series per label set, points as [unix ms, value].",
		Schema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"keywords, e.g. \"gpu memory\", \"ttft\", \"rdma\""}},"required":["query"]}`),
		Call: func(ctx context.Context, env ToolEnv, args json.RawMessage) (string, error) {
			var a struct {
				Query string `json:"query"`
			}
			if err := strictArgs(args, &a); err != nil {
				return "", err
			}
			api := env.API()
			if api == nil {
				return "", errNoAPI
			}
			status, body, err := api.Get(ctx, "/api/v1/monitor/metrics", nil)
			if err != nil {
				return "", err
			}
			if status != 200 {
				return "", fmt.Errorf("metric catalog: HTTP %d", status)
			}
			var catalog struct {
				Items []struct {
					Series      string   `json:"series"`
					Source      string   `json:"source"`
					Unit        string   `json:"unit"`
					Kind        string   `json:"kind"`
					Quantiles   bool     `json:"quantiles"`
					Labels      []string `json:"labels"`
					Description string   `json:"description"`
				} `json:"items"`
			}
			if err := json.Unmarshal(body, &catalog); err != nil {
				return "", err
			}
			// Series matching more of the words come first. The common
			// "gpuworks_" prefix is not matched: "gpu" would otherwise hit
			// every platform series and bury the GPU ones past the cap.
			terms := searchTerms(a.Query)
			type hit struct{ i, score int }
			var hits []hit
			for i, m := range catalog.Items {
				text := strings.ToLower(strings.TrimPrefix(m.Series, "gpuworks_") + " " + m.Source + " " + m.Description)
				score := 0
				for _, t := range terms {
					if strings.Contains(text, t) {
						score++
					}
				}
				if score > 0 || len(terms) == 0 {
					hits = append(hits, hit{i, score})
				}
			}
			if len(hits) == 0 {
				return "no stored series matches; try other words (gpu, memory, utilization, node, network, rdma, ttft, tokens, kv)", nil
			}
			sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
			var b strings.Builder
			for n, h := range hits {
				if n == searchMetricsResults {
					fmt.Fprintf(&b, "(%d more match; use more specific words)\n", len(hits)-searchMetricsResults)
					break
				}
				m := catalog.Items[h.i]
				fmt.Fprintf(&b, "%s — %s [%s, %s, %s", m.Series, m.Description, m.Source, m.Kind, m.Unit)
				if m.Quantiles {
					b.WriteString(", quantile label p50/p95/p99")
				}
				if len(m.Labels) > 0 {
					b.WriteString("; labels: " + strings.Join(m.Labels, ", "))
				}
				b.WriteString("]\n")
			}
			return strings.TrimSpace(b.String()), nil
		},
	}
}

func clusterOverviewTool() Tool {
	return Tool{
		Name: "cluster_overview",
		Description: "One call for the state of the whole cluster: node counts by state with capacity, GPU health/allocation/work counts, deployment counts, " +
			"the nodes and deployments that need attention (offline nodes and ones still connecting, failed or pending deployments) and the newest deployments. Start here for any cluster-wide question.",
		Schema: json.RawMessage(`{"type":"object","properties":{}}`),
		Call: func(ctx context.Context, env ToolEnv, args json.RawMessage) (string, error) {
			api := env.API()
			if api == nil {
				return "", errNoAPI
			}
			return clusterOverview(ctx, api)
		},
	}
}

// clusterOverview reads a bounded handful of pages - the dashboard summary,
// the GPU overview, at most 30 attention nodes and deployments each and the
// 20 newest deployments - whatever the fleet's size.
func clusterOverview(ctx context.Context, api APIReader) (string, error) {
	type read struct {
		key   string
		path  string
		query url.Values
		jq    string
	}
	reads := []read{
		{"summary", "/api/v1/dashboard/summary", nil, `{nodes, deployments, by_vendor}`},
		{"gpus", "/api/v1/gpus", url.Values{"page": {"1"}, "page_size": {"1"}}, `.overview`},
		{"nodes_needing_attention", "/api/v1/nodes", url.Values{"page": {"1"}, "page_size": {"30"}, "state": {"offline,connecting"}},
			`{total, items: [.items[] | {name, state, scheduling, gpus: ([.gpus[]?.name] | group_by(.) | map("\(.[0]) x\(length)")), last_seen_at}]}`},
		{"deployments_needing_attention", "/api/v1/deployments", url.Values{"page": {"1"}, "page_size": {"30"}, "state": {"failed,pending"}},
			`{total, items: [.items[] | {id, name, model_name, state, runtime_state, replicas, ready_replicas, node_names}]}`},
		{"newest_deployments", "/api/v1/deployments", url.Values{"page": {"1"}, "page_size": {"20"}},
			`{total, items: [.items[] | {id, name, model_name, state, runtime_state, replicas, ready_replicas, node_names}]}`},
	}
	out := map[string]any{}
	for _, r := range reads {
		status, body, err := api.Get(ctx, r.path, r.query)
		if err != nil || status != 200 {
			out[r.key] = fmt.Sprintf("unavailable (HTTP %d %v)", status, err)
			continue
		}
		var data any
		if err := json.Unmarshal(body, &data); err != nil {
			out[r.key] = fmt.Sprintf("unavailable (unreadable answer: %v)", err)
			continue
		}
		v, err := runJQ(ctx, r.jq, redact(data))
		if err != nil {
			out[r.key] = fmt.Sprintf("unavailable (%v)", err)
			continue
		}
		out[r.key] = humanize(v)
	}
	out["now"] = now().UTC().Format(time.RFC3339)
	return compact(out, "Read the parts you need with call_api."), nil
}

// strictArgs decodes a tool's JSON arguments, telling the model how to fix
// arguments that are not a JSON object of the right shape.
func strictArgs(args json.RawMessage, into any) error {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(args, into); err != nil {
		return fmt.Errorf("arguments are not valid JSON for this tool (%v); resend the call with a JSON object", err)
	}
	return nil
}
