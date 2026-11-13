package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/deployment"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// matchingTemplateSpecs uses the same unsupported veto and specificity order
// as deployment selection. Repeated selection retains every compatible recipe
// for browsing while placing the most specific recipe first.
func matchingTemplateSpecs(model store.InferenceModel, gpu chipmatch.GPU) ([]store.InferenceSpecRow, error) {
	unsupported, err := chipmatch.DecodeUnsupported(model.Unsupported)
	if err != nil {
		return nil, err
	}
	candidates := make([]chipmatch.Spec, 0, len(model.Specs))
	rows := make([]store.InferenceSpecRow, 0, len(model.Specs))
	vendors := []string{gpu.Vendor}
	if gpu.Vendor == "" {
		// The existing API permits a GPU name without a vendor. Keep that query
		// compatible by testing each declared vendor, never by guessing in the UI.
		seen := map[string]bool{}
		vendors = nil
		for _, row := range model.Specs {
			predicate, err := deployment.DecodeSpecPredicate(row)
			if err != nil {
				return nil, err
			}
			if !seen[predicate.Vendor] {
				vendors = append(vendors, predicate.Vendor)
				seen[predicate.Vendor] = true
			}
		}
	}
	result := []store.InferenceSpecRow{}
	for _, vendor := range vendors {
		candidates = candidates[:0]
		rows = rows[:0]
		for _, row := range model.Specs {
			predicate, err := deployment.DecodeSpecPredicate(row)
			if err != nil {
				return nil, err
			}
			if !strings.EqualFold(predicate.Vendor, vendor) {
				continue
			}
			candidates = append(candidates, chipmatch.Spec{Name: row.Name, Quantization: row.Quantization, GPUs: predicate})
			rows = append(rows, row)
		}
		actual := gpu
		actual.Vendor = vendor
		for len(candidates) > 0 {
			selection := chipmatch.Select(actual, candidates, unsupported)
			if !selection.Matched {
				break
			}
			index := selection.SpecIndex
			result = append(result, rows[index])
			candidates = append(candidates[:index], candidates[index+1:]...)
			rows = append(rows[:index], rows[index+1:]...)
		}
	}
	return result, nil
}

func detectedTemplateSupport(model store.InferenceModel, gpus []chipmatch.GPU) (dto.InferenceDetectedSupport, error) {
	result := dto.InferenceDetectedSupport{Warnings: []dto.InferenceDetectedWarning{}}
	rules, err := chipmatch.DecodeUnsupported(model.Unsupported)
	if err != nil {
		return result, err
	}
	selectedEngines := map[string]bool{}
	matching := make([][]store.InferenceSpecRow, len(gpus))
	for i, gpu := range gpus {
		rows, err := matchingTemplateSpecs(model, gpu)
		if err != nil {
			return result, err
		}
		matching[i] = rows
		if len(rows) > 0 {
			selectedEngines[rows[0].Engine] = true
		}
	}
	for _, rule := range rules {
		warning := dto.InferenceDetectedWarning{Reason: rule.Reason, Quantization: rule.Quantization}
		found := false
		for i, gpu := range gpus {
			if !chipmatch.Evaluate(gpu, rule.GPUs).Matched {
				continue
			}
			found = true
			for _, row := range matching[i] {
				if rule.Quantization != "" && !strings.EqualFold(rule.Quantization, row.Quantization) {
					warning.AlternativeAvailable = true
				}
			}
		}
		if found {
			result.Warnings = append(result.Warnings, warning)
		}
	}
	// With no detected compatible recipe, keep offline browsing usable. This
	// preference selects a tab; eligibility remains the evaluate API's decision.
	if len(selectedEngines) == 0 {
		for _, row := range model.Specs {
			selectedEngines[row.Engine] = true
		}
	}
	engines := make([]string, 0, len(selectedEngines))
	for engine := range selectedEngines {
		engines = append(engines, engine)
	}
	sort.Slice(engines, func(i, j int) bool {
		if (engines[i] == deploy.EngineVLLM) != (engines[j] == deploy.EngineVLLM) {
			return engines[i] == deploy.EngineVLLM
		}
		return engines[i] < engines[j]
	})
	if len(engines) > 0 {
		result.PreferredEngine = engines[0]
	}
	return result, nil
}

// templateCompatibilityGPUs scopes recommendation eligibility to the explicit
// node, or the online target-vendor pool for automatic placement. Specific GPU
// indexes, memory and other deployment constraints remain evaluate's decision.
func (a *apiHandlers) templateCompatibilityGPUs(r *http.Request) ([]chipmatch.GPU, bool, int, error) {
	query := r.URL.Query()
	mode := strings.TrimSpace(query.Get("compatibility"))
	if mode == "" {
		return nil, false, 0, nil
	}
	vendor := accelerator.NormalizeVendor(query.Get("vendor"))
	if mode != "detected" || vendor == "" {
		return nil, true, http.StatusBadRequest, fmt.Errorf("compatibility=detected requires a known vendor")
	}
	result := []chipmatch.GPU{}
	appendGPU := func(name, rawVendor, arch, cc string) {
		gpu := chipmatch.ReportedGPU(name, rawVendor, arch, cc)
		if gpu.Vendor == vendor {
			result = append(result, gpu)
		}
	}
	if uuid := strings.TrimSpace(query.Get("node_uuid")); uuid != "" {
		node, err := a.d.Store.NodeWithGPUs(uuid)
		if err != nil {
			status := http.StatusInternalServerError
			if err == store.ErrNotFound {
				status = http.StatusNotFound
			}
			return nil, true, status, err
		}
		if node.State == "online" {
			for _, gpu := range node.GPUs {
				appendGPU(gpu.Name, gpu.Vendor, gpu.ArchID, gpu.ComputeCapability)
			}
		}
		return result, true, 0, nil
	}
	rows, err := a.d.Store.ListDistinctOnlineDetectedGPUs()
	if err != nil {
		return nil, true, http.StatusInternalServerError, err
	}
	for _, gpu := range rows {
		appendGPU(gpu.Name, gpu.Vendor, gpu.ArchID, gpu.CC)
	}
	return result, true, 0, nil
}

func compatibleTemplateProfiles(model store.InferenceModel, gpus []chipmatch.GPU) (map[string]bool, error) {
	result := map[string]bool{}
	unsupported, err := chipmatch.DecodeUnsupported(model.Unsupported)
	if err != nil {
		return nil, err
	}
	for _, row := range model.Specs {
		predicate, err := deployment.DecodeSpecPredicate(row)
		if err != nil {
			return nil, err
		}
		spec := chipmatch.Spec{Name: row.Name, Quantization: row.Quantization, GPUs: predicate}
		for _, gpu := range gpus {
			if chipmatch.ValidateSpec(gpu, spec, unsupported) == nil {
				result[row.Source+"\x00"+row.Name] = true
				break
			}
		}
	}
	return result, nil
}
