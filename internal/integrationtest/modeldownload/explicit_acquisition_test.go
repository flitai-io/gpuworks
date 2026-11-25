package modeldownload

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// countingHub proxies the fake model hub and records the Host header of every
// request, which is how a test tells a mirrored download from a direct one.
type countingHub struct {
	*httptest.Server
	mu    sync.Mutex
	hosts []string
}

func newCountingMirror(t *testing.T, upstream string) *countingHub {
	t.Helper()
	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	mirror := &countingHub{}
	mirror.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirror.mu.Lock()
		mirror.hosts = append(mirror.hosts, r.Host)
		mirror.mu.Unlock()
		proxied := *r.URL
		proxied.Scheme, proxied.Host = target.Scheme, target.Host
		request, err := http.NewRequestWithContext(r.Context(), r.Method, proxied.String(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		request.Header = r.Header.Clone()
		request.Host = target.Host
		response, err := http.DefaultTransport.RoundTrip(request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for key, values := range response.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	t.Cleanup(mirror.Server.Close)
	return mirror
}

func (h *countingHub) requestCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.hosts)
}

// A site rule for Hugging Face reaches the agent as the download task's
// candidates and the transfer goes through the mirror it names, without
// anything configured on the node.
func TestASiteRuleRoutesTheDownloadThroughItsMirror(t *testing.T) {
	hub := testenv.FakeModelHub(t)
	mirror := newCountingMirror(t, hub.Endpoint)
	// The server itself has no model endpoint: only the rule may redirect
	// this download.
	env := testenv.New(t)
	env.AddNode("explicit-network-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "explicit-network-node")
	rows, err := env.Store().ListNodes()
	if err != nil || len(rows) != 1 {
		t.Fatalf("nodes=%+v err=%v", rows, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: hub.Repo, Revision: hub.Revision, Name: "explicit-network-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	rule, _ := json.Marshal(map[string]any{"match": "https://huggingface.co", "endpoints": []string{mirror.URL}, "allow_upstream": false})
	created, err := api.PostJSON("/api/v1/pull/source-rules", rule)
	if err != nil || created.StatusCode != http.StatusCreated {
		t.Fatalf("create rule status=%v err=%v", responseStatus(created), err)
	}
	created.Body.Close()
	downloadReq, _ := json.Marshal(map[string]any{"node_uuid": rows[0].UUID})
	started, err := api.PostJSON(fmt.Sprintf("/api/v1/models/%d/download", model.ID), downloadReq)
	if err != nil || started.StatusCode != http.StatusAccepted {
		t.Fatalf("start download status=%v err=%v", responseStatus(started), err)
	}
	started.Body.Close()
	var mf store.ModelFile
	env.Eventually(t, 60*time.Second, func() bool {
		got, getErr := env.Store().ModelFileByModelNode(model.ID, rows[0].ID)
		if getErr != nil {
			return false
		}
		mf = *got
		return mf.State == proto.MFPresent || mf.State == proto.MFFailed
	})
	if mf.State != proto.MFPresent {
		t.Fatalf("explicit-mirror download failed: %+v", mf)
	}
	if mirror.requestCount() == 0 {
		t.Fatal("the download did not go through the selected mirror")
	}
	mirrorHost, err := url.Parse(mirror.URL)
	if err != nil {
		t.Fatal(err)
	}
	mirror.mu.Lock()
	hosts := append([]string(nil), mirror.hosts...)
	mirror.mu.Unlock()
	for _, host := range hosts {
		if host != mirrorHost.Host {
			t.Fatalf("request reached the mirror with Host %q, want %q", host, mirrorHost.Host)
		}
	}
}

// Without a rule a download goes to the source's own site and nowhere else:
// no global endpoint setting exists any more, and the store leaves where a
// node downloads from to the hub's replay plan.
func TestDownloadWithoutARuleGoesToTheSourcesOwnSite(t *testing.T) {
	hub := testenv.FakeModelHub(t)
	env := testenv.New(t)
	env.AddNode("default-network-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "default-network-node")
	rows, err := env.Store().ListNodes()
	if err != nil || len(rows) != 1 {
		t.Fatalf("nodes=%+v err=%v", rows, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: hub.Repo, Revision: hub.Revision, Name: "default-network-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().PrepareModelDownload(store.FileRef{ModelID: model.ID, NodeID: rows[0].ID}, store.ModelArtifactPath(model)); err != nil {
		t.Fatal(err)
	}
	desired, err := env.Store().DesiredStateForNode(rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(desired.ModelFiles) != 1 {
		t.Fatalf("desired model files = %+v", desired.ModelFiles)
	}
	task := desired.ModelFiles[0]
	if task.Endpoint != "" || task.Candidates != nil {
		t.Fatalf("the store chose a network for a download: %+v", task)
	}
	planned, refused := planning.PlanModelFileTasks(nil, &rows[0], desired.ModelFiles)
	if len(refused) != 0 || len(planned) != 1 || planned[0].Endpoint != "https://huggingface.co" ||
		len(planned[0].Candidates) != 1 || !planned[0].Candidates[0].Upstream {
		t.Fatalf("planned without rules = %+v, refused %v", planned, refused)
	}
}

// A deployment names no acquisition: a node that already holds the model uses
// it, nothing is downloaded, and nothing about where the model comes from is
// recorded on the deployment.
func TestDeployUsesTheLocalCopyWithoutAChoice(t *testing.T) {
	hub := testenv.FakeModelHub(t)
	env := testenv.New(t)
	env.AddNode("acquisition-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 81920}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "acquisition-node")
	rows, err := env.Store().ListNodes()
	if err != nil || len(rows) != 1 {
		t.Fatalf("nodes=%+v err=%v", rows, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: hub.Repo, Revision: hub.Revision, Name: "acquisition-model",
		Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 28,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The model is already complete on the node, so no download may be
	// scheduled.
	if _, err := env.Store().RegisterModelFile(store.FileRef{ModelID: model.ID, NodeID: rows[0].ID}, t.TempDir(), 1<<28); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	body, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": rows[0].UUID, "name": "acquisition-deployment",
		"engine": "vllm",
	})
	created, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	defer created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(created.Body)
		t.Fatalf("deployment status=%v body=%s", responseStatus(created), data)
	}
	var deployment struct {
		ID                       uint           `json:"id"`
		CatalogSelectionSnapshot map[string]any `json:"catalog_selection_snapshot"`
	}
	if err := json.NewDecoder(created.Body).Decode(&deployment); err != nil {
		t.Fatal(err)
	}
	if _, recorded := deployment.CatalogSelectionSnapshot["acquisition"]; recorded {
		t.Fatalf("snapshot records an acquisition: %+v", deployment.CatalogSelectionSnapshot)
	}
	// A local copy must not have been turned into a download.
	file, err := env.Store().ModelFileByModelNode(model.ID, rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if file.Acquisition == proto.ModelAcquireDownload {
		t.Fatalf("a local copy scheduled a download: %+v", file)
	}
	_ = fmt.Sprint(deployment.ID)
}

// A storage the request names that no longer exists is rejected with a
// re-evaluate signal rather than silently replaced with another one.
func TestDeployRequestRejectsAStorageThatIsGone(t *testing.T) {
	hub := testenv.FakeModelHub(t)
	env := testenv.NewWithModelEndpoint(t, hub.Endpoint)
	env.AddNode("stale-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 81920}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "stale-node")
	rows, err := env.Store().ListNodes()
	if err != nil || len(rows) != 1 {
		t.Fatalf("nodes=%+v err=%v", rows, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: hub.Repo, Revision: hub.Revision, Name: "stale-model",
		Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 28,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	body, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": rows[0].UUID, "name": "stale-deployment",
		"engine": "vllm", "model_source": map[string]any{"storage_id": 9999},
	})
	created, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	defer created.Body.Close()
	if created.StatusCode != http.StatusConflict {
		t.Fatalf("missing storage status=%v, want 409", responseStatus(created))
	}
	var failure struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(created.Body).Decode(&failure); err != nil {
		t.Fatal(err)
	}
	if failure.Code != "ACQUISITION_CANDIDATE_STALE" {
		t.Fatalf("error code = %q, want ACQUISITION_CANDIDATE_STALE", failure.Code)
	}
}
