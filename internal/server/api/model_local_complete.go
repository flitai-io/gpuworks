package api

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// lacksMetadata reports a model whose source was never read: registered
// offline, or while the management node could not reach it.
func lacksMetadata(m *store.Model) bool {
	return m.Meta == "" || m.WeightBytes == 0
}

// completeFromLocalCopy fills what a registered model lacks from the copy just
// registered on a node: config.json and README as the directory has them,
// and its size - an offline cluster's Hugging Face model can then be sized
// and estimated. What the model already has is never overwritten. A model
// registered without a commit is pinned to the one the directory was
// downloaded at, when it is the model's only copy (store.PinModelRevision).
func (a *apiHandlers) completeFromLocalCopy(ctx context.Context, m *store.Model, node *store.Node, reg registerPathResponse) error {
	if m.Source == "huggingface" && reg.Revision != "" {
		if _, err := a.d.Store.PinModelRevision(m.ID, reg.Revision); err != nil {
			return err
		}
	}
	if !lacksMetadata(m) || reg.Metadata == nil {
		return nil
	}
	dir, err := a.modelDirFromMetadata(ctx, node.UUID, reg.Path, reg.Metadata)
	if err != nil {
		return nil //nolint:nilerr // Metadata completion is optional; retain the already registered local model.
	}
	local := dir.model(m.Source, m.SourceID, m.Revision, m.Name, reg.SizeBytes)
	current, err := a.d.Store.GetModel(m.ID)
	if err != nil {
		return err
	}
	changed := false
	fill := func(have *string, want string) {
		if *have == "" && want != "" {
			*have, changed = want, true
		}
	}
	fill(&current.Meta, local.Meta)
	fill(&current.Arch, local.Arch)
	fill(&current.Dtype, local.Dtype)
	fill(&current.Precision, local.Precision)
	fill(&current.README, local.README)
	if current.ContextLength == 0 && local.ContextLength > 0 {
		current.ContextLength, changed = local.ContextLength, true
	}
	if current.WeightBytes == 0 && local.WeightBytes > 0 {
		current.WeightBytes, changed = local.WeightBytes, true
	}
	if !changed {
		return nil
	}
	_, err = a.d.Store.UpsertModel(current)
	return err
}
