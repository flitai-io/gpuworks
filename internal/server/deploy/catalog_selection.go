// catalog_selection.go owns the deployment's catalog selection snapshot: the
// document the API writes into deployments.template_snapshot and every reader
// decodes again.

package deploy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/pullref"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/placement"
)

// CatalogSelection is what a deployment selected from the inference catalog:
// the flat spec it came from, the weights it expects, and the launch inputs the
// template owns. It is one type because the column used to have six decoders -
// the writer's struct, the controller's layer, and four ad-hoc maps - so a new
// field could land in five of them and be silently missing from the sixth.
//
// The wire shape is a store contract, not an implementation detail: rows
// written by an older server are read by this one and the other way around.
// That is why every field keeps the tag the writer used, why unknown fields are
// preserved (Extra), and why Encode reproduces the stored layout rather than
// re-rendering the document from the typed view (see fields()).
//
// A decoded selection is edited through SetTemplateArgs: that is the field the
// create path and the startup normalizer own, and it writes the stored
// document as well as the typed view. Assigning a typed
// field directly does not change the document, because the stored bytes - not
// the typed view - are what a row means (see TestCatalogSelectionIgnoresDirectFieldWrites).
type CatalogSelection struct {
	// Acquisition is the acquisition record the create request stated
	// (model_source, image_pull), kept as raw JSON: it is client vocabulary the
	// api layer owns, and this package only stores and returns it.
	Acquisition        json.RawMessage      `json:"acquisition,omitempty"`
	TemplateID         uint                 `json:"template_id"`
	ModelID            string               `json:"model_id"`
	SpecSource         string               `json:"spec_source"`
	SpecName           string               `json:"spec_name"`
	Scenario           string               `json:"scenario,omitempty"`
	Engine             string               `json:"engine"`
	Image              string               `json:"image"`
	ImageSources       []string             `json:"image_sources,omitempty"`
	Quantization       string               `json:"quantization,omitempty"`
	WeightProvider     string               `json:"weight_provider"`
	WeightRepoID       string               `json:"weight_repo_id"`
	WeightRevision     string               `json:"weight_revision,omitempty"`
	WeightSubfolder    string               `json:"weight_subfolder,omitempty"`
	WeightBytes        int64                `json:"weight_bytes,omitempty"`
	RuntimeWeightBytes int64                `json:"runtime_weight_bytes,omitempty"`
	GPUs               json.RawMessage      `json:"gpus"`
	Unsupported        json.RawMessage      `json:"unsupported,omitempty"`
	ExtraArgs          []string             `json:"extra_args,omitempty"`
	Env                map[string]string    `json:"env,omitempty"`
	PreStartArgv       []string             `json:"pre_start_argv,omitempty"`
	PreStartScript     string               `json:"pre_start_script,omitempty"`
	RuntimeAssets      []proto.RuntimeAsset `json:"runtime_assets,omitempty"`
	Entrypoint         []string             `json:"entrypoint,omitempty"`
	Placement          json.RawMessage      `json:"placement,omitempty"`
	APIContract        json.RawMessage      `json:"api,omitempty"`
	Components         []ComponentBinding   `json:"components,omitempty"`
	// RankWeights binds each rank of a deployment whose weight subfolder
	// names the rank (WeightSubfolder keeps the template, such as
	// "rank${NODE_RANK}") to the artifact that rank mounts; empty when the
	// weights are the same on every rank (ADR 0076).
	RankWeights []RankWeight `json:"rank_weights,omitempty"`
	// PDMode is the PD mode a PD-only spec was written for.
	PDMode string `json:"pd_mode,omitempty"`
	// ShmBytes is the /dev/shm size the template's engine needs; zero states
	// none. launchShmBytes takes it as a floor under the derived size.
	ShmBytes int64 `json:"shm_bytes,omitempty"`
	// MemlockUnlimited asks for --ulimit memlock=-1:-1 unless the deployment's
	// docker_args state a memlock ulimit.
	MemlockUnlimited bool `json:"memlock_unlimited,omitempty"`
	// StartTimeoutSec is the instance start timeout the template stated;
	// zero states none. resolveStartupBudget takes it over the cluster
	// default and under the deployment's own start_timeout_sec.
	StartTimeoutSec int `json:"start_timeout_sec,omitempty"`
	// DockerArgs are the template's own docker run options, applied before
	// the deployment's own docker_args and the platform's
	// (deploy.launchDockerArgs). A running deployment keeps the args it was
	// created with: this snapshot, not a fresh catalog read, is what every
	// later launch of the deployment resolves against.
	DockerArgs []string `json:"docker_args,omitempty"`
	// Dirs are the template's writable directories (spec dirs). A snapshot
	// written before templates declared them has none, which is what it
	// means.
	Dirs               []ManagedDir `json:"dirs,omitempty"`
	DeviatedFields     []string     `json:"deviated_fields,omitempty"`
	MatchedBy          string       `json:"matched_by"`
	ImageDigestWarning bool         `json:"image_digest_warning,omitempty"`
	// Extra holds the document's fields this type does not name, verbatim. A
	// field added by a newer server survives a decode/encode cycle here instead
	// of being dropped on the next write.
	Extra map[string]json.RawMessage `json:"-"`
	// raw is the whole document as it was decoded. Re-encoding a selection that
	// came from a row reproduces that row (see fields()).
	raw map[string]json.RawMessage
}

