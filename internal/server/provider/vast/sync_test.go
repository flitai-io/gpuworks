package vast

import (
	"net/http"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestSyncVastPreservesInventoryWhenSecondPageFails(t *testing.T) {
	var failSecond atomic.Bool
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/instances/" || r.Method != http.MethodGet {
			http.Error(w, "deprecated endpoint", http.StatusGone)
			return
		}
		switch {
		case r.URL.Query().Get("limit") == "1":
			writeInstancePage(w, 1, 1, 26, "next")
		case r.URL.Query().Get("after_token") == "":
			writeInstancePage(w, 1, 25, 26, "next")
		case r.URL.Query().Get("after_token") == "next":
			if failSecond.Load() {
				http.Error(w, "second page unavailable", http.StatusBadGateway)
				return
			}
			writeInstancePage(w, 26, 1, 26, nil)
		default:
			http.Error(w, "invalid cursor", http.StatusBadRequest)
		}
	})
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	db, err := st.DB().DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	box, err := auth.NewSecretbox("vast-pagination-sync-test")
	if err != nil {
		t.Fatal(err)
	}
	svc := provider.NewService(st, box)
	svc.SetDriver(d)
	creds := provider.Credentials{Endpoint: d.baseURL, APIKey: "test-key"}
	p, err := svc.Add(t.Context(), "vast", "vast-account", creds)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(t.Context(), p.ID, "vast-updated", creds); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.Sync(t.Context(), p.ID); err != nil || len(got) != 26 {
		t.Fatalf("initial sync: instances=%d, err=%v", len(got), err)
	}
	node, _, err := st.CreateNodePending("last-page-instance")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LinkProviderInstanceNode(p.ID, "26", node.ID); err != nil {
		t.Fatal(err)
	}
	before, err := st.ProviderInstances(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	failSecond.Store(true)
	if got, err := svc.Sync(t.Context(), p.ID); err == nil || got != nil {
		t.Fatalf("partial sync was accepted: instances=%v, err=%v", got, err)
	}
	after, err := st.ProviderInstances(p.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("failed sync changed stored inventory: before=%d, after=%d, err=%v", len(before), len(after), err)
	}
	linked, err := st.NodeByUUID(node.UUID)
	if err != nil || linked.RecoveryPhase != node.RecoveryPhase {
		t.Fatalf("failed sync changed linked node recovery: node=%+v, err=%v", linked, err)
	}
	failSecond.Store(false)
	if got, err := svc.Sync(t.Context(), p.ID); err != nil || len(got) != 26 {
		t.Fatalf("recovered sync: instances=%d, err=%v", len(got), err)
	}
}
