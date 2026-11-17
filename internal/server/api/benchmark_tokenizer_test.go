package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/bench/tokenizer"
)

// tokenizerFixture seeds one model on one node and the two seams the resolver
// uses: the fs.stat RPC and the bounded file stream.
type tokenizerFixture struct {
	admission  *benchmarkAdmission
	node       *store.Node
	model      *store.Model
	deployment *store.Deployment
	// stat answers fs.stat; a nil entry means "not there".
	statErr error
	stat    map[string]proto.FSStatResult
	// file is what the node serves over file.large_read, keyed by path.
	file        map[string][]byte
	laneOffline bool
	fetched     []string
}

func newTokenizerFixture(t *testing.T, dsn string) *tokenizerFixture {
	t.Helper()
	admission := newBenchmarkAdmission(t, dsn)
	node, err := admission.store.RegisterNode("tokenizer-node", store.HashToken("tokenizer-token"), "tokenizer-node")
	if err != nil {
		t.Fatal(err)
	}
	model, err := admission.store.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/model", Revision: "main", Name: "model", Operations: `[{"type":"openai.chat"}]`})
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := admission.store.CreateDeployment(&store.Deployment{Name: "tokenizer", ServedName: "tokenizer-served", State: "active", ModelID: model.ID})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &tokenizerFixture{
		admission:  admission,
		node:       node,
		model:      model,
		deployment: deployment,
		stat:       map[string]proto.FSStatResult{},
		file:       map[string][]byte{},
	}
	admission.handlers.d.CallNode = func(_ context.Context, _ string, method string, params any) (json.RawMessage, error) {
		if method != proto.RPCFSStat {
			return nil, fmt.Errorf("unexpected rpc %s", method)
		}
		if fixture.statErr != nil {
			return nil, fixture.statErr
		}
		request, _ := params.(proto.FSStatParams)
		result, ok := fixture.stat[request.Path]
		if !ok {
			return nil, fmt.Errorf("stat %s: no such file or directory", request.Path)
		}
		return json.Marshal(result)
	}
	admission.handlers.d.NodeStreams = &fakeNodeStreams{fixture: fixture}
	return fixture
}

// fakeNodeStreams serves the fixture's file map over a net.Pipe, which is what
// the resolver sees of a yamux stream: a net.Conn that ends at EOF.
type fakeNodeStreams struct {
	noStorageStreams
	fixture *tokenizerFixture
}

func (f *fakeNodeStreams) OpenLogtail(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("logtail is not part of this test")
}

func (*fakeNodeStreams) UpgradeAgent(string, string, string) (bool, error) {
	return false, errors.New("fakeNodeStreams does not upgrade agents")
}

func (f *fakeNodeStreams) OpenLargeFileRead(_ context.Context, nodeUUID, path string) (net.Conn, error) {
	if f.fixture.laneOffline {
		return nil, errors.New("node " + nodeUUID + " has no data lane for a file transfer")
	}
	content, ok := f.fixture.file[path]
	if !ok {
		// An agent that refuses the path closes the stream without a byte.
		server, client := net.Pipe()
		_ = server.Close()
		return client, nil
	}
	f.fixture.fetched = append(f.fixture.fetched, path)
	server, client := net.Pipe()
	go func() {
		_, _ = server.Write(content)
		_ = server.Close()
	}()
	return client, nil
}

// placeTokenizer records the model as present on the node at dir, and makes the
// node serve a loadable tokenizer.json from there.
func (f *tokenizerFixture) placeTokenizer(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "tokenizer.json")
	f.markPresent(t, dir)
	f.stat[path] = proto.FSStatResult{Path: path, SizeBytes: 32, Regular: true}
	f.file[path] = []byte(`{"model":{"vocab":{}}}`)
	return path
}

// markPresent records the model's presence row without claiming the file is
// there, which is how a directory that holds only weights looks.
func (f *tokenizerFixture) markPresent(t *testing.T, dir string) {
	t.Helper()
	if _, err := f.admission.store.UpsertModelFile(&store.ModelFile{ModelID: f.model.ID, NodeID: f.node.ID, Path: dir, State: proto.MFPresent}); err != nil {
		t.Fatal(err)
	}
}

// autoRequest is the automatic source as the create handler builds it for a
// managed target: the deployment whose model is being measured, and the node.
func (f *tokenizerFixture) autoRequest(loadOrigin string) benchmarkTokenizerRequest {
	return benchmarkTokenizerRequest{
		Source:     benchmarkTokenizerSourceAuto,
		TargetKind: "instance",
		LoadOrigin: loadOrigin,
		Deployment: f.deployment,
		Node:       f.node,
	}
}

