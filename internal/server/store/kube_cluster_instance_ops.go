package store

import (
	"encoding/json"
	"sort"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// StoppedInstance is an instance a stop request moved to removing, with where
// it ran.
type StoppedInstance struct {
	InstanceUUID string
	DeploymentID uint
	Deployment   string
	NodeUUID     string
}

// StopInstancesOnNodes asks every instance on the given nodes that has not
// finished or begun leaving to stop: it is moved to removing, the state the
// controller and the node's agent already treat as "stop this and reclaim its
// cards" (a restart without draining uses the same one), in one transaction.
// The instances are listed in id order. Nothing waits for the agent. Other
// instances of the same deployments are not touched, so a deployment that also
// runs elsewhere keeps those replicas; the controller decides afterwards
// whether and where a replica is rebuilt.
//
// Reads follow the instances' node index and the deployments' primary key.
func (s *Store) StopInstancesOnNodes(nodeIDs []uint, message string) ([]StoppedInstance, error) {
	if len(nodeIDs) == 0 {
		return nil, nil
	}
	from := append(ReplicaFillingStates(), proto.InstDraining)
	var rows []Instance
	err := s.db.Transaction(func(tx *gorm.DB) error {
		for start := 0; start < len(nodeIDs); start += kubeInstanceChunk {
			var chunk []Instance
			if err := tx.Select("id", "uuid", "deployment_id", "node_id").
				Where("node_id IN ? AND state IN ?", nodeIDs[start:min(start+kubeInstanceChunk, len(nodeIDs))], from).Order("id").Find(&chunk).Error; err != nil {
				return err
			}
			rows = append(rows, chunk...)
		}
		if len(rows) == 0 {
			return nil
		}
		ids := make([]uint, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		for start := 0; start < len(ids); start += kubeInstanceChunk {
			if err := tx.Model(&Instance{}).Where("id IN ?", ids[start:min(start+kubeInstanceChunk, len(ids))]).
				Updates(map[string]any{"state": proto.InstRemoving, "message": message, "runtime_failure": ""}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	s.announceTransition(rows, proto.InstRemoving, message)
	wanted := map[uint]bool{}
	nodes := map[uint]bool{}
	for _, r := range rows {
		wanted[r.DeploymentID], nodes[r.NodeID] = true, true
	}
	names, err := s.deploymentNames(wanted)
	if err != nil {
		return nil, err
	}
	nodeUUIDs, err := s.nodeUUIDsByID(nodes)
	if err != nil {
		return nil, err
	}
	out := make([]StoppedInstance, 0, len(rows))
	for _, r := range rows {
		out = append(out, StoppedInstance{InstanceUUID: r.UUID, DeploymentID: r.DeploymentID, Deployment: names[r.DeploymentID], NodeUUID: nodeUUIDs[r.NodeID]})
	}
	return out, nil
}

// nodeUUIDsByID reads the uuids of the given nodes by primary key.
func (s *Store) nodeUUIDsByID(wanted map[uint]bool) (map[uint]string, error) {
	ids := make([]uint, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}
	out := make(map[uint]string, len(ids))
	for start := 0; start < len(ids); start += kubeInstanceChunk {
		var rows []struct {
			ID   uint
			UUID string
		}
		if err := s.reader().Model(&Node{}).Select("id", "uuid").Where("id IN ?", ids[start:min(start+kubeInstanceChunk, len(ids))]).Scan(&rows).Error; err != nil {
			return nil, err
		}
		for _, r := range rows {
			out[r.ID] = r.UUID
		}
	}
	return out, nil
}

// KubeInstancePorts is an instance of a cluster entry's nodes that has not
// finished, with the host ports it holds.
type KubeInstancePorts struct {
	InstanceUUID string
	Deployment   string
	NodeUUID     string
	Ports        []int // the allocated ports, ascending, API port included
}

// KubeClusterInstancePorts lists the instances on an entry's nodes that have
// not finished (see KubeNodesWithInstances) with the ports they hold, in
// instance id order: the ports the instance's row recorded (Ports, which a PD
// instance's side channels are part of) and its API port. Reads follow the
// node's cluster index, the instances' node index and the deployments'
// primary key.
func (s *Store) KubeClusterInstancePorts(clusterID uint) ([]KubeInstancePorts, error) {
	var nodes []struct {
		ID   uint
		UUID string
	}
	if err := s.reader().Model(&Node{}).Select("id", "uuid").Where("kube_cluster_id = ?", clusterID).Order("id").Scan(&nodes).Error; err != nil {
		return nil, err
	}
	nodeUUID := make(map[uint]string, len(nodes))
	ids := make([]uint, 0, len(nodes))
	for _, n := range nodes {
		nodeUUID[n.ID] = n.UUID
		ids = append(ids, n.ID)
	}
	var rows []Instance
	for start := 0; start < len(ids); start += kubeInstanceChunk {
		var chunk []Instance
		err := s.reader().Select("id", "uuid", "deployment_id", "node_id", "ports", "api_port").
			Where("node_id IN ? AND state NOT IN ?", ids[start:min(start+kubeInstanceChunk, len(ids))], proto.InstanceFinishedStates()).Find(&chunk).Error
		if err != nil {
			return nil, err
		}
		rows = append(rows, chunk...)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	wanted := map[uint]bool{}
	for _, r := range rows {
		wanted[r.DeploymentID] = true
	}
	names, err := s.deploymentNames(wanted)
	if err != nil {
		return nil, err
	}
	out := make([]KubeInstancePorts, 0, len(rows))
	for _, r := range rows {
		var ports []int
		if r.Ports != "" {
			_ = json.Unmarshal([]byte(r.Ports), &ports)
		}
		if r.APIPort > 0 {
			ports = append(ports, r.APIPort)
		}
		held := map[int]bool{}
		list := []int{}
		for _, p := range ports {
			if p > 0 && !held[p] {
				held[p] = true
				list = append(list, p)
			}
		}
		sort.Ints(list)
		out = append(out, KubeInstancePorts{InstanceUUID: r.UUID, Deployment: names[r.DeploymentID], NodeUUID: nodeUUID[r.NodeID], Ports: list})
	}
	return out, nil
}
