package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func metadataSourcesStore(t *testing.T, name string) *store.Store {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

type metadataReads struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *metadataReads) hook(meta *modelmeta.Meta) func(context.Context, string, string, string, string, []string) (*modelmeta.Meta, error) {
	return func(_ context.Context, _, _, _, _ string, candidates []string) (*modelmeta.Meta, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls = append(r.calls, candidates)
		copied := *meta
		return &copied, nil
	}
}

func registerModelWire(t *testing.T, h *apiHandlers, body string) dto.Model {
	t.Helper()
	rec := httptest.NewRecorder()
	h.createModel(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	var wire dto.Model
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

// The management node reads a model's source at the addresses of the site
// rules chosen for it, in rule order and whatever nodes they select; a rule
// not chosen for it is not its business.
func TestRegistrationReadsThroughTheRulesChosenForTheManagementNode(t *testing.T) {
	st := metadataSourcesStore(t, "api_metadata_control_plane_rules")
	for _, spec := range []store.PullSourceRuleSpec{
		{Match: "https://huggingface.co", Endpoints: []string{"http://hf-proxy.idc:8090"}, AllowUpstream: false, Requirements: nodelabel.Requirements{"site": {"idc"}}},
		{Match: "https://huggingface.co", Endpoints: []string{"https://hf-mirror.com"}, AllowUpstream: true, ControlPlane: true, Requirements: nodelabel.Requirements{"site": {"office"}}},
	} {
		if _, err := st.CreatePullSourceRule(spec); err != nil {
			t.Fatal(err)
		}
	}
	reads := &metadataReads{}
	h := &apiHandlers{d: Deps{Store: st, FetchModelMeta: reads.hook(&modelmeta.Meta{WeightBytes: 1 << 30, ConfigRaw: `{"model_type":"qwen3"}`})}}
	wire := registerModelWire(t, h, `{"source":"huggingface","source_id":"org/model","revision":"main"}`)
	if want := [][]string{{"https://hf-mirror.com", "https://huggingface.co"}}; !reflect.DeepEqual(reads.calls, want) {
		t.Fatalf("reads = %v, want %v", reads.calls, want)
	}
	if wire.MetadataNotice != nil || wire.WeightGiB != 1 {
		t.Fatalf("registration = %+v", wire)
	}
}

// A registration that reads nothing still registers the model and says why,
// pointing at the rules: either the management node reached none of the
// addresses, or its rules forbid them all.
func TestRegistrationWithoutMetadataSaysWhy(t *testing.T) {
	st := metadataSourcesStore(t, "api_metadata_notice")
	if _, err := st.CreatePullSourceRule(store.PullSourceRuleSpec{Match: "https://huggingface.co", Endpoints: []string{"https://hf-mirror.com"}, AllowUpstream: true, ControlPlane: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePullSourceRule(store.PullSourceRuleSpec{Match: "https://modelscope.cn", AllowUpstream: false, ControlPlane: true}); err != nil {
		t.Fatal(err)
	}
	reads := &metadataReads{}
	h := &apiHandlers{d: Deps{Store: st, FetchModelMeta: reads.hook(&modelmeta.Meta{})}}

	unreachable := registerModelWire(t, h, `{"source":"huggingface","source_id":"org/model","revision":"main"}`)
	want := &dto.ProductMessage{Code: "MODEL_METADATA_UNREACHABLE", Message: "the management node reached none of hf-mirror.com, huggingface.co",
		Params: map[string]any{"source": "huggingface", "hosts": "hf-mirror.com, huggingface.co", "route": "/api/v1/pull/source-rules"}}
	if !reflect.DeepEqual(unreachable.MetadataNotice, want) || unreachable.ID == 0 {
		t.Fatalf("unreachable registration = %+v, notice %+v", unreachable, unreachable.MetadataNotice)
	}

	forbidden := registerModelWire(t, h, `{"source":"modelscope","source_id":"org/model","revision":"master"}`)
	if forbidden.MetadataNotice == nil || forbidden.MetadataNotice.Code != "MODEL_METADATA_FORBIDDEN" || forbidden.MetadataNotice.Params["source"] != "modelscope" {
		t.Fatalf("forbidden registration notice = %+v", forbidden.MetadataNotice)
	}
	if len(reads.calls) != 1 {
		t.Fatalf("reads = %v: a forbidden source must not be read at all", reads.calls)
	}

	// The notice is the registration's answer, not a property of the model.
	rec := httptest.NewRecorder()
	h.listModels(rec, httptest.NewRequest(http.MethodGet, "/api/v1/models", nil))
	if strings.Contains(rec.Body.String(), "metadata_notice") {
		t.Fatalf("model list carries a notice: %s", rec.Body.String())
	}
}

// A refresh that reads nothing is a failure, with the same reason.
func TestRefreshThatReadsNothingSaysWhy(t *testing.T) {
	st := metadataSourcesStore(t, "api_metadata_refresh_notice")
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/model", Revision: "main", Name: "m"})
	if err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st, FetchModelMeta: (&metadataReads{}).hook(&modelmeta.Meta{})}}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", fmt.Sprint(model.ID))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models/1/metadata/refresh", strings.NewReader(`{}`))
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.refreshModelMetadata(rec, req)
	var body struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadGateway || body.Code != "MODEL_METADATA_UNREACHABLE" || body.Params["hosts"] != "huggingface.co" {
		t.Fatalf("refresh = %d %s", rec.Code, rec.Body.String())
	}
}

// The console's browser reads metadata at the addresses the rules chosen for
// the management node give, and at none when they forbid every one.
func TestMetadataSourcesFollowTheManagementNodesRules(t *testing.T) {
	rig := newPullRig(t)
	for _, body := range []string{
		`{"match":"https://huggingface.co","endpoints":["https://hf-mirror.com"],"allow_upstream":true,"control_plane":true}`,
		`{"match":"https://huggingface.co","endpoints":["http://hf-proxy.idc:8090"],"allow_upstream":true,"node_requirements":{"site":["idc"]}}`,
		`{"match":"https://modelscope.cn","endpoints":[],"allow_upstream":false,"control_plane":true}`,
	} {
		if code, text := rig.do(rig.admin, "POST", "/api/v1/pull/source-rules", body); code != 201 {
			t.Fatalf("create rule = %d %s", code, text)
		}
	}
	rig.want("GET", "/api/v1/pull/metadata-sources?source=huggingface", "", 200, `{"source":"huggingface","bases":["https://hf-mirror.com","https://huggingface.co"]}`)
	rig.want("GET", "/api/v1/pull/metadata-sources?source=modelscope", "", 200, `{"source":"modelscope","bases":[]}`)
	if code, _ := rig.do(rig.member, "GET", "/api/v1/pull/metadata-sources?source=huggingface", ""); code != 200 {
		t.Fatalf("a signed-in member = %d", code)
	}
	if code, body := rig.do(rig.admin, "GET", "/api/v1/pull/metadata-sources?source=url", ""); code != 400 || !strings.Contains(body, `"field":"source"`) {
		t.Fatalf("source=url = %d %s", code, body)
	}
}
