package deployment

import (
	"context"
	"errors"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// previewSaveToStorageID is the storage an evaluation plans a node against
// saving into: the one the request names, else 0 (the default order of
// store.ModelSourceOn). It is exactly what a create stores. An explicit choice
// is passed through even when it cannot serve the node, so planning says why.
func (s *Service) previewSaveToStorageID(source *dto.ModelSource, _ *store.Model, _ *store.Node) uint {
	if source == nil {
		return 0
	}
	return source.StorageID
}

// resolveSaveToStorage validates the storage a create names for the model's
// files and returns it. A request that names none returns nil: each node then
// reads whichever copy it has and saves a fetched one by the default order
// (store.ModelSourceOn). A named storage is a preference for the nodes in its
// scope; a node outside it falls back to its local storage, as a launch does
// (ADR 0034). It must still exist, serve the nodes it applies to, and accept
// new files when one of them would have to save the model into it. The answer
// is validated, never substituted (ADR 0018).
func (s *Service) resolveSaveToStorage(source *dto.ModelSource, model *store.Model, targets []*store.Node) (*store.Storage, error) {
	requested := uint(0)
	if source != nil {
		requested = source.StorageID
	}
	if requested == 0 {
		return nil, nil
	}
	storage, err := s.d.Store.StorageByID(requested)
	if err != nil {
		return nil, productError(messages.ErrorAcquisitionCandidateStale, "the selected storage no longer exists", map[string]any{"storage_id": requested})
	}
	nodes := uniqueNodes(targets)
	if len(nodes) == 0 || model == nil {
		return storage, nil
	}
	if err := s.checkStorageServes(storage, model, nodes, false); err != nil {
		return nil, err
	}
	var inScope []*store.Node
	for _, node := range nodes {
		if _, applies, err := s.d.Store.EffectiveStorageForNode(storage, node); err != nil {
			return nil, err
		} else if applies {
			inScope = append(inScope, node)
		}
	}
	if len(inScope) == 0 || s.storageHoldsModel(storage, model, inScope) {
		return storage, nil
	}
	if err := s.d.Store.StorageAcceptsWrites(storage); err != nil {
		code := messages.ErrorStorageDisabled
		if errors.Is(err, store.ErrStorageNotWritable) {
			code = messages.ErrorStorageNotWritable
		}
		report, _ := s.inspectDeploymentModel(context.Background(), model, store.SourceRules{SaveTo: storage.ID}, nodes, false)
		reason := store.LocationStorageDisabled
		if code == messages.ErrorStorageNotWritable {
			reason = "storage_not_writable"
		}
		check := modelDiagnostic(model, nil, store.ModelSource{Location: store.ModelLocation{Storage: *storage}}, "write", "failed", reason, "The requested storage does not accept new model files")
		check.Message.Code = code
		report.Checks = append(report.Checks, check)
		return nil, WithDiagnostics(productError(code, "storage "+storage.Name+" does not accept new model files", map[string]any{"storage": storage.Name, "storage_id": storage.ID}), report)
	}
	return storage, nil
}

// storageHoldsModel reports whether every target node can already read the
// model from storage.
func (s *Service) storageHoldsModel(storage *store.Storage, model *store.Model, nodes []*store.Node) bool {
	for _, node := range nodes {
		loc, err := s.d.Store.ModelLocation(model.ID, storage.ID, node.ID)
		if err != nil || loc.Reason != "" {
			return false
		}
	}
	return true
}

// checkStorageServes validates the source on each target. A save-to preference
// only applies to nodes in its storage's scope; other nodes follow
// ModelSourceOn's local fallback. present requires the model there: on the node
// for a local storage, in the storage for a shared one (an nfs_cache storage
// copies it into the node cache at launch).
func (s *Service) checkStorageServes(storage *store.Storage, model *store.Model, nodes []*store.Node, present bool) error {
	var firstErr error
	report := &dto.DiagnosticReport{}
	for _, node := range uniqueNodes(nodes) {
		loc, err := s.d.Store.ModelLocation(model.ID, storage.ID, node.ID)
		if err != nil {
			check := modelDiagnostic(model, node, store.ModelSource{Location: loc}, "facts", "unknown", "facts_unavailable", "Model location facts could not be read")
			report.Checks = append(report.Checks, check)
			if firstErr == nil {
				firstErr = wrapProductError(messages.ErrorModelSourceUnavailable, check.Message.Message, check.Message.Params, err)
			}
			continue
		}
		src := store.ModelSource{Action: store.SourceRefused, Location: loc, Reason: loc.Reason}
		if loc.Ready {
			src.Action = store.SourceReady
		}
		appendModelLocationChecks(report, model, node, src)
		switch loc.Reason {
		case "":
			continue
		case store.LocationStorageNotOnNode:
			if !present {
				continue
			}
			fallthrough
		case store.LocationStorageUnavailable, store.LocationStorageNotFound:
			if firstErr == nil {
				firstErr = productError(messages.ErrorStorageUnavailableOnNode, "storage "+storage.Name+" is not usable on node "+nodeDisplayName(node), map[string]any{"storage": storage.Name, "storage_id": storage.ID, "node": node.UUID, "node_id": node.ID, "reason": loc.Reason})
			}
		case store.LocationNotCached:
			continue
		default:
			if present && firstErr == nil {
				firstErr = productError(messages.ErrorAcquisitionCandidateStale, "the model is no longer complete in the selected storage; choose another source", map[string]any{"node": node.UUID, "node_id": node.ID, "model_id": model.ID, "storage": storage.Name, "storage_id": storage.ID, "reason": loc.Reason})
			}
		}
	}
	if firstErr != nil {
		return WithDiagnostics(firstErr, report)
	}
	return nil
}

// startSharedModelDownload starts the one download a shared storage needs
// when a create saves the model there and the storage does not hold it yet:
// every target reads the model from that storage, so it is fetched once,
// through a writer node, rather than by each node. A local storage needs
// nothing here; each node gets the model when it starts (from a node of its
// sync group, else by downloading it).
func (s *Service) startSharedModelDownload(model *store.Model, storage *store.Storage, targets []*store.Node, owner ...uint) error {
	if model == nil || storage == nil || !proto.StorageShared(storage.Type) || s.storageHoldsModel(storage, model, uniqueNodes(targets)) {
		return nil
	}
	_, err := s.StartSharedDownloadOn(model, storage, "", owner...)
	return err
}

func uniqueNodes(nodes []*store.Node) []*store.Node {
	seen := make(map[uint]struct{}, len(nodes))
	out := make([]*store.Node, 0, len(nodes))
	for _, node := range nodes {
		if node == nil {
			continue
		}
		if _, ok := seen[node.ID]; ok {
			continue
		}
		seen[node.ID] = struct{}{}
		out = append(out, node)
	}
	return out
}
