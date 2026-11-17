// Package network is the one place the server asks which network a node uses
// for a role. It loads nodes and the network catalog from the store, converts
// them to netplan's model (convert.go is the only conversion), and answers the
// questions core flows have: the network a multi-node group shares, the path a
// copy between two nodes takes, whether a node may serve, where its serving
// forwarder listens. It also validates and applies changes to networks,
// attachments and management-network roles.
//
// The catalog of declared networks is read through Catalog. StoreCatalog is
// every network and attachment in the store; ManagementOnly declares none, so
// every role falls to the management network and the agent tunnel. Choosing
// between them is the whole seam for running without dedicated networks.
package network

import (
	"errors"

	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Catalog supplies the declared networks and their attachments.
type Catalog interface {
	// Load returns the networks and attachments in creation order.
	Load() ([]store.Network, []store.NetworkAttachment, error)
	// Dedicated reports whether declared networks exist at all: false means
	// the management network carries everything and the catalog cannot be
	// changed.
	Dedicated() bool
}

// StoreCatalog is every network and attachment in the store.
type StoreCatalog struct{ Store *store.Store }

func (c StoreCatalog) Load() ([]store.Network, []store.NetworkAttachment, error) {
	networks, err := c.Store.ListNetworks()
	if err != nil {
		return nil, nil, err
	}
	attachments, err := c.Store.ListNetworkAttachments()
	if err != nil {
		return nil, nil, err
	}
	return networks, attachments, nil
}

func (StoreCatalog) Dedicated() bool { return true }

// ManagementOnly declares no network: every node uses its management network
// for every role it allows there, and serving rides the agent tunnel.
type ManagementOnly struct{}

func (ManagementOnly) Load() ([]store.Network, []store.NetworkAttachment, error) {
	return nil, nil, nil
}

func (ManagementOnly) Dedicated() bool { return false }

// ErrDedicatedUnavailable rejects a change to declared networks when the
// catalog has none.
var ErrDedicatedUnavailable = errors.New("dedicated networks are not available")

// Service resolves networks for core flows and applies network changes.
type Service struct {
	store   *store.Store
	catalog Catalog
}

// New returns a service reading nodes from st and networks from catalog.
func New(st *store.Store, catalog Catalog) *Service {
	return &Service{store: st, catalog: catalog}
}

// Dedicated reports whether declared networks take effect.
func (s *Service) Dedicated() bool { return s.catalog.Dedicated() }

// State is everything the network pages show: the rows as stored and what
// every node resolves from them.
type State struct {
	Nodes       []store.Node
	Networks    []store.Network
	Attachments []store.NetworkAttachment
	Resolved    netplan.Resolution
}

// State loads every node and network and resolves them.
func (s *Service) State() (State, error) {
	nodes, err := s.store.FleetNodes()
	if err != nil {
		return State{}, err
	}
	networks, attachments, err := s.catalog.Load()
	if err != nil {
		return State{}, err
	}
	return State{
		Nodes: nodes, Networks: networks, Attachments: attachments,
		Resolved: netplan.Resolve(inputs(nodes, networks, attachments)),
	}, nil
}

// Resolve resolves the given nodes against the catalog.
func (s *Service) Resolve(nodes []store.Node) (netplan.Resolution, error) {
	networks, attachments, err := s.catalog.Load()
	if err != nil {
		return nil, err
	}
	return netplan.Resolve(inputs(nodes, networks, attachments)), nil
}

// NodeNetworks resolves one node.
func (s *Service) NodeNetworks(nodeID uint) (netplan.Resolution, error) {
	nodes, err := s.store.NodesForNetworks(nodeID)
	if err != nil {
		return nil, err
	}
	return s.Resolve(nodes)
}

// ComputeGroup picks the network a multi-node group shares; the first node
// is rank 0. A *netplan.GroupError says why there is none.
func (s *Service) ComputeGroup(nodes []store.Node) (netplan.Group, error) {
	resolved, err := s.Resolve(nodes)
	if err != nil {
		return netplan.Group{}, err
	}
	return netplan.ComputeGroup(Nodes(nodes), resolved)
}

// StoragePath picks the network a direct copy from source to target uses;
// netplan.ErrNoStoragePath when the two share none.
func (s *Service) StoragePath(sourceID, targetID uint) (netplan.StoragePath, error) {
	nodes, err := s.store.NodesForNetworks(sourceID, targetID)
	if err != nil {
		return netplan.StoragePath{}, err
	}
	byID := map[uint]store.Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	resolved, err := s.Resolve(nodes)
	if err != nil {
		return netplan.StoragePath{}, err
	}
	return netplan.StoragePathBetween(Node(byID[sourceID]), Node(byID[targetID]), resolved)
}

// CanServe reports whether a node may host an inference service. The
// scheduler asks for every node it considers, so a node whose management
// network may serve is answered without reading the catalog. A catalog that
// cannot be read refuses a node that relies on a serving network of its own.
func (s *Service) CanServe(node store.Node) bool {
	n := Node(node)
	if !n.RolesOff[netplan.RoleServing] {
		return true
	}
	resolved, err := s.Resolve([]store.Node{node})
	if err != nil {
		return false
	}
	return netplan.CanServe(n, resolved)
}

// ServingIP is where a node runs its serving forwarder, "" when it serves
// over the agent tunnel.
func (s *Service) ServingIP(nodeID uint) string {
	resolved, err := s.NodeNetworks(nodeID)
	if err != nil {
		return ""
	}
	return netplan.ServingIP(nodeID, resolved)
}

// ServingEndpoint is where the gateway can reach a node's serving forwarder,
// with the node token its instance tokens and TLS key derive from.
type ServingEndpoint struct {
	IP        string
	NodeToken string
}

// ServingEndpoints lists, by node UUID, every node whose serving network
// resolved to an IP. Nodes without a stored plaintext token (a placeholder
// counts as none) are skipped: the gateway could neither authenticate nor pin
// them.
func (s *Service) ServingEndpoints() (map[string]ServingEndpoint, error) {
	nodes, err := s.store.NodesForNetworks()
	if err != nil {
		return nil, err
	}
	resolved, err := s.Resolve(nodes)
	if err != nil {
		return nil, err
	}
	ips := netplan.ServingIPs(resolved)
	out := map[string]ServingEndpoint{}
	for _, node := range nodes {
		token, ok := node.PlaintextToken()
		if ip := ips[node.ID]; ip != "" && ok {
			out[node.UUID] = ServingEndpoint{IP: ip, NodeToken: token}
		}
	}
	return out, nil
}

// Endpoints is what one network resolves to on each of the given nodes, for
// internal tests of that network. ID 0 is the management network: every node's
// reported management address, whichever roles dedicated networks take from
// it there.
func (s *Service) Endpoints(networkID uint, nodes []store.Node) (netplan.Resolution, error) {
	var out netplan.Resolution
	if networkID == 0 {
		for _, n := range nodes {
			if r, ok := netplan.ManagementEndpoint(Node(n)); ok {
				out = append(out, r)
			}
		}
		return out, nil
	}
	resolved, err := s.Resolve(nodes)
	if err != nil {
		return nil, err
	}
	for _, r := range resolved {
		if r.Network.ID == networkID {
			out = append(out, r)
		}
	}
	return out, nil
}
