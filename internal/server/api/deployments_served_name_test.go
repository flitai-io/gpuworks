package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A create asking for a served name another deployment holds is refused with
// a code and the holder, both when the name is stated and when it defaults to
// the model's; nothing is written.
func TestCreateDeploymentRefusesAServedNameInUse(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	model, err := st.GetModel(modelID)
	if err != nil {
		t.Fatal(err)
	}
	create := func(name, served string) *httptest.ResponseRecorder {
		t.Helper()
		servedField := ""
		if served != "" {
			servedField = fmt.Sprintf(`"served_name":%q,`, served)
		}
		body := fmt.Sprintf(`{"name":%q,%s"model_id":%d,"engine":"vllm","image":"example/vllm:test","node_uuid":"auto-big","quick_params":{}}`, name, servedField, modelID)
		rec := httptest.NewRecorder()
		h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
		return rec
	}
	if rec := create("first", "qwen3-0.6b-ab12"); rec.Code != http.StatusCreated {
		t.Fatalf("first create = %d %s", rec.Code, rec.Body)
	}
	if rec := create("by-model-name", ""); rec.Code != http.StatusCreated {
		t.Fatalf("create under the model's name = %d %s", rec.Code, rec.Body)
	}
	first, err := st.DeploymentByServedName("qwen3-0.6b-ab12")
	if err != nil {
		t.Fatal(err)
	}
	byModel, err := st.DeploymentByServedName(model.Name)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		served string
		holder *store.Deployment
	}{{"qwen3-0.6b-ab12", first}, {"", byModel}} {
		rec := create("second", tc.served)
		want := tc.served
		if want == "" {
			want = model.Name
		}
		wire := fmt.Sprintf(`{"code":"SERVED_NAME_IN_USE","error":"served name \"%s\" is already used by deployment %s (id %d)","params":{"deployment":%q,"deployment_id":%d,"served_name":%q}}`,
			want, tc.holder.Name, tc.holder.ID, tc.holder.Name, tc.holder.ID, want)
		if rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != wire {
			t.Fatalf("create under %q = %d %s\nwant 409 %s", tc.served, rec.Code, rec.Body, wire)
		}
	}
	rows, err := st.ListDeployments()
	if err != nil || len(rows) != 2 {
		t.Fatalf("deployments after refused creates = %d, %v", len(rows), err)
	}
}

// A copy answers under the original's name with a random four-character
// suffix, so copying twice works; a copy given a name another deployment
// serves under is refused with the code.
func TestCloneDeploymentTakesAFreeServedName(t *testing.T) {
	st := urlModelStore(t, "clone_served_name_in_use")
	original, err := st.CreateDeploymentWithRequirements(&store.Deployment{Name: "original", ServedName: "gemma"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeploymentWithRequirements(&store.Deployment{Name: "taken", ServedName: "taken"}, nil); err != nil {
		t.Fatal(err)
	}
	clone := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", strconv.FormatUint(uint64(original.ID), 10))
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)).WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		(&apiHandlers{d: Deps{Store: st}}).cloneDeployment(rec, req)
		return rec
	}
	shape := regexp.MustCompile(`^gemma-[a-z0-9]{4}$`)
	seen := map[string]bool{}
	for range 2 {
		rec := clone("")
		var got dto.Deployment
		if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &got) != nil || !shape.MatchString(got.ServedName) || seen[got.ServedName] {
			t.Fatalf("clone = %d %s (seen %v)", rec.Code, rec.Body, seen)
		}
		seen[got.ServedName] = true
	}

	rec := clone(`{"name":"taken"}`)
	var out struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict || out.Code != ErrorServedNameInUse || !reflect.DeepEqual(out.Params, map[string]any{"deployment": "taken", "deployment_id": float64(2), "served_name": "taken"}) {
		t.Fatalf("clone onto a held name = %d %s", rec.Code, rec.Body)
	}
}
