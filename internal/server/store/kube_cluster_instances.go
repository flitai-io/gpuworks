package store

import (
	"sort"

	"gpuworks.ai/gpuworks/internal/proto"
)

// KubeNodeDeployments is a node of a Kubernetes cluster entry that runs
// instances, with the names of the deployments they belong to.
type KubeNodeDeployments struct {
	Node        Node
	Deployments []string
}

// kubeInstanceChunk bounds the ids of one IN list.
const kubeInstanceChunk = 500

// KubeNodesWithInstances lists the nodes of a cluster entry that still run
// instances (any instance not yet finished, see proto.InstanceFinishedStates),
// in node id order, with the deployments those instances belong to,
// sorted. Reads follow the node's cluster index, the instances' node index and
// the deployments' primary key, never a whole table.
func (s *Store) KubeNodesWithInstances(clusterID uint) ([]KubeNodeDeployments, error) {
	var nodes []Node
	if err := s.reader().Select("id", "uuid", "name", "kube_node_name", "scheduling").Where("kube_cluster_id = ?", clusterID).Order("id").Find(&nodes).Error; err != nil {
		return nil, err
	}
	ids := make([]uint, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	deployments := map[uint]map[uint]bool{} // node -> deployment ids
	for start := 0; start < len(ids); start += kubeInstanceChunk {
		var rows []struct{ NodeID, DeploymentID uint }
		err := s.reader().Model(&Instance{}).Select("node_id", "deployment_id").
			Where("node_id IN ? AND state NOT IN ?", ids[start:min(start+kubeInstanceChunk, len(ids))], proto.InstanceFinishedStates()).
			Scan(&rows).Error
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if deployments[r.NodeID] == nil {
				deployments[r.NodeID] = map[uint]bool{}
			}
			deployments[r.NodeID][r.DeploymentID] = true
		}
	}
	wanted := map[uint]bool{}
	for _, set := range deployments {
		for id := range set {
			wanted[id] = true
		}
	}
	names, err := s.deploymentNames(wanted)
	if err != nil {
		return nil, err
	}
	out := []KubeNodeDeployments{}
	for i := range nodes {
		set := deployments[nodes[i].ID]
		if len(set) == 0 {
			continue
		}
		item := KubeNodeDeployments{Node: nodes[i], Deployments: []string{}}
		for id := range set {
			if name := names[id]; name != "" {
				item.Deployments = append(item.Deployments, name)
			}
		}
		sort.Strings(item.Deployments)
		out = append(out, item)
	}
	return out, nil
}

// deploymentNames reads the names of the given deployments by id.
func (s *Store) deploymentNames(wanted map[uint]bool) (map[uint]string, error) {
	ids := make([]uint, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}
	out := make(map[uint]string, len(ids))
	for start := 0; start < len(ids); start += kubeInstanceChunk {
		var rows []struct {
			ID   uint
			Name string
		}
		if err := s.reader().Model(&Deployment{}).Select("id", "name").Where("id IN ?", ids[start:min(start+kubeInstanceChunk, len(ids))]).Scan(&rows).Error; err != nil {
			return nil, err
		}
		for _, r := range rows {
			out[r.ID] = r.Name
		}
	}
	return out, nil
}
