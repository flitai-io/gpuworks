package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// go test ./internal/server/api -run TestSharedModelPathConflict -count=1
func TestSharedModelPathConflict(t *testing.T) {
	for _, entry := range []string{"download", "deployment", "copy"} {
		t.Run(entry, func(t *testing.T) {
			r := newSaveToRig(t)
			r.nfsState(t, proto.StorageStateReady, true)
			owner, err := r.st.UpsertModel(&store.Model{Source: "modelscope", SourceID: r.model.SourceID, Revision: "main"})
			if err != nil {
				t.Fatal(err)
			}
			path := store.SharedModelRelPath(r.model)
			if path != store.SharedModelRelPath(owner) {
				t.Fatal("fixture must reproduce a collision between distinct model identities")
			}
			occupied, err := r.st.UpsertModelFile(&store.ModelFile{ModelID: owner.ID, StorageID: r.nfs.ID, Path: path, State: proto.MFPresent, SizeBytes: 15})
			if err != nil {
				t.Fatal(err)
			}
			r.h.d.NodeSync = nodeSyncCallbacks{full: func(string) error { t.Error("conflict pushed a download task"); return nil }}
			rec := httptest.NewRecorder()
			switch entry {
			case "download":
				req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(fmt.Sprintf(`{"storage_id":%d}`, r.nfs.ID)))
				ctx := chi.NewRouteContext()
				ctx.URLParams.Add("id", fmt.Sprint(r.model.ID))
				r.h.downloadModel(rec, req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, ctx)))
			case "deployment":
				_, err := r.h.deployments().StartSharedDownloadOn(r.model, r.nfs, "")
				if err == nil {
					t.Fatal("deployment accepted occupied path")
				}
				writeUseCaseErr(rec, err)
			case "copy":
				r.h.d.PeerSync.CopyModel = func(_ context.Context, modelID uint, _, dst store.FileRef, _ int) (*store.ModelFile, error) {
					return r.st.PrepareModelCopy(store.FileRef{ModelID: modelID, StorageID: dst.StorageID}, path)
				}
				req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(fmt.Sprintf(`{"source":{"storage_id":%d},"target":{"storage_id":%d}}`, r.st.DefaultLocalStorageID(), r.nfs.ID)))
				ctx := chi.NewRouteContext()
				ctx.URLParams.Add("id", fmt.Sprint(r.model.ID))
				r.h.copyModel(rec, req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, ctx)))
			}
			if rec.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var body struct {
				Error  string `json:"error"`
				Code   string `json:"code"`
				Params struct {
					StorageID uint   `json:"storage_id"`
					Path      string `json:"path"`
					ModelID   uint   `json:"model_id"`
				} `json:"params"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Code != "MODEL_FILE_PATH_OCCUPIED" || body.Params.StorageID != r.nfs.ID || body.Params.Path != path || body.Params.ModelID != owner.ID || strings.Contains(body.Error, "UNIQUE") {
				t.Fatalf("response=%s", rec.Body.String())
			}
			if row, err := r.st.ModelFileAt(store.FileRef{ModelID: r.model.ID, StorageID: r.nfs.ID}); err != store.ErrNotFound {
				t.Fatalf("conflict left row=%+v err=%v", row, err)
			}
			if row, err := r.st.ModelFileByID(occupied.ID); err != nil || row.State != proto.MFPresent || row.SizeBytes != 15 {
				t.Fatalf("owner changed: %+v err=%v", row, err)
			}
		})
	}
}
