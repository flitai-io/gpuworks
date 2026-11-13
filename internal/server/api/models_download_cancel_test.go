package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestCancelDownloadAPIStorageLocations(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "shared"}[shared], func(t *testing.T) {
			r := newSaveToRig(t)
			ref := store.FileRef{ModelID: r.model.ID, NodeID: r.node.ID, StorageID: r.st.DefaultLocalStorageID()}
			if shared {
				ref.NodeID = 0
				ref.StorageID = r.nfs.ID
			}
			row, err := r.st.PrepareModelDownload(ref, "org/model")
			if err != nil {
				t.Fatal(err)
			}
			if shared {
				if err := r.st.SetModelFileWriter(row.ID, r.node.ID); err != nil {
					t.Fatal(err)
				}
			}
			var pushed string
			r.h.d.NodeSync = nodeSyncCallbacks{full: func(uuid string) error { pushed = uuid; return nil }}
			body := fmt.Sprintf(`{"storage_id":%d,"node_uuid":%q}`, ref.StorageID, r.node.UUID)
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(r.model.ID)}))
			rec := httptest.NewRecorder()
			r.h.cancelModelDownload(rec, req)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"canceled"`) || pushed != r.node.UUID {
				t.Fatalf("response=%d %s writer=%s", rec.Code, rec.Body.String(), pushed)
			}
			got, _ := r.st.ModelFileByID(row.ID)
			if got.State != proto.MFCanceled {
				t.Fatalf("download still running: %+v", got)
			}
			ds, err := r.st.DesiredStateForNode(r.node.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(ds.ModelFiles) != 1 || ds.ModelFiles[0].DesiredState == "present" {
				t.Fatalf("writer reconnect resumes canceled task: %+v", ds.ModelFiles)
			}
		})
	}
}

func TestCancelDownloadAPIErrors(t *testing.T) {
	r := newSaveToRig(t)
	for _, tc := range []struct {
		name, id, body string
		want           int
	}{
		{"invalid JSON", fmt.Sprint(r.model.ID), `{`, 400},
		{"missing model", "999999", `{}`, 404},
		{"missing storage", fmt.Sprint(r.model.ID), `{"storage_id":999999}`, 404},
		{"missing node", fmt.Sprint(r.model.ID), fmt.Sprintf(`{"storage_id":%d}`, r.st.DefaultLocalStorageID()), 400},
		{"missing task", fmt.Sprint(r.model.ID), fmt.Sprintf(`{"storage_id":%d}`, r.nfs.ID), 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": tc.id}))
			rec := httptest.NewRecorder()
			r.h.cancelModelDownload(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d body=%s want=%d", rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}

func TestCancelDownloadRouteRequiresAuthenticationAndPost(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	for _, tc := range []struct {
		method, path, token string
		want                int
	}{
		{"POST", "/api/v1/models/1/download/cancel", "", 401},
		{"PATCH", "/api/v1/models/1/download/cancel", rig.session, 405},
		{"POST", "/api/v1/models/999999/download/cancel", rig.session, 404},
	} {
		rec := rig.do(tc.token, tc.method, tc.path, `{}`)
		if rec.Code != tc.want {
			t.Fatalf("%s %s status=%d body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

func TestExplicitDownloadAdoptsRunningServiceTransfer(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "shared"}[shared], func(t *testing.T) {
			r := newSaveToRig(t)
			ref := store.FileRef{ModelID: r.model.ID, StorageID: r.st.DefaultLocalStorageID(), NodeID: r.node.ID}
			r.node.State = "online"
			if err := r.st.DB().Model(r.node).Update("state", "online").Error; err != nil {
				t.Fatal(err)
			}
			if shared {
				ref.StorageID = r.nfs.ID
				ref.NodeID = 0
				r.nfsState(t, proto.StorageStateReady, true)
			}
			row, err := r.st.PrepareModelDownload(ref, "org/model")
			if err != nil {
				t.Fatal(err)
			}
			if shared {
				if err := r.st.SetModelFileWriter(row.ID, r.node.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.st.SetDownloadDeployment(row.ID, row.TransferID, 17); err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`{"storage_id":%d,"node_uuids":[%q]}`, ref.StorageID, r.node.UUID)
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(r.model.ID)}))
			rec := httptest.NewRecorder()
			r.h.downloadModel(rec, req)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("adoption status=%d body=%s", rec.Code, rec.Body.String())
			}
			got, _ := r.st.ModelFileByID(row.ID)
			if got.DownloadDeploymentID != 0 || got.TransferID != row.TransferID {
				t.Fatalf("request failed to adopt running transfer: %+v", got)
			}
		})
	}
}
