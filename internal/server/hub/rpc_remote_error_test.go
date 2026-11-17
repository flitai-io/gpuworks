package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Responses exactly as agents put them on the wire: a current agent sends a
// code; an agent older than codes sends only the sentence.
func TestRemoteRPCErrorReadsTheCodeAndOlderAgentsText(t *testing.T) {
	for _, tc := range []struct {
		name, method, wire string
		want               error
	}{
		{"coded unknown", proto.RPCFSStat, `{"error":"unknown rpc method: fs.stat","code":"RPC_UNKNOWN_METHOD"}`, proto.ErrRPCUnknownMethod},
		{"coded busy", proto.RPCAgentUpgrade, `{"error":"agent upgrade already in progress","code":"AGENT_UPGRADE_IN_PROGRESS"}`, proto.ErrAgentUpgradeInProgress},
		{"lower-case code", proto.RPCFSStat, `{"error":"x","code":"rpc_unknown_method"}`, proto.ErrRPCUnknownMethod},
		{"coded missing path", proto.RPCFSStat, `{"error":"stat /m/a: no such file or directory","code":"PATH_NOT_FOUND"}`, proto.ErrPathNotFound},
		{"coded bench 404", proto.RPCBenchProbe, `{"error":"models probe returned 404 Not Found","code":"BENCHMARK_ENDPOINT_NOT_FOUND"}`, proto.ErrBenchEndpointNotFound},
		{"older agent unknown", proto.RPCImageLoadURL, `{"error":"unknown rpc method: image.load_url"}`, proto.ErrRPCUnknownMethod},
		{"older agent busy", proto.RPCAgentUpgrade, `{"error":"agent upgrade already in progress"}`, proto.ErrAgentUpgradeInProgress},
		{"older agent missing path", proto.RPCFSStat, `{"error":"stat /m/a: no such file or directory"}`, proto.ErrPathNotFound},
		{"older agent bench 404", proto.RPCBenchProbe, `{"error":"models probe returned 404 Not Found"}`, proto.ErrBenchEndpointNotFound},
		// The legacy sentence only counts for the RPC whose older agents
		// wrote it.
		{"missing path text from another rpc", proto.RPCFSList, `{"error":"stat /m/a: no such file or directory"}`, nil},
		{"plain refusal", proto.RPCFSStat, `{"error":"path is required"}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var resp rpcResponse
			if err := json.Unmarshal([]byte(tc.wire), &resp); err != nil {
				t.Fatal(err)
			}
			err := fmt.Errorf("call: %w", remoteRPCError(tc.method, resp.Code, resp.Error, resp.Params))
			for _, kind := range []error{proto.ErrRPCUnknownMethod, proto.ErrAgentUpgradeInProgress, proto.ErrPathNotFound, proto.ErrBenchEndpointNotFound} {
				if errors.Is(err, kind) != (kind == tc.want) {
					t.Fatalf("errors.Is(%v, %v) = %v", err, kind, kind != tc.want)
				}
			}
			var remote *rpcRemoteError
			if !errors.As(err, &remote) || remote.Error() != resp.Error {
				t.Fatalf("remote error lost: %v", err)
			}
		})
	}
}

// A storage-files refusal, from a current agent (code + detail param) and
// from an older one (text only), reaches the API as the same typed refusal.
func TestRemoteRPCErrorRebuildsAStorageFilesRefusal(t *testing.T) {
	for name, wire := range map[string]string{
		"coded":        `{"error":"storage_files:not_found: files/a","code":"STORAGE_FILES_NOT_FOUND","params":{"detail":"files/a"}}`,
		"older agent":  `{"error":"storage_files:not_found: files/a"}`,
		"prefixed old": `{"error":"rpc: storage_files:not_found: files/a"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var resp rpcResponse
			if err := json.Unmarshal([]byte(wire), &resp); err != nil {
				t.Fatal(err)
			}
			err := fmt.Errorf("call: %w", remoteRPCError(proto.RPCStorageFilesList, resp.Code, resp.Error, resp.Params))
			var refusal *proto.StorageFilesError
			if !errors.As(err, &refusal) || refusal.Code != proto.StorageFilesErrNotFound || refusal.Detail != "files/a" {
				t.Fatalf("refusal = %#v from %v", refusal, err)
			}
		})
	}
}

