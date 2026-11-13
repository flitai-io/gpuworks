package planning

import (
	"strconv"

	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// NodeFacts are what generating a node's system labels needs beyond the node
// row. Either part may be nil, which leaves its labels off - that is what a
// caller that did not load it knows, not a claim that the node has none.
type NodeFacts struct {
	// Networks is the nodes' network resolution.
	Networks netplan.Resolution
	// Storages names the storages that apply to each node, by node id.
	Storages StorageScope
	// Providers says where each rented node runs, by node id.
	Providers map[uint]store.NodeProvider
	// KubeClusters names the Kubernetes clusters nodes joined from, by
	// cluster id: loaded once for a whole computation, never per node.
	KubeClusters KubeClusters
}

// KubeClusterRef is what a node's Kubernetes cluster label is made of: the
// cluster's UUID (the label value, stable across renames) and its name (the
// text shown).
type KubeClusterRef struct {
	UUID string
	Name string
}

// KubeClusters maps a Kubernetes cluster id to its UUID and name.
type KubeClusters map[uint]KubeClusterRef

// StorageScope lists the storages that apply to each node, by node id.
type StorageScope map[uint][]store.Storage

// LoadNodeFacts loads what matching reqs against nodes reads beyond the node
// rows: their networks (through resolve, the caller's network resolution) and
// the storages that apply to them, each only when a requirement names one. A
// part that cannot be loaded stays nil, which leaves its requirements unmet:
// an unreadable fact is never a match.
func LoadNodeFacts(st *store.Store, resolve func([]store.Node) (netplan.Resolution, error), nodes []store.Node, reqs nodelabel.Requirements) NodeFacts {
	var facts NodeFacts
	if RequirementsNeedNetworks(reqs) && resolve != nil {
		if resolved, err := resolve(nodes); err == nil {
			facts.Networks = resolved
		}
	}
	if RequirementsNeedProviders(reqs) {
		ids := make([]uint, len(nodes))
		for i := range nodes {
			ids[i] = nodes[i].ID
		}
		if providers, err := st.NodeProviders(ids); err == nil {
			facts.Providers = providers
		}
	}
	if RequirementsNeedKubeClusters(reqs) {
		facts.KubeClusters = LoadKubeClusters(st, nodes)
	}
	if RequirementsNeedStorages(reqs) {
		ptrs := make([]*store.Node, len(nodes))
		for i := range nodes {
			ptrs[i] = &nodes[i]
		}
		if storages, err := LoadStorageScope(st, ptrs); err == nil {
			facts.Storages = storages
		}
	}
	return facts
}

// LoadKubeClusters reads the clusters nodes joined from with one query over
// the distinct cluster ids; nil when no node joined from one or the read
// fails (a node then carries no cluster label, never a wrong one).
func LoadKubeClusters(st *store.Store, nodes []store.Node) KubeClusters {
	seen := map[uint]bool{}
	var ids []uint
	for i := range nodes {
		if id := nodes[i].KubeClusterID; id != 0 && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := st.KubeClusterRefs(ids)
	if err != nil {
		return nil
	}
	out := make(KubeClusters, len(rows))
	for id, row := range rows {
		out[id] = KubeClusterRef{UUID: row.UUID, Name: row.Name}
	}
	return out
}

// RequirementsNeedKubeClusters reports whether matching reqs reads the name
// or UUID of a node's Kubernetes cluster. The Kubernetes node label comes
// from the node row alone and needs nothing loaded.
func RequirementsNeedKubeClusters(reqs nodelabel.Requirements) bool {
	return len(reqs[nodelabel.KeyKubernetesCluster]) > 0
}

// kubeLabels say which Kubernetes cluster and node a node joined from; a node
// added any other way carries none. The cluster label is left off when the
// clusters were not loaded or the cluster is gone.
func kubeLabels(node *store.Node, clusters KubeClusters) []SystemLabel {
	if node.KubeClusterID == 0 {
		return nil
	}
	var labels []SystemLabel
	if cluster, ok := clusters[node.KubeClusterID]; ok && cluster.UUID != "" {
		display := cluster.Name
		if display == "" {
			display = cluster.UUID
		}
		labels = append(labels, SystemLabel{Key: nodelabel.KeyKubernetesCluster, Value: cluster.UUID, Display: display})
	}
	if node.KubeNodeName != "" {
		labels = append(labels, SystemLabel{Key: nodelabel.KeyKubernetesNode, Value: node.KubeNodeName, Display: node.KubeNodeName})
	}
	return labels
}

// LoadStorageScope reads which storages apply to each of nodes.
func LoadStorageScope(st *store.Store, nodes []*store.Node) (StorageScope, error) {
	byNode, err := st.StorageScopes(nodes)
	if err != nil {
		return nil, err
	}
	return StorageScope(byNode), nil
}

// RequirementsNeedProviders reports whether matching reqs reads where rented
// nodes run.
func RequirementsNeedProviders(reqs nodelabel.Requirements) bool {
	return len(reqs[nodelabel.KeyProvider]) > 0 || len(reqs[nodelabel.KeyRegion]) > 0
}

// RequirementsNeedStorages reports whether matching reqs reads which
// storages apply to a node.
func RequirementsNeedStorages(reqs nodelabel.Requirements) bool {
	return len(reqs[nodelabel.KeyStorage]) > 0
}

// storageLabels names every storage that applies to the node: whether it is
// mounted right now is not a label but a reason admission gives.
func storageLabels(node *store.Node, scope StorageScope) []SystemLabel {
	if scope == nil {
		return nil
	}
	var labels []SystemLabel
	for _, storage := range scope[node.ID] {
		labels = append(labels, SystemLabel{
			Key: nodelabel.KeyStorage, Value: strconv.FormatUint(uint64(storage.ID), 10), Display: storage.Name,
		})
	}
	return labels
}
