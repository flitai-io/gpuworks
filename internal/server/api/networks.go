package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Networks are declared once and attached to nodes or node labels; what a node
// actually uses is always recomputed by the network service, never stored.
// This file only turns the service's state into the network pages' wire shape.

// networks is the network service; without one wired in, every declared
// network in the store takes effect.
func (a *apiHandlers) networks() *network.Service {
	if a.d.Networks == nil {
		return network.New(a.d.Store, network.StoreCatalog{Store: a.d.Store})
	}
	return a.d.Networks
}

type networkState struct {
	network.State
}

func (a *apiHandlers) loadNetworkState() (networkState, error) {
	state, err := a.networks().State()
	return networkState{State: state}, err
}

// latestNetworkTest reads a network's newest test run, nil when it has none.
// A run's results reach megabytes (a card matrix stores every card pair), so
// only the detail views that show them read them; the state every network
// call loads does not carry them.
func (a *apiHandlers) latestNetworkTest(networkID uint) *store.NetworkTestRun {
	run, err := a.d.Store.LatestNetworkTestRun(networkID)
	if err != nil {
		return nil
	}
	return run
}

func (st networkState) nodeByID() map[uint]store.Node {
	out := make(map[uint]store.Node, len(st.Nodes))
	for _, n := range st.Nodes {
		out[n.ID] = n
	}
	return out
}

// networkDTO takes the state's nodes by ID (nodeByID) from the caller, which
// builds them once for every network it lists.
func (st networkState) networkDTO(n store.Network, nodes map[uint]store.Node) dto.Network {
	out := dto.Network{
		ID: n.ID, Name: n.Name, Description: n.Description, Roles: nonNilStrings(network.Network(n).Roles),
		Type: n.Type, CIDR: n.CIDR, Interface: n.Interface,
		Attachments: []dto.NetworkAttachment{}, CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt,
	}
	for _, att := range st.Attachments {
		if att.NetworkID == n.ID {
			out.Attachments = append(out.Attachments, attachmentDTO(att, nodes))
		}
	}
	resolvedNodes, warned := map[uint]bool{}, map[uint]bool{}
	for _, r := range st.Resolved {
		if r.Network.ID != n.ID {
			continue
		}
		resolvedNodes[r.NodeID] = true
		if len(r.Warnings) > 0 {
			warned[r.NodeID] = true
		}
	}
	out.ResolvedNodeCount, out.WarningNodeCount = len(resolvedNodes), len(warned)
	return out
}

func attachmentDTO(att store.NetworkAttachment, nodes map[uint]store.Node) dto.NetworkAttachment {
	out := dto.NetworkAttachment{
		ID: att.ID, Target: netplan.SourceLabel, LabelKey: att.LabelKey, LabelValue: att.LabelValue,
		Interface: att.Interface, IP: att.IP, HCAs: nonNilStrings(network.Attachment(att).HCAs), CreatedAt: att.CreatedAt,
	}
	if att.NodeID != 0 {
		out.Target = netplan.SourceNode
		if node, ok := nodes[att.NodeID]; ok {
			out.NodeUUID, out.NodeName = node.UUID, nodeDisplayName(&node)
		}
	}
	return out
}

func warningDTOs(ws []netplan.Warning) []dto.NetworkWarning {
	out := make([]dto.NetworkWarning, 0, len(ws))
	for _, w := range ws {
		out = append(out, dto.NetworkWarning{Code: w.Code, Params: w.Params})
	}
	return out
}

