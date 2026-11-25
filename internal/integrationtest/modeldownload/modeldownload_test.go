package modeldownload

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/download"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

func TestRegisterMetadataThenDownloadTinyShards(t *testing.T) {
	hub := testenv.FakeModelHub(t)
	meta, err := modelmeta.Fetch(context.Background(), modelrepo.SourceHF, hub.Repo, hub.Revision, "", hub.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if meta.WeightBytes != hub.WeightBytes {
		t.Fatalf("metadata weight bytes = %d, want declared %d", meta.WeightBytes, hub.WeightBytes)
	}
	root := t.TempDir()
	var downloaded int64
	var progressCalls int
	for i := 1; i <= 3; i++ {
		shard := fmt.Sprintf("model-%05d-of-00003.safetensors", i)
		dest := filepath.Join(root, shard)
		url := modelrepo.ResolveURL(modelrepo.SourceHF, hub.Repo, hub.Revision, shard, hub.Endpoint)
		err = download.Download(context.Background(), download.Options{URL: url, Dest: dest, Concurrency: 4}, func(p download.Progress) {
			progressCalls++
		})
		if err != nil {
			t.Fatal(err)
		}
		if st, statErr := os.Stat(dest); statErr != nil || st.Size() != 8<<20 {
			t.Fatalf("downloaded shard %d = %v, want 8 MiB", i, statErr)
		}
		if _, statErr := os.Stat(dest + ".part"); !os.IsNotExist(statErr) {
			t.Fatalf("download partial marker for shard %d remains: %v", i, statErr)
		}
		downloaded += 8 << 20
	}
	if downloaded != 24<<20 || progressCalls == 0 {
		t.Fatalf("downloaded=%d progress_calls=%d, want 24 MiB and progress", downloaded, progressCalls)
	}
}

// Optional release validation for the manually published ModelScope fixture.
// It is intentionally outside the default offline pyramid and only runs when
// the operator explicitly opts into network access.
func TestOptionalRealModelScopeFakeModel(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_NET") != "1" {
		t.Skip("set GPUWORKS_TEST_NET=1 to validate the published ModelScope fixture")
	}
	// Public model hosts can route a small shard through a cold CDN/Xet
	// backend. Keep the opt-in check bounded, but do not make a transiently
	// slow mirror look like a broken model (the shard is intentionally 8 MiB,
	// not a metadata-only probe).
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// The public ModelScope account is zstack; zstackai is the historical local
	// fixture identifier used by older offline tests.
	const repo = "zstack/qwen3-4b-fake"
	meta, err := modelmeta.Fetch(ctx, modelrepo.SourceMS, repo, "main", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if meta.WeightBytes != 8<<30 {
		t.Fatalf("published model weight bytes=%d, want declared 8 GiB", meta.WeightBytes)
	}
	dest := filepath.Join(t.TempDir(), "published-shard.safetensors")
	url := modelrepo.ResolveURL(modelrepo.SourceMS, repo, "main", "model-00001-of-00003.safetensors", "")
	if err := download.Download(ctx, download.Options{URL: url, Dest: dest, Concurrency: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dest); err != nil || st.Size() == 0 {
		t.Fatalf("published shard stat=%v", err)
	}
}

// TestOptionalRealHuggingFaceFakeModel is disabled because Hugging Face is not
// reliably reachable from domestic CI runners. The RunPod acceptance path
// covers the external model-download workflow on a real cloud node.
func TestOptionalRealHuggingFaceFakeModel(t *testing.T) {
	t.Skip("external model downloading is covered by the RunPod acceptance test; domestic CI cannot reliably reach Hugging Face")
	// Hugging Face may serve the shard from an Xet redirect whose first byte
	// is quick but whose sustained throughput is variable. This is an explicit
	// network acceptance test, so allow a bounded transfer window large enough
	// to distinguish slow egress from a missing repository.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	const repo = "zsatckai/qwen3-4b-fake"
	meta, err := modelmeta.Fetch(ctx, modelrepo.SourceHF, repo, "main", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if meta.WeightBytes != 8<<30 {
		t.Fatalf("published model weight bytes=%d, want declared 8 GiB", meta.WeightBytes)
	}
	dest := filepath.Join(t.TempDir(), "published-shard.safetensors")
	url := modelrepo.ResolveURL(modelrepo.SourceHF, repo, "main", "model-00001-of-00003.safetensors", "")
	if err := download.Download(ctx, download.Options{URL: url, Dest: dest, Concurrency: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dest); err != nil || st.Size() == 0 {
		t.Fatalf("published shard stat=%v", err)
	}
}

func TestFakeHubResumesPartialShard(t *testing.T) {
	hub := testenv.FakeModelHub(t)
	dest := filepath.Join(t.TempDir(), "resumed.safetensors")
	part := dest + ".part"
	const prefix = 128 << 10
	if err := os.WriteFile(part, make([]byte, prefix), 0o644); err != nil {
		t.Fatal(err)
	}
	url := modelrepo.ResolveURL(modelrepo.SourceHF, hub.Repo, hub.Revision, "model-00001-of-00003.safetensors", hub.Endpoint)
	if err := download.Download(context.Background(), download.Options{URL: url, Dest: dest, Concurrency: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dest); err != nil || st.Size() != 8<<20 {
		t.Fatalf("resumed shard = %v, want 8 MiB", err)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatalf("partial marker remains after resume: %v", err)
	}
}

func TestModelRegistrationUsesFakeHubThroughRealApp(t *testing.T) {
	hub := testenv.FakeModelHub(t)
	env := testenv.NewWithModelEndpoint(t, hub.Endpoint)
	api := env.API(t)
	body, _ := json.Marshal(map[string]any{
		"source": "huggingface", "source_id": hub.Repo, "revision": hub.Revision, "name": "qwen3-4b-fake",
	})
	resp, err := api.PostJSON("/api/v1/models", body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("model registration status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	models, err := env.Store().ListModels()
	if err != nil || len(models) != 1 {
		t.Fatalf("registered models=%+v err=%v", models, err)
	}
	if models[0].WeightBytes != hub.WeightBytes || models[0].Meta == "" || models[0].Arch != "Qwen3ForCausalLM" {
		t.Fatalf("registration metadata=%+v", models[0])
	}
}

func TestModelDownloadRunsThroughDesiredStateAndCompletionMarker(t *testing.T) {
	hub := testenv.FakeModelHub(t)
	env := testenv.NewWithModelEndpoint(t, hub.Endpoint)
	node := env.AddNode("model-download-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "model-download-node")
	rows, err := env.Store().ListNodes()
	if err != nil || len(rows) != 1 {
		t.Fatalf("nodes=%+v err=%v", rows, err)
	}
	api := env.API(t)
	body, _ := json.Marshal(map[string]any{
		"source": "huggingface", "source_id": hub.Repo, "revision": hub.Revision,
		"name": "node-download-model",
	})
	created, err := api.PostJSON("/api/v1/models", body)
	if err != nil || created.StatusCode != http.StatusCreated {
		t.Fatalf("register model status=%v err=%v", responseStatus(created), err)
	}
	var model struct {
		ID uint `json:"id"`
	}
	if err := json.NewDecoder(created.Body).Decode(&model); err != nil {
		created.Body.Close()
		t.Fatal(err)
	}
	created.Body.Close()
	if model.ID == 0 {
		t.Fatal("registered model id is zero")
	}
	downloadReq, _ := json.Marshal(map[string]any{"node_uuid": rows[0].UUID})
	started, err := api.PostJSON(fmt.Sprintf("/api/v1/models/%d/download", model.ID), downloadReq)
	if err != nil || started.StatusCode != http.StatusAccepted {
		t.Fatalf("start download status=%v err=%v", responseStatus(started), err)
	}
	started.Body.Close()
	var mf store.ModelFile
	env.Eventually(t, 30*time.Second, func() bool {
		got, getErr := env.Store().ModelFileByModelNode(model.ID, rows[0].ID)
		if getErr != nil {
			return false
		}
		mf = *got
		return mf.State == proto.MFPresent || mf.State == proto.MFFailed
	})
	if mf.State != proto.MFPresent {
		t.Fatalf("node download failed: %+v", mf)
	}
	// The fixture index advertises an 8 GiB metadata total while the actual
	// repository files are tiny. Completion must be based on the validated file
	// manifest, not index.metadata.total_size.
	if mf.Downloaded <= 0 || mf.Downloaded != mf.SizeBytes || mf.SizeBytes >= hub.WeightBytes {
		t.Fatalf("completion bytes=%d/%d, want validated repository total below index declaration %d", mf.Downloaded, mf.SizeBytes, hub.WeightBytes)
	}
	_ = node
}

func TestModelScopeEmptyRevisionPinsRegistrationAndDownload(t *testing.T) {
	hub := testenv.FakeModelHub(t)
	env := testenv.NewWithModelEndpoint(t, hub.Endpoint)
	env.AddNode("modelscope-download-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "modelscope-download-node")
	rows, err := env.Store().ListNodes()
	if err != nil || len(rows) != 1 {
		t.Fatalf("nodes=%+v err=%v", rows, err)
	}

	api := env.API(t)
	body, _ := json.Marshal(map[string]any{
		"source": modelrepo.SourceMS, "source_id": hub.Repo, "revision": "", "name": "modelscope-default",
	})
	created, err := api.PostJSON("/api/v1/models", body)
	if err != nil || created.StatusCode != http.StatusCreated {
		t.Fatalf("register model status=%v err=%v", responseStatus(created), err)
	}
	var model struct {
		ID       uint   `json:"id"`
		Revision string `json:"revision"`
	}
	if err := json.NewDecoder(created.Body).Decode(&model); err != nil {
		created.Body.Close()
		t.Fatal(err)
	}
	created.Body.Close()
	if model.ID == 0 || model.Revision != hub.CommitSHA {
		t.Fatalf("registered model = %+v, want revision pinned at %s", model, hub.CommitSHA)
	}

	downloadReq, _ := json.Marshal(map[string]any{"node_uuid": rows[0].UUID})
	started, err := api.PostJSON(fmt.Sprintf("/api/v1/models/%d/download", model.ID), downloadReq)
	if err != nil || started.StatusCode != http.StatusAccepted {
		t.Fatalf("start download status=%v err=%v", responseStatus(started), err)
	}
	started.Body.Close()
	var mf store.ModelFile
	env.Eventually(t, 30*time.Second, func() bool {
		got, getErr := env.Store().ModelFileByModelNode(model.ID, rows[0].ID)
		if getErr != nil {
			return false
		}
		mf = *got
		return mf.State == proto.MFPresent || mf.State == proto.MFFailed
	})
	if mf.State != proto.MFPresent || mf.Revision != hub.CommitSHA {
		t.Fatalf("ModelScope default-branch download = %+v, want present at %s", mf, hub.CommitSHA)
	}
}

func TestPeerModelSyncRunsThroughTwoRealAgents(t *testing.T) {
	env := testenv.New(t)
	source := env.AddNode("model-peer-source", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	target := env.AddNode("model-peer-target", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A10", VRAMMB: 16384}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, source.Name)
	env.WaitNodeOnline(t, target.Name)
	// Online is reported before the first GPU metadata update can arrive. On
	// a race-enabled or cold-build run, a single ListNodes immediately after
	// WaitNodeOnline can therefore contain one node without its GPU row yet.
	// Wait for the complete node/GPU projection before resolving the two peers.
	var nodes []store.Node
	env.Eventually(t, 10*time.Second, func() bool {
		rows, listErr := env.Store().ListNodes()
		if listErr != nil || len(rows) != 2 {
			return false
		}
		for _, row := range rows {
			with, getErr := env.Store().NodeWithGPUs(row.UUID)
			if getErr != nil || len(with.GPUs) == 0 {
				return false
			}
		}
		nodes = rows
		return true
	})
	findNode := func(testNode *testenv.Node) store.Node {
		t.Helper()
		prefix := "GPU-" + testNode.Name + "-"
		for _, row := range nodes {
			with, getErr := env.Store().NodeWithGPUs(row.UUID)
			if getErr == nil && len(with.GPUs) > 0 && strings.HasPrefix(with.GPUs[0].UUID, prefix) {
				return *with
			}
		}
		t.Fatalf("node %s not found in %+v", testNode.Name, nodes)
		return store.Node{}
	}
	sourceRow, targetRow := findNode(source), findNode(target)
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "peer/model", Revision: "main", Name: "peer-model",
		Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceDir := t.TempDir()
	config := []byte(`{"model_type":"qwen3"}`)
	if err := os.WriteFile(filepath.Join(sourceDir, "config.json"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	weights := bytes.Repeat([]byte("peer-weight-"), 128)
	if err := os.WriteFile(filepath.Join(sourceDir, "model.safetensors"), weights, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().RegisterModelFile(store.FileRef{ModelID: model.ID, NodeID: sourceRow.ID}, sourceDir, int64(len(config)+len(weights))); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	peerBody, _ := json.Marshal(map[string]string{"source_node": sourceRow.UUID})
	started, err := api.PostJSON(fmt.Sprintf("/api/v1/models/%d/nodes/%s/sync", model.ID, targetRow.UUID), peerBody)
	if err != nil || started.StatusCode != http.StatusAccepted {
		data := []byte{}
		if started != nil {
			data, _ = io.ReadAll(started.Body)
			started.Body.Close()
		}
		t.Fatalf("peer sync status=%v body=%s err=%v", responseStatus(started), data, err)
	}
	started.Body.Close()
	var targetMF store.ModelFile
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		got, getErr := env.Store().ModelFileByModelNode(model.ID, targetRow.ID)
		if getErr == nil {
			targetMF = *got
			if targetMF.State == proto.MFPresent || targetMF.State == proto.MFFailed {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if targetMF.State != proto.MFPresent || targetMF.Acquisition != proto.ModelAcquirePeer || targetMF.TransferredBytes <= int64(len(weights)) || targetMF.Downloaded != targetMF.SizeBytes {
		t.Fatalf("peer sync target model file did not complete: %+v", targetMF)
	}
	if targetMF.Path == "" {
		t.Fatal("peer sync returned an empty target path")
	}
	if got, err := os.ReadFile(filepath.Join(targetMF.Path, "model.safetensors")); err != nil || !bytes.Equal(got, weights) {
		t.Fatalf("target archive contents err=%v bytes=%d", err, len(got))
	}
}

func responseStatus(resp *http.Response) any {
	if resp == nil {
		return "<nil>"
	}
	return resp.StatusCode
}
