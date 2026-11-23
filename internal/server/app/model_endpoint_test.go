package app

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// --model-endpoint becomes one site rule per hub that never reaches the hub;
// running it again adds nothing, and a hub the operator already wrote a rule
// for keeps it.
func TestModelEndpointBecomesSiteRulesOnce(t *testing.T) {
	st, err := store.Open("file:app_model_endpoint_rules?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	operator, err := st.CreatePullSourceRule(store.PullSourceRuleSpec{Match: "https://modelscope.cn", Endpoints: []string{"https://ms.corp"}, AllowUpstream: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := ensureModelEndpointRules(st, " http://127.0.0.1:8090/ ", false); err != nil {
			t.Fatal(err)
		}
	}
	rules, err := st.PullSourceRules()
	if err != nil || len(rules) != 2 {
		t.Fatalf("rules = %+v, %v", rules, err)
	}
	if rules[0].ID != operator.ID || rules[0].Endpoints[0] != "https://ms.corp" {
		t.Fatalf("the operator's ModelScope rule changed: %+v", rules[0])
	}
	hf := rules[1]
	if hf.Match != "https://huggingface.co" || !reflect.DeepEqual(hf.Endpoints, []string{"http://127.0.0.1:8090"}) || hf.AllowUpstream || !hf.ControlPlane || len(hf.Requirements) != 0 {
		t.Fatalf("hugging face rule = %+v", hf)
	}
	// An ordinary server preserves even its original generated rule. Only the
	// experience console rewires it to the next offline hub's listening port.
	const next = "http://127.0.0.1:8091"
	for _, experience := range []bool{false, true} {
		if err := ensureModelEndpointRules(st, next, experience); err != nil {
			t.Fatal(err)
		}
		nextRules, err := st.PullSourceRules()
		if err != nil {
			t.Fatal(err)
		}
		if len(nextRules) != 2 || !reflect.DeepEqual(nextRules[0], rules[0]) {
			t.Fatalf("operator rule or count changed: %+v", nextRules)
		}
		want := hf
		if experience {
			want.Endpoints = []string{next}
		}
		if !reflect.DeepEqual(nextRules[1], want) {
			t.Fatalf("experience %t, got %+v, want %+v", experience, nextRules[1], want)
		}
	}
	if err := ensureModelEndpointRules(st, "hf-mirror.example", false); err == nil {
		t.Fatal("an endpoint that is not a URL was accepted")
	}
}
