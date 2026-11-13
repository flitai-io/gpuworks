package deployment

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"pgregory.net/rapid"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The world the deployment properties draw: a template spec, a model the
// request selects, where copies of it live, and the request itself. Every
// dimension is drawn independently of the others, so the generator reaches
// combinations a fixture author picking "matching" values would not.

const (
	worldTemplateRepo     = "Acme/Deploy-FP8"
	worldTemplateRevision = "template-rev"
	worldTargetNode       = "node-a"
	worldPeerNode         = "node-b"
	worldSpecWeightBytes  = int64(16 << 30)
)

// The public providers an inference spec may declare weight sources at.
var worldTemplateProviders = []string{"huggingface", "modelscope"}

type modelSourceKind int

const (
	sourceLocal modelSourceKind = iota
	sourceDeclared
	sourceUndeclared
)

var sourceKindNames = []string{"local", "declared-provider", "undeclared-provider"}

type sourceIDKind int

const (
	idIdentical sourceIDKind = iota
	idCaseOnly
	idUnrelated
)

var sourceIDKindNames = []string{"identical", "case-differs", "unrelated"}

type revisionKind int

const (
	revisionEmpty revisionKind = iota
	revisionTemplate
	revisionOther
)

var revisionKindNames = []string{"empty", "template", "other"}

type copyKind int

const (
	copyTargetLocal copyKind = iota
	copyPeerOnly
	copyShared
	copyFailedLeftover
	copyNone
)

var copyKindNames = []string{"target-local", "peer-only", "shared", "failed-leftover", "none"}

type requestProviderKind int

const (
	requestNoProvider requestProviderKind = iota
	requestDeclaredProvider
	requestUndeclaredProvider
)

var requestProviderNames = []string{"none", "declared", "undeclared"}

// templateSource is one weight source the drawn spec declares.
type templateSource struct {
	Provider string
	Pinned   bool
}

// launchShape is the part of the world property C adds: placement,
// replicas, parallelism, sleep and the nodes' cards.
type launchShape struct {
	Automatic     bool
	Replicas      int
	QuickTP       int // placement.tp, 0 = unstated
	QuickPP       int // placement.pp, 0 = unstated
	QuickDP       int // quick_params.data_parallel_size, 0 = unstated
	TemplateTP    int // --tensor-parallel-size in the spec args, 0 = absent
	TemplateDP    int // --data-parallel-size in the spec args, 0 = absent
	Sleepable     bool
	VRAMLimitGiB  float64
	TargetGPUs    int
	PeerGPUs      int
	CardMemoryGiB int64
	BusyTarget    bool // a foreign process holds the target's first card
	TargetDGPU    bool // the target reports a ready NVIDIA dGPU backend
}

type acquisitionWorld struct {
	Sources         []templateSource
	SelectedSource  modelSourceKind
	SelectedID      sourceIDKind
	SelectedRev     revisionKind
	Copy            copyKind
	Decoy           bool // a ready row at the spec's first source tempts a substitution
	RequestProvider requestProviderKind
	// SelectedAt and RequestAt index the declared source a declared-provider
	// selected model sits at and a declared request provider names, drawn
	// apart so the two agree only some of the time.
	SelectedAt, RequestAt int
	// NamesModel is whether the request names the selected model's id; one
	// that does not leaves the model row to the template's weight sources.
	NamesModel bool
	Shape      launchShape
}

func drawTemplateSources(t *rapid.T) []templateSource {
	providers := rapid.SliceOfNDistinct(rapid.SampledFrom(worldTemplateProviders), 1, len(worldTemplateProviders), rapid.ID[string]).Draw(t, "template_providers")
	sources := make([]templateSource, len(providers))
	for i, provider := range providers {
		sources[i] = templateSource{Provider: provider, Pinned: rapid.Bool().Draw(t, "pinned_"+provider)}
	}
	return sources
}

