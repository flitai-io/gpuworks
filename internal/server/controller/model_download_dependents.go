package controller

import (
	"sync"

	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A versioned reverse index keeps opaque launch documents in Go and avoids
// decoding the fleet for each canceled model/component. Deployment writes
// invalidate it through the store's existing event generation.
type downloadDependencyCache struct {
	mu         sync.Mutex
	generation uint64
	byModel    map[uint][]store.Deployment
	// undecodable are the active deployments whose models could not be read
	// from their launch documents, with the first such error.
	undecodable    []uint
	undecodableErr error
}

// refresh rebuilds the index if a deployment changed since it was built.
// The caller holds cache.mu.
func (c *Controller) refreshDownloadUsers(cache *downloadDependencyCache) error {
	generation := c.store.Bus.Generation("deployment")
	if cache.byModel != nil && cache.generation == generation {
		return nil
	}
	deps, err := c.store.ListDeployments()
	if err != nil {
		return err
	}
	byModel := map[uint][]store.Deployment{}
	var undecodable []uint
	var undecodableErr error
	for _, dep := range deps {
		if dep.State != store.DeployActive {
			continue
		}
		ids, err := planning.DeploymentModelIDs(&dep)
		if err != nil {
			undecodable = append(undecodable, dep.ID)
			if undecodableErr == nil {
				undecodableErr = err
			}
			continue
		}
		for _, id := range ids {
			byModel[id] = append(byModel[id], dep)
		}
	}
	cache.byModel, cache.generation = byModel, generation
	cache.undecodable, cache.undecodableErr = undecodable, undecodableErr
	return nil
}

// downloadDependents returns the active deployments that use any of the
// models. A deployment whose models cannot be read makes it fail: handing a
// download to a successor must not guess who else needs it.
func (c *Controller) downloadDependents(modelIDs []uint) ([]store.Deployment, error) {
	cache := &c.downloadUsers
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if err := c.refreshDownloadUsers(cache); err != nil {
		return nil, err
	}
	if cache.undecodableErr != nil {
		return nil, cache.undecodableErr
	}
	seen := map[uint]bool{}
	var out []store.Deployment
	for _, modelID := range modelIDs {
		for _, dep := range cache.byModel[modelID] {
			if !seen[dep.ID] {
				seen[dep.ID] = true
				out = append(out, dep)
			}
		}
	}
	return out, nil
}

// downloadDependentIDs returns the ids of the active deployments that use any
// of the models, and of every active deployment whose models cannot be read:
// for a reconcile, a doubt is one more pass, not a reason to reconcile the
// whole fleet.
func (c *Controller) downloadDependentIDs(modelIDs []uint) ([]uint, error) {
	cache := &c.downloadUsers
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if err := c.refreshDownloadUsers(cache); err != nil {
		return nil, err
	}
	seen := map[uint]bool{}
	out := append([]uint(nil), cache.undecodable...)
	for _, id := range out {
		seen[id] = true
	}
	for _, modelID := range modelIDs {
		for _, dep := range cache.byModel[modelID] {
			if !seen[dep.ID] {
				seen[dep.ID] = true
				out = append(out, dep.ID)
			}
		}
	}
	return out, nil
}
