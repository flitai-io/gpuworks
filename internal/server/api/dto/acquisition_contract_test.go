package dto

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// A deploy request names at most a storage and a weight provider; where a
// node fetches files from is not the request's to say, so the earlier source
// fields do not survive a decode.
func TestDeployRequestModelSourceWireContract(t *testing.T) {
	var req DeployRequest
	if err := json.Unmarshal([]byte(`{"model_source":{"storage_id":7,"provider":"huggingface"}}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.ModelSource == nil || *req.ModelSource != (ModelSource{StorageID: 7, Provider: "huggingface"}) {
		t.Fatalf("decoded=%+v", req.ModelSource)
	}
	b, err := json.Marshal(req.ModelSource)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"storage_id":7,"provider":"huggingface"}` {
		t.Fatalf("wire=%s", b)
	}
	// Empty fields stay off the wire.
	b, err = json.Marshal(ModelSource{})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{}` {
		t.Fatalf("empty wire=%s", b)
	}
	// Removed keys are ignored, not carried: the struct has no field for them.
	var legacy DeployRequest
	if err := json.Unmarshal([]byte(`{"model_source":{"kind":"public","provider":"modelscope","repo_id":"Qwen/x","revision":"abc","network":{"mode":"proxy"}},"image_pull":{"image":"quay.io/x/y:v1","kind":"public"}}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.ModelSource == nil || *legacy.ModelSource != (ModelSource{Provider: "modelscope"}) {
		t.Fatalf("legacy decode kept more than storage_id/provider: %+v", legacy.ModelSource)
	}
	out, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"image_pull", "repo_id", "revision", "network", `"kind"`} {
		if strings.Contains(string(out), key) {
			t.Fatalf("removed key %s re-emitted: %s", key, out)
		}
	}
}

func TestEvaluateAcquisitionWireContract(t *testing.T) {
	cached := false
	r := EvaluateResponse{OK: true, Image: "vllm:test", Acquisition: &AcquisitionResult{
		Providers: []AcquisitionCandidate{{Key: "public:hf:Qwen/x:main", Label: "huggingface", Reason: "configured model provider", Recommended: true}},
		Image: AcquisitionImage{
			Image:   "vllm:test",
			Cached:  &cached,
			Options: []ImageOption{{Image: "vllm:test", Reason: "catalog_match", Compatible: true}},
		},
		Nodes: []AcquisitionNode{{
			NodeUUID: "n1", NodeName: "gpu1", ImageCached: &cached,
			ImagePlan: "pull", ImagePullRefs: []string{"mirror.example/vllm:test", "vllm:test"}, ImagePullRanked: true,
			WeightsPlan: "download", WeightsProvider: "huggingface",
		}},
	}}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(b, &object); err != nil {
		t.Fatal(err)
	}
	acquisition := object["acquisition"].(map[string]any)
	wantKeys := []string{"image", "nodes", "providers"}
	var gotKeys []string
	for k := range acquisition {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(gotKeys)
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("acquisition keys=%v want %v", gotKeys, wantKeys)
	}
	for _, k := range []string{"local", "peers", "networks"} {
		if _, ok := acquisition[k]; ok {
			t.Fatalf("removed acquisition key %q present: %s", k, b)
		}
	}
	image := acquisition["image"].(map[string]any)
	if _, ok := image["peers"]; ok {
		t.Fatalf("removed image.peers present: %s", b)
	}
	labelled, err := json.Marshal(ImageOption{Image: "sglang:test", Reason: "catalog_match", Compatible: true, Engine: "sglang", EngineVersion: "0.5.21", SelectsEngine: "sglang"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"image":"sglang:test","reason":"catalog_match","compatible":true,"engine":"sglang","engine_version":"0.5.21","selects_engine":"sglang"}`; string(labelled) != want {
		t.Fatalf("catalog image option = %s, want %s", labelled, want)
	}
	if !strings.Contains(string(b), `"options":[{"image":"vllm:test","reason":"catalog_match","compatible":true}]`) {
		t.Fatalf("an option outside the catalog states an engine: %s", b)
	}
	option := image["options"].([]any)[0].(map[string]any)
	if option["image"] != "vllm:test" || option["reason"] != "catalog_match" || option["compatible"] != true {
		t.Fatalf("image option wire contract=%#v", option)
	}
	node, _ := json.Marshal(acquisition["nodes"].([]any)[0])
	const wantNode = `{"image_cached":false,"image_plan":"pull","image_pull_ranked":true,"image_pull_refs":["mirror.example/vllm:test","vllm:test"],"node_name":"gpu1","node_uuid":"n1","weights_plan":"download","weights_present":false,"weights_provider":"huggingface"}`
	if string(node) != wantNode {
		t.Fatalf("node wire=%s\nwant=%s", node, wantNode)
	}
	var round EvaluateResponse
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(round.Acquisition, r.Acquisition) {
		t.Fatalf("round trip lost acquisition: %s", b)
	}
}

func TestAcquisitionNodeLiteralWireRoundTrip(t *testing.T) {
	for _, literal := range []string{
		`{"node_uuid":"a","node_name":"gpu-a","image_cached":true,"image_plan":"present","weights_present":true,"weights_plan":"present"}`,
		`{"node_uuid":"b","node_name":"gpu-b","image_cached":false,"image_plan":"copy","image_copy_from":"gpu-a","weights_present":false,"weights_plan":"copy","weights_copy_from":"gpu-a"}`,
		`{"node_uuid":"d","node_name":"gpu-d","image_cached":false,"image_plan":"pull","image_pull_error":{"code":"FIELD_INVALID","message":"no usable pull source"},"weights_present":false,"weights_plan":"download","weights_provider":"huggingface","weights_fetch_hosts":["huggingface.co"]}`,
		`{"node_uuid":"c","node_name":"gpu-c","image_cache_reason":"node_offline","image_plan":"pull","image_pull_error":{"code":"IMAGE_SOURCE_FORBIDDEN","params":{"node":"gpu-c","route":"/api/v1/pull/source-rules"},"message":"no pull source is allowed"},"weights_present":false,"weights_plan":"storage","weights_storage":"shared-nfs"}`,
	} {
		var decoded AcquisitionNode
		if err := json.Unmarshal([]byte(literal), &decoded); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(decoded)
		if err != nil {
			t.Fatal(err)
		}
		var want, got map[string]any
		if err := json.Unmarshal([]byte(literal), &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("wire=%s want=%s", encoded, literal)
		}
	}
}
