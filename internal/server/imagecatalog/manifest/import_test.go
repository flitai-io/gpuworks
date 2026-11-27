package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"testing"
)

type importSink struct {
	series       map[string]SeriesState
	images       map[string]ImageState
	seriesInputs []SeriesInput
	imageInputs  []ImageInput
	reconciled   map[string]struct{}
}

func (s *importSink) FindSeries(name string) (SeriesState, error) { return s.series[name], nil }
func (s *importSink) FindImage(ref string) (ImageState, error)    { return s.images[ref], nil }
func (s *importSink) UpsertSeries(in SeriesInput) error {
	s.seriesInputs = append(s.seriesInputs, in)
	s.series[in.Name] = SeriesState{Exists: true, Source: "official"}
	return nil
}
func (s *importSink) UpsertImage(in ImageInput) error {
	s.imageInputs = append(s.imageInputs, in)
	s.images[in.ImageRef] = ImageState{Exists: true, Source: "official"}
	return nil
}
func (s *importSink) ReconcileMissingOfficial(seen map[string]struct{}, _ string) (int, int, error) {
	s.reconciled = seen
	return 2, 1, nil
}

// Created and Updated count version rows by whether the row existed before
// the import; the reconcile counts come from the sink.
func TestImportOfficialCountsAndCarriesManifestFields(t *testing.T) {
	sink := &importSink{
		series: map[string]SeriesState{"vLLM": {Exists: true, Source: "official"}},
		images: map[string]ImageState{"acme/runner:kept": {Exists: true, Source: "official"}},
	}
	m := Manifest{SchemaVersion: 2, CatalogVersion: "v4", Series: []ManifestSeries{{
		Name: "vLLM", Engine: "vllm", Framework: "cuda", Purpose: "inference",
		Versions: []ManifestVersion{
			{ImageRef: "acme/runner:new", RegistrySources: []string{"mirror.example/acme/runner:new"}, BlobSHA256: "deadbeef"},
			{ImageRef: "acme/runner:kept"},
		},
	}}}
	result, err := ImportOfficial(m, sink, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 1 || result.Updated != 1 || result.Deleted != 2 || result.Deprecated != 1 || result.CatalogVersion != "v4" {
		t.Fatalf("result = %+v", result)
	}
	if len(sink.seriesInputs) != 1 {
		t.Fatalf("series inputs = %+v", sink.seriesInputs)
	}
	if len(sink.imageInputs) != 2 {
		t.Fatalf("image inputs = %+v", sink.imageInputs)
	}
	got := sink.imageInputs[0]
	if got.BlobSHA256 != "deadbeef" || len(got.RegistrySources) != 1 {
		t.Fatalf("manifest fields were not carried through: %+v", got)
	}
}

func TestImportOfficialRejectsCommunityConflictBeforeWrites(t *testing.T) {
	sink := &importSink{series: map[string]SeriesState{"shared": {Exists: true, Source: "community"}}, images: map[string]ImageState{}}
	_, err := ImportOfficial(Manifest{SchemaVersion: 2, CatalogVersion: "v1", Series: []ManifestSeries{{Name: "shared", Versions: []ManifestVersion{{ImageRef: "x/y:v1"}}}}}, sink, ImportOptions{})
	if err == nil {
		t.Fatal("expected source conflict")
	}
	if len(sink.seriesInputs) != 0 || len(sink.imageInputs) != 0 {
		t.Fatalf("conflict wrote rows: series=%+v images=%+v", sink.seriesInputs, sink.imageInputs)
	}
}

// The startup sync must never be blocked by an operator who already named a
// community or custom row like a catalog entry: the conflicting entries are
// reported and left out, everything else is imported, and the left-out refs are
// not handed to reconciliation as if they were missing official rows. A manual
// import of the same manifest fails before writing anything.
func TestImportOfficialSkipConflicts(t *testing.T) {
	manifest := Manifest{SchemaVersion: 3, CatalogVersion: "2026-10-08.x", Series: []ManifestSeries{
		{Name: "mine", Engine: "vllm", Versions: []ManifestVersion{{ImageRef: "acme/a:v1"}}},
		{Name: "vLLM · CUDA", Engine: "vllm", Versions: []ManifestVersion{{ImageRef: "acme/b:v1"}, {ImageRef: "acme/shared:v1"}, {ImageRef: "acme/c:v1", Name: "c-v1", Notes: "note"}}},
	}}
	newSink := func() *importSink {
		return &importSink{
			series: map[string]SeriesState{"mine": {Exists: true, Source: "custom"}},
			images: map[string]ImageState{"acme/shared:v1": {Exists: true, Source: "community"}},
		}
	}
	sink := newSink()
	result, err := ImportOfficial(manifest, sink, ImportOptions{SkipConflicts: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Skipped) != 2 || result.Skipped[0].Kind != "series" || result.Skipped[0].Name != "mine" || result.Skipped[1].Kind != "image" || result.Skipped[1].Name != "acme/shared:v1" {
		t.Fatalf("skipped = %+v", result.Skipped)
	}
	if result.Created != 2 || result.Updated != 0 {
		t.Fatalf("result = %+v", result)
	}
	want := map[string]struct{}{"acme/b:v1": {}, "acme/c:v1": {}}
	if !reflect.DeepEqual(sink.reconciled, want) {
		t.Fatalf("seen = %v, want %v", sink.reconciled, want)
	}
	if len(sink.seriesInputs) != 1 || sink.seriesInputs[0].Name != "vLLM · CUDA" {
		t.Fatalf("series written = %+v", sink.seriesInputs)
	}
	if got := sink.imageInputs[1]; got.Name != "c-v1" || got.Notes != "note" {
		t.Fatalf("version name/notes not carried: %+v", got)
	}
	if got := sink.imageInputs[0]; got.Name != "vLLM · CUDA" {
		t.Fatalf("a version without a name must fall back to the series name: %+v", got)
	}

	strict := newSink()
	if _, err := ImportOfficial(manifest, strict, ImportOptions{}); err == nil {
		t.Fatal("manual import must fail on a conflict")
	}
	if len(strict.seriesInputs) != 0 || len(strict.imageInputs) != 0 || strict.reconciled != nil {
		t.Fatalf("a failed import wrote rows: %+v %+v %v", strict.seriesInputs, strict.imageInputs, strict.reconciled)
	}
}

// A manifest written before ADR 0031 still carries run_spec and command_mode
// per version. They are not launch configuration any more: the manifest
// parses, and the keys reach nothing.
func TestParseIgnoresRetiredLaunchDefaults(t *testing.T) {
	m, err := Parse([]byte(`{"schema_version":2,"catalog_version":"v1","series":[{"name":"vLLM","engine":"vllm","versions":[{"image_ref":"acme/runner:v1","run_spec":{"env":{"HF_HUB_OFFLINE":"1"},"docker_args":["--ipc=host"]},"command_mode":"engine"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(m.Series[0].Versions[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"image_ref":"acme/runner:v1"}` {
		t.Fatalf("retired keys survived parsing: %s", encoded)
	}
}

// A manifest written for an older release still declares vendors per series
// and GPU labels per version. They are not catalog data: the manifest parses,
// its sha256 still verifies, and the keys reach nothing.
func TestParseIgnoresLegacyAdaptationKeys(t *testing.T) {
	const plain = `{"schema_version":2,"catalog_version":"v1","series":[{"name":"vLLM","engine":"vllm","framework":"cuda","versions":[{"image_ref":"acme/runner:v1"}]}]}`
	want, err := Parse([]byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	legacy := `{"schema_version":2,"catalog_version":"v1","sha256":"` + hex.EncodeToString(sum[:]) + `","series":[{"name":"vLLM","engine":"vllm","framework":"cuda","vendors":["ascend"],"versions":[{"image_ref":"acme/runner:v1","vendors":["ascend"],"supported_gpus":["h100"],"unsupported_gpus":["sm70"]}]}]}`
	got, err := Parse([]byte(legacy))
	if err != nil {
		t.Fatal(err)
	}
	got.SHA256 = ""
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy manifest = %+v, want %+v", got, want)
	}
}
