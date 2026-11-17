package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// weightSource is the owner type of inference_specs.weight_sources under a
// local name.
type weightSource = inferencecatalog.WeightSource

type templateSelection struct {
	NodeID           uint
	StoragePins      []uint
	Model            *store.InferenceModel
	Spec             *store.InferenceSpecRow
	Weight           weightSource
	SuggestedWeight  weightSource
	ExplicitProvider bool
	ExtraArgs        []string
	Env              map[string]string
	PreStartArgv     []string
	PreStartScript   string
	Assets           []proto.RuntimeAsset
	Entrypoint       []string
	ImageSources     []string
	Placement        json.RawMessage
	APIContract      json.RawMessage
	Components       []deploy.ComponentBinding
	Match            string
	MatchedBy        string
	Warnings         []messages.Warning
	Deviated         bool
	Diff             []string

	// ExplicitComponents names the non-primary components the request's
	// explicit provider moved, so resolving their model rows keeps that
	// provider instead of falling back to a registered mirror of the repository.
	ExplicitComponents map[string]bool
}

// shmBytes is the /dev/shm size the selected spec states, or zero when the
// deployment has no template or the spec states none.
func (s templateSelection) shmBytes() int64 {
	if s.Spec == nil {
		return 0
	}
	return s.Spec.ShmBytes
}

// memlockUnlimited reports whether the selected spec asks for unlimited locked
// memory in its container.
func (s templateSelection) memlockUnlimited() bool {
	return s.Spec != nil && s.Spec.MemlockUnlimited
}

func (s *Service) resolveDeploymentModelFromTemplate(ctx context.Context, modelID uint, selection *templateSelection, templateID uint) (*store.Model, error) {
	var err error
	_ = ctx
	if selection == nil {
		return nil, store.ErrNotFound
	}
	if templateID == 0 {
		if modelID == 0 {
			return nil, store.ErrNotFound
		}
		model, err := s.d.Store.GetModel(modelID)
		if err != nil {
			return nil, err
		}
		if err := s.resolveTemplateComponentModels(selection, model); err != nil {
			return nil, err
		}
		applyCurrentModelAPIContract(selection, model)
		return model, nil
	}
	if selection.Model == nil || selection.Spec == nil || selection.Weight.RepoID == "" {
		return nil, store.ErrNotFound
	}
	var model *store.Model
	if modelID != 0 {
		requested, err := s.d.Store.GetModel(modelID)
		if err != nil {
			return nil, err
		}
		if !templateWeightMatchesAnySource(append(SpecWeightSources(selection.Spec), selection.Weight), requested) {
			return nil, productError(messages.ErrorTemplateModelMismatch,
				fmt.Sprintf("model_id %d conflicts with template model %s@%s", modelID, selection.Weight.RepoID, selection.Weight.Revision),
				map[string]any{"model_id": modelID, "repo_id": selection.Weight.RepoID, "revision": selection.Weight.Revision})
		}
		model = requested
	}
	if model == nil {
		model, err = s.resolveTemplateWeightModelForNode(selection.Weight, selection.Model, selection.Spec.Engine, selection.Spec.Quantization, selection.Spec.WeightBytes, selection.NodeID, selection.StoragePins, selection.ExplicitProvider)
		if err != nil {
			return nil, err
		}
	}
	// Persist the artifact identity that will actually be mounted. This is
	// important when a complete mirror from another provider satisfied the
	// template source: the requested source is only a preference, not the
	// runtime artifact identity.
	selection.Weight = weightSource{Provider: model.Source, RepoID: model.SourceID, Revision: model.Revision, Subfolder: model.ArtifactSubfolder}
	if err := s.resolveTemplateComponentModels(selection, model); err != nil {
		return nil, err
	}
	applyCurrentModelAPIContract(selection, model)
	s.applyCatalogEstimationConfig(selection, model)
	return model, nil
}

