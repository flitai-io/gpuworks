package inferencecatalog

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/messages"
	placementeval "gpuworks.ai/gpuworks/internal/server/placement"
)

type Asset struct {
	File      string `yaml:"file" json:"file"`
	SHA256    string `yaml:"sha256" json:"sha256"`
	SizeBytes int64  `yaml:"size_bytes,omitempty" json:"size_bytes,omitempty"`
	UnpackTo  string `yaml:"unpack_to,omitempty" json:"unpack_to,omitempty"`
}

type Placement struct {
	Language string `yaml:"language" json:"language"`
	Resolver string `yaml:"resolver" json:"resolver"`
}

type APIContract = inferenceapi.Contract
type APIOperation = inferenceapi.Operation

type Component struct {
	Name        string  `yaml:"name" json:"name"`
	Role        string  `yaml:"role" json:"role"`
	ModelID     string  `yaml:"model_id,omitempty" json:"model_id,omitempty"`
	Weights     Weights `yaml:"weights" json:"weights"`
	WeightBytes int64   `yaml:"weight_bytes" json:"weight_bytes"`
	MountPath   string  `yaml:"mount_path" json:"mount_path"`
}

var sha256Expression = regexp.MustCompile(`^[0-9a-f]{64}$`)

// pluginArgumentExpression matches the launch flags whose value names a Python
// plugin file the engine imports at startup: vLLM's --reasoning-parser-plugin
// and --tool-call-parser-plugin today, any later --*-plugin of the same shape
// tomorrow.
var pluginArgumentExpression = regexp.MustCompile(`^--[a-z0-9]+(?:-[a-z0-9]+)*-plugin$`)

// containerWorkingDirectory is the directory the engine resolves a relative
// argument value against. The platform does not make it configurable, and the
// engine reports the path it actually opened, which is how the defect this
// check exists for surfaced:
//
//	FileNotFoundError: [Errno 2] No such file or directory: '/nano_v3_reasoning_parser.py'
//
// for the relative argument value nano_v3_reasoning_parser.py.
const containerWorkingDirectory = "/"

func normalizeInputModalities(modelID string, values []string) ([]string, error) {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "text" && value != "image" && value != "audio" && value != "video" {
			return nil, fmt.Errorf("model %s has invalid input_modality %q", modelID, value)
		}
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result, nil
}

func validateExecutableContract(modelID string, spec *InferenceSpec) error {
	where := fmt.Sprintf("model %s spec %s", modelID, spec.Name)
	seenAssets := map[string]struct{}{}
	for index := range spec.Assets {
		asset := &spec.Assets[index]
		asset.File = path.Clean(strings.TrimSpace(strings.ReplaceAll(asset.File, "\\", "/")))
		asset.SHA256 = strings.ToLower(strings.TrimSpace(asset.SHA256))
		asset.UnpackTo = strings.TrimSpace(asset.UnpackTo)
		if asset.SizeBytes < 0 {
			return fmt.Errorf("%s asset %s has negative size_bytes", where, asset.File)
		}
		if asset.File == "." || !strings.HasPrefix(asset.File, "assets/") || !validRelativePath(asset.File) {
			return fmt.Errorf("%s asset %d must reference its own assets/ directory", where, index)
		}
		if !sha256Expression.MatchString(asset.SHA256) {
			return fmt.Errorf("%s asset %s requires a lowercase SHA-256", where, asset.File)
		}
		if _, duplicate := seenAssets[asset.File]; duplicate {
			return fmt.Errorf("%s has duplicate asset %s", where, asset.File)
		}
		seenAssets[asset.File] = struct{}{}
		if !validContainerPath(asset.UnpackTo) {
			return fmt.Errorf("%s asset %s has invalid unpack_to %q", where, asset.File, asset.UnpackTo)
		}
	}
	if err := validateFileValuedArguments(where, modelID, spec); err != nil {
		return err
	}
	for index, value := range spec.Entrypoint {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s entrypoint[%d] is empty", where, index)
		}
	}
	if spec.Placement != nil {
		spec.Placement.Language = strings.ToLower(strings.TrimSpace(spec.Placement.Language))
		if spec.Placement.Language != "starlark" || strings.TrimSpace(spec.Placement.Resolver) == "" {
			return fmt.Errorf("%s placement requires a Starlark resolver", where)
		}
		if len(spec.Placement.Resolver) > 32*1024 {
			return fmt.Errorf("%s placement resolver exceeds 32 KiB", where)
		}
		if err := placementeval.Compile(spec.Placement.Resolver); err != nil {
			return fmt.Errorf("%s placement resolver: %w", where, err)
		}
	}
	if err := validateTemplateVariables(where, spec); err != nil {
		return err
	}
	if spec.API != nil {
		if err := validateAPIContract(where, spec.API); err != nil {
			return err
		}
	}
	return validateComponents(where, spec)
}

