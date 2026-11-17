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

// The instance DTO's runtime_failure is a product message whose params are
// the reason's typed fields: literal wire JSON, exact keys and values, and a
// round trip. Causes are product messages of their own.
func TestInstanceRuntimeFailureLiteralWire(t *testing.T) {
	exit, device := 137, 0
	failure := proto.RuntimeFailure{
		Code: proto.ErrorContainerExited, Phase: proto.PhaseHealthCheck, ExitCode: &exit,
		Causes: []proto.FailureCause{
			{Code: "DGPU_MEMORY_LIMIT_EXCEEDED", Device: &device, NeededBytes: 5429658624, LimitBytes: 4294967296, Evidence: []string{"HAMI-core ERROR Device 0 OOM 5429658624 / 4294967296"}},
			{Code: "GLIBC_TOO_OLD", GLIBC: "GLIBC_2.33"},
		},
		Evidence: []string{"RuntimeError: CUDA error"},
	}
	raw, _ := json.Marshal(failure)
	got := instanceRuntimeFailure(&store.Instance{Message: "container exited (code 137)", RuntimeFailure: string(raw)})
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"code":"CONTAINER_EXITED","params":{` +
		`"arch":"",` +
		`"causes":[{"code":"DGPU_MEMORY_LIMIT_EXCEEDED","params":{"device":0,"evidence":["HAMI-core ERROR Device 0 OOM 5429658624 / 4294967296"],"limit_bytes":4294967296,"needed_bytes":5429658624},"message":"dGPU memory limit reached on GPU 0: the engine needed 5.1 GiB, its limit is 4.0 GiB"},` +
		`{"code":"GLIBC_TOO_OLD","params":{"glibc":"GLIBC_2.33"},"message":"container glibc too old: missing GLIBC_2.33; Ascend dGPU images require glibc 2.33 or newer"}],` +
		`"detail":"","evidence":["RuntimeError: CUDA error"],"exit_code":137,"feature":"","node":"","os":"","phase":"health_check",` +
		`"required_arch":"","required_os":"","required_runtime":"","resource":"","runtime":"","vendor":""},` +
		`"message":"container exited (code 137)"}`
	if string(wire) != want {
		t.Fatalf("wire = %s\nwant  %s", wire, want)
	}
	var back dto.ProductMessage
	if err := json.Unmarshal(wire, &back); err != nil || back.Code != proto.ErrorContainerExited || back.Params["exit_code"] != float64(137) || back.Message != "container exited (code 137)" {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
}

// Rows written before reasons were general: a failure from an older agent
// has no runtime_failure (its message is all there is), and a stored code an
// older agent spelled in lower case is read in the one spelling.
func TestInstanceRuntimeFailureOldRows(t *testing.T) {
	if got := instanceRuntimeFailure(&store.Instance{Message: "container exited (code 1)"}); got != nil {
		t.Fatalf("old row = %+v", got)
	}
	got := instanceRuntimeFailure(&store.Instance{Message: "port taken", RuntimeFailure: `{"code":"engine_port_refused","port":30000}`})
	if got == nil || got.Code != proto.ErrorEnginePortRefused || got.Params["port"] != 30000 {
		t.Fatalf("lower-case row = %+v", got)
	}
	// A server-decided failure without a message falls back to its code.
	got = runtimeFailureMessage(proto.RuntimeFailure{Code: proto.ErrorUnitRankGone}, "")
	if got.Message != proto.ErrorUnitRankGone {
		t.Fatalf("fallback = %+v", got)
	}
}

// A failed transition's timeline event carries the reason as a product
// message under params.failure next to its detail; an event without one is
// unchanged.
func TestInstanceEventCarriesTheFailure(t *testing.T) {
	h := newLaunchFactsHarness(t, "runtime-failure-event")
	if _, err := h.store.CreateInstance(&store.Instance{UUID: "event-failure", State: proto.InstStarting}); err != nil {
		t.Fatal(err)
	}
	exit := 2
	if err := h.store.UpdateInstanceStatus(proto.InstanceStatus{UUID: "event-failure", State: proto.InstFailed, Phase: "", Message: "container exited (code 2) with no output",
		RuntimeFailure: &proto.RuntimeFailure{Code: proto.ErrorContainerExited, ExitCode: &exit, Phase: proto.PhaseHealthCheck, Causes: []proto.FailureCause{{Code: "ENGINE_NO_OUTPUT"}}}}); err != nil {
		t.Fatal(err)
	}
	r := h.request(http.MethodGet, "/api/v1/instances/event-failure/events", true)
	if r.Code != http.StatusOK {
		t.Fatalf("events: %d %s", r.Code, r.Body.String())
	}
	var page struct {
		Items []struct {
			Code   string          `json:"code"`
			Params json.RawMessage `json:"params"`
		} `json:"items"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	var failed json.RawMessage
	for _, ev := range page.Items {
		var p map[string]json.RawMessage
		_ = json.Unmarshal(ev.Params, &p)
		if string(p["state"]) == `"failed"` {
			failed = p["failure"]
			if string(p["detail"]) != `"container exited (code 2) with no output"` {
				t.Fatalf("detail = %s", p["detail"])
			}
		} else if p["failure"] != nil {
			t.Fatalf("event %s carries a failure: %s", ev.Code, ev.Params)
		}
	}
	want := `{"code":"CONTAINER_EXITED","params":{"arch":"","causes":[{"code":"ENGINE_NO_OUTPUT","message":"engine no output"}],"detail":"","exit_code":2,"feature":"","node":"","os":"","phase":"health_check","required_arch":"","required_os":"","required_runtime":"","resource":"","runtime":"","vendor":""},"message":"container exited (code 2) with no output"}`
	if string(failed) != want {
		t.Fatalf("event failure = %s\nwant %s", failed, want)
	}
}
