package api

import (
	"net/http"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// acceleratorVendors is the vendor dictionary, in the order the platform
// lists vendors. It is what the vendors' definitions declare and does not
// depend on the cluster's nodes.
func (a *apiHandlers) acceleratorVendors(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, dto.NewList(acceleratorVendorDictionary()))
}

func acceleratorVendorDictionary() []dto.AcceleratorVendor {
	vendors := accelerator.Vendors()
	out := make([]dto.AcceleratorVendor, 0, len(vendors))
	for _, v := range vendors {
		features := make([]string, 0, len(v.Features))
		for _, feature := range v.Features {
			features = append(features, string(feature))
		}
		out = append(out, dto.AcceleratorVendor{
			ID: v.ID, Brand: v.Brand,
			Frameworks: orEmpty(v.Frameworks), Runtimes: orEmpty(v.Runtimes), Features: features,
			PartitionBackend: v.PartitionBackend,
		})
	}
	return out
}

func orEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
