package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Registration can infer a video task, but only the selected engine template
// knows whether its API is synchronous and which encoding it accepts.
func TestRegisteredVideoModelTakesTemplateContractUnlessExplicitlyEdited(t *testing.T) {
	for _, edited := range []bool{false, true} {
		t.Run(fmt.Sprintf("edited=%t", edited), func(t *testing.T) {
			st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			fixture := deploymentFlatFixture()
			fixture.ModelID = "acme/video"
			fixture.InputModalities = `["text"]`
			fixture.Specs[0].Name = "video"
			fixture.Specs[0].WeightSources = `[{"provider":"huggingface","repo_id":"Acme/Video","revision":"0123456789abcdef0123456789abcdef01234567"}]`
			fixture.Specs[0].APIContract = `{"operations":[{"type":"openai.video.generate","endpoint":"/v1/videos/sync","mode":"sync","request_encoding":"multipart","inputs":[{"name":"seconds","kind":"number"}]}]}`
			if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
				t.Fatal(err)
			}
			template, err := st.InferenceModelByModelID(fixture.ModelID)
			if err != nil {
				t.Fatal(err)
			}
			const nodeUUID = "video-contract-node"
			launchConfigNode(t, st, nodeUUID)
			signer, err := auth.NewTokenSigner("video-contract-test", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
			router := Router(Deps{Store: st, AuthSigner: signer})
			request := func(method, path, body string, wantStatus int) *httptest.ResponseRecorder {
				t.Helper()
				req := httptest.NewRequest(method, path, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				if rec.Code != wantStatus {
					t.Fatalf("%s %s = %d: %s", method, path, rec.Code, rec.Body.String())
				}
				return rec
			}
			registered := request(http.MethodPost, "/api/v1/models", `{"source":"huggingface","source_id":"Acme/Video","name":"Acme/Video","revision":"0123456789abcdef0123456789abcdef01234567","browser_metadata":true,"readme":"---\npipeline_tag: text-to-video\n---\n"}`, http.StatusCreated)
			var model dto.Model
			if err := json.Unmarshal(registered.Body.Bytes(), &model); err != nil {
				t.Fatal(err)
			}
			stored, err := st.GetModel(model.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Operations != `[{"inputs":null,"type":"openai.video.generate"}]` {
				t.Fatalf("registration must persist the inferred operation, got %s", stored.Operations)
			}
			if edited {
				request(http.MethodPatch, fmt.Sprintf("/api/v1/models/%d/inference-config", model.ID), `{"operations":[{"type":"openai.video.generate","endpoint":"/v1/videos/sync","mode":"sync","request_encoding":"json","inputs":[{"name":"seconds","kind":"number","default":1}]}],"input_modalities":["text"]}`, http.StatusOK)
				stored, err = st.GetModel(model.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			created := request(http.MethodPost, "/api/v1/deployments", fmt.Sprintf(`{"template_id":%d,"template_spec_name":"video","model_id":%d,"node_uuid":%q}`, template.ID, model.ID, nodeUUID), http.StatusCreated)
			var deployment dto.Deployment
			if err := json.Unmarshal(created.Body.Bytes(), &deployment); err != nil {
				t.Fatal(err)
			}
			after, err := st.GetModel(model.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantEncoding := inferenceapi.EncodingMultipart
			if edited {
				wantEncoding = inferenceapi.EncodingJSON
				if after.Operations != stored.Operations || after.InputModalities != stored.InputModalities {
					t.Fatal("template deployment replaced an explicit model configuration")
				}
			}
			contract := inferenceapi.ModelContract(after)
			if len(contract.Operations) != 1 {
				t.Fatalf("model contract = %+v", contract)
			}
			operation := contract.Operations[0]
			if operation.Endpoint != "/v1/videos/sync" || operation.Mode != "sync" || operation.RequestEncoding != wantEncoding || len(operation.Inputs) != 1 || operation.Inputs[0].Name != "seconds" {
				t.Fatalf("model contract did not retain the complete operation: %+v", operation)
			}
			if len(deployment.Operations) != 1 || deployment.Operations[0].Endpoint != operation.Endpoint {
				t.Fatalf("deployment view = %+v", deployment.Operations)
			}
			node, err := st.NodeByUUID(nodeUUID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.CreateInstance(&store.Instance{UUID: "video-instance", DeploymentID: deployment.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000}); err != nil {
				t.Fatal(err)
			}
			catalogResponse := request(http.MethodGet, "/api/v1/playground/models", "", http.StatusOK)
			var catalog dto.PlaygroundCatalog
			if err := json.Unmarshal(catalogResponse.Body.Bytes(), &catalog); err != nil {
				t.Fatal(err)
			}
			if len(catalog.Models) != 1 || len(catalog.Models[0].Operations) != 1 {
				t.Fatalf("playground catalog = %+v", catalog.Models)
			}
			view := catalog.Models[0].Operations[0]
			if view.Endpoint != operation.Endpoint || view.Mode != operation.Mode || view.RequestEncoding != wantEncoding {
				t.Fatalf("playground did not receive the model's effective contract: %+v", view)
			}
		})
	}
}
