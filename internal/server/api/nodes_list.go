package api

import (
	"cmp"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strconv"
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
// descending), after filtering and before paging. source (comma separated
// alternatives) keeps the nodes that came from any of them: provider:<id> is
// an external platform account, on one of whose instances or through one of
// whose clusters the node joined; cluster:<uuid> is one cluster entry; manual
// is the nodes added by hand. A page lists every source there is in sources.
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
	fleet := nodes
	wanted, err := parseNodeSourceFilter(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Where every node came from is read once, for the filter and for the
	// choices a page offers; a list that needs neither does not read it.
	var sources map[uint]store.NodeSource
	if wanted != nil || query.paged {
		if sources, err = a.d.Store.FleetNodeSources(fleet); err != nil {
			// A list filtered by source cannot be answered without them; any
			// other still is, each node saying its source could not be read.
			if wanted != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			sources = nil
		}
	}
	if wanted != nil {
		// The label choices and the total stay the whole fleet's, as with every other filter.
		nodes = slices.DeleteFunc(slices.Clone(nodes), func(n store.Node) bool {
			source, joined := sources[n.ID]
			return !wanted.matches(source, joined)
		})
	}
	if !query.paged {
		// Every match in full; the filters and order apply either way.
		matched := a.filterNodes(nodes, query, reqs)
		nodeListSorters.apply(matched, order)
		writeJSON(w, http.StatusOK, dto.NewList(a.nodeDTOs(matched, sources)))
		return
	}
	facets := map[string]map[string]bool{}
	for i := range fleet {
		// A node whose labels cannot be read has none, as the DTO shows it.
		nodeLabels, _ := store.NodeLabels(fleet[i].Labels)
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
		// The summary is the whole fleet's, as the label choices and the total are.
		summary, statuses, err = a.nodeAgentSummary(r, fleet)
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
		items := a.nodeDTOs(rows, sources)
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
	writeJSON(w, http.StatusOK, dto.NodePage{AgentFleet: summary, ListPage: page, All: len(fleet), Labels: nodeLabelFacets(facets), Sources: nodeSourceOptions(fleet, sources)})
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

// nodeDTOs maps nodes as the node list shows them; sources is the fleet's,
// when the list read it already, else nil.
func (a *apiHandlers) nodeDTOs(nodes []store.Node, sources map[uint]store.NodeSource) []dto.Node {
	out := make([]dto.Node, 0, len(nodes))
	for i := range nodes {
		view := toNodeDTO(&nodes[i])
		if a.d.NodeCaps != nil {
			view.Caps = a.d.NodeCaps(nodes[i].UUID)
		}
		out = append(out, view)
	}
	a.fillResolvedNodeFields(nodes, out, sources)
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

// The values of the node list's source filter.
const (
	nodeSourceManual         = "manual"
	nodeSourceProviderPrefix = "provider:"
	nodeSourceClusterPrefix  = "cluster:"
)

// nodeSourceFilter is the sources a node list was asked for: a node from any
// of them matches.
type nodeSourceFilter struct {
	manual    bool
	providers map[uint]bool
	clusters  map[string]bool
}

// parseNodeSourceFilter reads the source parameter; nil when it is absent. A
// value of no known form is refused rather than matching nothing, while a
// well-formed one that names no account or cluster simply has no nodes.
func parseNodeSourceFilter(r *http.Request) (*nodeSourceFilter, error) {
	raw, ok := listParam(r.URL.Query(), "source")
	if !ok {
		return nil, nil
	}
	filter := &nodeSourceFilter{providers: map[uint]bool{}, clusters: map[string]bool{}}
	for _, value := range strings.Split(raw, ",") {
		value = strings.TrimSpace(value)
		switch {
		case value == nodeSourceManual:
			filter.manual = true
		case strings.HasPrefix(value, nodeSourceProviderPrefix):
			id, err := strconv.ParseUint(strings.TrimPrefix(value, nodeSourceProviderPrefix), 10, 64)
			if err != nil || id == 0 {
				return nil, listFilterError("source", value)
			}
			filter.providers[uint(id)] = true
		case strings.HasPrefix(value, nodeSourceClusterPrefix) && value != nodeSourceClusterPrefix:
			filter.clusters[strings.TrimPrefix(value, nodeSourceClusterPrefix)] = true
		default:
			return nil, listFilterError("source", value)
		}
	}
	return filter, nil
}

func (f *nodeSourceFilter) matches(source store.NodeSource, joined bool) bool {
	if !joined {
		return f.manual
	}
	return (source.ProviderID != 0 && f.providers[source.ProviderID]) || (source.ClusterUUID != "" && f.clusters[source.ClusterUUID])
}

// nodeSourceOptions lists every source a node of the fleet came from, with
// how many: accounts by name, then clusters by name, then the nodes added by
// hand. An account's count takes in its clusters' nodes, as its filter does.
//
// Sources that could not be read (nil) offer no choice rather than calling
// every node added by hand.
func nodeSourceOptions(fleet []store.Node, sources map[uint]store.NodeSource) []dto.NodeSourceOption {
	if sources == nil {
		return []dto.NodeSourceOption{}
	}
	providers := map[uint]*dto.NodeSourceOption{}
	clusters := map[string]*dto.NodeSourceOption{}
	manual := 0
	for i := range fleet {
		source, joined := sources[fleet[i].ID]
		if !joined {
			manual++
			continue
		}
		if source.ProviderID != 0 {
			option := providers[source.ProviderID]
			if option == nil {
				option = &dto.NodeSourceOption{Value: nodeSourceProviderPrefix + strconv.FormatUint(uint64(source.ProviderID), 10),
					ProviderName: source.ProviderName, ProviderKind: source.ProviderKind}
				providers[source.ProviderID] = option
			}
			option.Nodes++
		}
		if source.ClusterUUID != "" {
			option := clusters[source.ClusterUUID]
			if option == nil {
				option = &dto.NodeSourceOption{Value: nodeSourceClusterPrefix + source.ClusterUUID,
					ProviderName: source.ProviderName, ProviderKind: source.ProviderKind, ClusterName: source.ClusterName}
				clusters[source.ClusterUUID] = option
			}
			option.Nodes++
		}
	}
	out := make([]dto.NodeSourceOption, 0, len(providers)+len(clusters)+1)
	for _, option := range providers {
		out = append(out, *option)
	}
	sort.Slice(out, func(i, j int) bool {
		return compareText(out[i].ProviderName, out[j].ProviderName) < 0 || (out[i].ProviderName == out[j].ProviderName && out[i].Value < out[j].Value)
	})
	byName := make([]dto.NodeSourceOption, 0, len(clusters))
	for _, option := range clusters {
		byName = append(byName, *option)
	}
	sort.Slice(byName, func(i, j int) bool {
		return compareText(byName[i].ClusterName, byName[j].ClusterName) < 0 || (byName[i].ClusterName == byName[j].ClusterName && byName[i].Value < byName[j].Value)
	})
	out = append(out, byName...)
	if manual > 0 {
		out = append(out, dto.NodeSourceOption{Value: nodeSourceManual, Nodes: manual})
	}
	return out
}
