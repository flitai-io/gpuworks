package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// nodeListAnswer posts a literal body to the evaluation and reads back the
// node list as "uuid: eligible" or "uuid: first reason code", with the
// evaluation's errors.
func nodeListAnswer(t *testing.T, h *apiHandlers, body string) (list map[string]string, raw map[string]json.RawMessage) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("evaluate = %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var nodes []struct {
		UUID     string `json:"uuid"`
		Eligible bool   `json:"eligible"`
		Reasons  []struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		} `json:"reasons"`
	}
	if err := json.Unmarshal(raw["eligible_nodes"], &nodes); err != nil {
		t.Fatal(err)
	}
	list = map[string]string{}
	for _, node := range nodes {
		list[node.UUID] = "eligible"
		if node.Eligible {
			continue
		}
		if len(node.Reasons) != 1 {
			t.Fatalf("node %s is turned down for %d reasons: %+v", node.UUID, len(node.Reasons), node.Reasons)
		}
		list[node.UUID] = node.Reasons[0].Code
		// What the node is told is about the node: the planner's own words
		// for a launch ("resolve launch: ...", which names no deployment in
		// a preview) are not it.
		if detail := fmt.Sprint(node.Reasons[0].Params["detail"]); strings.Contains(detail, "resolve launch") {
			t.Fatalf("node %s carries the planner's prefix: %s", node.UUID, detail)
		}
	}
	return list, raw
}

// The node list of a single-node draft says which nodes could ever run it,
// and that does not depend on the placement mode or on which node, if any,
// the draft has now: the form asks it to choose a node, and a list that only
// became right once a node that fits was chosen offered every node until
// then (GWKS-359). A template that states its own tensor-parallel degree is
// the case that used to slip: with no node, or with a node too small, its
// preview failed on a card count nobody chose and the list was left as it
// was. A node the list turns down is one a create naming it refuses, and one
// it offers is created on.
func TestEvaluateNodeListIsTheSameWhicheverNodeTheDraftHas(t *testing.T) {
	const body = `{"name":%q,"model_id":%d,"template_id":%d,"template_spec_source":"upstream","template_spec_name":"upstream","engine":"vllm","placement_mode":%q,"target_vendor":"nvidia","replicas":1,"nnodes":1,"quick_params":{}%s}`
	for _, tc := range []struct {
		name      string
		weightGiB int64
		specArgs  string
		list      map[string]string
	}{
		// 202 GiB as two cards: two 140 GiB cards hold it, 24 GiB cards do
		// not, however many the node has.
		{name: "two-cards-stated-by-the-template", weightGiB: 202, specArgs: `["--tensor-parallel-size","2"]`,
			list: map[string]string{"h20-8x140": "eligible", "rtx3090-8x24": "no_fit", "rtx2080ti-4x22": "no_fit"}},
		// 1404 GiB as two cards: no node holds it.
		{name: "two-cards-that-no-node-holds", weightGiB: 1404, specArgs: `["--tensor-parallel-size","2"]`,
			list: map[string]string{"h20-8x140": "no_fit", "rtx3090-8x24": "no_fit", "rtx2080ti-4x22": "no_fit"}},
		// 100 GiB with no degree stated: eight 24 GiB cards hold it too.
		{name: "no-degree-stated", weightGiB: 100,
			list: map[string]string{"h20-8x140": "eligible", "rtx3090-8x24": "eligible", "rtx2080ti-4x22": "no_fit"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, st := fitParityFleet(t, "node_list_"+tc.name)
			modelID, templateID := fitParityTemplate(t, st, tc.name, tc.weightGiB, tc.specArgs)
			drafts := map[string][2]string{
				"automatic":                {"auto", ""},
				"manual, no node yet":      {"manual", ""},
				"manual on h20-8x140":      {"manual", `,"node_uuid":"h20-8x140"`},
				"manual on rtx3090-8x24":   {"manual", `,"node_uuid":"rtx3090-8x24"`},
				"manual on rtx2080ti-4x22": {"manual", `,"node_uuid":"rtx2080ti-4x22"`},
			}
			for draft, fields := range drafts {
				list, raw := nodeListAnswer(t, h, fmt.Sprintf(body, tc.name, modelID, templateID, fields[0], fields[1]))
				if !reflect.DeepEqual(list, tc.list) {
					t.Fatalf("%s: node list = %v, want %v", draft, list, tc.list)
				}
				// A draft that has no node yet has nothing wrong with it.
				if _, failed := raw["errors"]; failed && draft == "manual, no node yet" {
					t.Fatalf("%s: errors = %s", draft, raw["errors"])
				}
			}
			for node, answer := range tc.list {
				name := tc.name + "-on-" + node
				rec := httptest.NewRecorder()
				h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(fmt.Sprintf(body, name, modelID, templateID, "manual", `,"node_uuid":"`+node+`","served_name":"`+name+`"`))))
				if offered := answer == "eligible"; offered != (rec.Code == http.StatusCreated) || (!offered && rec.Code != http.StatusBadRequest) {
					t.Fatalf("the list says %s is %s, but a create naming it answers %d %s", node, answer, rec.Code, rec.Body.String())
				}
			}
		})
	}
}
