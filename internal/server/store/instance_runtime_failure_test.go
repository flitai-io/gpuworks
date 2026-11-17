package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// The server fails an instance only with a reason: SetInstanceState refuses
// the failed state, SetInstanceFailed refuses an empty code, and a failure
// it records is stored whole and goes on the timeline event next to the
// detail. A later transition clears it.
func TestSetInstanceFailedRequiresAndRecordsItsReason(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateInstance(&Instance{UUID: "server-failed", DeploymentID: 3, State: proto.InstPending}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInstanceState("server-failed", proto.InstFailed, "no reason"); !errors.Is(err, ErrFailedNeedsReason) {
		t.Fatalf("SetInstanceState(failed) = %v", err)
	}
	if err := s.SetInstanceFailed("server-failed", proto.RuntimeFailure{}, "no code"); !errors.Is(err, ErrFailedNeedsReason) {
		t.Fatalf("SetInstanceFailed(no code) = %v", err)
	}
	if in, _ := s.InstanceByUUID("server-failed"); in.State != proto.InstPending {
		t.Fatalf("a refused failure was written: %+v", in)
	}
	rank := 1
	if err := s.SetInstanceFailed("server-failed", proto.RuntimeFailure{Code: proto.ErrorUnitRankNodeOffline, Rank: &rank, Node: "gpu-b", Detail: strings.Repeat("x", 1000)}, "rank 1 on gpu-b failed"); err != nil {
		t.Fatal(err)
	}
	in, _ := s.InstanceByUUID("server-failed")
	f := in.Failure()
	if in.State != proto.InstFailed || in.Message != "rank 1 on gpu-b failed" || f == nil || f.Code != proto.ErrorUnitRankNodeOffline || *f.Rank != 1 || f.Node != "gpu-b" {
		t.Fatalf("row = %+v reason %+v", in, f)
	}
	if len(f.Detail) > proto.MaxFailureEvidenceChars {
		t.Fatalf("stored detail is unbounded: %d bytes", len(f.Detail))
	}
	evs, err := s.InstanceTimeline("server-failed", 0, 10)
	if err != nil || len(evs) == 0 {
		t.Fatalf("timeline = %+v, %v", evs, err)
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(evs[0].Data), &params); err != nil {
		t.Fatal(err)
	}
	if ev := EventFailure(params); params["detail"] != "rank 1 on gpu-b failed" || ev == nil || ev.Code != proto.ErrorUnitRankNodeOffline || ev.Node != "gpu-b" {
		t.Fatalf("event params = %v", params)
	}
	if err := s.SetInstanceState("server-failed", proto.InstRemoving, "retry requested"); err != nil {
		t.Fatal(err)
	}
	if in, _ := s.InstanceByUUID("server-failed"); in.RuntimeFailure != "" || in.Failure() != nil {
		t.Fatalf("a later transition kept the reason: %+v", in)
	}
	evs, _ = s.InstanceTimeline("server-failed", 0, 1)
	params = nil
	_ = json.Unmarshal([]byte(evs[0].Data), &params)
	if _, ok := params[eventFailureParam]; ok {
		t.Fatalf("a transition without a failure carries one: %v", params)
	}
}

// An agent's report is stored in the one spelling and bounded again: an
// older agent's lower-case code, or one that sends more evidence than the
// wire allows, is read the way a current one is.
func TestReportedRuntimeFailureIsNormalizedAndBounded(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateInstance(&Instance{UUID: "reported", State: proto.InstStarting}); err != nil {
		t.Fatal(err)
	}
	failure := &proto.RuntimeFailure{Code: "engine_port_refused", Port: 30000, Evidence: []string{"a", "b", "c", "d", "e"}}
	if err := s.UpdateInstanceStatus(proto.InstanceStatus{UUID: "reported", State: proto.InstFailed, Message: "ENGINE_PORT_REFUSED: port 30000", RuntimeFailure: failure}); err != nil {
		t.Fatal(err)
	}
	in, _ := s.InstanceByUUID("reported")
	f := in.Failure()
	if f == nil || f.Code != proto.ErrorEnginePortRefused || f.Port != 30000 || len(f.Evidence) != proto.MaxFailureEvidence || f.Evidence[0] != "c" {
		t.Fatalf("stored reason = %+v (%s)", f, in.RuntimeFailure)
	}
	if !strings.Contains(in.RuntimeFailure, `"code":"ENGINE_PORT_REFUSED"`) {
		t.Fatalf("column = %s", in.RuntimeFailure)
	}
	// A row written by an older server keeps its lower-case code until read.
	old := Instance{RuntimeFailure: `{"code":"container_exited","exit_code":1}`}
	if f := old.Failure(); f == nil || f.Code != proto.ErrorContainerExited || *f.ExitCode != 1 {
		t.Fatalf("old row reason = %+v", f)
	}
	for _, malformed := range []string{"", "{}", "not json", `{"code":""}`} {
		if (&Instance{RuntimeFailure: malformed}).Failure() != nil {
			t.Fatalf("%q read as a reason", malformed)
		}
	}
}

// An agent that predates RuntimeFailure named a port refusal only as its
// message's prefix. Its failed row reads as the coded reason, with the port;
// nothing else is read from a message: not another prefix, not the refusal
// text elsewhere in it, not a row that is not failed, and never over a
// stored reason.
func TestOldAgentPortRefusalReadsAsItsCode(t *testing.T) {
	old := Instance{State: proto.InstFailed, Message: "ENGINE_PORT_REFUSED: port 30007 is already in use on this node; nothing was started"}
	f := old.Failure()
	if f == nil || f.Code != proto.ErrorEnginePortRefused || f.Port != 30007 || f.Phase != proto.PhaseContainerPrepare {
		t.Fatalf("old refusal = %+v", f)
	}
	for name, in := range map[string]Instance{
		"in use":        {State: proto.InstFailed, Message: "ENGINE_PORT_IN_USE: port 30000 is already in use on this node"},
		"nested":        {State: proto.InstFailed, Message: "rank 1 on n failed: ENGINE_PORT_REFUSED: port 30000"},
		"bare code":     {State: proto.InstFailed, Message: "ENGINE_PORT_REFUSED"},
		"other failure": {State: proto.InstFailed, Message: "image pull timed out"},
		"not failed":    {State: proto.InstRemoving, Message: "ENGINE_PORT_REFUSED: port 30000 is already in use"},
		"empty":         {State: proto.InstFailed},
	} {
		if f := in.Failure(); f != nil {
			t.Errorf("%s: read as %+v", name, f)
		}
	}
	if f := (&Instance{State: proto.InstFailed, Message: "ENGINE_PORT_REFUSED: port 1 x"}).Failure(); f == nil || f.Port != 1 {
		t.Fatalf("short refusal = %+v", f)
	}
	stored := Instance{State: proto.InstFailed, Message: "ENGINE_PORT_REFUSED: port 30000 x", RuntimeFailure: `{"code":"CONTAINER_EXITED"}`}
	if f := stored.Failure(); f == nil || f.Code != proto.ErrorContainerExited {
		t.Fatalf("stored reason was overridden by the message: %+v", f)
	}
	unparsable := Instance{State: proto.InstFailed, Message: "ENGINE_PORT_REFUSED: port many are taken"}
	if f := unparsable.Failure(); f == nil || f.Port != 0 {
		t.Fatalf("unparsable port = %+v", f)
	}
}
