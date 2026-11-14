package deployment

import (
	"context"
	"encoding/json"
	"log/slog"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Deps is what the deployment use cases read and call. Every function may be
// nil: the use case then reads what the store recorded instead.
type Deps struct {
	Store *store.Store
	Log   *slog.Logger
	// Online reports whether a node's agent is connected.
	Online func(uuid string) bool
	// UCXNetlinkHang is the live fact a node's agent last reported about its
	// kernel hanging UCX (NIXL): nil when it does not, was not reported, or
	// the node is offline. Nil means no node is known to.
	UCXNetlinkHang func(uuid string) *proto.UCXNetlinkHang
	// NodeSync pushes a node its desired state again.
	NodeSync interface{ SyncNode(uuid string) error }
	// CallNode is a short RPC to a node's agent.
	CallNode func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error)
	// ImageExists reports whether a node already holds an image.
	ImageExists     func(ctx context.Context, ref, nodeUUID string) (bool, error)
	CatalogPackages *packages.Store
	// FetchModelMeta reads a model's metadata from the first of candidates
	// that serves it (modelmeta.FetchFrom), the app's reader. Only when it is
	// set does a model diagnostic probe the source from the management node.
	FetchModelMeta func(ctx context.Context, source, repo, revision, subfolder string, candidates []string) (*modelmeta.Meta, error)
	// ReadModelMeta is the reader Service.FetchModelMeta falls back on when
	// FetchModelMeta is nil (the server reading the source itself); nil reads
	// none.
	ReadModelMeta func(ctx context.Context, source, repo, revision, subfolder string, candidates []string) (*modelmeta.Meta, error)
	// Networks resolves node networks; nil reads every declared network from
	// Store.
	Networks *network.Service
	// AfterAutomaticPick, when set, runs right after the server picked a node
	// for a create and before the create re-checks it: tests use it to take
	// the picked cards in that window, the race a fleet of concurrent creates
	// hits.
	AfterAutomaticPick func(nodeUUID string)
}

// Service runs the deployment use cases over Deps. It keeps no state of its
// own, so building one per request costs nothing.
type Service struct{ d Deps }

// New is the deployment use cases over d.
func New(d Deps) *Service { return &Service{d: d} }

// networks is the network service Deps names, or one over the store.
func (s *Service) networks() *network.Service {
	if s.d.Networks == nil {
		return network.New(s.d.Store, network.StoreCatalog{Store: s.d.Store})
	}
	return s.d.Networks
}
