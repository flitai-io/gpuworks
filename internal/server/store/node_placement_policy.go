package store

import (
	"errors"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/nodepolicy"
)

// NodePlacementPolicy is the placement policy a node row embeds, named here
// so callers that write it through the store need not import nodepolicy.
type NodePlacementPolicy = nodepolicy.Policy

// NodeChangePlacementPolicy is the change reason of an operator's edit of a
// node's placement policy. Like a cordon it is not a status report, so the
// controller retries the deployments waiting for a node at once.
const NodeChangePlacementPolicy = "placement_policy"

// SetNodePlacementPolicy reads the node's placement policy, lets apply
// produce the new one and writes it back whole, in one transaction: only the
// policy's own columns (placement_*) are written, whatever knobs it has.
// It returns the policy before and after, and the node with its GPUs. A
// missing node is ErrNotFound; an error from apply writes nothing.
func (s *Store) SetNodePlacementPolicy(uuid string, apply func(nodepolicy.Policy) (nodepolicy.Policy, error)) (old, updated nodepolicy.Policy, node *Node, err error) {
	err = s.db.Transaction(func(tx *gorm.DB) error {
		columns := placementPolicyColumns()
		var row Node
		if err := tx.Select(append([]string{"id"}, columns...)).Where("uuid = ?", uuid).Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		old = row.PlacementPolicy
		next, err := apply(old)
		if err != nil {
			return err
		}
		updated = next
		// Every knob is written by its column, so one switched off is
		// written rather than skipped as a zero value.
		values := make(map[string]any, len(columns))
		for key, value := range nodepolicy.Values(next) {
			values[placementPolicyColumn(key)] = value
		}
		return tx.Model(&Node{}).Where("id = ?", row.ID).Updates(values).Error
	})
	if err != nil {
		return old, updated, nil, err
	}
	s.publishNode(uuid, NodeChangePlacementPolicy)
	node, err = s.NodeWithGPUs(uuid)
	return old, updated, node, err
}

// placementPolicyColumn is a knob's column on the node row: the embedded
// Policy's prefix and the knob's key (nodepolicy: Key equals the column name
// without the prefix).
func placementPolicyColumn(key nodepolicy.Key) string { return "placement_" + string(key) }

// placementPolicyColumns are every knob's column.
func placementPolicyColumns() []string {
	settings := nodepolicy.Settings()
	columns := make([]string, 0, len(settings))
	for _, setting := range settings {
		columns = append(columns, placementPolicyColumn(setting.Key))
	}
	return columns
}
