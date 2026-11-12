package deployment

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// materializeDeployRequest turns a deploy request into the one launch
// configuration everything downstream reads: the estimator, the argv the engine
// receives, what the form shows back and what a create stores. Evaluate and
// create both go through it, so a preview cannot describe a different launch
// from the one it previews. engineVersion is the release of the image the
// request runs (requestEngineVersion): a flag that release may refuse gets no
// default.
func materializeDeployRequest(engine string, req *dto.DeployRequest, selection templateSelection, cards []deploy.CardFacts, engineVersion string) (deploy.Materialized, error) {
	extra, err := deploy.ParseArgsText(req.ExtraArgs)
	if err != nil {
		return deploy.Materialized{}, wrapProductError(messages.ErrorExtraArgsInvalid, "extra_args cannot be parsed", map[string]any{"detail": err.Error()}, err)
	}
	if err := deploy.ValidateExtraArgsJSON(extra); err != nil {
		return deploy.Materialized{}, err
	}
	command, err := effectiveRequestCommand(req, selection)
	if err != nil {
		return deploy.Materialized{}, err
	}
	templateArgs := selection.ExtraArgs
	// Task intent comes from the current explicit API contract, never a model
	// name or an architecture guess. A full command/entrypoint remains owned
	// by its author; only the ordinary engine-generated command gets defaults.
	cmdDisplay, _ := req.ContainerOverrides["cmd_display"].(string)
	if command.InterpretEngineDialect && len(command.Entrypoint) == 0 && strings.TrimSpace(cmdDisplay) == "" {
		defaults := deploy.PoolingTaskArgs(engine, engineVersion, selection.APIContract, templateArgs, extra)
		if selection.Spec != nil {
			templateArgs = append(append([]string(nil), templateArgs...), defaults...)
		} else {
			// Raw-model deployments have no catalog snapshot: keep the derived
			// tokens in their existing persisted extra_args launch column.
			extra = append(extra, defaults...)
		}
	}
	out, err := deploy.Materialize(deploy.MaterializeInput{
		Engine: engine, Command: &command, Raw: req.QuickParams, Unset: req.QuickParamsUnset, TemplateArgs: templateArgs, ExtraArgs: extra, Cards: cards,
		EngineVersion: engineVersion,
	})
	if err != nil {
		return deploy.Materialized{}, err
	}
	if err := ValidateRequestedLaunchConfig(engine, req, extra); err != nil {
		return deploy.Materialized{}, err
	}
	return out, nil
}

func effectiveRequestCommand(req *dto.DeployRequest, selection templateSelection) (deploy.EffectiveCommand, error) {
	overrides, err := RequestContainerOverrides(req.ContainerOverrides)
	if err != nil {
		return deploy.EffectiveCommand{}, err
	}
	return deploy.ResolveEffectiveCommand(overrides, selection.Entrypoint)
}

// ValidateRequestedLaunchConfig checks what this request stated - its form
// fields and its extra_args, including the values lifted out of them - against
// the parameter dictionary and the cross-field rules.
//
// A catalog spec's own arguments are deliberately not held to those rules. They
// are a curated artifact whose author writes the upstream recipe verbatim (GLM
// 4.5V's `--allowed-local-media-path /` is the upstream recipe and the form's
// rule forbids it), and the server has always passed them through. ADR 0020
// gives catalog packages their own import-time check, which is where a spec the
// dictionary cannot decode is rejected; making a curated recipe fail at deploy
// time would only move that report to the worst possible moment.
func ValidateRequestedLaunchConfig(engine string, req *dto.DeployRequest, extra []string) error {
	stated, err := deploy.Materialize(deploy.MaterializeInput{Engine: engine, Raw: req.QuickParams, Unset: req.QuickParamsUnset, ExtraArgs: extra})
	if err != nil {
		return err
	}
	if err := deploy.ValidateLaunchConfigForEngine(engine, stated.Quick); err != nil {
		return fmt.Errorf("invalid quick_params: %w", err)
	}
	return nil
}

// launchConfigMap renders a launch configuration as the object clients read and
// the deployment row stores. Going through the configuration's own JSON keeps
// the wire keys and the stored column identical by construction.
func launchConfigMap(quick deploy.LaunchConfig) map[string]any {
	encoded, err := json.Marshal(quick)
	if err != nil {
		return nil
	}
	values := map[string]any{}
	if err := json.Unmarshal(encoded, &values); err != nil {
		return nil
	}
	return values
}

