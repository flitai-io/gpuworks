package deploy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// The metrics port is decided when a deployment is created and lives in its
// template snapshot and in its instances' desired state; editing the template
// afterwards changes the deployments created next and nothing else. Over one
// real server and in-process agent: a template that declares the port is
// deployed, re-imported without the declaration (a new deployment gets no
// port, the running one keeps its port and its saved template still declares
// it), re-imported with it again (the next deployment gets a port distinct
// from the others'), and the YAML the editor shows for the template, saved
// back unchanged, keeps the declaration.
func TestTemplateEditsMoveOnlyTheNextDeploymentsMetricsPort(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("metrics-port-edit-node",
		testenv.WithGPUs(4, testenv.GPUSpec{Model: "NVIDIA A100", Vendor: "nvidia", ComputeCapability: "8.0", VRAMMB: 81920}),
		testenv.WithDriverName("docker"),
	)
	env.WaitNodeOnline(t, node.Name)
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	const modelID = "gpuworks/metrics-port-edit"
	const revision = "0123456789abcdef0123456789abcdef01234567"
	weight, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: modelID, Revision: revision, Name: modelID, Arch: "FakeForCausalLM", WeightBytes: 1 << 20, Meta: `{"model_type":"fake","num_hidden_layers":1,"hidden_size":64,"num_attention_heads":2,"num_key_value_heads":1,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: weight.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "metrics-port-edit")}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)

	template := func(args []string, metrics *inferenceapi.Metrics) inferencecatalog.InferenceModel {
		return inferencecatalog.InferenceModel{ModelID: modelID, Source: "user", Specs: []inferencecatalog.InferenceSpec{{
			Name: "own-metrics", Engine: "callable", Image: "example/launcher:test",
			Weights:     inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: modelID, Revision: revision}}},
			WeightBytes: 1 << 20, GPUs: inferencecatalog.GPUSelector{Vendor: "nvidia", CC: ">=8.0"},
			Entrypoint: []string{"/opt/launcher"}, Args: args,
			API: &inferenceapi.Contract{Operations: []inferenceapi.Operation{{Type: "openai.chat", Endpoint: "/v1/chat/completions", Mode: "stream"}}, Metrics: metrics},
		}}}
	}
	post := func(path, contentType string, body []byte) (int, []byte) {
		t.Helper()
		request, _ := http.NewRequest(http.MethodPost, env.URL()+path, bytes.NewReader(body))
		request.Header.Set("Content-Type", contentType)
		response, err := api.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		return response.StatusCode, raw
	}
	importTemplate := func(model inferencecatalog.InferenceModel) {
		t.Helper()
		packageFS, err := inferencecatalog.BuildPackageFS("2026-09-01T00:00:00Z", inferencecatalog.FlatCatalog{Models: []inferencecatalog.InferenceModel{model}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var archive bytes.Buffer
		if err := inferencecatalog.PackPackage(&archive, packageFS); err != nil {
			t.Fatal(err)
		}
		if status, raw := post("/api/v1/inference-templates/import?overwrite=true", "application/gzip", archive.Bytes()); status != http.StatusOK {
			t.Fatalf("import status/body=%d %s", status, raw)
		}
	}
	deploy := func(name string) store.Instance {
		t.Helper()
		template, err := env.Store().InferenceModelByModelID(modelID)
		if err != nil {
			t.Fatal(err)
		}
		request, _ := json.Marshal(map[string]any{"template_id": template.ID, "template_spec_name": "own-metrics", "node_uuid": nodes[0].UUID, "served_name": name})
		status, raw := post("/api/v1/deployments", "application/json", request)
		if status != http.StatusCreated {
			t.Fatalf("create %s status/body=%d %s", name, status, raw)
		}
		var created struct {
			ID uint `json:"id"`
		}
		if err := json.Unmarshal(raw, &created); err != nil {
			t.Fatal(err)
		}
		var instance store.Instance
		env.Eventually(t, 30*time.Second, func() bool {
			instances, _ := env.Store().InstancesForDeployment(created.ID)
			if len(instances) != 1 || instances[0].DesiredStateJSON == "" || instances[0].State != proto.InstRunning {
				return false
			}
			instance = instances[0]
			return true
		})
		return instance
	}
	desired := func(instance store.Instance) (proto.InstanceDesiredState, map[string]json.RawMessage) {
		t.Helper()
		var spec proto.InstanceDesiredState
		var raw map[string]json.RawMessage
		if json.Unmarshal([]byte(instance.DesiredStateJSON), &spec) != nil || json.Unmarshal([]byte(instance.DesiredStateJSON), &raw) != nil {
			t.Fatalf("desired state %q", instance.DesiredStateJSON)
		}
		return spec, raw
	}
	separate := &inferenceapi.Metrics{Port: inferenceapi.MetricsPortSeparate}

	// 1. The template declares the port.
	importTemplate(template([]string{"--port", "${GPUWORKS_API_PORT}", "--metrics-port", "${GPUWORKS_METRICS_PORT}"}, separate))
	first := deploy("declared-first")
	firstSpec, _ := desired(first)
	if firstSpec.MetricsPort != firstSpec.APIPort+1 || !slices.Equal(firstSpec.Ports, []int{firstSpec.APIPort, firstSpec.MetricsPort}) {
		t.Fatalf("first: api %d metrics %d ports %v", firstSpec.APIPort, firstSpec.MetricsPort, firstSpec.Ports)
	}

	// 2. The template is re-imported without the declaration.
	importTemplate(template([]string{"--port", "${GPUWORKS_API_PORT}"}, nil))
	second := deploy("undeclared-second")
	secondSpec, secondRaw := desired(second)
	if _, present := secondRaw["metrics_port"]; present || secondSpec.MetricsPort != 0 || !slices.Equal(secondSpec.Ports, []int{secondSpec.APIPort}) {
		t.Fatalf("second: a template without the declaration reserved a metrics port: %s", second.DesiredStateJSON)
	}
	if flagValue(secondSpec.Argv, "--metrics-port") != "" {
		t.Fatalf("second argv=%v", secondSpec.Argv)
	}
	// The running deployment is untouched: same desired state, same port, still
	// running, and the template saved back from it still declares the port.
	current, err := env.Store().InstanceByUUID(first.UUID)
	if err != nil || current.State != proto.InstRunning || current.DesiredStateJSON != first.DesiredStateJSON || current.Ports != first.Ports {
		t.Fatalf("the template edit touched the running instance: %+v err=%v", current, err)
	}
	status, raw := post("/api/v1/deployments/"+strconv.FormatUint(uint64(first.DeploymentID), 10)+"/catalog-template", "application/json", []byte(`{}`))
	if status != http.StatusOK || !strings.Contains(string(raw), "port: separate") || !strings.Contains(string(raw), "${GPUWORKS_METRICS_PORT}") {
		t.Fatalf("the saved template of the running deployment lost the port: status=%d %s", status, raw)
	}

	// 3. The template declares it again, with the variable under another flag.
	importTemplate(template([]string{"--port", "${GPUWORKS_API_PORT}", "--prom-port", "${METRICS_PORT}"}, separate))
	third := deploy("declared-third")
	thirdSpec, _ := desired(third)
	if thirdSpec.MetricsPort <= 0 || flagValue(thirdSpec.Argv, "--prom-port") != strconv.Itoa(thirdSpec.MetricsPort) {
		t.Fatalf("third: metrics port %d argv=%v", thirdSpec.MetricsPort, thirdSpec.Argv)
	}
	seen := map[int]string{}
	for name, spec := range map[string]proto.InstanceDesiredState{"first": firstSpec, "second": secondSpec, "third": thirdSpec} {
		for _, port := range spec.Ports {
			if owner, taken := seen[port]; taken {
				t.Fatalf("port %d is held by both %s and %s", port, owner, name)
			}
			seen[port] = name
		}
	}
	if len(seen) != 5 {
		t.Fatalf("held ports = %v, want 2+1+2", seen)
	}

	// 4. The YAML the editor shows, saved back unchanged, keeps the port, and a
	// deployment of it reserves one.
	request, _ := http.NewRequest(http.MethodGet, env.URL()+"/api/v1/inference-templates/yaml?model_id="+modelID, nil)
	response, err := api.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	yaml, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(yaml), "port: separate") || !strings.Contains(string(yaml), "${METRICS_PORT}") {
		t.Fatalf("editor yaml status=%d:\n%s", response.StatusCode, yaml)
	}
	if status, raw := post("/api/v1/inference-templates/import?overwrite=true", "application/yaml", yaml); status != http.StatusOK {
		t.Fatalf("re-import of the editor yaml status/body=%d %s", status, raw)
	}
	fourth := deploy("declared-fourth")
	if fourthSpec, _ := desired(fourth); fourthSpec.MetricsPort <= 0 || slices.Contains(fourthSpec.Ports, thirdSpec.MetricsPort) {
		t.Fatalf("fourth: metrics port %d ports %v after the round trip", fourthSpec.MetricsPort, fourthSpec.Ports)
	}
}