// resolve returns the reference a source resolves to. The automatic source
// on a deployment that serves tokenize names the engine first; what the
// tests below check is the copy or repository it falls back to, so that
// part is returned.
func (f *tokenizerFixture) resolve(t *testing.T, req benchmarkTokenizerRequest) string {
	t.Helper()
	ref, err := f.admission.handlers.resolveBenchmarkTokenizer(context.Background(), req)
	if err != nil {
		t.Fatalf("resolve %q: %v", req.Source, err)
	}
	if req.Source == benchmarkTokenizerSourceAuto && req.Deployment != nil && !req.Deployment.TokenizeDisabled {
		fallback, ok := tokenizer.IsEngineRef(ref)
		if !ok {
			t.Fatalf("auto on a deployment serving tokenize = %q, want the engine first", ref)
		}
		return fallback
	}
	return ref
}

func (f *tokenizerFixture) repositoryRef(t *testing.T) string {
	t.Helper()
	model, err := f.admission.store.GetModel(f.model.ID)
	if err != nil {
		t.Fatal(err)
	}
	return tokenizerRefForModel(model)
}

func TestBenchmarkTokenizerAutoPrefersTheNodeCopy(t *testing.T) {
	f := newTokenizerFixture(t, "benchmark_tokenizer_auto_local")
	dir := t.TempDir()
	want := f.placeTokenizer(t, dir)

	request := f.autoRequest(f.node.UUID)
	if got := f.resolve(t, request); got != want {
		t.Fatalf("ref = %q, want %q", got, want)
	}
	if len(f.fetched) != 0 {
		t.Fatalf("a node load origin must not transfer anything: %v", f.fetched)
	}
}

func TestBenchmarkTokenizerAutoSkipsACopyTheNodeCannotConfirm(t *testing.T) {
	f := newTokenizerFixture(t, "benchmark_tokenizer_auto_missing")
	f.markPresent(t, t.TempDir())

	request := f.autoRequest(f.node.UUID)
	if got, want := f.resolve(t, request), f.repositoryRef(t); got != want {
		t.Fatalf("ref = %q, want the repository %q", got, want)
	}
}

func TestBenchmarkTokenizerAutoAdoptsWhenTheAgentCannotStat(t *testing.T) {
	f := newTokenizerFixture(t, "benchmark_tokenizer_auto_old_agent")
	dir := t.TempDir()
	f.markPresent(t, dir)
	// An agent that predates fs.stat refuses the method by name.
	f.statErr = proto.UnknownRPCMethod(proto.RPCFSStat)

	request := f.autoRequest(f.node.UUID)
	if got, want := f.resolve(t, request), filepath.Join(dir, "tokenizer.json"); got != want {
		t.Fatalf("ref = %q, want the recorded path %q", got, want)
	}
}

