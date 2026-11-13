// Package templateexport writes a running deployment as a flat catalog
// template, for saving a deployment that works as a reusable spec. Every fact
// in the result comes from what the deployment actually ran: its launch
// configuration, the weights its instances read, the cards they ran on and the
// operations that answered when the export was made. A fact the platform has
// no reliable source for is left out and named in a note instead of guessed.
// See docs/how-to/save-deployment-as-template.md.
package templateexport

import (
	"crypto/sha256"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/pkg/modelconfig"
)

// Input is everything an export is built from. The server reads it from the
// store (internal/server/app); tests build it directly.
type Input struct {
	DeploymentID   uint
	DeploymentName string
	Engine         string
	// Image is the image the deployment's instances run.
	Image string
	// Quick, TemplateArgs and UserArgs are the stored launch layers: the
	// typed configuration, the template's residual args and the user's.
	Quick        deploy.LaunchConfig
	TemplateArgs []string
	UserArgs     []string
	// TemplateEnv is the template's env, UserEnv the deployment's own.
	TemplateEnv map[string]string
	UserEnv     map[string]string
	// IncludeEnv names the user env keys the caller asked to keep.
	IncludeEnv []string
	// Template is the catalog model and spec the deployment was created
	// from, nil for a deployment made without a template.
	Template     *inferencecatalog.InferenceModel
	TemplateSpec *inferencecatalog.InferenceSpec
	// Model is the deployment's model, for a deployment without a template.
	Model ModelFacts
	// GPUs are the cards one running instance holds; GPUCount is how many.
	GPUs     []GPUFact
	GPUCount int
	// VRAMLimitBytes is the deployment's dGPU memory limit per card, zero
	// for whole cards.
	VRAMLimitBytes int64
	// HasContainerOverrides reports container settings (mounts, docker args)
	// the export leaves out because they describe one host.
	HasContainerOverrides bool
	// KVBytesPerToken is the catalog's value for this model and engine,
	// zero when the catalog has none.
	KVBytesPerToken int64
	// Checks are the operation checks run for this export.
	Checks []Check
}

// ModelFacts is what a deployment without a template knows about its model.
type ModelFacts struct {
	Source            string // huggingface | modelscope | local | url
	RepoID            string
	Subfolder         string
	Name              string
	Quantization      string
	WeightBytes       int64
	ContextLength     int64
	ConfigJSON        string
	InputModalities   []string
	Operations        []inferenceapi.Operation
	ResolvedRevisions []string // ModelFile.Revision of each copy the instances read
}

// GPUFact is one card a running instance holds.
type GPUFact struct {
	Name          string
	Vendor        string
	MemoryMiB     int64
	ComputeCap    string
	DriverVersion string
}

