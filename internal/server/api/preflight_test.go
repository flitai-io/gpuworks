package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestProbeTargetUsesHostOnlyAddress(t *testing.T) {
	for input, want := range map[string]string{
		"10.0.0.2":     "10.0.0.2:29500",
		"2001:db8::10": "[2001:db8::10]:29500",
	} {
		got, err := probeTarget(input, 29500)
		if err != nil || got != want {
			t.Fatalf("probeTarget(%q)=%q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := probeTarget("10.0.0.2:19090", 29500); err == nil {
		t.Fatal("address with port was accepted")
	}
}

func TestProbeTargetRequiresPort(t *testing.T) {
	if _, err := probeTarget("10.0.0.2", 0); err == nil {
		t.Fatal("expected missing port error")
	}
}

func TestDistributedPreflightOrchestratesAuthenticatedDirectedPairs(t *testing.T) {
	st, err := store.Open("file:preflight_orchestration_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	hosts := map[string]string{"node-a": "10.0.0.1", "node-b": "10.0.0.2"}
	for uuid, host := range hosts {
		node, err := st.RegisterNode(uuid, store.HashToken("token"), uuid)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{Network: testManagement(host)}); err != nil {
			t.Fatal(err)
		}
	}

	tokens := map[string]string{}
	starts, probes, stops := 0, 0, 0
	call := func(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
		raw, _ := json.Marshal(params)
		switch method {
		case proto.RPCNetBenchStart:
			var p netBenchStartParamsForTest
			_ = json.Unmarshal(raw, &p)
			if len(p.Token) != 64 || p.Port != 29500 || p.TTLSeconds < 60 {
				t.Fatalf("start params = %+v", p)
			}
			tokens[uuid] = p.Token
			starts++
			return json.RawMessage(`{"port":29500}`), nil
		case proto.RPCNetProbe:
			var p struct {
				Target    string `json:"target"`
				Token     string `json:"token"`
				Bandwidth bool   `json:"bandwidth"`
				Seconds   int    `json:"seconds"`
			}
			_ = json.Unmarshal(raw, &p)
			targetUUID := "node-a"
			if strings.HasPrefix(p.Target, hosts["node-b"]+":") {
				targetUUID = "node-b"
			}
			if p.Token != tokens[targetUUID] || !p.Bandwidth || p.Seconds != 1 {
				t.Fatalf("probe params = %+v target=%s", p, targetUUID)
			}
			probes++
			return json.RawMessage(`{"reachable":true,"rtt_ms_p50":0.4,"rtt_ms_p99":0.8,"gbps":12.5,"bandwidth_tested":true,"bytes_sent":1000,"bytes_received":1000,"duration_seconds":1}`), nil
		case proto.RPCNetBenchStop:
			var p struct {
				Token string `json:"token"`
			}
			_ = json.Unmarshal(raw, &p)
			if p.Token != tokens[uuid] {
				t.Fatalf("stop token mismatch for %s", uuid)
			}
			stops++
			return json.RawMessage(`{"stopped":true}`), nil
		default:
			t.Fatalf("unexpected method %q", method)
			return nil, nil
		}
	}
	h := apiHandlers{d: Deps{Store: st, Online: func(string) bool { return true }, CallNode: call}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/preflight", bytes.NewBufferString(`{"node_uuids":["node-a","node-b"],"port":29500,"master_addr":"10.0.0.9","bandwidth":true,"seconds":1}`))
	rec := httptest.NewRecorder()
	h.distributedPreflight(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result distributedPreflightResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || len(result.Pairs) != 2 || starts != 2 || probes != 2 || stops != 2 {
		t.Fatalf("result=%+v starts=%d probes=%d stops=%d", result, starts, probes, stops)
	}
	for _, pair := range result.Pairs {
		if pair.ID != "node_pair" || pair.Status != proto.CheckPass || pair.Code != messages.CheckPreflightPairReachable || pair.Params.Source != pair.SourceUUID {
			t.Fatalf("pair check=%+v", pair)
		}
		if !pair.Reachable || !pair.BandwidthTested || pair.Gbps != 12.5 || pair.BytesReceived != 1000 {
			t.Fatalf("pair=%+v", pair)
		}
		if pair.TargetUUID == "node-a" && !strings.HasPrefix(pair.Target, "10.0.0.9:") {
			t.Fatalf("leader target did not use master_addr: %+v", pair)
		}
	}
}

func TestPreflightPairRequiresRequestedBandwidth(t *testing.T) {
	result := distributedPreflightResponse{OK: true}
	result.addPair(preflightPair{SourceUUID: "a", TargetUUID: "b", Target: "10.0.0.2:29500", Reachable: true, RTTP50MS: 1}, true, 29500)
	if result.OK || len(result.Pairs) != 1 || result.Pairs[0].Status != proto.CheckFail || result.Pairs[0].Code != messages.CheckPreflightBandwidthNotMeasured {
		t.Fatalf("result=%+v", result)
	}

	latencyOnly := distributedPreflightResponse{OK: true}
	latencyOnly.addPair(preflightPair{SourceUUID: "a", TargetUUID: "b", Reachable: true, RTTP50MS: 1}, false, 29500)
	if !latencyOnly.OK || latencyOnly.Pairs[0].Status != proto.CheckPass {
		t.Fatalf("latency-only result=%+v", latencyOnly)
	}
}

// Every way a pair fails has its own code; what the agent or the RPC said is
// evidence, never the message.
func TestPreflightPairFailureCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		pair preflightPair
		code string
	}{
		{"listener", preflightPair{failure: messages.CheckPreflightListenerFailed, Evidence: "node offline"}, messages.CheckPreflightListenerFailed},
		{"probe call", preflightPair{failure: messages.CheckPreflightProbeFailed, Evidence: "rpc timeout"}, messages.CheckPreflightProbeFailed},
		{"unreachable", preflightPair{Evidence: "dial tcp 10.0.0.2:29500: connection refused"}, messages.CheckPreflightPairUnreachable},
		{"agent error", preflightPair{Reachable: true, Evidence: "token rejected"}, messages.CheckPreflightPairUnreachable},
		{"bandwidth", preflightPair{Reachable: true, BandwidthTested: true, BandwidthEvidence: "short read"}, messages.CheckPreflightBandwidthNotMeasured},
	} {
		result := distributedPreflightResponse{OK: true}
		tc.pair.SourceUUID, tc.pair.TargetUUID, tc.pair.Target = "a", "b", "10.0.0.2:29500"
		result.addPair(tc.pair, true, 29500)
		got := result.Pairs[0]
		if result.OK || got.Status != proto.CheckFail || got.Code != tc.code || got.ID != "node_pair" {
			t.Fatalf("%s: %+v", tc.name, got)
		}
		if strings.Contains(got.Message, tc.pair.Evidence) && tc.pair.Evidence != "" || got.Evidence != tc.pair.Evidence {
			t.Fatalf("%s: evidence leaked into the message or was lost: %+v", tc.name, got)
		}
	}
}

func TestPreflightPairWireShape(t *testing.T) {
	result := distributedPreflightResponse{OK: true}
	result.addPair(preflightPair{SourceUUID: "a", TargetUUID: "b", Target: "10.0.0.2:29500", failure: messages.CheckPreflightListenerFailed, Evidence: "listener opened unexpected port 1"}, false, 29500)
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"ok":false,"pairs":[{"id":"node_pair","status":"fail","code":"PREFLIGHT_LISTENER_FAILED",` +
		`"params":{"address":"10.0.0.2:29500","port":29500,"source":"a","target":"b"},"message":"node b did not open the probe listener on port 29500",` +
		`"source_uuid":"a","target_uuid":"b","target":"10.0.0.2:29500","reachable":false,"rtt_ms_p50":0,"rtt_ms_p99":0,"bandwidth_tested":false,` +
		`"evidence":"listener opened unexpected port 1"}]}`
	if string(encoded) != want {
		t.Fatalf("wire shape\n got %s\nwant %s", encoded, want)
	}
	var decoded distributedPreflightResponse
	if err := json.Unmarshal([]byte(want), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.OK || decoded.Pairs[0].Code != messages.CheckPreflightListenerFailed || decoded.Pairs[0].Evidence != "listener opened unexpected port 1" || decoded.Pairs[0].Params != (preflightPairParams{Address: "10.0.0.2:29500", Port: 29500, Source: "a", Target: "b"}) {
		t.Fatalf("round trip = %+v", decoded)
	}
}

func TestDistributedPreflightRejectsDuplicateNodes(t *testing.T) {
	h := apiHandlers{d: Deps{CallNode: func(context.Context, string, string, any) (json.RawMessage, error) { return nil, nil }}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/preflight", bytes.NewBufferString(`{"node_uuids":["same","same"]}`))
	rec := httptest.NewRecorder()
	h.distributedPreflight(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

type netBenchStartParamsForTest struct {
	Port       int    `json:"port"`
	Token      string `json:"token"`
	TTLSeconds int    `json:"ttl_seconds"`
}
