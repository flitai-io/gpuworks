package app

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

func TestRPCRegisterPathValidatesModelDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"model_type":"qwen3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "shard"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "model.safetensors"), []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	n := &NodeRuntime{}
	got, err := n.OnRPC(context.Background(), proto.RPCRegisterPath, mustRaw(t, map[string]string{"path": dir}))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Path      string `json:"path"`
		SizeBytes int64  `json:"size_bytes"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Path != dir || out.SizeBytes <= 0 {
		t.Fatalf("register path result = %+v", out)
	}
}

func TestRPCRegisterPathValidatesSymlinkedModelDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"model_type":"qwen3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "model.safetensors"), []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Operators point model paths at stable symlinks over versioned download
	// directories (e.g. qwen3.5-0.8b → modelscope/Qwen_...-<hash>); the walk
	// must resolve the root link, not treat the directory as empty.
	link := filepath.Join(t.TempDir(), "model-link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	n := &NodeRuntime{}
	result, err := n.OnRPC(context.Background(), proto.RPCRegisterPath, mustRaw(t, map[string]string{"path": link}))
	if err != nil {
		t.Fatal(err)
	}
	if got := result.(registerPathResult); got.Path != link || got.SizeBytes <= 0 {
		t.Fatalf("symlinked model registration=%+v", got)
	}
}

func TestHTTPClientForUsesExplicitProxyOnly(t *testing.T) {
	if got := httpClientFor(proto.NetworkConfig{}); got != nil {
		t.Fatal("empty network should use the downloader default client")
	}
	client := httpClientFor(proto.NetworkConfig{HTTPProxy: "http://proxy.example:3128"})
	if client == nil {
		t.Fatal("explicit proxy should create a client")
	}
	transport, ok := client.Transport.(routeNamingTransport)
	if !ok || transport.Proxy == nil {
		t.Fatal("explicit proxy client has no proxy function")
	}
	req, err := http.NewRequest(http.MethodGet, "http://example.com/model", nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := transport.Proxy(req)
	if err != nil || proxy == nil || proxy.Host != "proxy.example:3128" {
		t.Fatalf("proxy = %v, err = %v", proxy, err)
	}
}

func TestModelDiskFullRecognizesPreflightAndWriteENOSPC(t *testing.T) {
	preflight := checkModelDownloadSpace(t.TempDir(), 1<<62)
	if preflight == nil {
		t.Fatal("an impossible admission passed")
	}
	enospc := fmt.Errorf("download model.safetensors: %w", &os.PathError{Op: "write", Path: "/models/model.safetensors.part", Err: syscall.ENOSPC})
	quota := &os.PathError{Op: "write", Path: "/models/a", Err: syscall.EDQUOT}
	for _, err := range []error{preflight, fmt.Errorf("copy: %w", preflight), enospc, quota} {
		if !modelDiskFull(err) {
			t.Fatalf("disk-space failure was not classified: %v", err)
		}
	}
	// Only the type counts: text that merely says so is not a disk failure.
	for _, err := range []error{
		errors.New("download: unexpected probe status 403"),
		errors.New("write /models/a: no space left on device"),
	} {
		if modelDiskFull(err) {
			t.Fatalf("classified by text: %v", err)
		}
	}
}

func TestRPCRegisterPathRejectsInvalidDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	n := &NodeRuntime{}
	if _, err := n.OnRPC(context.Background(), proto.RPCRegisterPath, mustRaw(t, map[string]string{"path": dir})); err == nil {
		t.Fatal("expected invalid model directory error")
	}
}

func TestRPCRegisterPathAcceptsAGGUFOnlyDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "model.gguf"), registrationGGUF(), 0o644); err != nil {
		t.Fatal(err)
	}
	n := &NodeRuntime{}
	if _, err := n.OnRPC(context.Background(), proto.RPCRegisterPath, mustRaw(t, map[string]string{"path": dir})); err != nil {
		t.Fatalf("GGUF-only directory rejected: %v", err)
	}
}

func TestValidateModelDirAcceptsNativeMistralLayout(t *testing.T) {
	dir := t.TempDir()
	for name := range map[string]string{"params.json": "{}", "tekken.json": "{}", "consolidated.safetensors": "weights"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if size, err := validateModelDir(dir); err != nil || size == 0 {
		t.Fatalf("native layout size=%d err=%v", size, err)
	}
}

// A repository that ships only GGUF shards has no config.json. The engine a
// template names decides what it needs, so the download must finish as a
// present model rather than fail its layout check after every byte arrived.
func TestDownloadRepoCompletesAGGUFOnlySubfolder(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "gguf")
	shards := map[string][]byte{
		"IQ2_XS/model-00001-of-00002.gguf": []byte("GGUF-first-shard"),
		"IQ2_XS/model-00002-of-00002.gguf": []byte("GGUF-second"),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/org/gguf/tree/main" {
			_, _ = fmt.Fprintf(w, `[{"type":"file","path":"README.md","size":9},{"type":"file","path":"Q4_K_M/model.gguf","size":99},{"type":"file","path":"IQ2_XS/model-00001-of-00002.gguf","size":%d},{"type":"file","path":"IQ2_XS/model-00002-of-00002.gguf","size":%d}]`,
				len(shards["IQ2_XS/model-00001-of-00002.gguf"]), len(shards["IQ2_XS/model-00002-of-00002.gguf"]))
			return
		}
		if body, ok := shards[strings.TrimPrefix(r.URL.Path, "/org/gguf/resolve/main/")]; ok {
			_, _ = w.Write(body)
			return
		}
		http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
	}))
	defer server.Close()

	n := &NodeRuntime{log: slog.New(slog.NewTextHandler(io.Discard, nil)), modelsDir: root, downloading: map[string]*modelDownload{}}
	var got proto.ModelFileStatus
	n.Bind(func(kind string, value any) error {
		if kind == proto.TStatusModelFile {
			got = value.(proto.ModelFileStatus)
		}
		return nil
	})
	task := proto.ModelFileTask{
		Key: "gguf", Source: modelrepo.SourceHF, SourceID: "org/gguf", Revision: "main", Subfolder: "IQ2_XS",
		Endpoint: server.URL, TargetPath: "gguf", DesiredState: "present", Acquisition: proto.ModelAcquireDownload,
	}
	n.downloadRepo(context.Background(), task, target, target)
	want := int64(len(shards["IQ2_XS/model-00001-of-00002.gguf"]) + len(shards["IQ2_XS/model-00002-of-00002.gguf"]))
	if got.State != proto.MFPresent || got.BytesTotal != want {
		t.Fatalf("status=%+v", got)
	}
	if _, ok := validCompletionMarker(target, task); !ok {
		t.Fatal("finished GGUF download left no valid completion marker")
	}
	if _, err := os.Stat(filepath.Join(target, "Q4_K_M")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an unselected quantization was downloaded: %v", err)
	}
}

func TestReconcilePresentPeerModelPreservesTransferIdentity(t *testing.T) {
	root := t.TempDir()
	model := filepath.Join(root, "model")
	if err := os.MkdirAll(model, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(model, "config.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(model, "model.safetensors"), []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	n := &NodeRuntime{log: slog.New(slog.NewTextHandler(io.Discard, nil)), modelsDir: root, downloading: map[string]*modelDownload{}}
	var got proto.ModelFileStatus
	n.Bind(func(kind string, value any) error {
		if kind == proto.TStatusModelFile {
			got = value.(proto.ModelFileStatus)
		}
		return nil
	})
	n.reconcileModelFile(context.Background(), proto.ModelFileTask{
		Key: "1-2", TargetPath: "model", DesiredState: "present",
		Acquisition: proto.ModelAcquirePeer, TransferID: "persisted-transfer",
	})
	if got.State != proto.MFPresent || got.TransferID != "persisted-transfer" || got.Acquisition != proto.ModelAcquirePeer {
		t.Fatalf("present model status = %+v", got)
	}
	if got.BytesTotal == 0 || got.BytesDone != got.BytesTotal {
		t.Fatalf("present peer model did not report its real size: %+v", got)
	}
}

func TestReconcileMissingRegisteredModelReportsAbsent(t *testing.T) {
	root := t.TempDir()
	n := &NodeRuntime{log: slog.New(slog.NewTextHandler(io.Discard, nil)), modelsDir: root, downloading: map[string]*modelDownload{}}
	var got proto.ModelFileStatus
	n.Bind(func(kind string, value any) error {
		if kind == proto.TStatusModelFile {
			got = value.(proto.ModelFileStatus)
		}
		return nil
	})
	n.reconcileModelFile(context.Background(), proto.ModelFileTask{
		Key: "7-9", TargetPath: "missing", DesiredState: "present", Acquisition: proto.ModelAcquireRegistered,
	})
	if got.State != proto.MFAbsent || got.Acquisition != proto.ModelAcquireRegistered || !strings.Contains(got.Message, "absent") {
		t.Fatalf("missing registered model status=%+v", got)
	}
}

func TestDownloadRepoBackfillsVerifiedLegacyCache(t *testing.T) {
	root := t.TempDir()
	model := filepath.Join(root, "model")
	if err := os.MkdirAll(model, 0o755); err != nil {
		t.Fatal(err)
	}
	config, weights := []byte(`{}`), []byte("weights")
	if err := os.WriteFile(filepath.Join(model, "config.json"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(model, "model.safetensors"), weights, 0o644); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models/org/model/tree/main" {
			t.Fatalf("legacy cache unexpectedly downloaded a file: %s", r.URL.Path)
		}
		_, _ = fmt.Fprintf(w, `[{"type":"file","path":"config.json","size":%d},{"type":"file","path":"model.safetensors","size":%d}]`, len(config), len(weights))
	}))
	defer server.Close()

	n := &NodeRuntime{log: slog.New(slog.NewTextHandler(io.Discard, nil)), modelsDir: root, downloading: map[string]*modelDownload{}}
	var got proto.ModelFileStatus
	n.Bind(func(kind string, value any) error {
		if kind == proto.TStatusModelFile {
			got = value.(proto.ModelFileStatus)
		}
		return nil
	})
	task := proto.ModelFileTask{
		Key: "7-9", Source: "huggingface", SourceID: "org/model", Revision: "main",
		Endpoint: server.URL, TargetPath: "model", DesiredState: "present", Acquisition: proto.ModelAcquireDownload,
	}
	n.downloadRepo(context.Background(), task, model, model)
	if got.State != proto.MFPresent || got.BytesTotal != int64(len(config)+len(weights)) || got.BytesDone != got.BytesTotal {
		t.Fatalf("backfilled cache status = %+v", got)
	}
	if size, ok := validCompletionMarker(model, task); !ok || size != got.BytesTotal {
		t.Fatalf("completion marker = size %d ok %v", size, ok)
	}
}

func TestDownloadRepoSelectsSubfolderAndPreservesRepositoryLayout(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "fl2va")
	config := []byte(`{"model_type":"fl2va"}`)
	weights := []byte("selected-weights")
	var downloaded []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/org/video/tree/main":
			_, _ = fmt.Fprintf(w, `[{"type":"file","path":"README.md","size":99},{"type":"file","path":"Ref2VA/config.json","size":99},{"type":"file","path":"FL2VA/config.json","size":%d},{"type":"file","path":"FL2VA/model.safetensors","size":%d}]`, len(config), len(weights))
		case "/org/video/resolve/main/FL2VA/config.json":
			downloaded = append(downloaded, r.URL.Path)
			_, _ = w.Write(config)
		case "/org/video/resolve/main/FL2VA/model.safetensors":
			downloaded = append(downloaded, r.URL.Path)
			_, _ = w.Write(weights)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	n := &NodeRuntime{log: slog.New(slog.NewTextHandler(io.Discard, nil)), modelsDir: root, downloading: map[string]*modelDownload{}}
	var got proto.ModelFileStatus
	n.Bind(func(kind string, value any) error {
		if kind == proto.TStatusModelFile {
			got = value.(proto.ModelFileStatus)
		}
		return nil
	})
	task := proto.ModelFileTask{
		Key: "subset", Source: modelrepo.SourceHF, SourceID: "org/video", Revision: "main", Subfolder: "FL2VA",
		Endpoint: server.URL, TargetPath: "fl2va", DesiredState: "present", Acquisition: proto.ModelAcquireDownload,
	}
	n.downloadRepo(context.Background(), task, target, target)
	if got.State != proto.MFPresent || got.BytesTotal != int64(len(config)+len(weights)) {
		t.Fatalf("status=%+v", got)
	}
	if len(downloaded) != 4 { // range probe + fallback for each selected file
		t.Fatalf("downloaded requests=%v", downloaded)
	}
	if data, err := os.ReadFile(filepath.Join(target, "FL2VA", "config.json")); err != nil || !bytes.Equal(data, config) {
		t.Fatalf("selected config=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(target, "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("subfolder was incorrectly flattened as repository root: %v", err)
	}
	marker, ok := readCompletionMarker(target)
	if !ok || marker.Subfolder != "FL2VA" || len(marker.Files) != 2 {
		t.Fatalf("completion marker=%+v ok=%v", marker, ok)
	}
	wholeRepoTask := task
	wholeRepoTask.Subfolder = ""
	if _, ok := validCompletionMarker(target, wholeRepoTask); ok {
		t.Fatal("subfolder cache was reused as a whole-repository cache")
	}
}

func TestReconcileDownloadRepairsPartialLegacyCache(t *testing.T) {
	root := t.TempDir()
	model := filepath.Join(root, "model")
	if err := os.MkdirAll(model, 0o755); err != nil {
		t.Fatal(err)
	}
	config := []byte(`{}`)
	firstShard := []byte("first")
	secondShard := []byte("second")
	if err := os.WriteFile(filepath.Join(model, "config.json"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(model, "model-00001-of-00002.safetensors"), firstShard, 0o644); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/org/partial/tree/main":
			_, _ = fmt.Fprintf(w, `[{"type":"file","path":"config.json","size":%d},{"type":"file","path":"model-00001-of-00002.safetensors","size":%d},{"type":"file","path":"model-00002-of-00002.safetensors","size":%d}]`, len(config), len(firstShard), len(secondShard))
		case "/org/partial/resolve/main/model-00002-of-00002.safetensors":
			_, _ = w.Write(secondShard)
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	n := &NodeRuntime{log: slog.New(slog.NewTextHandler(io.Discard, nil)), modelsDir: root, downloading: map[string]*modelDownload{}}
	status := make(chan proto.ModelFileStatus, 16)
	n.Bind(func(kind string, value any) error {
		if kind == proto.TStatusModelFile {
			status <- value.(proto.ModelFileStatus)
		}
		return nil
	})
	task := proto.ModelFileTask{
		Key: "8-10", Source: "huggingface", SourceID: "org/partial", Revision: "main",
		Endpoint: server.URL, TargetPath: "model", DesiredState: "present", Acquisition: proto.ModelAcquireDownload,
	}
	n.reconcileModelFile(context.Background(), task)
	// The repair is a five-byte download from a local httptest server, so on an
	// idle host this completes in milliseconds. The budget only matters under
	// parallel-suite CPU starvation, where a tight wall-clock deadline turns a
	// scheduling delay into a false timeout (job 172376); 30s keeps the wait
	// honest without ever masking a real hang worth failing on.
	deadline := time.After(30 * time.Second)
	for {
		select {
		case got := <-status:
			if got.State == proto.MFFailed {
				t.Fatalf("partial cache repair failed: %+v", got)
			}
			if got.State == proto.MFPresent {
				wantTotal := int64(len(config) + len(firstShard) + len(secondShard))
				if got.BytesTotal != wantTotal || got.BytesDone != wantTotal {
					t.Fatalf("repaired cache status = %+v, want %d bytes", got, wantTotal)
				}
				if data, err := os.ReadFile(filepath.Join(model, "model-00002-of-00002.safetensors")); err != nil || !bytes.Equal(data, secondShard) {
					t.Fatalf("second shard = %q, %v", data, err)
				}
				if _, ok := validCompletionMarker(model, task); !ok {
					t.Fatal("repaired cache has no valid completion marker")
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for partial cache repair")
		}
	}
}

func TestCompletionMarkerRejectsMissingOrTruncatedShard(t *testing.T) {
	dir := t.TempDir()
	files := []modelrepo.File{
		{Path: "config.json", Size: 2},
		{Path: "model-00001-of-00002.safetensors", Size: 3},
		{Path: "model-00002-of-00002.safetensors", Size: 4},
	}
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(dir, file.Path), bytes.Repeat([]byte("x"), int(file.Size)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	task := proto.ModelFileTask{Source: "modelscope", SourceID: "org/sharded", Revision: "main"}
	if total, err := writeCompletionMarker(dir, task, files); err != nil || total != 9 {
		t.Fatalf("write completion marker total=%d err=%v", total, err)
	}
	if _, ok := validCompletionMarker(dir, task); !ok {
		t.Fatal("complete sharded model was rejected")
	}
	if err := os.Truncate(filepath.Join(dir, files[2].Path), 1); err != nil {
		t.Fatal(err)
	}
	if _, ok := validCompletionMarker(dir, task); ok {
		t.Fatal("truncated shard was accepted")
	}
}

func TestValidateRepoManifestRejectsUnsafeAndDuplicatePaths(t *testing.T) {
	for _, files := range [][]modelrepo.File{
		{},
		{{Path: "../escape", Size: 1}},
		{{Path: "/absolute", Size: 1}},
		{{Path: "config.json", Size: 1}, {Path: "./config.json", Size: 1}},
		{{Path: modelCompleteMarker, Size: 1}},
		{{Path: "config.json", Size: 1, Revision: "abc"}, {Path: "model.safetensors", Size: 1, Revision: "def"}},
	} {
		if _, err := validateRepoManifest(files); err == nil {
			t.Fatalf("unsafe manifest was accepted: %+v", files)
		}
	}
}

func TestDownloadRepoUsesNodeProxyAndModelScopePinnedRevision(t *testing.T) {
	const (
		commit = "25d1d1fa49de445ab1f3a41f3537d7dad222826e"
		host   = "modelscope.invalid"
	)
	config := []byte(`{"model_type":"test"}`)
	weights := []byte("weights")
	var listRequests, downloadRequests int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != host {
			t.Errorf("proxied host = %q, want %q", r.URL.Host, host)
		}
		switch r.URL.Path {
		case "/api/v1/models/org/model/repo/files":
			listRequests++
			if _, ok := r.URL.Query()["Revision"]; ok {
				t.Errorf("empty revision was sent: %s", r.URL.RawQuery)
			}
			if r.URL.Query().Get("Recursive") != "true" {
				t.Errorf("Recursive query = %q", r.URL.Query().Get("Recursive"))
			}
			_, _ = fmt.Fprintf(w, `{"Code":200,"Data":{"Files":[{"Path":"config.json","Size":%d,"Type":"blob","Revision":"%s"},{"Path":"model.safetensors","Size":%d,"Type":"blob","Revision":"%s"}]}}`, len(config), commit, len(weights), commit)
		case "/models/org/model/resolve/" + commit + "/config.json":
			downloadRequests++
			_, _ = w.Write(config)
		case "/models/org/model/resolve/" + commit + "/model.safetensors":
			downloadRequests++
			_, _ = w.Write(weights)
		default:
			http.Error(w, "unexpected proxy path "+r.URL.String(), http.StatusNotFound)
		}
	}))
	defer proxy.Close()

	root := t.TempDir()
	target := filepath.Join(root, "model")
	n := &NodeRuntime{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), modelsDir: root,
		downloading: map[string]*modelDownload{}, netcfg: proto.NetworkConfig{HTTPProxy: proxy.URL},
	}
	var got proto.ModelFileStatus
	var downloading []string
	n.Bind(func(kind string, value any) error {
		if kind == proto.TStatusModelFile {
			got = value.(proto.ModelFileStatus)
			if got.State == proto.MFDownloading && got.BytesDone > 0 {
				downloading = append(downloading, got.Message)
			}
		}
		return nil
	})
	task := proto.ModelFileTask{
		Key: "11-12", Source: modelrepo.SourceMS, SourceID: "org/model", Revision: "",
		Endpoint: "http://" + host, TargetPath: "model", DesiredState: "present", Acquisition: proto.ModelAcquireDownload,
	}
	n.downloadRepo(context.Background(), task, target, target)
	if got.State != proto.MFPresent || got.Revision != commit {
		t.Fatalf("final status = %+v", got)
	}
	// While bytes arrive the status says where they come from.
	if len(downloading) == 0 || downloading[len(downloading)-1] != "downloading from "+host {
		t.Fatalf("downloading messages = %q, want the source host", downloading)
	}
	if listRequests != 1 || downloadRequests < 4 {
		// Each small file is requested once by the range probe and once by the
		// single-stream fallback after the proxy returns HTTP 200.
		t.Fatalf("list requests=%d download requests=%d", listRequests, downloadRequests)
	}
	marker, ok := readCompletionMarker(target)
	if !ok || marker.Revision != "" || manifestRevision(marker.Files) != commit {
		t.Fatalf("completion marker ok=%v marker=%+v", ok, marker)
	}
}

// The network configuration belongs to the node, not to the engine: a node
// that has not installed a container runtime yet still downloads through the
// configured proxy. This is the regression the engine-owned copy introduced.
func TestDownloadRepoUsesProxyWithoutARuntime(t *testing.T) {
	const (
		commit = "25d1d1fa49de445ab1f3a41f3537d7dad222826e"
		host   = "modelscope.invalid"
	)
	config := []byte(`{"model_type":"test"}`)
	weights := []byte("weights")
	var listRequests, downloadRequests int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != host {
			t.Errorf("proxied host = %q, want %q", r.URL.Host, host)
		}
		switch r.URL.Path {
		case "/api/v1/models/org/model/repo/files":
			listRequests++
			_, _ = fmt.Fprintf(w, `{"Code":200,"Data":{"Files":[{"Path":"config.json","Size":%d,"Type":"blob","Revision":"%s"},{"Path":"model.safetensors","Size":%d,"Type":"blob","Revision":"%s"}]}}`, len(config), commit, len(weights), commit)
		case "/models/org/model/resolve/" + commit + "/config.json":
			downloadRequests++
			_, _ = w.Write(config)
		case "/models/org/model/resolve/" + commit + "/model.safetensors":
			downloadRequests++
			_, _ = w.Write(weights)
		default:
			http.Error(w, "unexpected proxy path "+r.URL.String(), http.StatusNotFound)
		}
	}))
	defer proxy.Close()

	root := t.TempDir()
	n := newNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, root, "none", metricsOnlyProber{}, nil)
	if n.engine != nil {
		t.Fatal("this node was expected to have no runtime")
	}
	n.setNetwork(proto.NetworkConfig{HTTPProxy: proxy.URL})
	var got proto.ModelFileStatus
	n.Bind(func(kind string, value any) error {
		if kind == proto.TStatusModelFile {
			got = value.(proto.ModelFileStatus)
		}
		return nil
	})
	task := proto.ModelFileTask{
		Key: "11-13", Source: modelrepo.SourceMS, SourceID: "org/model", Revision: "",
		Endpoint: "http://" + host, TargetPath: "model", DesiredState: "present", Acquisition: proto.ModelAcquireDownload,
	}
	n.downloadRepo(context.Background(), task, filepath.Join(root, "model"), filepath.Join(root, "model"))
	if got.State != proto.MFPresent || got.Revision != commit {
		t.Fatalf("final status = %+v", got)
	}
	if listRequests != 1 || downloadRequests < 2 {
		t.Fatalf("the runtime-less node did not download through the proxy: list=%d download=%d", listRequests, downloadRequests)
	}
}

func TestStreamAndReceiveModelArchive(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "config.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "weights"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "weights", "model.safetensors"), []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	n := &NodeRuntime{}
	if err := n.StreamModelArchive(context.Background(), src, &buf); err != nil {
		t.Fatal(err)
	}
	dstRoot := t.TempDir()
	dst := filepath.Join(dstRoot, "model")
	dstNode := &NodeRuntime{}
	if err := dstNode.ReceiveModelArchive(context.Background(), dst, &buf); err != nil {
		t.Fatal(err)
	}
	if !isModelPresent(dst) {
		t.Fatalf("received model archive is not present at %s", dst)
	}
	// The received directory is readable by a non-root engine, like one
	// downloaded on the node.
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o755 {
		t.Fatalf("received model directory mode = %o, want 755", mode)
	}
}

func TestReceiveModelArchiveIsAtomicOnFailure(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "model")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "config.json"), []byte(`{"old":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "old.safetensors"), []byte("old-weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	n := &NodeRuntime{}
	if err := n.ReceiveModelArchive(context.Background(), dst, strings.NewReader("not a tar archive")); err == nil {
		t.Fatal("invalid archive unexpectedly succeeded")
	}
	got, err := os.ReadFile(filepath.Join(dst, "old.safetensors"))
	if err != nil || string(got) != "old-weights" {
		t.Fatalf("existing model was changed: %q, %v", got, err)
	}
	stages, err := filepath.Glob(filepath.Join(root, ".model.gpuworks-sync-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 0 {
		t.Fatalf("failed receive left staging directories: %v", stages)
	}
}

func TestReceiveModelArchiveReplacesInvalidTargetAtomically(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "model")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "incomplete.txt"), []byte("old-partial-download"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(root, "source")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "config.json"), []byte(`{"model_type":"test"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "model.safetensors"), []byte("new-weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := (&NodeRuntime{}).StreamModelArchive(context.Background(), src, &archive); err != nil {
		t.Fatal(err)
	}
	if err := (&NodeRuntime{}).ReceiveModelArchive(context.Background(), dst, &archive); err != nil {
		t.Fatal(err)
	}
	if !isModelPresent(dst) {
		t.Fatal("replacement model is incomplete")
	}
	if _, err := os.Stat(filepath.Join(dst, "incomplete.txt")); !os.IsNotExist(err) {
		t.Fatalf("invalid target content survived replacement: %v", err)
	}
	stages, err := filepath.Glob(filepath.Join(root, ".model.gpuworks-*-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 0 {
		t.Fatalf("successful replacement left staging or backup directories: %v", stages)
	}
}

func TestBoundedModelSyncMessagePreservesUTF8(t *testing.T) {
	message := boundedModelSyncMessage(errors.New(strings.Repeat("模型同步失败", 100)))
	if len(message) > 480 {
		t.Fatalf("bounded message has %d bytes", len(message))
	}
	if !utf8.ValidString(message) {
		t.Fatalf("bounded message is not valid UTF-8: %q", message)
	}
}

func TestStreamModelArchiveDereferencesWeightSymlink(t *testing.T) {
	root := t.TempDir()
	model := filepath.Join(root, "model")
	if err := os.MkdirAll(model, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(model, "config.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(root, "blob")
	if err := os.WriteFile(blob, []byte("linked-weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(blob, filepath.Join(model, "model.safetensors")); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := (&NodeRuntime{}).StreamModelArchive(context.Background(), model, &archive); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "received")
	if err := (&NodeRuntime{}).ReceiveModelArchive(context.Background(), dst, &archive); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(dst, "model.safetensors"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("received linked weight mode = %v, want regular", info.Mode())
	}
}

func TestModelSyncRPCTransfersDirectlyBetweenNodeRuntimes(t *testing.T) {
	sourceRoot := t.TempDir()
	sourceModel := filepath.Join(sourceRoot, "source")
	if err := os.MkdirAll(sourceModel, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceModel, "config.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	weights := bytes.Repeat([]byte("weights"), 10000)
	if err := os.WriteFile(filepath.Join(sourceModel, "model.safetensors"), weights, 0o644); err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	newRuntime := func(root string) *NodeRuntime {
		return &NodeRuntime{
			log: slog.New(slog.NewTextHandler(io.Discard, nil)), modelsDir: root,
			downloading: map[string]*modelDownload{}, modelSyncs: map[string]*activeModelSync{},
		}
	}
	source, target := newRuntime(sourceRoot), newRuntime(targetRoot)
	listenAny, err := target.OnRPC(context.Background(), proto.RPCModelSyncListen, mustRaw(t, proto.ModelSyncListenParams{
		TransferID: "transfer-1", Key: "1-2", TargetPath: "target", Token: "one-time-token",
		BindIP: "127.0.0.1", ExpectedFileBytes: int64(len(weights)) + 2,
	}))
	if err != nil {
		t.Fatal(err)
	}
	listenJSON, _ := json.Marshal(listenAny)
	var listen proto.ModelSyncListenResult
	if err := json.Unmarshal(listenJSON, &listen); err != nil {
		t.Fatal(err)
	}
	if len(listen.Endpoints) == 0 || listen.Fingerprint == "" || listen.InProgress {
		t.Fatalf("listen result = %+v", listen)
	}
	sendAny, err := source.OnRPC(context.Background(), proto.RPCModelSyncSend, mustRaw(t, proto.ModelSyncSendParams{
		TransferID: "transfer-1", SourcePath: "source", Token: "one-time-token",
		Endpoints: listen.Endpoints, Fingerprint: listen.Fingerprint,
	}))
	if err != nil {
		t.Fatal(err)
	}
	sendJSON, _ := json.Marshal(sendAny)
	var sent proto.ModelSyncTransferResult
	if err := json.Unmarshal(sendJSON, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Bytes <= int64(len(weights)) || len(sent.SHA256) != 64 {
		t.Fatalf("send result = %+v", sent)
	}
	dst := filepath.Join(targetRoot, "target")
	if !isModelPresent(dst) {
		t.Fatalf("direct target model is incomplete: %s", dst)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		target.syncMu.Lock()
		active := len(target.modelSyncs)
		target.syncMu.Unlock()
		if active == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("target receiver did not leave active map")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestModelSyncSourceArchiveFailurePreservesTargetAndCleansStage(t *testing.T) {
	sourceRoot := t.TempDir()
	sourceModel := filepath.Join(sourceRoot, "source")
	if err := os.MkdirAll(filepath.Join(sourceModel, "linked-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceModel, "config.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceModel, "model.safetensors"), []byte("source-weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceModel, "linked-dir", "file"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(sourceModel, "linked-dir"), filepath.Join(sourceModel, "directory-link")); err != nil {
		t.Fatal(err)
	}

	targetRoot := t.TempDir()
	targetModel := filepath.Join(targetRoot, "target")
	if err := os.MkdirAll(targetModel, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetModel, "config.json"), []byte(`{"old":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetModel, "old.safetensors"), []byte("old-weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	newRuntime := func(root string) *NodeRuntime {
		return &NodeRuntime{
			log: slog.New(slog.NewTextHandler(io.Discard, nil)), modelsDir: root,
			downloading: map[string]*modelDownload{}, modelSyncs: map[string]*activeModelSync{},
		}
	}
	source, target := newRuntime(sourceRoot), newRuntime(targetRoot)
	listenAny, err := target.OnRPC(context.Background(), proto.RPCModelSyncListen, mustRaw(t, proto.ModelSyncListenParams{
		TransferID: "source-error", Key: "1-2", TargetPath: "target", Token: "token", BindIP: "127.0.0.1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(listenAny)
	var listen proto.ModelSyncListenResult
	if err := json.Unmarshal(b, &listen); err != nil {
		t.Fatal(err)
	}
	_, err = source.OnRPC(context.Background(), proto.RPCModelSyncSend, mustRaw(t, proto.ModelSyncSendParams{
		TransferID: "source-error", SourcePath: "source", Token: "token",
		Endpoints: listen.Endpoints, Fingerprint: listen.Fingerprint,
	}))
	if err == nil || !strings.Contains(err.Error(), "symlinked directories") {
		t.Fatalf("source archive error = %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		target.syncMu.Lock()
		active := len(target.modelSyncs)
		target.syncMu.Unlock()
		if active == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failed target receiver did not stop")
		}
		time.Sleep(time.Millisecond)
	}
	old, err := os.ReadFile(filepath.Join(targetModel, "old.safetensors"))
	if err != nil || string(old) != "old-weights" {
		t.Fatalf("existing target changed after source failure: %q, %v", old, err)
	}
	stages, err := filepath.Glob(filepath.Join(targetRoot, ".target.gpuworks-sync-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 0 {
		t.Fatalf("source archive failure left target staging directories: %v", stages)
	}
}

func TestExtractModelArchiveRejectsUnsupportedEntry(t *testing.T) {
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	if err := extractModelArchive(context.Background(), t.TempDir(), &archive); err == nil {
		t.Fatal("symlink archive entry unexpectedly succeeded")
	}
}

func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A missing path is a coded refusal the server reads without parsing; the
// text stays the sentence os.Stat writes, which older servers matched.
func TestRPCFSStatRefusesAMissingPathWithItsCode(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	_, err := (&NodeRuntime{}).OnRPC(context.Background(), proto.RPCFSStat, mustRaw(t, map[string]string{"path": missing}))
	if !errors.Is(err, proto.ErrPathNotFound) || err.Error() != "stat "+missing+": no such file or directory" {
		t.Fatalf("err = %v", err)
	}
	if _, err := (&NodeRuntime{}).OnRPC(context.Background(), proto.RPCFSStat, mustRaw(t, map[string]string{"path": ""})); err == nil || errors.Is(err, proto.ErrPathNotFound) {
		t.Fatalf("empty path err = %v", err)
	}
}

func TestBenchProbeReportsA404AsTheEndpointNotFound(t *testing.T) {
	notFound := httptest.NewServer(http.NotFoundHandler())
	defer notFound.Close()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "offline", http.StatusServiceUnavailable) }))
	defer failing.Close()
	n := &NodeRuntime{}
	_, err := n.OnRPC(context.Background(), proto.RPCBenchProbe, mustRaw(t, map[string]string{"url": notFound.URL}))
	if !errors.Is(err, proto.ErrBenchEndpointNotFound) || err.Error() != "models probe returned 404 Not Found" {
		t.Fatalf("404 err = %v", err)
	}
	if _, err := n.OnRPC(context.Background(), proto.RPCBenchProbe, mustRaw(t, map[string]string{"url": failing.URL})); err == nil || errors.Is(err, proto.ErrBenchEndpointNotFound) {
		t.Fatalf("503 err = %v", err)
	}
}
