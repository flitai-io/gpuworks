package store

import "strings"

// NodeSource is the external platform a node joined through: the account an
// agent was installed on one of whose instances, or the Kubernetes cluster
// whose DaemonSet runs the node's agent (and the account that cluster was
// connected from, when it was). A node added by hand has none.
type NodeSource struct {
	// ProviderID is 0 for a cluster connected on its own.
	ProviderID   uint
	ProviderName string
	ProviderKind string
	// InstanceKind is the instance's own kind ("vm", "container"); empty for
	// a cluster node.
	InstanceKind string
	// ClusterUUID and ClusterName are set for a cluster node only.
	ClusterUUID string
	ClusterName string
}

// NodeSources says which external platform each of nodes joined through, with
// one query over the instances of those nodes and one over their distinct
// clusters. Nodes added by hand are absent. It is for a page of nodes; the
// whole fleet goes through FleetNodeSources.
func (s *Store) NodeSources(nodes []Node) (map[uint]NodeSource, error) {
	return s.nodeSources(nodes, false)
}

// FleetNodeSources is NodeSources for every node there is: it reads every
// instance a node sits on and every cluster a node joined from, rather than
// naming a thousand ids in one statement.
func (s *Store) FleetNodeSources(nodes []Node) (map[uint]NodeSource, error) {
	return s.nodeSources(nodes, true)
}

// NodeCountsByProvider is how many compute nodes joined through each account,
// on its instances or through the clusters connected from it, keyed by
// provider id. It counts the sources NodeSources derives, so an account's
// count is exactly the nodes its source filter lists.
func (s *Store) NodeCountsByProvider() (map[uint]int, error) {
	var nodes []Node
	if err := s.reader().Select("id", "kube_cluster_id").Find(&nodes).Error; err != nil {
		return nil, err
	}
	sources, err := s.nodeSources(nodes, true)
	if err != nil {
		return nil, err
	}
	out := map[uint]int{}
	for i := range nodes {
		if source, ok := sources[nodes[i].ID]; ok && source.ProviderID != 0 {
			out[source.ProviderID]++
		}
	}
	return out, nil
}

func (s *Store) nodeSources(nodes []Node, fleet bool) (map[uint]NodeSource, error) {
	out := map[uint]NodeSource{}
	if len(nodes) == 0 {
		return out, nil
	}
	ids := make([]uint, 0, len(nodes))
	var clusterIDs []uint
	seen := map[uint]bool{}
	for i := range nodes {
		ids = append(ids, nodes[i].ID)
		if id := nodes[i].KubeClusterID; id != 0 && !seen[id] {
			seen[id] = true
			clusterIDs = append(clusterIDs, id)
		}
	}
	var instances []struct {
		NodeID       uint
		ProviderID   uint
		ProviderName string
		ProviderKind string
		InstanceKind string
	}
	query := s.reader().Table("provider_instances").
		Select("provider_instances.node_id AS node_id, providers.id AS provider_id, providers.name AS provider_name, providers.kind AS provider_kind, provider_instances.kind AS instance_kind").
		Joins("JOIN providers ON providers.id = provider_instances.provider_id")
	if fleet {
		query = query.Where("provider_instances.node_id > 0")
	} else {
		query = query.Where("provider_instances.node_id IN ?", ids)
	}
	if err := query.Scan(&instances).Error; err != nil {
		return nil, err
	}
	for _, row := range instances {
		out[row.NodeID] = NodeSource{ProviderID: row.ProviderID, ProviderName: row.ProviderName,
			ProviderKind: strings.TrimSpace(row.ProviderKind), InstanceKind: strings.TrimSpace(row.InstanceKind)}
	}
	if len(clusterIDs) == 0 {
		return out, nil
	}
	var clusters []struct {
		ID           uint
		UUID         string
		Name         string
		ProviderID   uint
		ProviderName string
		ProviderKind string
	}
	query = s.reader().Table("kube_clusters").
		Select("kube_clusters.id AS id, kube_clusters.uuid AS uuid, kube_clusters.name AS name, COALESCE(providers.id, 0) AS provider_id, COALESCE(providers.name, '') AS provider_name, COALESCE(providers.kind, '') AS provider_kind").
		Joins("LEFT JOIN providers ON providers.id = kube_clusters.provider_id")
	if !fleet {
		query = query.Where("kube_clusters.id IN ?", clusterIDs)
	}
	if err := query.Scan(&clusters).Error; err != nil {
		return nil, err
	}
	byCluster := make(map[uint]NodeSource, len(clusters))
	for _, c := range clusters {
		byCluster[c.ID] = NodeSource{ProviderID: c.ProviderID, ProviderName: c.ProviderName, ProviderKind: strings.TrimSpace(c.ProviderKind),
			ClusterUUID: c.UUID, ClusterName: c.Name}
	}
	// A node of a managed cluster is also an instance of the account: it
	// joined through the cluster, which is what it is listed under.
	for i := range nodes {
		if source, ok := byCluster[nodes[i].KubeClusterID]; ok {
			out[nodes[i].ID] = source
		}
	}
	return out, nil
}
