package api

import (
	"cmp"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

var nodeListStates = []string{store.NodeStateConnecting, store.NodeStateOnline, store.NodeStateOffline}

// nodeListSorters are the orders GET /nodes offers, by the node list's column keys.
var nodeListSorters = listSorters[store.Node]{
	"name":          func(a, b store.Node) int { return compareText(a.DisplayName(), b.DisplayName()) },
	"state":         func(a, b store.Node) int { return cmp.Compare(a.ConnectionState(), b.ConnectionState()) },
	"agent_version": func(a, b store.Node) int { return cmp.Compare(a.AgentVersion, b.AgentVersion) },
	"gpu_count":     func(a, b store.Node) int { return cmp.Compare(a.GPUCount, b.GPUCount) },
	"cpu_cores":     func(a, b store.Node) int { return cmp.Compare(a.CPUCores, b.CPUCores) },
	"mem_total":     func(a, b store.Node) int { return cmp.Compare(a.MemTotalKB, b.MemTotalKB) },
	"created_at":    func(a, b store.Node) int { return a.CreatedAt.Compare(b.CreatedAt) },
}

// listNodes answers GET /nodes. With page or page_size it returns one
// dto.NodePage filtered by q (a substring of the name, hostname or uuid),
// state (comma separated) and label (key=value, repeatable). The label
// filter is a set of node requirements, matched the way placement matches
// them (planning.MatchRequirements): values of one key are alternatives,
// different keys must all hold, and a gpuworks.ai/ key is matched against the
// node's system labels. A page row leaves out the topology and path usage,
// which GET /nodes/{uuid} carries. Without page parameters it returns every
// node the filters match, in full. A page is ordered by sort (one of nodeListSorters, "-" for
// descending), after filtering and before paging.
func (a *apiHandlers) listNodes(w http.ResponseWriter, r *http.Request) {
	query, err := parseListQuery(r, nodeListStates)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	order, err := nodeListSorters.parse(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	reqs, err := parseLabelFilter(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	nodes, err := a.d.Store.ListNodes()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	mode := r.URL.Query().Get("agent_upgrade")
	if mode != "" && (!query.paged || !slices.Contains([]string{"all", "align", "upgrade", "downgrade", "different_build", "check"}, mode)) {
		writeErr(w, http.StatusBadRequest, listFilterError("agent_upgrade", mode))
		return
	}
	if !query.paged {
		// Every match in full; the filters and order apply either way.
		matched := a.filterNodes(nodes, query, reqs)
		nodeListSorters.apply(matched, order)
		writeJSON(w, http.StatusOK, dto.NewList(a.nodeDTOs(matched)))
		return
	}
	facets := map[string]map[string]bool{}
	for i := range nodes {
		// A node whose labels cannot be read has none, as the DTO shows it.
		nodeLabels, _ := store.NodeLabels(nodes[i].Labels)
		for key, value := range nodeLabels {
			if facets[key] == nil {
				facets[key] = map[string]bool{}
			}
			facets[key][value] = true
		}
	}
	matched := a.filterNodes(nodes, query, reqs)
	var summary *dto.AgentFleetSummary
	var statuses map[string]nodeAgentStatus
	if mode != "" {
		summary, statuses, err = a.nodeAgentSummary(r, nodes)
		if mode != "all" {
			if err != nil {
				writeErr(w, http.StatusServiceUnavailable, err)
				return
			}
			matched = slices.DeleteFunc(matched, func(n store.Node) bool {
				status := statuses[n.UUID]
				if mode == "align" {
					return status.eligibility != "eligible"
				}
				return status.group != mode
			})
		}
	}
	nodeListSorters.apply(matched, order)
	page := pageOf(matched, query, func(rows []store.Node) []dto.Node {
		items := a.nodeDTOs(rows)
		for i := range items {
			status := statuses[items[i].UUID]
			items[i].AgentEligibility = status.eligibility
			if status.eligibility == "eligible" {
				items[i].AgentVersionChange = status.group
			}
			items[i].Topology = nil
			items[i].PathUsage = nil
		}
		return items
	})
	writeJSON(w, http.StatusOK, dto.NodePage{AgentFleet: summary, ListPage: page, All: len(nodes), Labels: nodeLabelFacets(facets)})
}

// filterNodes keeps the nodes a list query and label requirements match, in
// the order given. Networks are resolved only when a requirement reads them.
func (a *apiHandlers) filterNodes(nodes []store.Node, query listQuery, reqs map[string][]string) []store.Node {
	// An unresolvable set leaves the network and storage keys unmet, as
	// placement treats it: an unreadable fact is never a match.
	facts := a.resolveForRequirements(nodePointers(nodes), reqs)
	var matched []store.Node
	for i := range nodes {
		n := &nodes[i]
		if !query.matchesState(n.ConnectionState()) || !query.matchesSearch(n.Name, n.Hostname, n.UUID) {
			continue
		}
		if planning.MatchRequirements(n, facts, reqs) != nil {
			continue
		}
		matched = append(matched, *n)
	}
	return matched
}

// nodeDTOs maps nodes as the node list shows them.
func (a *apiHandlers) nodeDTOs(nodes []store.Node) []dto.Node {
	out := make([]dto.Node, 0, len(nodes))
	for i := range nodes {
		view := toNodeDTO(&nodes[i])
		if a.d.NodeCaps != nil {
			view.Caps = a.d.NodeCaps(nodes[i].UUID)
		}
		out = append(out, view)
	}
	a.fillResolvedNodeFields(nodes, out)
	return out
}

// parseLabelFilter reads the repeatable label=key=value parameter as node
// requirements, normalized and validated as a deployment's are: a malformed
// pair or an impossible system value is refused rather than matching nothing.
func parseLabelFilter(r *http.Request) (map[string][]string, error) {
	reqs := map[string][]string{}
	for _, raw := range r.URL.Query()["label"] {
		key, value, ok := strings.Cut(raw, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, listFilterError("label", raw)
		}
		reqs[key] = append(reqs[key], value)
	}
	normalized, err := planning.RequestRequirements(planning.RequirementsRequest{Stated: reqs})
	if err != nil {
		return nil, productError(ErrorListFilterInvalid, err.Error(), map[string]any{"name": "label", "value": err.Error()})
	}
	return normalized, nil
}

// writtenLabels is a node's written labels as the API shows them: a node
// whose labels cannot be read has none.
func writtenLabels(n *store.Node) map[string]string {
	labels, _ := store.NodeLabels(n.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	return labels
}

func nodeLabelFacets(facets map[string]map[string]bool) []dto.NodeLabelValues {
	out := make([]dto.NodeLabelValues, 0, len(facets))
	for key, values := range facets {
		item := dto.NodeLabelValues{Key: key, Values: make([]string, 0, len(values))}
		for value := range values {
			item.Values = append(item.Values, value)
		}
		sort.Strings(item.Values)
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

type nodeAgentStatus struct {
	eligibility string
	group       string
}

func agentFleetGroup(eligibility, current, target string) string {
	switch eligibility {
	case "eligible":
		return distribution.VersionChange(current, target)
	case "offline":
		return "offline"
	case "", "busy", "already_current":
		return ""
	default:
		return "check"
	}
}

func (a *apiHandlers) nodeAgentSummary(r *http.Request, nodes []store.Node) (*dto.AgentFleetSummary, map[string]nodeAgentStatus, error) {
	svc := a.d.AgentUpgrades.Service
	if svc == nil {
		return nil, nil, errors.New("agent status unavailable")
	}
	fleet, err := svc.Fleet(r.Context(), nodes)
	if err != nil {
		return nil, nil, err
	}
	summary := &dto.AgentFleetSummary{Target: fleet.Target, RunID: fleet.RunID}
	statuses := make(map[string]nodeAgentStatus, len(nodes))
	for _, n := range nodes {
		state := fleet.Eligibility[n.UUID]
		group := agentFleetGroup(state, n.AgentVersion, fleet.Target)
		statuses[n.UUID] = nodeAgentStatus{eligibility: state, group: group}
		switch group {
		case "upgrade":
			summary.Upgrade++
		case "downgrade":
			summary.Downgrade++
		case "different_build":
			summary.DifferentBuild++
		case "check":
			summary.Check++
		case "offline":
			summary.Offline++
		}
	}
	summary.Align = summary.Upgrade + summary.Downgrade + summary.DifferentBuild
	return summary, statuses, nil
}

func (a *apiHandlers) agentFleetSummary(w http.ResponseWriter, r *http.Request) {
	nodes, err := a.d.Store.NodesForAgentUpgrade()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	summary, _, err := a.nodeAgentSummary(r, nodes)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}
