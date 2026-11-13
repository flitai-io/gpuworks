package deployment

import (
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func (s *Service) resolveTemplateWeightModelForNode(source weightSource, original *store.InferenceModel, engine, quantization string, weightBytes int64, nodeID uint, pinned []uint, explicitProvider ...bool) (*store.Model, error) {
	if source.RepoID == "" || original == nil {
		return nil, store.ErrNotFound
	}
	var resolveErr error
	source, resolveErr = s.resolveWeightRevision(source, nodeID)
	if resolveErr != nil {
		return nil, resolveErr
	}
	existing, findErr := s.d.Store.FindModelArtifact(source.RepoID, source.Provider, source.Revision, source.Subfolder)
	if findErr != nil && findErr != store.ErrNotFound {
		return nil, findErr
	}
	explicit := len(explicitProvider) > 0 && explicitProvider[0]
	models, err := s.d.Store.ListModels(source.RepoID)
	if err != nil {
		return nil, err
	}
	candidates := make([]store.Model, 0, len(models))
	for _, model := range models {
		if TemplateWeightMatchesModel(source, &model) && (!explicit || strings.EqualFold(strings.TrimSpace(model.Source), strings.TrimSpace(source.Provider))) {
			candidates = append(candidates, model)
		}
	}
	availability, err := s.templateArtifactAvailability(candidates, nodeID, pinned)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		state := availability[existing.ID]
		if !state.failedOnly && (state.ready || nodeID == 0) {
			return existing, nil
		}
	}
	var candidate *store.Model
	if explicit {
		candidate = selectExplicitTemplateArtifact(candidates, source.Revision, weightBytes, availability)
	} else {
		candidate = selectPreferredTemplateArtifact(candidates, source, availability)
	}
	if candidate != nil {
		return candidate, nil
	}
	if existing != nil {
		if !availability[existing.ID].failedOnly {
			return existing, nil
		}
		return nil, productError(messages.ErrorModelSourceUnavailable, "the selected template artifact has failed; retry or select another model", map[string]any{
			"model_id": existing.ID, "provider": existing.Source, "repo_id": existing.SourceID, "revision": existing.Revision,
		})
	}
	if strings.EqualFold(strings.TrimSpace(source.Provider), "local") {
		return nil, productError(messages.ErrorModelSourceUnavailable, "register the local model weights before deploying this template", map[string]any{
			"provider": "local", "repo_id": source.RepoID, "reason": "local_model_not_registered",
		})
	}
	return s.createTemplateWeightModel(source, original, engine, quantization, weightBytes)
}

func selectExplicitTemplateArtifact(providerCandidates []store.Model, revision string, weightBytes int64, availability map[uint]templateArtifactState) *store.Model {
	revision = strings.TrimSpace(revision)
	var imported *store.Model
	for i := range providerCandidates {
		item := &providerCandidates[i]
		if !availability[item.ID].ready {
			continue
		}
		itemRevision := strings.TrimSpace(item.Revision)
		if itemRevision == revision {
			return item
		}
		// Unknown revisions may be imported copies of the pinned weights;
		// a different known revision is never a substitute (ADR 0018).
		if imported == nil && itemRevision == "" && sameWeightSize(item.WeightBytes, weightBytes) {
			imported = item
		}
	}
	return imported
}

func selectPreferredTemplateArtifact(candidates []store.Model, source weightSource, availability map[uint]templateArtifactState) *store.Model {
	var candidate *store.Model
	for i := range candidates {
		item := &candidates[i]
		if availability[item.ID].failedOnly {
			continue
		}
		if candidate == nil || preferTemplateArtifact(item, candidate, source, availability) {
			candidate = item
		}
	}
	return candidate
}

