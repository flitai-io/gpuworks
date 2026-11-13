package api

import (
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A multi-node group runs over one network: the compute network every rank
// is on, else the management network when every rank lets it carry compute
// traffic (network.Service.ComputeGroup).

// fillResolvedNodeFields sets what a node's resolved networks tell the API
// about it: the compute network the deploy form uses to tell which nodes can
// join a multi-node group, and the generated system labels, whose network
// keys name the network carrying each role.
//
// A resolution that cannot be loaded still leaves every node its system
// labels, without the network (or storage) ones - the hardware facts are known either
// way, and dropping them all would make a node look unlabelled.
//
// sources are where the nodes came from when the caller has read them
// already (the whole fleet's, for the list); nil reads those of nodes.
func (a *apiHandlers) fillResolvedNodeFields(nodes []store.Node, out []dto.Node, sources map[uint]store.NodeSource) {
	resolved, err := a.networks().Resolve(nodes)
	if err != nil {
		resolved = nil
	}
	storages, err := planning.LoadStorageScope(a.d.Store, nodePointers(nodes))
	if err != nil {
		storages = nil
	}
	ids := make([]uint, len(nodes))
	for i := range nodes {
		ids[i] = nodes[i].ID
	}
	providers, err := a.d.Store.NodeProviders(ids)
	if err != nil {
		providers = nil
	}
	facts := planning.NodeFacts{Networks: resolved, Storages: storages, Providers: providers, KubeClusters: planning.LoadKubeClusters(a.d.Store, nodes)}
	// An unreadable source is said to be so, not shown as a node added by hand.
	sourceUnknown := false
	if sources == nil {
		if sources, err = a.d.Store.NodeSources(nodes); err != nil {
			sourceUnknown = true
		}
	}
	for i := range nodes {
		out[i].SourceUnknown = sourceUnknown
		if source, ok := sources[nodes[i].ID]; ok {
			out[i].Source = nodeSourceDTO(source)
		}
		if r, ok := resolved.For(nodes[i].ID, netplan.RoleCompute); ok {
			out[i].ComputeNetwork, out[i].ComputeAddress = r.Network.Name, r.IP
		}
		out[i].SystemLabels = systemLabelsDTO(planning.SystemLabels(&nodes[i], facts))
	}
}

// systemLabelsDTO is the wire form of the generated labels. It is a straight
// rename of planning's fields; the API adds nothing, which is what keeps the
// label a node shows and the label a selector matches the same thing.
func systemLabelsDTO(labels []planning.SystemLabel) []dto.SystemLabel {
	out := make([]dto.SystemLabel, 0, len(labels))
	for _, label := range labels {
		out = append(out, dto.SystemLabel{
			Key: label.Key, Value: label.Value,
			Display: label.Display, DisplayCode: label.DisplayCode,
		})
	}
	return out
}

// nodeSourceDTO names the external platform a node joined through and what
// the node is there.
func nodeSourceDTO(source store.NodeSource) *dto.NodeSource {
	out := &dto.NodeSource{ProviderID: source.ProviderID, ProviderName: source.ProviderName, ProviderKind: source.ProviderKind,
		ClusterUUID: source.ClusterUUID, ClusterName: source.ClusterName}
	if source.ClusterUUID != "" {
		out.Resource = string(provider.ResourceKubernetes)
	} else {
		out.Resource = string(provider.InstanceResource(source.ProviderKind, source.InstanceKind))
	}
	return out
}
