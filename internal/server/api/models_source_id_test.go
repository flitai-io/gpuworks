package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type sourceIDFixture struct {
	t      *testing.T
	st     *store.Store
	router http.Handler
	token  string
}

func newSourceIDFixture(t *testing.T) *sourceIDFixture {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := auth.NewTokenSigner(t.Name(), time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	return &sourceIDFixture{t: t, st: st, router: Router(Deps{Store: st, AuthSigner: signer}), token: token}
}

func (f *sourceIDFixture) do(method string, id uint, body string, authorized bool) *httptest.ResponseRecorder {
	f.t.Helper()
	request := httptest.NewRequest(method, "/api/v1/models/"+strconv.Itoa(int(id)), strings.NewReader(body))
	if authorized {
		request.Header.Set("Authorization", "Bearer "+f.token)
	}
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, request)
	return recorder
}

func (f *sourceIDFixture) model(m *store.Model) *store.Model {
	f.t.Helper()
	created, err := f.st.UpsertModel(m)
	if err != nil {
		f.t.Fatal(err)
	}
	return created
}

func sourceIDErrorCode(t *testing.T, rec *httptest.ResponseRecorder) (string, map[string]any) {
	t.Helper()
	var body struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body %s: %v", rec.Body.String(), err)
	}
	return body.Code, body.Params
}

func TestUpdateModelSourceIDRenamesALocalModelAndPublishesAfterCommit(t *testing.T) {
	f := newSourceIDFixture(t)
	model := f.model(&store.Model{Source: "local", SourceID: "Qwen3-0.6B", Revision: "local", Name: "qwen"})
	node, err := f.st.RegisterNode("rename-node", store.HashToken("rename-token"), "rename-node")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/data/Qwen3-0.6B"}); err != nil {
		t.Fatal(err)
	}
	before := f.st.Bus.Generation("model")

	rec := f.do(http.MethodPatch, model.ID, `{"source_id":"  /Qwen/Qwen3-0.6B/ "}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	if string(wire["source_id"]) != `"Qwen/Qwen3-0.6B"` || string(wire["source_id_editable"]) != `true` || string(wire["source"]) != `"local"` {
		t.Fatalf("wire = %s", rec.Body.String())
	}
	stored, _ := f.st.GetModel(model.ID)
	if stored.SourceID != "Qwen/Qwen3-0.6B" || stored.Revision != "local" || stored.Name != "qwen" {
		t.Fatalf("stored = %+v", stored)
	}
	if got := f.st.Bus.Generation("model"); got != before+1 {
		t.Fatalf("model generation = %d, want %d", got, before+1)
	}
	// The copy already on disk keeps its recorded path.
	files, err := f.st.ModelFilesForModel(model.ID)
	if err != nil || len(files) != 1 || files[0].Path != "/data/Qwen3-0.6B" {
		t.Fatalf("files = %+v err = %v", files, err)
	}

	// Setting the same id again changes nothing and publishes nothing.
	if rec := f.do(http.MethodPatch, model.ID, `{"source_id":"Qwen/Qwen3-0.6B"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("no-op status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := f.st.Bus.Generation("model"); got != before+1 {
		t.Fatalf("no-op published: generation = %d", got)
	}
}

func TestUpdateModelSourceIDRefusals(t *testing.T) {
	f := newSourceIDFixture(t)
	local := f.model(&store.Model{Source: "local", SourceID: "dir-a", Revision: "local"})
	other := f.model(&store.Model{Source: "local", SourceID: "Acme/Taken", Revision: "local"})
	public := f.model(&store.Model{Source: "huggingface", SourceID: "Acme/Public", Revision: "main"})
	busy := f.model(&store.Model{Source: "url", SourceID: "busy", Revision: "main", SourceURL: "https://models.example/busy"})
	node, err := f.st.RegisterNode("busy-node", store.HashToken("busy-token"), "busy-node")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: busy.ID, NodeID: node.ID, State: proto.MFDownloading, Path: "url/busy-main"}); err != nil {
		t.Fatal(err)
	}
	before := f.st.Bus.Generation("model")

	for _, tc := range []struct {
		name   string
		id     uint
		body   string
		status int
		code   string
	}{
		{"empty", local.ID, `{"source_id":"  / "}`, http.StatusBadRequest, ErrorModelSourceIDInvalid},
		{"missing", local.ID, `{}`, http.StatusBadRequest, ErrorModelSourceIDInvalid},
		{"unknown field", local.ID, `{"source_id":"x","name":"y"}`, http.StatusBadRequest, ErrorModelSourceIDInvalid},
		{"malformed", local.ID, `{"source_id":`, http.StatusBadRequest, ErrorModelSourceIDInvalid},
		{"control character", local.ID, `{"source_id":"a\u0007b"}`, http.StatusBadRequest, ErrorModelSourceIDInvalid},
		{"too long", local.ID, `{"source_id":"` + strings.Repeat("a", 256) + `"}`, http.StatusBadRequest, ErrorModelSourceIDInvalid},
		{"public repository", public.ID, `{"source_id":"Acme/Else"}`, http.StatusBadRequest, ErrorModelSourceIDFixed},
		{"taken ignoring case", local.ID, `{"source_id":"acme/taken"}`, http.StatusConflict, ErrorModelSourceIDTaken},
		{"transfer in flight", busy.ID, `{"source_id":"renamed"}`, http.StatusConflict, ErrorModelTransferActive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(http.MethodPatch, tc.id, tc.body, true)
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			code, params := sourceIDErrorCode(t, rec)
			if code != tc.code {
				t.Fatalf("code=%s body=%s", code, rec.Body.String())
			}
			if tc.code == ErrorModelSourceIDTaken && params["model_id"] != float64(other.ID) {
				t.Fatalf("params=%v, want model_id %d", params, other.ID)
			}
		})
	}
	if rec := f.do(http.MethodPatch, 9999, `{"source_id":"x"}`, true); rec.Code != http.StatusNotFound {
		t.Fatalf("missing model status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := f.do(http.MethodPatch, local.ID, `{"source_id":"x"}`, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := f.do(http.MethodPut, local.ID, `{"source_id":"x"}`, true); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, m := range []*store.Model{local, public, busy} {
		stored, _ := f.st.GetModel(m.ID)
		if stored.SourceID != m.SourceID {
			t.Fatalf("refused change was applied: %+v", stored)
		}
	}
	if got := f.st.Bus.Generation("model"); got != before {
		t.Fatalf("refusals published model events: generation %d -> %d", before, got)
	}
}

func TestModelDTOMarksOnlyLocalAndURLSourceIDsEditable(t *testing.T) {
	h := &apiHandlers{}
	for source, want := range map[string]bool{"local": true, "url": true, "LOCAL": true, "huggingface": false, "modelscope": false} {
		raw, err := json.Marshal(h.modelDTO(&store.Model{Source: source, SourceID: "a"}, false))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(raw), `"source_id_editable":true`); got != want {
			t.Fatalf("source %s: wire %s", source, raw)
		}
		if !want && strings.Contains(string(raw), "source_id_editable") {
			t.Fatalf("source %s: an uneditable id must omit the key: %s", source, raw)
		}
	}
}
