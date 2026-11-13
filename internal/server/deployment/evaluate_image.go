package deployment

import (
	"context"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/acquisition"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// evaluateImageEnv is the request state the image projection reads.
type evaluateImageEnv struct {
	ctx          context.Context
	request      *dto.DeployRequest
	response     *dto.EvaluateResponse
	engine       deploy.Engine
	selection    templateSelection
	targetVendor string
	profileNode  *store.Node
	node         *store.Node
	nodeFound    bool
	weightFacts  acquisition.WeightFacts
}

// evaluateImageResult is what the projection reads off the node and the image
// cache: the hardware identity the launch preview passes on and the selected
// image's cache fact. The same fact narrowed to the node the estimate lands on
// is decided by evaluatePlan, which is the step that knows the node.
type evaluateImageResult struct {
	arch, archID, vendor string
	imageCached          *bool
}

// evaluateImageFacts resolves the image this preview shows, the acquisition
// facts behind its options, and the hardware identity that comes from the same
// card inspection. It writes to the response it is handed and nothing else.
func (s *Service) evaluateImageFacts(env evaluateImageEnv) evaluateImageResult {
	req, resp, node := env.request, env.response, env.node
	eng, selection, targetVendor := env.engine, env.selection, env.targetVendor
	profileNode := env.profileNode
	out := evaluateImageResult{}
	if owned := node.OwnedGPUs(); env.nodeFound && len(owned) > 0 {
		g := owned[0]
		arch := node.ArchOf(g)
		out.arch = arch.Family
		out.archID = arch.ID
		out.vendor = node.VendorOf(g)
		resolution := s.rankImage(eng.Name(), selection, req.Image, node)
		resp.Image = resolution.Image
		resp.Warnings = append(resp.Warnings, resolution.Warnings...)
		if resolution.Blocking {
			resp.OK = false
		}
	} else if targetVendor != "" {
		target := deploymentImageTarget(eng.Name(), targetVendor, nil)
		target.Override = req.Image
		target.TrustedOverride = templateOwnsImage(selection, req.Image)
		resolution := imagecatalog.Resolve(s.d.Store, target)
		resp.Image = resolution.Image
		resp.Warnings = append(resp.Warnings, resolution.Warnings...)
		if resolution.Blocking {
			resp.OK = false
		}
	}

	cached, cacheReason := s.imageCacheFact(env.ctx, resp.Image, profileNode)
	out.imageCached = cached
	acq := acquisitionDTO(acquisition.Resolve(env.weightFacts, acquisition.ImageFacts{Canonical: resp.Image, Cached: out.imageCached}))
	acq.Image.CacheReason = cacheReason
	acq.Image.Options = s.deploymentImageOptions(deploymentImageTarget(eng.Name(), targetVendor, profileNode), selection, resp.Image, out.imageCached)
	resp.Acquisition = &acq
	return out
}

// imageCacheFact asks a node whether it holds the image. The reason says why
// there is no answer: no node, an offline node, no image, or a failed query.
func (s *Service) imageCacheFact(ctx context.Context, image string, node *store.Node) (*bool, string) {
	switch {
	case node == nil:
		return nil, "node_unselected"
	case s.d.Online != nil && !s.d.Online(node.UUID):
		return nil, "node_offline"
	case strings.TrimSpace(image) == "":
		return nil, "image_unavailable"
	case s.d.ImageExists != nil:
		if exists, err := s.d.ImageExists(ctx, image, node.UUID); err == nil {
			return &exists, ""
		}
	}
	return nil, "query_failed"
}

// evaluateWeightFacts is the weight side of the preview's acquisition panel:
// the public sources the model or its template is registered under, and the
// storages that hold it or may save it for the selected node. No additional
// state is persisted for these facts.
//
// A registration from a source absent in the spec has no template provider
// to choose. Offering its own source would make the form send a provider
// the template cannot accept on the next evaluation.
func (s *Service) evaluateWeightFacts(model *store.Model, selection templateSelection, profileNode *store.Node) (acquisition.WeightFacts, error) {
	weightFacts := acquisition.WeightFacts{}
	if model.Source != "local" {
		weightFacts.Providers = []acquisition.WeightSource{{
			Provider: model.Source, RepoID: model.SourceID, Revision: model.Revision, Suggested: true,
			DisplayName: modelSourceHost(model),
		}}
		if selection.Spec != nil {
			if specDeclaresProvider(selection.Spec, model.Source) {
				weightFacts.Providers = templateAcquisitionProviders(selection)
			} else {
				weightFacts.Providers = nil
			}
		}
	}
	var err error
	weightFacts.Present, weightFacts.Targets, err = s.storageFacts(model, profileNode)
	return weightFacts, err
}

// storageFacts lists the storages the profile node can read that hold the
// model, and the storages a copy or download may save into for that node.
func (s *Service) storageFacts(model *store.Model, node *store.Node) ([]acquisition.StoragePresence, []acquisition.StorageTarget, error) {
	if model == nil || node == nil {
		return nil, nil, nil
	}
	locations, err := s.d.Store.ModelLocations(model.ID, node.ID)
	if err != nil {
		check := modelDiagnostic(model, node, store.ModelSource{}, "facts", "unknown", "facts_unavailable", "Model location facts could not be read")
		return nil, nil, WithDiagnostics(wrapProductError(messages.ErrorModelSourceUnavailable, check.Message.Message, check.Message.Params, err), &dto.DiagnosticReport{Checks: []dto.DiagnosticCheck{check}})
	}
	var present []acquisition.StoragePresence
	var targets []acquisition.StorageTarget
	for _, loc := range locations {
		storage := loc.Storage
		// A storage that already holds the model for this node is offered as
		// "already there", never as a place to save another copy into.
		held := (storage.Type == proto.StorageLocal && loc.Ready) ||
			(storage.Type != proto.StorageLocal && loc.Shared.Ready())
		if !held && (storage.Type == proto.StorageLocal || modelHasPublicSource(model)) && s.d.Store.StorageAcceptsWrites(&storage) == nil {
			targets = append(targets, acquisition.StorageTarget{StorageID: storage.ID, StorageName: storage.Name, StorageType: storage.Type, Default: storage.IsDefault})
		}
		p := acquisition.StoragePresence{StorageID: storage.ID, StorageName: storage.Name, StorageType: storage.Type, SizeBytes: loc.SizeBytes}
		switch storage.Type {
		case proto.StorageLocal:
			if !loc.Ready {
				continue
			}
			p.OnNode = true
		case proto.StorageNFSCache:
			if !loc.Shared.Ready() {
				continue
			}
			p.OnNode = loc.Ready
			if !loc.Ready {
				if loc.Reason == store.LocationNotCached {
					p.CacheBytes = loc.SizeBytes
				} else {
					p.Unavailable = loc.Reason
				}
			}
		default:
			if !loc.Shared.Ready() {
				continue
			}
			if !loc.Ready {
				p.Unavailable = loc.Reason
			}
		}
		present = append(present, p)
	}
	return present, targets, nil
}
