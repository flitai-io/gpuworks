package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/rpccontext"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const connectionRuntimeWire = `{"report":"unsafe raw report","host":{"hostname":"unsafe host"},"findings":[{"code":"bad_token","probe":"unsafe probe"}],"facts":[
{"code":"connection.source","value":"agent_runtime","identity":{"user":"unsafe identity"}},
{"code":"connection.checked_at","value":"2026-11-05T03:00:00.123Z"},
{"code":"connection.connect_mode","value":"agent_dial"},
{"code":"connection.server_url","value":"https://runtime.example/prefix"},
{"code":"connection.server_source","value":"agent_runtime"},
{"code":"connection.token_present","value":"true"},
{"code":"connection.control","value":"connected"},
{"code":"connection.data_lane","value":"pending"},
{"code":"connection.outcome","value":"ok"},
{"code":"connection.reason","value":"none"},
{"code":"connection.phase.dns","value":"not_observed"},
{"code":"connection.phase.session","value":"failed"},
{"code":"connection.elapsed_ms.session","value":"1200"},
{"code":"connection.observed_at.session","value":"2026-11-05T02:59:58Z"},
{"code":"connection.history_state","value":"found"},
{"code":"connection.last_failure.source","value":"agent_log"},
{"code":"connection.last_failure.lane","value":"single"}]}`

func TestNodeConnectionDiagnosticsUnavailableHooksAreNotLegacyEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		online func(string) bool
		caps   func(string) []string
		want   string
	}{
		{"no online observation", nil, nil, "unavailable"},
		{"offline", func(string) bool { return false }, nil, "offline"},
		{"missing RPC and capabilities", func(string) bool { return true }, nil, "unavailable"},
		{"missing RPC", func(string) bool { return true }, func(string) []string { return []string{proto.CapConnectionDiagnostics} }, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := apiHandlers{d: Deps{Online: tc.online, NodeCaps: tc.caps}}
			out := dto.NodeConnectionDiagnostics{}
			check, attempted := h.nodeConnectionAgentReport(context.Background(), &store.Node{UUID: "node"}, &out)
			if attempted || out.AgentReport != nil || check.Outcome != tc.want {
				t.Fatalf("unavailable integration is not an old-agent verdict: %+v", check)
			}
		})
	}
}

func TestNodeConnectionDiagnosticsOnlineWithoutSSHUsesRuntimeWire(t *testing.T) {
	for _, ssh := range []bool{false, true} {
		t.Run(map[bool]string{false: "no SSH", true: "optional SSH not invoked"}[ssh], func(t *testing.T) {
			st, _ := platformKeyAPIStore(t, true)
			node, _, err := st.CreateNodePending("runtime", "agent_dial")
			if err != nil {
				t.Fatal(err)
			}
			if ssh {
				if _, err := st.SetNodeSSHCredential(node.UUID, "node.example", 22, "root", store.NodeSSHSecrets{Password: "private-SSH-password"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.SetNodeReject(node.UUID, "proto_mismatch"); err != nil {
				t.Fatal(err)
			}
			before, _ := st.NodeByUUID(node.UUID)
			installer := &connectionDiagnosticStub{}
			calls := 0
			h := apiHandlers{d: Deps{Store: st, NodeInstaller: installer,
				Online:   func(uuid string) bool { return uuid == node.UUID },
				NodeCaps: func(string) []string { return []string{proto.CapConnectionDiagnostics} },
				CallNode: func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
					calls++
					deadline, ok := ctx.Deadline()
					if uuid != node.UUID || method != proto.RPCNodeConnectionDiagnostics || !ok || time.Until(deadline) > 15*time.Second || time.Until(deadline) <= 0 || rpccontext.Timeout(ctx, 0) != 15*time.Second {
						t.Fatalf("wrong RPC identity/budget: %s %s %v", uuid, method, deadline)
					}
					body, _ := json.Marshal(params)
					if string(body) != "{}" {
						t.Fatalf("unexpected RPC parameters: %s", body)
					}
					return json.RawMessage(connectionRuntimeWire), nil
				}}}
			req := withNodeRecoveryURLParam(httptest.NewRequest(http.MethodPost, "/", nil), "uuid", node.UUID)
			rec := httptest.NewRecorder()
			h.nodeConnectionDiagnostics(rec, req)
			if rec.Code != 200 {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			var out dto.NodeConnectionDiagnostics
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || installer.calls != 0 || len(out.Checks) != 4 || out.Checks[0].Outcome != "runtime_snapshot" || out.AgentReport == nil {
				t.Fatalf("runtime report missing or SSH invoked: %s", rec.Body)
			}
			if proto.ConnectionValue(*out.AgentReport, "connection.checked_at") != "2026-11-05T03:00:00.123Z" || out.LastRejectReason != "proto_mismatch" || out.LastRejectAt == nil {
				t.Fatalf("lost observation provenance: %s", rec.Body)
			}
			for _, forbidden := range []string{"unsafe", "history_state", "last_failure", `"host"`, `"findings"`, `"identity"`, node.Token} {
				if strings.Contains(rec.Body.String(), forbidden) {
					t.Fatalf("unsafe or CLI evidence escaped: %s", rec.Body)
				}
			}
			if out.AgentReport.Report != "" || proto.ConnectionValue(*out.AgentReport, "connection.data_lane") != "pending" {
				t.Fatal("unexpected current snapshot")
			}
			after, _ := st.NodeByUUID(node.UUID)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("runtime diagnostic changed node state")
			}
		})
	}
}

func TestNodeConnectionDiagnosticsRuntimePartialMatrix(t *testing.T) {
	for _, tc := range []struct {
		name        string
		online, cap bool
		callErr     error
		want        string
		rpc, ssh    int
	}{
		{"offline", false, true, nil, "offline", 0, 1},
		{"legacy", true, false, nil, "unsupported", 0, 1},
		{"cap read then old session", true, true, proto.UnknownRPCMethod(proto.RPCNodeConnectionDiagnostics), "unsupported", 1, 0},
		{"timeout", true, true, context.DeadlineExceeded, "timeout", 1, 0},
		{"cancelled", true, true, context.Canceled, "cancelled", 1, 0},
		{"disconnect", true, true, errors.New("node session closed: private-token"), "unavailable", 1, 0},
		{"malicious echo", true, true, errors.New("https://user:secret@example/?token=private-token"), "unavailable", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := platformKeyAPIStore(t, true)
			node, _, err := st.CreateNodePending("partial", "agent_dial")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.SetNodeSSHCredential(node.UUID, "node.example", 22, "root", store.NodeSSHSecrets{Password: "private-SSH-password"}); err != nil {
				t.Fatal(err)
			}
			installer := &connectionDiagnosticStub{}
			calls := 0
			h := apiHandlers{d: Deps{Store: st, NodeInstaller: installer,
				Online: func(string) bool { return tc.online }, NodeCaps: func(string) []string {
					if tc.cap {
						return []string{proto.CapConnectionDiagnostics}
					}
					return nil
				},
				CallNode: func(_ context.Context, _, method string, _ any) (json.RawMessage, error) {
					calls++
					if method != proto.RPCNodeConnectionDiagnostics {
						t.Fatal("fell back to full doctor")
					}
					return nil, tc.callErr
				},
			}}
			rec := httptest.NewRecorder()
			h.nodeConnectionDiagnostics(rec, withNodeRecoveryURLParam(httptest.NewRequest(http.MethodPost, "/", nil), "uuid", node.UUID))
			var out dto.NodeConnectionDiagnostics
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			if rec.Code != 200 || len(out.Checks) != 4 || out.Checks[0].Outcome != tc.want || calls != tc.rpc || installer.calls != tc.ssh || out.AgentReport != nil {
				t.Fatalf("partial mismatch: rpc=%d ssh=%d %s", calls, installer.calls, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "private-token") || strings.Contains(rec.Body.String(), "user:secret") {
				t.Fatal("raw RPC error escaped")
			}
		})
	}
}

func TestNodeConnectionDiagnosticsDiscardsCancelledLateRPCResult(t *testing.T) {
	st, _ := platformKeyAPIStore(t, true)
	node, _, _ := st.CreateNodePending("late", "agent_dial")
	ctx, cancel := context.WithCancel(context.Background())
	h := apiHandlers{d: Deps{Store: st, Online: func(string) bool { return true }, NodeCaps: func(string) []string { return []string{proto.CapConnectionDiagnostics} }, CallNode: func(context.Context, string, string, any) (json.RawMessage, error) {
		cancel()
		return json.RawMessage(connectionRuntimeWire), nil
	}}}
	rec := httptest.NewRecorder()
	req := withNodeRecoveryURLParam(httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx), "uuid", node.UUID)
	h.nodeConnectionDiagnostics(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"outcome":"cancelled"`) || strings.Contains(rec.Body.String(), `"agent_report"`) {
		t.Fatalf("late RPC accepted: %s", rec.Body)
	}
}

