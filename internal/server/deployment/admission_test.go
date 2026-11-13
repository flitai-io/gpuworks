package deployment

import (
	"context"
	"errors"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestAdmitDeploymentModelRequiresReachableSourceBeforeCreate(t *testing.T) {
	st, err := store.Open("file:deployment_admission?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("admission-node", store.HashToken("token"), "admission-node")
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/model", Revision: "rev"})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Store: st, FetchModelMeta: func(ctx context.Context, _, _, _, _ string, _ []string) (*modelmeta.Meta, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			return nil, errors.New("source down")
		}
	}})
	err = h.admitDeploymentModel(context.Background(), model, store.SourceRules{}, []*store.Node{node})
	if err == nil || !containsProductCode(err, messages.ErrorModelSourceUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestAdmitDeploymentModelAcceptsCompleteLocalArtifact(t *testing.T) {
	st, err := store.Open("file:deployment_admission_present?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("admission-present", store.HashToken("token"), "admission-present")
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/model"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/opt/gpuworks/models/org/model"}); err != nil {
		t.Fatal(err)
	}
	called := false
	h := New(Deps{Store: st, FetchModelMeta: func(context.Context, string, string, string, string, []string) (*modelmeta.Meta, error) {
		called = true
		return nil, errors.New("must not call")
	}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.admitDeploymentModel(ctx, model, store.SourceRules{}, []*store.Node{node}); err != nil || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func TestAdmitDeploymentModelWaitsForAcceptedLocalCopy(t *testing.T) {
	st := modelEvictionStore(t, "admission-copy")
	_, node := modelEvictionFixture(t, st)
	model, err := st.UpsertModel(&store.Model{Source: "local", SourceID: "copy", Name: "copy"})
	if err != nil {
		t.Fatal(err)
	}
	file, err := st.PrepareModelCopy(store.FileRef{ModelID: model.ID, NodeID: node.ID}, "/copy")
	if err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Store: st, FetchModelMeta: func(context.Context, string, string, string, string, []string) (*modelmeta.Meta, error) {
		t.Fatal("copy admission fetched a public source")
		return nil, nil
	}})
	if err := h.admitDeploymentModel(context.Background(), model, store.SourceRules{}, []*store.Node{node}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{Key: store.ModelFileKey(file.ID), TransferID: file.TransferID, State: proto.MFFailed}); err != nil {
		t.Fatal(err)
	}
	if err := h.admitDeploymentModel(context.Background(), model, store.SourceRules{}, []*store.Node{node}); !containsProductCode(err, messages.ErrorModelSourceUnavailable) {
		t.Fatalf("failed copy admitted: %v", err)
	}
}
