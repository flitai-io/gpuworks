package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestModelFileEvictionRouteAuthorization(t *testing.T) {
	st := modelEvictionStore(t, "auth")
	signer := modelEvictionSigner(t)
	router := Router(Deps{Store: st, AuthSigner: signer})

	request := func(token string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/models/1/nodes/node-1", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	if got := request(""); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d body=%s", got.Code, got.Body.String())
	}
	userToken, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 2, Username: "user", Role: "user"})
	if got := request(userToken); got.Code != http.StatusForbidden {
		t.Fatalf("non-admin status=%d body=%s", got.Code, got.Body.String())
	}
}

func TestModelFileEvictionSuccessAndIdempotencyWireContract(t *testing.T) {
	st := modelEvictionStore(t, "success")
	model, node := modelEvictionFixture(t, st)
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "org/model", SizeBytes: 100, Downloaded: 80}); err != nil {
		t.Fatal(err)
	}
	var calls int
	router, token := modelEvictionRouter(t, st, func(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
		calls++
		if uuid != node.UUID || method != proto.RPCModelEvict || !reflect.DeepEqual(params, map[string]string{"path": "org/model"}) {
			return nil, fmt.Errorf("unexpected RPC uuid=%s method=%s params=%#v", uuid, method, params)
		}
		return json.RawMessage(`{}`), nil
	})

	path := fmt.Sprintf("/api/v1/models/%d/nodes/%s", model.ID, node.UUID)
	first := modelEvictionRequest(t, router, token, path)
	if first.Code != http.StatusOK || first.Body.String() != "{\"logical_bytes\":100,\"physical_bytes\":80}\n" {
		t.Fatalf("first response=%d %s", first.Code, first.Body.String())
	}
	if calls != 1 {
		t.Fatalf("RPC calls=%d, want 1", calls)
	}
	if _, err := st.ModelFileByModelNode(model.ID, node.ID); err != store.ErrNotFound {
		t.Fatalf("model file after eviction err=%v", err)
	}

	second := modelEvictionRequest(t, router, token, path)
	if second.Code != http.StatusOK || second.Body.String() != "{\"logical_bytes\":0,\"physical_bytes\":0}\n" || calls != 1 {
		t.Fatalf("idempotent response=%d %s calls=%d", second.Code, second.Body.String(), calls)
	}
}

// A file still arriving by peer sync has its transfer canceled before the
// node deletes it, so the target does not go on writing into a removed
// directory; a cancel that fails leaves the file in place.
func TestModelFileEvictionCancelsAPeerSyncFirst(t *testing.T) {
	for _, cancelErr := range []error{nil, errors.New("target unreachable")} {
		st := modelEvictionStore(t, fmt.Sprintf("syncing-%v", cancelErr != nil))
		model, node := modelEvictionFixture(t, st)
		if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFSyncing, Path: "org/model", Acquisition: proto.ModelAcquirePeer, TransferID: "t-1"}); err != nil {
			t.Fatal(err)
		}
		var order []string
		signer := modelEvictionSigner(t)
		token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
		if err != nil {
			t.Fatal(err)
		}
		router := Router(Deps{Store: st, AuthSigner: signer,
			CallNode: func(context.Context, string, string, any) (json.RawMessage, error) {
				order = append(order, "evict")
				return json.RawMessage(`{}`), nil
			},
			ModelTransfers: &fakeTransfers{cancel: func(modelID uint, nodeUUID string) (*store.ModelFile, error) {
				order = append(order, fmt.Sprintf("cancel %d %s", modelID, nodeUUID))
				return &store.ModelFile{State: proto.MFDownloadRequired}, cancelErr
			}},
		})
		rec := modelEvictionRequest(t, router, token, fmt.Sprintf("/api/v1/models/%d/nodes/%s", model.ID, node.UUID))
		wantCancel := fmt.Sprintf("cancel %d %s", model.ID, node.UUID)
		if cancelErr == nil {
			if rec.Code != http.StatusOK || strings.Join(order, ",") != wantCancel+",evict" {
				t.Fatalf("eviction = %d %s, calls %v; want the cancel, then the eviction", rec.Code, rec.Body, order)
			}
			continue
		}
		if rec.Code != http.StatusConflict || strings.Join(order, ",") != wantCancel {
			t.Fatalf("failed cancel = %d %s, calls %v; want a conflict and no eviction", rec.Code, rec.Body, order)
		}
		if _, err := st.ModelFileByModelNode(model.ID, node.ID); err != nil {
			t.Fatalf("a failed cancel removed the file: %v", err)
		}
	}
}

func TestModelFileEvictionConflictListsDeploymentIDs(t *testing.T) {
	st := modelEvictionStore(t, "conflict")
	model, node := modelEvictionFixture(t, st)
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "org/model"}); err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	rpcCalled := false
	router, token := modelEvictionRouter(t, st, func(context.Context, string, string, any) (json.RawMessage, error) {
		rpcCalled = true
		return nil, nil
	})

	response := modelEvictionRequest(t, router, token, fmt.Sprintf("/api/v1/models/%d/nodes/%s", model.ID, node.UUID))
	want := fmt.Sprintf("{\"code\":\"%s\",\"error\":\"model file is referenced by active deployments [%d]\",\"params\":{\"deployment_ids\":[%d]}}\n", ErrorModelInUse, dep.ID, dep.ID)
	if response.Code != http.StatusConflict || response.Body.String() != want || rpcCalled {
		t.Fatalf("conflict response=%d %s rpc=%v want=%s", response.Code, response.Body.String(), rpcCalled, want)
	}
}

func TestModelFileEvictionNotFoundWireContract(t *testing.T) {
	st := modelEvictionStore(t, "not_found")
	model, _ := modelEvictionFixture(t, st)
	router, token := modelEvictionRouter(t, st, nil)
	for _, path := range []string{
		"/api/v1/models/999999/nodes/missing-node",
		fmt.Sprintf("/api/v1/models/%d/nodes/missing-node", model.ID),
	} {
		response := modelEvictionRequest(t, router, token, path)
		if response.Code != http.StatusNotFound || response.Body.String() != "{\"code\":\"NOT_FOUND\",\"error\":\"not found\"}\n" {
			t.Fatalf("path=%s response=%d %s", path, response.Code, response.Body.String())
		}
	}
}

func modelEvictionStore(t *testing.T, suffix string) *store.Store {
	t.Helper()
	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name() + "_" + suffix)
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func modelEvictionFixture(t *testing.T, st *store.Store) (*store.Model, *store.Node) {
	t.Helper()
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/model", Revision: "main", Name: "model"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("eviction-node", store.HashToken("token"), "eviction-node")
	if err != nil {
		t.Fatal(err)
	}
	return model, node
}

func modelEvictionSigner(t *testing.T) *auth.TokenSigner {
	t.Helper()
	signer, err := auth.NewTokenSigner("model-eviction-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func modelEvictionRouter(t *testing.T, st *store.Store, callNode func(context.Context, string, string, any) (json.RawMessage, error)) (http.Handler, string) {
	t.Helper()
	signer := modelEvictionSigner(t)
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	return Router(Deps{Store: st, AuthSigner: signer, CallNode: callNode}), token
}

func modelEvictionRequest(t *testing.T, router http.Handler, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}
