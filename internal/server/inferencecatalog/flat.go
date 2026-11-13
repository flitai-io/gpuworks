package inferencecatalog

// This file contains the flat catalog format introduced by ADR 0009: a list
// of original models and executable specs, not a compatibility report.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/pdmode"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/pkg/modelconfig"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// FlatCatalog is the on-disk/user-facing YAML document.
type FlatCatalog struct {
	Models []InferenceModel `yaml:"models" json:"models"`
}

// InferenceModel is the original model row and its executable specs.
type InferenceModel struct {
	ModelID         string   `yaml:"model_id" json:"model_id"`
	Source          string   `yaml:"source,omitempty" json:"source,omitempty"`
	DisplayTitle    string   `yaml:"display_title,omitempty" json:"display_title,omitempty"`
	ParameterCount  string   `yaml:"parameter_count,omitempty" json:"parameter_count,omitempty"`
	Architecture    string   `yaml:"architecture,omitempty" json:"architecture,omitempty"`
	InputModalities []string `yaml:"input_modalities,omitempty" json:"input_modalities,omitempty"`
	// ModelConfig is the estimation-relevant subset of the model's config.json,
	// written in config.json's own field names. It is what lets the KV-cache and
	// VRAM estimate run for a preset model without fetching the repository: a
	// gated or relocated repository can never be read by the server, and a
	// repository whose config.json omits its attention shape (or that has no
	// config.json at all, like a diffusion pipeline) would otherwise have no
	// estimate. A model with no token-indexed KV cache declares kv_cache: false.
	// The same shape serves both weight providers: ModelScope publishes the
	// HuggingFace config.json layout unchanged.
	ModelConfig *modelconfig.Config `yaml:"model_config,omitempty" json:"model_config,omitempty"`
	ReadmeZHCN  string              `yaml:"readme_zh_cn,omitempty" json:"readme_zh_cn,omitempty"`
	ReadmeEN    string              `yaml:"readme_en,omitempty" json:"readme_en,omitempty"`
	// KVBytesPerToken is the KV cache this model costs per token across the
	// whole tensor-parallel group, keyed by serving engine.
	//
	// It is the engine's own number, not our derivation of it, and it is keyed
	// because the engines genuinely differ: vLLM pads a recurrent layer's page up
	// to the attention page size and so charges every layer of a Mamba or
	// gated-linear hybrid - Qwen3.5-0.8B measured 49,152 B/token - while SGLang
	// counts only mambaish.full_attention_layer_ids and charges the attention
	// layers alone, 12,284 on the same model. Models without recurrent layers
	// agree exactly.
	//
	// Each value is produced by asking the engine: vLLM's own layout functions
	// (is_deepseek_mla / get_head_size / get_total_num_kv_heads) applied to the
	// model's config.json, which is a CPU computation needing no GPU, or a
	// --load-format dummy run, which gives the same figure. A missing engine key
	// means the catalog carries no value for it, and the deploy form then reports
	// no token capacity rather than a wrong one.
	KVBytesPerToken map[string]int64 `yaml:"kv_bytes_per_token,omitempty" json:"kv_bytes_per_token,omitempty"`
	ContextLength   int64            `yaml:"context_length,omitempty" json:"context_length,omitempty"`
	ReleaseDate     string           `yaml:"release_date,omitempty" json:"release_date,omitempty"`
	Unsupported     []UnsupportedGPU `yaml:"unsupported,omitempty" json:"unsupported,omitempty"`
	Specs           []InferenceSpec  `yaml:"specs" json:"specs"`
}

