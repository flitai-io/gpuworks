package planning

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func pdNode(uuid, state string, gpus ...string) *store.Node {
	node := &store.Node{UUID: uuid, Name: uuid, State: state}
	for i, name := range gpus {
		vendor := "nvidia"
		switch name {
		case "Ascend910B4":
			vendor = "ascend"
		case "PPU-ZW810E":
			vendor = "ppu"
		case "K100_AI":
			vendor = "hygon"
		}
		node.GPUs = append(node.GPUs, store.GPU{Index: i, Name: name, Vendor: vendor})
	}
	return node
}

func parsePDText(t *testing.T, text string) *PDPlacement {
	t.Helper()
	placement, err := ParsePlacement(text)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if placement.PD == nil {
		t.Fatal("no pd section")
	}
	return placement.PD
}

func productCode(err error) string {
	var product *messages.ProductError
	if errors.As(err, &product) {
		return product.Code()
	}
	return ""
}

func TestParsePlacementReadsPD(t *testing.T) {
	pd := parsePDText(t, `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"tp":1,"units":[{"node_uuid":" n1 ","gpus":[0]},{"node_uuid":"n2"}]},"decode":{"tp":2,"units":[{"node_uuid":"n3","gpus":[0,1]}]}},"fallback":"fail"}}`)
	if pd.Mode != "vllm-nixl" || len(pd.Units("prefill")) != 2 || pd.Units("prefill")[0].NodeUUID != "n1" {
		t.Fatalf("pd = %+v", pd)
	}
	if pd.TP("decode") != 2 || pd.EffectiveFallback() != "fail" || pd.EffectiveKVLoadFailure() != "fail" || pd.InstanceCount() != 3 {
		t.Fatalf("derived values wrong: %+v", pd)
	}
	// Decode first: the mode picks decode before prefill.
	mode, err := ValidatePD(pd, "vllm")
	if err != nil {
		t.Fatal(err)
	}
	if got := pd.NodeUUIDs(mode); len(got) != 3 || got[0] != "n3" || got[1] != "n1" {
		t.Fatalf("node order = %v", got)
	}
	// Round trip through the stored JSON keeps the section.
	encoded, _ := json.Marshal(Placement{PD: pd})
	again := parsePDText(t, string(encoded))
	if again.TP("decode") != 2 || again.Units("prefill")[1].NodeUUID != "n2" {
		t.Fatalf("round trip = %s", encoded)
	}
}

func TestParsePlacementRejectsUnreadablePD(t *testing.T) {
	for _, text := range []string{
		`{"pd":"vllm-nixl"}`,
		`{"pd":{"mode":"vllm-nixl","roles":{},"ratio":2}}`,
		`{"pd":{"roles":{"prefill":{"units":[{"node":"n1"}]}}}}`,
	} {
		if _, err := ParsePlacement(text); err == nil {
			t.Fatalf("accepted %s", text)
		}
	}
	placement, err := ParsePlacement(`{"tp":2}`)
	if err != nil || placement.PD != nil {
		t.Fatalf("an ordinary placement grew a PD section: %+v %v", placement, err)
	}
}