// Check is one operation the export sent a request for, or decided not to.
type Check struct {
	Operation  string `json:"operation"`
	Endpoint   string `json:"endpoint,omitempty"`
	Tested     bool   `json:"tested"`
	Passed     bool   `json:"passed"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Message    string `json:"message,omitempty"`
}

// Export is a deployment written as a template.
type Export struct {
	Model inferencecatalog.InferenceModel
	// YAML is Model as a flat catalog document, with comments that state what
	// was verified and what is missing.
	YAML string
	// Checks are the operation checks the export was made with.
	Checks []Check
	// Notes are the facts the comments state, for the client to show in its
	// own language.
	Notes []Note
	// ExcludedEnv are the user env keys left out of the spec.
	ExcludedEnv []string
	// Verified reports that every tested operation answered; Saveable that
	// the result may be written into the catalog as it is.
	Verified bool
	Saveable bool
	// Evidence is the plain-text test record a contributor attaches when
	// proposing the template upstream.
	Evidence string
}

// Build writes in as a template. It fails only for a deployment that cannot
// be expressed as a spec at all; everything else becomes a note.
func Build(in Input) (Export, error) {
	if strings.TrimSpace(in.Engine) == "" {
		// A deployment stored before the engine was recorded runs the default.
		in.Engine = deploy.DefaultEngine
	}
	if in.GPUCount <= 0 || len(in.GPUs) != in.GPUCount {
		return Export{}, Unavailable(ReasonNoGPUFacts, nil)
	}
	if in.TemplateSpec != nil && len(in.TemplateSpec.Assets) > 0 {
		// Assets travel in a catalog package beside the YAML; a plain
		// document cannot carry them.
		return Export{}, Unavailable(ReasonTemplateAssets, nil)
	}
	if in.TemplateSpec != nil && len(in.TemplateSpec.Components) > 0 {
		// A component names another catalog model, which a one-model
		// document cannot resolve on its own.
		return Export{}, Unavailable(ReasonComponents, nil)
	}
	launch, err := deploy.ExportLaunchArgs(in.Engine, in.Quick, in.TemplateArgs, in.UserArgs)
	if err != nil {
		return Export{}, err
	}
	gpus, err := Selector(in.GPUs[0])
	if err != nil {
		return Export{}, err
	}
	for _, card := range in.GPUs[1:] {
		selector, err := Selector(card)
		if err != nil {
			return Export{}, err
		}
		if selector != gpus {
			return Export{}, Unavailable(ReasonMixedGPUs, nil)
		}
	}
	env, excluded := exportEnv(in.TemplateEnv, in.UserEnv, in.IncludeEnv)

	var model inferencecatalog.InferenceModel
	var spec inferencecatalog.InferenceSpec
	var modelNotes []Note
	if in.Template != nil && in.TemplateSpec != nil {
		model, spec = fromTemplate(*in.Template, *in.TemplateSpec)
	} else {
		model, spec, modelNotes, err = fromModel(in)
		if err != nil {
			return Export{}, err
		}
	}
	spec.Name = specName(in)
	spec.DisplayNameZHCN, spec.DisplayNameEN = "", ""
	spec.Engine = in.Engine
	if strings.TrimSpace(in.Image) != "" {
		spec.Image = in.Image
	}
	spec.GPUs = gpus
	spec.Args = launch.Args
	spec.Env = env
	model.Specs = []inferencecatalog.InferenceSpec{spec}

	notes := launchNotes(in, launch, excluded, model)
	notes = append(notes, modelNotes...)
	notes = append(notes, checkNotes(in.Checks)...)
	verified := true
	for _, check := range in.Checks {
		if check.Tested && !check.Passed {
			verified = false
		}
	}
	out := Export{Model: model, Checks: in.Checks, Notes: notes, ExcludedEnv: excluded, Verified: verified, Saveable: verified}
	if out.YAML, err = renderYAML(model, in, notes); err != nil {
		return Export{}, err
	}
	out.Evidence = renderEvidence(in, notes)
	return out, nil
}

// fromTemplate starts the spec from the one the deployment was created from:
// what the export does not replace (weights, placement, components, API
// contract, pre-start steps) is the template's, which the deployment ran.
// The model's own facts stay the template's too; the documents a catalog
// package carries beside the YAML do not come along, because a plain YAML
// document cannot carry them.
func fromTemplate(template inferencecatalog.InferenceModel, source inferencecatalog.InferenceSpec) (inferencecatalog.InferenceModel, inferencecatalog.InferenceSpec) {
	model := template
	model.Source = "user"
	model.ReadmeZHCN, model.ReadmeEN = "", ""
	spec := source
	spec.Args = append([]string(nil), source.Args...)
	return model, spec
}

// fromModel writes a model the catalog does not have yet from the facts the
// deployment's model row and its downloaded copies carry.
func fromModel(in Input) (inferencecatalog.InferenceModel, inferencecatalog.InferenceSpec, []Note, error) {
	facts := in.Model
	source, err := weightSource(facts)
	if err != nil {
		return inferencecatalog.InferenceModel{}, inferencecatalog.InferenceSpec{}, nil, err
	}
	model := inferencecatalog.InferenceModel{
		ModelID:         strings.ToLower(strings.TrimSpace(facts.RepoID)),
		Source:          "user",
		DisplayTitle:    displayTitle(facts),
		ContextLength:   facts.ContextLength,
		InputModalities: append([]string(nil), facts.InputModalities...),
	}
	// Keep human local weight identities intact. Catalog keys must also be
	// safe URL path segments so whitespace and Unicode do not change lookup.
	if source.Provider == "local" {
		portable := inferencecatalog.ValidateModelID(model.ModelID) == nil
		for _, segment := range strings.Split(model.ModelID, "/") {
			portable = portable && url.QueryEscape(segment) == segment
		}
		if !portable {
			model.ModelID = fmt.Sprintf("local/%x", sha256.Sum256([]byte(model.ModelID)))
		}
	}
	var notes []Note
	if strings.TrimSpace(facts.ConfigJSON) != "" {
		if config, err := modelconfig.Parse(strings.NewReader(facts.ConfigJSON)); err == nil {
			model.ModelConfig = config
		}
	}
	if model.ModelConfig == nil {
		notes = append(notes, Note{Code: NoteModelConfigMissing})
	}
	if in.KVBytesPerToken > 0 {
		model.KVBytesPerToken = map[string]int64{in.Engine: in.KVBytesPerToken}
	} else {
		notes = append(notes, Note{Code: NoteKVBytesMissing, Params: map[string]any{"engine": in.Engine}})
	}
	spec := inferencecatalog.InferenceSpec{
		Scenario:    "balanced",
		Weights:     inferencecatalog.Weights{Quantization: quantization(facts.Quantization), Sources: []inferencecatalog.WeightSource{source}},
		WeightBytes: facts.WeightBytes,
	}
	if len(facts.Operations) > 0 {
		spec.API = &inferencecatalog.APIContract{HealthEndpoint: "/health", Operations: append([]inferenceapi.Operation(nil), facts.Operations...)}
	}
	return model, spec, notes, nil
}

// exportEnv keeps the template's env and only the user env keys asked for:
// a deployment's own env is where tokens and host paths live, and the export
// is a file meant to be shared.
func exportEnv(template, user map[string]string, include []string) (map[string]string, []string) {
	env := map[string]string{}
	for key, value := range template {
		env[key] = value
	}
	keep := map[string]bool{}
	for _, key := range include {
		keep[strings.TrimSpace(key)] = true
	}
	var excluded []string
	for key, value := range user {
		if keep[key] {
			env[key] = value
			continue
		}
		if templateValue, inherited := template[key]; inherited && templateValue == value {
			continue
		}
		excluded = append(excluded, key)
	}
	sort.Strings(excluded)
	if len(env) == 0 {
		env = nil
	}
	return env, excluded
}

// specName names the new spec after what makes it distinct: the engine, the
// card it was verified on and the deployment it came from.
func specName(in Input) string {
	card := slug(in.GPUs[0].Name)
	if card == "" {
		card = slug(in.GPUs[0].Vendor)
	}
	return fmt.Sprintf("%s-%dx%s-d%d", slug(in.Engine), in.GPUCount, card, in.DeploymentID)
}

func slug(text string) string {
	var out strings.Builder
	dash := false
	for _, char := range strings.ToLower(text) {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
			out.WriteRune(char)
			dash = false
		case !dash && out.Len() > 0:
			out.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(out.String(), "-")
}

func displayTitle(facts ModelFacts) string {
	if name := strings.TrimSpace(facts.Name); name != "" {
		return name
	}
	repo := strings.TrimSpace(facts.RepoID)
	if at := strings.LastIndex(repo, "/"); at >= 0 {
		return repo[at+1:]
	}
	return repo
}

// quantization is the spec's required quantization label; a model row that
// states none is described by its weights as they are.
func quantization(label string) string {
	label = strings.ToLower(strings.TrimSpace(label))
	switch label {
	case "", "auto":
		return "auto"
	case "bfloat16":
		return "bf16"
	case "float16", "half":
		return "fp16"
	}
	return label
}