// InferenceSpec is one complete, deployable configuration.
type InferenceSpec struct {
	Name               string            `yaml:"name" json:"name"`
	DisplayNameZHCN    string            `yaml:"display_name_zh_cn,omitempty" json:"display_name_zh_cn,omitempty"` // see DisplayNameEN
	DisplayNameEN      string            `yaml:"display_name_en,omitempty" json:"display_name_en,omitempty"`       // human-written names, see validateDisplayNamesDistinct
	Scenario           string            `yaml:"scenario,omitempty" json:"scenario,omitempty"`
	Engine             string            `yaml:"engine" json:"engine"`
	Image              string            `yaml:"image" json:"image"`
	ImageSources       []string          `yaml:"image_sources,omitempty" json:"image_sources,omitempty"` // further pull references for Image, see normalizeImageSources
	Weights            Weights           `yaml:"weights" json:"weights"`
	WeightBytes        int64             `yaml:"weight_bytes,omitempty" json:"weight_bytes,omitempty"`
	RuntimeWeightBytes int64             `yaml:"runtime_weight_bytes,omitempty" json:"runtime_weight_bytes,omitempty"`
	GPUs               GPUSelector       `yaml:"gpus" json:"gpus"`
	Args               []string          `yaml:"args,omitempty" json:"args,omitempty"`
	Env                map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	PreStartArgv       []string          `yaml:"pre_start_argv,omitempty" json:"pre_start_argv,omitempty"`
	PreStartScript     string            `yaml:"pre_start_script,omitempty" json:"pre_start_script,omitempty"`
	Assets             []Asset           `yaml:"assets,omitempty" json:"assets,omitempty"`
	Entrypoint         []string          `yaml:"entrypoint,omitempty" json:"entrypoint,omitempty"`
	Placement          *Placement        `yaml:"placement,omitempty" json:"placement,omitempty"`
	API                *APIContract      `yaml:"api,omitempty" json:"api,omitempty"`
	Components         []Component       `yaml:"components,omitempty" json:"components,omitempty"`
	// PDMode marks a spec that exists for PD disaggregated deployment in that
	// mode (a name from package pdmode): it may only be deployed as PD, and
	// its args and env are what the model needs there (plan D15).
	PDMode string `yaml:"pd_mode,omitempty" json:"pd_mode,omitempty"`
	// ShmBytes is the /dev/shm size the engine needs, for an engine that keeps
	// large state in shared memory. Zero states none. It is a floor on the
	// container's shared memory, not a replacement for the deployment's own
	// shm_bytes (see deploy.launchShmBytes).
	ShmBytes int64 `yaml:"shm_bytes,omitempty" json:"shm_bytes,omitempty"`
	// MemlockUnlimited starts the container with unlimited locked memory
	// (--ulimit memlock=-1:-1), for an engine that pins large host memory.
	// A memlock ulimit the deployment's docker_args state wins over it (see
	// deploy.withMemlockDockerArgs).
	MemlockUnlimited bool `yaml:"memlock_unlimited,omitempty" json:"memlock_unlimited,omitempty"`
	// RetiredDisplayProfile is the display_profile key specs carried before
	// display names. It is still read, so older YAML imports, but never
	// stored; CatalogWarnings reports it.
	RetiredDisplayProfile string `yaml:"display_profile,omitempty" json:"-"`
}

type Weights struct {
	Quantization string         `yaml:"quantization,omitempty" json:"quantization,omitempty"`
	Sources      []WeightSource `yaml:"sources" json:"sources"`
}

// WeightSource accepts the intentionally compact YAML form
// "- huggingface: org/repo" while serializing back to that same form.
type WeightSource struct {
	Provider  string `json:"provider" yaml:"-"`
	RepoID    string `json:"repo_id" yaml:"-"`
	Revision  string `json:"revision,omitempty" yaml:"-"`
	Subfolder string `json:"subfolder,omitempty" yaml:"-"`
}

// DecodeWeightSources decodes the inference_specs.weight_sources column, whose
// owner type is []WeightSource. A blank column declares no sources and is not
// an error. When the value does not decode, the error comes back together with
// whatever encoding/json had already filled in, so a caller that ignores the
// error sees exactly what json.Unmarshal would have left it.
func DecodeWeightSources(raw string) ([]WeightSource, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var sources []WeightSource
	err := json.Unmarshal([]byte(raw), &sources)
	return sources, err
}

func (s *WeightSource) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode || len(value.Content) != 2 {
		return errors.New("weight source must be a one-key mapping")
	}
	s.Provider = strings.ToLower(strings.TrimSpace(value.Content[0].Value))
	payload := value.Content[1]
	if payload.Kind == yaml.ScalarNode {
		s.RepoID = strings.TrimSpace(payload.Value)
		s.Revision = ""
		s.Subfolder = ""
		return nil
	}
	if payload.Kind != yaml.MappingNode || len(payload.Content)%2 != 0 {
		return errors.New("weight source value must be a repository id or {repo_id, revision, subfolder}")
	}
	for index := 0; index < len(payload.Content); index += 2 {
		key, item := payload.Content[index].Value, payload.Content[index+1]
		if item.Kind != yaml.ScalarNode {
			return fmt.Errorf("weight source %s must be a scalar", key)
		}
		switch key {
		case "repo_id":
			s.RepoID = strings.TrimSpace(item.Value)
		case "revision":
			s.Revision = strings.TrimSpace(item.Value)
		case "subfolder":
			s.Subfolder = strings.TrimSpace(item.Value)
		default:
			return fmt.Errorf("unknown weight source field %q", key)
		}
	}
	return nil
}

