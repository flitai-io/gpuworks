package templateprobe

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
	"gpuworks.ai/gpuworks/pkg/safetensors"
)

const (
	defaultConfigRange = int64(4 << 20)
	defaultIndexRange  = int64(16 << 20)
	defaultHeaderRange = int64(32 << 20)
)

type ByteRange struct {
	Body       []byte
	StatusCode int
	TotalBytes int64
	Complete   bool
}

type RangeFetcher interface {
	FetchRange(context.Context, string, int64, int64) (ByteRange, error)
}

type HTTPRangeFetcher struct {
	Client  *http.Client
	Headers map[string]string
}

func (f HTTPRangeFetcher) FetchRange(ctx context.Context, url string, start, end int64) (ByteRange, error) {
	if start < 0 || end < start {
		return ByteRange{}, fmt.Errorf("invalid byte range %d-%d", start, end)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ByteRange{}, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	for name, value := range f.Headers {
		req.Header.Set(name, value)
	}
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return ByteRange{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return ByteRange{StatusCode: resp.StatusCode}, &HTTPStatusError{URL: url, StatusCode: resp.StatusCode}
	}
	limit := end - start + 1
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	total := contentRangeTotal(resp.Header.Get("Content-Range"))
	// ModelScope's resolve endpoint can return status 200 with Content-Length
	// equal to the requested range while sending only the complete small file.
	// Its Content-Range still carries the real total. Accept that observed API
	// behavior only when the received body exactly satisfies the declared total;
	// every other unexpected EOF remains a transport failure.
	if readErr != nil && !(errors.Is(readErr, io.ErrUnexpectedEOF) && start == 0 && total > 0 && int64(len(body)) == total) {
		return ByteRange{}, readErr
	}
	if int64(len(body)) > limit {
		return ByteRange{}, fmt.Errorf("%s ignored range and exceeded %d-byte inspection limit", url, limit)
	}
	if total == 0 && resp.ContentLength >= 0 {
		total = resp.ContentLength
	}
	complete := resp.StatusCode == http.StatusOK || (total > 0 && start == 0 && int64(len(body)) >= total)
	return ByteRange{Body: body, StatusCode: resp.StatusCode, TotalBytes: total, Complete: complete}, nil
}

type HTTPStatusError struct {
	URL        string
	StatusCode int
}

func (e *HTTPStatusError) Error() string { return fmt.Sprintf("%s: http %d", e.URL, e.StatusCode) }

type L1Capabilities struct {
	ImageDigest      string   `json:"image_digest,omitempty"`
	Architectures    []string `json:"architectures,omitempty"`
	Quantizations    []string `json:"quantizations,omitempty"`
	MaxContextLength int64    `json:"max_context_length,omitempty"`
}

type L1Input struct {
	Source       string         `json:"source"`
	ModelID      string         `json:"model_id"`
	Revision     string         `json:"revision,omitempty"`
	Endpoint     string         `json:"endpoint,omitempty"`
	Capabilities L1Capabilities `json:"capabilities,omitempty"`
}

// Finding is one L1 check result (ADR 0067): Status is proto.CheckFail for a
// deterministic exclusion and proto.CheckWarn for an inconclusive or advisory
// one; Code is UPPER_SNAKE; Message is the English fallback; Evidence is the
// external error text (an HTTP fetch or a parser) behind it.
type Finding struct {
	Code          string `json:"code"`
	Status        string `json:"status"`
	Message       string `json:"message"`
	Evidence      string `json:"evidence,omitempty"`
	Deterministic bool   `json:"deterministic"`
	Excludes      bool   `json:"excludes"`
}

// L1 finding codes. The exclusions share the launch failure family codes
// (signatures.go) of the failure they predict.
const (
	FindingConfigUnavailable              = "CONFIG_UNAVAILABLE"
	FindingConfigTooLarge                 = "CONFIG_TOO_LARGE"
	FindingConfigInvalid                  = string(FailureConfig)
	FindingWeightFileListUnavailable      = "WEIGHT_FILE_LIST_UNAVAILABLE"
	FindingWeightFilesMissing             = "WEIGHT_FILES_MISSING"
	FindingSafetensorsIndexInvalid        = "SAFETENSORS_INDEX_INVALID"
	FindingSafetensorsHeadersIncomplete   = "SAFETENSORS_HEADERS_INCOMPLETE"
	FindingSafetensorsMetadataUnavailable = "SAFETENSORS_METADATA_UNAVAILABLE"
	FindingCapabilitySnapshotUnscoped     = "CAPABILITY_SNAPSHOT_UNSCOPED"
	FindingArchitectureNotRegistered      = string(FailureArchitecture)
	FindingQuantizationKernelUnavailable  = string(FailureQuantKernel)
	FindingContextLengthExceeded          = string(FailureConfig)
	FindingDtypeDeclarationMismatch       = "DTYPE_DECLARATION_MISMATCH"
)

type L1Metadata struct {
	Architectures      []string `json:"architectures,omitempty"`
	ModelType          string   `json:"model_type,omitempty"`
	ContextLength      int64    `json:"context_length,omitempty"`
	ParameterCount     int64    `json:"parameter_count,omitempty"`
	ParameterCountText string   `json:"parameter_count_text,omitempty"`
	WeightBytes        int64    `json:"weight_bytes,omitempty"`
	TensorCount        int      `json:"tensor_count,omitempty"`
	TorchDtype         string   `json:"torch_dtype,omitempty"`
	DominantDtype      string   `json:"dominant_safetensors_dtype,omitempty"`
	Quantization       string   `json:"quantization,omitempty"`
	MoE                bool     `json:"moe"`
	Multimodal         bool     `json:"multimodal"`
	ShardHeaders       int      `json:"shard_headers_inspected,omitempty"`
}

type L1Report struct {
	Level        string             `json:"level"`
	Assessment   string             `json:"assessment"`
	Source       string             `json:"source"`
	ModelID      string             `json:"model_id"`
	Revision     string             `json:"revision,omitempty"`
	ImageDigest  string             `json:"image_digest,omitempty"`
	Metadata     L1Metadata         `json:"metadata"`
	Truncation   TruncationDecision `json:"truncation"`
	Findings     []Finding          `json:"findings,omitempty"`
	Excluded     bool               `json:"excluded"`
	Inconclusive bool               `json:"inconclusive"`
}

type Inspector struct {
	Fetcher                      RangeFetcher
	ConfigMaxBytes               int64
	IndexMaxBytes                int64
	HeaderMaxBytes               int64
	SkipTensorHeadersWhenIndexed bool
	ListWeightFilesFallback      bool
}

func (i Inspector) Inspect(ctx context.Context, input L1Input) (L1Report, error) {
	report := L1Report{
		Level: "L1", Assessment: "metadata_only", Source: input.Source, ModelID: input.ModelID,
		Revision: input.Revision, ImageDigest: input.Capabilities.ImageDigest,
	}
	if strings.TrimSpace(input.ModelID) == "" {
		return report, fmt.Errorf("model_id is required")
	}
	if input.Source == "" {
		input.Source = modelrepo.SourceHF
		report.Source = input.Source
	}
	if input.Source != modelrepo.SourceHF && input.Source != modelrepo.SourceMS {
		return report, fmt.Errorf("unsupported source %q", input.Source)
	}
	fetcher := i.Fetcher
	if fetcher == nil {
		fetcher = HTTPRangeFetcher{}
	}
	configLimit := positiveLimit(i.ConfigMaxBytes, defaultConfigRange)
	configURL := modelrepo.ResolveURL(input.Source, input.ModelID, input.Revision, "config.json", input.Endpoint)
	configRange, err := fetcher.FetchRange(ctx, configURL, 0, configLimit-1)
	switch {
	case err != nil:
		report.Inconclusive = true
		report.Findings = append(report.Findings, inconclusiveEvidence(FindingConfigUnavailable, "config.json could not be read", err))
	case !configRange.Complete:
		report.Inconclusive = true
		report.Findings = append(report.Findings, inconclusive(FindingConfigTooLarge, "config.json exceeded the bounded range request"))
	default:
		config, parseErr := ParseModelConfig(configRange.Body)
		if parseErr != nil {
			report.Inconclusive = true
			report.Findings = append(report.Findings, deterministicExclusionEvidence(FindingConfigInvalid, "config.json is not a valid model config", parseErr))
			report.Excluded = true
		} else {
			report.Metadata = metadataFromConfig(config)
			report.Truncation = DecideTruncation(config)
		}
	}

	indexLimit := positiveLimit(i.IndexMaxBytes, defaultIndexRange)
	indexURL := modelrepo.ResolveURL(input.Source, input.ModelID, input.Revision, "model.safetensors.index.json", input.Endpoint)
	indexRange, indexErr := fetcher.FetchRange(ctx, indexURL, 0, indexLimit-1)
	if indexErr == nil && indexRange.Complete {
		i.inspectIndexAndHeaders(ctx, fetcher, input, indexRange.Body, &report)
	} else {
		i.inspectSingleSafetensors(ctx, fetcher, input, &report)
	}
	if report.Metadata.WeightBytes == 0 && i.ListWeightFilesFallback {
		i.inspectWeightFileList(ctx, fetcher, input, &report)
	}
	i.applyCapabilities(input.Capabilities, &report)
	for _, finding := range report.Findings {
		if finding.Excludes {
			report.Excluded = true
		}
	}
	return report, nil
}

func (i Inspector) inspectWeightFileList(ctx context.Context, fetcher RangeFetcher, input L1Input, report *L1Report) {
	httpFetcher, ok := fetcher.(HTTPRangeFetcher)
	if !ok {
		return
	}
	files, err := modelrepo.ListFiles(ctx, input.Source, input.ModelID, input.Revision, input.Endpoint, httpFetcher.Headers, modelrepo.RequestOptions{Client: httpFetcher.Client})
	if err != nil {
		report.Findings = append(report.Findings, inconclusiveEvidence(FindingWeightFileListUnavailable, "the repository file list could not be read", err))
		report.Inconclusive = true
		return
	}
	for _, file := range files {
		path := strings.ToLower(file.Path)
		if strings.HasSuffix(path, ".safetensors") || strings.HasSuffix(path, ".bin") {
			report.Metadata.WeightBytes += file.Size
		}
	}
	if report.Metadata.WeightBytes == 0 {
		report.Findings = append(report.Findings, inconclusive(FindingWeightFilesMissing, "repository listing contains no safetensors or bin weight files"))
		report.Inconclusive = true
	}
}

func metadataFromConfig(config ModelConfig) L1Metadata {
	metadata := L1Metadata{
		Architectures: append([]string(nil), config.Architectures...), ModelType: config.ModelType,
		ContextLength: config.MaxPositionEmbedding, ParameterCountText: config.ParameterCount,
		TorchDtype: config.TorchDtype, Quantization: config.QuantizationMethod,
		MoE: hasMoESignal(config) || config.FirstKDenseReplace != nil, Multimodal: config.Multimodal,
	}
	if parsed, err := strconv.ParseInt(config.ParameterCount, 10, 64); err == nil && parsed > 0 {
		metadata.ParameterCount = parsed
	}
	return metadata
}

func (i Inspector) inspectIndexAndHeaders(ctx context.Context, fetcher RangeFetcher, input L1Input, body []byte, report *L1Report) {
	var index struct {
		Metadata struct {
			TotalSize json.Number `json:"total_size"`
		} `json:"metadata"`
		WeightMap map[string]string `json:"weight_map"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&index); err != nil {
		report.Inconclusive = true
		report.Findings = append(report.Findings, inconclusiveEvidence(FindingSafetensorsIndexInvalid, "model.safetensors.index.json is not valid", err))
		return
	}
	if value, err := index.Metadata.TotalSize.Int64(); err == nil && value > 0 {
		report.Metadata.WeightBytes = value
	}
	shards := make(map[string]struct{})
	for _, shard := range index.WeightMap {
		if strings.TrimSpace(shard) != "" {
			shards[shard] = struct{}{}
		}
	}
	names := make([]string, 0, len(shards))
	for shard := range shards {
		names = append(names, shard)
	}
	sort.Strings(names)
	if i.SkipTensorHeadersWhenIndexed && report.Metadata.WeightBytes > 0 {
		return
	}
	if !i.inspectHeaders(ctx, fetcher, input, names, report) {
		report.Inconclusive = true
		report.Findings = append(report.Findings, inconclusive(FindingSafetensorsHeadersIncomplete, "not every indexed safetensors shard header was available within the inspection limit"))
	}
}

func (i Inspector) inspectSingleSafetensors(ctx context.Context, fetcher RangeFetcher, input L1Input, report *L1Report) {
	if !i.inspectHeaders(ctx, fetcher, input, []string{"model.safetensors"}, report) {
		report.Inconclusive = true
		report.Findings = append(report.Findings, inconclusive(FindingSafetensorsMetadataUnavailable, "neither a complete index nor model.safetensors header was available"))
	}
}

func (i Inspector) inspectHeaders(ctx context.Context, fetcher RangeFetcher, input L1Input, shards []string, report *L1Report) bool {
	var parameterCount int64
	perDtype := map[string]int64{}
	allInspected := len(shards) > 0
	for _, shard := range shards {
		url := modelrepo.ResolveURL(input.Source, input.ModelID, input.Revision, shard, input.Endpoint)
		prefix, err := fetcher.FetchRange(ctx, url, 0, 7)
		if err != nil || len(prefix.Body) != 8 {
			allInspected = false
			continue
		}
		headerLength := binary.LittleEndian.Uint64(prefix.Body)
		limit := uint64(positiveLimit(i.HeaderMaxBytes, defaultHeaderRange))
		if headerLength == 0 || headerLength > limit {
			allInspected = false
			continue
		}
		headerRange, err := fetcher.FetchRange(ctx, url, 0, int64(headerLength)+7)
		if err != nil || uint64(len(headerRange.Body)) < headerLength+8 {
			allInspected = false
			continue
		}
		header, err := safetensors.ParseHeader(bytes.NewReader(headerRange.Body))
		if err != nil {
			allInspected = false
			continue
		}
		report.Metadata.ShardHeaders++
		report.Metadata.TensorCount += len(header)
		for _, tensor := range header {
			count, ok := tensorElements(tensor.Shape)
			if !ok {
				allInspected = false
				continue
			}
			parameterCount += count
			if len(tensor.Offsets) == 2 && tensor.Offsets[1] >= tensor.Offsets[0] {
				perDtype[tensor.Dtype] += tensor.Offsets[1] - tensor.Offsets[0]
			}
		}
	}
	if allInspected {
		report.Metadata.ParameterCount = parameterCount
		report.Metadata.ParameterCountText = strconv.FormatInt(parameterCount, 10)
	}
	report.Metadata.DominantDtype = dominantDtype(perDtype)
	if report.Metadata.WeightBytes == 0 {
		for _, bytes := range perDtype {
			report.Metadata.WeightBytes += bytes
		}
	}
	return allInspected
}

func (i Inspector) applyCapabilities(capabilities L1Capabilities, report *L1Report) {
	if capabilities.ImageDigest == "" && (len(capabilities.Architectures) > 0 || len(capabilities.Quantizations) > 0 || capabilities.MaxContextLength > 0) {
		report.Inconclusive = true
		report.Findings = append(report.Findings, inconclusive(FindingCapabilitySnapshotUnscoped, "image_digest is required before capability misses can exclude a combination"))
		return
	}
	architecture := ""
	if len(report.Metadata.Architectures) > 0 {
		architecture = report.Metadata.Architectures[0]
	}
	if len(capabilities.Architectures) > 0 && architecture != "" && !containsFold(capabilities.Architectures, architecture) {
		report.Findings = append(report.Findings, deterministicExclusion(FindingArchitectureNotRegistered, fmt.Sprintf("architecture %s is absent from the image capability snapshot", architecture)))
	}
	if len(capabilities.Quantizations) > 0 && report.Metadata.Quantization != "" && !containsFold(capabilities.Quantizations, report.Metadata.Quantization) {
		report.Findings = append(report.Findings, deterministicExclusion(FindingQuantizationKernelUnavailable, fmt.Sprintf("quantization %s is absent from the image capability snapshot", report.Metadata.Quantization)))
	}
	if capabilities.MaxContextLength > 0 && report.Metadata.ContextLength > capabilities.MaxContextLength {
		report.Findings = append(report.Findings, deterministicExclusion(FindingContextLengthExceeded, fmt.Sprintf("context length %d exceeds image limit %d", report.Metadata.ContextLength, capabilities.MaxContextLength)))
	}
	if report.Metadata.TorchDtype != "" && report.Metadata.DominantDtype != "" && report.Metadata.Quantization == "" {
		expected := safetensorsDtype(report.Metadata.TorchDtype)
		if expected != "" && expected != report.Metadata.DominantDtype {
			report.Findings = append(report.Findings, Finding{Code: FindingDtypeDeclarationMismatch, Status: proto.CheckWarn, Message: fmt.Sprintf("config declares %s but dominant safetensors dtype is %s", expected, report.Metadata.DominantDtype)})
		}
	}
}

func deterministicExclusion(code, message string) Finding {
	return Finding{Code: code, Status: proto.CheckFail, Message: message, Deterministic: true, Excludes: true}
}

func deterministicExclusionEvidence(code, message string, err error) Finding {
	finding := deterministicExclusion(code, message)
	finding.Evidence = err.Error()
	return finding
}

func inconclusive(code, message string) Finding {
	return Finding{Code: code, Status: proto.CheckWarn, Message: message}
}

func inconclusiveEvidence(code, message string, err error) Finding {
	finding := inconclusive(code, message)
	finding.Evidence = err.Error()
	return finding
}

func tensorElements(shape []int64) (int64, bool) {
	if len(shape) == 0 {
		return 1, true
	}
	result := int64(1)
	for _, dimension := range shape {
		if dimension < 0 || (dimension > 0 && result > (1<<63-1)/dimension) {
			return 0, false
		}
		result *= dimension
	}
	return result, true
}

func dominantDtype(perDtype map[string]int64) string {
	name := ""
	var largest int64
	for dtype, size := range perDtype {
		if size > largest || (size == largest && dtype < name) {
			name, largest = dtype, size
		}
	}
	return name
}

func safetensorsDtype(dtype string) string {
	switch strings.ToLower(strings.TrimSpace(dtype)) {
	case "bfloat16", "bf16":
		return "BF16"
	case "float16", "half", "fp16":
		return "F16"
	case "float32", "float", "fp32":
		return "F32"
	default:
		return ""
	}
}

func containsFold(values []string, expected string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(expected)) {
			return true
		}
	}
	return false
}

func positiveLimit(value, fallback int64) int64 {
	if value > 0 {
		return value
	}
	return fallback
}

func contentRangeTotal(value string) int64 {
	slash := strings.LastIndexByte(value, '/')
	if slash < 0 || slash+1 >= len(value) || value[slash+1:] == "*" {
		return 0
	}
	total, _ := strconv.ParseInt(value[slash+1:], 10, 64)
	return total
}
