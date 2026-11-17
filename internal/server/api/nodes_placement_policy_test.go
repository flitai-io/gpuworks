package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type placementPolicyAPI struct {
	acceleratorAPI
}

func newPlacementPolicyAPI(t *testing.T) *placementPolicyAPI {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, token := roleTestToken(t, st, "admin")
	a := &placementPolicyAPI{acceleratorAPI{t: t, st: st, token: token}}
	a.router = Router(Deps{Store: st, AuthSigner: signer})
	if _, err := st.RegisterNode("policy", store.HashToken("policy"), "policy"); err != nil {
		t.Fatal(err)
	}
	return a
}

// The node's placement policy and whether each knob takes effect ride on
// the node DTO, keyed as the dictionary keys them.
func TestNodeDTOCarriesThePlacementPolicyOnTheWire(t *testing.T) {
	a := newPlacementPolicyAPI(t)
	status, _, raw := a.do(a.token, http.MethodGet, "/api/v1/nodes/policy", "")
	if status != http.StatusOK {
		t.Fatalf("get node: %d %s", status, raw)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		t.Fatal(err)
	}
	if got := string(wire["placement_policy"]); got != `{"share_with_foreign":false}` {
		t.Fatalf("placement_policy = %s", got)
	}
	// A node with no partition backend: the switch has no effect there,
	// and the reason says why.
	if got := string(wire["placement_policy_availability"]); got != `[{"key":"share_with_foreign","applicable":false,"reason":"NODE_PLACEMENT_POLICY_NO_PARTITION_BACKEND"}]` {
		t.Fatalf("placement_policy_availability = %s", got)
	}
}

func TestPatchNodePlacementPolicy(t *testing.T) {
	a := newPlacementPolicyAPI(t)
	sub := a.st.Bus.Subscribe(8, "node")
	defer sub.Close()
	// Not applicable on this node, and written all the same: an operator
	// may set it before the agent or backend is ready, or clear it after.
	for _, value := range []string{"true", "false", "true"} {
		status, out, raw := a.do(a.token, http.MethodPatch, "/api/v1/nodes/policy/placement-policy", `{"share_with_foreign":`+value+`}`)
		if status != http.StatusOK {
			t.Fatalf("patch %s: %d %s", value, status, raw)
		}
		if got := out["placement_policy"]; !reflect.DeepEqual(got, map[string]any{"share_with_foreign": value == "true"}) {
			t.Fatalf("patch %s: placement_policy = %v", value, got)
		}
		if ev := <-sub.C(); ev.Key != "policy" || ev.Reason != store.NodeChangePlacementPolicy {
			t.Fatalf("event = %+v", ev)
		}
		if availability, _ := out["placement_policy_availability"].([]any); len(availability) != 1 || availability[0].(map[string]any)["applicable"] != false {
			t.Fatalf("availability = %v", out["placement_policy_availability"])
		}
	}
	if node, _ := a.st.NodeByUUID("policy"); !node.PlacementPolicy.ShareWithForeign {
		t.Fatal("the switch was not stored")
	}
	// An empty patch changes nothing and is not an error.
	if status, out, raw := a.do(a.token, http.MethodPatch, "/api/v1/nodes/policy/placement-policy", `{}`); status != http.StatusOK || out["placement_policy"].(map[string]any)["share_with_foreign"] != true {
		t.Fatalf("empty patch: %d %s", status, raw)
	}
}

func TestPatchNodePlacementPolicyRefusals(t *testing.T) {
	a := newPlacementPolicyAPI(t)
	for _, tc := range []struct {
		name, body string
		status     int
		field      string
	}{
		{"unknown key", `{"reticulate":true}`, http.StatusBadRequest, "reticulate"},
		{"string value", `{"share_with_foreign":"yes"}`, http.StatusBadRequest, "share_with_foreign"},
		{"null value", `{"share_with_foreign":null}`, http.StatusBadRequest, "share_with_foreign"},
		{"not an object", `[true]`, http.StatusBadRequest, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, out, raw := a.do(a.token, http.MethodPatch, "/api/v1/nodes/policy/placement-policy", tc.body)
			if status != tc.status {
				t.Fatalf("status %d: %s", status, raw)
			}
			if tc.field != "" {
				params, _ := out["params"].(map[string]any)
				if out["code"] != ErrorFieldInvalid || params["field"] != "placement_policy."+tc.field {
					t.Fatalf("refusal = %s", raw)
				}
			}
		})
	}
	if node, _ := a.st.NodeByUUID("policy"); node.PlacementPolicy.ShareWithForeign {
		t.Fatal("a refused patch was stored")
	}
	if status, _, raw := a.do(a.token, http.MethodPatch, "/api/v1/nodes/missing/placement-policy", `{"share_with_foreign":true}`); status != http.StatusNotFound {
		t.Fatalf("missing node: %d %s", status, raw)
	}
	if status, _, _ := a.do("", http.MethodPatch, "/api/v1/nodes/policy/placement-policy", `{"share_with_foreign":true}`); status != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", status)
	}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPost} {
		if status, _, _ := a.do(a.token, method, "/api/v1/nodes/policy/placement-policy", `{}`); status != http.StatusMethodNotAllowed {
			t.Fatalf("%s: %d", method, status)
		}
	}
}

// The dictionary the UI renders the policy from: every knob's key, kind and
// default, in display order.
func TestNodePlacementPolicyDictionaryWire(t *testing.T) {
	a := newPlacementPolicyAPI(t)
	status, _, raw := a.do(a.token, http.MethodGet, "/api/v1/scheduling/node-placement-policies", "")
	if status != http.StatusOK || strings.TrimSpace(raw) != `{"items":[{"key":"share_with_foreign","kind":"bool","default":false}],"total":1}` {
		t.Fatalf("dictionary: %d %s", status, raw)
	}
	if status, _, _ := a.do("", http.MethodGet, "/api/v1/scheduling/node-placement-policies", ""); status != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", status)
	}
	if status, _, _ := a.do(a.token, http.MethodPost, "/api/v1/scheduling/node-placement-policies", ""); status != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", status)
	}
}
