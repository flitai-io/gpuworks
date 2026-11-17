package store

import (
	"errors"

	"gorm.io/gorm"
)

// The narrow reads below serve the controller's event routing: a bus event
// names a node, an instance or a model, and the controller needs only which
// deployments that touches, never the deployment rows themselves.

// DeploymentIDs lists every deployment's id, without loading the rows.
func (s *Store) DeploymentIDs() ([]uint, error) {
	var ids []uint
	err := s.reader().Model(&Deployment{}).Order("id").Pluck("id", &ids).Error
	return ids, err
}

// DeploymentIDsOnNodes lists the deployments with an instance, in any state,
// on one of the nodes named by UUID. unknown reports that some UUID matched no
// node: a deleted node's instances can no longer be found through it, so the
// caller cannot know the answer is complete. Both reads bind one IN list, so
// the UUIDs a single call names stay far below SQLite's variable limit only
// while callers batch a tick's worth of nodes, not the fleet's history.
func (s *Store) DeploymentIDsOnNodes(uuids []string) (ids []uint, unknown bool, err error) {
	if len(uuids) == 0 {
		return nil, false, nil
	}
	wanted := map[string]bool{}
	for _, uuid := range uuids {
		wanted[uuid] = true
	}
	var nodes []struct {
		ID   uint
		UUID string
	}
	keys := make([]string, 0, len(wanted))
	for uuid := range wanted {
		keys = append(keys, uuid)
	}
	if err := s.reader().Model(&Node{}).Select("id", "uuid").Where("uuid IN ?", keys).Scan(&nodes).Error; err != nil {
		return nil, false, err
	}
	nodeIDs := make([]uint, 0, len(nodes))
	for _, n := range nodes {
		delete(wanted, n.UUID)
		nodeIDs = append(nodeIDs, n.ID)
	}
	if len(nodeIDs) > 0 {
		if err := s.reader().Model(&Instance{}).Distinct("deployment_id").Where("node_id IN ?", sortedUnique(nodeIDs)).
			Order("deployment_id").Pluck("deployment_id", &ids).Error; err != nil {
			return nil, false, err
		}
	}
	return ids, len(wanted) > 0, nil
}

// InstanceRefs returns the deployment and node of the instance with this
// UUID, without loading the row.
func (s *Store) InstanceRefs(uuid string) (deploymentID, nodeID uint, err error) {
	var row struct {
		DeploymentID uint
		NodeID       uint
	}
	err = s.reader().Model(&Instance{}).Select("deployment_id", "node_id").Where("uuid = ?", uuid).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, 0, ErrNotFound
	}
	return row.DeploymentID, row.NodeID, err
}
