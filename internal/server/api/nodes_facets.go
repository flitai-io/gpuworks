package api

import (
	"net/http"
	"sort"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// nodeFacets answers GET /nodes/facets: for the nodes q, state and label
// match (as on the paged node list), how many there are and which written
// labels, system labels and accelerator models they carry, with counts. The
// deploy form and the placement editor offer these choices without fetching
// every node.
func (a *apiHandlers) nodeFacets(w http.ResponseWriter, r *http.Request) {
	query, err := parseListQuery(r, nodeListStates)
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
	matched := a.filterNodes(nodes, query, reqs)
	writeJSON(w, http.StatusOK, a.facetsOf(matched))
}

func (a *apiHandlers) facetsOf(nodes []store.Node) dto.NodeFacets {
	// Network system labels need the resolution; it is per node, so the
	// matched set alone is enough.
	resolved, _ := a.networks().Resolve(nodes)
	// Storage labels are what offers "storage" as a node condition.
	storages, _ := planning.LoadStorageScope(a.d.Store, nodePointers(nodes))
	ids := make([]uint, len(nodes))
	for i := range nodes {
		ids[i] = nodes[i].ID
	}
	// Provider labels are what offer "rented on" as a node condition.
	providers, _ := a.d.Store.NodeProviders(ids)
	facts := planning.NodeFacts{Networks: resolved, Storages: storages, Providers: providers, KubeClusters: planning.LoadKubeClusters(a.d.Store, nodes)}
	written := map[string]map[string]int{}
	system := map[string]map[string]*dto.SystemLabelValueCount{}
	type chip struct{ vendor, name, cc, archID string }
	gpus := map[chip]int{}
	// cards and memory sum the chip's cards across the matched nodes and keep
	// the largest card's memory, for a reader sizing a model against the fleet.
	cards := map[chip]int{}
	memory := map[chip]int64{}
	for i := range nodes {
		n := &nodes[i]
		// A node whose labels cannot be read has none, as the DTO shows it.
		labels, _ := store.NodeLabels(n.Labels)
		for key, value := range labels {
			if written[key] == nil {
				written[key] = map[string]int{}
			}
			written[key][value]++
		}
		for _, label := range planning.SystemLabels(n, facts) {
			if planning.IsNodeIdentity(label) {
				continue
			}
			if system[label.Key] == nil {
				system[label.Key] = map[string]*dto.SystemLabelValueCount{}
			}
			entry := system[label.Key][label.Value]
			if entry == nil {
				entry = &dto.SystemLabelValueCount{Value: label.Value, Display: label.Display, DisplayCode: label.DisplayCode}
				system[label.Key][label.Value] = entry
			}
			// A mixed node carries several values of one key, each once.
			entry.Nodes++
		}
		seen := map[chip]bool{}
		for _, g := range n.OwnedGPUs() {
			c := chip{g.Vendor, g.Name, g.ComputeCapability, g.ArchID}
			cards[c]++
			memory[c] = max(memory[c], g.MemoryTotal)
			if !seen[c] {
				seen[c] = true
				gpus[c]++
			}
		}
	}
	out := dto.NodeFacets{Total: len(nodes), Labels: []dto.NodeLabelCounts{}, SystemLabels: []dto.NodeSystemLabelCounts{}, GPUs: []dto.NodeGPUCount{}}
	for key, values := range written {
		item := dto.NodeLabelCounts{Key: key, Values: make([]dto.ValueCount, 0, len(values))}
		for value, count := range values {
			item.Values = append(item.Values, dto.ValueCount{Value: value, Nodes: count})
		}
		sort.Slice(item.Values, func(i, j int) bool { return item.Values[i].Value < item.Values[j].Value })
		out.Labels = append(out.Labels, item)
	}
	sort.Slice(out.Labels, func(i, j int) bool { return out.Labels[i].Key < out.Labels[j].Key })
	for key, values := range system {
		item := dto.NodeSystemLabelCounts{Key: key, Values: make([]dto.SystemLabelValueCount, 0, len(values))}
		for _, entry := range values {
			item.Values = append(item.Values, *entry)
		}
		sort.Slice(item.Values, func(i, j int) bool { return item.Values[i].Value < item.Values[j].Value })
		out.SystemLabels = append(out.SystemLabels, item)
	}
	sort.Slice(out.SystemLabels, func(i, j int) bool { return out.SystemLabels[i].Key < out.SystemLabels[j].Key })
	for c, count := range gpus {
		out.GPUs = append(out.GPUs, dto.NodeGPUCount{Vendor: c.vendor, Name: c.name, CC: c.cc, ArchID: c.archID, Model: accelerator.NormalizeGPUName(c.name), Nodes: count, Cards: cards[c], MemoryBytes: memory[c]})
	}
	sort.Slice(out.GPUs, func(i, j int) bool {
		a, b := out.GPUs[i], out.GPUs[j]
		if a.Vendor != b.Vendor {
			return a.Vendor < b.Vendor
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.CC != b.CC {
			return a.CC < b.CC
		}
		return a.ArchID < b.ArchID
	})
	return out
}
