package app

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/templateexport"
)

// catalogTemplates saves a running deployment as an inference template
// (api.CatalogTemplates): it reads the deployment from the store, tests the
// operations it serves through the gateway and builds the template.
type catalogTemplates struct {
	store *store.Store
	probe templateexport.Prober
}

func (c catalogTemplates) Export(ctx context.Context, deploymentID uint, includeEnv []string) (dto.CatalogTemplateExport, error) {
	source, err := loadTemplateSource(c.store, deploymentID, includeEnv)
	if err != nil {
		return dto.CatalogTemplateExport{}, err
	}
	// A gateway retry or another replica cannot establish facts about the
	// instance whose hardware and model copy this document describes.
	probe := c.probe
	if probe != nil {
		probe = func(ctx context.Context, id uint, instanceUUID, path string, payload map[string]any) templateexport.ProbeReply {
			reply := c.probe(ctx, id, instanceUUID, path, payload)
			if !reply.NoReplica && reply.InstanceUUID != source.instance.UUID && reply.Message == "" {
				reply.Message = "the probe did not complete on the instance being exported"
			}
			return reply
		}
	}
	if _, err := templateexport.Build(source.input); err != nil {
		return dto.CatalogTemplateExport{}, err
	}
	checks := templateexport.Checks(ctx, probe, deploymentID, source.instance.UUID, source.contract)
	after, err := loadTemplateSource(c.store, deploymentID, includeEnv)
	if err != nil {
		return dto.CatalogTemplateExport{}, err
	}
	if source.instance.UUID != after.instance.UUID || !reflect.DeepEqual(source.input, after.input) {
		return dto.CatalogTemplateExport{}, templateexport.Unavailable(templateexport.ReasonLaunchChanged, nil)
	}
	source.input.Checks = checks
	out, err := templateexport.Build(source.input)
	if err != nil {
		return dto.CatalogTemplateExport{}, err
	}
	return catalogTemplateDTO(out), nil
}

func catalogTemplateDTO(out templateexport.Export) dto.CatalogTemplateExport {
	result := dto.CatalogTemplateExport{
		ModelID: out.Model.ModelID, YAML: out.YAML, Verified: out.Verified, Saveable: out.Saveable,
		Checks: []dto.CatalogTemplateCheck{}, Notes: []messages.Warning{}, ExcludedEnv: []string{}, Evidence: out.Evidence,
	}
	if len(out.Model.Specs) > 0 {
		result.SpecName = out.Model.Specs[0].Name
	}
	for _, note := range out.Notes {
		result.Notes = append(result.Notes, messages.NewWarning(note.Code, note.Params))
	}
	for _, check := range out.Checks {
		result.Checks = append(result.Checks, dto.CatalogTemplateCheck{
			ID: check.ID, Operation: check.Operation, Status: check.Status, Code: check.Code,
			Params:  dto.CatalogTemplateCheckParams{Operation: check.Operation, Endpoint: check.Endpoint, HTTPStatus: check.HTTPStatus},
			Message: check.Message(), Evidence: check.Evidence,
		})
	}
	result.ExcludedEnv = append(result.ExcludedEnv, out.ExcludedEnv...)
	return result
}

// templateSource is a loaded deployment: the export input and the API
// contract its operation checks are chosen from.
type templateSource struct {
	input    templateexport.Input
	contract inferenceapi.Contract
	instance store.Instance
}