func TestBenchmarkTokenizerAutoJoinsARelativeModelPathWithTheModelsRoot(t *testing.T) {
	f := newTokenizerFixture(t, "benchmark_tokenizer_auto_relative")
	root := t.TempDir()
	if err := f.admission.store.UpdateNodeStatus(f.node.UUID, proto.NodeStatus{Sys: proto.SysInfo{Hostname: "tokenizer-node", ModelsRoot: root}}); err != nil {
		t.Fatal(err)
	}
	node, err := f.admission.store.NodeByUUID(f.node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	f.node = node
	want := f.placeTokenizer(t, filepath.Join(root, "hf", "org_model-main"))

	request := f.autoRequest(f.node.UUID)
	if got := f.resolve(t, request); got != want {
		t.Fatalf("ref = %q, want %q", got, want)
	}
}

func TestBenchmarkTokenizerAutoSkipsARelativePathWithoutAModelsRoot(t *testing.T) {
	f := newTokenizerFixture(t, "benchmark_tokenizer_auto_no_root")
	f.markPresent(t, filepath.Join("hf", "org_model-main"))

	request := f.autoRequest(f.node.UUID)
	if got, want := f.resolve(t, request), f.repositoryRef(t); got != want {
		t.Fatalf("ref = %q, want the repository %q", got, want)
	}
}

// A model downloaded as one subfolder of a larger repository lives in its own
// digest-named directory; the row that records that download is the only place
// the layout exists, so the resolver must read the path rather than rebuild it.
func TestBenchmarkTokenizerAutoUsesTheRecordedArtifactDirectory(t *testing.T) {
	f := newTokenizerFixture(t, "benchmark_tokenizer_auto_subfolder")
	model := *f.model
	model.ArtifactSubfolder = "FL2VA"
	model.ID = 0
	saved, err := f.admission.store.UpsertModel(&store.Model{Source: model.Source, SourceID: model.SourceID, Revision: model.Revision, ArtifactSubfolder: "FL2VA", Name: "subset"})
	if err != nil {
		t.Fatal(err)
	}
	f.model = saved
	root := t.TempDir()
	dir := filepath.Join(root, store.ModelArtifactPath(saved))
	want := f.placeTokenizer(t, dir)
	if !strings.Contains(dir, "subfolder") {
		t.Fatalf("expected a subfolder-named directory, got %q", dir)
	}

	// The target deployment is what carries the model identity into the
	// resolver, so this request measures the sparse artifact.
	request := f.autoRequest(f.node.UUID)
	request.Deployment = &store.Deployment{ID: f.deployment.ID, ModelID: saved.ID}
	if got := f.resolve(t, request); got != want {
		t.Fatalf("ref = %q, want %q", got, want)
	}
}

// A pending download is not a copy: the file may be half written.
func TestBenchmarkTokenizerAutoIgnoresAnIncompleteCopy(t *testing.T) {
	f := newTokenizerFixture(t, "benchmark_tokenizer_auto_incomplete")
	dir := t.TempDir()
	f.placeTokenizer(t, dir)
	if _, err := f.admission.store.UpsertModelFile(&store.ModelFile{ModelID: f.model.ID, NodeID: f.node.ID, Path: dir, State: proto.MFSyncing}); err != nil {
		t.Fatal(err)
	}

	request := f.autoRequest(f.node.UUID)
	if got, want := f.resolve(t, request), f.repositoryRef(t); got != want {
		t.Fatalf("ref = %q, want the repository %q", got, want)
	}
}

func TestBenchmarkTokenizerAutoFetchesFromTheTargetMachineForAServerOrigin(t *testing.T) {
	f := newTokenizerFixture(t, "benchmark_tokenizer_auto_fetch")
	f.admission.handlers.d.BenchmarkArtifactDir = t.TempDir()
	dir := t.TempDir()
	remote := f.placeTokenizer(t, dir)

	got := f.resolve(t, f.autoRequest("server"))
	if got == remote || !strings.HasPrefix(got, f.admission.handlers.d.BenchmarkArtifactDir+string(os.PathSeparator)) {
		t.Fatalf("ref = %q, want a copy under the artifact directory", got)
	}
	raw, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(f.file[remote]) {
		t.Fatalf("fetched %q, want %q", raw, f.file[remote])
	}
	// Content addressed: the same bytes resolve to the same file next time.
	if again := f.resolve(t, f.autoRequest("server")); again != got {
		t.Fatalf("second resolution = %q, want the cached %q", again, got)
	}
}

func TestBenchmarkTokenizerAutoFallsBackWhenTheFetchCannotRun(t *testing.T) {
	cases := []struct {
		name  string
		apply func(t *testing.T, f *tokenizerFixture, dir string)
	}{
		{
			name: "no data lane",
			apply: func(_ *testing.T, f *tokenizerFixture, _ string) {
				f.laneOffline = true
			},
		},
		{
			name: "the node holds no tokenizer",
			apply: func(t *testing.T, f *tokenizerFixture, dir string) {
				f.markPresent(t, dir)
			},
		},
		{
			name: "the file is not a tokenizer",
			apply: func(t *testing.T, f *tokenizerFixture, dir string) {
				f.file[f.placeTokenizer(t, dir)] = []byte("not json at all")
			},
		},
		{
			name: "the node returns nothing",
			apply: func(t *testing.T, f *tokenizerFixture, dir string) {
				f.file[f.placeTokenizer(t, dir)] = nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTokenizerFixture(t, "benchmark_tokenizer_auto_fallback_"+strings.ReplaceAll(tc.name, " ", "_"))
			f.admission.handlers.d.BenchmarkArtifactDir = t.TempDir()
			tc.apply(t, f, t.TempDir())

			if got, want := f.resolve(t, f.autoRequest("server")), f.repositoryRef(t); got != want {
				t.Fatalf("ref = %q, want the repository %q", got, want)
			}
			entries, err := os.ReadDir(f.admission.handlers.d.BenchmarkArtifactDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("a failed fetch must leave nothing behind: %v", entries)
			}
		})
	}
}

func TestBenchmarkTokenizerAutoResolvesTheLoadOriginNodesOwnCopy(t *testing.T) {
	f := newTokenizerFixture(t, "benchmark_tokenizer_auto_origin_node")
	// The model lives on the origin node; the target machine has nothing.
	origin, err := f.admission.store.RegisterNode("origin-node", store.HashToken("origin-token"), "origin-node")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "tokenizer.json")
	if _, err := f.admission.store.UpsertModelFile(&store.ModelFile{ModelID: f.model.ID, NodeID: origin.ID, Path: dir, State: proto.MFPresent}); err != nil {
		t.Fatal(err)
	}
	f.stat[path] = proto.FSStatResult{Path: path, SizeBytes: 32, Regular: true}
	f.file[path] = []byte(`{"model":{"vocab":{}}}`)

	if got := f.resolve(t, f.autoRequest(origin.UUID)); got != path {
		t.Fatalf("ref = %q, want the load origin's copy %q", got, path)
	}
}

