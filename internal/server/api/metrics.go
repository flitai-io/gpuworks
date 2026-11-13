package api

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// metricsQuery serves range queries for the dashboards.
// GET /api/v1/metrics/query?metric=gpuworks_gpu_utilization&range=24h&step=60s&node=..&gpu=..
// Optional start/end (unix ms) select an arbitrary window and take precedence
// over range; a missing end defaults to now, a missing start to end-range.
func (a *apiHandlers) metricsQuery(w http.ResponseWriter, r *http.Request) {
	if a.d.MetricsQuery == nil {
		writeJSON(w, http.StatusOK, dto.NewList[dto.MonitorSeries](nil))
		return
	}
	q := r.URL.Query()
	metric := q.Get("metric")
	if metric == "" {
		writeProductError(w, http.StatusBadRequest, ErrorMetricRequired, "metric required", nil)
		return
	}
	rng := parseDur(q.Get("range"), 6*time.Hour)
	step := parseDur(q.Get("step"), time.Minute)
	end := time.Now().UTC()
	if v, err := strconv.ParseInt(q.Get("end"), 10, 64); err == nil && v > 0 {
		end = time.UnixMilli(v).UTC()
	}
	// A missing start looks back one `range` from the resolved end, so an
	// end-only request is a historical window, not a now-anchored one.
	start := end.Add(-rng)
	if v, err := strconv.ParseInt(q.Get("start"), 10, 64); err == nil && v > 0 {
		start = time.UnixMilli(v).UTC()
	}
	if q.Get("start") != "" || q.Get("end") != "" {
		if !start.Before(end) {
			writeProductError(w, http.StatusBadRequest, ErrorQueryWindowInvalid, "start must be before end", nil)
			return
		}
	}

	matchers := map[string]string{}
	for _, key := range []string{"node", "gpu", "gpu_uuid", "instance", "benchmark", "point", "iface", "hca", "role"} {
		if v := q.Get(key); v != "" {
			matchers[key] = v
		}
	}
	res, err := a.d.MetricsQuery(metric, matchers, start.UnixMilli(), end.UnixMilli(), step.Milliseconds())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// No data under a name the catalog does not define is a misspelt metric,
	// not an empty window: say so, with the names it is closest to. Data
	// under any name is answered, catalogued or not.
	if len(res) == 0 && a.d.Monitor != nil {
		if similar, known := similarMetrics(a.d.Monitor.Metrics(), metric); !known {
			writeProductError(w, http.StatusBadRequest, ErrorMetricUnknown, "unknown metric "+metric+"; similar: "+strings.Join(similar, ", "),
				map[string]any{"metric": metric, "similar": strings.Join(similar, ", ")})
			return
		}
	}
	a.nameSeries(res)
	writeJSON(w, http.StatusOK, dto.NewList(res))
}

// nameSeries adds display-only labels to the series of an answer: node_name
// to every series that carries a node, deployment_name to every one that
// carries a deployment, and node_deleted / deployment_deleted = "true" when
// that name is the one a deleted node or deployment left. A chart of the
// fleet's cards cannot be read by node UUID, a client cannot look up a
// thousand names one by one, and samples outlive what they measured. One
// read of the nodes' identities (no GPU rows) and one of the answer's
// deployments serve the whole answer. The added labels are never matchers;
// the series are still selected by node and deployment.
func (a *apiHandlers) nameSeries(series []dto.MonitorSeries) {
	if a.d.Store == nil {
		return
	}
	var nodeUUIDs []string
	var depIDs []uint
	seenNode, seenDep := map[string]bool{}, map[uint]bool{}
	for _, s := range series {
		if node := s.Labels["node"]; node != "" && !seenNode[node] {
			seenNode[node] = true
			nodeUUIDs = append(nodeUUIDs, node)
		}
		if id := parseUint(s.Labels["deployment"]); id != 0 && !seenDep[id] {
			seenDep[id] = true
			depIDs = append(depIDs, id)
		}
	}
	nodes := a.seriesNodeNames(nodeUUIDs)
	deps := map[uint]store.DeploymentName{}
	if len(depIDs) > 0 {
		if named, err := a.d.Store.DeploymentNames(depIDs); err == nil {
			deps = named
		}
	}
	if len(nodes) == 0 && len(deps) == 0 {
		return
	}
	for i := range series {
		node, nodeOK := nodes[series[i].Labels["node"]]
		dep, depOK := deps[parseUint(series[i].Labels["deployment"])]
		nodeOK = nodeOK && node.Name != ""
		depOK = depOK && dep.Name != ""
		if !nodeOK && !depOK {
			continue
		}
		labels := make(map[string]string, len(series[i].Labels)+4)
		for key, value := range series[i].Labels {
			labels[key] = value
		}
		if nodeOK {
			labels["node_name"] = node.Name
			if node.Deleted {
				labels["node_deleted"] = "true"
			}
		}
		if depOK {
			labels["deployment_name"] = dep.Name
			if dep.Deleted {
				labels["deployment_deleted"] = "true"
			}
		}
		series[i].Labels = labels
	}
}

// seriesNodeNames names the nodes of uuids: every live node's display name
// from one read of their identities, then the names deleted ones left.
func (a *apiHandlers) seriesNodeNames(uuids []string) map[string]store.NodeName {
	out := make(map[string]store.NodeName, len(uuids))
	if len(uuids) == 0 {
		return out
	}
	nodes, err := a.d.Store.NodeIdentities()
	if err != nil {
		return out
	}
	live := make(map[string]string, len(nodes))
	for i := range nodes {
		live[nodes[i].UUID] = nodes[i].DisplayName()
	}
	var missing []string
	for _, uuid := range uuids {
		if name, ok := live[uuid]; ok {
			out[uuid] = store.NodeName{Name: name}
		} else {
			missing = append(missing, uuid)
		}
	}
	if gone, err := a.d.Store.DeletedNodes(missing); err == nil {
		for uuid, n := range gone {
			out[uuid] = store.NodeName{Name: n.Name, Deleted: true}
		}
	}
	return out
}

func parseDur(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return def
}

// similarMetrics reports whether metric is a catalogued series and, when it
// is not, up to five catalogued names closest to it: most "_"-separated
// words in common, then fewest words besides, then longest shared prefix.
func similarMetrics(catalog []dto.MonitorMetricSeries, metric string) (similar []string, known bool) {
	words := map[string]bool{}
	for _, w := range strings.Split(strings.TrimPrefix(metric, "gpuworks_"), "_") {
		if w != "" {
			words[w] = true
		}
	}
	type scored struct {
		name                  string
		common, extra, shared int
	}
	var ranked []scored
	for _, c := range catalog {
		if c.Series == metric {
			return nil, true
		}
		common, extra := 0, 0
		for _, w := range strings.Split(strings.TrimPrefix(c.Series, "gpuworks_"), "_") {
			if words[w] {
				common++
			} else {
				extra++
			}
		}
		if common == 0 {
			continue
		}
		shared := 0
		for shared < len(c.Series) && shared < len(metric) && c.Series[shared] == metric[shared] {
			shared++
		}
		ranked = append(ranked, scored{c.Series, common, extra, shared})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].common != ranked[j].common {
			return ranked[i].common > ranked[j].common
		}
		if ranked[i].extra != ranked[j].extra {
			return ranked[i].extra < ranked[j].extra
		}
		if ranked[i].shared != ranked[j].shared {
			return ranked[i].shared > ranked[j].shared
		}
		return ranked[i].name < ranked[j].name
	})
	for i := 0; i < len(ranked) && i < 5; i++ {
		similar = append(similar, ranked[i].name)
	}
	return similar, false
}