func (s WeightSource) MarshalYAML() (any, error) {
	node := &yaml.Node{Kind: yaml.MappingNode}
	value := &yaml.Node{Kind: yaml.ScalarNode, Value: s.RepoID}
	if strings.TrimSpace(s.Revision) != "" || strings.TrimSpace(s.Subfolder) != "" {
		value = &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "repo_id"}, {Kind: yaml.ScalarNode, Value: s.RepoID},
		}}
		if strings.TrimSpace(s.Revision) != "" {
			value.Content = append(value.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: "revision"}, &yaml.Node{Kind: yaml.ScalarNode, Value: s.Revision})
		}
		if strings.TrimSpace(s.Subfolder) != "" {
			value.Content = append(value.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: "subfolder"}, &yaml.Node{Kind: yaml.ScalarNode, Value: s.Subfolder})
		}
	}
	node.Content = []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: s.Provider},
		value,
	}
	return node, nil
}

type GPUSelector struct {
	Vendor string `yaml:"vendor" json:"vendor"`
	CC     string `yaml:"cc,omitempty" json:"cc,omitempty"`
	Name   string `yaml:"name,omitempty" json:"name,omitempty"`
	// RetiredDisplayTarget is the display_target key that paired with
	// display_profile: still read so older YAML imports, never stored.
	RetiredDisplayTarget string `yaml:"display_target,omitempty" json:"-"`
	ArchID               string `yaml:"arch_id,omitempty" json:"arch_id,omitempty"`
}

type UnsupportedGPU struct {
	GPUs         GPUSelector `yaml:"gpus" json:"gpus"`
	Quantization string      `yaml:"quantization,omitempty" json:"quantization,omitempty"`
	Reason       string      `yaml:"reason" json:"reason"`
}

