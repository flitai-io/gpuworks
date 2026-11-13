package planning

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/parallelism"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// DeploymentParallelism derives the transient GPU requirement from the stored
// launch configuration. Placement stays the authority for TP/PP and node count;
// DP lives in quick_params or the effective argument stream, not a second
// persisted placement field.
func DeploymentParallelism(dep *store.Deployment, placement Placement) (parallelism.Spec, error) {
	spec := placement.Parallelism(dep.NNodes)
	quick, extra, err := deploymentLaunchSettings(dep)
	if err != nil {
		return spec, err
	}
	var template []string
	if dep.CatalogSelectionSnapshot != "" {
		selection, err := DeploymentTemplate(dep)
		if err != nil {
			return spec, err
		}
		template = selection.ExtraArgs
	}
	dp, err := deploy.PlacementDataParallel(dep.Engine, quick.DP, template, extra)
	if err != nil {
		return spec, fmt.Errorf("deployment %d data parallel: %w", dep.ID, err)
	}
	spec.DP = dp
	return spec, nil
}
