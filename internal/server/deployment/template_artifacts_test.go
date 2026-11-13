package deployment

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// unknownRevisionRig is GWKS-99: a provider CLI downloaded the repository into
// an NFS storage, the scan registered it without a revision, and a template
// pins that repository at a commit.
type unknownRevisionRig struct {
	*saveToRig
	imported  *store.Model
	selection templateSelection
}

const (
	unknownRevisionSpecBytes = int64(30890049597)
	unknownRevisionRepo      = "Qwen/Qwen3.8-27B-FP8"
	unknownRevisionPin       = "416b6c4c4ff599f76304db98ae7b5518973df13c"
)

func newUnknownRevisionRig(t *testing.T, importedBytes int64) *unknownRevisionRig {
	t.Helper()
	r := &unknownRevisionRig{saveToRig: newSaveToRig(t)}
	imported, err := r.st.UpsertModel(&store.Model{Source: "modelscope", SourceID: unknownRevisionRepo, Revision: "", Name: unknownRevisionRepo, WeightBytes: importedBytes})
	if err != nil {
		t.Fatal(err)
	}
	r.imported = imported
	if _, err := r.st.UpsertModelFile(&store.ModelFile{ModelID: imported.ID, StorageID: r.nfs.ID, Path: unknownRevisionRepo, State: proto.MFPresent,
		Acquisition: proto.ModelAcquireImported, SizeBytes: importedBytes, Downloaded: importedBytes}); err != nil {
		t.Fatal(err)
	}
	r.selection = templateSelection{NodeID: r.node.ID, Model: &store.InferenceModel{ModelID: "qwen/qwen3.8-27b"}, Spec: &store.InferenceSpecRow{
		Engine: "vllm", Quantization: "fp8", WeightBytes: unknownRevisionSpecBytes,
		WeightSources: `[{"provider":"huggingface","repo_id":"` + unknownRevisionRepo + `","revision":"017b9c7af6b5689d5dd426a76e0bc077eb5ca20a"},` +
			`{"provider":"modelscope","repo_id":"` + unknownRevisionRepo + `","revision":"` + unknownRevisionPin + `"}]`}}
	if err := r.h.selectExplicitWeight(&r.selection, &dto.ModelSource{Provider: "modelscope"}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *unknownRevisionRig) resolve(t *testing.T) *store.Model {
	t.Helper()
	model, err := r.h.resolveDeploymentModelFromTemplate(context.Background(), 0, &r.selection, 1)
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func (r *unknownRevisionRig) modelCount(t *testing.T) int {
	t.Helper()
	models, err := r.st.ListModels()
	if err != nil {
		t.Fatal(err)
	}
	return len(models)
}

// A request that chose the provider reuses the imported copy the node can
// mount instead of registering the pinned revision and downloading it again
// beside the copy; the sizes differ only by the provider's manifest files.
func TestExplicitProviderReusesUnknownRevisionCopyOnNode(t *testing.T) {
	r := newUnknownRevisionRig(t, unknownRevisionSpecBytes-2326)
	r.nfsState(t, proto.StorageStateReady, true)
	before := r.modelCount(t)
	got := r.resolve(t)
	if got.ID != r.imported.ID {
		t.Fatalf("model=%d revision=%q, want the imported copy %d", got.ID, got.Revision, r.imported.ID)
	}
	if r.selection.Weight.Revision != "" || r.selection.Weight.Provider != "modelscope" {
		t.Fatalf("snapshot weight=%+v, want the mounted identity", r.selection.Weight)
	}
	if after := r.modelCount(t); after != before {
		t.Fatalf("models %d -> %d, want no new registration", before, after)
	}
	// The pinned row an earlier evaluate registered does not outrank the copy.
	pinned, err := r.st.UpsertModel(&store.Model{Source: "modelscope", SourceID: unknownRevisionRepo, Revision: unknownRevisionPin})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.resolve(t); got.ID != r.imported.ID {
		t.Fatalf("model=%d, want the imported copy over pinned row %d", got.ID, pinned.ID)
	}
}

func TestExplicitProviderKeepsPinnedRevisionWithoutAUsableCopy(t *testing.T) {
	for name, tc := range map[string]struct {
		importedBytes int64
		nfsState      string
	}{
		"size differs beyond bookkeeping files": {unknownRevisionSpecBytes - 64<<20, proto.StorageStateReady},
		"imported size unknown":                 {0, proto.StorageStateReady},
		"node cannot mount the storage":         {unknownRevisionSpecBytes, proto.StorageStateFailed},
	} {
		t.Run(name, func(t *testing.T) {
			r := newUnknownRevisionRig(t, tc.importedBytes)
			r.nfsState(t, tc.nfsState, true)
			got := r.resolve(t)
			if got.ID == r.imported.ID || got.Source != "modelscope" || got.Revision != unknownRevisionPin {
				t.Fatalf("model=%+v, want the pinned modelscope revision", got)
			}
		})
	}
}

func TestSameWeightSize(t *testing.T) {
	for _, tc := range []struct {
		actual, expected int64
		want             bool
	}{
		{30890047271, 30890049597, true},
		{30890049597 + 29<<20, 30890049597, true},
		{30890049597 + 40<<20, 30890049597, false},
		{100, 100 + 1<<20, true},
		{100, 100 + 1<<20 + 1, false},
		{0, 100, false},
		{100, 0, false},
	} {
		if got := sameWeightSize(tc.actual, tc.expected); got != tc.want {
			t.Fatalf("sameWeightSize(%d, %d)=%v, want %v", tc.actual, tc.expected, got, tc.want)
		}
	}
}