func TestValidatePDRules(t *testing.T) {
	cases := []struct {
		name, text, engine, code string
	}{
		{"ok", `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"a"}]},"decode":{"units":[{"node_uuid":"b"}]}}}}`, "vllm", ""},
		{"unknown mode", `{"pd":{"mode":"x","roles":{}}}`, "vllm", messages.ErrorPDModeUnknown},
		{"engine", `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"a"}]},"decode":{"units":[{"node_uuid":"b"}]}}}}`, "sglang", messages.ErrorPDModeEngineMismatch},
		{"missing role", `{"pd":{"mode":"vllm-nixl","roles":{"decode":{"units":[{"node_uuid":"b"}]}}}}`, "vllm", messages.ErrorPDRoleMissing},
		{"unknown role", `{"pd":{"mode":"vllm-nixl","roles":{"encode":{"units":[{"node_uuid":"a"}]},"prefill":{"units":[{"node_uuid":"a"}]},"decode":{"units":[{"node_uuid":"b"}]}}}}`, "vllm", messages.ErrorPDRoleUnknown},
		{"empty role", `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[]},"decode":{"units":[{"node_uuid":"b"}]}}}}`, "vllm", messages.ErrorPDRoleUnitsEmpty},
		{"no node", `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":""}]},"decode":{"units":[{"node_uuid":"b"}]}}}}`, "vllm", messages.ErrorPDNodeRequired},
		{"pinned count", `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"tp":2,"units":[{"node_uuid":"a","gpus":[0]}]},"decode":{"tp":2,"units":[{"node_uuid":"b"}]}}}}`, "vllm", messages.ErrorPDUnitGPUCount},
		{"overlap", `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"a","gpus":[0]}]},"decode":{"units":[{"node_uuid":"a","gpus":[0]}]}}}}`, "vllm", messages.ErrorPDGPUOverlap},
		{"tp direction", `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"tp":4,"units":[{"node_uuid":"a"}]},"decode":{"tp":2,"units":[{"node_uuid":"b"}]}}}}`, "vllm", messages.ErrorPDTPDirection},
		{"negative tp", `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"tp":-1,"units":[{"node_uuid":"a"}]},"decode":{"units":[{"node_uuid":"b"}]}}}}`, "vllm", messages.ErrorPDTPInvalid},
		{"fallback", `{"pd":{"mode":"vllm-nixl","fallback":"drop","roles":{"prefill":{"units":[{"node_uuid":"a"}]},"decode":{"units":[{"node_uuid":"b"}]}}}}`, "vllm", messages.ErrorPDFallbackInvalid},
		{"kv load", `{"pd":{"mode":"vllm-nixl","kv_load_failure":"retry","roles":{"prefill":{"units":[{"node_uuid":"a"}]},"decode":{"units":[{"node_uuid":"b"}]}}}}`, "vllm", messages.ErrorPDKVLoadFailureInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ValidatePD(parsePDText(t, c.text), c.engine)
			if got := productCode(err); got != c.code {
				t.Fatalf("code = %q (%v), want %q", got, err, c.code)
			}
		})
	}
	var many []PDUnit
	for i := 0; i < 33; i++ {
		many = append(many, PDUnit{NodeUUID: "a"})
	}
	pd := &PDPlacement{Mode: "vllm-nixl", Roles: map[string]PDRole{"prefill": {Units: many}, "decode": {Units: []PDUnit{{NodeUUID: "b"}}}}}
	if _, err := ValidatePD(pd, "vllm"); productCode(err) != messages.ErrorPDRoleUnitsTooMany {
		t.Fatalf("33 units: %v", err)
	}
}

// A mode whose transport stays inside one host takes a same-node placement
// without the throughput warning, and refuses instances spread over nodes.
func TestValidatePDNodesSameNodeMode(t *testing.T) {
	nodes := map[string]*store.Node{
		"p1": pdNode("p1", store.NodeStateOnline, "PPU-ZW810E", "PPU-ZW810E"),
		"p2": pdNode("p2", store.NodeStateOnline, "PPU-ZW810E", "PPU-ZW810E"),
	}
	check := func(text string) ([]messages.Warning, error) {
		pd := parsePDText(t, text)
		mode, err := ValidatePD(pd, "sglang")
		if err != nil {
			t.Fatalf("structural: %v", err)
		}
		return ValidatePDNodes(pd, mode, nodes, true)
	}
	warnings, err := check(`{"pd":{"mode":"sglang-ppu-ipc","roles":{"prefill":{"units":[{"node_uuid":"p1","gpus":[0]}]},"decode":{"units":[{"node_uuid":"p1","gpus":[1]}]}}}}`)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("same node: %v %v", warnings, err)
	}
	_, err = check(`{"pd":{"mode":"sglang-ppu-ipc","roles":{"prefill":{"units":[{"node_uuid":"p1"}]},"decode":{"units":[{"node_uuid":"p2"}]}}}}`)
	if productCode(err) != messages.ErrorPDSameNodeRequired {
		t.Fatalf("two nodes: %v", err)
	}
	var product *messages.ProductError
	if !errors.As(err, &product) || product.Params()["nodes"] != "p1, p2" || product.Params()["mode"] != "sglang-ppu-ipc" {
		t.Fatalf("params = %+v", product)
	}
}