func TestResolveBenchmarkTokenizerSourceRules(t *testing.T) {
	f := newTokenizerFixture(t, "benchmark_tokenizer_source_rules")

	cases := []struct {
		name    string
		request benchmarkTokenizerRequest
		want    string
		wantErr string
	}{
		{
			name:    "an explicit estimate keeps estimation reachable",
			request: benchmarkTokenizerRequest{Source: benchmarkTokenizerSourceEstimate, Reference: "hf://org/model", TargetKind: "instance", LoadOrigin: "server", Node: f.node},
			want:    "",
		},
		{
			name:    "an explicit repository keeps the client's reference",
			request: benchmarkTokenizerRequest{Source: benchmarkTokenizerSourceHuggingFace, Reference: "hf://org/other", TargetKind: "instance", LoadOrigin: "server", Node: f.node},
			want:    "hf://org/other",
		},
		{
			name:    "an upload keeps the file the client posted",
			request: benchmarkTokenizerRequest{Source: benchmarkTokenizerSourceUpload, Reference: "/var/lib/gpuworks/tokenizer-1.json", TargetKind: "external", LoadOrigin: "server"},
			want:    "/var/lib/gpuworks/tokenizer-1.json",
		},
		{
			name:    "auto on an external endpoint is its own tokenizer, with nothing to fall back to",
			request: benchmarkTokenizerRequest{Source: benchmarkTokenizerSourceAuto, TargetKind: "external", LoadOrigin: "server"},
			want:    "engine:",
		},
		{
			name:    "the engine named on its own has no fallback",
			request: benchmarkTokenizerRequest{Source: benchmarkTokenizerSourceEngine, TargetKind: "instance", LoadOrigin: "server", Deployment: &store.Deployment{}, Node: f.node},
			want:    "engine:",
		},
		{
			name:    "the engine of an external endpoint",
			request: benchmarkTokenizerRequest{Source: benchmarkTokenizerSourceEngine, TargetKind: "external", LoadOrigin: "server"},
			want:    "engine:",
		},
		{
			name:    "the engine of a deployment with tokenize turned off is refused",
			request: benchmarkTokenizerRequest{Source: benchmarkTokenizerSourceEngine, TargetKind: "instance", LoadOrigin: "server", Deployment: &store.Deployment{TokenizeDisabled: true}, Node: f.node},
			wantErr: ErrorBenchmarkTokenizerSourceInvalid,
		},
		{
			name:    "a repository written as people write it",
			request: benchmarkTokenizerRequest{Source: benchmarkTokenizerSourceHuggingFace, Reference: " moonshotai/Kimi-K3.1-Pro ", TargetKind: "external", LoadOrigin: "server"},
			want:    "hf://moonshotai/Kimi-K3.1-Pro",
		},
		{
			name:    "a ModelScope repository with a revision",
			request: benchmarkTokenizerRequest{Source: benchmarkTokenizerSourceModelScope, Reference: "Qwen/Qwen3-8B@master", TargetKind: "external", LoadOrigin: "server"},
			want:    "ms://Qwen/Qwen3-8B@master",
		},
		{
			name:    "a reference for the other repository source is refused",
			request: benchmarkTokenizerRequest{Source: benchmarkTokenizerSourceHuggingFace, Reference: "ms://Qwen/Qwen3-8B", TargetKind: "external", LoadOrigin: "server"},
			wantErr: ErrorBenchmarkParameterInvalid,
		},
		{
			name:    "a bare model name is not a repository",
			request: benchmarkTokenizerRequest{Source: benchmarkTokenizerSourceHuggingFace, Reference: "Kimi-K3.1-Pro", TargetKind: "external", LoadOrigin: "server"},
			wantErr: ErrorBenchmarkParameterInvalid,
		},
		{
			name:    "an unknown source is refused, not guessed",
			request: benchmarkTokenizerRequest{Source: "something-new", TargetKind: "instance", LoadOrigin: "server"},
			wantErr: ErrorBenchmarkTokenizerSourceInvalid,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.admission.handlers.resolveBenchmarkTokenizer(context.Background(), tc.request)
			if tc.wantErr != "" {
				var productErr *messages.ProductError
				if !errors.As(err, &productErr) || productErr.Code() != tc.wantErr {
					t.Fatalf("err = %v, want code %s", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("ref = %q err = %v, want %q", got, err, tc.want)
			}
		})
	}
}

// The legacy contract has to survive: a client that names no source and sends
// no reference still gets the repository for a managed target, and one that
// names a repository keeps it.
func TestCreateBenchmarkTokenizerSourceContract(t *testing.T) {
	f := newTokenizerFixture(t, "benchmark_tokenizer_create_contract")
	deployment, err := f.admission.store.CreateDeployment(&store.Deployment{Name: "contract", ServedName: "contract-served", State: "active", ModelID: f.model.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.admission.store.CreateInstance(&store.Instance{UUID: "contract-leader", DeploymentID: deployment.ID, NodeID: f.node.ID, State: proto.InstRunning, APIPort: 31000}); err != nil {
		t.Fatal(err)
	}
	body := func(source string) string {
		sourceField := ""
		if source != "" {
			sourceField = fmt.Sprintf(`"tokenizer_source":%q,`, source)
		}
		return fmt.Sprintf(`{"name":"contract","target_kind":"instance","deployment_id":%d,"load_origin":%q,%s"dataset":{"kind":"random","input_tokens":8,"output_tokens":4},"mode":"grid","search":{"concurrency":[1],"isl":[8],"osl":[4],"requests_per_point_multiplier":1}}`, deployment.ID, f.node.UUID, sourceField)
	}
	// Each subtest measures the same target, so the previous run has to be
	// finished before the next create can be admitted.
	settle := func(t *testing.T) {
		t.Helper()
		runs, err := f.admission.store.ListBenchmarkRuns()
		if err != nil {
			t.Fatal(err)
		}
		for i := range runs {
			if runs[i].State == "pending" || runs[i].State == "running" {
				if err := f.admission.store.CancelBenchmarkRun(runs[i].ID, "test cleanup"); err != nil {
					t.Fatal(err)
				}
			}
		}
	}

	t.Run("no source keeps the repository default", func(t *testing.T) {
		settle(t)
		rec := f.admission.create(t, body(""))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var created benchmarkDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		if created.TokenizerRef != f.repositoryRef(t) {
			t.Fatalf("tokenizer_ref = %q, want the repository", created.TokenizerRef)
		}
	})

	t.Run("an explicit estimate is never filled in", func(t *testing.T) {
		settle(t)
		rec := f.admission.create(t, body(benchmarkTokenizerSourceEstimate))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var created benchmarkDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		if created.TokenizerRef != "" {
			t.Fatalf("tokenizer_ref = %q, want none: estimation must stay reachable", created.TokenizerRef)
		}
	})

	t.Run("an invalid source is refused with its own code", func(t *testing.T) {
		settle(t)
		rec := f.admission.create(t, body("telepathy"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var payload struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Code != ErrorBenchmarkTokenizerSourceInvalid || payload.Params["source"] != "telepathy" {
			t.Fatalf("payload=%+v", payload)
		}
	})
}

// An agentic run without any resolvable tokenizer fails at creation, where the
// user can still fix it, rather than at execution where it is fatal. A
// deployment that serves tokenize always has one: the engine's own
// (TestCreateBenchmarkAgenticUsesTheEngineWhenNothingElseResolves).
func TestCreateBenchmarkAgenticRequiresAResolvableTokenizer(t *testing.T) {
	f := newTokenizerFixture(t, "benchmark_tokenizer_agentic_required")
	local, err := f.admission.store.UpsertModel(&store.Model{Source: "local", SourceID: "gguf-only", Revision: "main", Name: "local", Operations: `[{"type":"openai.chat"}]`})
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := f.admission.store.CreateDeployment(&store.Deployment{Name: "agentic", ServedName: "agentic-served", State: "active", ModelID: local.ID, TokenizeDisabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.admission.store.CreateInstance(&store.Instance{UUID: "agentic-leader", DeploymentID: deployment.ID, NodeID: f.node.ID, State: proto.InstRunning, APIPort: 31000}); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"name":"agentic","target_kind":"instance","deployment_id":%d,"load_origin":%q,"tokenizer_source":"auto","dataset":{"kind":"agentic"},"mode":"grid","search":{"concurrency":[1],"requests_per_point_multiplier":1}}`, deployment.ID, f.node.UUID)
	rec := f.admission.create(t, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != ErrorBenchmarkAgenticTokenizer {
		t.Fatalf("code = %q, want %q", payload.Code, ErrorBenchmarkAgenticTokenizer)
	}
}

func TestReadBoundedNodeFileRejectsOversizeAndEmptyStreams(t *testing.T) {
	cases := []struct {
		name    string
		server  func(net.Conn)
		wantErr bool
		want    int
	}{
		{name: "content", server: func(c net.Conn) { _, _ = c.Write([]byte("tokenizer")); _ = c.Close() }, want: len("tokenizer")},
		{name: "empty", server: func(c net.Conn) { _ = c.Close() }, wantErr: true},
		{name: "oversize", server: func(c net.Conn) {
			_, _ = c.Write(make([]byte, proto.LargeFileReadMaxBytes+1))
			_ = c.Close()
		}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, client := net.Pipe()
			go tc.server(server)
			raw, err := readBoundedNodeFile(context.Background(), client)
			_ = client.Close()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("raw=%d bytes, want an error", len(raw))
				}
				return
			}
			if err != nil || len(raw) != tc.want {
				t.Fatalf("raw=%q err=%v, want %d bytes", raw, err, tc.want)
			}
		})
	}
}

func TestValidTokenizerJSONMatchesTheUploadProbe(t *testing.T) {
	for _, raw := range []string{`{"model":{}}`, `{"vocab":[]}`, `{"added_tokens":[]}`} {
		if !validTokenizerJSON([]byte(raw)) {
			t.Fatalf("%s should be accepted", raw)
		}
	}
	for _, raw := range []string{"", "not json", `{"other":1}`, `[]`} {
		if validTokenizerJSON([]byte(raw)) {
			t.Fatalf("%q should be refused", raw)
		}
	}
}

// A deployment that does not serve tokenize resolves "auto" to the copy or
// repository alone: naming the engine would only fail at run time.
func TestBenchmarkTokenizerAutoSkipsTheEngineOfADeploymentWithTokenizeOff(t *testing.T) {
	f := newTokenizerFixture(t, "benchmark_tokenizer_auto_tokenize_off")
	request := f.autoRequest(f.node.UUID)
	off := *request.Deployment
	off.TokenizeDisabled = true
	request.Deployment = &off
	got, err := f.admission.handlers.resolveBenchmarkTokenizer(context.Background(), request)
	if err != nil || got != f.repositoryRef(t) {
		t.Fatalf("ref = %q err = %v, want the repository %q", got, err, f.repositoryRef(t))
	}
}

// With nothing on disk or in a repository, "auto" on a deployment that serves
// tokenize is the engine alone, and the agentic run is admitted with it.
func TestCreateBenchmarkAgenticUsesTheEngineWhenNothingElseResolves(t *testing.T) {
	f := newTokenizerFixture(t, "benchmark_tokenizer_agentic_engine")
	local, err := f.admission.store.UpsertModel(&store.Model{Source: "local", SourceID: "gguf-only", Revision: "main", Name: "local", Operations: `[{"type":"openai.chat"}]`})
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := f.admission.store.CreateDeployment(&store.Deployment{Name: "agentic", ServedName: "agentic-served", State: "active", ModelID: local.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.admission.store.CreateInstance(&store.Instance{UUID: "agentic-leader", DeploymentID: deployment.ID, NodeID: f.node.ID, State: proto.InstRunning, APIPort: 31000}); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"name":"agentic","target_kind":"instance","deployment_id":%d,"load_origin":%q,"tokenizer_source":"auto","dataset":{"kind":"agentic"},"mode":"grid","search":{"concurrency":[1],"requests_per_point_multiplier":1}}`, deployment.ID, f.node.UUID)
	rec := f.admission.create(t, body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var run struct {
		TokenizerRef string `json:"tokenizer_ref"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil || run.TokenizerRef != "engine:" {
		t.Fatalf("tokenizer_ref = %q err = %v, want the engine alone", run.TokenizerRef, err)
	}
}