func preferTemplateArtifact(candidate, current *store.Model, source weightSource, availability map[uint]templateArtifactState) bool {
	if ready, currentReady := availability[candidate.ID].ready, availability[current.ID].ready; ready != currentReady {
		return ready
	}
	providerMatches := func(model *store.Model) bool {
		return strings.EqualFold(strings.TrimSpace(model.Source), strings.TrimSpace(source.Provider))
	}
	if matches, currentMatches := providerMatches(candidate), providerMatches(current); matches != currentMatches {
		return matches
	}
	return strings.TrimSpace(candidate.Revision) == strings.TrimSpace(source.Revision) && strings.TrimSpace(current.Revision) != strings.TrimSpace(source.Revision)
}

// sameWeightSize accepts sizes that differ by provider bookkeeping files
// (manifests, checksums): at most 0.1% of the expected size, and never less
// than 1 MiB.
func sameWeightSize(actual, expected int64) bool {
	if actual <= 0 || expected <= 0 {
		return false
	}
	diff := actual - expected
	if diff < 0 {
		diff = -diff
	}
	return diff <= max(expected/1000, 1<<20)
}

func (s *Service) readyModelArtifacts(models []store.Model, nodeID uint, pinned []uint) (map[uint]bool, error) {
	ready := make(map[uint]bool, len(models))
	if nodeID == 0 || len(models) == 0 {
		return ready, nil
	}
	node, err := s.d.Store.NodeByID(nodeID)
	if err != nil {
		return nil, err
	}
	ids := make([]uint, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	sources, err := s.d.Store.ModelSourcesFor(ids, []*store.Node{node})
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		ready[id] = sources.Source(id, nodeID, store.SourceRules{Pinned: pinned}).Action == store.SourceReady
	}
	return ready, nil
}

type templateArtifactState struct {
	ready      bool
	failedOnly bool
}

func (s *Service) templateArtifactAvailability(models []store.Model, nodeID uint, pinned []uint) (map[uint]templateArtifactState, error) {
	out := make(map[uint]templateArtifactState, len(models))
	if len(models) == 0 {
		return out, nil
	}
	ready, err := s.readyModelArtifacts(models, nodeID, pinned)
	if err != nil {
		return nil, err
	}
	ids := make([]uint, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
		out[model.ID] = templateArtifactState{ready: ready[model.ID]}
	}
	files, err := s.d.Store.ModelFilesForModels(ids, nodeID)
	if err != nil {
		return nil, err
	}
	var applies map[uint]bool
	if nodeID != 0 {
		node, err := s.d.Store.NodeByID(nodeID)
		if err != nil {
			return nil, err
		}
		storages, err := s.d.Store.StoragesForNode(node)
		if err != nil {
			return nil, err
		}
		applies = make(map[uint]bool, len(storages))
		for _, storage := range storages {
			applies[storage.Storage.ID] = true
		}
	}
	hasFailure := map[uint]bool{}
	viable := map[uint]bool{}
	for i := range files {
		file := &files[i]
		if (len(pinned) > 0 && !slices.Contains(pinned, file.StorageID)) || (applies != nil && !applies[file.StorageID]) {
			continue
		}
		hasFailure[file.ModelID] = hasFailure[file.ModelID] || file.State == proto.MFFailed
		viable[file.ModelID] = viable[file.ModelID] || file.Ready() || store.TransferUnderWay(file)
	}
	for id, state := range out {
		state.failedOnly = hasFailure[id] && !state.ready && !viable[id]
		out[id] = state
	}
	return out, nil
}

func (s *Service) resolveTemplateWeightModel(source weightSource, original *store.InferenceModel, engine, quantization string, weightBytes int64) (*store.Model, error) {
	return s.resolveTemplateWeightModelForNode(source, original, engine, quantization, weightBytes, 0, nil)
}

func (s *Service) modelHasReadyArtifact(model *store.Model, nodeID uint, pinned []uint) (bool, error) {
	if model == nil || nodeID == 0 {
		return false, nil
	}
	ready, err := s.readyModelArtifacts([]store.Model{*model}, nodeID, pinned)
	return ready[model.ID], err
}