// loadTemplateSource reads deployment id from st. It reads one deployment,
// one running instance, its node and what that instance reads - never a list
// of deployments or nodes - because an export is a per-deployment action.
func loadTemplateSource(st *store.Store, id uint, includeEnv []string) (templateSource, error) {
	dep, err := st.GetDeployment(id)
	if err != nil {
		return templateSource{}, err
	}
	if dep.State != store.DeployActive {
		return templateSource{}, templateexport.Unavailable(templateexport.ReasonNotRunning, nil)
	}
	if dep.NNodes > 1 {
		return templateSource{}, templateexport.Unavailable(templateexport.ReasonDistributed, nil)
	}
	placement, err := planning.ParsePlacement(dep.Placement)
	if err != nil {
		return templateSource{}, fmt.Errorf("deployment %d placement: %w", dep.ID, err)
	}
	if placement.PD != nil {
		return templateSource{}, templateexport.Unavailable(templateexport.ReasonPD, nil)
	}
	overrides, err := deploy.DecodeContainerOverrides([]byte(dep.ContainerOverrides))
	if err != nil {
		return templateSource{}, fmt.Errorf("deployment %d container overrides: %w", dep.ID, err)
	}
	if overrides.EntrypointDisplay != "" || overrides.CmdDisplay != "" || overrides.CommandMode != "" {
		return templateSource{}, templateexport.Unavailable(templateexport.ReasonCommandOverridden, nil)
	}
	instance, err := runningTemplateInstance(st, dep.ID)
	if err != nil {
		return templateSource{}, err
	}
	in := templateexport.Input{
		DeploymentID: dep.ID, DeploymentName: dep.Name, Engine: dep.Engine, Image: instance.Image,
		UserEnv: overrides.Env, IncludeEnv: includeEnv, VRAMLimitBytes: dep.VRAMLimitBytes,
		HasContainerOverrides: len(overrides.ExtraMounts) > 0 || len(overrides.DockerArgs) > 0 || len(overrides.EnvUnset) > 0 ||
			overrides.ShmBytes > 0 || overrides.ModelMountPath != "",
	}
	if in.Image == "" {
		in.Image = dep.Image
	}
	if strings.TrimSpace(dep.QuickParams) != "" {
		if err := json.Unmarshal([]byte(dep.QuickParams), &in.Quick); err != nil {
			return templateSource{}, fmt.Errorf("deployment %d quick_params: %w", dep.ID, err)
		}
	}
	if in.UserArgs, err = deploy.ParseArgsText(dep.ExtraArgs); err != nil {
		return templateSource{}, fmt.Errorf("deployment %d extra_args: %w", dep.ID, err)
	}
	node, err := loadTemplateCards(st, instance, &in)
	if err != nil {
		return templateSource{}, err
	}
	model, err := st.GetModel(dep.ModelID)
	if err != nil {
		return templateSource{}, templateexport.Unavailable(templateexport.ReasonModelMissing, nil)
	}
	selection, err := planning.DeploymentTemplate(dep)
	if err != nil {
		return templateSource{}, err
	}
	contract := inferenceapi.ModelContract(model)
	if dep.TemplateID != 0 {
		if err := loadTemplateSpec(st, dep, selection, &in); err != nil {
			return templateSource{}, err
		}
		if len(selection.APIContract) > 0 {
			var declared inferenceapi.Contract
			if json.Unmarshal(selection.APIContract, &declared) == nil && len(declared.Operations) > 0 {
				if normalized, err := inferenceapi.NormalizeContract(declared); err == nil {
					contract = normalized
				}
			}
		}
	} else if err := loadTemplateModel(st, model, instance, &in); err != nil {
		return templateSource{}, err
	}
	if err := verifyTemplateLaunch(dep, instance, node, model, selection, &in); err != nil {
		return templateSource{}, err
	}
	// The running instance owns its executable API contract too. The model's
	// editable operations may already describe a later generation.
	var desired proto.InstanceDesiredState
	_ = json.Unmarshal([]byte(instance.DesiredStateJSON), &desired)
	if len(desired.APIContract) > 0 {
		if err := json.Unmarshal(desired.APIContract, &contract); err != nil {
			return templateSource{}, err
		}
	}
	return templateSource{input: in, contract: contract, instance: instance}, nil
}

// runningTemplateInstance is the deployment's first running instance. One is enough:
// every instance of a single-node deployment runs the same launch.
func runningTemplateInstance(st *store.Store, deploymentID uint) (store.Instance, error) {
	instances, err := st.InstancesForDeployment(deploymentID)
	if err != nil {
		return store.Instance{}, err
	}
	for _, instance := range instances {
		if instance.State == proto.InstRunning {
			return instance, nil
		}
	}
	return store.Instance{}, templateexport.Unavailable(templateexport.ReasonNotRunning, nil)
}

