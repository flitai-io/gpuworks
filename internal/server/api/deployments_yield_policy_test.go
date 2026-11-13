package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// presetDeployment states the yield policy, with its defaults, on dep.
func presetDeployment(dep *store.Deployment, yield string) *store.Deployment {
	p, _ := yieldpolicy.Parse(yield, yieldpolicy.Overrides{})
	dep.SetPolicy(p)
	return dep
}

type yieldPolicyRig struct {
	*tokenRig
	node                         *store.Node
	plain, sleepy, multi, sglang *store.Deployment
}

func newYieldPolicyRig(t *testing.T) *yieldPolicyRig {
	t.Helper()
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	node, err := rig.st.RegisterNode("sl-node", store.HashToken("sl"), "sl-node")
	if err != nil {
		t.Fatal(err)
	}
	create := func(dep *store.Deployment) *store.Deployment {
		dep.State = store.DeployActive
		dep.ServedName = dep.Name
		created, err := rig.st.CreateDeployment(dep)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	return &yieldPolicyRig{
		tokenRig: rig,
		node:     node,
		plain:    create(&store.Deployment{Name: "plain", Engine: "vllm", Replicas: 1}),
		sleepy:   create(&store.Deployment{Name: "sleepy", Engine: "vllm", Sleepable: true, Replicas: 1}),
		multi:    create(&store.Deployment{Name: "multi", Engine: "vllm", Sleepable: true, NNodes: 2, Replicas: 1}),
		sglang:   create(&store.Deployment{Name: "sgl", Engine: "sglang", Replicas: 1}),
	}
}

// place puts a running instance of dep on the rig's node's cards.
func (r *yieldPolicyRig) place(t *testing.T, dep *store.Deployment, uuid, gpus string) {
	t.Helper()
	if _, err := r.st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: r.node.ID, State: proto.InstRunning, GPUIndexes: gpus}); err != nil {
		t.Fatal(err)
	}
}

func TestYieldPolicyRouteAuthAndTarget(t *testing.T) {
	rig := newYieldPolicyRig(t)
	path := schedPath(rig.sleepy, "yield-policy")
	const body = `{"policy":"anytime"}`
	const answer = `{"policy":"anytime","idle_sec":30,"sleep_level":"auto","unfinished":"abort","hold_max_sec":3600}`
	if rec := rig.do("", http.MethodPut, path, body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d %s, want 401", rec.Code, rec.Body)
	}
	read := rig.mint("read")
	if rec := rig.do(read, http.MethodPut, path, body); rec.Code != http.StatusForbidden || decodeBody(t, rec)["code"] != "PLATFORM_TOKEN_READ_ONLY" {
		t.Fatalf("read token = %d %s, want 403", rec.Code, rec.Body)
	}
	if dep, _ := rig.st.GetDeployment(rig.sleepy.ID); dep.Policy().Yield != yieldpolicy.Never {
		t.Fatalf("a refused request changed the policy to %+v", dep.Policy())
	}
	if rec := rig.do(rig.mint("write"), http.MethodPut, path, body); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != answer {
		t.Fatalf("write token = %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodPut, "/api/v1/deployments/9999/yield-policy", body); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown deployment = %d %s, want 404", rec.Code, rec.Body)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		if rec := rig.do(rig.session, method, path, "{}"); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s = %d, want 405", method, rec.Code)
		}
	}
}

// The two routes the yield policy replaced are gone, not kept as aliases.
func TestTheServiceLevelAndIdleSleepRoutesAreGone(t *testing.T) {
	rig := newYieldPolicyRig(t)
	for _, tc := range []struct{ tail, body string }{
		{"service-level", `{"level":"offline"}`},
		{"idle-sleep", `{"after_sec":900,"level":2}`},
	} {
		for _, method := range []string{http.MethodPut, http.MethodGet, http.MethodPost} {
			if rec := rig.do(rig.session, method, schedPath(rig.sleepy, tc.tail), tc.body); rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s = %d %s, want 404", method, tc.tail, rec.Code, rec.Body)
			}
		}
	}
	if dep, _ := rig.st.GetDeployment(rig.sleepy.ID); dep.Policy().Yield != yieldpolicy.Never {
		t.Fatalf("a removed route changed the policy: %+v", dep.Policy())
	}
}

