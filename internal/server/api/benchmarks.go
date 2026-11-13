package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	benchmeta "gpuworks.ai/gpuworks/pkg/bench"
	"gpuworks.ai/gpuworks/pkg/bench/dataset"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

const benchmarkCancelTimeout = 10 * time.Second

type benchmarkExecution struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (a *apiHandlers) getBenchmarkMeta(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, benchmeta.Dictionary())
}

func (a *apiHandlers) listBenchmarks(w http.ResponseWriter, r *http.Request) {
	runs, err := a.d.Store.ListBenchmarkRuns()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	out := make([]benchmarkDTO, 0, len(runs))
	for i := range runs {
		out = append(out, a.benchmarkDTO(&runs[i], false))
	}
	writeJSON(w, http.StatusOK, dto.NewList(out))
}
func (a *apiHandlers) getBenchmark(w http.ResponseWriter, r *http.Request) {
	run, err := a.d.Store.GetBenchmarkRun(parseUint(chi.URLParam(r, "id")))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, 404, err)
		return
	}
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, a.benchmarkDTO(run, true))
}

func (a *apiHandlers) downloadBenchmarkDetail(w http.ResponseWriter, r *http.Request) {
	runID := parseUint(chi.URLParam(r, "id"))
	seq := parseUint(chi.URLParam(r, "seq"))
	points, err := a.d.Store.ListBenchmarkPoints(runID)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	for _, p := range points {
		if uint(p.Seq) == seq && p.DetailPath != "" {
			clean := filepath.Clean(p.DetailPath)
			root := filepath.Clean(a.d.BenchmarkArtifactDir)
			if root == "." || clean == "." || !strings.HasPrefix(clean, root+string(os.PathSeparator)) {
				writeErr(w, 404, store.ErrNotFound)
				return
			}
			f, e := os.Open(clean)
			if e != nil {
				writeErr(w, 404, e)
				return
			}
			defer f.Close()
			w.Header().Set("Content-Type", "application/gzip")
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=benchmark-%d-point-%d.jsonl.gz", runID, p.Seq))
			http.ServeContent(w, r, filepath.Base(clean), time.Time{}, f)
			return
		}
	}
	writeErr(w, 404, store.ErrNotFound)
}
func (a *apiHandlers) createBenchmark(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name            string          `json:"name"`
		TargetKind      string          `json:"target_kind"`
		DeploymentID    uint            `json:"deployment_id"`
		BaseURL         string          `json:"base_url"`
		APIKey          string          `json:"api_key"`
		Model           string          `json:"model"`
		LoadOrigin      string          `json:"load_origin"`
		Dataset         json.RawMessage `json:"dataset"`
		Mode            string          `json:"mode"`
		Search          json.RawMessage `json:"search"`
		KeepDetail      bool            `json:"keep_detail"`
		DetailCapMB     int             `json:"detail_cap_mb"`
		NumGPUs         int             `json:"num_gpus"`
		TokenizerRef    string          `json:"tokenizer_ref"`
		TokenizerSource string          `json:"tokenizer_source"`
		// AllowConflict admits a run the admission check would otherwise refuse.
		// The client sends it only after showing the conflicts to the user.
		AllowConflict bool `json:"allow_conflict"`
		// InstanceUUID measures one instance of the deployment directly instead
		// of the deployment through the gateway (plan D9).
		InstanceUUID string `json:"instance_uuid"`
	}
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeErr(w, 400, benchmarkParameterError("body", "request body must contain one JSON object"))
		return
	}
	if !validateName(w, req.Name) {
		return
	}
	if req.TargetKind == "" {
		req.TargetKind = "external"
	}
	if req.TargetKind != "external" && req.TargetKind != "instance" {
		writeErr(w, 400, benchmarkParameterError("target_kind", fmt.Sprintf("unknown benchmark target_kind %q", req.TargetKind)))
		return
	}
	if req.Mode == "" {
		req.Mode = "grid"
	}
	if req.NumGPUs < 0 {
		writeProductError(w, 400, ErrorBenchmarkGPUCountNegative, "num_gpus cannot be negative", nil)
		return
	}
	if len(req.Dataset) == 0 {
		req.Dataset = json.RawMessage(fmt.Sprintf(`{"kind":"random","input_tokens":%d,"output_tokens":%d}`, benchmeta.DefaultInputTokens, benchmeta.DefaultOutputTokens))
	}
	datasetConfig, err := validateBenchmarkDataset(req.Dataset)
	if err != nil {
		writeProductError(w, 400, ErrorBenchmarkAgenticInvalid, "invalid benchmark dataset config", map[string]any{"detail": err.Error()})
		return
	}
	if datasetConfig.Kind == "agenttrace" {
		writeProductError(w, 400, ErrorBenchmarkDatasetRetired, "ETO WebShop has been removed and cannot be used for new runs", map[string]any{"kind": datasetConfig.Kind})
		return
	}
	if req.DetailCapMB < 0 {
		writeErr(w, 400, benchmarkParameterError("detail_cap_mb", "detail_cap_mb cannot be negative"))
		return
	}
	if req.DetailCapMB == 0 {
		req.DetailCapMB = benchmeta.DefaultDetailCapMB
	}
	if req.DetailCapMB > benchmeta.MaxDetailCapMB {
		writeProductError(w, 400, ErrorBenchmarkDetailCapTooLarge, fmt.Sprintf("detail_cap_mb cannot exceed %d", benchmeta.MaxDetailCapMB), map[string]any{"maximum": benchmeta.MaxDetailCapMB})
		return
	}
	if len(req.Search) == 0 {
		req.Search = json.RawMessage(`{"concurrency":1}`)
	}
	if err := validateBenchmarkLoad(req.Mode, req.Search, datasetConfig); err != nil {
		writeErr(w, 400, err)
		return
	}
	if req.TargetKind == "external" && strings.TrimSpace(req.Model) == "" {
		writeErr(w, 400, benchmarkParameterError("model", "external target model required"))
		return
	}
	instanceUUID := ""
	var targetDeployment *store.Deployment
	var targetInstance *store.Instance
	var targetInstances []store.Instance
	var targetNode *store.Node
	var targetNodeID uint
	entry := ""
	if req.TargetKind == "instance" {
		target, targetErr := a.resolveManagedBenchmarkTarget(r, req.DeploymentID, req.InstanceUUID, req.LoadOrigin)
		if targetErr != nil {
			targetErr.write(w)
			return
		}
		targetDeployment, targetInstances, targetInstance, targetNode = target.deployment, target.instances, target.instance, target.node
		if targetNode != nil {
			targetNodeID = targetNode.ID
		}
		req.BaseURL, req.Model, req.LoadOrigin, entry = target.baseURL, target.model, target.origin, target.entry
		instanceUUID = target.instanceUUID
		if target.apiKey != "" {
			req.APIKey = target.apiKey
		}
		if target.numGPUs > 0 {
			req.NumGPUs = target.numGPUs
		}
	}
	if req.TargetKind == "external" {
		if strings.TrimSpace(req.BaseURL) == "" {
			writeProductError(w, 400, ErrorBenchmarkExternalURLRequired, "external target url required", nil)
			return
		}
		req.BaseURL, _ = loadgen.NormalizeBaseURL(req.BaseURL)
		if err := a.probeExternalBenchmark(r.Context(), req.LoadOrigin, req.BaseURL, req.APIKey); err != nil {
			writeBenchmarkProbeError(w, fmt.Errorf("external endpoint probe failed: %w", err), req.BaseURL)
			return
		}
	}
	if req.LoadOrigin == "" {
		req.LoadOrigin = "server"
	}
	if req.TargetKind == "external" && req.LoadOrigin == "auto" {
		req.LoadOrigin = "server"
	}
	if strings.TrimSpace(req.TokenizerSource) == "" {
		// No source named: the client's own reference stands, and a managed
		// target with no reference uses its declared tokenizer or model repository.
		if req.TargetKind == "instance" && strings.TrimSpace(req.TokenizerRef) == "" && targetDeployment != nil {
			if model, e := a.d.Store.GetModel(targetDeployment.ModelID); e == nil {
				req.TokenizerRef = tokenizerRefForModel(model)
				if declared := a.declaredBenchmarkTokenizer(targetDeployment, model); declared != "" {
					req.TokenizerRef = declared
				}
			}
		}
	} else {
		resolved, err := a.resolveBenchmarkTokenizer(r.Context(), benchmarkTokenizerRequest{
			Source:     strings.TrimSpace(req.TokenizerSource),
			Reference:  req.TokenizerRef,
			TargetKind: req.TargetKind,
			LoadOrigin: req.LoadOrigin,
			Deployment: targetDeployment,
			Node:       targetNode,
		})
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		req.TokenizerRef = resolved
	}
	if datasetConfig.Kind == "agentic" {
		if strings.TrimSpace(req.TokenizerRef) == "" {
			writeProductError(w, 400, ErrorBenchmarkAgenticTokenizer, "agentic workload requires a tokenizer: upload a tokenizer.json or name the model repository", nil)
			return
		}
		req.Dataset, _ = json.Marshal(datasetConfig)
	}
	loadWarnings, err := a.benchmarkLoadSupport(req.LoadOrigin, req.Search)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	conflicts := a.benchmarkConflicts(req.LoadOrigin, req.TargetKind, req.DeploymentID, targetNodeID)
	if len(conflicts) > 0 && !req.AllowConflict {
		writeBenchmarkConflict(w, conflicts)
		return
	}
	targetSnapshot, err := a.benchmarkTargetSnapshot(req.TargetKind, entry, req.BaseURL, req.Model, req.LoadOrigin, req.NumGPUs, targetDeployment, targetInstance, targetInstances)
	if err != nil {
		writeProductError(w, 500, ErrorBenchmarkTargetSnapshotFailed, fmt.Sprintf("snapshot benchmark target: %v", err), nil)
		return
	}
	encryptedKey, err := store.EncryptAIKey(a.d.AISecretbox, req.APIKey)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	run, err := a.d.Store.CreateBenchmarkRun(&store.BenchmarkRun{Name: req.Name, TargetKind: req.TargetKind, DeploymentID: req.DeploymentID, InstanceUUID: instanceUUID, BaseURL: req.BaseURL, APIKey: encryptedKey, Model: req.Model, LoadOrigin: req.LoadOrigin, NumGPUs: req.NumGPUs, Dataset: string(req.Dataset), Mode: req.Mode, Search: string(req.Search), TokenizerRef: req.TokenizerRef, TargetSnapshot: targetSnapshot, KeepDetail: req.KeepDetail, DetailCapMB: req.DetailCapMB})
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	// Reaching this point with conflicts means the user overrode them; the run
	// carries that decision so the detail page and the report can show it.
	a.recordBenchmarkConflictWarning(run.ID, conflicts)
	for _, warning := range loadWarnings {
		_ = a.d.Store.AddBenchmarkWarning(run.ID, warning)
	}
	if req.LoadOrigin != "server" && a.d.NodeSync != nil {
		_ = a.d.NodeSync.SyncNode(req.LoadOrigin)
	} else if req.LoadOrigin == "server" {
		ctx, cancel := context.WithCancel(context.Background())
		execution := &benchmarkExecution{cancel: cancel, done: make(chan struct{})}
		a.benchmarkMu.Lock()
		a.benchmarkExecutions[run.ID] = execution
		a.benchmarkMu.Unlock()
		go func() {
			defer func() {
				close(execution.done)
				a.benchmarkMu.Lock()
				if a.benchmarkExecutions[run.ID] == execution {
					delete(a.benchmarkExecutions, run.ID)
				}
				a.benchmarkMu.Unlock()
			}()
			a.runServerBenchmark(ctx, run)
		}()
	}
	writeJSON(w, 202, a.benchmarkDTO(run, false))
}