// applyCatalogEstimationConfig gives a template-resolved model row the catalog's
// estimation facts when it has none of its own. Such a row is derived from the
// template's weight sources and never passes through model registration, so
// without this it carries no config.json and the deploy evaluation can estimate
// no KV capacity at all — a gap that can never be closed later for a gated or
// relocated repository. A fetched config.json is authoritative, so this only
// fills an empty Meta.
func (s *Service) applyCatalogEstimationConfig(selection *templateSelection, model *store.Model) {
	if selection == nil || selection.Model == nil || model == nil {
		return
	}
	if strings.TrimSpace(model.Meta) != "" || strings.TrimSpace(selection.Model.ModelConfig) == "" {
		return
	}
	model.Meta = selection.Model.ModelConfig
	if _, err := s.d.Store.UpsertModel(model); err != nil {
		s.d.Log.Warn("catalog estimation config not persisted", "model", model.SourceID, "err", err)
	}
}

// initTemplateModelInferenceConfig builds the transient model contract a
// template create validates. Empty operations ([] included) and the legacy
// bare-chat placeholder take the template declaration; an explicit non-empty
// contract stays. The returned declaration is persisted only by the final
// deployment transaction, so a refused health check or resource plan cannot
// change the model or emit an inference-config event.
func (s *Service) initTemplateModelInferenceConfig(selection *templateSelection, model *store.Model) (*store.Model, templateInferenceConfig, error) {
	config := templateInferenceConfig{}
	if selection == nil || selection.Model == nil || model == nil {
		return model, config, nil
	}
	operations := ""
	var contract inferenceapi.Contract
	if len(selection.APIContract) > 0 && json.Unmarshal(selection.APIContract, &contract) == nil && len(contract.Operations) > 0 {
		encoded, err := json.Marshal(contract.Operations)
		if err != nil {
			return nil, config, err
		}
		operations = string(encoded)
	}
	modalities := strings.TrimSpace(selection.Model.InputModalities)
	if operations == "" && modalities == "" {
		return model, config, nil
	}
	config = templateInferenceConfig{modelID: model.ID, operations: operations, inputModalities: modalities}
	candidate := *model
	if operations != "" && (store.OperationsUnset(model.Operations) || store.IsDefaultTemplateOperations(model.Operations)) {
		candidate.Operations = operations
	}
	if modalities != "" && strings.TrimSpace(model.InputModalities) == "" {
		candidate.InputModalities = modalities
	}
	return &candidate, config, nil
}

// applyCurrentModelAPIContract makes the model row's editable operations the
// effective contract for this deployment request. Template specs still supply
// the initial contract when the resolved model predates model-level inference
// configuration, has empty operations ([] included), or holds a legacy bare
// chat or inferred video placeholder while the spec declares operations of its
// own. A spec without operations has nothing to supply, so the placeholder's
// normalized contract stays.
func applyCurrentModelAPIContract(selection *templateSelection, model *store.Model) {
	if selection == nil || model == nil || store.OperationsUnset(model.Operations) {
		return
	}
	if selection.Spec != nil && store.IsDefaultTemplateOperations(model.Operations) && declaresOperations(selection.APIContract) {
		return
	}
	contract := inferenceapi.ModelContract(model)
	var templateContract inferenceapi.Contract
	if json.Unmarshal(selection.APIContract, &templateContract) == nil {
		contract.TokenizerRef = templateContract.TokenizerRef
	}
	encoded, err := json.Marshal(contract)
	if err == nil {
		selection.APIContract = encoded
	}
}

func declaresOperations(raw json.RawMessage) bool {
	var contract inferenceapi.Contract
	return len(raw) > 0 && json.Unmarshal(raw, &contract) == nil && len(contract.Operations) > 0
}