// genAcquisitionWorld draws the model-identity dimensions; the launch shape
// is the single-GPU, pinned or automatic default property B needs.
func genAcquisitionWorld(t *rapid.T) acquisitionWorld {
	w := acquisitionWorld{
		Sources:         drawTemplateSources(t),
		SelectedSource:  modelSourceKind(rapid.IntRange(0, 2).Draw(t, "selected_source")),
		SelectedID:      sourceIDKind(rapid.IntRange(0, 2).Draw(t, "selected_id")),
		SelectedRev:     revisionKind(rapid.IntRange(0, 2).Draw(t, "selected_revision")),
		Copy:            copyKind(rapid.IntRange(0, 4).Draw(t, "copy")),
		Decoy:           rapid.Bool().Draw(t, "decoy"),
		RequestProvider: requestProviderKind(rapid.IntRange(0, 2).Draw(t, "request_provider")),
		NamesModel:      rapid.SampledFrom([]bool{true, true, false}).Draw(t, "names_model"),
		Shape: launchShape{
			Automatic: rapid.Bool().Draw(t, "automatic"), Replicas: 1,
			TargetGPUs: 1, PeerGPUs: 1, CardMemoryGiB: 80,
		},
	}
	w.drawDeclaredIndexes(t)
	if rapid.IntRange(0, 3).Draw(t, "imported_copy") == 0 {
		w.importedCopy(t)
	}
	return w
}

// importedCopy turns one draw in four into the GWKS-99 shape, which uniform
// independent draws reach about once in five hundred: a request that names
// no model but names a provider, whose spec pins a revision there, beside a
// registered row at that provider and repository with no revision (a scan
// import) that the target reads already. Where the copy sits, the repository
// id's case and the placement stay drawn.
func (w *acquisitionWorld) importedCopy(t *rapid.T) {
	w.NamesModel = false
	w.RequestProvider, w.SelectedSource = requestDeclaredProvider, sourceDeclared
	w.SelectedAt = w.RequestAt
	w.Sources[w.RequestAt].Pinned = true
	w.SelectedRev = revisionEmpty
	w.SelectedID = sourceIDKind(rapid.IntRange(0, 1).Draw(t, "imported_id"))
	w.Copy = rapid.SampledFrom([]copyKind{copyTargetLocal, copyShared}).Draw(t, "imported_copy_at")
}

func (w *acquisitionWorld) drawDeclaredIndexes(t *rapid.T) {
	w.SelectedAt = rapid.IntRange(0, len(w.Sources)-1).Draw(t, "selected_at")
	w.RequestAt = rapid.IntRange(0, len(w.Sources)-1).Draw(t, "request_at")
}

