package planning

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// DeploymentModelIDs is the common dependency set for admission, launch and
// preparation display: the deployment's own model, each template component's,
// then each model an extra mount row names. Component bindings are immutable
// model IDs.
func DeploymentModelIDs(dep *store.Deployment) ([]uint, error) {
	layer, err := DeploymentTemplate(dep)
	if err != nil {
		return nil, err
	}
	ids := []uint{dep.ModelID}
	seen := map[uint]bool{dep.ModelID: true}
	for _, component := range layer.Components {
		if component.StoreModelID == 0 {
			return nil, fmt.Errorf("deployment %d component %s has no immutable model binding", dep.ID, component.Name)
		}
		if !seen[component.StoreModelID] {
			ids = append(ids, component.StoreModelID)
			seen[component.StoreModelID] = true
		}
	}
	for _, id := range LaunchSourceFromDeployment(dep).Overrides.ModelMountIDs() {
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	return ids, nil
}