// loadTemplateCards reads the cards instance holds from its node, by the hardware
// identity it is bound to.
func loadTemplateCards(st *store.Store, instance store.Instance, in *templateexport.Input) (*store.Node, error) {
	var idents []string
	if strings.TrimSpace(instance.GPUIdents) != "" {
		if err := json.Unmarshal([]byte(instance.GPUIdents), &idents); err != nil {
			return nil, fmt.Errorf("instance %s gpu_idents: %w", instance.UUID, err)
		}
	}
	if len(idents) == 0 {
		return nil, templateexport.Unavailable(templateexport.ReasonNoGPUFacts, nil)
	}
	node, err := st.NodeByID(instance.NodeID)
	if err != nil {
		return nil, templateexport.Unavailable(templateexport.ReasonNoGPUFacts, nil)
	}
	held := map[string]bool{}
	for _, ident := range idents {
		held[ident] = true
	}
	driver := node.NvidiaDriver
	if driver == "" {
		driver = node.GPUDriver
	}
	for _, card := range node.GPUs {
		if held[card.Ident] {
			in.GPUs = append(in.GPUs, templateexport.GPUFact{
				Name: card.Name, Vendor: card.Vendor, MemoryMiB: card.MemoryTotal >> 20,
				ComputeCap: card.ComputeCapability, DriverVersion: driver,
			})
		}
	}
	if len(in.GPUs) != len(idents) || len(held) != len(idents) {
		return nil, templateexport.Unavailable(templateexport.ReasonNoGPUFacts, nil)
	}
	in.GPUCount = len(idents)
	return node, nil
}

// loadTemplateSpec reads the catalog spec the deployment was created from and
// the template layers its snapshot recorded.
func loadTemplateSpec(st *store.Store, dep *store.Deployment, selection deploy.CatalogSelection, in *templateexport.Input) error {
	row, err := st.InferenceModelByID(dep.TemplateID)
	if err != nil {
		return templateexport.Unavailable(templateexport.ReasonTemplateMissing, nil)
	}
	template, err := inferencecatalog.FromFlatStoreModel(*row)
	if err != nil {
		return err
	}
	template.Unsupported = nil
	if len(selection.Unsupported) > 0 {
		if err := json.Unmarshal(selection.Unsupported, &template.Unsupported); err != nil {
			return err
		}
	}
	spec, err := templateSpecFromSnapshot(selection, in.Image)
	if err != nil {
		return err
	}
	in.Template, in.TemplateSpec = &template, &spec
	if selection.ModelID != "" {
		in.Template.ModelID = selection.ModelID
	}
	in.TemplateArgs = append([]string(nil), selection.ExtraArgs...)
	in.TemplateEnv = selection.Env
	return nil
}

// loadTemplateModel reads the facts of a model the catalog may not have: its row,
// and the copies the running instance reads, which say which commit it ran.
func loadTemplateModel(st *store.Store, model *store.Model, instance store.Instance, in *templateexport.Input) error {
	facts := templateexport.ModelFacts{
		Source: model.Source, RepoID: model.SourceID, Subfolder: model.ArtifactSubfolder, Name: model.Name,
		Quantization: firstNonEmptyText(model.QuantLabel, model.Precision, model.Dtype),
		WeightBytes:  model.WeightBytes, ContextLength: model.ContextLength, ConfigJSON: model.Meta,
		Operations: inferenceapi.ModelContract(model).Operations,
	}
	if strings.TrimSpace(model.InputModalities) != "" {
		if err := json.Unmarshal([]byte(model.InputModalities), &facts.InputModalities); err != nil {
			return fmt.Errorf("model %d input_modalities: %w", model.ID, err)
		}
	}
	reads, err := st.InstanceModelFiles([]uint{instance.ID})
	if err != nil {
		return err
	}
	for _, read := range reads[instance.ID] {
		if read.ModelID != model.ID {
			continue
		}
		file, err := st.ModelFileByID(read.ModelFileID)
		if err != nil {
			return err
		}
		facts.ResolvedRevisions = append(facts.ResolvedRevisions, file.Revision)
		if facts.WeightBytes == 0 {
			facts.WeightBytes = file.SizeBytes
		}
	}
	if len(facts.ResolvedRevisions) == 0 {
		facts.ResolvedRevisions = []string{model.Revision}
	}
	in.Model = facts
	in.KVBytesPerToken = planning.CatalogKVBytesPerToken(st, nil, model, in.Engine)
	return nil
}

func firstNonEmptyText(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
