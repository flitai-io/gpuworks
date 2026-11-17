package api

import (
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// listRows is every deployment and instance with what they join against: the
// server's shared snapshot (Deps.Lists) when it has one, read for this request
// otherwise. The deployments and instances are the caller's own copies; the
// rows are shared and read-only, and a page reads its progress again with
// ListRows.Fresh.
func (a *apiHandlers) listRows() (*deployment.ListRows, []store.Deployment, []store.Instance, error) {
	if a.d.Lists != nil {
		rows, err := a.d.Lists.Rows()
		if err != nil {
			return nil, nil, nil, err
		}
		return rows, rows.Deployments(), rows.AllInstances(), nil
	}
	rows, err := deployment.ReadListRows(a.d.Store, a.d.Preparation)
	if err != nil {
		return nil, nil, nil, err
	}
	return rows, rows.Deployments(), rows.AllInstances(), nil
}

// deploymentIDsOf are the ids of deps.
func deploymentIDsOf(deps []*store.Deployment) []uint {
	ids := make([]uint, 0, len(deps))
	for _, d := range deps {
		ids = append(ids, d.ID)
	}
	return ids
}