func TestValidatePDNodes(t *testing.T) {
	nodes := map[string]*store.Node{
		"a":   pdNode("a", store.NodeStateOnline, "RTX 4090", "RTX 4090"),
		"b":   pdNode("b", store.NodeStateOnline, "RTX 3080 Ti"),
		"off": pdNode("off", "offline", "RTX 4090"),
		"npu": pdNode("npu", store.NodeStateOnline, "Ascend910B4"),
		"dcu": pdNode("dcu", store.NodeStateOnline, "K100_AI", "K100_AI"),
	}
	check := func(text string, requireOnline bool) ([]messages.Warning, error) {
		pd := parsePDText(t, text)
		mode, err := ValidatePD(pd, "vllm")
		if err != nil {
			t.Fatalf("structural: %v", err)
		}
		return ValidatePDNodes(pd, mode, nodes, requireOnline)
	}
	warnings, err := check(`{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"a","gpus":[0]}]},"decode":{"units":[{"node_uuid":"a","gpus":[1]}]}}}}`, true)
	if err != nil || len(warnings) != 1 || warnings[0].Code != messages.WarningPDSameNodeThroughput {
		t.Fatalf("same node: %v %v", warnings, err)
	}
	warnings, err = check(`{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"b"}]},"decode":{"units":[{"node_uuid":"a"}]}}}}`, true)
	if err != nil || len(warnings) != 1 || warnings[0].Code != messages.WarningPDGPUModelsDiffer {
		t.Fatalf("mixed models: %v %v", warnings, err)
	}
	// Hygon DCUs run vllm-mooncake and no NIXL mode.
	if _, err := check(`{"pd":{"mode":"vllm-mooncake","roles":{"prefill":{"units":[{"node_uuid":"dcu","gpus":[0]}]},"decode":{"units":[{"node_uuid":"dcu","gpus":[1]}]}}}}`, true); err != nil {
		t.Fatalf("vllm-mooncake on DCUs: %v", err)
	}
	for text, code := range map[string]string{
		`{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"dcu"}]},"decode":{"units":[{"node_uuid":"a"}]}}}}`:                 messages.ErrorPDVendorUnsupported,
		`{"pd":{"mode":"vllm-mooncake","roles":{"prefill":{"units":[{"node_uuid":"a"}]},"decode":{"units":[{"node_uuid":"a"}]}}}}`:               messages.ErrorPDVendorUnsupported,
		`{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"zz"}]},"decode":{"units":[{"node_uuid":"a"}]}}}}`:                  messages.ErrorPDNodeUnknown,
		`{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"off"}]},"decode":{"units":[{"node_uuid":"a"}]}}}}`:                 messages.ErrorPDNodeOffline,
		`{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"b","gpus":[3]}]},"decode":{"units":[{"node_uuid":"a"}]}}}}`:        messages.ErrorPDNodeGPUMissing,
		`{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"b"},{"node_uuid":"b"}]},"decode":{"units":[{"node_uuid":"a"}]}}}}`: messages.ErrorPDNodeGPUsInsufficient,
		`{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"npu"}]},"decode":{"units":[{"node_uuid":"a"}]}}}}`:                 messages.ErrorPDVendorUnsupported,
		`{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"tp":1,"units":[{"node_uuid":"a"}]},"decode":{"tp":2,"units":[{"node_uuid":"a"}]}}}}`:     messages.ErrorPDNodeGPUsInsufficient,
	} {
		if _, err := check(text, true); productCode(err) != code {
			t.Fatalf("%s: got %v, want %s", text, err, code)
		}
	}
	// The controller re-checks a stored placement without requiring the node
	// to be online at that instant.
	if _, err := check(`{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"off"}]},"decode":{"units":[{"node_uuid":"a"}]}}}}`, false); err != nil {
		t.Fatalf("offline without requireOnline: %v", err)
	}
}