func TestRPCResponseWireKeys(t *testing.T) {
	raw, err := json.Marshal(rpcResponse{Error: "unknown rpc method: x", Code: proto.ErrorRPCUnknownMethod})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"error":"unknown rpc method: x","code":"RPC_UNKNOWN_METHOD"}` {
		t.Fatalf("wire = %s", raw)
	}
	raw, _ = json.Marshal(rpcResponse{Error: "storage_files:exists: a", Code: "STORAGE_FILES_EXISTS", Params: map[string]any{"detail": "a"}})
	if string(raw) != `{"error":"storage_files:exists: a","code":"STORAGE_FILES_EXISTS","params":{"detail":"a"}}` {
		t.Fatalf("wire = %s", raw)
	}
	raw, _ = json.Marshal(proto.AgentUpgradeStatus{State: "failed", Error: "agent upgrade already in progress", Code: proto.ErrorAgentUpgradeInProgress})
	if string(raw) != `{"state":"failed","error":"agent upgrade already in progress","code":"AGENT_UPGRADE_IN_PROGRESS"}` {
		t.Fatalf("upgrade status wire = %s", raw)
	}
	raw, _ = json.Marshal(proto.AgentUpgradeStatus{State: "verified"})
	if string(raw) != `{"state":"verified"}` {
		t.Fatalf("code must be omitted when empty: %s", raw)
	}
}

func TestUpgradeStatusCodeReadsTheCodeAndOlderAgentsText(t *testing.T) {
	for wire, want := range map[string]string{
		`{"state":"failed","error":"rolled back: new binary never registered","code":"AGENT_UPGRADE_ROLLED_BACK"}`: proto.ErrorAgentUpgradeRolledBack,
		`{"state":"failed","error":"rolled back: new binary never registered"}`:                                    proto.ErrorAgentUpgradeRolledBack,
		`{"state":"failed","error":"agent upgrade already in progress"}`:                                           proto.ErrorAgentUpgradeInProgress,
		`{"state":"failed","error":"self-upgrade requires server URL and node token"}`:                             "",
	} {
		var status proto.AgentUpgradeStatus
		if err := json.Unmarshal([]byte(wire), &status); err != nil {
			t.Fatal(err)
		}
		if got := upgradeStatusCode(status); got != want {
			t.Errorf("%s -> %q, want %q", wire, got, want)
		}
	}
}

// A coded agent refusal answers the API as a ProductError with the agent's
// code and params, so writeErr passes them to the console; the proto sentinel
// still matches through it. A storage-files refusal keeps its own type.
func TestRemoteRPCErrorAnswersTheAPIWithTheAgentsCode(t *testing.T) {
	err := fmt.Errorf("stat: %w", remoteRPCError(proto.RPCFSStat, proto.ErrorPathNotFound, "stat /m/a: no such file or directory", map[string]any{"path": "/m/a"}))
	var product *messages.ProductError
	if !errors.As(err, &product) || product.Code() != proto.ErrorPathNotFound || product.Params()["path"] != "/m/a" || product.Error() != "stat /m/a: no such file or directory" {
		t.Fatalf("product error = %#v", product)
	}
	if !errors.Is(err, proto.ErrPathNotFound) {
		t.Fatal("the proto sentinel no longer matches")
	}
	legacy := remoteRPCError(proto.RPCImageLoadURL, "", "unknown rpc method: image.load_url", nil)
	if !errors.As(legacy, &product) || product.Code() != proto.ErrorRPCUnknownMethod {
		t.Fatalf("older agent's refusal = %#v", legacy)
	}
	files := remoteRPCError(proto.RPCStorageFilesList, "STORAGE_FILES_NOT_FOUND", "storage_files:not_found: a", map[string]any{"detail": "a"})
	if errors.As(files, &product) {
		t.Fatal("a storage-files refusal is mapped by the storage-files API, not as a ProductError")
	}
	if plain := remoteRPCError(proto.RPCFSList, "", "path is required", nil); errors.As(plain, &product) {
		t.Fatal("an uncoded refusal has no code to pass")
	}
}
