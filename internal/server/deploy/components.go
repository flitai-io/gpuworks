package deploy

// ComponentBinding is the immutable deployment-snapshot binding between one
// catalog component, its selected weight artifact, and its container mount.
// StoreModelID is an internal database projection used by the controller; it
// is never inferred again from the mutable catalog after deployment creation.
type ComponentBinding struct {
	Name            string `json:"name"`
	Role            string `json:"role"`
	OriginalModelID string `json:"model_id,omitempty"`
	Provider        string `json:"provider"`
	RepoID          string `json:"repo_id"`
	Revision        string `json:"revision,omitempty"`
	// Subfolder is the template's: a rank-scoped one keeps the rank variable
	// (as in "rank${GPUWORKS_NODE_RANK}"), and each rank's own subfolder is in
	// RankWeights.
	Subfolder    string `json:"subfolder,omitempty"`
	Quantization string `json:"quantization,omitempty"`
	MountPath    string `json:"mount_path"`
	// StoreModelID is the artifact the component mounts, rank 0's when the
	// component is rank-scoped.
	StoreModelID uint `json:"store_model_id"`
	// WeightBytes is what one rank reads: the whole component, or one rank's
	// part of a rank-scoped one.
	WeightBytes int64 `json:"weight_bytes,omitempty"`
	// RankWeights binds each rank of a non-primary component whose subfolder
	// names the rank to the artifact that rank mounts at MountPath (ADR 0076);
	// empty when every rank mounts StoreModelID. The primary component's ranks
	// are the deployment's own (CatalogSelection.RankWeights).
	RankWeights []RankWeight `json:"rank_weights,omitempty"`
}

func PrimaryComponent(components []ComponentBinding) (ComponentBinding, bool) {
	for _, component := range components {
		if component.Role == "primary" {
			return component, true
		}
	}
	return ComponentBinding{}, false
}

// RankScoped reports whether each rank mounts its own artifact of the
// component.
func (c ComponentBinding) RankScoped() bool { return len(c.RankWeights) > 0 }

// RankModelID is the artifact rank mounts: its own for a rank-scoped
// component, StoreModelID otherwise. False when the component binds no
// artifact for rank.
func (c ComponentBinding) RankModelID(rank int) (uint, bool) {
	if !c.RankScoped() {
		return c.StoreModelID, c.StoreModelID != 0
	}
	for _, weight := range c.RankWeights {
		if weight.Rank == rank && weight.StoreModelID != 0 {
			return weight.StoreModelID, true
		}
	}
	return 0, false
}

// GroupWeightBytes is what the whole group holds of the component: one rank's
// part times the ranks bound for a rank-scoped component, the component
// itself otherwise (every rank holds the same bytes).
func (c ComponentBinding) GroupWeightBytes() int64 {
	return c.WeightBytes * int64(max(len(c.RankWeights), 1))
}