// A CC selector may be an exact value ("8.0"), one-sided bound, or range.
// Keep the exact form explicit: the previous alternation accidentally made
// callers believe only bounded selectors were accepted in the flat catalog.
var ccExpression = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]+)?|(?:>=|<)[0-9]+(?:\.[0-9]+)?|[0-9]+(?:\.[0-9]+)?\.\.[0-9]+(?:\.[0-9]+)?)$`)
var modelIDExpression = regexp.MustCompile(`^[^\s/]+(?:/[^\s/]+)*$`)

// ValidateModelID checks the identity grammar shared by imports and clones.
// Callers normalize case and surrounding whitespace before validation.
func ValidateModelID(modelID string) error {
	if !modelIDExpression.MatchString(modelID) {
		return errors.New("requires a valid model_id")
	}
	if !validModelIDPath(modelID) {
		return errors.New("model_id cannot contain dot segments or backslashes")
	}
	return nil
}

// ParseYAML decodes and validates a flat catalog. Unknown keys are rejected so
// retired five-layer fields cannot silently survive an import.
func ParseYAML(data []byte) (FlatCatalog, error) {
	catalog, err := DecodeYAML(data)
	if err != nil {
		return FlatCatalog{}, err
	}
	if err := ValidateFlatCatalog(&catalog); err != nil {
		return FlatCatalog{}, err
	}
	return catalog, nil
}

// DecodeYAML decodes a flat catalog document without validating it. A caller
// that resolves references between models against more than the document -
// the import API lets a template mount a component model the server already
// has - validates the result itself with ValidateFlatCatalog.
func DecodeYAML(data []byte) (FlatCatalog, error) {
	var catalog FlatCatalog
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&catalog); err != nil {
		return FlatCatalog{}, fmt.Errorf("parse inference catalog YAML: %w", requestDeclarationDecodeError(data, err))
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return FlatCatalog{}, errors.New("inference catalog YAML must contain one document")
		}
		return FlatCatalog{}, fmt.Errorf("parse inference catalog YAML: %w", err)
	}
	return catalog, nil
}

// MarshalYAML validates before emitting a catalog, preventing callers from
// writing a document that cannot be imported again.
func MarshalYAML(catalog FlatCatalog) ([]byte, error) {
	if err := ValidateFlatCatalog(&catalog); err != nil {
		return nil, err
	}
	data, err := yaml.Marshal(catalog)
	if err != nil {
		return nil, fmt.Errorf("marshal inference catalog YAML: %w", err)
	}
	return data, nil
}

// validateFlatCatalogStructure normalizes and validates every model/spec in a
// document without resolving relationships between separate original models.
// Package writers need this boundary when serializing one model.yaml at a
// time; the complete catalog validator below remains responsible for all
// cross-model invariants.
func validateFlatCatalogStructure(catalog *FlatCatalog) error {
	if catalog == nil {
		return errors.New("inference catalog is nil")
	}
	if len(catalog.Models) == 0 {
		return errors.New("inference catalog requires at least one model")
	}
	modelIDs := make(map[string]struct{}, len(catalog.Models))
	for mi := range catalog.Models {
		model := &catalog.Models[mi]
		model.ModelID = strings.ToLower(strings.TrimSpace(model.ModelID))
		model.Source = strings.ToLower(strings.TrimSpace(model.Source))
		if model.Source == "" {
			model.Source = "upstream"
		}
		model.DisplayTitle = strings.TrimSpace(model.DisplayTitle)
		model.ParameterCount = strings.TrimSpace(model.ParameterCount)
		model.Architecture = strings.TrimSpace(model.Architecture)
		model.ReadmeZHCN = strings.TrimSpace(model.ReadmeZHCN)
		model.ReadmeEN = strings.TrimSpace(model.ReadmeEN)
		if model.ReadmeZHCN != "" && model.ReadmeZHCN != "README.zh-CN.md" {
			return fmt.Errorf("model %s readme_zh_cn must be README.zh-CN.md", model.ModelID)
		}
		if model.ReadmeEN != "" && model.ReadmeEN != "README.en.md" {
			return fmt.Errorf("model %s readme_en must be README.en.md", model.ModelID)
		}
		modalities, err := normalizeInputModalities(model.ModelID, model.InputModalities)
		if err != nil {
			return err
		}
		model.InputModalities = modalities
		model.ReleaseDate = strings.TrimSpace(model.ReleaseDate)
		if err := ValidateModelID(model.ModelID); err != nil {
			return fmt.Errorf("model %d %w", mi, err)
		}
		if _, ok := modelIDs[model.ModelID]; ok {
			return fmt.Errorf("duplicate model_id %q", model.ModelID)
		}
		modelIDs[model.ModelID] = struct{}{}
		if model.Source != "upstream" && model.Source != "user" {
			return fmt.Errorf("model %s has invalid source %q", model.ModelID, model.Source)
		}
		if model.ContextLength < 0 {
			return fmt.Errorf("model %s context_length cannot be negative", model.ModelID)
		}
		if model.ReleaseDate != "" {
			if !dateExpression.MatchString(model.ReleaseDate) {
				return fmt.Errorf("model %s release_date must be YYYY-MM-DD", model.ModelID)
			}
			if _, err := time.Parse("2006-01-02", model.ReleaseDate); err != nil {
				return fmt.Errorf("model %s release_date is invalid: %w", model.ModelID, err)
			}
		}
		if err := validateModel(model); err != nil {
			return err
		}
		names := make(map[string]struct{}, len(model.Specs))
		for si := range model.Specs {
			if err := validateSpec(model.ModelID, si, &model.Specs[si], names); err != nil {
				return err
			}
			if _, err := inferenceapi.EffectiveContract(model.Specs[si].API, inferenceapi.DefaultsInput{Engine: model.Specs[si].Engine}); err != nil {
				return fmt.Errorf("model %s spec %s API contract: %w", model.ModelID, model.Specs[si].Name, err)
			}
		}
		if err := validateDisplayNamesDistinct(model); err != nil {
			return err
		}
		for ui := range model.Unsupported {
			if err := validateUnsupported(model.ModelID, ui, &model.Unsupported[ui]); err != nil {
				return err
			}
		}
	}
	return nil
}

func ValidateFlatCatalog(catalog *FlatCatalog) error {
	if err := validateFlatCatalogStructure(catalog); err != nil {
		return err
	}
	if err := validateComponentReferences(*catalog); err != nil {
		return err
	}
	if err := validateComponentOnlyModels(*catalog); err != nil {
		return err
	}
	return nil
}

var dateExpression = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

func validateSelector(where string, selector *GPUSelector) error {
	if selector.ArchID != "" && strings.TrimSpace(selector.ArchID) == "" {
		return fmt.Errorf("%s has blank gpus.arch_id", where)
	}
	selector.Vendor = strings.ToLower(strings.TrimSpace(selector.Vendor))
	selector.CC = strings.TrimSpace(selector.CC)
	selector.Name = strings.TrimSpace(selector.Name)
	selector.ArchID = strings.TrimSpace(selector.ArchID)
	if selector.Vendor == "" {
		return fmt.Errorf("%s requires gpus.vendor", where)
	}
	if selector.CC != "" && !ccExpression.MatchString(selector.CC) {
		return fmt.Errorf("%s has invalid gpus.cc %q", where, selector.CC)
	}
	if strings.Contains(selector.CC, "..") {
		bounds := strings.Split(selector.CC, "..")
		lo, loErr := strconv.ParseFloat(bounds[0], 64)
		hi, hiErr := strconv.ParseFloat(bounds[1], 64)
		if loErr != nil || hiErr != nil || lo > hi {
			return fmt.Errorf("%s has invalid gpus.cc range %q", where, selector.CC)
		}
	}
	if selector.Name != "" {
		if _, err := regexp.Compile("(?i)" + selector.Name); err != nil {
			return fmt.Errorf("%s has invalid gpus.name: %w", where, err)
		}
	}
	return nil
}

// validateModel checks the facts that belong to the model rather than to one
// deployable configuration.
func validateModel(model *InferenceModel) error {
	for engine, bytes := range model.KVBytesPerToken {
		if bytes < 0 {
			return fmt.Errorf("model %s kv_bytes_per_token[%s] must not be negative", model.ModelID, engine)
		}
		if strings.TrimSpace(engine) == "" {
			return fmt.Errorf("model %s has a kv_bytes_per_token entry with no engine", model.ModelID)
		}
	}
	return nil
}

func validateSpec(modelID string, index int, spec *InferenceSpec, names map[string]struct{}) error {
	spec.Name = strings.TrimSpace(spec.Name)
	spec.DisplayNameZHCN = strings.TrimSpace(spec.DisplayNameZHCN)
	spec.DisplayNameEN = strings.TrimSpace(spec.DisplayNameEN)
	spec.Scenario = strings.ToLower(strings.TrimSpace(spec.Scenario))
	spec.Engine = strings.ToLower(strings.TrimSpace(spec.Engine))
	spec.Image = strings.TrimSpace(spec.Image)
	spec.Weights.Quantization = strings.ToLower(strings.TrimSpace(spec.Weights.Quantization))
	if spec.Name == "" || spec.Engine == "" || spec.Image == "" {
		return fmt.Errorf("model %s spec %d requires name, engine, and image", modelID, index)
	}
	imageSources, err := normalizeImageSources(modelID, spec.Name, spec.Image, spec.ImageSources)
	if err != nil {
		return err
	}
	spec.ImageSources = imageSources
	if err := validateDisplayName(modelID, spec.Name, "display_name_zh_cn", spec.DisplayNameZHCN, maxDisplayNameZHCN); err != nil {
		return err
	}
	if err := validateDisplayName(modelID, spec.Name, "display_name_en", spec.DisplayNameEN, maxDisplayNameEN); err != nil {
		return err
	}
	if spec.WeightBytes < 0 {
		return fmt.Errorf("model %s spec %s weight_bytes cannot be negative", modelID, spec.Name)
	}
	spec.PDMode = strings.TrimSpace(spec.PDMode)
	if spec.PDMode != "" {
		mode, ok := pdmode.Lookup(spec.PDMode)
		if !ok {
			return fmt.Errorf("model %s spec %s has unknown pd_mode %q (known: %s)", modelID, spec.Name, spec.PDMode, strings.Join(pdmode.Names(), ", "))
		}
		if mode.Engine != spec.Engine {
			return fmt.Errorf("model %s spec %s pd_mode %s runs on %s, not %s", modelID, spec.Name, spec.PDMode, mode.Engine, spec.Engine)
		}
	}
	if err := validateShmBytes(modelID, spec); err != nil {
		return err
	}
	if spec.RuntimeWeightBytes < 0 {
		return fmt.Errorf("model %s spec %s runtime_weight_bytes cannot be negative", modelID, spec.Name)
	}
	if spec.RuntimeWeightBytes > 0 && spec.WeightBytes > 0 && spec.RuntimeWeightBytes > spec.WeightBytes {
		return fmt.Errorf("model %s spec %s runtime_weight_bytes cannot exceed weight_bytes", modelID, spec.Name)
	}
	if _, duplicate := names[spec.Name]; duplicate {
		return fmt.Errorf("model %s has duplicate spec name %q", modelID, spec.Name)
	}
	names[spec.Name] = struct{}{}
	if spec.Scenario != "" && spec.Scenario != "latency" && spec.Scenario != "balanced" && spec.Scenario != "throughput" {
		return fmt.Errorf("model %s spec %s has invalid scenario %q", modelID, spec.Name, spec.Scenario)
	}
	if err := validateSelector(fmt.Sprintf("model %s spec %s", modelID, spec.Name), &spec.GPUs); err != nil {
		return err
	}
	if err := validateWeights(modelID, spec.Name, &spec.Weights); err != nil {
		return err
	}
	if len(spec.PreStartArgv) > 0 && strings.TrimSpace(spec.PreStartScript) != "" {
		return fmt.Errorf("model %s spec %s cannot set both pre_start_argv and pre_start_script", modelID, spec.Name)
	}
	for i, arg := range spec.PreStartArgv {
		if strings.TrimSpace(arg) == "" {
			return fmt.Errorf("model %s spec %s pre_start_argv[%d] is empty", modelID, spec.Name, i)
		}
	}
	if len([]byte(spec.PreStartScript)) > 64*1024 {
		return fmt.Errorf("model %s spec %s pre_start_script exceeds 64 KiB", modelID, spec.Name)
	}
	for key := range spec.Env {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("model %s spec %s has an empty env key", modelID, spec.Name)
		}
	}
	return validateExecutableContract(modelID, spec)
}

func validateWeights(modelID, specName string, weights *Weights) error {
	if strings.TrimSpace(weights.Quantization) == "" {
		return fmt.Errorf("model %s spec %s requires weights.quantization", modelID, specName)
	}
	if len(weights.Sources) == 0 {
		return fmt.Errorf("model %s spec %s requires weights.sources", modelID, specName)
	}
	for i := range weights.Sources {
		source := &weights.Sources[i]
		source.Provider = strings.ToLower(strings.TrimSpace(source.Provider))
		source.RepoID = strings.TrimSpace(source.RepoID)
		source.Revision = strings.TrimSpace(source.Revision)
		source.Subfolder = strings.TrimSpace(source.Subfolder)
		if source.Provider != "huggingface" && source.Provider != "modelscope" {
			return fmt.Errorf("model %s spec %s has unsupported weight source %q", modelID, specName, source.Provider)
		}
		if source.RepoID == "" || strings.ContainsAny(source.RepoID, "\r\n") {
			return fmt.Errorf("model %s spec %s has invalid %s repository id", modelID, specName, source.Provider)
		}
		if strings.ContainsAny(source.Revision, "\r\n") {
			return fmt.Errorf("model %s spec %s has invalid %s revision", modelID, specName, source.Provider)
		}
		subfolder, err := modelrepo.NormalizeSubfolder(source.Subfolder)
		if err != nil {
			return fmt.Errorf("model %s spec %s has invalid %s subfolder: %w", modelID, specName, source.Provider, err)
		}
		source.Subfolder = subfolder
	}
	return nil
}

func validateUnsupported(modelID string, index int, item *UnsupportedGPU) error {
	if err := validateSelector(fmt.Sprintf("model %s unsupported %d", modelID, index), &item.GPUs); err != nil {
		return err
	}
	item.Quantization = strings.ToLower(strings.TrimSpace(item.Quantization))
	item.Reason = strings.TrimSpace(item.Reason)
	if item.Reason == "" {
		return fmt.Errorf("model %s unsupported %d requires reason", modelID, index)
	}
	return nil
}