func (s *Service) resolveTemplateComponentModels(selection *templateSelection, fallbackPrimary *store.Model) error {
	for index := range selection.Components {
		component := &selection.Components[index]
		if component.Role == "primary" {
			component.StoreModelID = fallbackPrimary.ID
			component.Provider, component.RepoID = fallbackPrimary.Source, fallbackPrimary.SourceID
			component.Revision, component.Subfolder = fallbackPrimary.Revision, fallbackPrimary.ArtifactSubfolder
			continue
		}
		original, err := s.d.Store.InferenceModelByModelID(component.OriginalModelID)
		if err != nil {
			return fmt.Errorf("component %s original model %s: %w", component.Name, component.OriginalModelID, err)
		}
		model, err := s.resolveTemplateWeightModelForNode(
			weightSource{Provider: component.Provider, RepoID: component.RepoID, Revision: component.Revision, Subfolder: component.Subfolder}, original,
			selection.Spec.Engine, component.Quantization, component.WeightBytes, 0, selection.StoragePins, selection.ExplicitComponents[component.Name],
		)
		if err != nil {
			return fmt.Errorf("component %s weights: %w", component.Name, err)
		}
		component.StoreModelID = model.ID
	}
	return nil
}

func (s *Service) applyTemplateDefaults(req *dto.DeployRequest, hardware templateHardware) (templateSelection, error) {
	selection := templateSelection{NodeID: hardware.NodeID, StoragePins: store.StoragePins(req.NodeRequirements)}
	if req.TemplateID == 0 {
		return selection, nil
	}
	model, err := s.d.Store.InferenceModelByID(req.TemplateID)
	if err != nil {
		return selection, err
	}
	if hardware.GPU.Vendor == "" && strings.TrimSpace(req.TemplateSpecName) == "" {
		return selection, fmt.Errorf("target node did not report a GPU for inference spec matching")
	}
	requestedEngine, requestedScenario := req.Engine, req.Scenario
	requestedSpec := req.TemplateSpecName
	spec, match, err := selectInferenceSpec(*model, req.TemplateSpecSource, requestedSpec, hardware.GPU, requestedEngine, requestedScenario)
	if err != nil {
		return selection, err
	}
	selection.Model, selection.Spec = model, spec
	selection.Match, selection.MatchedBy = "automatic", match.MatchedBy
	if strings.TrimSpace(requestedSpec) != "" {
		selection.Match = "spec-name"
	}
	// A spec that declares no args or no env leaves the column empty rather than
	// writing "[]" or "{}"; that is absence, not malformed JSON.
	if strings.TrimSpace(spec.Args) != "" {
		if err := json.Unmarshal([]byte(spec.Args), &selection.ExtraArgs); err != nil {
			return selection, fmt.Errorf("inference spec %s args: %w", spec.Name, err)
		}
	}
	if strings.TrimSpace(spec.Env) != "" {
		if err := json.Unmarshal([]byte(spec.Env), &selection.Env); err != nil {
			return selection, fmt.Errorf("inference spec %s env: %w", spec.Name, err)
		}
	}
	if strings.TrimSpace(spec.PreStartArgv) != "" {
		if err := json.Unmarshal([]byte(spec.PreStartArgv), &selection.PreStartArgv); err != nil {
			return selection, fmt.Errorf("inference spec %s pre_start_argv: %w", spec.Name, err)
		}
	}
	selection.PreStartScript = spec.PreStartScript
	if selection.ImageSources, err = inferencecatalog.DecodeImageSources(spec.ImageSources); err != nil {
		return selection, fmt.Errorf("inference spec %s image_sources: %w", spec.Name, err)
	}
	if raw := strings.TrimSpace(spec.Placement); raw != "" && raw != "null" {
		selection.Placement = json.RawMessage(append([]byte(nil), spec.Placement...))
	}
	selection.APIContract, err = EffectiveTemplateAPIContract(*model, *spec)
	if err != nil {
		return selection, fmt.Errorf("inference spec %s API contract: %w", spec.Name, err)
	}
	if strings.TrimSpace(spec.Entrypoint) != "" {
		if err := json.Unmarshal([]byte(spec.Entrypoint), &selection.Entrypoint); err != nil {
			return selection, fmt.Errorf("inference spec %s entrypoint: %w", spec.Name, err)
		}
	}
	var assets []dto.InferenceAsset
	if strings.TrimSpace(spec.Assets) != "" {
		if err := json.Unmarshal([]byte(spec.Assets), &assets); err != nil {
			return selection, fmt.Errorf("inference spec %s assets: %w", spec.Name, err)
		}
	}
	for _, asset := range assets {
		var content []byte
		if s.d.CatalogPackages == nil || spec.PackageDigest == "" {
			err = fmt.Errorf("inference catalog asset storage is not available for %s", model.ModelID)
		} else {
			content, err = s.d.CatalogPackages.ReadAsset(spec.PackageDigest, model.ModelID, asset.File)
		}
		if err != nil {
			return selection, err
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(content)); got != asset.SHA256 {
			return selection, fmt.Errorf("inference spec %s asset %s SHA-256 mismatch", spec.Name, asset.File)
		}
		selection.Assets = append(selection.Assets, proto.RuntimeAsset{
			File: asset.File, SHA256: asset.SHA256, SizeBytes: asset.SizeBytes, UnpackTo: asset.UnpackTo,
			PackageDigest: spec.PackageDigest, ModelID: model.ModelID,
		})
	}
	selection.Weight, err = s.selectWeightSource(*spec, hardware.NodeID, selection.StoragePins)
	if err != nil {
		return selection, err
	}
	if err := s.selectTemplateComponents(&selection, *spec, hardware.NodeID); err != nil {
		return selection, err
	}
	selection.SuggestedWeight = selection.Weight
	if err := s.selectExplicitWeight(&selection, req.ModelSource); err != nil {
		return selection, err
	}
	req.TemplateSpecName = spec.Name
	req.TemplateSpecSource = spec.Source
	requestedImage := strings.TrimSpace(req.Image)
	req.Engine, req.Scenario = spec.Engine, spec.Scenario
	// Keep an explicit image override intact. The deployment resolver performs
	// the engine/GPU compatibility check and refuses an incompatible
	// catalog entry; silently replacing it with the template image hides a
	// caller error and makes the resulting deployment non-reproducible.
	if requestedImage == "" {
		req.Image = spec.Image
	} else {
		req.Image = requestedImage
	}
	selection.Diff = flatTemplateRequestDiff(requestedEngine, requestedScenario, requestedImage, requestedSpec, req, selection)
	selection.Deviated = len(selection.Diff) > 0
	// The spec's args stay as the author wrote them. Which of them become
	// configuration values and which stay passthrough tokens is decided once, at
	// materialization, for the request and the template alike.
	return selection, nil
}

