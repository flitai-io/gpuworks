package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/recovery"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The node DTO carries the code of its last recovery error beside the text;
// an error recorded before codes has none and the key is absent.
func TestNodeDTORecoveryCodeWire(t *testing.T) {
	for _, tc := range []struct {
		name string
		node store.Node
		want map[string]any
	}{
		{name: "coded", node: store.Node{UUID: "n", RecoveryPhase: store.RecoveryPhaseProbeFailed, LastRecoveryError: "ssh: unable to authenticate", LastRecoveryCode: messages.ErrorSSHAuthFailed},
			want: map[string]any{"recovery_phase": "probe_failed", "last_recovery_error": "ssh: unable to authenticate", "last_recovery_code": "SSH_AUTH_FAILED"}},
		{name: "old row", node: store.Node{UUID: "n", RecoveryPhase: store.RecoveryPhaseUnreachable, LastRecoveryError: "network: ssh dial: connection refused"},
			want: map[string]any{"recovery_phase": "unreachable", "last_recovery_error": "network: ssh dial: connection refused"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(toNodeDTO(&tc.node))
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			got := map[string]any{}
			for _, key := range []string{"recovery_phase", "last_recovery_error", "last_recovery_code"} {
				if value, ok := wire[key]; ok {
					got[key] = value
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("wire = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// The probe endpoint's outcome carries skipped_code and error_code beside
// their English text, under exactly these keys.
func TestRecoveryProbeOutcomeWire(t *testing.T) {
	trigger := &recoveryTriggerStub{outcome: recovery.Outcome{
		NodeUUID: "node-1", Action: "probe", Phase: store.RecoveryPhaseUnreachable,
		Probe:     provision.ProbeResult{Detail: "ssh dial: connection refused"},
		ErrorCode: messages.ErrorSSHUnreachable, Error: "ssh dial: connection refused",
	}}
	h := apiHandlers{d: Deps{Recovery: trigger}}
	req := withNodeRecoveryURLParam(httptest.NewRequest(http.MethodPost, "/api/v1/nodes/node-1/recovery/probe", nil), "uuid", "node-1")
	rec := httptest.NewRecorder()
	h.probeNodeRecovery(rec, req)
	want := `{"node_uuid":"node-1","phase":"unreachable","action":"probe","probe":{"reachable":false,"agent_running":false,"agent_installed":false,"detail":"ssh dial: connection refused"},"recovered":false,"error_code":"SSH_UNREACHABLE","error":"ssh dial: connection refused"}`
	if rec.Code != http.StatusOK || compactJSON(t, rec.Body.Bytes()) != want {
		t.Fatalf("status=%d body=%s\nwant %s", rec.Code, rec.Body, want)
	}
	var back recovery.Outcome
	if err := json.Unmarshal(rec.Body.Bytes(), &back); err != nil || back.ErrorCode != messages.ErrorSSHUnreachable || back.Error != trigger.outcome.Error {
		t.Fatalf("round trip = %+v (%v)", back, err)
	}

	trigger.outcome = recovery.Outcome{NodeUUID: "node-1", Action: "skipped", SkippedCode: messages.RecoverySkippedNoSSHCredential, SkippedReason: "ssh credential is not configured"}
	rec = httptest.NewRecorder()
	h.probeNodeRecovery(rec, req)
	want = `{"node_uuid":"node-1","action":"skipped","probe":{"reachable":false,"agent_running":false,"agent_installed":false},"recovered":false,"skipped_code":"RECOVERY_SKIPPED_NO_SSH_CREDENTIAL","skipped_reason":"ssh credential is not configured"}`
	if compactJSON(t, rec.Body.Bytes()) != want {
		t.Fatalf("body=%s\nwant %s", rec.Body, want)
	}
}

func compactJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var value json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
