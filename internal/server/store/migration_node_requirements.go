package store

import (
	"encoding/json"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

// moveNodeRequirementsOutOfPlacement is migration 120. Where a deployment may
// run used to be spread over three places: a label selector inside the
// placement document, the vendor of whichever node the request named (never
// stored), and the node list. It becomes the deployment_node_requirements
// table plus a placement_strategy column, and the placement document loses
// the keys that described it.
//
// Every existing deployment keeps exactly the nodes it could run on before:
//
//   - A selector that is a valid written-label map becomes one requirement
//     per pair and leaves the document. A selector that is not - malformed,
//     or naming a key migration 119 deleted from every node - is left in the
//     document, where the placement parser refuses it: dropping it would
//     widen where the deployment may run, and that refusal is the existing
//     fail-closed path for a selector the platform cannot honour.
//   - The vendor of the deployment's primary node, when the node is known
//     and reports one, becomes the vendor requirement.
//   - A single-node deployment that is not PD is pinned to its node list as
//     gpuworks.ai/node: nothing ever recorded whether its node was picked by
//     the platform or by a person, so it stays where it was. A node that no
//     longer exists is pinned by a value no node carries ("#<id>"), so the
//     pin can never become "no pin". Multi-node and PD deployments name
//     their nodes per rank and get no node requirement.
//   - "mode" and "node_uuid" at the top of the document are dropped. Nothing
//     has read either for many releases; the nested node_uuid of PD and
//     multi-node units is a different key and is kept.
//
// A document that does not decode is left exactly as it is, and so is the
// node list: this migration moves facts, it does not repair rows.
func moveNodeRequirementsOutOfPlacement(db *gorm.DB) error {
	if err := db.AutoMigrate(&DeploymentNodeRequirement{}); err != nil {
		return err
	}
	if !db.Migrator().HasTable("deployments") {
		return nil
	}
	if !db.Migrator().HasColumn(&Deployment{}, "PlacementStrategy") {
		if err := db.Migrator().AddColumn(&Deployment{}, "PlacementStrategy"); err != nil {
			return err
		}
	}
	type deploymentRow struct {
		ID        uint
		NodeID    uint
		NodeIDs   string
		NNodes    int
		Placement string
	}
	var rows []deploymentRow
	if err := db.Raw("SELECT id, node_id, node_ids, n_nodes, placement FROM deployments ORDER BY id").Scan(&rows).Error; err != nil {
		return err
	}
	nodes, err := migrationNodeFacts(db)
	if err != nil {
		return err
	}
	for _, row := range rows {
		reqs := nodelabel.Requirements{}
		document, decoded := placementDocument(row.Placement)
		changed := false
		if decoded {
			for _, dead := range []string{"mode", "node_uuid"} {
				if _, present := document[dead]; present {
					delete(document, dead)
					changed = true
				}
			}
			if selector, ok := movableSelector(document); ok {
				for key, value := range selector {
					reqs[key] = []string{value}
				}
				delete(document, "node_labels")
				changed = true
			}
		}
		if vendor := nodes[row.NodeID].vendor; vendor != "" {
			reqs[nodelabel.KeyVendor] = []string{vendor}
		}
		_, pd := document["pd"]
		if decoded && row.NNodes <= 1 && !pd {
			reqs[nodelabel.KeyNode] = pinnedNodes(row.NodeID, row.NodeIDs, nodes)
		}
		if err := insertRequirementRows(db, requirementRows(row.ID, reqs)); err != nil {
			return err
		}
		if !changed {
			continue
		}
		encoded, err := json.Marshal(document)
		if err != nil {
			return err
		}
		if err := db.Exec("UPDATE deployments SET placement = ? WHERE id = ?", string(encoded), row.ID).Error; err != nil {
			return err
		}
	}
	return nil
}

type migrationNode struct {
	uuid   string
	vendor string
}

func migrationNodeFacts(db *gorm.DB) (map[uint]migrationNode, error) {
	out := map[uint]migrationNode{}
	if !db.Migrator().HasTable("nodes") {
		return out, nil
	}
	type nodeRow struct {
		ID        uint
		UUID      string
		GPUVendor string
	}
	var rows []nodeRow
	if err := db.Raw("SELECT id, uuid, gpu_vendor FROM nodes").Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.ID] = migrationNode{uuid: row.UUID, vendor: accelerator.NormalizeVendor(row.GPUVendor)}
	}
	return out, nil
}

// placementDocument decodes a stored placement. An empty column and a JSON
// null are the empty document; anything that is not an object does not
// decode.
func placementDocument(stored string) (map[string]any, bool) {
	text := strings.TrimSpace(stored)
	if text == "" || text == "null" {
		return map[string]any{}, true
	}
	document := map[string]any{}
	if json.Unmarshal([]byte(text), &document) != nil || document == nil {
		return nil, false
	}
	return document, true
}

// movableSelector is the document's node_labels when it is a written-label
// map that could have matched a node; see moveNodeRequirementsOutOfPlacement
// for why anything else stays behind.
func movableSelector(document map[string]any) (map[string]string, bool) {
	raw, present := document["node_labels"]
	if !present {
		return nil, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	var selector map[string]string
	if json.Unmarshal(encoded, &selector) != nil || selector == nil {
		return nil, false
	}
	if nodelabel.ValidateWritten(selector) != nil {
		return nil, false
	}
	return selector, true
}

// pinnedNodes is the node requirement a single-node deployment keeps: the
// UUIDs of its node list, read the way the controller reads it (the list, or
// the primary node when the list is empty or does not decode).
func pinnedNodes(nodeID uint, nodeIDs string, nodes map[uint]migrationNode) []string {
	ids := (&Deployment{NodeID: nodeID, NodeIDs: nodeIDs}).NodeIDList()
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if node, ok := nodes[id]; ok && node.uuid != "" {
			out = append(out, node.uuid)
			continue
		}
		out = append(out, fmt.Sprintf("#%d", id))
	}
	return out
}