func TestCheckPDArgsAndWarnings(t *testing.T) {
	if err := CheckPDArgs("vllm", []string{"--max-model-len", "8192"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--kv-transfer-config", "{}"}, {"--kv-offloading-size=4"}, {"--kv-offloading-backend", "native"}} {
		if productCode(CheckPDArgs("vllm", nil, args)) != messages.ErrorPDConnectorArgConflict {
			t.Fatalf("vllm: %v accepted", args)
		}
	}
	// Each engine refuses the flags its own adapter composes, and only those.
	for _, args := range [][]string{{"--disaggregation-mode", "decode"}, {"--disaggregation-transfer-backend=mooncake"}, {"--disaggregation-bootstrap-port", "9000"}} {
		if productCode(CheckPDArgs("sglang", nil, args)) != messages.ErrorPDConnectorArgConflict {
			t.Fatalf("sglang: %v accepted", args)
		}
		if err := CheckPDArgs("vllm", args); err != nil {
			t.Fatalf("vllm refused sglang's %v: %v", args, err)
		}
	}
	if err := CheckPDArgs("unknown-engine", []string{"--kv-transfer-config", "{}"}); err != nil {
		t.Fatalf("an unknown engine composes nothing: %v", err)
	}
	got := PDFeatureWarnings([]string{pdmode.RolePrefill}, true)
	if len(got) != 2 || got[0].Code != messages.WarningPDKVOffloadPrefillOnly || got[0].Params != nil || got[1].Params["feature"] != "dgpu" {
		t.Fatalf("warnings = %+v", got)
	}
	if len(PDFeatureWarnings(nil, false)) != 0 {
		t.Fatal("no feature, no warning")
	}
	// Offload kept on both roles is not prefill only.
	if got := PDFeatureWarnings([]string{pdmode.RolePrefill, pdmode.RoleDecode}, false); len(got) != 0 {
		t.Fatalf("offload on both roles warned: %+v", got)
	}
}

func TestPDStatusOf(t *testing.T) {
	pd := &PDPlacement{Mode: "vllm-nixl", Roles: map[string]PDRole{
		"prefill": {Units: []PDUnit{{NodeUUID: "a"}, {NodeUUID: "a"}}},
		"decode":  {TP: 2, Units: []PDUnit{{NodeUUID: "b"}}},
	}}
	running := func(role string) store.Instance { return store.Instance{Role: role, State: "running"} }
	for _, c := range []struct {
		name  string
		insts []store.Instance
		state string
		short bool
	}{
		{"none", nil, PDStateNotReady, true},
		{"decode only", []store.Instance{running("decode")}, PDStatePartial, true},
		{"asleep counts as not ready", []store.Instance{{Role: "decode", State: "running", SleepLevel: 1}, running("prefill")}, PDStatePartial, true},
		{"one each", []store.Instance{running("decode"), running("prefill"), {Role: "prefill", State: "starting"}}, PDStateRunning, true},
		{"full", []store.Instance{running("decode"), running("prefill"), running("prefill")}, PDStateRunning, false},
	} {
		got := PDStatusOf(pd, c.insts)
		if got.State != c.state || got.Short != c.short {
			t.Fatalf("%s: %+v", c.name, got)
		}
	}
	got := PDStatusOf(pd, nil)
	if got.Roles[0].Role != "decode" || got.Roles[0].TP != 2 || got.Roles[1].Desired != 2 {
		t.Fatalf("roles = %+v", got.Roles)
	}
	// Prefill scaled to zero on purpose serves from the decoder: running.
	scaledDown := &PDPlacement{Mode: "vllm-nixl", Roles: map[string]PDRole{"prefill": {}, "decode": {Units: []PDUnit{{NodeUUID: "b"}}}}}
	if got := PDStatusOf(scaledDown, []store.Instance{running("decode")}); got.State != PDStateRunning || got.Short {
		t.Fatalf("prefill at zero = %+v", got)
	}
	// A multi-node decode leader counts once, and only with its followers.
	multi := &PDPlacement{Mode: "vllm-nixl", Roles: map[string]PDRole{"prefill": {}, "decode": {TP: 2, Units: []PDUnit{{NodeUUID: "b", Followers: []PDRank{{NodeUUID: "c"}}}}}}}
	leader := store.Instance{UUID: "l", Role: "decode", State: "running", NNodes: 2, DistGroup: "g"}
	follower := store.Instance{UUID: "f", Role: "decode", State: "starting", NNodes: 2, NodeRank: 1, Headless: true, DistGroup: "g"}
	if got := PDStatusOf(multi, []store.Instance{leader, follower}); got.State != PDStateNotReady {
		t.Fatalf("leader without its follower = %+v", got)
	}
	follower.State = "running"
	if got := PDStatusOf(multi, []store.Instance{leader, follower}); got.State != PDStateRunning || got.Roles[0].Ready != 1 {
		t.Fatalf("complete group = %+v", got)
	}
}

func TestValidateRunningPDAllowsNoPrefill(t *testing.T) {
	pd := parsePDText(t, `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"tp":1,"units":[]},"decode":{"units":[{"node_uuid":"b"}]}}}}`)
	if _, err := ValidatePD(pd, "vllm"); productCode(err) != messages.ErrorPDRoleUnitsEmpty {
		t.Fatalf("create-time validation = %v", err)
	}
	if _, err := ValidateRunningPD(pd, "vllm"); err != nil {
		t.Fatalf("running validation = %v", err)
	}
	noDecode := parsePDText(t, `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"a"}]},"decode":{"units":[]}}}}`)
	if _, err := ValidateRunningPD(noDecode, "vllm"); productCode(err) != messages.ErrorPDRoleUnitsEmpty {
		t.Fatalf("a running deployment kept no decoder: %v", err)
	}
}

func TestValidatePDMultiNodeUnits(t *testing.T) {
	for _, c := range []struct {
		name, text, code string
	}{
		{"repeated node", `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"a"}]},"decode":{"tp":2,"units":[{"node_uuid":"b","followers":[{"node_uuid":"b"}]}]}}}}`, messages.ErrorPDRankNodeRepeated},
		{"follower without node", `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"a"}]},"decode":{"tp":2,"units":[{"node_uuid":"b","followers":[{"node_uuid":" "}]}]}}}}`, messages.ErrorPDNodeRequired},
		{"tp unstated", `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"a"}]},"decode":{"units":[{"node_uuid":"b","gpus":[0],"followers":[{"node_uuid":"c"}]}]}}}}`, messages.ErrorPDRankTPRequired},
		{"indivisible", `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"a"}]},"decode":{"tp":3,"units":[{"node_uuid":"b","followers":[{"node_uuid":"c"}]}]}}}}`, messages.ErrorPDRankSplit},
		{"rank pins the whole TP", `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"a"}]},"decode":{"tp":2,"units":[{"node_uuid":"b","followers":[{"node_uuid":"c","gpus":[0,1]}]}]}}}}`, messages.ErrorPDUnitGPUCount},
		{"follower GPU overlaps another instance", `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"c","gpus":[0]}]},"decode":{"tp":2,"units":[{"node_uuid":"b","followers":[{"node_uuid":"c","gpus":[0]}]}]}}}}`, messages.ErrorPDGPUOverlap},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ValidatePD(parsePDText(t, c.text), "vllm"); productCode(err) != c.code {
				t.Fatalf("err = %v, want %s", err, c.code)
			}
		})
	}

	pd := parsePDText(t, `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"a"}]},"decode":{"tp":2,"pp":2,"units":[{"node_uuid":"b","followers":[{"node_uuid":"c","gpus":[1,0]}]}]}}}}`)
	mode, err := ValidatePD(pd, "vllm")
	if err != nil {
		t.Fatal(err)
	}
	unit := pd.Units("decode")[0]
	if got := pd.RankGPUs("decode", unit); got != 2 {
		t.Fatalf("TP 2 × PP 2 over two ranks = %d GPUs per rank", got)
	}
	if spec := pd.Parallelism("decode", unit); spec.TP != 2 || spec.PP != 2 || spec.NNodes != 2 {
		t.Fatalf("parallelism = %+v", spec)
	}
	if got := pd.NodeUUIDs(mode); len(got) != 3 || got[0] != "b" || got[1] != "c" || got[2] != "a" {
		t.Fatalf("nodes = %v", got)
	}
	if pd.InstanceCount() != 2 {
		t.Fatalf("a multi-node unit is one instance: %d", pd.InstanceCount())
	}
	nodes := map[string]*store.Node{
		"a": pdNode("a", store.NodeStateOnline, "RTX"),
		"b": pdNode("b", store.NodeStateOnline, "RTX", "RTX"),
		"c": pdNode("c", store.NodeStateOnline, "RTX"),
	}
	if _, err := ValidatePDNodes(pd, mode, nodes, true); productCode(err) != messages.ErrorPDNodeGPUMissing {
		t.Fatalf("a follower pinned to a missing GPU = %v", err)
	}
	nodes["c"] = pdNode("c", store.NodeStateOnline, "RTX", "RTX")
	if _, err := ValidatePDNodes(pd, mode, nodes, true); err != nil {
		t.Fatal(err)
	}
	// The follower's unpinned share counts against its node.
	unpinned := parsePDText(t, `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"a"}]},"decode":{"tp":4,"units":[{"node_uuid":"b","followers":[{"node_uuid":"a"}]}]}}}}`)
	if _, err := ValidatePDNodes(unpinned, mode, nodes, true); productCode(err) != messages.ErrorPDNodeGPUsInsufficient {
		t.Fatalf("node a needs 1 + 2 GPUs of its 1: %v", err)
	}
}

