package store

import (
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/accelerator"

	"gpuworks.ai/gpuworks/pkg/topoparse"
)

// OnlineGPUGroup is the cards of one model on one online node. A fleet-wide
// sizing summary needs only these counts, so it reads grouped rows per card
// model per node instead of every node and GPU row.
type OnlineGPUGroup struct {
	NodeID            uint
	Framework         string
	Vendor            string
	Name              string
	ArchID            string
	ComputeCapability string
	// Cards counts the cards that can take work (GPU.CanTakeWork); Unusable
	// the node's own cards that cannot. A card that is not the node's own —
	// lent to a virtual machine, or held by another driver — is neither:
	// it is not this node's capacity, and it is not broken either.
	Cards     int
	Unusable  int
	MaxMemory int64
}

// OnlineGPUGroups groups the online nodes' cards by node and card model.
// Card names are normalized when a report is stored (newCardRow), so one
// model is one name.
//
// SQL does the grouping and Go decides which groups count: the rows are
// further split by exactly the facts GPU.CanTakeWork reads, and each split
// is put to the same rule (accelerator.CardSchedulable), which lives
// nowhere else. The splits add at
// most a handful of rows per node and model.
func (s *Store) OnlineGPUGroups() ([]OnlineGPUGroup, error) {
	var rows []struct {
		OnlineGPUGroup
		Ownership    string
		HealthStatus string
		Held         bool
		Stopped      bool
		Allowed      bool
		HolderHeld   bool
		Count        int
	}
	err := s.reader().Raw(`SELECT n.id AS node_id, n.framework AS framework, g.vendor AS vendor, g.name AS name,
		g.arch_id AS arch_id, g.compute_capability AS compute_capability,
		g.ownership AS ownership, g.health_status AS health_status, g.foreign_processes <> '' AS held,
		g.scheduling_stopped AS stopped, g.scheduling_allowed_at IS NOT NULL AS allowed,
		g.scheduling_held_by <> '' AS holder_held,
		COUNT(*) AS count, MAX(g.memory_total) AS max_memory
		FROM gpus g JOIN nodes n ON n.id = g.node_id
		WHERE n.state = ?
		GROUP BY n.id, n.framework, g.vendor, g.name, g.arch_id, g.compute_capability,
			g.ownership, g.health_status, g.foreign_processes <> '',
			g.scheduling_stopped, g.scheduling_allowed_at IS NOT NULL, g.scheduling_held_by <> ''`, NodeStateOnline).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	type groupKey struct {
		node                              uint
		framework, vendor, name, arch, cc string
	}
	index := map[groupKey]int{}
	var out []OnlineGPUGroup
	for _, row := range rows {
		if !accelerator.CardUsable(row.Ownership) {
			continue
		}
		key := groupKey{row.NodeID, row.Framework, row.Vendor, row.Name, row.ArchID, row.ComputeCapability}
		i, seen := index[key]
		if !seen {
			i = len(out)
			index[key] = i
			group := row.OnlineGPUGroup
			group.Cards, group.Unusable, group.MaxMemory = 0, 0, 0
			out = append(out, group)
		}
		if cardTakesWork(row.Ownership, row.HealthStatus, row.Held, row.Stopped, row.Allowed, row.HolderHeld) {
			out[i].Cards += row.Count
		} else {
			out[i].Unusable += row.Count
		}
		out[i].MaxMemory = max(out[i].MaxMemory, row.MaxMemory)
	}
	return out, nil
}

// NodeCardLayout is one node's interconnect and which card model sits at each
// index - what it takes to say how many cards of a model share a fast link.
type NodeCardLayout struct {
	Topology *topoparse.Topology
	// Names maps a card index to its (normalized) model name.
	Names map[int]string
}

// NodeCardLayouts reads the layouts of a bounded set of nodes in two queries.
// A node without a parseable topology still gets its card names.
func (s *Store) NodeCardLayouts(nodeIDs []uint) (map[uint]NodeCardLayout, error) {
	out := make(map[uint]NodeCardLayout, len(nodeIDs))
	if len(nodeIDs) == 0 {
		return out, nil
	}
	var nodes []Node
	if err := s.reader().Select("id", "topo_json").Where("id IN ?", nodeIDs).Find(&nodes).Error; err != nil {
		return nil, err
	}
	for _, n := range nodes {
		layout := NodeCardLayout{Names: map[int]string{}}
		if n.TopoJSON != "" {
			var t topoparse.Topology
			if json.Unmarshal([]byte(n.TopoJSON), &t) == nil {
				layout.Topology = &t
			}
		}
		out[n.ID] = layout
	}
	var gpus []GPU
	if err := s.reader().Select("node_id", "index", "name").Where("node_id IN ?", nodeIDs).Find(&gpus).Error; err != nil {
		return nil, err
	}
	for _, g := range gpus {
		if layout, ok := out[g.NodeID]; ok {
			layout.Names[g.Index] = g.Name
		}
	}
	return out, nil
}
