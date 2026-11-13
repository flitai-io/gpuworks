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

// KubeLeave is what a confirmed change of an entry's node selection does to
// the nodes it leaves out, written in the same transaction as the change so a
// change that fails (a name taken, an entry gone) stops nothing.
type KubeLeave struct {
	// StopNodeIDs are the nodes whose instances are asked to stop.
	StopNodeIDs []uint
	// CordonNodeUUIDs are the nodes that take no new replica from now on,
	// as if an operator had cordoned them.
	CordonNodeUUIDs []string
	// Message is what the stopped instances say on their timelines.
	Message string
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
// A confirmed selection change does this inside the transaction that saves it
// (UpdateKubeClusterLeaving, SetKubeClusterPoolsLeaving); this is the same
// write on its own.
func (s *Store) StopInstancesOnNodes(nodeIDs []uint, message string) ([]StoppedInstance, error) {
	var rows []Instance
	var stopped []StoppedInstance
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var err error
		rows, stopped, err = stopInstancesOnNodesTx(tx, nodeIDs, message)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.announceTransition(rows, proto.InstRemoving, message)
	return stopped, nil
}

// stopInstancesOnNodesTx moves the instances in tx and returns their rows (as
// before the move, for the caller to announce once the transaction commits)
// and their descriptions. Reads follow the instances' node index and the
// deployments' primary key.
func stopInstancesOnNodesTx(tx *gorm.DB, nodeIDs []uint, message string) ([]Instance, []StoppedInstance, error) {
	if len(nodeIDs) == 0 {
		return nil, nil, nil
	}
	from := append(ReplicaFillingStates(), proto.InstDraining)
	var rows []Instance
	for start := 0; start < len(nodeIDs); start += kubeInstanceChunk {
		var chunk []Instance
		if err := tx.Select("id", "uuid", "deployment_id", "node_id").
			Where("node_id IN ? AND state IN ?", nodeIDs[start:min(start+kubeInstanceChunk, len(nodeIDs))], from).Order("id").Find(&chunk).Error; err != nil {
			return nil, nil, err
		}
		rows = append(rows, chunk...)
	}
	if len(rows) == 0 {
		return nil, nil, nil
	}
	ids := make([]uint, 0, len(rows))
	wanted := map[uint]bool{}
	nodes := map[uint]bool{}
	for _, r := range rows {
		ids = append(ids, r.ID)
		wanted[r.DeploymentID], nodes[r.NodeID] = true, true
	}
	for start := 0; start < len(ids); start += kubeInstanceChunk {
		if err := tx.Model(&Instance{}).Where("id IN ?", ids[start:min(start+kubeInstanceChunk, len(ids))]).
			Updates(map[string]any{"state": proto.InstRemoving, "message": message, "runtime_failure": ""}).Error; err != nil {
			return nil, nil, err
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	names, err := deploymentNamesOn(tx, wanted)
	if err != nil {
		return nil, nil, err
	}
	nodeUUIDs, err := nodeUUIDsByIDOn(tx, nodes)
	if err != nil {
		return nil, nil, err
	}
	out := make([]StoppedInstance, 0, len(rows))
	for _, r := range rows {
		out = append(out, StoppedInstance{InstanceUUID: r.UUID, DeploymentID: r.DeploymentID, Deployment: names[r.DeploymentID], NodeUUID: nodeUUIDs[r.NodeID]})
	}
	return rows, out, nil
}

// cordonNodesTx takes the nodes out of scheduling as an operator's cordon, in tx.
func cordonNodesTx(tx *gorm.DB, uuids []string) error {
	if len(uuids) == 0 {
		return nil
	}
	return tx.Model(&Node{}).Where("uuid IN ?", uuids).
		Updates(map[string]any{"scheduling": NodeCordoned, "scheduling_by": SchedulingByOperator}).Error
}

// applyKubeLeaveTx writes what leave asks for in tx.
func applyKubeLeaveTx(tx *gorm.DB, leave KubeLeave) ([]Instance, []StoppedInstance, error) {
	if err := cordonNodesTx(tx, leave.CordonNodeUUIDs); err != nil {
		return nil, nil, err
	}
	return stopInstancesOnNodesTx(tx, leave.StopNodeIDs, leave.Message)
}

// announceKubeLeave publishes what applyKubeLeaveTx wrote, after commit.
func (s *Store) announceKubeLeave(leave KubeLeave, rows []Instance) {
	for _, uuid := range leave.CordonNodeUUIDs {
		s.publishNode(uuid, NodeChangeScheduling)
	}
	s.announceTransition(rows, proto.InstRemoving, leave.Message)
}

// nodeUUIDsByIDOn reads the uuids of the given nodes by primary key.
func nodeUUIDsByIDOn(db *gorm.DB, wanted map[uint]bool) (map[uint]string, error) {
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
		if err := db.Model(&Node{}).Select("id", "uuid").Where("id IN ?", ids[start:min(start+kubeInstanceChunk, len(ids))]).Scan(&rows).Error; err != nil {
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
