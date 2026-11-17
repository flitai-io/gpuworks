//go:build cloudgpu

package l4

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// l4PullSourceRule is a mirror an L4 site would configure as a pull source
// rule (/api/v1/pull/source-rules): images under Match are tried from
// Endpoints first, and from their own registry afterwards, so a mirror that
// cannot be reached degrades to the direct pull instead of failing the case.
// This is what the removed image_registry_rewrites setting expressed.
type l4PullSourceRule struct {
	Match     string
	Endpoints []string
}

func (r l4PullSourceRule) request() dto.PullSourceRuleRequest {
	allowUpstream := true
	return dto.PullSourceRuleRequest{Match: r.Match, Endpoints: r.Endpoints, AllowUpstream: &allowUpstream}
}

// createPullSourceRules creates the rules, in order, through the operator API
// of an in-process control plane; they apply to every node.
func createPullSourceRules(t *testing.T, api *testenv.APIClient, rules ...l4PullSourceRule) {
	t.Helper()
	for _, rule := range rules {
		body, err := json.Marshal(rule.request())
		if err != nil {
			t.Fatal(err)
		}
		resp, err := api.PostJSON("/api/v1/pull/source-rules", body)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create pull source rule %q status=%d body=%s", rule.Match, resp.StatusCode, data)
		}
	}
}

type cloudDeployment struct {
	envs         []*cloudTestEnv
	deploymentID uint
	servedName   string
	apiKey       string
	instances    []store.Instance
}