// genLaunchWorld extends the acquisition world with the launch shape property
// C varies. The identity dimensions keep all their values, but the two that
// refuse at template selection whatever the shape (an undeclared request
// provider, an unrelated repository id) are drawn one time in ten, so most
// iterations reach the placement and parallelism rules C is about.
func genLaunchWorld(t *rapid.T) acquisitionWorld {
	w := acquisitionWorld{
		Sources:         drawTemplateSources(t),
		SelectedSource:  modelSourceKind(rapid.IntRange(0, 2).Draw(t, "selected_source")),
		SelectedID:      sourceIDKind(rapid.SampledFrom([]int{0, 0, 0, 0, 0, 1, 1, 1, 1, 2}).Draw(t, "selected_id")),
		SelectedRev:     revisionKind(rapid.IntRange(0, 2).Draw(t, "selected_revision")),
		Copy:            copyKind(rapid.IntRange(0, 4).Draw(t, "copy")),
		Decoy:           rapid.Bool().Draw(t, "decoy"),
		RequestProvider: requestProviderKind(rapid.SampledFrom([]int{0, 0, 0, 0, 0, 1, 1, 1, 1, 2}).Draw(t, "request_provider")),
		NamesModel:      true,
	}
	w.drawDeclaredIndexes(t)
	w.Shape = launchShape{
		Automatic:     rapid.Bool().Draw(t, "automatic"),
		Replicas:      rapid.IntRange(1, 3).Draw(t, "replicas"),
		QuickTP:       rapid.SampledFrom([]int{0, 1, 2, 4}).Draw(t, "placement_tp"),
		QuickPP:       rapid.SampledFrom([]int{0, 1, 2}).Draw(t, "placement_pp"),
		QuickDP:       rapid.SampledFrom([]int{0, 1, 2}).Draw(t, "quick_dp"),
		TemplateTP:    rapid.SampledFrom([]int{0, 1, 2}).Draw(t, "template_tp"),
		TemplateDP:    rapid.SampledFrom([]int{0, 1, 2}).Draw(t, "template_dp"),
		Sleepable:     rapid.Bool().Draw(t, "sleepable"),
		VRAMLimitGiB:  rapid.SampledFrom([]float64{0, 0, 20, 40}).Draw(t, "vram_limit_gib"),
		TargetGPUs:    rapid.IntRange(1, 4).Draw(t, "target_gpus"),
		PeerGPUs:      rapid.IntRange(1, 4).Draw(t, "peer_gpus"),
		CardMemoryGiB: rapid.SampledFrom([]int64{24, 48, 80}).Draw(t, "card_memory_gib"),
		BusyTarget:    rapid.Bool().Draw(t, "busy_target"),
		TargetDGPU:    rapid.Bool().Draw(t, "target_dgpu"),
	}
	return w
}

func (w acquisitionWorld) declared(provider string) bool {
	for _, source := range w.Sources {
		if strings.EqualFold(source.Provider, provider) {
			return true
		}
	}
	return false
}

// undeclaredProvider is a public source the spec does not declare: the other
// hub when the spec declares one, else an HTTP-registered model.
func (w acquisitionWorld) undeclaredProvider() string {
	for _, provider := range worldTemplateProviders {
		if !w.declared(provider) {
			return provider
		}
	}
	return "url"
}

func (w acquisitionWorld) weightSourcesJSON() string {
	type source struct {
		Provider string `json:"provider"`
		RepoID   string `json:"repo_id"`
		Revision string `json:"revision,omitempty"`
	}
	out := make([]source, len(w.Sources))
	for i, s := range w.Sources {
		out[i] = source{Provider: s.Provider, RepoID: worldTemplateRepo}
		if s.Pinned {
			out[i].Revision = worldTemplateRevision
		}
	}
	raw, _ := json.Marshal(out)
	return string(raw)
}

func (w acquisitionWorld) selectedModel() store.Model {
	model := store.Model{Name: "selected", WeightBytes: worldSpecWeightBytes}
	switch w.SelectedSource {
	case sourceLocal:
		model.Source = "local"
	case sourceDeclared:
		model.Source = w.Sources[w.SelectedAt].Provider
	default:
		model.Source = w.undeclaredProvider()
	}
	switch w.SelectedID {
	case idIdentical:
		model.SourceID = worldTemplateRepo
	case idCaseOnly:
		model.SourceID = strings.ToLower(worldTemplateRepo)
	default:
		model.SourceID = "deploy-fp8-weights-dir"
	}
	switch w.SelectedRev {
	case revisionTemplate:
		model.Revision = worldTemplateRevision
	case revisionOther:
		model.Revision = "other-rev"
	}
	return model
}

func (w acquisitionWorld) String() string {
	return fmt.Sprintf("sources=%v selected=%s/%s/rev=%s copy=%s decoy=%t provider=%s names_model=%t shape=%+v",
		w.Sources, sourceKindNames[w.SelectedSource], sourceIDKindNames[w.SelectedID], revisionKindNames[w.SelectedRev],
		copyKindNames[w.Copy], w.Decoy, requestProviderNames[w.RequestProvider], w.NamesModel, w.Shape)
}