func (a *apiHandlers) listNetworks(w http.ResponseWriter, r *http.Request) {
	st, err := a.loadNetworkState()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := dto.NetworkList{Items: []dto.Network{st.managementNetworkDTO()}, Roles: netplan.Roles, Types: netplan.Types, TypeRoles: typeRoles()}
	nodes := st.nodeByID()
	for _, n := range st.Networks {
		out.Items = append(out.Items, st.networkDTO(n, nodes))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *apiHandlers) getNetwork(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	st, err := a.loadNetworkState()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if isManagementNetworkID(chi.URLParam(r, "id")) {
		writeJSON(w, http.StatusOK, st.managementDetail(a.latestNetworkTest(0)))
		return
	}
	for _, n := range st.Networks {
		if n.ID == id {
			writeJSON(w, http.StatusOK, st.networkDetail(n, a.latestNetworkTest(n.ID)))
			return
		}
	}
	writeProductError(w, http.StatusNotFound, ErrorNetworkNotFound, "network not found", nil)
}

// networkDetail is one network with the nodes it resolved to and its latest
// test run, nil when it has none.
func (st networkState) networkDetail(n store.Network, latest *store.NetworkTestRun) dto.NetworkDetail {
	nodes := st.nodeByID()
	out := dto.NetworkDetail{Network: st.networkDTO(n, nodes), Nodes: []dto.NetworkNode{}, Roles: netplan.Roles, Types: netplan.Types, TypeRoles: typeRoles(), TestSupported: networkTestable(n)}
	out.LatestTest = networkTestDTO(latest, nodes)
	index := map[uint]int{}
	for _, r := range st.Resolved {
		if r.Network.ID != n.ID {
			continue
		}
		i, ok := index[r.NodeID]
		if !ok {
			node := nodes[r.NodeID]
			i = len(out.Nodes)
			index[r.NodeID] = i
			hcaSource, bootstrapSource := endpointSources(r)
			out.Nodes = append(out.Nodes, dto.NetworkNode{
				NodeUUID: node.UUID, NodeName: nodeDisplayName(&node), Labels: writtenLabels(&node), Roles: []string{},
				AttachmentID: r.AttachmentID, Source: r.Source, LabelKey: r.LabelKey, LabelValue: r.LabelValue,
				Interface: r.Interface, IP: r.IP, HCAs: railDTOs(network.Inventory(node), r.Network.Type, r.HCAs), LinkLayer: r.LinkLayer,
				HCASource: hcaSource, BootstrapSource: bootstrapSource, Tools: nodeTools(node), Warnings: []dto.NetworkWarning{},
			})
		}
		out.Nodes[i].Roles = append(out.Nodes[i].Roles, r.Role)
		out.Nodes[i].Warnings = append(out.Nodes[i].Warnings, warningDTOs(r.Warnings)...)
	}
	return out
}

func (a *apiHandlers) createNetwork(w http.ResponseWriter, r *http.Request) {
	spec, ok := decodeNetworkSpec(w, r)
	if !ok {
		return
	}
	created, err := a.networks().CreateNetwork(spec)
	if err != nil {
		writeNetworkErr(w, err)
		return
	}
	a.writeNetworkDetail(w, http.StatusCreated, created.ID)
}

func (a *apiHandlers) updateNetwork(w http.ResponseWriter, r *http.Request) {
	spec, ok := decodeNetworkSpec(w, r)
	if !ok {
		return
	}
	id := parseUint(chi.URLParam(r, "id"))
	if _, err := a.networks().UpdateNetwork(id, spec); err != nil {
		writeNetworkErr(w, err)
		return
	}
	a.writeNetworkDetail(w, http.StatusOK, id)
}

func (a *apiHandlers) deleteNetwork(w http.ResponseWriter, r *http.Request) {
	if err := a.networks().DeleteNetwork(parseUint(chi.URLParam(r, "id"))); err != nil {
		writeNetworkErr(w, err)
		return
	}
	a.syncNodesIfOnline()
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// syncNodesIfOnline asks for desired state, which carries the resolved
// storage-sync selector, to be re-sent to the given online nodes, or to every
// online node when none is named. A network change can move any node's
// resolution.
func (a *apiHandlers) syncNodesIfOnline(uuids ...string) {
	if len(uuids) == 0 {
		a.requestSyncAllNodes()
		return
	}
	a.requestSyncNodes(uuids)
}

// writeNetworkDetail answers a successful network mutation: it resyncs the
// online nodes and returns the network as it now resolves.
func (a *apiHandlers) writeNetworkDetail(w http.ResponseWriter, status int, id uint) {
	a.syncNodesIfOnline()
	st, err := a.loadNetworkState()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	for _, n := range st.Networks {
		if n.ID == id {
			writeJSON(w, status, st.networkDetail(n, a.latestNetworkTest(n.ID)))
			return
		}
	}
	writeProductError(w, http.StatusNotFound, ErrorNetworkNotFound, "network not found", nil)
}

// writeNetworkErr answers a failed network change.
func writeNetworkErr(w http.ResponseWriter, err error) {
	var invalid *network.InvalidError
	switch {
	case errors.As(err, &invalid):
		writeNetworkInvalid(w, invalid)
	case errors.Is(err, network.ErrDedicatedUnavailable):
		writeProductError(w, http.StatusForbidden, ErrorNetworkDedicatedUnavailable, "dedicated networks are not enabled", nil)
	case errors.Is(err, network.ErrAttachmentNotFound):
		writeProductError(w, http.StatusNotFound, ErrorNetworkAttachmentNotFound, "network attachment not found", nil)
	case errors.Is(err, store.ErrNotFound):
		writeProductError(w, http.StatusNotFound, ErrorNetworkNotFound, "network not found", nil)
	case errors.Is(err, store.ErrNetworkNameTaken):
		writeProductError(w, http.StatusConflict, ErrorNetworkNameTaken, "network name already exists", nil)
	case errors.Is(err, store.ErrNetworkAttachmentExists):
		writeProductError(w, http.StatusConflict, ErrorNetworkAttachmentExists, "network is already attached to this target", nil)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

// typeRoles is the dictionary of roles each network type may carry.
func typeRoles() map[string][]string {
	out := map[string][]string{}
	for _, t := range netplan.Types {
		out[t] = netplan.TypeRoles(t)
	}
	return out
}

func writeNetworkInvalid(w http.ResponseWriter, e *network.InvalidError) {
	switch e.Kind {
	case network.InvalidName:
		writeProductError(w, http.StatusBadRequest, ErrorNetworkNameRequired, "network name is required (at most 64 characters)", nil)
	case network.InvalidRole:
		writeProductError(w, http.StatusBadRequest, ErrorNetworkRoleInvalid, "network roles must be one or more of storage_sync, compute, serving", map[string]any{"role": e.Value})
	case network.InvalidType:
		writeProductError(w, http.StatusBadRequest, ErrorNetworkTypeInvalid, "network type must be host_ethernet, host_rdma, accelerator_rdma or accelerator_hccs", map[string]any{"type": e.Value})
	case network.InvalidTypeRole:
		writeProductError(w, http.StatusBadRequest, ErrorNetworkTypeRoleInvalid, "a network of type "+e.Field+" cannot carry role "+e.Value, map[string]any{"type": e.Field, "role": e.Value})
	case network.InvalidSelector:
		writeProductError(w, http.StatusBadRequest, ErrorNetworkSelectorInvalid, "invalid network selector "+e.Field, map[string]any{"field": e.Field, "value": e.Value})
	case network.InvalidOverride:
		writeProductError(w, http.StatusBadRequest, ErrorNetworkOverrideInvalid, "invalid network override "+e.Field, map[string]any{"field": e.Field, "value": e.Value})
	default:
		writeProductError(w, http.StatusBadRequest, ErrorNetworkAttachmentTargetInvalid, e.Error(), map[string]any{"node": e.Value})
	}
}

// decodeNetworkSpec reads a network body, writing the error response itself
// when it returns false. The network service validates the fields.
func decodeNetworkSpec(w http.ResponseWriter, r *http.Request) (network.NetworkSpec, bool) {
	var req dto.NetworkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return network.NetworkSpec{}, false
	}
	if !validateName(w, req.Name) {
		return network.NetworkSpec{}, false
	}
	return network.NetworkSpec{
		Name: req.Name, Description: req.Description, Roles: req.Roles, Type: req.Type,
		CIDR: req.CIDR, Interface: req.Interface,
	}, true
}

// nodeTools lists the network test tools a node's agent reported.
func nodeTools(node store.Node) []string {
	if inv := network.Inventory(node); inv != nil {
		return nonNilStrings(inv.Tools)
	}
	return []string{}
}

// managementIP is the address a node's agent reached the server from, "" until
// it reports one.
func managementIP(node store.Node) string {
	addr, _ := network.ManagementAddress(node)
	return addr.IP
}