func (a *apiHandlers) probeExternalBenchmark(ctx context.Context, origin, baseURL, apiKey string) error {
	if origin == "auto" {
		origin = "server"
	}
	if origin != "" && origin != "server" {
		if a.d.CallNode == nil {
			return productError(ErrorBenchmarkAgentRPCUnavailable, "agent RPC unavailable", nil)
		}
		raw, err := a.d.CallNode(ctx, origin, proto.RPCBenchProbe, map[string]string{"url": strings.TrimRight(baseURL, "/"), "api_key": apiKey})
		if err != nil {
			return err
		}
		var body struct {
			Data []loadgen.Model `json:"data"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return err
		}
		if len(body.Data) == 0 {
			return productError(ErrorBenchmarkModelsEmpty, "models endpoint returned no models", nil)
		}
		return nil
	}
	client := &http.Client{Transport: a.outboundTransport(), Timeout: 30 * time.Second}
	defer client.CloseIdleConnections()
	models, err := (loadgen.Client{HTTP: client, BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey}).Models(ctx)
	if err != nil {
		return err
	}
	if len(models) == 0 {
		return productError(ErrorBenchmarkModelsEmpty, "models endpoint returned no models", nil)
	}
	return nil
}
func (a *apiHandlers) cancelBenchmark(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	run, err := a.d.Store.GetBenchmarkRun(id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if run.State == "done" || run.State == "failed" || run.State == "canceled" {
		writeJSON(w, 200, a.benchmarkDTO(run, false))
		return
	}
	if err := a.d.Store.CancelBenchmarkRun(id, "canceled by user"); err != nil {
		writeErr(w, 500, err)
		return
	}
	var stopErr error
	if run.LoadOrigin == "server" {
		stopErr = a.cancelServerBenchmark(id)
	} else {
		stopErr = a.cancelAgentBenchmark(r.Context(), run.LoadOrigin, id)
		// A full desired-state sync excludes the now-terminal run. It is the
		// reconnect/fallback convergence path in addition to the acknowledged
		// cancellation RPC used for an online agent.
		if a.d.NodeSync != nil {
			// The acknowledged RPC determines whether execution stopped. This
			// best-effort full sync preserves desired-state convergence if the
			// session disconnects immediately after acknowledging cancellation.
			_ = a.d.NodeSync.SyncNode(run.LoadOrigin)
		}
	}
	updated, getErr := a.d.Store.GetBenchmarkRun(id)
	if getErr != nil {
		writeErr(w, 500, getErr)
		return
	}
	if stopErr != nil {
		writeProductError(
			w,
			http.StatusServiceUnavailable,
			ErrorBenchmarkCancelUnconfirmed,
			fmt.Sprintf("benchmark %d was marked canceled, but execution stop was not confirmed: %v", id, stopErr),
			map[string]any{"run_id": id, "detail": stopErr.Error()},
		)
		return
	}
	writeJSON(w, 200, a.benchmarkDTO(updated, false))
}

func (a *apiHandlers) cancelServerBenchmark(id uint) error {
	a.benchmarkMu.Lock()
	execution := a.benchmarkExecutions[id]
	a.benchmarkMu.Unlock()
	if execution == nil {
		return nil
	}
	execution.cancel()
	timer := time.NewTimer(benchmarkCancelTimeout)
	defer timer.Stop()
	select {
	case <-execution.done:
		return nil
	case <-timer.C:
		return fmt.Errorf("server execution did not stop within %s", benchmarkCancelTimeout)
	}
}

func (a *apiHandlers) cancelAgentBenchmark(parent context.Context, nodeUUID string, id uint) error {
	if a.d.CallNode == nil {
		return errors.New("agent cancellation RPC unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, benchmarkCancelTimeout)
	defer cancel()
	raw, err := a.d.CallNode(ctx, nodeUUID, proto.RPCBenchCancel, proto.BenchmarkCancelParams{ID: id})
	if err != nil {
		return fmt.Errorf("agent %s: %w", nodeUUID, err)
	}
	var result proto.BenchmarkCancelResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("agent %s returned invalid cancellation acknowledgement: %w", nodeUUID, err)
	}
	if result.ID != id || result.State != "canceled" || result.Running {
		return fmt.Errorf("agent %s returned an invalid cancellation acknowledgement for benchmark %d", nodeUUID, id)
	}
	return nil
}
func (a *apiHandlers) probeBenchmark(w http.ResponseWriter, r *http.Request) {
	if a.d.CallNode == nil {
		writeProductError(w, 503, ErrorBenchmarkAgentRPCUnavailable, "agent RPC unavailable", nil)
		return
	}
	var req struct{ NodeUUID, URL, APIKey string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	normalizedURL, _ := loadgen.NormalizeBaseURL(req.URL)
	raw, err := a.d.CallNode(r.Context(), req.NodeUUID, proto.RPCBenchProbe, map[string]string{"url": normalizedURL, "api_key": req.APIKey})
	if err != nil {
		writeBenchmarkProbeError(w, err, normalizedURL)
		return
	}
	writeJSON(w, 200, json.RawMessage(raw))
}

func (a *apiHandlers) listBenchmarkModels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NodeUUID string `json:"node_uuid"`
		URL      string `json:"url"`
		APIKey   string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		writeProductError(w, 400, ErrorBenchmarkURLRequired, "url required", nil)
		return
	}
	normalizedURL, _ := loadgen.NormalizeBaseURL(req.URL)
	if req.NodeUUID == "auto" {
		req.NodeUUID = "server"
	}
	if req.NodeUUID != "" && req.NodeUUID != "server" {
		if a.d.CallNode == nil {
			writeProductError(w, 503, ErrorBenchmarkAgentRPCUnavailable, "agent RPC unavailable", nil)
			return
		}
		raw, err := a.d.CallNode(r.Context(), req.NodeUUID, proto.RPCBenchProbe, map[string]string{"url": normalizedURL, "api_key": req.APIKey})
		if err != nil {
			writeBenchmarkProbeError(w, err, normalizedURL)
			return
		}
		var body struct {
			Data []loadgen.Model `json:"data"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			writeErr(w, 409, err)
			return
		}
		out := benchmarkModelsDTO{Models: make([]string, 0, len(body.Data)), NormalizedURL: normalizedURL}
		for _, m := range body.Data {
			if m.ID != "" {
				out.Models = append(out.Models, m.ID)
			}
		}
		if len(out.Models) == 0 {
			writeProductError(w, 409, ErrorBenchmarkModelsEmpty, "models endpoint returned no models", nil)
			return
		}
		writeJSON(w, 200, out)
		return
	}
	client := &http.Client{Transport: a.outboundTransport(), Timeout: 30 * time.Second}
	defer client.CloseIdleConnections()
	models, err := (loadgen.Client{HTTP: client, BaseURL: normalizedURL, APIKey: req.APIKey}).Models(r.Context())
	if err != nil {
		writeBenchmarkProbeError(w, err, normalizedURL)
		return
	}
	out := benchmarkModelsDTO{Models: make([]string, 0, len(models)), NormalizedURL: normalizedURL}
	for _, m := range models {
		if m.ID != "" {
			out.Models = append(out.Models, m.ID)
		}
	}
	writeJSON(w, 200, out)
}