func TestNodeConnectionDiagnosticsRuntimeWireBudgetsAndSafeProjection(t *testing.T) {
	appendFact := func(wire, code, value string) string {
		c, _ := json.Marshal(code)
		v, _ := json.Marshal(value)
		return strings.TrimSuffix(wire, "]}") + `,{"code":` + string(c) + `,"value":` + string(v) + `}]}`
	}
	for _, tc := range []struct {
		name, wire string
		rejected   bool
	}{
		{"malformed", "{broken", true},
		{"legacy report", `{"report":"raw secret"}`, true},
		{"CLI source", strings.Replace(connectionRuntimeWire, `"agent_runtime"`, `"cli"`, 1), true},
		{"bad timestamp", strings.Replace(connectionRuntimeWire, "2026-11-05T03:00:00.123Z", "not-a-date", 1), true},
		{"duplicate accepted field", appendFact(connectionRuntimeWire, "connection.control", "not_connected"), true},
		{"oversize accepted value", appendFact(connectionRuntimeWire, "connection.proxy_url", strings.Repeat("x", 2049)), true},
		{"oversize document", `{"report":"` + strings.Repeat("x", 32768) + `"}`, true},
		{"unknown fields", appendFact(connectionRuntimeWire, "connection.credentials", "secret-token"), false},
		{"unsafe optional URL", appendFact(connectionRuntimeWire, "connection.proxy_url", "https://user:secret-token@proxy/?token=secret-token"), false},
		{"proxy path", appendFact(connectionRuntimeWire, "connection.proxy_url", "https://proxy/private"), false},
		{"unknown stage", appendFact(connectionRuntimeWire, "connection.phase.untrusted", "secret-token"), false},
		{"invalid duration", appendFact(connectionRuntimeWire, "connection.elapsed_ms.dns", "-1"), false},
		{"overflow duration", appendFact(connectionRuntimeWire, "connection.elapsed_ms.dns", "86400001"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := safeNodeConnectionReport(json.RawMessage(tc.wire), "secret-token")
			if (out == nil) != tc.rejected {
				t.Fatalf("rejected=%v report=%+v", tc.rejected, out)
			}
			if out != nil {
				wire, _ := json.Marshal(out)
				for _, text := range []string{"secret-token", "unsafe", "history_state", "last_failure", "phase.untrusted", `"value":"-1"`, "86400001", "proxy/private"} {
					if strings.Contains(string(wire), text) {
						t.Fatalf("unsafe projection: %s", wire)
					}
				}
			}
		})
	}
	wire := connectionRuntimeWire
	for i := 0; i < 48; i++ {
		wire = appendFact(wire, "unknown", "value")
	}
	if safeNodeConnectionReport(json.RawMessage(wire), "") != nil {
		t.Fatal("more than 64 facts accepted")
	}
	for _, value := range []string{"https://example/secret-token", "https://example/%73ecret-token", "https://example/\nsecret", "https://user:secret@example", "https://example?token=hidden"} {
		if safeNodeConnectionURL(value, "secret-token", false) != "" {
			t.Fatalf("unsafe URL accepted: %q", value)
		}
	}
}
