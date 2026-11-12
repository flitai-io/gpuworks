package deployment

import (
	"context"
	"errors"
	"net/url"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Forecast plans, reported by an evaluation for each node a request names.
const (
	forecastPresent     = "present"
	forecastCopy        = "copy"
	forecastPull        = "pull"
	forecastStorage     = "storage"
	forecastDownload    = "download"
	forecastUnavailable = "unavailable"
)

// pinnedNodeUUIDs are the nodes a request names: the ranks of a multi-node
// draft, or the one node of a manual placement. An automatic placement names
// none, and gets no forecast: where it lands is decided at launch.
func pinnedNodeUUIDs(req *dto.DeployRequest) []string {
	if req.NNodes > 1 {
		return req.NodeUUIDs
	}
	if req.NodeUUID != "" {
		return []string{req.NodeUUID}
	}
	return nil
}

// evaluateNodeAcquisition forecasts, for each named node, how it would get
// the image and the weights if it started now: from itself, by copy from a
// node of its sync group, or by pulling/downloading through the pull
// settings. It is the same decision the controller makes at launch
// (controller.deploymentImageDelivery, controller.syncModelWithin), read
// without side effects.
//
// known holds image presence the caller already asked a node about, so the
// same evaluation does not ask the node twice.
func (s *Service) evaluateNodeAcquisition(ctx context.Context, uuids []string, image string, templateSources []string, model *store.Model, rules store.SourceRules, known map[string]*bool) []dto.AcquisitionNode {
	out := make([]dto.AcquisitionNode, 0, len(uuids))
	copies := s.d.Store.ImageCopies(image, templateSources)
	resolve := s.networks().Resolve
	for _, uuid := range uuids {
		node, err := s.d.Store.NodeByUUID(uuid)
		if err != nil {
			continue
		}
		fact := dto.AcquisitionNode{NodeUUID: node.UUID, NodeName: nodeDisplayName(node)}
		if cached, ok := known[node.UUID]; ok && cached != nil {
			fact.ImageCached = cached
		} else {
			fact.ImageCached, fact.ImageCacheReason = s.imageCacheFact(ctx, image, node)
		}
		s.forecastImage(&fact, node, image, copies, resolve)
		if model != nil {
			s.forecastWeights(&fact, node, model, rules, resolve)
		}
		out = append(out, fact)
	}
	return out
}

func (s *Service) forecastImage(fact *dto.AcquisitionNode, node *store.Node, image string, copies []string, resolve planning.NodeResolver) {
	if fact.ImageCached != nil && *fact.ImageCached {
		fact.ImagePlan = forecastPresent
		return
	}
	if row, err := s.d.Store.ResolveImageRef(image); err == nil {
		if holders := s.imageHolders(row.ID, node); len(holders) > 0 {
			if peers, err := planning.SyncPeersFor(s.d.Store, resolve, node, holders); err == nil && len(peers) > 0 {
				fact.ImagePlan = forecastCopy
				for i := range holders {
					if holders[i].ID == peers[0].NodeID {
						fact.ImageCopyFrom = nodeDisplayName(&holders[i])
					}
				}
				return
			}
		}
	}
	fact.ImagePlan = forecastPull
	candidates, err := planning.PullCandidatesFor(s.d.Store, resolve, node, image, copies)
	if err != nil {
		fact.ImagePullError = ProductMessage(err)
		return
	}
	for _, candidate := range candidates {
		fact.ImagePullRefs = append(fact.ImagePullRefs, candidate.Ref)
		fact.ImagePullRanked = fact.ImagePullRanked || candidate.Rank
		// Rule endpoints lead unranked (pullref.PlanCandidates): the node
		// tries them as listed before the copies it orders itself.
		if !candidate.Rank && len(fact.ImagePullRefs) == fact.ImagePullFixed+1 {
			fact.ImagePullFixed++
		}
	}
}

// imageHolders are the online nodes other than target that hold the image.
func (s *Service) imageHolders(imageID uint, target *store.Node) []store.Node {
	ids, err := s.d.Store.ImageHolders(imageID, target.ID)
	if err != nil {
		return nil
	}
	return s.onlineNodes(ids)
}

// onlineNodes reads the named nodes in one query and keeps the online ones.
func (s *Service) onlineNodes(ids []uint) []store.Node {
	nodes, err := s.d.Store.NodesForLabelsByID(ids, true)
	if err != nil {
		return nil
	}
	out := nodes[:0]
	for _, node := range nodes {
		if s.d.Online == nil || s.d.Online(node.UUID) {
			out = append(out, node)
		}
	}
	return out
}

func (s *Service) forecastWeights(fact *dto.AcquisitionNode, node *store.Node, model *store.Model, rules store.SourceRules, resolve planning.NodeResolver) {
	src, err := s.d.Store.ModelSourceOn(model.ID, node, rules)
	if err != nil {
		fact.WeightsPlan = forecastUnavailable
		check := modelDiagnostic(model, node, src, "facts", "unknown", "facts_unavailable", "Model location facts could not be read")
		fact.WeightsFetchError = &check.Message
		return
	}
	switch src.Action {
	case store.SourceReady:
		fact.WeightsPresent = true
		fact.WeightsPlan = forecastPresent
		return
	case store.SourceFillCache, store.SourceWaitShared:
		fact.WeightsPlan = forecastStorage
		fact.WeightsStorage = src.Location.Storage.Name
		return
	case store.SourceRefused:
		if src.Reason != store.LocationModelMissingInStore || !modelHasPublicSource(model) {
			fact.WeightsPlan = forecastUnavailable
			fact.WeightsFetchError = &dto.ProductMessage{Code: messages.ErrorModelSourceUnavailable,
				Params: modelDiagnosticParams(model, node, src, src.Reason), Message: "The requested model storage is not usable on this node"}
			return
		}
	}
	if src.Action == store.SourceAcquire {
		peer, peerErr := s.modelCopyPeer(model.ID, node)
		if errors.Is(peerErr, netplan.ErrNoStoragePath) {
			if !modelHasPublicSource(model) {
				fact.WeightsPlan = forecastUnavailable
				fact.WeightsFetchError = ProductMessage(peerErr)
				return
			}
		} else if peerErr != nil {
			fact.WeightsPlan = forecastUnavailable
			fact.WeightsFetchError = &dto.ProductMessage{Code: messages.ErrorModelSourceUnavailable,
				Params: modelDiagnosticParams(model, node, src, "facts_unavailable"), Message: "Model peer facts could not be read"}
			return
		}
		if peer != "" {
			fact.WeightsPlan, fact.WeightsCopyFrom = forecastCopy, peer
			return
		}
	}
	if !modelHasPublicSource(model) {
		fact.WeightsPlan = forecastUnavailable
		fact.WeightsFetchError = &dto.ProductMessage{Code: messages.ErrorModelSourceUnavailable,
			Params: modelDiagnosticParams(model, node, src, "no_acquisition_path"), Message: "This local model has no qualified peer or public download source"}
		return
	}
	fact.WeightsPlan = forecastDownload
	fact.WeightsProvider = model.Source
	candidates, err := planning.ModelWeightsFetch(s.d.Store, resolve, node, model)
	if err != nil {
		fact.WeightsFetchError = ProductMessage(err)
		return
	}
	for _, candidate := range candidates {
		if parsed, err := url.Parse(candidate.Base); err == nil && parsed.Host != "" {
			fact.WeightsFetchHosts = append(fact.WeightsFetchHosts, parsed.Host)
		}
	}
}

// ProductMessage carries an error inside an answer: a ProductError keeps its
// code and params, anything else becomes FIELD_INVALID with the detail.
func ProductMessage(err error) *dto.ProductMessage {
	var productErr *messages.ProductError
	if errors.As(err, &productErr) {
		return &dto.ProductMessage{Code: productErr.Code(), Params: productErr.Params(), Message: productErr.Error()}
	}
	return &dto.ProductMessage{Code: messages.ErrorFieldInvalid, Params: map[string]any{"detail": err.Error()}, Message: err.Error()}
}
