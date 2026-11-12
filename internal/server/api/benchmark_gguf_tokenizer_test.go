package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/proxy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// These references and HTTP outcomes come from GWKS-217's reproduction
// attachment. The small vocabulary checks source resolution and execution,
// not the real model's tokenizer accuracy or GPU performance.
const (
	ggufTokenizerWeightRepo = "ISTA-DASLab/Qwen3.8-Flash-Next-GSQ-RCO-GGUF"
	ggufTokenizerBaseRepo   = "Qwen/Qwen3.8-Flash-Next"
	ggufTokenizerRevision   = "2741eec155d03a8ce151b993ccce1a7b1e398d6b"
)

func TestBenchmarkGGUFDefaultTokenizerCompletesViaAPI(t *testing.T) {
	testBenchmarkGGUFTokenizerViaAPI(t, ggufTokenizerCase{source: "auto"})
}

func TestBenchmarkGGUFExplicitTokenizerCompletesViaAPI(t *testing.T) {
	testBenchmarkGGUFTokenizerViaAPI(t, ggufTokenizerCase{source: "modelscope", reference: ggufTokenizerBaseRepo + "@master"})
}

func TestBenchmarkGGUFOmittedTokenizerSourceUsesDeclaration(t *testing.T) {
	testBenchmarkGGUFTokenizerViaAPI(t, ggufTokenizerCase{})
}

func TestBenchmarkGGUFTokenizerSnapshotSurvivesCatalogRemoval(t *testing.T) {
	testBenchmarkGGUFTokenizerViaAPI(t, ggufTokenizerCase{source: "auto", frozen: true, removeCatalog: true})
}

func TestBenchmarkGGUFTokenizerNodeCopyPrecedesDeclaration(t *testing.T) {
	testBenchmarkGGUFTokenizerViaAPI(t, ggufTokenizerCase{source: "auto", nodeCopy: true})
}

func TestBenchmarkGGUFTokenizerRejectsUnrelatedOrInvalidDeclarations(t *testing.T) {
	for _, mismatch := range []string{"snapshot revision", "catalog weight revision", "spec name", "catalog unpinned tokenizer", "deviated runtime"} {
		t.Run(mismatch, func(t *testing.T) {
			testBenchmarkGGUFTokenizerViaAPI(t, ggufTokenizerCase{source: "auto", mismatch: mismatch})
		})
	}
}

type ggufTokenizerCase struct {
	source, reference, mismatch     string
	frozen, removeCatalog, nodeCopy bool
}

