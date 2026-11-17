package templateprobe

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestInspectorUsesBoundedRangesAndExactTensorShapes(t *testing.T) {
	config := []byte(`{
  "architectures":["FixtureForCausalLM"],
  "model_type":"fixture",
  "num_hidden_layers":24,
  "max_position_embeddings":8192,
  "num_parameters":999,
  "torch_dtype":"bfloat16"
}`)
	shard := safetensorsFixture(t, map[string]safetensorsTestTensor{
		"model.embed.weight": {Dtype: "BF16", Shape: []int64{2, 3}},
		"model.norm.weight":  {Dtype: "BF16", Shape: []int64{4}},
	})
	index, _ := json.Marshal(map[string]any{
		"metadata":   map[string]any{"total_size": 20},
		"weight_map": map[string]string{"model.embed.weight": "model-00001-of-00001.safetensors", "model.norm.weight": "model-00001-of-00001.safetensors"},
	})
	files := map[string][]byte{
		"/org/model/resolve/rev/config.json":                      config,
		"/org/model/resolve/rev/model.safetensors.index.json":     index,
		"/org/model/resolve/rev/model-00001-of-00001.safetensors": shard,
	}
	var mu sync.Mutex
	ranges := map[string][]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		ranges[r.URL.Path] = append(ranges[r.URL.Path], r.Header.Get("Range"))
		mu.Unlock()
		http.ServeContent(w, r, r.URL.Path, time.Unix(0, 0), bytes.NewReader(body))
	}))
	defer server.Close()

	report, err := (Inspector{Fetcher: HTTPRangeFetcher{Client: server.Client()}}).Inspect(t.Context(), L1Input{
		Source: "huggingface", ModelID: "org/model", Revision: "rev", Endpoint: server.URL,
		Capabilities: L1Capabilities{ImageDigest: "sha256:fixture", Architectures: []string{"FixtureForCausalLM"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Inconclusive || report.Excluded {
		t.Fatalf("report status = %+v", report)
	}
	if report.Assessment != "metadata_only" || report.ImageDigest != "sha256:fixture" {
		t.Fatalf("scope not preserved: %+v", report)
	}
	if report.Metadata.ParameterCount != 10 || report.Metadata.ParameterCountText != "10" {
		t.Fatalf("parameter count = %d/%q, want exact header-derived 10", report.Metadata.ParameterCount, report.Metadata.ParameterCountText)
	}
	if report.Metadata.WeightBytes != 20 || report.Metadata.TensorCount != 2 || report.Metadata.ShardHeaders != 1 {
		t.Fatalf("safetensors metadata = %+v", report.Metadata)
	}
	if report.Truncation.MinLayers != 2 || !report.Truncation.Allowed {
		t.Fatalf("truncation = %+v", report.Truncation)
	}
	mu.Lock()
	defer mu.Unlock()
	for path, values := range ranges {
		for _, value := range values {
			if !strings.HasPrefix(value, "bytes=") {
				t.Errorf("%s request range = %q, want bounded byte range", path, value)
			}
		}
	}
	shardRanges := ranges["/org/model/resolve/rev/model-00001-of-00001.safetensors"]
	if len(shardRanges) != 2 || shardRanges[0] != "bytes=0-7" {
		t.Fatalf("shard ranges = %v, want length prefix then header", shardRanges)
	}
}

func TestInspectorCanStopAtIndexedWeightMetadata(t *testing.T) {
	config := []byte(`{"architectures":["FixtureForCausalLM"],"max_position_embeddings":4096}`)
	index, _ := json.Marshal(map[string]any{
		"metadata":   map[string]any{"total_size": 123456},
		"weight_map": map[string]string{"model.weight": "model-00001-of-00001.safetensors"},
	})
	shardRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/config.json"):
			http.ServeContent(w, r, "config.json", time.Unix(0, 0), bytes.NewReader(config))
		case strings.HasSuffix(r.URL.Path, "/model.safetensors.index.json"):
			http.ServeContent(w, r, "index.json", time.Unix(0, 0), bytes.NewReader(index))
		default:
			shardRequests++
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	report, err := (Inspector{
		Fetcher: HTTPRangeFetcher{Client: server.Client()}, SkipTensorHeadersWhenIndexed: true,
	}).Inspect(t.Context(), L1Input{Source: "huggingface", ModelID: "org/model", Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if report.Metadata.WeightBytes != 123456 || report.Inconclusive || shardRequests != 0 {
		t.Fatalf("indexed metadata report=%+v shard requests=%d", report, shardRequests)
	}
}

func TestInspectorFallsBackToRepositoryWeightFileSizes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/org/model/tree/main":
			_, _ = w.Write([]byte(`[{"type":"file","path":"part-1.safetensors","size":100},{"type":"file","path":"part-2.bin","size":23},{"type":"file","path":"README.md","size":999}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	report, err := (Inspector{
		Fetcher: HTTPRangeFetcher{Client: server.Client()}, ListWeightFilesFallback: true,
	}).Inspect(t.Context(), L1Input{Source: "huggingface", ModelID: "org/model", Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if report.Metadata.WeightBytes != 123 || !report.Inconclusive {
		t.Fatalf("repository file fallback metadata = %+v", report.Metadata)
	}
}

func TestInspectorCapabilityMissIsScopedDeterministicExclusion(t *testing.T) {
	config := []byte(`{"architectures":["NewArchitecture"],"num_hidden_layers":2,"quantization_config":{"quant_method":"awq"}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/config.json"):
			http.ServeContent(w, r, "config.json", time.Unix(0, 0), bytes.NewReader(config))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	report, err := (Inspector{Fetcher: HTTPRangeFetcher{Client: server.Client()}}).Inspect(t.Context(), L1Input{
		Source: "huggingface", ModelID: "org/model", Endpoint: server.URL,
		Capabilities: L1Capabilities{ImageDigest: "sha256:image-a", Architectures: []string{"OldArchitecture"}, Quantizations: []string{"gptq"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Excluded || report.ImageDigest != "sha256:image-a" {
		t.Fatalf("report = %+v, want exclusion scoped to image digest", report)
	}
	codes := map[string]bool{}
	for _, finding := range report.Findings {
		if finding.Excludes && finding.Deterministic {
			codes[finding.Code] = true
		}
	}
	if !codes[FindingArchitectureNotRegistered] || !codes[FindingQuantizationKernelUnavailable] {
		t.Fatalf("deterministic finding codes = %v", codes)
	}
}

func TestInspectorInvalidConfigDoesNotClaimProbePass(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "config.json", time.Unix(0, 0), strings.NewReader("{not-json"))
	}))
	defer server.Close()
	report, err := (Inspector{Fetcher: HTTPRangeFetcher{Client: server.Client()}}).Inspect(t.Context(), L1Input{
		Source: "huggingface", ModelID: "org/model", Endpoint: server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Excluded || !report.Inconclusive || report.Assessment != "metadata_only" {
		t.Fatalf("invalid config report = %+v", report)
	}
}

func TestInspectorDoesNotApplyUnscopedCapabilitySnapshot(t *testing.T) {
	config := []byte(`{"architectures":["NewArchitecture"],"num_hidden_layers":2}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/config.json") {
			http.ServeContent(w, r, "config.json", time.Unix(0, 0), bytes.NewReader(config))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	report, err := (Inspector{Fetcher: HTTPRangeFetcher{Client: server.Client()}}).Inspect(t.Context(), L1Input{
		Source: "huggingface", ModelID: "org/model", Endpoint: server.URL,
		Capabilities: L1Capabilities{Architectures: []string{"OldArchitecture"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Excluded || !report.Inconclusive {
		t.Fatalf("unscoped capabilities report = %+v", report)
	}
	for _, finding := range report.Findings {
		if finding.Code == FindingArchitectureNotRegistered {
			t.Fatalf("unscoped capability miss excluded architecture: %+v", report)
		}
	}
}

type safetensorsTestTensor struct {
	Dtype string
	Shape []int64
}

func safetensorsFixture(t *testing.T, tensors map[string]safetensorsTestTensor) []byte {
	t.Helper()
	header := map[string]any{}
	offset := int64(0)
	for name, tensor := range tensors {
		count, ok := tensorElements(tensor.Shape)
		if !ok {
			t.Fatalf("invalid test tensor shape %v", tensor.Shape)
		}
		bytesPerElement := int64(2)
		if tensor.Dtype == "F32" {
			bytesPerElement = 4
		}
		size := count * bytesPerElement
		header[name] = map[string]any{"dtype": tensor.Dtype, "shape": tensor.Shape, "data_offsets": []int64{offset, offset + size}}
		offset += size
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	result := make([]byte, 8+len(headerJSON)+int(offset))
	binary.LittleEndian.PutUint64(result[:8], uint64(len(headerJSON)))
	copy(result[8:], headerJSON)
	return result
}

func TestContentRangeTotal(t *testing.T) {
	if got := contentRangeTotal("bytes 0-7/1234"); got != 1234 {
		t.Fatalf("contentRangeTotal = %d", got)
	}
	if got := contentRangeTotal(fmt.Sprintf("bytes 0-7/%s", "*")); got != 0 {
		t.Fatalf("wildcard total = %d", got)
	}
}

func TestHTTPRangeFetcherAcceptsModelScopeSmallFileResponse(t *testing.T) {
	body := []byte(`{"num_hidden_layers":28}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-4194303/%d", len(body)))
		w.Header().Set("Content-Length", "4194304")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	got, err := (HTTPRangeFetcher{Client: server.Client()}).FetchRange(t.Context(), server.URL, 0, 4194303)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Complete || got.TotalBytes != int64(len(body)) || !bytes.Equal(got.Body, body) {
		t.Fatalf("FetchRange() = %+v, want complete %d-byte body", got, len(body))
	}
}

func TestHTTPRangeFetcherRejectsTruncatedModelScopeResponse(t *testing.T) {
	body := []byte(`{"num_hidden_layers":28}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-4194303/%d", len(body)+1))
		w.Header().Set("Content-Length", "4194304")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	if _, err := (HTTPRangeFetcher{Client: server.Client()}).FetchRange(t.Context(), server.URL, 0, 4194303); err == nil {
		t.Fatal("FetchRange accepted a body shorter than Content-Range total")
	}
}

// The probe validates catalog specs, whose weight sources are always one of
// the two repository APIs. An HTTP directory is a registered model's own
// endpoint and never appears in a spec, so the probe refuses it outright
// rather than issuing repository-shaped requests against a file server.
func TestL1RefusesASourceTheCatalogNeverDeclares(t *testing.T) {
	for _, source := range []string{"url", "local", "s3"} {
		report, err := (Inspector{}).Inspect(t.Context(), L1Input{Source: source, ModelID: "Qwen/Qwen3-0.6B"})
		if err == nil || !strings.Contains(err.Error(), "unsupported source") {
			t.Fatalf("Inspect(%q) err = %v, want an unsupported-source refusal", source, err)
		}
		if report.Source != source {
			t.Fatalf("refused report source = %q, want %q echoed back", report.Source, source)
		}
	}
}

// A finding uses the check vocabulary (ADR 0067): an UPPER_SNAKE code, a
// status of fail or warn, and the parser's own text as evidence only.
func TestInspectorFindingWire(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "config.json", time.Unix(0, 0), strings.NewReader("{not-json"))
	}))
	defer server.Close()
	report, err := (Inspector{Fetcher: HTTPRangeFetcher{Client: server.Client()}}).Inspect(t.Context(), L1Input{
		Source: "huggingface", ModelID: "org/model", Endpoint: server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	var invalid *Finding
	for i := range report.Findings {
		f := &report.Findings[i]
		if f.Code != strings.ToUpper(f.Code) || (f.Status != proto.CheckFail && f.Status != proto.CheckWarn) {
			t.Errorf("finding %+v is outside the check vocabulary", f)
		}
		if f.Code == FindingConfigInvalid {
			invalid = f
		}
	}
	if invalid == nil || invalid.Status != proto.CheckFail || invalid.Message != "config.json is not a valid model config" || invalid.Evidence == "" {
		t.Fatalf("invalid config finding = %+v (findings %+v)", invalid, report.Findings)
	}
	raw, err := json.Marshal(Finding{Code: FindingWeightFilesMissing, Status: proto.CheckWarn, Message: "m", Evidence: "e"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"code":"WEIGHT_FILES_MISSING","status":"warn","message":"m","evidence":"e","deterministic":false,"excludes":false}`; string(raw) != want {
		t.Fatalf("wire = %s, want %s", raw, want)
	}
	for _, family := range []FailureFamily{FailureArchitecture, FailureQuantKernel, FailureArgv, FailureConfig, FailureMissingOperator, FailureDependency, FailureCatalogAsset, FailureForwardKernel} {
		if string(family) != strings.ToUpper(string(family)) {
			t.Errorf("family %s is not UPPER_SNAKE", family)
		}
	}
}