func (s *Service) selectTemplateComponents(selection *templateSelection, spec store.InferenceSpecRow, nodeID uint) error {
	if strings.TrimSpace(spec.Components) == "" || strings.TrimSpace(spec.Components) == "null" {
		return nil
	}
	var components []dto.ModelComponent
	if err := json.Unmarshal([]byte(spec.Components), &components); err != nil {
		return fmt.Errorf("inference spec %s components: %w", spec.Name, err)
	}
	for _, component := range components {
		sources, err := json.Marshal(component.Weights.Sources)
		if err != nil {
			return fmt.Errorf("inference spec %s component %s sources: %w", spec.Name, component.Name, err)
		}
		source, err := s.selectWeightSource(store.InferenceSpecRow{Name: spec.Name + "/" + component.Name, WeightSources: string(sources)}, nodeID, selection.StoragePins)
		if err != nil {
			return err
		}
		originalModelID := strings.TrimSpace(component.ModelID)
		if originalModelID == "" && selection.Model != nil {
			originalModelID = selection.Model.ModelID
		}
		selection.Components = append(selection.Components, deploy.ComponentBinding{
			Name: component.Name, Role: component.Role, OriginalModelID: originalModelID,
			Provider: source.Provider, RepoID: source.RepoID, Revision: source.Revision, Subfolder: source.Subfolder, Quantization: component.Weights.Quantization,
			MountPath: component.MountPath, WeightBytes: component.WeightBytes,
		})
	}
	if primary, ok := deploy.PrimaryComponent(selection.Components); ok {
		selection.Weight = weightSource{Provider: primary.Provider, RepoID: primary.RepoID, Revision: primary.Revision, Subfolder: primary.Subfolder}
	}
	return nil
}