func TestYieldPolicySetting(t *testing.T) {
	rig := newYieldPolicyRig(t)
	wire := func(dep *store.Deployment) map[string]any {
		t.Helper()
		return (&replacementRig{tokenRig: rig.tokenRig}).deploymentWire(t, dep)
	}
	got := wire(rig.sleepy)
	if jsonOf(t, got["yield_policy"]) != `{"policy":"never"}` || got["sleep_held"] != false {
		t.Fatalf("a new deployment reads yield_policy=%s sleep_held=%v, want never/false", jsonOf(t, got["yield_policy"]), got["sleep_held"])
	}
	for _, gone := range []string{"service_level", "idle_sleep"} {
		if _, ok := got[gone]; ok {
			t.Fatalf("the deployment still carries %s", gone)
		}
	}
	path := schedPath(rig.sleepy, "yield-policy")
	// Every body is answered with the policy as stored, and the deployment
	// then reads exactly that.
	for _, tc := range []struct{ name, body, want string }{
		// A policy alone takes its defaults.
		{"idle alone", `{"policy":"idle"}`, `{"policy":"idle","idle_sec":60,"sleep_level":"auto","hold_max_sec":300}`},
		{"timeslice alone", `{"policy":"timeslice"}`, `{"policy":"timeslice","idle_sec":30,"sleep_level":"auto","max_serve_sec":120,"unfinished":"wait","unfinished_wait_sec":30,"hold_max_sec":600}`},
		{"anytime alone", `{"policy":"anytime"}`, `{"policy":"anytime","idle_sec":30,"sleep_level":"auto","unfinished":"abort","hold_max_sec":3600}`},
		{"never alone", `{"policy":"never"}`, `{"policy":"never"}`},
		// What the UI sends: every parameter.
		{"idle with a 15 s idle time", `{"policy":"idle","idle_sec":15,"sleep_level":"l1","hold_max_sec":300}`, `{"policy":"idle","idle_sec":15,"sleep_level":"l1","hold_max_sec":300}`},
		{"anytime that waits", `{"policy":"anytime","idle_sec":45,"sleep_level":"l2","unfinished":"wait","unfinished_wait_sec":90,"hold_max_sec":60}`, `{"policy":"anytime","idle_sec":45,"sleep_level":"l2","unfinished":"wait","unfinished_wait_sec":90,"hold_max_sec":60}`},
		{"timeslice that cuts off", `{"policy":"timeslice","idle_sec":15,"max_serve_sec":30,"unfinished":"abort","unfinished_wait_sec":30}`, `{"policy":"timeslice","idle_sec":15,"sleep_level":"auto","max_serve_sec":30,"unfinished":"abort","hold_max_sec":600}`},
		// Parameters that do not apply are ignored, however they are spelt.
		{"inapplicable parameters", `{"policy":"idle","idle_sec":60,"max_serve_sec":99999,"unfinished":"abort","unfinished_wait_sec":0}`, `{"policy":"idle","idle_sec":60,"sleep_level":"auto","hold_max_sec":300}`},
		{"never ignores everything", `{"policy":"never","idle_sec":5,"sleep_level":"l2","hold_max_sec":1}`, `{"policy":"never"}`},
		// A response sent back as it is.
		{"a response sent back", `{"policy":"timeslice","idle_sec":100,"sleep_level":"l1","max_serve_sec":100,"unfinished":"wait","unfinished_wait_sec":9,"hold_max_sec":15}`, `{"policy":"timeslice","idle_sec":100,"sleep_level":"l1","max_serve_sec":100,"unfinished":"wait","unfinished_wait_sec":9,"hold_max_sec":15}`},
		{"bounds", `{"policy":"timeslice","idle_sec":15,"max_serve_sec":86400,"unfinished":"wait","unfinished_wait_sec":600,"hold_max_sec":10}`, `{"policy":"timeslice","idle_sec":15,"sleep_level":"auto","max_serve_sec":86400,"unfinished":"wait","unfinished_wait_sec":600,"hold_max_sec":10}`},
		{"upper bounds", `{"policy":"idle","idle_sec":604800,"hold_max_sec":3600}`, `{"policy":"idle","idle_sec":604800,"sleep_level":"auto","hold_max_sec":3600}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := rig.do(rig.session, http.MethodPut, path, tc.body)
			if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != tc.want {
				t.Fatalf("PUT %s = %d %s\nwant %s", tc.body, rec.Code, rec.Body, tc.want)
			}
			var stated, read map[string]any
			_ = json.Unmarshal([]byte(tc.want), &stated)
			if read, _ = wire(rig.sleepy)["yield_policy"].(map[string]any); jsonOf(t, read) != jsonOf(t, stated) {
				t.Fatalf("the deployment reads %s after %s, want %s", jsonOf(t, read), tc.body, jsonOf(t, stated))
			}
		})
	}

	rangeOf := func(field string, lo, hi int) map[string]any {
		return map[string]any{"field": field, "reason": "out_of_range", "minimum": float64(lo), "maximum": float64(hi)}
	}
	for _, tc := range []struct {
		dep    *store.Deployment
		body   string
		status int
		code   string
		params map[string]any
	}{
		{rig.sleepy, `{}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", map[string]any{"field": "policy", "reason": "required"}},
		{rig.sleepy, `{"idle_sec":60}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", map[string]any{"field": "policy", "reason": "required"}},
		{rig.sleepy, `{"policy":"gold"}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", map[string]any{"field": "policy", "reason": "invalid_value"}},
		{rig.sleepy, `{"policy":"idle","sleep_level":"l3"}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", map[string]any{"field": "sleep_level", "reason": "invalid_value"}},
		{rig.sleepy, `{"policy":"never","unfinished":"maybe"}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", map[string]any{"field": "unfinished", "reason": "invalid_value"}},
		{rig.sleepy, `{"policy":"timeslice","unfinished":"wait_unbounded"}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", map[string]any{"field": "unfinished", "reason": "invalid_value"}},
		{rig.sleepy, `{"policy":"idle","idle_sec":14}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", rangeOf("idle_sec", 15, 604800)},
		{rig.sleepy, `{"policy":"anytime","idle_sec":604801}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", rangeOf("idle_sec", 15, 604800)},
		{rig.sleepy, `{"policy":"timeslice","idle_sec":61,"max_serve_sec":60}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", rangeOf("idle_sec", 15, 60)},
		{rig.sleepy, `{"policy":"timeslice","max_serve_sec":29,"idle_sec":15}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", rangeOf("max_serve_sec", 30, 86400)},
		{rig.sleepy, `{"policy":"anytime","unfinished":"wait","unfinished_wait_sec":0}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", rangeOf("unfinished_wait_sec", 1, 600)},
		{rig.sleepy, `{"policy":"timeslice","unfinished":"wait","unfinished_wait_sec":601}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", rangeOf("unfinished_wait_sec", 1, 600)},
		{rig.sleepy, `{"policy":"idle","hold_max_sec":9}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", rangeOf("hold_max_sec", 10, 3600)},
		{rig.sleepy, `{"policy":"anytime","hold_max_sec":3601}`, http.StatusBadRequest, "YIELD_POLICY_INVALID", rangeOf("hold_max_sec", 10, 3600)},
		{rig.plain, `{"policy":"anytime"}`, http.StatusBadRequest, "SLEEP_NOT_ENABLED", nil},
		{rig.sglang, `{"policy":"timeslice"}`, http.StatusBadRequest, "SLEEP_NOT_ENABLED", nil},
		{rig.multi, `{"policy":"idle"}`, http.StatusBadRequest, "YIELD_POLICY_SHAPE", nil},
	} {
		rec := rig.do(rig.session, http.MethodPut, schedPath(tc.dep, "yield-policy"), tc.body)
		resp := decodeBody(t, rec)
		if rec.Code != tc.status || resp["code"] != tc.code {
			t.Fatalf("%s %s = %d %s, want %d %s", tc.dep.Name, tc.body, rec.Code, rec.Body, tc.status, tc.code)
		}
		if tc.params != nil {
			if got, _ := resp["params"].(map[string]any); jsonOf(t, got) != jsonOf(t, tc.params) {
				t.Fatalf("%s %s params = %s, want %s", tc.dep.Name, tc.body, jsonOf(t, got), jsonOf(t, tc.params))
			}
		}
	}
	// Never needs nothing: any deployment can go back to it.
	for _, dep := range []*store.Deployment{rig.plain, rig.multi, rig.sglang} {
		if rec := rig.do(rig.session, http.MethodPut, schedPath(dep, "yield-policy"), `{"policy":"never"}`); rec.Code != http.StatusOK {
			t.Fatalf("%s back to never = %d %s", dep.Name, rec.Code, rec.Body)
		}
	}
	for _, body := range []string{`{"policy":"idle","x":1}`, `{"preset":"online"}`, `{"policy":"never","auto_sleep":true}`, `{"policy":"timeslice","min_serve_sec":60}`, `{"policy":1}`, `{"policy":"idle","idle_sec":"60"}`, `{`, ``} {
		if rec := rig.do(rig.session, http.MethodPut, path, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT %q = %d, want 400", body, rec.Code)
		}
	}
	// Every refusal left the last accepted policy: the upper bounds test's.
	if dep, _ := rig.st.GetDeployment(rig.sleepy.ID); dep.Policy().IdleSec != 604800 || dep.Policy().HoldMaxSec != 3600 || dep.Policy().Yield != yieldpolicy.Idle {
		t.Fatalf("a refused request changed the policy: %+v", dep.Policy())
	}
}

// Two idle deployments may share a card: the answer names the other so the
// operator is warned that neither gives way to the other while busy.
func TestTwoIdleDeploymentsOnACardAreNamed(t *testing.T) {
	rig := newYieldPolicyRig(t)
	other, err := rig.st.CreateDeployment(&store.Deployment{Name: "other", ServedName: "other", Engine: "vllm", Sleepable: true, Replicas: 1, State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	rig.place(t, rig.sleepy, "sleepy-1", "[0,1]")
	rig.place(t, other, "other-1", "[1]")
	if rec := rig.do(rig.session, http.MethodPut, schedPath(other, "yield-policy"), `{"policy":"idle"}`); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "idle_neighbours") {
		t.Fatalf("first idle = %d %s, want no neighbours", rec.Code, rec.Body)
	}
	rec := rig.do(rig.session, http.MethodPut, schedPath(rig.sleepy, "yield-policy"), `{"policy":"idle","idle_sec":30}`)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"policy":"idle","idle_sec":30,"sleep_level":"auto","hold_max_sec":300,"idle_neighbours":["other"]}` {
		t.Fatalf("second idle on a shared card = %d %s, want 200 naming other", rec.Code, rec.Body)
	}
	if dep, _ := rig.st.GetDeployment(rig.sleepy.ID); dep.Policy().Yield != yieldpolicy.Idle {
		t.Fatalf("the second idle policy was not stored: %+v", dep.Policy())
	}
	// The deployment itself does not carry the warning: only the answer.
	if got := (&replacementRig{tokenRig: rig.tokenRig}).deploymentWire(t, rig.sleepy); strings.Contains(jsonOf(t, got["yield_policy"]), "idle_neighbours") {
		t.Fatalf("the deployment reads %s", jsonOf(t, got["yield_policy"]))
	}
	// Next to timeslice or anytime there is nothing to warn of.
	for _, body := range []string{`{"policy":"timeslice"}`, `{"policy":"anytime"}`} {
		if rec := rig.do(rig.session, http.MethodPut, schedPath(rig.sleepy, "yield-policy"), body); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "idle_neighbours") {
			t.Fatalf("%s next to idle = %d %s", body, rec.Code, rec.Body)
		}
	}
	// A stopped instance holds no card.
	if err := rig.st.DB().Model(&store.Instance{}).Where("uuid = ?", "other-1").Update("state", proto.InstStopped).Error; err != nil {
		t.Fatal(err)
	}
	if rec := rig.do(rig.session, http.MethodPut, schedPath(rig.sleepy, "yield-policy"), `{"policy":"idle"}`); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "idle_neighbours") {
		t.Fatalf("idle once the other's instance stopped = %d %s", rec.Code, rec.Body)
	}
}

// An operator's sleep is held (a request does not wake it); the deployment,
// all of whose replicas it holds, reads it as sleep_held.
func TestAnOperatorsSleepIsHeld(t *testing.T) {
	rig := newYieldPolicyRig(t)
	spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: "sleepy-1", DesiredState: "running", Sleepable: true})
	if _, err := rig.st.CreateInstance(&store.Instance{UUID: "sleepy-1", DeploymentID: rig.sleepy.ID, NodeID: rig.node.ID, State: proto.InstRunning, GPUIndexes: "[0]", DesiredStateJSON: string(spec)}); err != nil {
		t.Fatal(err)
	}
	if rec := rig.do(rig.session, http.MethodPost, schedPath(rig.sleepy, "sleep?level=2"), ""); rec.Code != http.StatusOK {
		t.Fatalf("sleep = %d %s", rec.Code, rec.Body)
	}
	got := (&replacementRig{tokenRig: rig.tokenRig}).deploymentWire(t, rig.sleepy)
	if got["sleep_held"] != true {
		t.Fatalf("after an operator's sleep sleep_held = %v", got["sleep_held"])
	}
}

// The dictionary is the server's rules on the wire: a client's editor reads
// its defaults, which parameters apply and their ranges from here.
func TestYieldPolicyDictionary(t *testing.T) {
	rig := newYieldPolicyRig(t)
	if rec := rig.do("", http.MethodGet, "/api/v1/scheduling/yield-policies", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", rec.Code)
	}
	rec := rig.do(rig.mint("read"), http.MethodGet, "/api/v1/scheduling/yield-policies", "")
	const want = `{"policies":[` +
		`{"policy":"never","defaults":{"policy":"never"},"applies":[]},` +
		`{"policy":"idle","defaults":{"policy":"idle","idle_sec":60,"sleep_level":"auto","hold_max_sec":300},"applies":["idle_sec","sleep_level","hold_max_sec"]},` +
		`{"policy":"timeslice","defaults":{"policy":"timeslice","idle_sec":30,"sleep_level":"auto","max_serve_sec":120,"unfinished":"wait","unfinished_wait_sec":30,"hold_max_sec":600},"applies":["idle_sec","sleep_level","max_serve_sec","unfinished","unfinished_wait_sec","hold_max_sec"]},` +
		`{"policy":"anytime","defaults":{"policy":"anytime","idle_sec":30,"sleep_level":"auto","unfinished":"abort","unfinished_wait_sec":30,"hold_max_sec":3600},"applies":["idle_sec","sleep_level","unfinished","unfinished_wait_sec","hold_max_sec"]}],` +
		`"applies_when":{"unfinished_wait_sec":{"unfinished":["wait"]}},` +
		`"bounds":{"hold_max_sec":{"minimum":10,"maximum":3600},"idle_sec":{"minimum":15,"maximum":604800,"at_most":"max_serve_sec"},"max_serve_sec":{"minimum":30,"maximum":86400},"unfinished_wait_sec":{"minimum":1,"maximum":600}},` +
		`"sleep_levels":["auto","l1","l2"],"unfinished":["abort","wait"]}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("dictionary = %d\n got %s\nwant %s", rec.Code, rec.Body, want)
	}
	// Every default is a policy the server takes as it is.
	var dict dto.YieldPolicyDictionary
	if err := json.Unmarshal(rec.Body.Bytes(), &dict); err != nil {
		t.Fatal(err)
	}
	for _, entry := range dict.Policies {
		raw, _ := json.Marshal(entry.Defaults)
		var req dto.YieldPolicyRequest
		_ = json.Unmarshal(raw, &req)
		if _, v := req.Parse(); v != nil {
			t.Errorf("%s defaults refused: %+v", entry.Policy, v)
		}
	}
	// What a client reads off applies and applies_when is the server's rule
	// (yieldpolicy.Applies) for every choice of the selectors.
	for _, entry := range dict.Policies {
		for _, level := range dict.SleepLevels {
			for _, mode := range dict.Unfinished {
				chosen := map[string]string{"sleep_level": level, "unfinished": mode}
				client := map[string]bool{}
				for _, name := range entry.Applies {
					client[name] = true
					for selector, values := range dict.AppliesWhen[name] {
						held := false
						for _, v := range values {
							held = held || v == chosen[selector]
						}
						client[name] = client[name] && held
					}
				}
				rule := yieldpolicy.Policy{Yield: entry.Policy, SleepLevel: level, Unfinished: mode}.Normalize().Applies()
				server := map[string]bool{
					"idle_sec": rule.IdleSec, "sleep_level": rule.SleepLevel, "max_serve_sec": rule.MaxServeSec,
					"unfinished": rule.Unfinished, "unfinished_wait_sec": rule.UnfinishedWaitSec, "hold_max_sec": rule.HoldMaxSec,
				}
				for name, applies := range server {
					if client[name] != applies {
						t.Errorf("%s with sleep_level=%s unfinished=%s: the dictionary says %s applies %v, the rule %v", entry.Policy, level, mode, name, client[name], applies)
					}
				}
			}
		}
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/scheduling/yield-policies", "{}"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", rec.Code)
	}
}