// validateTemplateVariables refuses, at import, a launch no deployment could
// expand. An engine's required variable (the callable adapter's ${API_PORT})
// has to appear somewhere in the launch fields. Without a placement resolver
// the platform's vocabulary is every name there is, so an argv or env value
// naming anything else is a typo found now instead of at launch; the
// preparation script is a shell's, and its other names are the shell's.
func validateTemplateVariables(where string, spec *InferenceSpec) error {
	strict := append(append(append([]string{}, spec.Args...), spec.Entrypoint...), spec.PreStartArgv...)
	for _, value := range spec.Env {
		strict = append(strict, value)
	}
	referenced := map[string]struct{}{}
	for _, name := range placementeval.References(append(strict, spec.PreStartScript)...) {
		referenced[name] = struct{}{}
	}
	for _, name := range deploy.RequiredTemplateVariables(spec.Engine) {
		if _, ok := referenced[name]; !ok {
			return fmt.Errorf("%s engine %s must reference ${%s}: the engine adds no platform argument of its own", where, spec.Engine, name)
		}
	}
	if spec.Placement != nil {
		return nil
	}
	for _, name := range placementeval.References(strict...) {
		if !placementeval.PlatformVariable(name) {
			return fmt.Errorf("%s references ${%s}, which no placement resolver returns and the platform does not state", where, name)
		}
	}
	return nil
}

// validateFileValuedArguments rejects a spec that hands the engine a plugin
// file no declaration inside the spec can supply. It is a ProductError so the
// catalog importer, the release tooling and the API all report the same stable
// code for a package that cannot start under any image.
//
// Deliberate limits. Neither the image's contents nor the weights repository's
// are in the control plane's hands, so this check is not complete and cannot be
// made complete by static analysis:
//
//   - only a relative value can be judged at all. The engine resolves a
//     relative value against the container working directory, which the
//     platform fixes at /, so a relative value names a path at the container
//     root that has to come from the spec itself (asset, mount or start
//     script). Nothing else can put it there;
//   - an absolute value is accepted on its form alone. It may name a file the
//     image ships, an asset's unpack_to, a component mount - or nothing at
//     all. Telling those apart needs the image, which only L2/L3 probing can
//     load, so an absolute path stays a blind spot here by construction;
//   - --chat-template and the other template-valued flags are out of scope.
//     The catalog's template values are engine-relative paths such as
//     examples/tool_chat_template_gemma4.jinja that point inside an engine
//     image, so the same rule would reject specs whose file the image does
//     supply. Those need the same L2/L3 evidence a plugin file needs.
//
// What it does catch is the shape that produced this defect and cannot start
// under any image: a value that is neither absolute nor supplied by the spec's
// own assets, mounts or start script.
func validateFileValuedArguments(where, modelID string, spec *InferenceSpec) error {
	flag, value, unresolved := UnresolvedFileValuedArgument(spec)
	if !unresolved {
		return nil
	}
	return messages.NewProductError(
		messages.ErrorCatalogFileValuedArgUnresolved,
		fmt.Sprintf("%s: %s value %q is supplied by no image, asset, mount or start script of the spec", where, flag, value),
		map[string]any{"model": modelID, "spec": spec.Name, "flag": flag, "value": value},
	)
}

