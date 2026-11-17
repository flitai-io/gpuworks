package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestModelSyncAndCancelEndpoints(t *testing.T) {
	var syncModelID, cancelModelID uint
	var syncNode, syncSource, cancelNode string
	syncRate := 0
	h := apiHandlers{d: Deps{ModelTransfers: &fakeTransfers{
		sync: func(modelID uint, nodeUUID, sourceUUID string, rate int) (*store.ModelFile, error) {
			syncModelID, syncNode, syncSource, syncRate = modelID, nodeUUID, sourceUUID, rate
			return &store.ModelFile{State: proto.MFSyncing, TransferID: "transfer-1"}, nil
		},
		cancel: func(modelID uint, nodeUUID string) (*store.ModelFile, error) {
			cancelModelID, cancelNode = modelID, nodeUUID
			return &store.ModelFile{State: proto.MFDownloadRequired, TransferID: "transfer-1", Message: "canceled"}, nil
		},
	}}}

	syncReq := httptest.NewRequest(http.MethodPost, "/api/v1/models/7/nodes/node-1/sync", nil)
	syncReq = syncReq.WithContext(chiRouteContext(syncReq.Context(), map[string]string{"id": "7", "uuid": "node-1"}))
	syncRec := httptest.NewRecorder()
	h.syncModelToNode(syncRec, syncReq)
	// No body: the controller chooses the source and keeps the recorded rate.
	if syncRec.Code != http.StatusAccepted || syncModelID != 7 || syncNode != "node-1" || syncSource != "" || syncRate != -1 || !strings.Contains(syncRec.Body.String(), "transfer-1") {
		t.Fatalf("sync response/call = %d %s id=%d node=%q source=%q rate=%d", syncRec.Code, syncRec.Body.String(), syncModelID, syncNode, syncSource, syncRate)
	}

	cancelReq := httptest.NewRequest(http.MethodPost, "/api/v1/models/7/nodes/node-1/sync/cancel", nil)
	cancelReq = cancelReq.WithContext(chiRouteContext(cancelReq.Context(), map[string]string{"id": "7", "uuid": "node-1"}))
	cancelRec := httptest.NewRecorder()
	h.cancelModelSync(cancelRec, cancelReq)
	if cancelRec.Code != http.StatusOK || cancelModelID != 7 || cancelNode != "node-1" || !strings.Contains(cancelRec.Body.String(), "download_required") {
		t.Fatalf("cancel response/call = %d %s id=%d node=%q", cancelRec.Code, cancelRec.Body.String(), cancelModelID, cancelNode)
	}
}

