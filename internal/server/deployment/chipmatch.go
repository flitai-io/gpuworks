package deployment

import (
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type templateHardware struct {
	NodeID    uint
	GPU       chipmatch.GPU
	HBMBytes  int64
	CardCount int
}

func nodeTemplateHardware(node *store.Node) templateHardware {
	if node == nil {
		return templateHardware{}
	}
	owned := node.OwnedGPUs()
	if len(owned) == 0 {
		return templateHardware{}
	}
	first := owned[0]
	hardware := templateHardware{NodeID: node.ID, GPU: chipmatch.ReportedGPU(first.Name, first.Vendor, first.ArchID, first.ComputeCapability), HBMBytes: first.MemoryTotal}
	for _, gpu := range owned {
		if !gpu.CanTakeWork() || !strings.EqualFold(gpu.Name, first.Name) {
			continue
		}
		hardware.CardCount++
		if hardware.HBMBytes == 0 || (gpu.MemoryTotal > 0 && gpu.MemoryTotal < hardware.HBMBytes) {
			hardware.HBMBytes = gpu.MemoryTotal
		}
	}
	return hardware
}

func DecodeSpecPredicate(spec store.InferenceSpecRow) (chipmatch.Predicate, error) {
	predicate, err := chipmatch.DecodePredicate(spec.GPUs)
	if err != nil {
		return predicate, fmt.Errorf("spec %s has invalid gpus predicate: %w", spec.Name, err)
	}
	return predicate, nil
}

func decodeUnsupported(model store.InferenceModel) ([]chipmatch.Unsupported, error) {
	result, err := chipmatch.DecodeUnsupported(model.Unsupported)
	if err != nil {
		return nil, fmt.Errorf("model %s has invalid unsupported rules: %w", model.ModelID, err)
	}
	return result, nil
}

func selectInferenceSpec(model store.InferenceModel, requestedSource, requestedName string, gpu chipmatch.GPU, engine, scenario string) (*store.InferenceSpecRow, chipmatch.Selection, error) {
	requestedSource = strings.ToLower(strings.TrimSpace(requestedSource))
	requestedName = strings.TrimSpace(requestedName)
	engine = strings.ToLower(strings.TrimSpace(engine))
	scenario = strings.ToLower(strings.TrimSpace(scenario))
	unsupported, err := decodeUnsupported(model)
	if err != nil {
		return nil, chipmatch.Selection{}, err
	}

	type candidate struct {
		index int
		row   chipmatch.Spec
	}
	var candidates []candidate
	for index := range model.Specs {
		spec := &model.Specs[index]
		if requestedSource != "" && spec.Source != requestedSource {
			continue
		}
		if requestedName != "" && spec.Name != requestedName {
			continue
		}
		if engine != "" && !strings.EqualFold(spec.Engine, engine) {
			continue
		}
		if scenario != "" && !strings.EqualFold(spec.Scenario, scenario) {
			continue
		}
		predicate, err := DecodeSpecPredicate(*spec)
		if err != nil {
			return nil, chipmatch.Selection{}, err
		}
		candidates = append(candidates, candidate{index: index, row: chipmatch.Spec{Name: spec.Name, GPUs: predicate, Quantization: spec.Quantization}})
	}
	if requestedName != "" && len(candidates) == 0 {
		return nil, chipmatch.Selection{}, fmt.Errorf("inference spec %s/%q does not exist for the selected engine and scenario", requestedSource, requestedName)
	}
	if requestedName != "" && requestedSource == "" && len(candidates) > 1 {
		return nil, chipmatch.Selection{Ambiguous: true}, fmt.Errorf("inference spec %q exists in multiple catalog sources; request template_spec_source explicitly", requestedName)
	}
	rows := make([]chipmatch.Spec, len(candidates))
	for index := range candidates {
		rows[index] = candidates[index].row
	}
	// An explicitly named flat spec is a complete execution contract. Preview
	// may therefore resolve it without pretending that an online GPU exists;
	// actual deployment still evaluates the predicate against its target node.
	if requestedName != "" && gpu.Vendor == "" && gpu.Name == "" && gpu.CC == 0 {
		selected := &model.Specs[candidates[0].index]
		return selected, chipmatch.Selection{Matched: true, SpecIndex: 0, Reason: "explicit spec name", MatchedBy: "spec-name"}, nil
	}
	selection := chipmatch.Select(gpu, rows, unsupported)
	if !selection.Matched {
		if selection.Reason == "" {
			selection.Reason = "no matching inference spec"
		}
		return nil, selection, fmt.Errorf("%s: %s", gpu.Name, selection.Reason)
	}
	if selection.Ambiguous && requestedName == "" {
		return nil, selection, fmt.Errorf("%s matches multiple equally specific inference specs; request template_spec_name explicitly", gpu.Name)
	}
	selected := &model.Specs[candidates[selection.SpecIndex].index]
	return selected, selection, nil
}
