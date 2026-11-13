package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestSharedDownloadHonorsSelectedWriter(t *testing.T) {
	for _, scenario := range []string{"selected", "offline", "read-only", "not-ready", "outside-scope", "missing", "multiple", "running-other-writer"} {
		t.Run(scenario, func(t *testing.T) {
			r := newSaveToRig(t)
			r.nfsState(t, proto.StorageStateReady, true)
			selected, err := r.st.RegisterNode("selected", store.HashToken("selected"), "selected")
			if err != nil {
				t.Fatal(err)
			}
			state, writable := proto.StorageStateReady, true
			if scenario == "read-only" {
				writable = false
			}
			if scenario == "not-ready" {
				state = proto.StorageStateFailed
			}
			if err := r.st.ApplyStorageStatus(selected.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: r.nfs.ID, State: state, Path: "/mnt/gpuworks/team-nfs", Writable: writable}}}); err != nil {
				t.Fatal(err)
			}
			if scenario == "offline" {
				r.h.d.Online = func(uuid string) bool { return uuid != selected.UUID }
			}
			if scenario == "outside-scope" {
				r.nfs.AllNodes = false
				if _, err := r.st.UpdateStorage(r.nfs.ID, store.StoragePatch{AllNodes: &r.nfs.AllNodes, Targets: &[]store.StorageNode{{NodeID: r.node.ID}}}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "running-other-writer" {
				row, err := r.st.PrepareModelDownload(store.FileRef{ModelID: r.model.ID, StorageID: r.nfs.ID}, store.SharedModelRelPath(r.model))
				if err != nil {
					t.Fatal(err)
				}
				if err := r.st.SetModelFileWriter(row.ID, r.node.ID); err != nil {
					t.Fatal(err)
				}
			}
			uuids := `"selected"`
			if scenario == "missing" {
				uuids = `"does-not-exist"`
			}
			if scenario == "multiple" {
				uuids = `"gpu-a","selected"`
			}
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(fmt.Sprintf(`{"storage_id":%d,"node_uuids":[%s]}`, r.nfs.ID, uuids)))
			ctx := chi.NewRouteContext()
			ctx.URLParams.Add("id", fmt.Sprint(r.model.ID))
			rec := httptest.NewRecorder()
			r.h.downloadModel(rec, req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, ctx)))
			if scenario == "selected" {
				if rec.Code != http.StatusAccepted {
					t.Fatalf("status=%d %s", rec.Code, rec.Body)
				}
				row, err := r.st.ModelFileAt(store.FileRef{ModelID: r.model.ID, StorageID: r.nfs.ID})
				if err != nil || row.WriterNodeID != selected.ID {
					t.Fatalf("selected writer ignored: row=%+v err=%v", row, err)
				}
			} else {
				if rec.Code < 400 {
					t.Fatalf("invalid writer silently accepted: status=%d %s", rec.Code, rec.Body)
				}
				row, err := r.st.ModelFileAt(store.FileRef{ModelID: r.model.ID, StorageID: r.nfs.ID})
				if scenario == "running-other-writer" {
					if err != nil || row.WriterNodeID != r.node.ID {
						t.Fatalf("running writer changed: %+v %v", row, err)
					}
				} else if err == nil {
					t.Fatalf("invalid request created task: %+v", row)
				}
			}
		})
	}
}
