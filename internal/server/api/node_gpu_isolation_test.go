package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The commands are what an operator pastes: their text is the contract, so a
// change to one is a change to this list.
const (
	wantDevicePluginPatch = `kubectl patch clusterpolicy "$(kubectl get clusterpolicy -o jsonpath='{.items[0].metadata.name}')" --type=merge -p '{"spec":{"devicePlugin":{"enabled":true}}}'`
	wantLegacyAppendPatch = `kubectl patch clusterpolicy "$(kubectl get clusterpolicy -o jsonpath='{.items[0].metadata.name}')" --type=json -p '[{"op":"add","path":"/spec/toolkit/env/-","value":{"name":"NVIDIA_CONTAINER_RUNTIME_MODE","value":"legacy"}}]'`
	wantLegacyCreatePatch = `kubectl patch clusterpolicy "$(kubectl get clusterpolicy -o jsonpath='{.items[0].metadata.name}')" --type=json -p '[{"op":"add","path":"/spec/toolkit/env","value":[{"name":"NVIDIA_CONTAINER_RUNTIME_MODE","value":"legacy"}]}]'`
	wantHostLegacyCtk     = `sudo nvidia-ctk config --in-place --set nvidia-container-runtime.mode=legacy`
	wantHostRestart       = `sudo systemctl restart containerd`
)

func TestGPUIsolationFixesFollowWhoseToolkitTheAgentFound(t *testing.T) {
	cases := []struct {
		toolkit string
		want    []string // fix code: commands, in order
	}{
		{proto.NVIDIAToolkitGPUOperator, []string{
			"gpu_operator_device_plugin: " + wantDevicePluginPatch,
			"gpu_operator_toolkit_legacy_append: " + wantLegacyAppendPatch,
			"gpu_operator_toolkit_legacy_create: " + wantLegacyCreatePatch,
		}},
		{proto.NVIDIAToolkitHost, []string{
			"host_toolkit_legacy: " + wantHostLegacyCtk + " | " + wantHostRestart,
		}},
		{"", []string{
			"gpu_operator_device_plugin: " + wantDevicePluginPatch,
			"gpu_operator_toolkit_legacy_append: " + wantLegacyAppendPatch,
			"gpu_operator_toolkit_legacy_create: " + wantLegacyCreatePatch,
			"host_toolkit_legacy: " + wantHostLegacyCtk + " | " + wantHostRestart,
		}},
	}
	for _, tc := range cases {
		got := nodeGPUIsolationDTO(&proto.NVIDIAIsolation{Level: "soft", Reason: "cdi_names_missing", Toolkit: tc.toolkit, Cards: 2})
		var lines []string
		for _, fix := range got.Fixes {
			lines = append(lines, fix.Code+": "+strings.Join(fix.Commands, " | "))
		}
		if strings.Join(lines, "\n") != strings.Join(tc.want, "\n") {
			t.Fatalf("toolkit %q fixes:\n%s\nwant:\n%s", tc.toolkit, strings.Join(lines, "\n"), strings.Join(tc.want, "\n"))
		}
	}
	if nodeGPUIsolationDTO(nil) != nil {
		t.Fatal("a strict node got a notice")
	}
	// Never NVIDIA's device plugin next to another one: every command that
	// touches the device plugin only keeps the Operator's own enabled.
	for _, fix := range nodeGPUIsolationDTO(&proto.NVIDIAIsolation{Level: "soft"}).Fixes {
		for _, c := range fix.Commands {
			if strings.Contains(c, "helm") || strings.Contains(c, "nvidia-device-plugin") {
				t.Fatalf("%s offers to install a device plugin: %s", fix.Code, c)
			}
		}
	}
}

// The node page carries the notice an online node reported, as
// literal wire JSON, and nothing for a node that did not; the list never does.
func TestNodeAPICarriesTheGPUIsolationNoticeAsWireJSON(t *testing.T) {
	st, err := store.Open("file:node_gpu_isolation_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, err := auth.NewTokenSigner("node-gpu-isolation", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := managementSessionTestToken(t, st, signer, auth.Claims{Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	for _, uuid := range []string{"soft", "strict"} {
		if _, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
	}
	runtime := &fakeNodeRuntime{isolation: map[string]*proto.NVIDIAIsolation{
		"soft": {Level: "soft", Reason: "cdi_names_missing", Toolkit: "host", Cards: 2},
	}}
	router := Router(Deps{Store: st, AuthSigner: signer, Online: func(string) bool { return true }, NodeRuntime: runtime})
	get := func(path string) []byte {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+admin)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body)
		}
		return rec.Body.Bytes()
	}
	var soft map[string]json.RawMessage
	if err := json.Unmarshal(get("/api/v1/nodes/soft"), &soft); err != nil {
		t.Fatal(err)
	}
	want := `{"level":"soft","reason":"cdi_names_missing","toolkit":"host","cards":2,"fixes":[` +
		`{"code":"host_toolkit_legacy","commands":["sudo nvidia-ctk config --in-place --set nvidia-container-runtime.mode=legacy","sudo systemctl restart containerd"]}]}`
	jsonEqual(t, soft["gpu_isolation"], want)
	var strict map[string]json.RawMessage
	if err := json.Unmarshal(get("/api/v1/nodes/strict"), &strict); err != nil {
		t.Fatal(err)
	}
	if raw, present := strict["gpu_isolation"]; present {
		t.Fatalf("a strict node carries gpu_isolation %s", raw)
	}
	var list struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(get("/api/v1/nodes"), &list); err != nil {
		t.Fatal(err)
	}
	// The list stays lean: the notice and its commands are the detail's.
	for _, item := range list.Items {
		if raw, present := item["gpu_isolation"]; present {
			t.Fatalf("the node list carries gpu_isolation %s", raw)
		}
	}
	if len(list.Items) != 2 {
		t.Fatalf("list has %d nodes", len(list.Items))
	}
}