func TestValidatePDTPRuleCodes(t *testing.T) {
	for mode, code := range map[string]string{
		"vllm-nixl":            messages.ErrorPDTPDirection,
		"vllm-ascend-mooncake": messages.ErrorPDTPPrefillAtLeastDecode,
		"sglang-nixl":          messages.ErrorPDTPEqual,
	} {
		prefill, decode := 2, 1
		if code != messages.ErrorPDTPDirection {
			prefill, decode = 1, 2
		}
		text := fmt.Sprintf(`{"pd":{"mode":%q,"roles":{"prefill":{"tp":%d,"units":[{"node_uuid":"a"}]},"decode":{"tp":%d,"units":[{"node_uuid":"b"}]}}}}`, mode, prefill, decode)
		if _, err := ValidatePD(parsePDText(t, text), ""); productCode(err) != code {
			t.Fatalf("%s: err = %v, want %s", mode, err, code)
		}
	}
}

func TestPDOfTellsAnOrdinaryPlacementFromAnUnreadablePDOne(t *testing.T) {
	for _, raw := range []string{"", "null", `{"node_uuid":"a"}`} {
		if pd, err := PDOf(raw); pd != nil || err != nil {
			t.Fatalf("PDOf(%q) = %v, %v; want an ordinary placement", raw, pd, err)
		}
	}
	pd, err := PDOf(`{"pd":{"mode":"vllm-nixl","roles":{"decode":{"units":[{"node_uuid":"b"}]}}}}`)
	if err != nil || pd == nil || pd.Mode != "vllm-nixl" || len(pd.Units("decode")) != 1 {
		t.Fatalf("stored PD placement = %+v, %v", pd, err)
	}
	pd, err = PDOf(map[string]any{"pd": map[string]any{"mode": "vllm-nixl", "roles": map[string]any{}}})
	if err != nil || pd == nil {
		t.Fatalf("requested PD placement = %+v, %v", pd, err)
	}
	for _, raw := range []string{`{"pd":{"mode":"vllm-nixl","bogus":1}}`, `{"pd": [1]}`, `{"pd":`} {
		if pd, err := PDOf(raw); err == nil {
			t.Fatalf("PDOf(%q) = %+v without an error", raw, pd)
		}
	}
}

