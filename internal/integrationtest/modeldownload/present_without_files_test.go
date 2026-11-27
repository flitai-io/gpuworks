package modeldownload

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// staleCopy is a model the server records as synced to a node's own disk at
// a directory that is not there: the dead state of GWKS-517 and GWKS-501.
type staleCopy struct {
	env   *testenv.Env
	node  store.Node
	model *store.Model
	gone  string
}

// newStaleCopy records the copy once the node is online, so nothing has
// handed the row to the agent yet; record false leaves the row to the caller.
func newStaleCopy(t *testing.T, endpoint, repo, revision string, record bool) *staleCopy {
	t.Helper()
	env := testenv.NewWithModelEndpoint(t, endpoint)
	env.AddNode("stale-copy-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 81920}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "stale-copy-node")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: repo, Revision: revision, Name: "stale-copy-model",
		Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 28,
		Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The record names a directory inside a storage root that still holds a
	// model, which is what the node reports as gone; an empty root is what a
	// mount that dropped looks like, and the node would not call that missing
	// (TestAModelPathThatComesBackKeepsItsCopyThroughTheAbsence).
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "modelscope", "another-model"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &staleCopy{env: env, node: nodes[0], model: model, gone: filepath.Join(root, "modelscope", "never-written")}
	if record {
		c.record(t)
	}
	return c
}

func (c *staleCopy) record(t *testing.T) {
	t.Helper()
	if _, err := c.env.Store().UpsertModelFile(&store.ModelFile{
		ModelID: c.model.ID, NodeID: c.node.ID, State: proto.MFPresent, Path: c.gone, SizeBytes: 1 << 28,
		Acquisition: proto.ModelAcquirePeer, TransferID: "transfer-of-the-lost-copy",
	}); err != nil {
		t.Fatal(err)
	}
}

func (c *staleCopy) deploy(t *testing.T) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model_id": c.model.ID, "node_uuid": c.node.UUID, "served_name": "stale", "quick_params": map[string]any{"max_model_len": 512}})
	resp, err := c.env.API(t).PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create deployment status = %d", resp.StatusCode)
	}
}

func (c *staleCopy) instances(t *testing.T) []store.Instance {
	t.Helper()
	instances, err := c.env.Store().ListInstances()
	if err != nil {
		t.Fatal(err)
	}
	return instances
}