func TestModelDTOIncludesPeerSyncDetails(t *testing.T) {
	st, err := store.Open("file:api_model_sync_dto?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, _ := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "test/model", Revision: "main", Name: "model"})
	source, _ := st.RegisterNode("source-node", store.HashToken("source"), "source")
	target, _ := st.RegisterNode("target-node", store.HashToken("target"), "target")
	if _, err := st.UpsertModelFile(&store.ModelFile{
		ModelID: model.ID, NodeID: target.ID, Path: "model", State: proto.MFSyncing,
		SizeBytes: 1000, Downloaded: 250, TransferredBytes: 300, RateBPS: 125, Acquisition: proto.ModelAcquirePeer,
		TransferID: "transfer-1", TransferSourceNodeID: source.ID, Message: "copying",
	}); err != nil {
		t.Fatal(err)
	}
	d := (&apiHandlers{d: Deps{Store: st}}).modelDTO(model, true)
	if len(d.Nodes) != 1 {
		t.Fatalf("model presence = %+v", d.Nodes)
	}
	p := d.Nodes[0]
	if p.RateBps != 125 || p.TransferredBytes != 300 || p.Acquisition != proto.ModelAcquirePeer || p.TransferID != "transfer-1" ||
		p.SourceNodeUUID != source.UUID || p.Message != "copying" || p.Progress != 0.25 {
		t.Fatalf("peer sync DTO = %+v", p)
	}
}

func TestListModelsIncludesNodePresence(t *testing.T) {
	st, err := store.Open("file:api_models_presence_list?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, _ := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "test/list", Revision: "main", Name: "list"})
	node, _ := st.RegisterNode("list-node", store.HashToken("token"), "list-node")
	if _, err := st.UpsertModelFile(&store.ModelFile{
		ModelID: model.ID, NodeID: node.ID, Path: "list", State: proto.MFPresent,
		SizeBytes: 10, Downloaded: 10, Acquisition: proto.ModelAcquirePeer,
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).listModels(rec, httptest.NewRequest(http.MethodGet, "/api/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	var models []dto.Model
	if err := unmarshalList(rec.Body.Bytes(), &models); err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || len(models[0].Nodes) != 1 || models[0].Nodes[0].NodeUUID != node.UUID {
		t.Fatalf("models response = %+v", models)
	}
}

func TestModelSyncOptionalRateLimit(t *testing.T) {
	for _, tc := range []struct {
		body         string
		rate, status int
	}{
		{"{}", -1, 202}, {`{"source_node":"src"}`, -1, 202},
		{`{"source_node":"src","rate_limit_kib_s":0}`, 0, 202},
		{`{"source_node":"src","rate_limit_kib_s":10240}`, 10240, 202},
		{`{"rate_limit_kib_s":-1}`, 0, 400},
	} {
		t.Run(tc.body, func(t *testing.T) {
			called := false
			wantSource := ""
			if strings.Contains(tc.body, "source_node") {
				wantSource = "src"
			}
			h := apiHandlers{d: Deps{ModelTransfers: &fakeTransfers{sync: func(modelID uint, node, source string, rate int) (*store.ModelFile, error) {
				called = true
				if modelID != 7 || node != "dst" || source != wantSource || rate != tc.rate {
					t.Fatalf("sync(%d, %q, %q, %d), want (7, dst, %q, %d)", modelID, node, source, rate, wantSource, tc.rate)
				}
				return &store.ModelFile{State: proto.MFSyncing}, nil
			}}}}
			rec := postJSON(t, h.syncModelToNode, "/api/v1/models/7/nodes/dst/sync", tc.body, map[string]string{"id": "7", "uuid": "dst"})
			if rec.Code != tc.status || called != (tc.status == 202) {
				t.Fatalf("status=%d called=%v body=%s", rec.Code, called, rec.Body)
			}
		})
	}
}

// A probe names the target from the path and the source from the body.
func TestModelProbeNamesTargetAndSource(t *testing.T) {
	var got [3]any
	h := apiHandlers{d: Deps{ModelTransfers: &fakeTransfers{probe: func(modelID uint, target, source string) error {
		got = [3]any{modelID, target, source}
		return nil
	}}}}
	rec := postJSON(t, h.probeModelToNode, "/api/v1/models/7/nodes/dst/probe", `{"source_node":"src"}`, map[string]string{"id": "7", "uuid": "dst"})
	if rec.Code != http.StatusOK || got != [3]any{uint(7), "dst", "src"} {
		t.Fatalf("probe = %d %s, called with %v", rec.Code, rec.Body, got)
	}
	// A failed probe is the caller's conflict, with the reason.
	h.d.ModelTransfers = &fakeTransfers{probe: func(uint, string, string) error { return errors.New("target refused the path") }}
	rec = postJSON(t, h.probeModelToNode, "/api/v1/models/7/nodes/dst/probe", `{"source_node":"src"}`, map[string]string{"id": "7", "uuid": "dst"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "target refused the path") {
		t.Fatalf("failed probe = %d %s", rec.Code, rec.Body)
	}
}

// Without the controller's transfers every transfer endpoint answers that
// model sync is not configured, and touches nothing.
func TestModelTransferEndpointsWithoutTransfers(t *testing.T) {
	h := apiHandlers{}
	for name, call := range map[string]func(http.ResponseWriter, *http.Request){
		"sync": h.syncModelToNode, "probe": h.probeModelToNode, "cancel": h.cancelModelSync,
	} {
		rec := postJSON(t, call, "/api/v1/models/7/nodes/dst/"+name, `{"source_node":"src"}`, map[string]string{"id": "7", "uuid": "dst"})
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), ErrorModelSyncUnavailable) {
			t.Fatalf("%s without transfers = %d %s", name, rec.Code, rec.Body)
		}
	}
}

// fakeTransfers answers each transfer with the function a test gives it and
// fails one it was not given, so an unexpected call cannot pass as success.
type fakeTransfers struct {
	sync   func(modelID uint, nodeUUID, sourceUUID string, rate int) (*store.ModelFile, error)
	probe  func(modelID uint, targetUUID, sourceUUID string) error
	cancel func(modelID uint, nodeUUID string) (*store.ModelFile, error)
	copy   func(modelID uint, src, dst store.FileRef, rate int) (*store.ModelFile, error)
}

var errUnexpectedTransfer = errors.New("unexpected model transfer")

func (f *fakeTransfers) Sync(_ context.Context, modelID uint, nodeUUID, sourceUUID string, rate int) (*store.ModelFile, error) {
	if f.sync == nil {
		return nil, errUnexpectedTransfer
	}
	return f.sync(modelID, nodeUUID, sourceUUID, rate)
}

func (f *fakeTransfers) Probe(_ context.Context, modelID uint, targetUUID, sourceUUID string) error {
	if f.probe == nil {
		return errUnexpectedTransfer
	}
	return f.probe(modelID, targetUUID, sourceUUID)
}

func (f *fakeTransfers) Cancel(_ context.Context, modelID uint, nodeUUID string) (*store.ModelFile, error) {
	if f.cancel == nil {
		return nil, errUnexpectedTransfer
	}
	return f.cancel(modelID, nodeUUID)
}

func (f *fakeTransfers) Copy(_ context.Context, modelID uint, src, dst store.FileRef, rate int) (*store.ModelFile, error) {
	if f.copy == nil {
		return nil, errUnexpectedTransfer
	}
	return f.copy(modelID, src, dst, rate)
}