func TestValidatePDChecksPoliciesAgainstTheMode(t *testing.T) {
	sglang := func(fallback, kv string) *PDPlacement {
		return &PDPlacement{Mode: "sglang-nixl", Fallback: fallback, KVLoadFailure: kv, Roles: map[string]PDRole{
			"prefill": {Units: []PDUnit{{NodeUUID: "a"}}}, "decode": {Units: []PDUnit{{NodeUUID: "b"}}},
		}}
	}
	if _, err := ValidatePD(sglang("", ""), "sglang"); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if p := sglang("", ""); p.EffectiveFallback() != "fail" || p.EffectiveKVLoadFailure() != "" {
		t.Fatalf("sglang defaults = %q / %q", p.EffectiveFallback(), p.EffectiveKVLoadFailure())
	}
	// Its decoder cannot serve alone, and SGLang has no KV load failure setting.
	if _, err := ValidatePD(sglang("decode", ""), "sglang"); productCode(err) != messages.ErrorPDFallbackInvalid {
		t.Fatalf("sglang fallback decode: %v", err)
	}
	if _, err := ValidatePD(sglang("", "fail"), "sglang"); productCode(err) != messages.ErrorPDKVLoadFailureInvalid {
		t.Fatalf("sglang kv_load_failure: %v", err)
	}
}

