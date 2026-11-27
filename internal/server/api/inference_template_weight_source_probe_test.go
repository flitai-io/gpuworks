package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// probeFixture is the source-id fixture with the management node's read of a
// model source replaced by answer, and the reads it made counted.
func probeFixture(t *testing.T, answer *modelmeta.Meta) (*sourceIDFixture, *metadataReads) {
	t.Helper()
	f := newSourceIDFixture(t)
	reads := &metadataReads{}
	signer, _ := auth.NewTokenSigner(t.Name(), 0)
	f.token, _ = managementSessionTestToken(t, f.st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	f.router = Router(Deps{Store: f.st, AuthSigner: signer, FetchModelMeta: reads.hook(answer)})
	return f, reads
}

func probePath(templateID uint, source map[string]string) string {
	query := url.Values{"template_id": {fmt.Sprint(templateID)}, "spec_source": {"upstream"}, "spec_name": {"fp8-balanced"}}
	for key, value := range source {
		query.Set(key, value)
	}
	return "/api/v1/inference-templates/weight-source-probe?" + query.Encode()
}

const probeSources = `[{"provider":"huggingface","repo_id":"inclusionAI/Ling-3.0-flash","revision":"42766a81"},{"provider":"modelscope","repo_id":"inclusionAI/Ling-3.0-flash","revision":"9c3c2a34"}]`

var (
	probeHF = map[string]string{"provider": "huggingface", "repo_id": "inclusionAI/Ling-3.0-flash", "revision": "42766a81"}
	probeMS = map[string]string{"provider": "modelscope", "repo_id": "inclusionAI/Ling-3.0-flash", "revision": "9c3c2a34"}
)

// The answer is the server's judgement of one declared source: proceed when
// the management node read the repository there, and otherwise the reason,
// with what the site said.
func TestWeightSourceProbeAnswersWhetherTheSourceServesTheRepository(t *testing.T) {
	for name, tc := range map[string]struct {
		answer modelmeta.Meta
		source map[string]string
		want   string
	}{
		"served": {modelmeta.Meta{ConfigRaw: `{"model_type":"ling"}`, Endpoint: "https://modelscope.cn"}, probeMS,
			`{"proceed":true,"host":"modelscope.cn"}`},
		"site unreachable": {modelmeta.Meta{Answer: modelmeta.Answer{Outcome: modelmeta.OutcomeUnreachable, Endpoint: "https://huggingface.co"}}, probeHF,
			`{"proceed":false,"reason":{"code":"MODEL_METADATA_UNREACHABLE","params":{"hosts":"huggingface.co","route":"/api/v1/pull/source-rules","source":"huggingface"},"message":"the management node reached none of huggingface.co"}}`},
		"no such repository": {modelmeta.Meta{Answer: modelmeta.Answer{Outcome: modelmeta.OutcomeRepoNotFound, Endpoint: "https://modelscope.cn", Status: 404}}, probeMS,
			`{"proceed":false,"reason":{"code":"MODEL_REPO_NOT_FOUND","params":{"host":"modelscope.cn","repo":"inclusionAI/Ling-3.0-flash","source":"modelscope","status":404},"message":"modelscope.cn answered 404 for inclusionAI/Ling-3.0-flash"}}`},
		"no such revision": {modelmeta.Meta{Answer: modelmeta.Answer{Outcome: modelmeta.OutcomeRevisionNotFound, Endpoint: "https://modelscope.cn", Status: 404}}, probeMS,
			`{"proceed":false,"reason":{"code":"MODEL_REVISION_NOT_FOUND","params":{"host":"modelscope.cn","repo":"inclusionAI/Ling-3.0-flash","revision":"9c3c2a34","source":"modelscope","status":404},"message":"modelscope.cn answered 404 for inclusionAI/Ling-3.0-flash"}}`},
		"needs a token": {modelmeta.Meta{Answer: modelmeta.Answer{Outcome: modelmeta.OutcomeDenied, Endpoint: "https://huggingface.co", Status: 401}}, probeHF,
			`{"proceed":false,"reason":{"code":"MODEL_REPO_ACCESS_DENIED","params":{"host":"huggingface.co","repo":"inclusionAI/Ling-3.0-flash","source":"huggingface","status":401},"message":"huggingface.co answered 401 for inclusionAI/Ling-3.0-flash"}}`},
	} {
		answer := tc.answer
		f, reads := probeFixture(t, &answer)
		templateID := weightModelsTemplate(t, f, probeSources)
		rec := f.get(probePath(templateID, tc.source), true)
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != tc.want {
			t.Errorf("%s: probe = %d %s\nwant %s", name, rec.Code, rec.Body.String(), tc.want)
		}
		if len(reads.calls) != 1 {
			t.Errorf("%s: %d reads of the source, want one", name, len(reads.calls))
		}
		_ = f.st.Close()
	}
}

// A source whose choice reuses a registered model with files in place may be
// deployed whatever the site says, and the site is not asked: an offline
// site deploys what it already holds. A registered model without files does
// not count, nor does one of the other provider (choosing this source would
// register a new model and download it).
func TestWeightSourceProbeLetsFilesInPlaceProceedWithoutAskingTheSite(t *testing.T) {
	unreachable := modelmeta.Meta{Answer: modelmeta.Answer{Outcome: modelmeta.OutcomeUnreachable, Endpoint: "https://huggingface.co"}}
	f, reads := probeFixture(t, &unreachable)
	templateID := weightModelsTemplate(t, f, probeSources)
	node, err := f.st.RegisterNode("weights-node", store.HashToken("weights-token"), "weights-node")
	if err != nil {
		t.Fatal(err)
	}
	place := func(model *store.Model, state string) {
		t.Helper()
		if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: state, Path: fmt.Sprintf("/models/%d", model.ID)}); err != nil {
			t.Fatal(err)
		}
	}
	place(f.model(&store.Model{Source: "modelscope", SourceID: "inclusionAI/Ling-3.0-flash", Revision: "9c3c2a34"}), proto.MFPresent)

	rec := f.get(probePath(templateID, probeHF), true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"proceed":false`) || len(reads.calls) != 1 {
		t.Fatalf("the other provider's files let Hugging Face proceed: %d %s (%d reads)", rec.Code, rec.Body.String(), len(reads.calls))
	}
	rec = f.get(probePath(templateID, probeMS), true)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"proceed":true,"present":true}` || len(reads.calls) != 1 {
		t.Fatalf("files in place = %d %s (%d reads)", rec.Code, rec.Body.String(), len(reads.calls))
	}

	place(f.model(&store.Model{Source: "huggingface", SourceID: "inclusionAI/Ling-3.0-flash", Revision: "42766a81"}), proto.MFDownloading)
	rec = f.get(probePath(templateID, probeHF), true)
	if !strings.Contains(rec.Body.String(), `"proceed":false`) || len(reads.calls) != 2 {
		t.Fatalf("a model still downloading let the source proceed unasked: %s (%d reads)", rec.Body.String(), len(reads.calls))
	}
}