func writeBenchmarkProbeError(w http.ResponseWriter, err error, normalizedURL string) {
	if strings.Contains(strings.ToLower(err.Error()), "404") || strings.Contains(strings.ToLower(err.Error()), "not found") {
		writeProductError(w, http.StatusConflict, ErrorBenchmarkEndpointNotFound, "The models endpoint was not found. Confirm that the URL is the root of an OpenAI-compatible endpoint.", map[string]any{"url": normalizedURL, "detail": "the endpoint could not be probed"})
		return
	}
	writeProductError(w, http.StatusConflict, ErrorBenchmarkProbeFailed, "The benchmark endpoint probe failed", map[string]any{"url": normalizedURL, "detail": "the endpoint could not be probed"})
}

func (a *apiHandlers) importBenchmarkDataset(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 5<<20)
	samples, err := dataset.ImportJSONL(r.Body)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if len(samples) == 0 {
		writeProductError(w, 400, ErrorBenchmarkDatasetEmpty, "dataset contains no samples", nil)
		return
	}
	for i := range samples {
		if samples[i].OutputTokens <= 0 {
			samples[i].OutputTokens = dataset.DefaultImportedOutputTokens
		}
	}
	profile := dataset.ProfileSamples(samples)
	writeJSON(w, 200, dataset.Config{Kind: "imported", Samples: samples, Profile: &profile})
}