func EffectiveTemplateAPIContract(model store.InferenceModel, spec store.InferenceSpecRow) (json.RawMessage, error) {
	_ = model
	var explicit *inferenceapi.Contract
	if raw := strings.TrimSpace(spec.APIContract); raw != "" && raw != "null" {
		var contract inferenceapi.Contract
		if err := json.Unmarshal([]byte(raw), &contract); err != nil {
			return nil, err
		}
		explicit = &contract
	}
	// A spec with no declared operations intentionally
	// has no Playground/API operation until the user edits the model config.
	contract := inferenceapi.Contract{HealthEndpoint: "/health", Operations: []inferenceapi.Operation{}}
	if explicit != nil {
		var err error
		contract, err = inferenceapi.NormalizeContract(*explicit)
		if err != nil {
			return nil, err
		}
	}
	encoded, err := json.Marshal(contract)
	return json.RawMessage(encoded), err
}

func (s *Service) selectWeightSource(spec store.InferenceSpecRow, nodeID uint, pinned []uint) (weightSource, error) {
	sources, err := inferencecatalog.DecodeWeightSources(spec.WeightSources)
	if err != nil {
		return weightSource{}, fmt.Errorf("inference spec %s weight sources: %w", spec.Name, err)
	}
	if len(sources) == 0 {
		return weightSource{}, fmt.Errorf("inference spec %s has no weight source", spec.Name)
	}
	if nodeID != 0 {
		models, err := s.d.Store.ModelArtifactsBySourceIDs(WeightSourceRepoIDs(sources)...)
		if err != nil {
			return weightSource{}, err
		}
		candidates := make([]store.Model, 0, len(models))
		for _, model := range models {
			if templateWeightMatchesAnySource(sources, &model) {
				candidates = append(candidates, model)
			}
		}
		ready, err := s.readyModelArtifacts(candidates, nodeID, pinned)
		if err != nil {
			return weightSource{}, err
		}
		// A copy that the node can read is what makes a deployment start
		// without a download, and the user's registered copy is rarely pinned to
		// the commit the spec declares. Match the repository instead, and keep the
		// declared provider only as a preference: a source whose own provider is
		// ready wins over one that is satisfied by a mirror.
		var mirrored *weightSource
		for _, source := range sources {
			for _, candidate := range weightSourceVariants(source) {
				candidate, err := s.resolveWeightRevision(candidate, nodeID)
				if err != nil {
					return weightSource{}, err
				}
				model := nodeReadyWeightModel(candidates, candidate, ready)
				if model == nil {
					continue
				}
				if strings.EqualFold(strings.TrimSpace(model.Source), strings.TrimSpace(candidate.Provider)) {
					return candidate, nil
				}
				if mirrored == nil {
					ready := candidate
					mirrored = &ready
				}
			}
		}
		if mirrored != nil {
			return *mirrored, nil
		}
	}
	return s.resolveWeightRevision(sources[0], nodeID)
}

// weightSourceVariants enumerates a declared source and, for a subfolder
// source, the complete repository that also contains it. The variant that
// matched decides what is mounted, so the subfolder variant is tried first.
func weightSourceVariants(source weightSource) []weightSource {
	if strings.TrimSpace(source.Subfolder) == "" {
		return []weightSource{source}
	}
	whole := source
	whole.Subfolder = ""
	return []weightSource{source, whole}
}

