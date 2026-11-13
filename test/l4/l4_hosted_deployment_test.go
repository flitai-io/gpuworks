//go:build cloudgpu && (compshare || aliyun)

package l4

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

type hostedDeployment struct {
	harness    *hostedControlPlane
	id         uint
	servedName string
	instances  []dto.Instance
	stopMu     sync.Mutex
	stopped    bool
}

const hostedModelRegistrationAttempts = 4 // initial request plus three retries

// registerHostedModel retries only the model-registration business operation.
// The hosted API client itself intentionally remains single-shot for POSTs;
// dropping the forward here lets the next business attempt establish a fresh
// SSH tunnel and login after a transient network failure.
func registerHostedModel(ctx context.Context, api *hostedAPIClient, input map[string]any, output *dto.Model) error {
	var lastErr error
	for attempt := 0; attempt < hostedModelRegistrationAttempts; attempt++ {
		if err := api.PostJSON(ctx, "/api/v1/models", input, output); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if attempt+1 == hostedModelRegistrationAttempts {
			break
		}
		api.DropForward()
		delay := time.Duration(attempt+1) * time.Second
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("register hosted model failed after %d attempts: %w", hostedModelRegistrationAttempts, lastErr)
}

func hostedPrepareRealModel(t *testing.T, harness *hostedControlPlane) dto.Model {
	t.Helper()
	backend := harness.backend
	var model dto.Model
	if err := registerHostedModel(t.Context(), harness.API, map[string]any{
		"source": "modelscope", "source_id": backend.Model.SourceID,
		"revision": "master", "name": backend.Model.Name,
	}, &model); err != nil {
		t.Fatalf("register hosted model: %v", err)
	}
	// Distributed acceptance is intentionally hermetic with respect to model
	// repositories: each provider's public-download case owns the ModelScope
	// contract, so a missing preloaded model is a fixture error.
	preloaded := backend.PreloadedModelPath(t)
	for vmIndex, vm := range harness.VMs {
		if out, err := runCloudCommand(t.Context(), vm.Instance, "test -d "+shellQuoteForTest(preloaded)); err != nil {
			t.Fatalf("preloaded model path on VM %d is unavailable: %v (%s)", vmIndex, err, strings.TrimSpace(out))
		}
	}
	if len(harness.Nodes) == 0 {
		t.Fatal("preloaded model has no online nodes to register")
	}
	// One physical VM may host two logical agents. Register the shared path for
	// every logical node, not merely once per VM.
	for rank, node := range harness.Nodes {
		var response map[string]any
		if err := harness.API.PostJSON(t.Context(), fmt.Sprintf("/api/v1/models/%d/register-path", model.ID), map[string]string{"node_uuid": node.UUID, "path": preloaded}, &response); err != nil {
			t.Fatalf("register preloaded model path on rank %d: %v", rank, err)
		}
	}
	return model
}

func hostedWaitModelPresent(t *testing.T, harness *hostedControlPlane, modelID uint, expected int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	nextLog := time.Now()
	var last dto.Model
	var lastErr error
	for time.Now().Before(deadline) {
		lastErr = harness.API.GetJSON(t.Context(), fmt.Sprintf("/api/v1/models/%d", modelID), &last)
		if lastErr == nil {
			present := 0
			for _, node := range last.Nodes {
				if node.State == proto.MFPresent || node.State == proto.MFRegistered {
					present++
				}
			}
			if present == expected {
				return
			}
			if time.Now().After(nextLog) {
				t.Logf("hosted model %d download: present=%d/%d nodes=%+v", modelID, present, expected, last.Nodes)
				nextLog = time.Now().Add(15 * time.Second)
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("hosted model %d did not become present on %d nodes within %s: model=%+v err=%v\n%s", modelID, expected, timeout, last, lastErr, harness.Diagnostics(t.Context()))
}

func hostedRequireDistributedPreflight(t *testing.T, harness *hostedControlPlane, port int) {
	t.Helper()
	var result struct {
		OK    bool `json:"ok"`
		Pairs []struct {
			SourceUUID      string  `json:"source_uuid"`
			TargetUUID      string  `json:"target_uuid"`
			Reachable       bool    `json:"reachable"`
			BandwidthTested bool    `json:"bandwidth_tested"`
			Gbps            float64 `json:"gbps"`
			Error           string  `json:"error"`
			BandwidthError  string  `json:"bandwidth_error"`
		} `json:"pairs"`
	}
	if err := harness.API.PostJSON(t.Context(), "/api/v1/deployments/preflight", map[string]any{
		"node_uuids": []string{harness.Nodes[0].UUID, harness.Nodes[1].UUID},
		"port":       port, "master_addr": harness.VMs[0].Instance.PrivateIP,
		"bandwidth": true, "seconds": 2,
	}, &result); err != nil {
		t.Fatalf("hosted distributed preflight: %v", err)
	}
	if !result.OK || len(result.Pairs) != 2 {
		t.Fatalf("hosted distributed preflight result=%+v", result)
	}
	for _, pair := range result.Pairs {
		if !pair.Reachable || !pair.BandwidthTested || pair.Gbps <= 0 || pair.Error != "" || pair.BandwidthError != "" {
			t.Fatalf("hosted distributed preflight pair failed: %+v", pair)
		}
		t.Logf("hosted private network %s -> %s: %.3f Gbps", pair.SourceUUID, pair.TargetUUID, pair.Gbps)
	}
}

func createHostedDeployment(t *testing.T, harness *hostedControlPlane, body map[string]any, expected int, timeout time.Duration) *hostedDeployment {
	t.Helper()
	servedName := strings.TrimSpace(fmt.Sprint(body["served_name"]))
	var created dto.Deployment
	if err := harness.API.PostJSON(t.Context(), "/api/v1/deployments", body, &created); err != nil {
		t.Fatalf("create hosted deployment: %v", err)
	}
	if created.ID == 0 {
		t.Fatal("create hosted deployment returned an empty ID")
	}
	d := &hostedDeployment{harness: harness, id: created.ID, servedName: servedName}
	t.Cleanup(func() { d.stop(t) })
	d.instances = hostedWaitDeploymentRunning(t, harness, created.ID, expected, timeout)
	return d
}

func hostedWaitDeploymentRunning(t *testing.T, harness *hostedControlPlane, deploymentID uint, expected int, timeout time.Duration) []dto.Instance {
	t.Helper()
	deadline := time.Now().Add(timeout)
	nextLog := time.Now()
	var last dto.Deployment
	var lastErr error
	var failedSince time.Time
	seenInstances := make(map[string]struct{})
	// A failed attempt that the platform replaces leaves no trace once the case
	// passes, so its engine log is written to the case log the moment the row
	// shows up: a transient start failure is otherwise undiagnosable.
	reportedFailures := make(map[string]struct{})
	for time.Now().Before(deadline) {
		lastErr = harness.API.GetJSON(t.Context(), fmt.Sprintf("/api/v1/deployments/%d", deploymentID), &last)
		if lastErr == nil {
			running := 0
			for _, instance := range last.Instances {
				if instance.UUID != "" {
					seenInstances[instance.UUID] = struct{}{}
				}
				if instance.State == proto.InstRunning && instance.ContainerID != "" {
					running++
				}
			}
			for _, instance := range last.Instances {
				if instance.State != proto.InstFailed || instance.UUID == "" {
					continue
				}
				if _, done := reportedFailures[instance.UUID]; done {
					continue
				}
				reportedFailures[instance.UUID] = struct{}{}
				t.Logf("hosted deployment %d: attempt %s (role=%q node=%s) failed; engine log:\n%s", deploymentID, instance.UUID, instance.Role, instance.NodeUUID, hostedDeploymentLogDiagnostics(t.Context(), harness, map[string]struct{}{instance.UUID: {}}))
			}
			// A failed first attempt stays in the deployment's instance list next
			// to its replacement, so the rows to count are the live ones.
			if active := hostedActiveInstances(last.Instances); running == expected && len(active) == expected {
				return active
			}
			if hostedDeploymentExhausted(last) {
				if failedSince.IsZero() {
					failedSince = time.Now()
				} else if time.Since(failedSince) >= cloudRetryGrace {
					t.Fatalf("hosted deployment %d exhausted viable attempts: runtime=%s instances=%+v\n%s\n%s", deploymentID, last.RuntimeState, last.Instances, hostedDeploymentLogDiagnostics(t.Context(), harness, seenInstances), harness.Diagnostics(t.Context()))
				}
			} else {
				failedSince = time.Time{}
			}
			if time.Now().After(nextLog) {
				t.Logf("hosted deployment %d: runtime=%s running=%d/%d instances=%s", deploymentID, last.RuntimeState, running, expected, hostedInstanceProgress(last.Instances))
				nextLog = time.Now().Add(15 * time.Second)
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("hosted deployment %d did not reach %d running ranks within %s: deployment=%+v err=%v\n%s\n%s", deploymentID, expected, timeout, last, lastErr, hostedDeploymentLogDiagnostics(t.Context(), harness, seenInstances), harness.Diagnostics(t.Context()))
	return nil
}

// hostedActiveInstances drops the instances of failed or stopped attempts: the
// platform retries an engine that dies at start and keeps the dead attempt's row
// beside the replacement (a vLLM start can fail transiently on a cold VM, and
// the retry then succeeds, leaving more rows than ranks).
func hostedActiveInstances(instances []dto.Instance) []dto.Instance {
	active := make([]dto.Instance, 0, len(instances))
	for _, instance := range instances {
		switch instance.State {
		case proto.InstFailed, proto.InstStopped, proto.InstRemoving, "pending-removal":
		default:
			active = append(active, instance)
		}
	}
	return active
}

// hostedDeploymentLogDiagnostics reads the server-side logstore before test
// cleanup removes failed containers. Distributed retries replace the current
// rows, so keep every UUID observed while polling instead of inspecting only
// the terminal generation.
func hostedDeploymentLogDiagnostics(ctx context.Context, harness *hostedControlPlane, instances map[string]struct{}) string {
	if harness == nil || harness.API == nil || len(instances) == 0 {
		return "hosted container logs: no observed instance UUIDs"
	}
	ids := make([]string, 0, len(instances))
	for id := range instances {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var result strings.Builder
	result.WriteString("hosted container logs:")
	for _, id := range ids {
		var page apiwire.LogsPage
		requestPath := "/api/v1/logs/search?instance=" + url.QueryEscape(id) + "&limit=400"
		if err := harness.API.GetJSON(ctx, requestPath, &page); err != nil {
			fmt.Fprintf(&result, "\n--- instance %s log query failed: %v", id, err)
			continue
		}
		entries := page.Items
		fmt.Fprintf(&result, "\n--- instance %s (%d lines)", id, len(entries))
		// The search endpoint returns newest-first. Diagnostics read naturally in
		// chronological order and should end at the actual fatal line.
		for index := len(entries) - 1; index >= 0; index-- {
			line := strings.TrimSpace(entries[index].Line)
			if line != "" {
				result.WriteByte('\n')
				result.WriteString(line)
			}
		}
	}
	return result.String()
}

func hostedInstanceProgress(instances []dto.Instance) string {
	if len(instances) == 0 {
		return "[]"
	}
	result := make([]string, 0, len(instances))
	for _, instance := range instances {
		message := strings.TrimSpace(instance.Message)
		if index := strings.LastIndexByte(message, '\n'); index >= 0 {
			message = strings.TrimSpace(message[index+1:])
		}
		if len(message) > 120 {
			message = message[len(message)-120:]
		}
		result = append(result, fmt.Sprintf("rank%d=%s/%s %.0f%% %q", instance.NodeRank, instance.State, instance.Phase, instance.Progress*100, message))
	}
	return "[" + strings.Join(result, "; ") + "]"
}

// hostedDeploymentExhausted reports whether a deployment spent its retry
// budget: a unit's spent attempts are its failed leader rows (ADR 0029), and
// nothing viable is left.
func hostedDeploymentExhausted(deployment dto.Deployment) bool {
	if deployment.RuntimeState == proto.InstFailed {
		return true
	}
	if deployment.DesiredState != "active" {
		return false
	}
	failedLeaders := 0
	for _, instance := range deployment.Instances {
		switch instance.State {
		case proto.InstFailed:
			if !instance.Headless {
				failedLeaders++
			}
		case proto.InstStopped, proto.InstRemoving:
			// This generation cannot make additional progress.
		default:
			return false
		}
	}
	return failedLeaders >= 3
}

func (d *hostedDeployment) models(t *testing.T) []byte {
	t.Helper()
	data, status, err := d.gatewayJSON(t.Context(), http.MethodGet, "/v1/models", nil)
	if err != nil || status != http.StatusOK || !bytes.Contains(data, []byte(d.servedName)) {
		t.Fatalf("hosted models status=%d err=%v body=%s", status, err, data)
	}
	return data
}

func (d *hostedDeployment) chat(t *testing.T) []byte {
	t.Helper()
	body := map[string]any{
		"model": d.servedName, "messages": []map[string]string{{"role": "user", "content": "reply with the word ready"}}, "max_tokens": 16,
		// A model that thinks first (Qwen3) would spend the 16 tokens on its
		// reasoning and return no content; a template without the switch ignores it.
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	}
	data, status, err := d.gatewayJSON(t.Context(), http.MethodPost, "/v1/chat/completions", body)
	if err != nil || status != http.StatusOK {
		t.Fatalf("hosted chat status=%d err=%v body=%s", status, err, data)
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &decoded) != nil || len(decoded.Choices) == 0 || strings.TrimSpace(decoded.Choices[0].Message.Content) == "" {
		t.Fatalf("hosted chat returned no content: %s", data)
	}
	return data
}

func (d *hostedDeployment) gatewayJSON(ctx context.Context, method, path string, input any) ([]byte, int, error) {
	var key struct {
		Key string `json:"key"`
	}
	if err := d.harness.API.PostJSON(ctx, "/api/v1/apikeys", map[string]string{"name": "hosted-l4"}, &key); err != nil || key.Key == "" {
		return nil, 0, fmt.Errorf("create hosted API key: %w", err)
	}
	baseURL, _, err := d.harness.API.session(ctx)
	if err != nil {
		return nil, 0, err
	}
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return nil, 0, err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+path, body)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+key.Key)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := d.harness.API.client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	return data, response.StatusCode, readErr
}

func (d *hostedDeployment) stop(t *testing.T) {
	t.Helper()
	if d == nil || d.id == 0 || d.harness == nil || d.harness.API == nil {
		return
	}
	d.stopMu.Lock()
	defer d.stopMu.Unlock()
	if d.stopped {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var response map[string]string
	if err := d.harness.API.PostJSON(ctx, fmt.Sprintf("/api/v1/deployments/%d/stop", d.id), nil, &response); err != nil {
		t.Errorf("stop hosted deployment %d: %v", d.id, err)
		return
	}
	for ctx.Err() == nil {
		var deployment dto.Deployment
		if err := d.harness.API.GetJSON(ctx, fmt.Sprintf("/api/v1/deployments/%d", d.id), &deployment); err == nil && deployment.RuntimeState == proto.InstStopped {
			d.stopped = true
			return
		}
		time.Sleep(time.Second)
	}
	t.Errorf("hosted deployment %d did not stop within 3m", d.id)
}