// UnresolvedFileValuedArgument reports the first launch argument in spec whose
// value names a plugin file that nothing inside the spec supplies, together
// with the flag and the value that failed. It is exported because the seed
// catalog contract test asserts the property over the embedded catalog
// directly, independently of the load path that ValidateFlatCatalog takes.
//
// An empty value means the flag was the last token of args, which is
// unresolvable for the same reason: there is no file to load.
func UnresolvedFileValuedArgument(spec *InferenceSpec) (flag, value string, unresolved bool) {
	if spec == nil {
		return "", "", false
	}
	for index := 0; index < len(spec.Args); index++ {
		name, inlineValue, inline := splitInlineFlag(spec.Args[index])
		if !pluginArgumentExpression.MatchString(name) {
			continue
		}
		resolved := strings.TrimSpace(inlineValue)
		if !inline {
			if index+1 >= len(spec.Args) {
				return name, "", true
			}
			index++
			resolved = strings.TrimSpace(spec.Args[index])
		}
		if !fileValuedArgumentIsSupplied(spec, resolved) {
			return name, resolved, true
		}
	}
	return "", "", false
}

// fileValuedArgumentIsSupplied reports whether value names a file that is
// somewhere the engine will look for it, judged from the spec alone.
func fileValuedArgumentIsSupplied(spec *InferenceSpec, value string) bool {
	if value == "" {
		return false
	}
	// An absolute path is the image's, an asset's or a mount's to supply. See
	// the limits in validateFileValuedArguments: this branch is the check's
	// deliberate blind spot, not a judgement that the file exists.
	if path.IsAbs(value) {
		return true
	}
	target := path.Join(containerWorkingDirectory, value)
	if target == containerWorkingDirectory {
		return false
	}
	directory := path.Dir(target)
	for index := range spec.Assets {
		if directoryWithin(spec.Assets[index].UnpackTo, directory) {
			return true
		}
	}
	for index := range spec.Components {
		if directoryWithin(spec.Components[index].MountPath, directory) {
			return true
		}
	}
	// A start script or entrypoint that names the file is textual evidence
	// that the spec materializes it. It is evidence, not proof: the platform
	// cannot execute the script to find out what it does.
	base := path.Base(target)
	for _, text := range []string{spec.PreStartScript, strings.Join(spec.PreStartArgv, " "), strings.Join(spec.Entrypoint, " ")} {
		if base != "" && strings.Contains(text, base) {
			return true
		}
	}
	return false
}

// directoryWithin reports whether target is root or lies below it. Both values
// are container paths.
func directoryWithin(root, target string) bool {
	if root == "" || !strings.HasPrefix(root, "/") {
		return false
	}
	return target == root || strings.HasPrefix(target, strings.TrimSuffix(root, "/")+"/")
}

func validateAPIContract(where string, contract *APIContract) error {
	normalized, err := inferenceapi.NormalizeContract(*contract)
	if err != nil {
		return fmt.Errorf("%s %w", where, err)
	}
	*contract = normalized
	return nil
}

