package imagecatalog

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/manifest"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// oldGPUStackSeed is the 72-row gpustack/runner catalog every install before
// the gpuworks-runner matrix was seeded with.
func oldGPUStackSeed(t *testing.T) manifest.Manifest {
	t.Helper()
	raw, err := os.ReadFile("testdata/catalog-seed-gpustack-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func seedRefs(t *testing.T) map[string]struct{} {
	t.Helper()
	m, err := Seed()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]struct{}{}
	for _, series := range m.Series {
		for _, version := range series.Versions {
			out[version.ImageRef] = struct{}{}
		}
	}
	return out
}

// An install upgraded from the gpustack catalog: one old row is named by a
// deployment, one by an unfinished instance, one by a model, and one only
// pulled onto a node. The sync deletes every other old row (with its node rows
// and emptied series), keeps those four as older versions (deprecated), and
// keeps the archive URL an operator registered on one of them.
func TestSyncSeedReplacesGPUStackCatalog(t *testing.T) {
	st, err := store.Open("file:image_sync_upgrade?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// The pre-upgrade catalog: the old seed, and nothing from the new.
	old := oldGPUStackSeed(t)
	if _, err := manifest.ImportOfficial(old, manifestSink{store: st}, manifest.ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	var oldCount int64
	st.DB().Model(&store.Image{}).Where("source = ?", "official").Count(&oldCount)
	if oldCount != 72 {
		t.Fatalf("old catalog rows = %d, want 72", oldCount)
	}

	const (
		byDeployment = "gpustack/runner:cuda12.9-vllm0.25.1"
		byInstance   = "gpustack/runner:cuda13.0-vllm0.24.0"
		byModel      = "gpustack/runner:cuda13.0-vllm0.22.1"
		byStopped    = "gpustack/runner:cuda13.0-vllm0.22.0"
		onNode       = "gpustack/runner:cuda12.9-vllm0.24.0"
		archiveURL   = "https://mirror.example/runner-cuda12.9-vllm0.25.1.tar"
	)
	deployed, err := st.ImageByRef(byDeployment)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateImageRegistrySources(deployed.ID, encodeSources(t, append(decodeSources(t, deployed.RegistrySources), archiveURL))); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.Deployment{Name: "uses-old-image", State: "running", Image: byDeployment}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.Instance{UUID: "inst-running", State: proto.InstRunning, Image: byInstance}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.Instance{UUID: "inst-stopped", State: proto.InstStopped, Image: byStopped}).Error; err != nil {
		t.Fatal(err)
	}
	modelImage, err := st.ImageByRef(byModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.Model{Name: "bound-to-old-image", ImageID: modelImage.ID}).Error; err != nil {
		t.Fatal(err)
	}
	stopped, err := st.ImageByRef(byStopped)
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("node-sync", store.HashToken("t"), "sync")
	if err != nil {
		t.Fatal(err)
	}
	// A node that failed to pull the stopped instance's image does not hold it.
	if _, err := st.UpsertImageNode(&store.ImageNode{ImageVersionID: stopped.ID, NodeID: node.ID, State: "failed"}); err != nil {
		t.Fatal(err)
	}
	pulled, err := st.ImageByRef(onNode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImageNode(&store.ImageNode{ImageVersionID: pulled.ID, NodeID: node.ID, State: store.ImageStatePresent}); err != nil {
		t.Fatal(err)
	}

	result, err := SyncSeed(st)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 36 || result.Updated != 0 || result.Deleted != 68 || result.Deprecated != 4 || len(result.Skipped) != 0 {
		t.Fatalf("sync result = %+v", result)
	}

	counts, err := st.ImageReferenceCounts([]string{byDeployment, byInstance, byModel, byStopped})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{byDeployment, byInstance, byModel} {
		row, err := st.ImageByRef(ref)
		if err != nil {
			t.Fatalf("referenced row %s was deleted: %v", ref, err)
		}
		if !row.Deprecated || row.DeprecatedSince != result.CatalogVersion {
			t.Fatalf("referenced row %s = %+v", ref, row)
		}
		if counts[ref] != 1 {
			t.Fatalf("reference count of %s = %d, want 1", ref, counts[ref])
		}
	}
	if counts[byStopped] != 0 {
		t.Fatalf("a stopped instance counted as a reference: %v", counts)
	}
	// An image a node holds stays visible as an older version, though nothing
	// references it (and deleting it is not refused).
	if row, err := st.ImageByRef(onNode); err != nil || !row.Deprecated || row.DeprecatedSince != result.CatalogVersion {
		t.Fatalf("pulled row = %+v, %v", row, err)
	}
	if held, _ := st.ImageReferenceCounts([]string{onNode}); held[onNode] != 0 {
		t.Fatalf("a node holding an image counted as a reference: %v", held)
	}
	if _, err := st.ImageByRef(byStopped); err != store.ErrNotFound {
		t.Fatalf("unreferenced row survived: %v", err)
	}
	var nodeRows int64
	st.DB().Model(&store.ImageNode{}).Where("image_version_id = ?", stopped.ID).Count(&nodeRows)
	if nodeRows != 0 {
		t.Fatalf("node rows of a deleted version survived: %d", nodeRows)
	}
	deployed, err = st.ImageByRef(byDeployment)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(deployed.RegistrySources, archiveURL) {
		t.Fatalf("archive URL lost on the deprecated row: %s", deployed.RegistrySources)
	}

	var official []store.Image
	st.DB().Where("source = ? AND deprecated = ?", "official", false).Find(&official)
	want := seedRefs(t)
	if len(official) != len(want) {
		t.Fatalf("active system rows = %d, want %d", len(official), len(want))
	}
	for _, row := range official {
		if _, ok := want[row.ImageRef]; !ok {
			t.Fatalf("unexpected active system row %s", row.ImageRef)
		}
	}
	// Old series that lost every version are gone; the ones still holding a
	// referenced row stay.
	var seriesNames []string
	st.DB().Model(&store.ImageSeries{}).Where("source = ?", "official").Order("name").Pluck("name", &seriesNames)
	joined := "," + strings.Join(seriesNames, ",") + ","
	for _, gone := range []string{"vLLM Ascend", "SGLang", "SGLang Ascend", "vLLM Omni", "EvalScope", "vLLM Hygon", "vLLM PPU", "SGLang PPU"} {
		if strings.Contains(joined, ","+gone+",") {
			t.Fatalf("emptied series %q survived: %v", gone, seriesNames)
		}
	}
	if !strings.Contains(joined, ",vLLM,") || !strings.Contains(joined, ",vLLM · CUDA,") {
		t.Fatalf("series = %v", seriesNames)
	}

	// A second start changes nothing but rewriting the same 25 rows.
	again, err := SyncSeed(st)
	if err != nil {
		t.Fatal(err)
	}
	if again.Created != 0 || again.Updated != 36 || again.Deleted != 0 || again.Deprecated != 0 {
		t.Fatalf("second sync = %+v", again)
	}
}

// The sync runs on every start, so it must never undo an operator's switch.
func TestSyncSeedKeepsOperatorSwitches(t *testing.T) {
	st, err := store.Open("file:image_sync_switches?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := SyncSeed(st); err != nil {
		t.Fatal(err)
	}
	version, err := st.ImageByRef("gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetImageEnabled(version.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetImageSeriesEnabled(version.SeriesID, false); err != nil {
		t.Fatal(err)
	}
	result, err := SyncSeed(st)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 0 || result.Deleted != 0 || result.Deprecated != 0 {
		t.Fatalf("restart sync = %+v", result)
	}
	version, err = st.ImageByRef(version.ImageRef)
	if err != nil {
		t.Fatal(err)
	}
	series, err := st.GetImageSeries(version.SeriesID)
	if err != nil {
		t.Fatal(err)
	}
	if version.Enabled || series.Enabled {
		t.Fatalf("operator switches were reset: version=%v series=%v", version.Enabled, series.Enabled)
	}
}

// A custom series that took a catalog series' name, and a community row on a
// catalog ref, are left alone by the startup sync, which skips those entries
// instead of refusing to start.
func TestSyncSeedSkipsConflicts(t *testing.T) {
	st, err := store.Open("file:image_sync_conflicts?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.UpsertImageSeries(&store.ImageSeries{Name: "SGLang · DTK", Engine: "sglang", Framework: "dtk", Source: "custom", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImage(&store.Image{ImageRef: "gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2", Engine: "sglang", Framework: "cuda", Source: "community", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	result, err := SyncSeed(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Skipped) != 2 || result.Created != 34 {
		t.Fatalf("result = %+v", result)
	}
	community, err := st.ImageByRef("gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2")
	if err != nil || community.Source != "community" || community.Deprecated {
		t.Fatalf("community row = %+v, %v", community, err)
	}
}

// The embedded seed is the gpuworks-runner matrix: every version is an
// immutable runner tag with its two public copies (quay, SWR) and nothing
// else - an internal registry is an installation's own rule - every accelerator framework the
// product supports has images, and no launch defaults or vendor labels ride
// along (the generator itself is tested in cmd/gpuworks-image-manifest).
func TestBuiltinSeedIsRunnerMatrix(t *testing.T) {
	var seed manifest.Manifest
	if err := json.Unmarshal(seedJSON, &seed); err != nil {
		t.Fatal(err)
	}
	frameworks := map[string]bool{}
	for _, series := range seed.Series {
		frameworks[series.Framework] = true
		if len(series.CommonArgs) != 0 || len(series.CommonEnv) != 0 {
			t.Fatalf("seed series %s carries common args/env", series.Name)
		}
		for _, image := range series.Versions {
			if !strings.HasPrefix(image.ImageRef, "gpuworks/gpuworks-runner:") || len(image.RegistrySources) != 2 || strings.Contains(strings.Join(image.RegistrySources, " "), "registry.zstack.ai") {
				t.Fatalf("seed %s is not a runner tag with its mirrors: %+v", image.ImageRef, image.RegistrySources)
			}
		}
	}
	for _, key := range []string{`"vendors"`, `"supported_gpus"`, `"unsupported_gpus"`} {
		if strings.Contains(string(seedJSON), key) {
			t.Fatalf("seed carries %s", key)
		}
	}
	for _, framework := range []string{"cuda", "cann", "hggc", "dtk", "metal"} {
		if !frameworks[framework] {
			t.Fatalf("seed is missing required framework %q", framework)
		}
	}
}

func decodeSources(t *testing.T, raw string) []string {
	t.Helper()
	var out []string
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func encodeSources(t *testing.T, sources []string) string {
	t.Helper()
	b, err := json.Marshal(sources)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
