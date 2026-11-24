package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestRuntimeImageImportVerifiesNodeBeforeRecordingPresence(t *testing.T) {
	st, _, _ := newSessionRouter(t)
	node, _, err := st.CreateNodePending("local-images", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	signer, admin := roleTestToken(t, st, "admin")
	_, viewer := roleTestToken(t, st, "viewer")
	const ref = "example/runner:v1"
	id := "sha256:" + strings.Repeat("a", 64)
	target := "sha256:" + strings.Repeat("b", 64)
	var calls []string
	conflict := false
	router := Router(Deps{Store: st, AuthSigner: signer, CallNode: func(_ context.Context, uuid, method string, input any) (json.RawMessage, error) {
		if uuid != node.UUID {
			t.Fatalf("wrong node %s", uuid)
		}
		p := input.(proto.RuntimeImageSelection)
		calls = append(calls, method)
		switch method {
		case proto.RPCImageRuntimeList:
			if p.Namespace != "default" {
				t.Fatalf("wrong namespace %s", p.Namespace)
			}
			return json.Marshal(proto.RuntimeImageList{Runtime: "nerdctl", TargetNamespace: "gpuworks", Namespace: "default", Items: []proto.RuntimeImage{{Ref: ref, Identity: target}}})
		case proto.RPCImageRuntimeInspect, proto.RPCImageRuntimeImport:
			if p.Namespace != "default" || p.TargetNamespace != "gpuworks" || p.Identity != target || p.Ref != ref {
				t.Fatalf("wrong selection %+v", p)
			}
			if conflict {
				return nil, &proto.RPCError{Code: proto.ErrorLocalImageConflict}
			}
			return json.Marshal(proto.RuntimeImageDetails{ImageID: id, Metadata: proto.ArchiveImageMetadata{Engine: "vllm-dcu", EngineVersion: "0.30.0"}})
		default:
			t.Fatalf("unexpected RPC %s", method)
			return nil, nil
		}
	}})
	request := func(method, path, token, body string, want int) string {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
		}
		return w.Body.String()
	}
	route := "/api/v1/images/nodes/" + node.UUID + "/runtime-images"
	selection := fmt.Sprintf(`{"node_uuid":%q,"runtime":"nerdctl","namespace":"default","target_namespace":"gpuworks","ref":%q,"identity":%q}`, node.UUID, ref, target)
	body := fmt.Sprintf(`{"runtime_image":%s,"series":[{"source":"custom","engine":"vllm","framework":"dtk","versions":[{"image_ref":%q,"framework_version":"25.04"}]}]}`, selection, ref)
	request("GET", route, viewer, "", http.StatusForbidden)
	request("POST", route+"/inspect", viewer, selection, http.StatusForbidden)
	request("POST", "/api/v1/images/import", viewer, body, http.StatusForbidden)
	if len(calls) != 0 {
		t.Fatal("unauthorized RPC")
	}
	request("GET", route+"?namespace=default", admin, "", http.StatusOK)
	got := request("POST", route+"/inspect", admin, selection, http.StatusOK)
	if !strings.Contains(got, `"engine":"vllm"`) {
		t.Fatalf("preview %s", got)
	}
	conflict = true
	got = request("POST", "/api/v1/images/import", admin, body, http.StatusConflict)
	if !strings.Contains(got, proto.ErrorLocalImageConflict) {
		t.Fatal(got)
	}
	if _, err = st.ImageByRef(ref); err != store.ErrNotFound {
		t.Fatalf("catalog changed on conflict: %v", err)
	}
	conflict = false
	request("POST", "/api/v1/images/import", admin, body, http.StatusOK)
	image, err := st.ImageByRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	image.RegistrySources = `["https://images.example/runner.tar"]`
	image.Notes = "operator note"
	if _, err := st.UpsertImage(image); err != nil {
		t.Fatal(err)
	}
	request("POST", "/api/v1/images/import", admin, body, http.StatusOK)
	saved, err := st.ImageByRef(ref)
	if err != nil || saved.RegistrySources != image.RegistrySources || saved.Notes != image.Notes {
		t.Fatalf("repeat import changed the registered version: %+v %v", saved, err)
	}
	row, err := st.ImageNode(image.ID, node.ID)
	if err != nil || row.State != store.ImageStatePresent || row.ImageDigestID != id {
		t.Fatalf("presence %+v, %v", row, err)
	}
}