func validateComponents(where string, spec *InferenceSpec) error {
	if len(spec.Components) == 0 {
		return nil
	}
	seenNames, seenMounts := map[string]struct{}{}, map[string]struct{}{}
	primary := 0
	for index := range spec.Components {
		component := &spec.Components[index]
		component.Name = strings.ToLower(strings.TrimSpace(component.Name))
		component.Role = strings.ToLower(strings.TrimSpace(component.Role))
		component.ModelID = strings.ToLower(strings.TrimSpace(component.ModelID))
		component.MountPath = strings.TrimSpace(component.MountPath)
		if component.Name == "" {
			return fmt.Errorf("%s component %d requires name", where, index)
		}
		if _, duplicate := seenNames[component.Name]; duplicate {
			return fmt.Errorf("%s has duplicate component name %q", where, component.Name)
		}
		seenNames[component.Name] = struct{}{}
		switch component.Role {
		case "primary":
			primary++
		case "speculative_draft", "verifier":
			if component.ModelID == "" {
				return fmt.Errorf("%s component %s requires model_id", where, component.Name)
			}
		case "auxiliary":
			// Weights the engine reads beside the primary model (IndexTTS 2.5's
			// Wav2Vec2-BERT, CAMPPlus and BigVGAN). They are not a catalog model of
			// their own, so model_id is optional: without one the component
			// resolves against the spec's own model, as the primary does.
		default:
			return fmt.Errorf("%s component %s has invalid role %q", where, component.Name, component.Role)
		}
		if err := validateWeights(where, component.Name, &component.Weights); err != nil {
			return err
		}
		if component.WeightBytes <= 0 {
			return fmt.Errorf("%s component %s requires positive weight_bytes", where, component.Name)
		}
		if !validContainerPath(component.MountPath) {
			return fmt.Errorf("%s component %s has invalid mount_path %q", where, component.Name, component.MountPath)
		}
		if _, duplicate := seenMounts[component.MountPath]; duplicate {
			return fmt.Errorf("%s has duplicate component mount_path %q", where, component.MountPath)
		}
		for existing := range seenMounts {
			if strings.HasPrefix(component.MountPath, existing+"/") || strings.HasPrefix(existing, component.MountPath+"/") {
				return fmt.Errorf("%s has nested component mount_paths %q and %q", where, existing, component.MountPath)
			}
		}
		seenMounts[component.MountPath] = struct{}{}
	}
	if primary != 1 {
		return fmt.Errorf("%s components require exactly one primary, got %d", where, primary)
	}
	launchValues := append(append(append([]string{}, spec.Args...), spec.Entrypoint...), spec.PreStartArgv...)
	launchValues = append(launchValues, spec.PreStartScript)
	for key, value := range spec.Env {
		launchValues = append(launchValues, key, value)
	}
	launchText := strings.Join(launchValues, "\x00")
	for _, component := range spec.Components {
		if component.Role != "primary" && !strings.Contains(launchText, component.MountPath) {
			return fmt.Errorf("%s component %s mount_path %q is not referenced by the launch contract", where, component.Name, component.MountPath)
		}
	}
	return nil
}

func validateComponentReferences(catalog FlatCatalog) error {
	models := make(map[string]struct{}, len(catalog.Models))
	for _, model := range catalog.Models {
		models[model.ModelID] = struct{}{}
	}
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			for _, component := range spec.Components {
				if component.ModelID == "" {
					continue
				}
				if _, ok := models[component.ModelID]; !ok {
					return fmt.Errorf("model %s spec %s component %s references missing original model %s", model.ModelID, spec.Name, component.Name, component.ModelID)
				}
			}
		}
	}
	return nil
}

// validateComponentOnlyModels permits a draft/verifier artifact to remain an
// independently addressable original model even when it cannot serve alone.
// Empty spec lists are otherwise rejected so ordinary undeployable rows cannot
// silently enter the product catalog.
func validateComponentOnlyModels(catalog FlatCatalog) error {
	referenced := make(map[string]struct{})
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			for _, component := range spec.Components {
				if component.ModelID != "" {
					referenced[component.ModelID] = struct{}{}
				}
			}
		}
	}
	for _, model := range catalog.Models {
		if len(model.Specs) != 0 {
			continue
		}
		if _, ok := referenced[model.ModelID]; !ok {
			return fmt.Errorf("model %s requires at least one spec unless referenced as a runtime component", model.ModelID)
		}
	}
	return nil
}

func validRelativePath(value string) bool {
	return value != "." && !path.IsAbs(value) && path.Clean(value) == value && !strings.HasPrefix(value, "../")
}

func validContainerPath(value string) bool {
	return strings.HasPrefix(value, "/") && path.Clean(value) == value && value != "/"
}

// splitInlineFlag separates a "--flag=value" token. It is spelled out here
// because the catalog reads argv only to find plugin file arguments; turning a
// spec's args into parameter values is the deploy package's job.
func splitInlineFlag(token string) (string, string, bool) {
	if index := strings.IndexByte(token, '='); index > 0 && strings.HasPrefix(token, "-") {
		return token[:index], token[index+1:], true
	}
	return token, "", false
}