func testBenchmarkGGUFTokenizerViaAPI(t *testing.T, tc ggufTokenizerCase) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	f := newTokenizerFixture(t, strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()))
	model, err := f.admission.store.UpsertModel(&store.Model{
		Source: "modelscope", SourceID: ggufTokenizerWeightRepo,
		Revision: "1868010e27505d4b8cc15970cdcc4dd76005d7de", ArtifactSubfolder: "IQ2_XS",
		Name: ggufTokenizerWeightRepo, Operations: `[{"type":"openai.chat","inputs":[],"endpoint":"/v1/chat/completions","mode":"stream","request_encoding":"json"}]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.model = model
	// Template-derived models do not go through README metadata fetching.
	// Keep the deployed snapshot in its pre-fix form, as in the Jira evidence.
	catalog, err := seed.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range catalog.Models {
		if entry.ModelID != "ista-daslab/qwen3.8-flash-next-strata" {
			continue
		}
		rows, err := inferencecatalog.ToFlatStoreModels(inferencecatalog.FlatCatalog{Models: []inferencecatalog.InferenceModel{entry}})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.admission.store.ReconcileInferenceCatalog(rows); err != nil {
			t.Fatal(err)
		}
	}
	template, err := f.admission.store.InferenceModelByModelID("ista-daslab/qwen3.8-flash-next-strata")
	if err != nil {
		t.Fatal(err)
	}
	spec := template.Specs[0]
	selection := deploy.CatalogSelection{
		TemplateID: template.ID, ModelID: template.ModelID, SpecSource: spec.Source, SpecName: spec.Name,
		Engine: spec.Engine, Image: spec.Image, WeightProvider: model.Source,
		WeightRepoID: model.SourceID, WeightRevision: model.Revision, WeightSubfolder: model.ArtifactSubfolder,
		APIContract: json.RawMessage(`{"health_endpoint":"/health","operations":[{"type":"openai.chat"}]}`),
	}
	if tc.frozen {
		// Exercise the production snapshot writer, including the merge with
		// model-level operations. A hand-built API document would hide a lost
		// tokenizer field behind the current-catalog compatibility fallback.
		selection = deployment.SpecCatalogSelection(template, &spec, model)
	}
	switch tc.mismatch {
	case "snapshot revision":
		selection.WeightRevision = "a-different-artifact"
	case "catalog weight revision":
		spec.WeightSources = strings.ReplaceAll(spec.WeightSources, model.Revision, "a-different-artifact")
		template.Specs[0] = spec
		if err := f.admission.store.ReconcileInferenceCatalog([]store.InferenceModel{*template}); err != nil {
			t.Fatal(err)
		}
	case "spec name":
		selection.SpecName = "a-different-spec"
	case "catalog unpinned tokenizer":
		var contract map[string]any
		if err := json.Unmarshal([]byte(spec.APIContract), &contract); err != nil {
			t.Fatal(err)
		}
		contract["tokenizer_ref"] = "ms://" + ggufTokenizerBaseRepo + "@master"
		raw, err := json.Marshal(contract)
		if err != nil {
			t.Fatal(err)
		}
		spec.APIContract = string(raw)
		template.Specs[0] = spec
		if err := f.admission.store.ReconcileInferenceCatalog([]store.InferenceModel{*template}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := json.Marshal(selection)
	if err != nil {
		t.Fatal(err)
	}
	// As on the reported node, the model is present but neither independent
	// tokenizer file exists. The node stream fixture refuses unknown files.
	f.markPresent(t, "/opt/gpuworks/models/strata-gguf")
	if tc.nodeCopy {
		path := f.placeTokenizer(t, "/opt/gpuworks/models/strata-gguf")
		f.file[path] = []byte(`{"model":{"vocab":{"hello":0,"world":1,"test":2}}}`)
	}
	dep, err := f.admission.store.CreateDeployment(&store.Deployment{
		Name: "strata-gguf", ModelID: model.ID, Engine: "callable",
		ServedName: ggufTokenizerWeightRepo + "-xold", State: "active",
		TemplateID: template.ID, TemplateSpecName: selection.SpecName, TemplateSpecSource: spec.Source,
		Image: spec.Image, CatalogSelectionSnapshot: string(snapshot),
		TemplateDeviated: tc.mismatch == "deviated runtime",
	})
	if err != nil {
		t.Fatal(err)
	}
	if tc.removeCatalog {
		if err := f.admission.store.ReconcileInferenceCatalog([]store.InferenceModel{deploymentFlatFixture()}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.admission.store.CreateInstance(&store.Instance{
		UUID: "strata-gguf-instance", DeploymentID: dep.ID, NodeID: f.node.ID,
		State: proto.InstRunning, APIPort: 30000, GPUIndexes: `[0]`,
	}); err != nil {
		t.Fatal(err)
	}

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			// Strata does not offer tokenize/detokenize, including root paths.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"not found"}}`)
			return
		}
		var body struct {
			Model     string            `json:"model"`
			Stream    bool              `json:"stream"`
			MaxTokens int               `json:"max_tokens"`
			Messages  []json.RawMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != dep.ServedName || !body.Stream || body.MaxTokens != 16 || len(body.Messages) == 0 {
			http.Error(w, "invalid chat request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":118,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(engine.Close)

	// Only the external model repository is replaced. Unknown hosts and
	// paths fail closed, and a weight-repository vocabulary is always 404.
	oldClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Host != "modelscope.cn" {
			return nil, fmt.Errorf("unexpected repository request %s %s", r.Method, r.URL)
		}
		status, body := http.StatusNotFound, `{"Code":404,"Message":"file not found"}`
		switch r.URL.Path {
		case "/models/" + ggufTokenizerBaseRepo + "/resolve/master/tokenizer.json",
			"/models/" + ggufTokenizerBaseRepo + "/resolve/" + ggufTokenizerRevision + "/tokenizer.json":
			status, body = http.StatusOK, `{"model":{"vocab":{"hello":0,"world":1,"test":2}}}`
		case "/models/" + ggufTokenizerWeightRepo + "/resolve/main/tokenizer.json",
			"/models/" + ggufTokenizerWeightRepo + "/resolve/main/tiktoken.model",
			"/models/" + ggufTokenizerWeightRepo + "/resolve/master/tokenizer.json",
			"/models/" + ggufTokenizerWeightRepo + "/resolve/master/tiktoken.model":
		default:
			return nil, fmt.Errorf("unexpected repository path %s", r.URL.Path)
		}
		return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = oldClient })

	// Use the production gateway, authentication and API router. Only the
	// agent's TCP tunnel is replaced with a connection to the engine fixture.
	st := f.admission.store
	gateway := proxy.New(st, network.New(st, network.StoreCatalog{Store: st}), ggufTokenizerTunnel{
		node: f.node.UUID, addr: strings.TrimPrefix(engine.URL, "http://"),
	}, nil, 0)
	gateway.SetAccountant(proxy.NewAccountant(nil, nil))
	t.Cleanup(gateway.Close)
	server := httptest.NewUnstartedServer(nil)
	d := f.admission.handlers.d
	d.Server = StaticServerSettings{Loopback: "http://" + server.Listener.Addr().String()}
	d.PlaygroundV1, d.ProxyV1 = gateway.PlaygroundHandler(d.AISecretbox), gateway.Handler()
	server.Config.Handler = Router(d)
	server.Start()
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 5 * time.Second
	request := func(method, path, body string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+f.admission.token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, raw
	}
	input := map[string]any{
		"name": "Strata tokenizer verification", "target_kind": "instance", "deployment_id": dep.ID,
		"load_origin": "server", "tokenizer_source": tc.source,
		"dataset": map[string]any{"kind": "random", "input_tokens": 64, "output_tokens": 16, "ignore_eos": false},
		"mode":    "grid", "search": map[string]any{
			"concurrency": []int{1}, "isl": []int{64}, "osl": []int{16},
			"warmup_requests": 0, "requests_per_point_multiplier": 1,
		},
	}
	if tc.reference != "" {
		input["tokenizer_ref"] = tc.reference
	}
	if tc.source == "" {
		delete(input, "tokenizer_source")
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	status, raw := request(http.MethodPost, "/api/v1/benchmarks", string(body))
	if status != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", status, raw)
	}
	var created benchmarkDTO
	if err := json.Unmarshal(raw, &created); err != nil || created.ID == 0 {
		t.Fatalf("create body=%s err=%v", raw, err)
	}
	path := fmt.Sprintf("/api/v1/benchmarks/%d", created.ID)
	t.Cleanup(func() { request(http.MethodPost, path+"/cancel", "{}") })
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		status, raw := request(http.MethodGet, path, "")
		if status != http.StatusOK {
			t.Fatalf("get status=%d body=%s", status, raw)
		}
		var final benchmarkDTO
		if err := json.Unmarshal(raw, &final); err != nil {
			t.Fatal(err)
		}
		switch final.State {
		case "pending", "running", "queued":
		case "done":
			if tc.mismatch != "" {
				t.Fatalf("unrelated or invalid declaration admitted: %s", raw)
			}
			if len(final.Points) != 1 || final.Points[0].Success != 1 || final.Points[0].Errors != 0 {
				t.Fatalf("expected one successful measurement: %s", raw)
			}
			tail := final.Points[0].LongTail
			if tail == nil || tail.Evidence == nil || tail.Evidence.Execution.LocalTokenCountMethod != "local_tokenizer_estimate" {
				t.Fatalf("token-controlled workload lost its tokenizer: %s", raw)
			}
			if tc.reference != "" && final.TokenizerRef != "ms://"+tc.reference {
				t.Fatalf("explicit source was changed: %s", raw)
			}
			if tc.nodeCopy && !strings.Contains(final.TokenizerRef, "tokenizer-fetched-") {
				t.Fatalf("available node copy was not selected: %s", raw)
			}
			if tc.source == "auto" && !tc.nodeCopy && final.TokenizerRef != "engine:ms://"+ggufTokenizerBaseRepo+"@"+ggufTokenizerRevision {
				t.Fatalf("declared tokenizer provenance missing: %s", raw)
			}
			return
		default:
			if tc.mismatch != "" && final.State == "failed" && strings.Contains(final.Error, "HTTP 404") && len(final.Points) == 0 {
				return
			}
			t.Fatalf("expected completed evaluation with a usable tokenizer; state=%s tokenizer_ref=%s error=%s points=%d", final.State, final.TokenizerRef, final.Error, len(final.Points))
		}
		select {
		case <-timeout.C:
			t.Fatalf("evaluation did not finish: %s", raw)
		case <-tick.C:
		}
	}
}

type ggufTokenizerTunnel struct{ node, addr string }

func (t ggufTokenizerTunnel) Online(node string) bool { return node == t.node }

func (t ggufTokenizerTunnel) OpenTunnel(ctx context.Context, node, dst string) (net.Conn, error) {
	if node != t.node || dst != "127.0.0.1:30000" {
		return nil, fmt.Errorf("unexpected tunnel %s %s", node, dst)
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", t.addr)
}