func (a *apiHandlers) importBenchmarkTokenizer(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(a.d.BenchmarkArtifactDir) == "" {
		writeProductError(w, http.StatusServiceUnavailable, ErrorBenchmarkArtifactDirUnavailable, "benchmark artifact directory unavailable", nil)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(raw) == 0 {
		writeProductError(w, http.StatusBadRequest, ErrorBenchmarkTokenizerEmpty, "tokenizer file is empty", nil)
		return
	}
	ext, ok := tokenizerFileKind(raw)
	if !ok {
		writeProductError(w, http.StatusBadRequest, ErrorBenchmarkTokenizerUnsupported, "file is not a supported tokenizer.json or tiktoken vocabulary", nil)
		return
	}
	if err := os.MkdirAll(a.d.BenchmarkArtifactDir, 0o750); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	name := fmt.Sprintf("tokenizer-%d%s", time.Now().UTC().UnixNano(), ext)
	path := filepath.Join(a.d.BenchmarkArtifactDir, name)
	if err := os.WriteFile(path, raw, 0o640); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"tokenizer_ref": path, "name": name})
}

// tokenizerRefForModel names the repository a benchmark can download this
// model's tokenizer from. Only the two repository APIs can serve one by
// repository id; a plain HTTP directory and a node-local path have no such
// reference, so they leave it empty and the caller falls back to an uploaded
// tokenizer or to none.
func tokenizerRefForModel(model *store.Model) string {
	if model == nil {
		return ""
	}
	scheme := map[string]string{"huggingface": "hf", "modelscope": "ms"}[model.Source]
	if scheme == "" || strings.TrimSpace(model.SourceID) == "" {
		return ""
	}
	return scheme + "://" + model.SourceID
}
