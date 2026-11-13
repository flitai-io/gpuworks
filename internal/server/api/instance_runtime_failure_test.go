package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestInstanceRuntimeFailureWireCompatibility(t *testing.T) {
	raw := `{"uuid":"rejected","state":"failed","message":"finite memlock","runtime_failure":{"code":"ACCELERATOR_RESOURCE_UNAVAILABLE","vendor":"kunlunxin","resource":"memlock"}}`
	var status proto.InstanceStatus
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := json.Marshal(status.RuntimeFailure)
	in := &store.Instance{Message: status.Message, RuntimeFailure: string(snapshot)}
	got := instanceRuntimeFailure(in)
	if got == nil || got.Code != "ACCELERATOR_RESOURCE_UNAVAILABLE" || got.Params["resource"] != "memlock" || got.Params["vendor"] != "Kunlunxin" || got.Message != "finite memlock" {
		t.Fatalf("mapped=%+v", got)
	}
	wire, err := json.Marshal(dto.Instance{Message: in.Message, RuntimeFailure: got})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]json.RawMessage
	if json.Unmarshal(wire, &out) != nil || out["runtime_failure"] == nil || string(out["message"]) != `"finite memlock"` {
		t.Fatalf("wire=%s", wire)
	}
	for _, legacy := range []string{"", "malformed", "{}"} {
		in.RuntimeFailure = legacy
		if instanceRuntimeFailure(in) != nil {
			t.Fatalf("legacy=%q", legacy)
		}
	}
	wire, _ = json.Marshal(dto.Instance{Message: "old agent error"})
	json.Unmarshal(wire, &out)
	var old map[string]json.RawMessage
	json.Unmarshal(wire, &old)
	if old["runtime_failure"] != nil {
		t.Fatalf("legacy shape changed: %s", wire)
	}
}

func TestInstanceRuntimeFailureThroughAuthenticatedRouter(t *testing.T) {
	h := newLaunchFactsHarness(t, "runtime-failure-wire")
	if _, err := h.store.CreateInstance(&store.Instance{UUID: "runtime-refusal", State: proto.InstStarting}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpdateInstanceStatus(proto.InstanceStatus{UUID: "runtime-refusal", State: proto.InstFailed, Message: "XDR missing", RuntimeFailure: &proto.RuntimeFailure{Code: proto.ErrorAcceleratorResourceUnavailable, Vendor: "kunlunxin", Resource: "/dev/xdrdrv"}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path string
		auth         bool
		want         int
	}{
		{http.MethodGet, "/api/v1/instances/runtime-refusal", false, http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/instances/missing", true, http.StatusNotFound},
		{http.MethodPut, "/api/v1/instances/runtime-refusal", true, http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/v1/instances/runtime-refusal", true, http.StatusOK},
	} {
		r := h.request(tc.method, tc.path, tc.auth)
		if r.Code != tc.want {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, r.Code, r.Body.String())
		}
		if tc.want == http.StatusOK {
			var wire struct {
				State   string             `json:"state"`
				Message string             `json:"message"`
				Failure dto.ProductMessage `json:"runtime_failure"`
			}
			if err := json.Unmarshal(r.Body.Bytes(), &wire); err != nil {
				t.Fatal(err)
			}
			if wire.State != "failed" || wire.Message != "XDR missing" || wire.Failure.Code != "ACCELERATOR_RESOURCE_UNAVAILABLE" || wire.Failure.Params["resource"] != "/dev/xdrdrv" {
				t.Fatalf("wire=%+v", wire)
			}
		}
	}
}
