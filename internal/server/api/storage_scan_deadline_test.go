package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/rpccontext"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestScanStorageUsesPageBudgetWithoutCancellationRPC(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	node, err := st.RegisterNode("scan-node", store.HashToken("token"), "scan-node")
	if err != nil {
		t.Fatal(err)
	}
	shared, err := st.CreateStorage(&store.Storage{Name: "shared", Type: proto.StorageNFS, Enabled: true, AllNodes: true, Path: "/mnt/models", Export: "nas:/models"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: shared.ID, State: proto.StorageStateReady, Path: "/mnt/models"}}}); err != nil {
		t.Fatal(err)
	}
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout=%v", fail), func(t *testing.T) {
			calls := 0
			h := apiHandlers{d: Deps{Store: st, Online: func(string) bool { return true },
				CallNode: func(ctx context.Context, uuid, method string, raw any) (json.RawMessage, error) {
					calls++
					if method != proto.RPCStorageScan || uuid != node.UUID {
						t.Fatalf("unexpected RPC %s %s", uuid, method)
					}
					p := raw.(proto.StorageScanParams)
					if p.StorageID != shared.ID || p.Cursor != "org/previous" || p.Limit != 200 {
						t.Fatalf("input=%+v", p)
					}
					if budget := rpccontext.Timeout(ctx, 30*time.Second); budget != scanPageTimeout+10*time.Second {
						t.Fatalf("RPC wait budget=%s", budget)
					}
					deadline, ok := ctx.Deadline()
					remaining := time.Until(deadline)
					if !ok || remaining <= time.Duration(p.TimeoutSeconds)*time.Second || remaining > scanPageTimeout+10*time.Second {
						t.Fatalf("scan budget=%d RPC remaining=%s", p.TimeoutSeconds, remaining)
					}
					if fail {
						return nil, context.DeadlineExceeded
					}
					return json.Marshal(proto.StorageScanResult{Complete: true})
				},
			}}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/storages/1/scan", strings.NewReader(`{"cursor":"org/previous","limit":200}`))
			route := chi.NewRouteContext()
			route.URLParams.Add("id", fmt.Sprint(shared.ID))
			req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
			rec := httptest.NewRecorder()
			h.scanStorage(rec, req)
			want := http.StatusOK
			if fail {
				want = http.StatusConflict
			}
			if rec.Code != want || calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
			}
		})
	}
}