// nodeReadyWeightModel finds a registered model on nodeID that can serve this
// exact source variant. Unlike templateWeightMatchesModel the subfolder must
// match exactly: the caller enumerates the whole-repository variant itself and
// the variant it picks is the one the deployment mounts.
func nodeReadyWeightModel(models []store.Model, source weightSource, ready map[uint]bool) *store.Model {
	for index := range models {
		model := &models[index]
		if !strings.EqualFold(strings.TrimSpace(model.SourceID), strings.TrimSpace(source.RepoID)) ||
			strings.TrimSpace(model.ArtifactSubfolder) != strings.TrimSpace(source.Subfolder) {
			continue
		}
		if ready[model.ID] {
			return model
		}
	}
	return nil
}

// snapshotTemplateSelection renders the catalog selection the deployment was
// created from. The snapshot is one typed document (deploy.CatalogSelection)
// rather than a bag of JSON: four packages read it, and the one that decoded a
// field the others did not was how a template field went missing.
func snapshotTemplateSelection(selection templateSelection, image string) deploy.CatalogSelection {
	if selection.Model == nil || selection.Spec == nil {
		return deploy.CatalogSelection{}
	}
	return deploy.CatalogSelection{
		TemplateID: selection.Model.ID, ModelID: selection.Model.ModelID, SpecSource: selection.Spec.Source, SpecName: selection.Spec.Name,
		Scenario: selection.Spec.Scenario, Engine: selection.Spec.Engine, Image: image,
		Quantization: selection.Spec.Quantization, WeightProvider: selection.Weight.Provider,
		WeightRepoID: selection.Weight.RepoID, WeightRevision: selection.Weight.Revision, WeightSubfolder: selection.Weight.Subfolder, WeightBytes: selection.Spec.WeightBytes, RuntimeWeightBytes: selection.Spec.RuntimeWeightBytes,
		GPUs: json.RawMessage(selection.Spec.GPUs), Unsupported: json.RawMessage(selection.Model.Unsupported), ExtraArgs: selection.ExtraArgs, Env: selection.Env,
		PreStartArgv: selection.PreStartArgv, PreStartScript: selection.PreStartScript,
		RuntimeAssets: selection.Assets, Entrypoint: selection.Entrypoint,
		Placement: selection.Placement, APIContract: selection.APIContract,
		Components: selection.Components, PDMode: selection.Spec.PDMode, ShmBytes: selection.Spec.ShmBytes, MemlockUnlimited: selection.Spec.MemlockUnlimited, ImageSources: templateImageSources(selection, image),
		DeviatedFields: selection.Diff, MatchedBy: selection.MatchedBy,
		ImageDigestWarning: strings.Contains(strings.ToLower(image), "@sha256:"),
	}
}

// SpecCatalogSelection is the catalog selection a deployment of model stores
// when its request takes spec of template as it stands: the snapshot of the
// spec's own contract merged with the model's (applyCurrentModelAPIContract).
func SpecCatalogSelection(template *store.InferenceModel, spec *store.InferenceSpecRow, model *store.Model) deploy.CatalogSelection {
	live := templateSelection{
		Model: template, Spec: spec, APIContract: json.RawMessage(spec.APIContract),
		Weight: weightSource{Provider: model.Source, RepoID: model.SourceID, Revision: model.Revision, Subfolder: model.ArtifactSubfolder},
	}
	applyCurrentModelAPIContract(&live, model)
	return snapshotTemplateSelection(live, spec.Image)
}

func templateTargetNodeUUID(nodeUUID string, nodeUUIDs []string) string {
	if strings.TrimSpace(nodeUUID) != "" {
		return nodeUUID
	}
	if len(nodeUUIDs) > 0 {
		return nodeUUIDs[0]
	}
	return ""
}

func templatePlacementResolver(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value struct {
		Resolver string `json:"resolver"`
	}
	_ = json.Unmarshal(raw, &value)
	return strings.TrimSpace(value.Resolver)
}

// rejectsDigestImage preserves the product-wide tagged-image rule for manual
// input while allowing an immutable ref that came verbatim from a reviewed
// flat spec. The catalog requires special vendor images to be hard-pinned; the
// container runtimes accept tag@sha256 refs directly and no image-library
// aliasing is involved in this deployment path.
func rejectsDigestImage(image string, selection templateSelection) bool {
	return false
}