func TestWeightSourceProbeRefusals(t *testing.T) {
	f, reads := probeFixture(t, &modelmeta.Meta{ConfigRaw: `{}`})
	templateID := weightModelsTemplate(t, f, probeSources)
	if rec := f.get(probePath(templateID, probeMS), false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d", rec.Code)
	}
	for name, tc := range map[string]struct {
		path   string
		status int
		code   string
	}{
		"no provider":        {probePath(templateID, map[string]string{"repo_id": "inclusionAI/Ling-3.0-flash"}), http.StatusBadRequest, "WEIGHT_SOURCE_PROBE_QUERY_INVALID"},
		"no template":        {strings.Replace(probePath(templateID, probeMS), "template_id=", "template=", 1), http.StatusBadRequest, "WEIGHT_SOURCE_PROBE_QUERY_INVALID"},
		"undeclared repo":    {probePath(templateID, map[string]string{"provider": "modelscope", "repo_id": "zai-org/glm-5", "revision": "9c3c2a34"}), http.StatusBadRequest, "WEIGHT_SOURCE_NOT_DECLARED"},
		"undeclared version": {probePath(templateID, map[string]string{"provider": "modelscope", "repo_id": "inclusionAI/Ling-3.0-flash", "revision": "master"}), http.StatusBadRequest, "WEIGHT_SOURCE_NOT_DECLARED"},
		"unknown spec":       {strings.Replace(probePath(templateID, probeMS), "fp8-balanced", "missing", 1), http.StatusNotFound, ""},
	} {
		rec := f.get(tc.path, true)
		if rec.Code != tc.status || (tc.code != "" && !strings.Contains(rec.Body.String(), `"code":"`+tc.code+`"`)) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if len(reads.calls) != 0 {
		t.Fatalf("%d reads of a source by refused requests", len(reads.calls))
	}
}
