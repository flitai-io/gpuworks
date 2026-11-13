package deployment

import (
	"context"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// createFixture is one node with an H100 and a model that fits it, the
// smallest cluster a create can succeed on.
func createFixture(t *testing.T) (*Service, *store.Store, uint) {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.RegisterNode("uc-node", store.HashToken("token"), "uc-node"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("uc-node", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "uc-node", Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/uc", Revision: "main", Name: "uc-model", WeightBytes: 20 << 30})
	if err != nil {
		t.Fatal(err)
	}
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	return New(Deps{Store: st}), st, model.ID
}

// The use case creates a deployment from the request type alone - no HTTP
// request or response - and stores what it answers.
func TestCreateStoresTheDeploymentWithoutHTTP(t *testing.T) {
	s, st, modelID := createFixture(t)
	created, err := s.Create(context.Background(), dto.DeployRequest{Name: "uc", ModelID: modelID, Engine: "vllm", Image: "example/vllm:test", NodeUUID: "uc-node"})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetDeployment(created.ID)
	if err != nil || stored.Name != "uc" || stored.ServedName != "uc-model" || stored.Replicas != 1 || stored.State != store.DeployActive {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
}

// An automatic create that names no node has the server pick one, as the
// evaluation would.
func TestCreateWithoutANodePicksTheEvaluatedOne(t *testing.T) {
	s, st, modelID := createFixture(t)
	var picked []string
	s.d.AfterAutomaticPick = func(uuid string) { picked = append(picked, uuid) }
	created, err := s.Create(context.Background(), dto.DeployRequest{Name: "auto", ModelID: modelID, Engine: "vllm", PlacementMode: "auto", TargetVendor: "nvidia"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.NodeByUUID("uc-node")
	if err != nil {
		t.Fatal(err)
	}
	if created.NodeID != node.ID || len(picked) != 1 || picked[0] != "uc-node" {
		t.Fatalf("created on node %d, picks %v; want %d once", created.NodeID, picked, node.ID)
	}
}

// Each refusal says what it is to the caller: a wrong request, a conflict
// with the cluster as it stands, or the server's own failure. Nothing is
// written for any of them.
func TestCreateRefusalsCarryTheirKind(t *testing.T) {
	s, st, modelID := createFixture(t)
	if _, err := s.Create(context.Background(), dto.DeployRequest{Name: "first", ServedName: "taken", ModelID: modelID, Engine: "vllm", Image: "example/vllm:test", NodeUUID: "uc-node"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		req  dto.DeployRequest
		kind Kind
		code string
	}{
		{"unknown vendor", dto.DeployRequest{ModelID: modelID, TargetVendor: "nobody"}, KindInvalid, messages.ErrorTargetVendorInvalid},
		{"negative replicas", dto.DeployRequest{ModelID: modelID, Engine: "vllm", NodeUUID: "uc-node", Replicas: -1}, KindInvalid, messages.ErrorReplicasNegative},
		{"control character in the name", dto.DeployRequest{Name: "a\x00b", ModelID: modelID}, KindInvalid, messages.ErrorFieldInvalid},
		{"unknown node", dto.DeployRequest{ModelID: modelID, Engine: "vllm", Image: "example/vllm:test", NodeUUID: "nowhere"}, KindInvalid, messages.ErrorFieldInvalid},
		{"served name in use", dto.DeployRequest{Name: "second", ServedName: "taken", ModelID: modelID, Engine: "vllm", Image: "example/vllm:test", NodeUUID: "uc-node"}, KindConflict, messages.ErrorServedNameInUse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created, err := s.Create(context.Background(), tc.req)
			if err == nil || created != nil {
				t.Fatalf("create = %+v, %v; want a refusal", created, err)
			}
			if KindOf(err) != tc.kind || !containsProductCode(err, tc.code) {
				t.Fatalf("refusal = kind %d %v, want kind %d %s", KindOf(err), err, tc.kind, tc.code)
			}
		})
	}
	if rows, err := st.ListDeployments(); err != nil || len(rows) != 1 {
		t.Fatalf("deployments after refused creates = %d, %v", len(rows), err)
	}
}

// An error that names no Kind is the server's own; the outermost Kind wins.
func TestKindOf(t *testing.T) {
	plain := errors.New("disk")
	if KindOf(plain) != KindInternal || KindOf(nil) != KindInternal {
		t.Fatal("an error without a kind is not internal")
	}
	if KindOf(invalid(conflict(plain))) != KindInvalid || KindOf(internal(conflict(plain))) != KindInternal {
		t.Fatal("the outermost kind does not win")
	}
	if !errors.Is(conflict(plain), plain) || withKind(KindConflict, nil) != nil {
		t.Fatal("a kind hides its cause or makes an error of nil")
	}
}

// Evaluate answers a draft's field errors in the preview rather than refusing
// it, and refuses only what cannot be previewed at all.
func TestEvaluateAnswersFieldErrorsInThePreview(t *testing.T) {
	s, _, modelID := createFixture(t)
	resp, err := s.Evaluate(context.Background(), dto.EvaluateRequest{DeployRequest: dto.DeployRequest{ModelID: modelID, Engine: "no-such-engine", NodeUUID: "uc-node"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.OK || len(resp.Errors) != 1 || resp.Errors[0].Field != "engine" || resp.Errors[0].Code != messages.ErrorEngineInvalid {
		t.Fatalf("preview = ok %v errors %+v", resp.OK, resp.Errors)
	}
	resp, err = s.Evaluate(context.Background(), dto.EvaluateRequest{DeployRequest: dto.DeployRequest{ModelID: modelID, Engine: "vllm", Image: "example/vllm:test", NodeUUID: "uc-node"}})
	if err != nil || !resp.OK || resp.SchemaVersion != evaluateSchemaVersion {
		t.Fatalf("preview = %+v, %v", resp, err)
	}
}