func LaunchConfigJSON(quick deploy.LaunchConfig) string {
	encoded, err := json.Marshal(quick)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// catalogSelectionDocument renders the stored form of the selection a create
// request made. Where its model and image come from is not part of it: each
// node decides that when it starts.
func catalogSelectionDocument(selection templateSelection, image string) (string, error) {
	snapshot := snapshotTemplateSelection(selection, image)
	encoded, err := snapshot.Encode()
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// flatTemplateRequestDiff reports which parts of a request depart from the
// catalog spec it selected. Parameters are compared as configurations rather
// than as argument text: the same value can be written as a form field or as a
// spec argument, and a deployment that says exactly what its template says has
// not deviated no matter which spelling it used.
func flatTemplateRequestDiff(requestedEngine, requestedScenario, requestedImage, requestedSpec string, req *dto.DeployRequest, selection templateSelection) []string {
	var diff []string
	if requestedEngine != "" && !strings.EqualFold(requestedEngine, selection.Spec.Engine) {
		diff = append(diff, "engine")
	}
	if requestedScenario != "" && !strings.EqualFold(requestedScenario, selection.Spec.Scenario) {
		diff = append(diff, "scenario")
	}
	if requestedImage != "" && requestedImage != selection.Spec.Image {
		diff = append(diff, "image")
	}
	if requestedSpec != "" && requestedSpec != selection.Spec.Name {
		diff = append(diff, "spec")
	}
	if supplied, ok := req.ContainerOverrides["env"]; ok {
		var requestedEnv map[string]string
		if raw, err := json.Marshal(supplied); err == nil {
			_ = json.Unmarshal(raw, &requestedEnv)
		}
		if !reflect.DeepEqual(requestedEnv, selection.Env) {
			diff = append(diff, "env")
		}
	}
	diff = append(diff, launchConfigDiff(req, selection)...)
	sort.Strings(diff)
	return diff
}

// launchConfigDiff compares the request's launch configuration against the one
// the template alone produces. A failure to materialize is itself a deviation
// worth reporting: the request is about to be rejected, and claiming it matches
// the template would be the one answer that is certainly wrong.
func launchConfigDiff(req *dto.DeployRequest, selection templateSelection) []string {
	engine := selection.Spec.Engine
	// Both sides run the template's image as far as the question goes: a
	// request that picked another image already reports "image".
	engineVersion := deploy.ImageEngineVersion(engine, selection.Spec.Image, "")
	// Neither side is given cards. The question here is whether the deployer
	// said something the template did not, and a default derived from the
	// card they target is not something they said - capping one side and not
	// the other would report every small card as a deviation.
	requested, err := materializeDeployRequest(engine, req, selection, nil, engineVersion)
	if err != nil {
		return []string{"params"}
	}
	templateCommand, commandErr := deploy.ResolveEffectiveCommand(deploy.ContainerOverrides{}, selection.Entrypoint)
	if commandErr != nil {
		return []string{"params"}
	}
	template, err := deploy.Materialize(deploy.MaterializeInput{Engine: engine, Command: &templateCommand, TemplateArgs: selection.ExtraArgs, EngineVersion: engineVersion})
	if err != nil {
		return []string{"params"}
	}
	var diff []string
	if LaunchConfigJSON(requested.Quick) != LaunchConfigJSON(template.Quick) {
		diff = append(diff, "params")
	}
	if len(requested.UserResidual) > 0 {
		diff = append(diff, "extra_args")
	}
	return diff
}

// imageEngineVersion is the engine release an image runs: what the image
// catalog records for it, otherwise what its tag says
// (deploy.ImageEngineVersion).
func (s *Service) imageEngineVersion(engine, image string) string {
	catalogVersion := ""
	if row, err := s.d.Store.ResolveImageRef(image); err == nil {
		catalogVersion = row.EngineVersion
	}
	return deploy.ImageEngineVersion(engine, image, catalogVersion)
}

// requestEngineVersion is the engine release a deploy request will run, as far
// as it can be told before placement: that of the image the request names,
// otherwise of the image catalog's pick for the node it targets, or for the
// vendor while it names no node - the resolution the preview and the create
// repeat once the node is chosen.
func (s *Service) requestEngineVersion(engine string, req *dto.DeployRequest, selection templateSelection, targetVendor string, node *store.Node) string {
	target := deploymentImageTarget(engine, targetVendor, node)
	target.Override = req.Image
	target.TrustedOverride = templateOwnsImage(selection, req.Image)
	return s.imageEngineVersion(engine, imagecatalog.Resolve(s.d.Store, target).Image)
}

// DeployTargetCards are the accelerators a request's card-dependent defaults
// have to fit: the cards it pinned, or every healthy card of the node it
// targets when it pinned none. An unhealthy card is left out because no
// deployment can land on one, and counting it would reserve against a card
// that will never run anything.
//
// Nothing is returned for a request with no node yet (the automatic mode
// decides one after submission), which leaves the dictionary's own defaults in
// place - the honest answer when the hardware is not known.
func DeployTargetCards(node *store.Node, placement any) []deploy.CardFacts {
	if node == nil {
		return nil
	}
	pinned, err := planning.ParsePlacement(placement)
	if err == nil && len(pinned.GPUIndexes) > 0 {
		cards := make([]deploy.CardFacts, 0, len(pinned.GPUIndexes))
		for _, index := range pinned.GPUIndexes {
			if gpu, ok := node.GPUByIndex(index); ok {
				cards = append(cards, deploy.CardFacts{Name: gpu.Name, MemoryTotalBytes: gpu.MemoryTotal})
			}
		}
		if len(cards) > 0 {
			return cards
		}
	}
	owned := node.OwnedGPUs()
	cards := make([]deploy.CardFacts, 0, len(owned))
	for _, gpu := range owned {
		if gpu.CanTakeWork() {
			cards = append(cards, deploy.CardFacts{Name: gpu.Name, MemoryTotalBytes: gpu.MemoryTotal})
		}
	}
	return cards
}