// deploySeedInferenceTemplateFake exercises the flat spec catalog deployment
// contract on a real cloud node without downloading a multi-gigabyte model.
func deploySeedInferenceTemplateFake(t *testing.T, env *cloudTestEnv, image, servedName string) *cloudDeployment {
	t.Helper()
	models, err := env.Control.Store().ListInferenceModels()
	if err != nil {
		t.Fatal(err)
	}
	var model *store.InferenceModel
	var selectedSpec *store.InferenceSpecRow
	for index := range models {
		candidate := &models[index]
		if !strings.EqualFold(candidate.ModelID, "qwen/qwen3-1.7b") || candidate.Source != "upstream" {
			continue
		}
		for specIndex := range candidate.Specs {
			if strings.EqualFold(candidate.Specs[specIndex].Engine, "vllm") {
				model, selectedSpec = candidate, &candidate.Specs[specIndex]
				break
			}
		}
	}
	if model == nil || selectedSpec == nil {
		t.Fatal("flat qwen/qwen3-1.7b upstream catalog has no vllm spec")
	}
	var weightSources []struct {
		Provider  string `json:"provider"`
		RepoID    string `json:"repo_id"`
		Revision  string `json:"revision"`
		Subfolder string `json:"subfolder"`
	}
	if err := json.Unmarshal([]byte(selectedSpec.WeightSources), &weightSources); err != nil || len(weightSources) == 0 {
		t.Fatalf("decode selected template weight sources %q: %v", selectedSpec.WeightSources, err)
	}
	weightSource := weightSources[0]
	node, err := env.Control.Store().NodeWithGPUs(env.NodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	weightModel, err := env.Control.Store().UpsertModel(&store.Model{
		Source: weightSource.Provider, SourceID: weightSource.RepoID, Revision: weightSource.Revision, ArtifactSubfolder: weightSource.Subfolder,
		Name: model.DisplayTitle, Arch: "qwen2", WeightBytes: 1 << 30,
		Dtype: "bfloat16", Meta: `{"model_type":"qwen2","num_hidden_layers":28,"hidden_size":1536,"num_attention_heads":12,"num_key_value_heads":2,"max_position_embeddings":32768}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The node's directory of the default local storage, where the files of a
	// row registered without a storage live.
	local, err := env.Control.Store().StorageByID(env.Control.Store().DefaultLocalStorageID())
	if err != nil {
		t.Fatal(err)
	}
	eff, applies, err := env.Control.Store().EffectiveStorageForNode(local, node)
	if err != nil || !applies || strings.TrimSpace(eff.Path) == "" {
		t.Fatalf("default local storage on the cloud node = %+v applies=%v err=%v", eff, applies, err)
	}
	modelsRoot := eff.Path
	fakeModelPath := filepath.Join(modelsRoot, "inference-template-fake")
	if _, err := env.Control.Store().RegisterModelFile(store.FileRef{ModelID: weightModel.ID, NodeID: node.ID}, fakeModelPath, 0); err != nil {
		t.Fatal(err)
	}
	prepareModel := fmt.Sprintf("mkdir -p %s && printf '{}\\n' > %s", shellQuoteForTest(fakeModelPath), shellQuoteForTest(filepath.Join(fakeModelPath, "config.json")))
	if out, err := runCloudCommand(t.Context(), env.Instance, prepareModel); err != nil {
		t.Fatalf("prepare fake model directory: %v (%s)", err, strings.TrimSpace(out))
	}
	// Register the fake image in the catalog; the executable inference
	// template owns the command, and the fixture accepts the normal engine argv.
	if _, err := env.Control.Store().UpsertImage(&store.Image{
		ImageRef: image, Name: "GPUWorks inference template fake", Engine: "vllm", Framework: "cuda",
		Source: "community", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	// The catalog spec is the source of the image used by the deployment. Point
	// this test-only spec at the published fake image rather than reviving the
	// retired profile/image override path.
	if err := env.Control.Store().DB().Model(&store.InferenceSpecRow{}).Where("id = ?", selectedSpec.ID).Update("image", image).Error; err != nil {
		t.Fatal(err)
	}
	selectedSpec.Image = image
	d := createCloudDeployment(t, []*cloudTestEnv{env}, map[string]any{
		"template_id": model.ID, "template_spec_name": selectedSpec.Name, "scenario": selectedSpec.Scenario,
		"model_id":  weightModel.ID,
		"node_uuid": env.NodeUUID, "served_name": servedName,
		"image": image, "quick_params": map[string]any{"max_model_len": 256},
		"nnodes": 1, "placement": map[string]any{"tp": 1},
	}, 1)
	var spec proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(d.instances[0].DesiredStateJSON), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.CommandMode != "engine" || spec.Image != image {
		t.Fatalf("inference template fake spec mode/image = %q/%q, want engine/%q", spec.CommandMode, spec.Image, image)
	}
	for _, required := range []string{"--tensor-parallel-size", "1"} {
		found := false
		for _, arg := range spec.Argv {
			if arg == required {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("inference template profile arg %q missing from argv=%v", required, spec.Argv)
		}
	}
	d.chat(t)
	return d
}

func cloudTestModelSource(t *testing.T, env *cloudTestEnv, source, sourceID, revision string) *store.Model {
	t.Helper()
	name := "Qwen2-1.5B-Instruct"
	weightBytes := int64(3 << 30)
	meta := "{\"model_type\":\"qwen2\",\"num_hidden_layers\":28,\"hidden_size\":1536,\"num_attention_heads\":12,\"num_key_value_heads\":2,\"max_position_embeddings\":32768}"
	if strings.Contains(sourceID, "0.5B") {
		name = "Qwen2-0.5B-Instruct"
		weightBytes = 1 << 30
		meta = "{\"model_type\":\"qwen2\",\"num_hidden_layers\":24,\"hidden_size\":896,\"num_attention_heads\":14,\"num_key_value_heads\":2,\"max_position_embeddings\":32768}"
	}
	model, err := env.Control.Store().UpsertModel(&store.Model{
		Source: source, SourceID: sourceID, Revision: revision,
		Name: name, Arch: "qwen2", WeightBytes: weightBytes, Dtype: "bfloat16", Meta: meta,
	})
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func prepareCloudModelDownload(t *testing.T, env *cloudTestEnv, model *store.Model) {
	t.Helper()
	node, err := env.Control.Store().NodeByUUID(env.NodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	if mf, err := env.Control.Store().ModelFileByModelNode(model.ID, node.ID); err == nil && (mf.State == proto.MFPresent || mf.State == proto.MFRegistered) {
		return
	}
	path := model.Source + "/" + strings.ReplaceAll(model.SourceID, "/", "_") + "-" + model.Revision
	if _, err := env.Control.Store().PrepareModelDownload(store.FileRef{ModelID: model.ID, NodeID: node.ID}, path); err != nil {
		t.Fatal(err)
	}
}

func verifyCloudPublicShard(t *testing.T, env *cloudTestEnv, source, repo, revision string) {
	t.Helper()
	const shard = "model-00001-of-00003.safetensors"
	const shardBytes = 8 << 20
	url := modelrepo.ResolveURL(source, repo, revision, shard, "")
	cmd := fmt.Sprintf(`set -eu
target="$(mktemp /tmp/gpuworks-public-shard.XXXXXX)"
trap 'rm -f "$target"' EXIT
curl -fL --retry 2 --retry-all-errors --connect-timeout 15 --max-time 300 -o "$target" %s
bytes="$(wc -c < "$target")"
test "$bytes" -eq %d
printf 'downloaded_bytes=%%s\n' "$bytes"`, shellQuoteForTest(url), shardBytes)
	out, err := runCloudCommand(t.Context(), env.Instance, cmd)
	if err != nil {
		t.Fatalf("download public %s shard from %s: %v (%s)", source, repo, err, strings.TrimSpace(out))
	}
	if !strings.Contains(out, fmt.Sprintf("downloaded_bytes=%d", shardBytes)) {
		t.Fatalf("public %s shard download returned unexpected evidence: %q", source, strings.TrimSpace(out))
	}
}

func createCloudDeployment(t *testing.T, envs []*cloudTestEnv, body map[string]any, expectedInstances int) *cloudDeployment {
	return createCloudDeploymentWithTimeout(t, envs, body, expectedInstances, 30*time.Minute)
}

func createCloudDeploymentWithTimeout(t *testing.T, envs []*cloudTestEnv, body map[string]any, expectedInstances int, startupTimeout time.Duration) *cloudDeployment {
	t.Helper()
	if len(envs) == 0 {
		t.Fatal("cloud deployment requires at least one environment")
	}
	servedName := strings.TrimSpace(fmt.Sprint(body["served_name"]))
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := envs[0].Control.API(t).PostJSON("/api/v1/deployments", payload)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("create cloud deployment status=%d body=%s", resp.StatusCode, data)
	}
	var created struct {
		ID uint `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	instances := waitCloudDeploymentGroup(t, envs, created.ID, expectedInstances, startupTimeout)
	key, err := envs[0].Control.Store().CreateAPIKey("l4-cloud", 0)
	if err != nil {
		t.Fatal(err)
	}
	d := &cloudDeployment{envs: envs, deploymentID: created.ID, servedName: servedName, apiKey: key, instances: instances}
	t.Cleanup(func() { d.stop(t) })
	return d
}

func waitCloudDeploymentGroup(t *testing.T, envs []*cloudTestEnv, deploymentID uint, expected int, timeout time.Duration) []store.Instance {
	t.Helper()
	control := envs[0].Control
	deadline := time.Now().Add(timeout)
	nextLog := time.Now()
	loggedImageRefs := false
	var allFailedSince time.Time
	for time.Now().Before(deadline) {
		rows, _ := control.Store().InstancesForDeployment(deploymentID)
		dep, _ := control.Store().GetDeployment(deploymentID)
		modelState := "model=unknown"
		if dep != nil {
			if mf, err := control.Store().ModelFileByModelNode(dep.ModelID, dep.NodeID); err == nil {
				modelState = fmt.Sprintf("model state=%s downloaded=%d/%d transferred=%d rate=%d path=%q message=%q", mf.State, mf.Downloaded, mf.SizeBytes, mf.TransferredBytes, mf.RateBPS, mf.Path, mf.Message)
			} else {
				modelState = "model file error=" + err.Error()
			}
		}
		active := make([]store.Instance, 0, expected)
		for _, row := range rows {
			switch row.State {
			case proto.InstFailed, proto.InstStopped, "pending-removal":
			default:
				active = append(active, row)
			}
		}
		if len(active) == expected {
			allFailedSince = time.Time{}
			allRunning := true
			for _, row := range active {
				if row.State != proto.InstRunning || row.ContainerID == "" {
					allRunning = false
				}
			}
			if allRunning {
				return active
			}
		}
		if len(rows) > 0 && len(active) == 0 {
			allFailed := true
			for _, row := range rows {
				if row.State != proto.InstFailed && row.State != proto.InstStopped && row.State != "pending-removal" {
					allFailed = false
					break
				}
			}
			if !allFailed {
				allFailedSince = time.Time{}
			} else if allFailedSince.IsZero() {
				allFailedSince = time.Now()
			} else if time.Since(allFailedSince) >= cloudRetryGrace {
				diagnosticInstance := cloudDiagnosticInstanceForNode(envs[0], rows)
				out, sshErr := runCloudCommand(t.Context(), envs[0].Instance, cloudStartupCommand(diagnosticInstance))
				logs := cloudDeploymentLogDiagnostics(t, envs[0], rows)
				t.Fatalf("cloud deployment %d has no viable attempts: %+v\n%s\nssh diagnostics (err=%v):\n%s", deploymentID, rows, logs, sshErr, strings.TrimSpace(out))
			}
		} else {
			allFailedSince = time.Time{}
		}
		if !loggedImageRefs && len(active) > 0 {
			var spec proto.InstanceDesiredState
			if json.Unmarshal([]byte(active[0].DesiredStateJSON), &spec) == nil && len(spec.ImagePullRefs) > 0 {
				t.Logf("cloud deployment %d image=%s pull_refs=%v", deploymentID, spec.Image, spec.ImagePullRefs)
				loggedImageRefs = true
			}
		}
		if time.Now().After(nextLog) {
			var states []string
			for _, row := range rows {
				states = append(states, fmt.Sprintf("rank=%d state=%s phase=%s progress=%.1f%% bytes=%d/%d rate=%d message=%q", row.NodeRank, row.State, row.Phase, row.Progress*100, row.BytesDone, row.BytesTotal, row.RateBPS, row.Message))
			}
			t.Logf("cloud deployment %d: active=%d/%d rows=%d [%s] %s", deploymentID, len(active), expected, len(rows), strings.Join(states, "; "), modelState)
			nextLog = time.Now().Add(15 * time.Second)
		}
		time.Sleep(2 * time.Second)
	}
	rows, _ := control.Store().InstancesForDeployment(deploymentID)
	var diagnostics []string
	for _, env := range envs {
		out, err := runCloudCommand(t.Context(), env.Instance, cloudStartupCommand(cloudDiagnosticInstanceForNode(env, rows)))
		diagnostics = append(diagnostics, fmt.Sprintf("node=%s err=%v\n%s", env.NodeUUID, err, strings.TrimSpace(out)))
	}
	logs := cloudDeploymentLogDiagnostics(t, envs[0], rows)
	t.Fatalf("cloud deployment %d did not reach %d running instances within %s: %+v\n%s\n%s", deploymentID, expected, timeout, rows, logs, strings.Join(diagnostics, "\n---\n"))
	return nil
}

func cloudDiagnosticInstanceForNode(env *cloudTestEnv, rows []store.Instance) *store.Instance {
	if env == nil || env.Control == nil {
		return nil
	}
	node, err := env.Control.Store().NodeByUUID(env.NodeUUID)
	if err != nil {
		return nil
	}
	// InstancesForDeployment is newest-first. Prefer the current attempt, or
	// the final retained failed attempt after the retry budget is exhausted.
	for index := range rows {
		if rows[index].NodeID == node.ID {
			return &rows[index]
		}
	}
	return nil
}

func (d *cloudDeployment) chat(t *testing.T) []byte {
	t.Helper()
	// enable_thinking: a model that thinks first (Qwen3) would spend the 16
	// tokens on its reasoning and return no content; a template without the
	// switch ignores it.
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"reply with the word ready"}],"max_tokens":16,"chat_template_kwargs":{"enable_thinking":false}}`, d.servedName)
	req, _ := http.NewRequest(http.MethodPost, d.envs[0].Control.URL()+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+d.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cloud inference status=%d body=%s", resp.StatusCode, data)
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &decoded) != nil || len(decoded.Choices) == 0 || strings.TrimSpace(decoded.Choices[0].Message.Content) == "" {
		t.Fatalf("cloud inference returned no content: %s", data)
	}
	return data
}

func (d *cloudDeployment) models(t *testing.T) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, d.envs[0].Control.URL()+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+d.apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), d.servedName) {
		t.Fatalf("cloud models status=%d body=%s", resp.StatusCode, data)
	}
	return data
}

func (d *cloudDeployment) stop(t *testing.T) {
	t.Helper()
	if d == nil || d.deploymentID == 0 {
		return
	}
	resp, err := d.envs[0].Control.API(t).PostJSON(fmt.Sprintf("/api/v1/deployments/%d/stop", d.deploymentID), nil)
	if err != nil {
		t.Errorf("stop cloud deployment: %v", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("stop cloud deployment status=%d", resp.StatusCode)
		return
	}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		rows, _ := d.envs[0].Control.Store().InstancesForDeployment(d.deploymentID)
		if len(rows) == 0 {
			d.deploymentID = 0
			return
		}
		time.Sleep(time.Second)
	}
	t.Errorf("deployment %d did not stop", d.deploymentID)
}
