package api

import (
	"net/http"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func (a *apiHandlers) playgroundModels(w http.ResponseWriter, r *http.Request) {
	catalog := dto.PlaygroundCatalog{
		Models:        []dto.PlaygroundModel{},
		Parameters:    playgroundParameters(),
		StreamOptions: playgroundStreamOptions(),
		HTMLPreview:   dto.PlaygroundHTMLPreview{AllowNetwork: a.d.Store.SecurityPolicy().PlaygroundPreviewNetwork},
	}

	deps, err := a.d.Store.ListDeployments()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	type deploymentCandidate struct {
		deployment *store.Deployment
		instances  []store.Instance
		score      int
	}
	candidates := make([]deploymentCandidate, 0, len(deps))
	byServedName := make(map[string]int, len(deps))
	for i := range deps {
		dep := &deps[i]
		if dep.State != store.DeployActive || strings.TrimSpace(dep.ServedName) == "" {
			continue
		}
		instances, _ := a.d.Store.InstancesForDeployment(dep.ID)
		candidate := deploymentCandidate{deployment: dep, instances: instances, score: playgroundDeploymentScore(instances)}
		if index, exists := byServedName[dep.ServedName]; exists {
			// ListDeployments is newest-first. Keep that deterministic fallback,
			// but never let a replacement that is still starting hide the
			// rollout which is already serving (or sleeping and can be woken).
			if candidate.score > candidates[index].score {
				candidates[index] = candidate
			}
			continue
		}
		byServedName[dep.ServedName] = len(candidates)
		candidates = append(candidates, candidate)
	}
	for _, candidate := range candidates {
		// The page treats catalog length as the number of available models.
		// Score 0 cannot serve and cannot be woken, so a failed deployment
		// or one that is still starting must not be counted.
		if candidate.score == 0 {
			continue
		}
		dep := candidate.deployment
		model, err := a.d.Store.GetModel(dep.ModelID)
		if err != nil {
			continue
		}
		modelView := a.modelDTO(model, false)
		// The page offers a wake exactly when the gateway refuses every request
		// of the deployment for sleep, which is what score 1 means: at least one
		// instance fills a replica, and every one of them is asleep. A
		// deployment some of whose replicas are awake is served by them, so it
		// needs no wake.
		sleeping := candidate.score == 1
		contract := inferenceapi.ResolveDeploymentContract(a.d.Store, dep)
		contract.Operations = omitUnservedTranscription(contract.Operations, candidate.instances)
		if len(contract.Operations) == 0 {
			// An explicitly empty or invalid model operation configuration has no
			// callable Playground entry. Do not synthesize Chat as a fallback.
			continue
		}
		hasChat := false
		for _, operation := range contract.Operations {
			if operation.Type == inferenceapi.OperationChat {
				hasChat = true
				break
			}
		}
		catalog.Models = append(catalog.Models, dto.PlaygroundModel{
			ID: dep.ServedName, Label: dep.ServedName, Operations: playgroundOperations(contract, dep.ServedName),
			InputModalities: modelView.InputModalities, ContextLength: model.ContextLength,
			Sleeping: sleeping, WakesOnRequest: sleeping && planning.RequestWakes(dep, candidate.instances), DeploymentID: dep.ID, TemplateID: dep.TemplateID, TemplateSpecSource: dep.TemplateSpecSource, TemplateSpecName: dep.TemplateSpecName, PublicDataPlane: true,
			ExtraParameterPresets: playgroundPresets(playgroundThinkingSwitchFor(dep.Engine, dep.ServedName, model, hasChat)),
		})
	}

	writeJSON(w, http.StatusOK, catalog)
}

// playgroundDeploymentScore ranks a deployment's instances for the catalog: 2
// when one of them can answer now, 1 when they all sleep (the page offers a
// wake), 0 when none could serve whatever happens (a failed or starting
// deployment is not a model the page lists). Sleep here is the gateway's
// judgement, so a replica under a sleep order it has not carried out yet is
// not counted as able to answer.
func playgroundDeploymentScore(instances []store.Instance) int {
	score := 0
	for _, instance := range instances {
		if instance.State != proto.InstRunning || instance.Headless || instance.APIPort <= 0 {
			continue
		}
		if !planning.InstanceAsleep(&instance) {
			return 2
		}
		score = 1
	}
	return score
}

func playgroundOperations(contract inferenceapi.Contract, model string) []dto.PlaygroundOperation {
	result := make([]dto.PlaygroundOperation, 0, len(contract.Operations))
	for _, operation := range contract.Operations {
		definition, ok := inferenceapi.Lookup(operation.Type)
		if !ok {
			continue
		}
		operationView := dto.PlaygroundOperation{
			RequestDefaults: operation.RequestDefaults, Inputs: append([]inferenceapi.InputField{}, operation.Inputs...),
			PrimaryInputs: definition.PrimaryInputs, Discovery: definition.Discovery,
			Type: operation.Type, Endpoint: operation.Endpoint, Mode: operation.Mode,
			RequestEncoding:  operation.RequestEncoding,
			RequestEncodings: definition.RequestEncodings, Renderer: definition.Renderer,
			Response: definition.Response, Async: definition.Async,
			ExampleInput:       operation.ExampleInput,
			MaxReferenceImages: operation.ReferenceImageLimit(),
		}
		if example, err := inferenceapi.BuildExample(operation, model); err == nil {
			operationView.Example = &example
		}
		result = append(result, operationView)
	}
	return result
}

// omitUnservedTranscription drops openai.audio.transcribe when a running
// replica has published an OpenAPI document that does not list
// /v1/audio/transcriptions. An unknown observation leaves the declared
// contract unchanged.
func omitUnservedTranscription(operations []inferenceapi.Operation, instances []store.Instance) []inferenceapi.Operation {
	known, present := false, false
	for _, instance := range instances {
		if instance.Headless || instance.State != proto.InstRunning || instance.TranscriptionServed == nil {
			continue
		}
		known = true
		present = present || *instance.TranscriptionServed
	}
	if !known || present {
		return operations
	}
	kept := make([]inferenceapi.Operation, 0, len(operations))
	for _, operation := range operations {
		if operation.Type == inferenceapi.OperationAudioTranscribe {
			continue
		}
		kept = append(kept, operation)
	}
	return kept
}