// catalogSelectionKeys is every key this type owns, derived from the struct
// tags so it cannot drift from them.
var catalogSelectionKeys = func() map[string]struct{} {
	keys := map[string]struct{}{}
	kind := reflect.TypeOf(CatalogSelection{})
	for index := 0; index < kind.NumField(); index++ {
		tag, tagged := kind.Field(index).Tag.Lookup("json")
		if !tagged {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		keys[name] = struct{}{}
	}
	return keys
}()

// DecodeCatalogSelection reads a stored snapshot. An empty document decodes to
// the zero selection: a deployment without a catalog template has no snapshot,
// which is a state and not an error. A document that is not a JSON object is an
// error, because that is a row nobody can read.
func DecodeCatalogSelection(document []byte) (CatalogSelection, error) {
	var selection CatalogSelection
	if len(bytes.TrimSpace(document)) == 0 {
		return selection, nil
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(document, &raw); err != nil {
		return selection, fmt.Errorf("invalid catalog selection snapshot: %w", err)
	}
	if raw == nil {
		return selection, fmt.Errorf("invalid catalog selection snapshot: expected object")
	}
	if err := json.Unmarshal(document, &selection); err != nil {
		return selection, fmt.Errorf("invalid catalog selection snapshot: %w", err)
	}
	selection.raw = raw
	for key, value := range raw {
		if _, known := catalogSelectionKeys[key]; known {
			continue
		}
		if selection.Extra == nil {
			selection.Extra = map[string]json.RawMessage{}
		}
		selection.Extra[key] = value
	}
	return selection, nil
}

// Encode renders the snapshot for storage.
func (s CatalogSelection) Encode() ([]byte, error) {
	fields, err := s.fields()
	if err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

// fields renders the document as a keyed object, which is the layout the writer
// has always stored: the pre-typed writer round-tripped every create through a
// map, so the stored top-level keys are sorted while nested objects keep
// whatever order their own marshal produced.
//
// A selection that was decoded starts from the document it was read from, so an
// unchanged row re-encodes byte for byte (that is the snapshot's stability
// across upgrades) and a field a caller changed through a setter replaces just
// its own entry. A selection built in this process has no such document and is
// rendered from the typed view, in the shape the writer would have produced for
// it: the pre-typed writer had two, because it only round-tripped the document
// through a map[string]any when it had an acquisition record to insert. That is
// why the nested bytes of a fresh selection depend on whether one was stated -
// it is not a new rule, it is the stored layout this writer inherits.
func (s CatalogSelection) fields() (map[string]json.RawMessage, error) {
	if s.raw != nil {
		fields := make(map[string]json.RawMessage, len(s.raw)+len(s.Extra))
		for key, value := range s.raw {
			fields[key] = value
		}
		for key, value := range s.Extra {
			if _, present := fields[key]; !present {
				fields[key] = value
			}
		}
		return fields, nil
	}
	if s.empty() {
		// A deployment created without a catalog template has no selection at
		// all. The writer stores just the acquisition record, when the request
		// stated one; rendering the typed view would write a row of zero-valued
		// fields instead.
		fields := map[string]json.RawMessage{}
		if len(s.Acquisition) > 0 {
			fields["acquisition"] = s.Acquisition
		}
		for key, value := range s.Extra {
			fields[key] = value
		}
		return fields, nil
	}
	acquisition := s.Acquisition
	wire := s
	wire.Acquisition, wire.Extra, wire.raw = nil, nil, nil
	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("encode catalog selection snapshot: %w", err)
	}
	if len(acquisition) == 0 {
		// No acquisition record: the writer stored the object as the struct
		// marshaled it, so only the top level is re-keyed and the nested bytes
		// stay exactly as their own marshal produced them.
		fields := map[string]json.RawMessage{}
		if err := json.Unmarshal(encoded, &fields); err != nil {
			return nil, fmt.Errorf("encode catalog selection snapshot: %w", err)
		}
		for key, value := range s.Extra {
			if _, present := fields[key]; !present {
				fields[key] = value
			}
		}
		return fields, nil
	}
	document := map[string]any{}
	if err := json.Unmarshal(encoded, &document); err != nil {
		return nil, fmt.Errorf("encode catalog selection snapshot: %w", err)
	}
	// The acquisition record is client vocabulary the create request already
	// marshaled; it is stored verbatim rather than through the map above.
	document["acquisition"] = acquisition
	for key, value := range s.Extra {
		if _, present := document[key]; !present {
			document[key] = value
		}
	}
	merged, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode catalog selection snapshot: %w", err)
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(merged, &fields); err != nil {
		return nil, fmt.Errorf("encode catalog selection snapshot: %w", err)
	}
	return fields, nil
}

// empty reports whether this selection names no catalog row at all - which is
// what a deployment created without a template has. The acquisition record and
// the unknown fields are not part of that judgement: they are written for such
// a deployment too.
func (s CatalogSelection) empty() bool {
	probe, zero := s, CatalogSelection{}
	for _, value := range []*CatalogSelection{&probe, &zero} {
		value.Acquisition, value.Extra, value.raw = nil, nil, nil
	}
	probeJSON, err := json.Marshal(probe)
	if err != nil {
		return false
	}
	zeroJSON, err := json.Marshal(zero)
	if err != nil {
		return false
	}
	return string(probeJSON) == string(zeroJSON)
}

// SetTemplateArgs replaces the template's residual arguments. The stored
// document is updated in place so the rest of the row keeps its own bytes.
func (s *CatalogSelection) SetTemplateArgs(args []string) error {
	s.ExtraArgs = args
	encoded, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("encode template arguments: %w", err)
	}
	s.setDocumentField("extra_args", encoded)
	return nil
}

// ImageSourcesFor returns the further pull references the template declared for
// its image (ADR 0038) when image is the image this deployment was created with.
// The API records the sources only when that image was the template's own
// (an override gets none), so the snapshot's Image is the template image
// whenever there are sources to return; comparing against it keeps a launch for
// any other image - a preview of an overriding request, say - from pulling
// another image's sources. Images compare as pullref.SameRef does.
func (s CatalogSelection) ImageSourcesFor(image string) []string {
	if len(s.ImageSources) == 0 || !pullref.SameRef(s.Image, image) {
		return nil
	}
	return s.ImageSources
}

// PlacementResolver returns the placement resolver language the selection
// declares, or an empty string when the row carries none.
func (s CatalogSelection) PlacementResolver() string {
	return strings.TrimSpace(s.placementContract().Resolver)
}

func (s CatalogSelection) placementContract() placement.Contract {
	var contract placement.Contract
	if json.Unmarshal(s.Placement, &contract) != nil {
		return placement.Contract{}
	}
	return contract
}

// ValidatePlacementNodes applies the spec's launch-wide limit before the
// per-node resolver sees the selected cards.
func (s CatalogSelection) ValidatePlacementNodes(nodes int) error {
	limit := int(s.placementContract().MaxNodes)
	if limit > 0 && nodes > limit {
		return messages.NewProductError(messages.ErrorTemplatePlacementNodeLimit,
			fmt.Sprintf("the inference spec supports at most %d nodes per launch, but %d were requested", limit, nodes),
			map[string]any{"maximum": limit, "actual": nodes})
	}
	return nil
}

// AcquisitionField returns one field of the acquisition record as it was
// stored, or nil when the row carries no record or no such field. The record's
// shape belongs to the api layer; this package only preserves it.
func (s CatalogSelection) AcquisitionField(name string) json.RawMessage {
	if len(s.Acquisition) == 0 {
		return nil
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(s.Acquisition, &fields); err != nil {
		return nil
	}
	return fields[name]
}

// setDocumentField writes one key into the decoded document, if there is one.
// A selection built in this process renders from the typed view instead, so it
// has nothing to patch.
func (s *CatalogSelection) setDocumentField(key string, value json.RawMessage) {
	if s.raw == nil {
		return
	}
	// CatalogSelection is a value type that carries a map, so a copy shares the
	// document it was decoded from. Writing through a copy would therefore
	// change the original as well; the copy takes its own map first.
	detached := make(map[string]json.RawMessage, len(s.raw))
	for existing, raw := range s.raw {
		detached[existing] = raw
	}
	detached[key] = value
	s.raw = detached
}
