package inferencecatalog

import (
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/deploy"
)

// validateRankScopedWeights checks a spec whose weight subfolder names the
// rank (ADR 0076), in its own weights or a component's. Each weights block
// decides on its own, and every source of it has to name the rank or none
// may: a mirror that did not would hand each rank the whole repository, which
// is exactly what the block avoids. The primary component is the spec's own
// weights, so the two decide alike. The PD layouts number their ranks per
// role instance, not as one group, so none of them may combine with it.
func validateRankScopedWeights(modelID string, spec *InferenceSpec) error {
	where := fmt.Sprintf("model %s spec %s", modelID, spec.Name)
	scoped, err := rankScopedSources(where, spec.Weights)
	if err != nil {
		return err
	}
	anyScoped := scoped
	for _, component := range spec.Components {
		componentScoped, err := rankScopedSources(where+" component "+component.Name, component.Weights)
		if err != nil {
			return err
		}
		if strings.EqualFold(strings.TrimSpace(component.Role), "primary") && componentScoped != scoped {
			return fmt.Errorf("%s: the primary component %s and the spec's own weights must both name ${GPUWORKS_NODE_RANK} in their subfolder or neither", where, component.Name)
		}
		anyScoped = anyScoped || componentScoped
	}
	if anyScoped && (spec.PDMode != "" || len(spec.PDRoles) > 0) {
		return fmt.Errorf("%s: rank-scoped weights cannot be combined with pd_mode", where)
	}
	return nil
}

// rankScopedSources reports whether the sources of one weights block name the
// rank, refusing a block where only some of them do.
func rankScopedSources(where string, weights Weights) (bool, error) {
	scoped := 0
	for _, source := range weights.Sources {
		if deploy.RankScopedSubfolder(source.Subfolder) {
			scoped++
		}
	}
	if scoped != 0 && scoped != len(weights.Sources) {
		return false, fmt.Errorf("%s: either every weight source names ${GPUWORKS_NODE_RANK} in its subfolder or none does", where)
	}
	return scoped != 0, nil
}