// worldFixture is a drawn world materialized in its own SQLite store.
type worldFixture struct {
	st           *store.Store
	svc          *Service
	target, peer *store.Node
	nfs          *store.Storage
	template     *store.InferenceModel
	selected     *store.Model
	decoy        *store.Model
}

var worldSerial atomic.Int64

// openWorldStore opens a fresh in-memory store for one iteration. The first
// open in the process records the migrated schema; every later one replays
// it, which keeps an iteration's store at a few tens of milliseconds.
func openWorldStore(t *rapid.T) *store.Store {
	st, err := store.Open(fmt.Sprintf("file:property_world_%d?mode=memory&cache=shared", worldSerial.Add(1)), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// buildWorld materializes w in a fresh store. The caller closes the returned
// fixture's store.
func buildWorld(t *rapid.T, w acquisitionWorld) *worldFixture {
	st := openWorldStore(t)
	var err error
	f := &worldFixture{st: st}
	f.target = worldNode(t, st, worldTargetNode, w.Shape.TargetGPUs, w.Shape.CardMemoryGiB, w.Shape.BusyTarget, w.Shape.TargetDGPU)
	f.peer = worldNode(t, st, worldPeerNode, w.Shape.PeerGPUs, w.Shape.CardMemoryGiB, false, false)
	online := map[string]bool{f.target.UUID: true, f.peer.UUID: true}
	f.svc = New(Deps{Store: st, Online: func(uuid string) bool { return online[uuid] }})
	f.nfs, err = st.CreateStorage(&store.Storage{Name: "team-models", Type: proto.StorageNFS, Enabled: true, AllNodes: true,
		Path: "/mnt/models", Export: "nfs.example.internal:/models"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyStorageStatus(f.target.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{
		{ID: f.nfs.ID, State: proto.StorageStateReady, Path: "/mnt/models", Writable: true},
	}}); err != nil {
		t.Fatal(err)
	}
	catalog := deploymentFlatFixture()
	catalog.Specs[0].WeightSources = w.weightSourcesJSON()
	catalog.Specs[0].WeightBytes = worldSpecWeightBytes
	catalog.Specs[0].Args = templateArgsJSON(w.Shape)
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{catalog}); err != nil {
		t.Fatal(err)
	}
	if f.template, err = st.InferenceModelByModelID(catalog.ModelID); err != nil {
		t.Fatal(err)
	}
	selected := w.selectedModel()
	if f.selected, err = st.UpsertModel(&selected); err != nil {
		t.Fatal(err)
	}
	f.placeCopy(t, w.Copy, f.selected)
	if w.Decoy {
		decoy := store.Model{Name: "decoy", Source: w.Sources[0].Provider, SourceID: worldTemplateRepo, WeightBytes: worldSpecWeightBytes}
		if w.Sources[0].Pinned {
			decoy.Revision = worldTemplateRevision
		}
		if f.decoy, err = st.UpsertModel(&decoy); err != nil {
			t.Fatal(err)
		}
		if f.decoy.ID == f.selected.ID {
			f.decoy = nil
		} else {
			f.placeCopy(t, copyTargetLocal, f.decoy)
		}
	}
	return f
}

func worldNode(t *rapid.T, st *store.Store, name string, gpus int, memoryGiB int64, busy, dgpu bool) *store.Node {
	node, err := st.RegisterNode(name, store.HashToken(name), name)
	if err != nil {
		t.Fatal(err)
	}
	status := proto.NodeStatus{Sys: proto.SysInfo{Hostname: name, Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4", RuntimeDriver: proto.RuntimePreferenceDocker}}
	for index := 0; index < gpus; index++ {
		card := proto.GPUInfo{Index: index, UUID: fmt.Sprintf("GPU-%s-%d", name, index), Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90",
			ComputeCapability: "9.0", MemoryTotalBytes: memoryGiB << 30, Healthy: true}
		if busy && index == 0 {
			card.ForeignProcesses = []proto.CardProcess{{PID: 4242, Name: "python", MemoryBytes: (memoryGiB - 4) << 30}}
		}
		status.GPUs = append(status.GPUs, card)
	}
	if dgpu {
		// What an agent with the NVIDIA dGPU assets reports once every card
		// passed the behavioral partition probe.
		status.Sys.PartitionAssets = []proto.AssetReport{{Backend: accelerator.BackendNVIDIALogical, Root: "/assets", Files: []string{"libvgpu.so"}}}
		status.Sys.PartitionSafetyProtocol = 1
		for index := 0; index < gpus; index++ {
			status.Sys.PartitionProbes = append(status.Sys.PartitionProbes, proto.PartitionProbeResult{Backend: accelerator.BackendNVIDIALogical, GPUIndex: &index, Status: proto.PartitionProbePassed})
		}
	}
	if err := st.UpdateNodeStatus(name, status); err != nil {
		t.Fatal(err)
	}
	if node, err = st.NodeByID(node.ID); err != nil {
		t.Fatal(err)
	}
	return node
}

// placeCopy records where model's bytes are, the way agents and storage
// scans report them.
func (f *worldFixture) placeCopy(t *rapid.T, kind copyKind, model *store.Model) {
	var file *store.ModelFile
	switch kind {
	case copyTargetLocal:
		file = &store.ModelFile{ModelID: model.ID, StorageID: f.st.DefaultLocalStorageID(), NodeID: f.target.ID, Path: "/models/target-copy", State: proto.MFPresent, SizeBytes: model.WeightBytes}
	case copyPeerOnly:
		file = &store.ModelFile{ModelID: model.ID, StorageID: f.st.DefaultLocalStorageID(), NodeID: f.peer.ID, Path: "/models/peer-copy", State: proto.MFPresent, SizeBytes: model.WeightBytes}
	case copyShared:
		file = &store.ModelFile{ModelID: model.ID, StorageID: f.nfs.ID, Path: "Acme/shared-copy", State: proto.MFPresent, SizeBytes: model.WeightBytes}
	case copyFailedLeftover:
		file = &store.ModelFile{ModelID: model.ID, StorageID: f.nfs.ID, Path: "Acme/failed-copy", State: proto.MFFailed, Message: "revision returned HTTP 404"}
	default:
		return
	}
	if _, err := f.st.UpsertModelFile(file); err != nil {
		t.Fatal(err)
	}
}

func templateArgsJSON(shape launchShape) string {
	args := []string{"--max-model-len", "32768"}
	if shape.TemplateTP > 0 {
		args = append(args, "--tensor-parallel-size", fmt.Sprint(shape.TemplateTP))
	}
	if shape.TemplateDP > 0 {
		args = append(args, "--data-parallel-size", fmt.Sprint(shape.TemplateDP))
	}
	raw, _ := json.Marshal(args)
	return string(raw)
}

// request is the create body the drawn world sends; evaluate previews the
// same body.
func (f *worldFixture) request(w acquisitionWorld) dto.DeployRequest {
	req := dto.DeployRequest{Name: "property", TemplateID: f.template.ID,
		TemplateSpecSource: "upstream", TemplateSpecName: "fp8-balanced", QuickParams: map[string]any{}, Placement: map[string]any{}}
	if w.NamesModel {
		req.ModelID = f.selected.ID
	}
	switch w.RequestProvider {
	case requestDeclaredProvider:
		req.ModelSource = &dto.ModelSource{Provider: w.Sources[w.RequestAt].Provider}
	case requestUndeclaredProvider:
		req.ModelSource = &dto.ModelSource{Provider: w.undeclaredProvider()}
	}
	shape := w.Shape
	if shape.Automatic {
		req.PlacementMode, req.TargetVendor = "auto", "nvidia"
	} else {
		req.NodeUUID = f.target.UUID
	}
	if shape.Replicas > 1 {
		req.Replicas = shape.Replicas
	}
	if shape.QuickTP > 0 {
		req.Placement["tp"] = shape.QuickTP
	}
	if shape.QuickPP > 0 {
		req.Placement["pp"] = shape.QuickPP
	}
	if shape.QuickDP > 0 {
		req.QuickParams["data_parallel_size"] = shape.QuickDP
	}
	req.Sleepable = shape.Sleepable
	req.VRAMLimitGiB = shape.VRAMLimitGiB
	return req
}

// rowSnapshot is every model row and model-file row, the persistent facts a
// substitution or an unasked download would leave behind.
type rowSnapshot struct {
	models []uint
	files  []string
}

func (f *worldFixture) snapshot(t *rapid.T) rowSnapshot {
	models, err := f.st.ListModels()
	if err != nil {
		t.Fatal(err)
	}
	files, err := f.st.ListModelFiles()
	if err != nil {
		t.Fatal(err)
	}
	var out rowSnapshot
	for _, model := range models {
		out.models = append(out.models, model.ID)
	}
	for _, file := range files {
		out.files = append(out.files, fmt.Sprintf("model=%d storage=%d node=%d state=%s path=%s", file.ModelID, file.StorageID, file.NodeID, file.State, file.Path))
	}
	sort.Slice(out.models, func(i, j int) bool { return out.models[i] < out.models[j] })
	sort.Strings(out.files)
	return out
}

func (s rowSnapshot) equal(other rowSnapshot) bool {
	return fmt.Sprint(s.models) == fmt.Sprint(other.models) && fmt.Sprint(s.files) == fmt.Sprint(other.files)
}

// newFiles are the model-file rows other has that s does not.
func (s rowSnapshot) newFiles(other rowSnapshot) []string {
	seen := map[string]bool{}
	for _, file := range s.files {
		seen[file] = true
	}
	var out []string
	for _, file := range other.files {
		if !seen[file] {
			out = append(out, file)
		}
	}
	return out
}

// usableOn asks the store's own source rule whether node reads model now,
// without fetching it from anywhere.
func (f *worldFixture) usableOn(t *rapid.T, model *store.Model, node *store.Node) bool {
	src, err := f.st.ModelSourceOn(model.ID, node, store.SourceRules{})
	if err != nil {
		t.Fatal(err)
	}
	return src.Action == store.SourceReady
}

// stableCode is the product code a refusal carries: the ProductError's own,
// never the FIELD_INVALID wrapper evaluation gives an untyped error.
func stableCode(err error) (string, bool) {
	var product *messages.ProductError
	if errors.As(err, &product) {
		return product.Code(), true
	}
	return "", false
}

// stableFieldErrors reports whether every evaluation field error carries a
// typed code. evaluateFieldErrors.add turns an untyped error into
// FIELD_INVALID whose only param is the error text; that is a message, not a
// code a client can act on.
func stableFieldErrors(errs []dto.FieldError) bool {
	for _, item := range errs {
		if item.Code == "" {
			return false
		}
		if _, detail := item.Params["detail"]; item.Code == messages.ErrorFieldInvalid && detail && len(item.Params) == 1 {
			return false
		}
	}
	return true
}

func fieldErrorCodes(errs []dto.FieldError) string {
	codes := make([]string, 0, len(errs))
	for _, item := range errs {
		codes = append(codes, item.Field+":"+item.Code)
	}
	sort.Strings(codes)
	return strings.Join(codes, ",")
}

// outcomeTally counts the outcome classes a property's iterations reached.
type outcomeTally struct {
	counts map[string]int
	total  int
}

func (o *outcomeTally) add(class string) {
	if o.counts == nil {
		o.counts = map[string]int{}
	}
	o.counts[class]++
	o.total++
}

func (o *outcomeTally) log(t *testing.T, name string) {
	classes := make([]string, 0, len(o.counts))
	for class := range o.counts {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	for _, class := range classes {
		t.Logf("%s: %-60s %4d (%.0f%%)", name, class, o.counts[class], 100*float64(o.counts[class])/float64(max(o.total, 1)))
	}
}