// TestCheckPDArgsSpellings pins which spellings of the composed vLLM flags
// CheckPDArgs refuses today: the exact flag and its --flag=value form. The
// underscore spelling vLLM's parser also accepts passes, which the KV offload
// backend seam leaves as it is (changing it changes native outcomes).
func TestCheckPDArgsSpellings(t *testing.T) {
	config := `{"kv_connector":"LMCacheConnectorV1","kv_role":"kv_both"}`
	for _, tc := range []struct {
		args    []string
		refused bool
	}{
		{[]string{"--kv-transfer-config", config}, true},
		{[]string{"--kv-transfer-config=" + config}, true},
		{[]string{"--kv_transfer_config", config}, false},
		{[]string{"--kv_transfer_config=" + config}, false},
		{[]string{"--kv-offloading-size", "8"}, true},
		{[]string{"--kv-offloading-size=8"}, true},
		{[]string{"--kv_offloading_size", "8"}, false},
		{[]string{"--kv-offloading-backend", "native"}, true},
		{[]string{"--kv_offloading_backend=native"}, false},
		{[]string{"'--kv-transfer-config'", config}, false},
	} {
		for _, layer := range []string{"template", "user"} {
			layers := [][]string{tc.args, nil}
			if layer == "user" {
				layers = [][]string{nil, tc.args}
			}
			err := CheckPDArgs("vllm", layers...)
			if refused := productCode(err) == messages.ErrorPDConnectorArgConflict; refused != tc.refused {
				t.Fatalf("%s %v: refused=%v (%v), want %v", layer, tc.args, refused, err, tc.refused)
			}
		}
	}
}
