package app_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// Exercise the real management key creation path: assigning a hand-written
// UserID to the key fixture would bypass the identity mismatch in GWKS-317.
func TestConsoleBatchesAreReadableByTheirUsersHTTPKeys(t *testing.T) {
	env := testenv.New(t)
	env.AddNode("batch-owner", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "batch-owner")
	st := env.Store()
	nodes, err := st.ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("fixture nodes=%d err=%v", len(nodes), err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "zstackai/qwen3-4b-fake", Revision: "main", Name: "owner-model", Arch: "qwen3", WeightBytes: 1 << 30,
		Operations: `[{"type":"openai.chat","endpoint":"/v1/chat/completions"}]`,
		Meta:       `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "fake")}); err != nil {
		t.Fatal(err)
	}
	admin := env.API(t)
	admin.Client.Timeout = 15 * time.Second
	body, _ := json.Marshal(map[string]any{"model_id": model.ID, "node_uuid": nodes[0].UUID, "served_name": "owner-model", "quick_params": map[string]any{"max_model_len": 512}})
	res, err := admin.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	var dep struct {
		ID uint `json:"id"`
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusCreated || json.Unmarshal(raw, &dep) != nil || dep.ID == 0 {
		t.Fatalf("create deployment=%d %s", res.StatusCode, raw)
	}
	env.Eventually(t, 25*time.Second, func() bool {
		instances, _ := st.InstancesForDeployment(dep.ID)
		for _, in := range instances {
			if in.State == proto.InstRunning {
				return true
			}
		}
		return false
	})
	otherDep, err := st.CreateDeployment(&store.Deployment{Name: "other-model", ServedName: "other-model"})
	if err != nil {
		t.Fatal(err)
	}
	key := ownershipHTTPKey(t, admin, `{"name":"owner-key"}`)
	second := ownershipHTTPKey(t, admin, `{"name":"second-owner-key"}`)
	restricted := ownershipHTTPKey(t, admin, fmt.Sprintf(`{"name":"restricted-owner-key","all_deployments":false,"deployment_ids":[%d]}`, otherDep.ID))
	owner, err := st.AuthenticateUser("admin", testenv.AdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := st.ResolveAPIKey(key)
	if !ok || identity.UserID != owner.ID {
		t.Errorf("HTTP-created key owner=%d valid=%v, want current user %d", identity.UserID, ok, owner.ID)
	}

	input := `{"custom_id":"owned","method":"POST","url":"/v1/chat/completions","body":{"model":"owner-model","messages":[{"role":"user","content":"hello"}]}}` + "\n"
	batchRaw := ownershipUpload(t, admin, "/api/v1/batches", input)
	var created struct {
		ID    string `json:"id"`
		Input string `json:"input_file_id"`
	}
	if json.Unmarshal(batchRaw, &created) != nil || created.ID == "" || created.Input == "" {
		t.Fatalf("console batch=%s", batchRaw)
	}
	var output string
	env.Eventually(t, 10*time.Second, func() bool {
		row, err := st.GetBatch(created.ID)
		if err == nil && row.Status == store.BatchCompleted {
			output = row.OutputFileID
			return output != ""
		}
		return false
	})
	for _, sameUserKey := range []string{key, second, restricted} {
		client := &testenv.APIClient{Client: admin.Client, BaseURL: admin.BaseURL, Token: sameUserKey}
		for _, path := range []string{"/v1/batches/" + created.ID, "/v1/files/" + created.Input, "/v1/files/" + created.Input + "/content", "/v1/files/" + output + "/content"} {
			code, raw := ownershipGet(t, client, path)
			if code != http.StatusOK {
				t.Errorf("same-user key GET %s=%d %s", path, code, raw)
			}
		}
		code, raw := ownershipGet(t, client, "/v1/batches")
		if code != http.StatusOK || !bytes.Contains(raw, []byte(created.ID)) {
			t.Errorf("same-user batch listing=%d %s", code, raw)
		}
	}
	// Reading owned history does not grant the restricted key an execution
	// scope it did not have. Scope continues to apply at the gateway.
	client := &testenv.APIClient{Client: admin.Client, BaseURL: admin.BaseURL, Token: restricted}
	resp, err := client.PostJSON("/v1/chat/completions", []byte(`{"model":"owner-model","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !bytes.Contains(raw, []byte("MODEL_ACCESS_DENIED")) {
		t.Errorf("restricted key execution=%d %s", resp.StatusCode, raw)
	}
	// Submitting another owned input still runs each request with the new
	// batch's restricted key; reading the input does not widen that scope.
	resp, err = client.PostJSON("/v1/batches", []byte(fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"24h"}`, created.Input)))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var restrictedBatch struct {
		ID string `json:"id"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &restrictedBatch) != nil || restrictedBatch.ID == "" {
		t.Fatalf("restricted batch submission=%d %s", resp.StatusCode, raw)
	}
	var errorFile string
	env.Eventually(t, 10*time.Second, func() bool {
		row, err := st.GetBatch(restrictedBatch.ID)
		if err == nil && row.Status == store.BatchCompleted && row.Failed == 1 {
			errorFile = row.ErrorFileID
			return errorFile != ""
		}
		return false
	})
	if code, raw := ownershipGet(t, client, "/v1/files/"+errorFile+"/content"); code != http.StatusOK || !bytes.Contains(raw, []byte("MODEL_ACCESS_DENIED")) {
		t.Errorf("restricted batch execution=%d %s", code, raw)
	}

	other := store.User{Username: "other-owner", PasswordHash: owner.PasswordHash, Role: "user"}
	if err := st.DB().Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	otherClient := ownershipLogin(t, admin, other.Username)
	otherKey := ownershipHTTPKey(t, otherClient, `{"name":"other-user-key"}`)
	if err := st.SetSecurityPolicy(store.SecurityPolicy{InferenceOpenAccess: true}); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{otherKey, ""} {
		client := &testenv.APIClient{Client: admin.Client, BaseURL: admin.BaseURL, Token: token}
		for _, path := range []string{"/v1/batches/" + created.ID, "/v1/files/" + created.Input, "/v1/files/" + output + "/content"} {
			if code, raw := ownershipGet(t, client, path); code != http.StatusNotFound {
				t.Errorf("other/anonymous GET %s=%d %s", path, code, raw)
			}
		}
		if _, raw := ownershipGet(t, client, "/v1/batches"); bytes.Contains(raw, []byte(created.ID)) {
			t.Errorf("other/anonymous lists private batch: %s", raw)
		}
	}
	checkHistoricalBatchKeyIsolation(t, env, admin, input)
}

func ownershipHTTPKey(t *testing.T, client *testenv.APIClient, body string) string {
	t.Helper()
	resp, err := client.PostJSON("/api/v1/apikeys", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Key string `json:"key"`
	}
	if resp.StatusCode != http.StatusCreated || json.Unmarshal(raw, &out) != nil || out.Key == "" {
		t.Fatalf("create personal key=%d %s", resp.StatusCode, raw)
	}
	return out.Key
}

func ownershipUpload(t *testing.T, client *testenv.APIClient, path, contents string) []byte {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("purpose", "batch"); err != nil {
		t.Fatal(err)
	}
	part, err := form.CreateFormFile("file", "owned.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(part, strings.NewReader(contents)); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload=%d %s", resp.StatusCode, raw)
	}
	return raw
}

func ownershipGet(t *testing.T, client *testenv.APIClient, path string) (int, []byte) {
	t.Helper()
	resp, err := client.Get(path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func ownershipLogin(t *testing.T, base *testenv.APIClient, user string) *testenv.APIClient {
	t.Helper()
	client := &testenv.APIClient{Client: base.Client, BaseURL: base.BaseURL}
	resp, err := client.PostJSON("/api/v1/auth/login", []byte(fmt.Sprintf(`{"username":%q,"password":%q}`, user, testenv.AdminPassword)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Token string `json:"token"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&body) != nil || body.Token == "" {
		t.Fatal("other user login failed")
	}
	client.Token = body.Token
	return client
}