// The agent refuses the launch because the directory is not there. That is
// the node's word that the copy is gone: the record is taken back, the model
// is fetched again and the deployment comes up, with the one refused launch
// on record.
func TestALaunchThatFindsNoModelDirectoryFetchesTheModelAgain(t *testing.T) {
	hub := testenv.FakeModelHub(t)
	c := newStaleCopy(t, hub.Endpoint, hub.Repo, hub.Revision, true)
	c.deploy(t)

	var running *store.Instance
	c.env.Eventually(t, 90*time.Second, func() bool {
		for _, inst := range c.instances(t) {
			if inst.State == proto.InstRunning {
				running = &inst
				return true
			}
		}
		return false
	})
	refused := 0
	for _, inst := range c.instances(t) {
		if failure := inst.Failure(); failure != nil {
			if failure.Code != proto.ErrorModelPathMissing || failure.Resource != c.gone {
				t.Fatalf("failed attempt = %+v, want %s on %s", failure, proto.ErrorModelPathMissing, c.gone)
			}
			refused++
		}
	}
	if refused != 1 {
		t.Fatalf("%d launches were refused for the missing directory, want 1", refused)
	}
	file, err := c.env.Store().ModelFileByModelNode(c.model.ID, c.node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if file.State != proto.MFPresent || file.Acquisition != proto.ModelAcquireDownload || file.Path == c.gone {
		t.Fatalf("the copy the instance runs on = %+v, want a fresh download", file)
	}
	_ = running
}

// When the source no longer has the model, the same refusal ends in a failed
// download the deployment shows, not in further launches on a directory that
// is not there. (A source that does not answer is retried by the download
// itself for minutes before it fails; that policy is the download's own.)
func TestALaunchThatFindsNoModelDirectoryStopsAtAFailedDownload(t *testing.T) {
	source := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(source.Close)
	endpoint := source.URL
	c := newStaleCopy(t, endpoint, "zstackai/qwen3-4b-fake", "main", true)
	c.deploy(t)

	var file *store.ModelFile
	if !c.env.EventuallyNoFail(90*time.Second, func() bool {
		got, err := c.env.Store().ModelFileByModelNode(c.model.ID, c.node.ID)
		if err != nil {
			file = nil
			return false
		}
		file = got
		return got.State == proto.MFFailed
	}) {
		t.Fatalf("the model's row on the node = %+v, instances = %+v", file, c.instances(t))
	}
	if file.Acquisition != proto.ModelAcquireDownload || file.Path == c.gone {
		t.Fatalf("failed row = %+v, want the download that replaced the lost copy", file)
	}
	// Nothing launches while the model is not on the node.
	time.Sleep(2 * time.Second)
	for _, inst := range c.instances(t) {
		if inst.State == proto.InstRunning {
			t.Fatalf("an instance runs without the model: %+v", inst)
		}
	}
	refused := 0
	for _, inst := range c.instances(t) {
		if inst.Failure() != nil {
			refused++
		}
	}
	if refused != 1 {
		t.Fatalf("%d launches were refused, want the one that found the directory missing", refused)
	}
}

// An agent that is handed its node's rows on connecting looks for each on
// disk. A peer-synced copy that is not there is reported, and the server
// takes the record back without any deployment having to fail on it.
func TestAReconnectingAgentReportsASyncedCopyThatIsGone(t *testing.T) {
	c := newStaleCopy(t, "", "peer/model", "main", false)
	c.env.StopNode(t, "stale-copy-node")
	c.record(t)
	c.env.RestartNode(t, "stale-copy-node")
	c.env.Eventually(t, 30*time.Second, func() bool {
		_, err := c.env.Store().ModelFileByModelNode(c.model.ID, c.node.ID)
		return err != nil
	})
}

// A mount that goes away leaves its empty mount point behind: the model path
// reads as absent while the files are fine. That is not the node's word that
// the copy is gone, so the record stays and, once the mount is back, the
// deployment runs on the same copy without a download.
func TestAModelPathThatComesBackKeepsItsCopyThroughTheAbsence(t *testing.T) {
	env := testenv.New(t)
	env.AddNode("transient-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 81920}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "transient-node")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	node := nodes[0]
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "peer/model", Revision: "main", Name: "transient-model",
		Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 20,
		Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The storage root a mount would be hung on: empty while the mount is
	// away, as a dropped NFS mount point is.
	root := t.TempDir()
	modelPath := filepath.Join(root, "models")
	if err := os.MkdirAll(modelPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelPath, "config.json"), []byte(`{"model_type":"qwen3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	row, err := env.Store().UpsertModelFile(&store.ModelFile{
		ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: modelPath, SizeBytes: 1 << 20,
		Acquisition: proto.ModelAcquirePeer, TransferID: "sync-of-the-copy",
	})
	if err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	deploy := func(servedName string) uint {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"model_id": model.ID, "node_uuid": node.UUID, "served_name": servedName, "quick_params": map[string]any{"max_model_len": 512}})
		resp, err := api.PostJSON("/api/v1/deployments", body)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			data, _ := io.ReadAll(resp.Body)
			t.Fatalf("create deployment status = %d body = %s", resp.StatusCode, data)
		}
		var created struct {
			ID uint `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
			t.Fatal(err)
		}
		return created.ID
	}
	instances := func() []store.Instance {
		t.Helper()
		rows, err := env.Store().ListInstances()
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	first := deploy("transient-first")
	env.Eventually(t, 90*time.Second, func() bool {
		for _, inst := range instances() {
			if inst.State == proto.InstRunning {
				return true
			}
		}
		return false
	})
	// The node has one card; the second deployment needs it free.
	del, err := api.Delete(fmt.Sprintf("/api/v1/deployments/%d", first))
	if err != nil {
		t.Fatal(err)
	}
	del.Body.Close()
	env.Eventually(t, 90*time.Second, func() bool {
		for _, inst := range instances() {
			if inst.State == proto.InstRunning {
				return false
			}
		}
		return true
	})

	// The mount goes away.
	hidden := filepath.Join(t.TempDir(), "models")
	if err := os.Rename(modelPath, hidden); err != nil {
		t.Fatal(err)
	}
	deploy("transient-second")
	if !env.EventuallyNoFail(90*time.Second, func() bool {
		for _, inst := range instances() {
			if failure := inst.Failure(); failure != nil && inst.State == proto.InstFailed {
				if failure.Code != proto.ErrorModelPathUnavailable {
					t.Fatalf("a launch without the mount failed with %+v, want %s", failure, proto.ErrorModelPathUnavailable)
				}
				return true
			}
		}
		return false
	}) {
		for _, inst := range instances() {
			t.Logf("instance %s: state=%s failure=%+v message=%q", inst.UUID, inst.State, inst.Failure(), inst.Message)
		}
		row, _ := env.Store().ModelFileByID(row.ID)
		t.Fatalf("no failed attempt on the vanished mount; row = %+v", row)
	}
	// The record is untouched: the node has not said the files are gone.
	after, err := env.Store().ModelFileByID(row.ID)
	if err != nil || after.State != proto.MFPresent || after.Acquisition != proto.ModelAcquirePeer || after.TransferID != "sync-of-the-copy" || after.Path != modelPath {
		t.Fatalf("row after the mount left = %+v, %v; want the same present copy", after, err)
	}

	// The mount comes back; the same copy serves the retried deployment.
	if err := os.Rename(hidden, modelPath); err != nil {
		t.Fatal(err)
	}
	env.Eventually(t, 120*time.Second, func() bool {
		for _, inst := range instances() {
			if inst.State == proto.InstRunning {
				return true
			}
		}
		return false
	})
	final, err := env.Store().ModelFileByID(row.ID)
	if err != nil || final.State != proto.MFPresent || final.Acquisition != proto.ModelAcquirePeer || final.Path != modelPath {
		t.Fatalf("row after the mount returned = %+v, %v; want the copy it was, downloaded nothing", final, err)
	}
}
