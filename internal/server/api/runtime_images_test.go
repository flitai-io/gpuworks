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

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
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
	online, legacy, copyRequired := true, false, false
	transfers := 0
	router := Router(Deps{Store: st, AuthSigner: signer, Online: func(string) bool { return online },
		ImageFetch: ImageFetchHooks{Pull: func(ctx context.Context, id uint, uuid string, fetch imagecatalog.FetchRequest) (*store.ImageNode, error) {
			transfers++
			if item, err := st.GetImage(id); err != nil || fetch.RuntimeImage == nil || item.ImageRef != fetch.RuntimeImage.Ref || uuid != node.UUID {
				t.Fatal("unregistered or incorrect copy request", id, uuid, fetch)
			}
			if _, err := st.ImageNode(id, node.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("presence already recorded: %v", err)
			}
			return &store.ImageNode{State: store.ImageStateSyncing}, nil
		}},
		CallNode: func(_ context.Context, uuid, method string, input any) (json.RawMessage, error) {
			if uuid != node.UUID {
				t.Fatalf("wrong node %s", uuid)
			}
			if legacy {
				return nil, proto.ErrRPCUnknownMethod
			}
			p := input.(proto.RuntimeImageSelection)
			calls = append(calls, method)
			switch method {
			case proto.RPCImageRuntimeList:
				if p.Namespace != "default" {
					t.Fatalf("wrong namespace %s", p.Namespace)
				}
				return json.Marshal(proto.RuntimeImageList{Runtime: "nerdctl", TargetNamespace: "gpuworks", Namespace: "default", Namespaces: []string{"default", "gpuworks"}, Items: []proto.RuntimeImage{{Ref: ref, Identity: target}}})
			case proto.RPCImageRuntimeInspect, proto.RPCImageRuntimePrepare:
				if p.Namespace != "default" || p.TargetNamespace != "gpuworks" || p.Identity != target || p.Ref != ref {
					t.Fatalf("wrong selection %+v", p)
				}
				if conflict {
					return nil, &proto.RPCError{Code: proto.ErrorLocalImageConflict}
				}
				return json.Marshal(proto.RuntimeImageDetails{ImageID: id, CopyRequired: copyRequired, Metadata: proto.ArchiveImageMetadata{Engine: "vllm-dcu", EngineVersion: "0.30.0"}})
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
	for _, endpoint := range []struct{ verb, suffix, body string }{{"GET", "", ""}, {"POST", "/inspect", selection}} {
		request(endpoint.verb, strings.Replace(route, node.UUID, "missing", 1)+endpoint.suffix, admin, endpoint.body, http.StatusNotFound)
		online = false
		got := request(endpoint.verb, route+endpoint.suffix, admin, endpoint.body, http.StatusConflict)
		if !strings.Contains(got, ErrorNodeOffline) {
			t.Fatal(got)
		}
		online, legacy = true, true
		got = request(endpoint.verb, route+endpoint.suffix, admin, endpoint.body, http.StatusConflict)
		if !strings.Contains(got, messages.ErrorAgentUpgradeRequired) {
			t.Fatal(got)
		}
		legacy = false
	}
	request("POST", route+"/inspect", admin, "{", http.StatusBadRequest)
	request("POST", route+"/inspect", admin, "{}", http.StatusBadRequest)
	list := request("GET", route+"?namespace=default", admin, "", http.StatusOK)
	var gotList, wantList any
	if err := json.Unmarshal([]byte(list), &gotList); err != nil {
		t.Fatal(err)
	}
	wire := fmt.Sprintf(`{"runtime":"nerdctl","target_namespace":"gpuworks","namespace":"default","namespaces":["default","gpuworks"],"items":[{"ref":%q,"identity":%q,"size":""}]}`, ref, target)
	if err := json.Unmarshal([]byte(wire), &wantList); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotList, wantList) {
		t.Fatalf("list response: %s", list)
	}
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
	copyRequired = true
	request("POST", "/api/v1/images/import", admin, body, http.StatusOK)
	if transfers != 1 {
		t.Fatalf("copy dispatches: %d", transfers)
	}
	copyRequired = false
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

// Another admin can change the catalog while the node verifies an image.
// Use the real store on both sides of that RPC.
func TestRuntimeImageCatalogChangesDuringVerification(t *testing.T) {
	for _, scenario := range []string{"new version", "concurrent registration", "deleted", "replaced", "inherited framework missing version"} {
		t.Run(scenario, func(t *testing.T) {
			st, _, _ := newSessionRouter(t)
			node, _, err := st.CreateNodePending("worker", "agent_dial")
			if err != nil {
				t.Fatal(err)
			}
			signer, admin := roleTestToken(t, st, "admin")
			const ref = "example/runner:v2"
			const sources = `["https://images.example/keep.tar"]`
			series, err := st.UpsertImageSeries(&store.ImageSeries{Name: "example/runner", Source: "custom", Engine: "vllm", Framework: "dtk", Purpose: store.ImagePurposeEvaluation, Notes: "operator note"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.SetImageSeriesEnabled(series.ID, false); err != nil {
				t.Fatal(err)
			}
			create := func(tag string) *store.Image {
				t.Helper()
				row, err := st.UpsertImage(&store.Image{SeriesID: series.ID, ImageRef: tag, Engine: "vllm", Framework: "dtk", FrameworkVersion: "25.04", Source: "custom", Notes: "existing version", RegistrySources: sources})
				if err != nil {
					t.Fatal(err)
				}
				return row
			}
			create("example/runner:v1")
			var before *store.Image
			if scenario == "deleted" || scenario == "replaced" {
				before = create(ref)
			}
			installedID := "sha256:" + strings.Repeat("a", 64)
			calls := 0
			router := Router(Deps{Store: st, AuthSigner: signer, CallNode: func(_ context.Context, uuid, method string, _ any) (json.RawMessage, error) {
				calls++
				if uuid != node.UUID || method != proto.RPCImageRuntimePrepare {
					t.Fatal("unexpected RPC", uuid, method)
				}
				switch scenario {
				case "concurrent registration":
					create(ref)
				case "deleted", "replaced":
					if err := st.DeleteImage(before.ID); err != nil {
						t.Fatal(err)
					}
					if scenario == "replaced" {
						create(ref)
					}
				}
				return json.Marshal(proto.RuntimeImageDetails{ImageID: installedID})
			}})
			body := fmt.Sprintf(`{"runtime_image":{"node_uuid":%q,"runtime":"nerdctl","namespace":"default","target_namespace":"gpuworks","ref":%q,"identity":%q},"series":[{"engine":"vllm","source":"custom","versions":[{"image_ref":%q,"framework_version":"25.04"}]}]}`, node.UUID, ref, installedID, ref)
			if scenario == "inherited framework missing version" {
				body = strings.Replace(body, `,"framework_version":"25.04"`, "", 1)
			}
			req := httptest.NewRequest("POST", "/api/v1/images/import", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+admin)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if scenario == "inherited framework missing version" {
				if calls != 0 || rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), ErrorImageFrameworkVersionRequired) {
					t.Fatalf("validation: calls=%d status=%d body=%s", calls, rec.Code, rec.Body)
				}
				return
			}
			if calls != 1 {
				t.Fatalf("node calls: %d", calls)
			}
			if scenario == "deleted" || scenario == "replaced" {
				if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), messages.ErrorLocalImageCatalogChanged) {
					t.Fatalf("expected conflict: %d %s", rec.Code, rec.Body)
				}
				image, err := st.ImageByRef(ref)
				if scenario == "deleted" && !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("deleted version reappeared: %v", err)
				}
				if scenario == "replaced" {
					if err != nil || image.ID == before.ID {
						t.Fatalf("expected replacement: %v", err)
					}
					if _, err := st.ImageNode(image.ID, node.ID); !errors.Is(err, store.ErrNotFound) {
						t.Fatalf("unexpected replacement presence: %v", err)
					}
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("register: %d %s", rec.Code, rec.Body)
			}
			saved, err := st.GetImageSeries(series.ID)
			if err != nil || saved.Enabled || saved.Purpose != store.ImagePurposeEvaluation || saved.Notes != "operator note" {
				t.Fatalf("series settings changed: %+v %v", saved, err)
			}
			image, err := st.ImageByRef(ref)
			if err != nil || image.SeriesID != series.ID || image.Framework != "dtk" {
				t.Fatalf("version: %+v %v", image, err)
			}
			if scenario == "concurrent registration" && (image.Notes != "existing version" || image.RegistrySources != sources) {
				t.Fatalf("version metadata changed: %+v", image)
			}
			present, err := st.ImageNode(image.ID, node.ID)
			if err != nil || present.State != store.ImageStatePresent || present.ImageDigestID != installedID {
				t.Fatalf("presence: %+v %v", present, err)
			}
		})
	}
}
