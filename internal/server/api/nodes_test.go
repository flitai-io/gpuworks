package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/rpccontext"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestNodeDTOIncludesGPUVendorAndArchID(t *testing.T) {
	n := &store.Node{
		UUID: "node-1", GPUVendor: "nvidia", GPUDriver: "550.54.15", NvidiaDriver: "550.54.15",
		RuntimeDriver: "docker", RuntimePlugin: "NVIDIA Container Toolkit 1.20.0", Runtimes: `["docker"]`, PartitionAssets: `[{"backend":"nvidia-dgpu","root":"embedded://dgpu","files":["libvgpu.so"]}]`, PartitionSafetyProtocol: 1,
		ConnectMode: "server_dial", ServerDialAddress: "10.0.0.8:19090",
		GPUs: []store.GPU{{
			Index: 0, UUID: "gpu-1", Name: "NVIDIA H100",
			ComputeCapability: "9.0", Vendor: "nvidia", ArchID: "sm_90",
			MIGEnabled: true, MemoryTotal: 80 << 30,
			HealthStatus: "Warning", HealthCode: "81078603", HealthMessage: "port link status change(up to down)",
		}},
	}
	d := toNodeDTO(n)
	if len(d.GPUs) != 1 {
		t.Fatalf("GPUs len = %d", len(d.GPUs))
	}
	if d.GPUs[0].Vendor != "nvidia" || d.GPUs[0].ArchID != "sm_90" {
		t.Fatalf("vendor/arch_id = %q/%q, want nvidia/sm_90", d.GPUs[0].Vendor, d.GPUs[0].ArchID)
	}
	if d.GPUVendor != "nvidia" || d.GPUDriver != "550.54.15" || d.NvidiaDriver != "550.54.15" {
		t.Fatalf("node gpu vendor/driver/nvidia_driver = %q/%q/%q", d.GPUVendor, d.GPUDriver, d.NvidiaDriver)
	}
	if !d.GPUs[0].MIGEnabled || len(d.GPUs[0].PartitionCapabilities) != 1 || d.GPUs[0].PartitionCapabilities[0].Status != "ready" {
		t.Fatalf("gpu capability/mig = %+v", d.GPUs[0])
	}
	// A warning serves: the level is what decides.
	if !d.GPUs[0].Schedulable || d.GPUs[0].HealthStatus != "Warning" || d.GPUs[0].HealthCode != "81078603" || d.GPUs[0].HealthMessage == "" {
		t.Fatalf("gpu health detail = %+v", d.GPUs[0])
	}
	if d.RuntimeDriver != "docker" || len(d.Runtimes) != 1 || d.Runtimes[0] != "docker" || len(d.PartitionCapabilities) != 1 || d.PartitionCapabilities[0].Backend != "nvidia-dgpu" {
		t.Fatalf("node capabilities = %+v", d)
	}
	if d.RuntimePlugin != "NVIDIA Container Toolkit 1.20.0" {
		t.Fatalf("runtime plugin = %q", d.RuntimePlugin)
	}
	if d.ConnectMode != "server_dial" || d.ServerDialAddress != "10.0.0.8:19090" {
		t.Fatalf("node connection = %q/%q", d.ConnectMode, d.ServerDialAddress)
	}
}

// The architecture in the node DTO is derived from the stored raw facts:
// NVIDIA's from its compute capability (the stored arch_id is empty), every
// other vendor's from its arch_id; an identity this build does not know has
// no family.
func TestNodeDTODerivesGPUArchitecture(t *testing.T) {
	n := &store.Node{UUID: "node-arch", GPUs: []store.GPU{
		{Index: 0, UUID: "gpu-0", Name: "Quadro RTX 8000", ComputeCapability: "7.5", Vendor: "nvidia"},
		{Index: 1, UUID: "ascend-1", Name: "910C", Vendor: "ascend", ArchID: "910C"},
		{Index: 2, UUID: "GPU-k100", Name: "K100", Vendor: "hygon", ArchID: "K100"},
		{Index: 3, UUID: "ascend-3", Name: "Ascend910", Vendor: "ascend", ArchID: "Ascend910"},
	}}
	raw, err := json.Marshal(toNodeDTO(n))
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		GPUs []map[string]any `json:"gpus"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	want := []struct{ name, archID, family string }{
		{"Quadro RTX 8000", "sm_75", "turing"},
		{"910C", "910C", "910c"},
		{"K100", "K100", "k100"},
		{"Ascend910", "Ascend910", ""},
	}
	if len(wire.GPUs) != len(want) {
		t.Fatalf("gpus = %s", raw)
	}
	for i, w := range want {
		g := wire.GPUs[i]
		if g["name"] != w.name || g["arch_id"] != w.archID || g["arch_family"] != w.family {
			t.Errorf("gpu %d wire = name %v arch_id %v arch_family %v, want %+v", i, g["name"], g["arch_id"], g["arch_family"], w)
		}
	}
}

func TestCreateNodeEndpointReturnsPendingTokenAndInstallCommand(t *testing.T) {
	st, err := store.Open("file:api_create_pending_node_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes", bytes.NewReader([]byte(`{"name":"worker-a","server_url":"https://node.example"}`)))
	rec := httptest.NewRecorder()
	h.createNode(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Node struct {
			UUID      string `json:"uuid"`
			Name      string `json:"name"`
			State     string `json:"state"`
			ServerURL string `json:"server_url"`
		} `json:"node"`
		Token          string `json:"token"`
		InstallCommand string `json:"install_command"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Node.UUID == "" || body.Node.Name != "worker-a" || body.Node.State != store.NodeStateConnecting || body.Token == "" || !strings.Contains(body.InstallCommand, "--token '"+body.Token+"'") {
		t.Fatalf("create response=%+v", body)
	}
	if body.Node.ServerURL != "https://node.example" {
		t.Fatalf("response node server_url=%q", body.Node.ServerURL)
	}
	if got := strings.Count(body.InstallCommand, "--server "); got != 1 {
		t.Fatalf("install command has %d --server flags: %q", got, body.InstallCommand)
	}
	if !strings.Contains(body.InstallCommand, "'https://node.example'") {
		t.Fatalf("explicit server_url missing from install command: %q", body.InstallCommand)
	}
	node, err := st.NodeByUUID(body.Node.UUID)
	if err != nil || node.ServerURL != "https://node.example" {
		t.Fatalf("persisted server_url=%q err=%v", node.ServerURL, err)
	}
}

// The console always sends the address its browser uses; the server never
// guesses one, so a request without it is refused before any node exists.
func TestCreateNodeRequiresTheServerURL(t *testing.T) {
	st, err := store.Open("file:api_create_node_requires_url_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	for _, body := range []string{`{"name":"no-url"}`, `{"name":"blank-url","server_url":"  "}`} {
		req := httptest.NewRequest(http.MethodPost, "http://internal.example/api/v1/nodes", strings.NewReader(body))
		// Forwarded headers from a proxy no longer stand in for the address.
		req.Header.Set("X-Forwarded-Host", "proxy.example:8443")
		rec := httptest.NewRecorder()
		h.createNode(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusBadRequest || out["code"] != ErrorServerURLRequired {
			t.Fatalf("%s: status = %d body=%s", body, rec.Code, rec.Body.String())
		}
	}
	if nodes, err := st.ListNodes(); err != nil || len(nodes) != 0 {
		t.Fatalf("nodes after refused creates = %+v, %v", nodes, err)
	}
}

func TestCreateNodeServerDialPersistsAddressAndInstallCommand(t *testing.T) {
	st, err := store.Open("file:api_create_node_serverdial_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes", bytes.NewReader([]byte(`{
		"name":"reverse-worker",
		"server_url":"https://control.example",
		"connect_mode":"server_dial",
		"server_dial_listen":":3389",
		"host_address":"203.0.113.20"
	}`)))
	rec := httptest.NewRecorder()
	h.createNode(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Node struct {
			UUID              string `json:"uuid"`
			ServerDialAddress string `json:"server_dial_address"`
		} `json:"node"`
		ServerDialAddress string `json:"server_dial_address"`
		InstallCommand    string `json:"install_command"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Node.UUID == "" || body.Node.ServerDialAddress != "203.0.113.20:3389" || body.ServerDialAddress != "203.0.113.20:3389" {
		t.Fatalf("server_dial address = node=%q response=%q", body.Node.ServerDialAddress, body.ServerDialAddress)
	}
	// The server dials the host, which cannot reach the server: the command
	// runs the offline package and names no server, although the console
	// sent one.
	if !strings.HasPrefix(body.InstallCommand, "sh gpuworks-agent.run --token '") || !strings.HasSuffix(body.InstallCommand, "--connect-mode server_dial --listen ':3389'") ||
		strings.Contains(body.InstallCommand, "control.example") || strings.Contains(body.InstallCommand, "curl") {
		t.Fatalf("install command = %q", body.InstallCommand)
	}
	node, err := st.NodeByUUID(body.Node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if node.ServerURL != "" {
		t.Fatalf("a server_dial node recorded server_url %q", node.ServerURL)
	}
	if node.ServerDialAddress != "203.0.113.20:3389" {
		t.Fatalf("persisted server_dial_address = %q", node.ServerDialAddress)
	}
	redial, err := st.ServerDialNodes()
	if err != nil || len(redial) != 1 || redial[0].UUID != node.UUID {
		t.Fatalf("server_dial pending nodes = %+v, err=%v", redial, err)
	}
}

func TestCreateNodeServerDialRejectsAnInvalidListenAddress(t *testing.T) {
	st, err := store.Open("file:api_create_node_serverdial_listen_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	for _, listen := range []string{"0.0.0.0:70000", "host-only", ":0"} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes", strings.NewReader(`{"connect_mode":"server_dial","server_dial_listen":"`+listen+`","host_address":"203.0.113.20"}`))
		rec := httptest.NewRecorder()
		h.createNode(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusBadRequest || out["code"] != ErrorServerDialListenInvalid {
			t.Fatalf("%s: status = %d body=%s", listen, rec.Code, rec.Body.String())
		}
	}
	if nodes, err := st.ListNodes(); err != nil || len(nodes) != 0 {
		t.Fatalf("refused creates left nodes: %+v, %v", nodes, err)
	}
}

func TestCreateNodeServerDialRejectsInvalidAddress(t *testing.T) {
	st, err := store.Open("file:api_create_node_serverdial_invalid_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes", bytes.NewReader([]byte(`{
		"connect_mode":"server_dial",
		"server_dial_listen":":3389",
		"host_address":"http://bad host:3389"
	}`)))
	rec := httptest.NewRecorder()
	h.createNode(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	nodes, err := st.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("invalid address left pending nodes: %+v", nodes)
	}
}

func TestCreateNodeServerDialRequiresAddress(t *testing.T) {
	st, err := store.Open("file:api_create_node_serverdial_missing_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes", bytes.NewReader([]byte(`{
		"connect_mode":"server_dial",
		"server_dial_listen":":3389"
	}`)))
	rec := httptest.NewRecorder()
	h.createNode(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	nodes, err := st.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("missing address left pending nodes: %+v", nodes)
	}
}

func TestCreateNodeRejectsInvalidServerURLBeforeCreatingPendingNode(t *testing.T) {
	st, err := store.Open("file:api_create_node_invalid_url_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	for _, raw := range []string{
		"control.example",
		"ftp://control.example",
		"https://control.example?x=1",
		"https://user:pass@control.example",
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes", bytes.NewReader([]byte(`{"server_url":"`+raw+`"}`)))
		rec := httptest.NewRecorder()
		h.createNode(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("server_url %q status = %d body=%s", raw, rec.Code, rec.Body.String())
		}
	}
	nodes, err := st.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("invalid server URLs left pending nodes: %+v", nodes)
	}
}

func TestRenameNodeEndpointDoesNotExposeToken(t *testing.T) {
	st, err := store.Open("file:api_rename_node_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	n, err := st.RegisterNode("node-rename", store.HashToken("secret-token"), "node-rename")
	if err != nil {
		t.Fatal(err)
	}
	// Legacy RegisterNode rows have no display token; set one explicitly to
	// make the regression meaningful.
	if err := st.DB().Model(&store.Node{}).Where("id = ?", n.ID).Update("token", "secret-token").Error; err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/nodes/node-rename/name", bytes.NewReader([]byte(`{"name":"renamed"}`)))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"uuid": "node-rename"}))
	rec := httptest.NewRecorder()
	h.renameNode(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["token"]; ok {
		t.Fatalf("rename response leaked token: %v", body["token"])
	}
}

func TestGetNodeOnlyReturnsTokenToAdmin(t *testing.T) {
	st, err := store.Open("file:api_node_token_acl_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	n, token, err := st.CreateNodePending("acl-node")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("node-token-acl-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	call := func(role string) map[string]any {
		t.Helper()
		jwt, signErr := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: role, Role: role})
		if signErr != nil {
			t.Fatal(signErr)
		}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes/"+n.UUID, nil)
		req.Header.Set("Authorization", "Bearer "+jwt)
		req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"uuid": n.UUID}))
		rec := httptest.NewRecorder()
		h := apiHandlers{d: Deps{Store: st, AuthSigner: signer}}
		h.getNode(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("role=%s status=%d body=%s", role, rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	if _, ok := call("user")["token"]; ok {
		t.Fatal("ordinary management user received node token")
	}
	if got, ok := call("admin")["token"].(string); !ok || got != token {
		t.Fatalf("admin token = %q, want %q", got, token)
	}
}

func TestNodeDTOKeepsResourceGroupAsPlainLabel(t *testing.T) {
	d := toNodeDTO(&store.Node{UUID: "node-1", Labels: `{"resource_group":"pool-a","owner":"ml"}`})
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["resource_group"]; ok {
		t.Fatalf("node DTO still exposes resource_group: %s", raw)
	}
	if got := string(wire["labels"]); got != `{"owner":"ml","resource_group":"pool-a"}` {
		t.Fatalf("labels = %s", got)
	}
}

func TestNodeDTOFallsBackToLegacyNVIDIADriver(t *testing.T) {
	d := toNodeDTO(&store.Node{UUID: "node-1", NvidiaDriver: "535.00"})
	if d.GPUVendor != "nvidia" || d.GPUDriver != "535.00" || d.NvidiaDriver != "535.00" {
		t.Fatalf("legacy driver fallback = %q/%q/%q", d.GPUVendor, d.GPUDriver, d.NvidiaDriver)
	}
}

func TestGPUDTOPreservesLegacyMIGEnabledWithoutPartitionMode(t *testing.T) {
	d := toNodeDTO(&store.Node{UUID: "node-1", GPUs: []store.GPU{{Index: 0, MIGEnabled: true}}})
	if len(d.GPUs) != 1 || !d.GPUs[0].MIGEnabled {
		t.Fatalf("legacy MIG fallback = %+v", d.GPUs)
	}
}

func TestNodeDoctorEndpoint(t *testing.T) {
	var gotUUID, gotMethod string
	var gotBudget time.Duration
	h := apiHandlers{d: Deps{
		CallNode: func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
			gotUUID = uuid
			gotMethod = method
			gotBudget = rpccontext.Timeout(ctx, 0)
			return json.RawMessage(`{"report":"== gpuworks agent doctor ==\n"}`), nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/node-doctor/doctor", nil)
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"uuid": "node-doctor"}))
	rec := httptest.NewRecorder()

	h.nodeDoctor(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if gotUUID != "node-doctor" || gotMethod != proto.RPCNodeDoctor {
		t.Fatalf("rpc = %q/%q", gotUUID, gotMethod)
	}
	// Doctor outlasts the ordinary 30-second RPC wait on a host whose
	// driver answers slowly (zstone-164), so it carries its own.
	if gotBudget != nodeDoctorTimeout || nodeDoctorTimeout <= 30*time.Second {
		t.Fatalf("doctor RPC budget = %s, want %s (> 30s)", gotBudget, nodeDoctorTimeout)
	}
	var body struct {
		Report string `json:"report"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Report == "" {
		t.Fatalf("body = %+v", body)
	}
}

func TestUpdateNodeRuntimeEndpoint(t *testing.T) {
	st, err := store.Open("file:api_runtime_switch_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateNodePending("runtime-node"); err != nil {
		t.Fatal(err)
	}
	nodes, err := st.ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%d err=%v", len(nodes), err)
	}
	var gotMethod string
	h := apiHandlers{d: Deps{Store: st, CallNode: func(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
		gotMethod = method
		return json.RawMessage(`{"runtime":"udocker"}`), nil
	}}}
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/nodes/"+nodes[0].UUID+"/runtime", bytes.NewReader([]byte(`{"runtime":"udocker"}`)))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"uuid": nodes[0].UUID}))
	rec := httptest.NewRecorder()
	h.updateNodeRuntime(rec, req)
	if rec.Code != http.StatusOK || gotMethod != proto.RPCNodeSetRuntime {
		t.Fatalf("status=%d method=%q body=%s", rec.Code, gotMethod, rec.Body.String())
	}
	stored, err := st.NodeByUUID(nodes[0].UUID)
	if err != nil || stored.RuntimePreference != proto.RuntimePreferenceUdocker {
		t.Fatalf("stored runtime preference = %q err=%v", stored.RuntimePreference, err)
	}
}

func TestUpgradeNodeEndpointUsesAgentUpgradeRPC(t *testing.T) {
	var gotUUID, gotMethod string
	h := apiHandlers{d: Deps{
		Online: func(uuid string) bool { return true },
		CallNode: func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
			gotUUID, gotMethod = uuid, method
			return json.RawMessage(`{"state":"upgrading"}`), nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/node-upgrade/upgrade", nil)
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"uuid": "node-upgrade"}))
	rec := httptest.NewRecorder()
	h.upgradeNode(rec, req)
	if rec.Code != http.StatusAccepted || gotUUID != "node-upgrade" || gotMethod != proto.RPCAgentUpgrade {
		t.Fatalf("status=%d rpc=%q/%q body=%s", rec.Code, gotUUID, gotMethod, rec.Body.String())
	}
}

// An agent that can take a pushed binary gets the one for its own
// architecture over its connection; the RPC pull path is not used, so a node
// that cannot reach the server URL (server_dial) still upgrades.
// upgradeStreams is a NodeStreams that only answers UpgradeAgent.
type upgradeStreams func(uuid, goos, arch string) (bool, error)

func (upgradeStreams) OpenLogtail(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("logtail not used")
}

func (upgradeStreams) OpenLargeFileRead(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("large file read not used")
}

func (f upgradeStreams) UpgradeAgent(uuid, goos, arch string) (bool, error) {
	return f(uuid, goos, arch)
}

func (upgradeStreams) DataLaneAttached(string) bool { return false }

func (upgradeStreams) UploadStorageFile(context.Context, string, proto.StreamHeader, io.Reader) (proto.StorageFilesStatus, error) {
	return proto.StorageFilesStatus{}, errors.New("storage upload not used")
}

func (upgradeStreams) OpenStorageDownload(context.Context, string, proto.StreamHeader) (proto.StorageDownloadHead, io.ReadCloser, error) {
	return proto.StorageDownloadHead{}, nil, errors.New("storage download not used")
}

func TestUpgradeNodeEndpointPushesTheBinaryForTheNodeArch(t *testing.T) {
	st, err := store.Open("file:api_upgrade_push_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	node, _, err := st.CreateNodePending("upgrade-node", "server_dial")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{Sys: proto.SysInfo{OS: "darwin", Arch: "arm64"}}); err != nil {
		t.Fatal(err)
	}
	var pushedUUID, pushedOS, pushedArch string
	rpcCalls := 0
	pushed := true
	var pushErr error
	h := apiHandlers{d: Deps{
		Store:  st,
		Online: func(string) bool { return true },
		NodeStreams: upgradeStreams(func(uuid, goos, arch string) (bool, error) {
			pushedUUID, pushedOS, pushedArch = uuid, goos, arch
			return pushed, pushErr
		}),
		CallNode: func(context.Context, string, string, any) (json.RawMessage, error) {
			rpcCalls++
			return json.RawMessage(`{"state":"upgrading"}`), nil
		},
	}}
	call := func(uuid string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/"+uuid+"/upgrade", nil)
		req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"uuid": uuid}))
		rec := httptest.NewRecorder()
		h.upgradeNode(rec, req)
		return rec
	}

	rec := call(node.UUID)
	if rec.Code != http.StatusAccepted || strings.TrimSpace(rec.Body.String()) != `{"status":"upgrading"}` {
		t.Fatalf("push: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if pushedUUID != node.UUID || pushedOS != "darwin" || pushedArch != "arm64" || rpcCalls != 0 {
		t.Fatalf("pushed %q/%q/%q, rpc calls %d", pushedUUID, pushedOS, pushedArch, rpcCalls)
	}

	pushed = false // an agent from before pushed upgrades
	if rec := call(node.UUID); rec.Code != http.StatusAccepted || rpcCalls != 1 {
		t.Fatalf("fallback: status=%d rpc calls=%d body=%s", rec.Code, rpcCalls, rec.Body.String())
	}

	pushErr = errors.New("agent binary gpuworks-agent-linux-arm64: not found")
	if rec := call(node.UUID); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "gpuworks-agent-linux-arm64") || rpcCalls != 1 {
		t.Fatalf("push error: status=%d rpc calls=%d body=%s", rec.Code, rpcCalls, rec.Body.String())
	}
	if rec := call("missing-node"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown node: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// A RunPod pod whose udocker setup failed on the NFS volume the agent lives on.
const runtimeAndMountsReport = `{"report":"text\n","host":{"hostname":"pod","os":"linux","arch":"amd64","kernel":"5.15","cpu_model":"EPYC","cpu_cores":8,"mem_total_bytes":1,"mem_avail_bytes":1,"disks":[` +
	`{"kind":"root","path":"/","total_bytes":100,"avail_bytes":50,"mount":{"point":"/","fs_type":"overlay","source":"overlay","options":"rw,relatime","ephemeral":true}},` +
	`{"kind":"udocker","path":"/workspace/udocker","total_bytes":100,"avail_bytes":90,"mount":{"point":"/workspace","fs_type":"nfs4","source":"10.1.2.3:/volumes/abcd","options":"rw,vers=4.1","network":true}}]},` +
	`"findings":[{"code":"runtime_provision_failed","level":"error","probe":"set up the udocker runtime","evidence":[{"name":"error","value":"prepare udocker ownership: exit status 1"}],"recheck":"findmnt -T /workspace/udocker"},` +
	`{"code":"path_ephemeral","level":"warning","disk":{"kind":"models","path":"/opt/gpuworks/models","total_bytes":100,"avail_bytes":50,"mount":{"point":"/","fs_type":"overlay","ephemeral":true}},"probe":"/proc/self/mountinfo","recheck":"findmnt -T /opt/gpuworks/models"}],` +
	`"facts":[{"code":"runtime_identity","value":"the agent's own user (uid 0)"},{"code":"network_filesystem","value":"/workspace is nfs4"}]}`

// A root agent whose state directory is under /root: the runtime user cannot
// search it, so every launch fails with permission denied.
const stateUntraversableReport = `{"report":"text\n","host":{"hostname":"gpu1","os":"linux","arch":"amd64","kernel":"5.15","cpu_model":"EPYC","cpu_cores":8,"mem_total_bytes":1,"mem_avail_bytes":1,"disks":[{"kind":"root","path":"/","total_bytes":100,"avail_bytes":50}]},` +
	`"findings":[{"code":"runtime_state_untraversable","level":"error","probe":"search permission on every directory above /root/gpuworks for udocker (uid 1001, gid 1001)",` +
	`"evidence":[{"name":"directory","value":"/root"},{"name":"mode","value":"0700"},{"name":"owner","value":"0:0"},{"name":"user","value":"udocker"},{"name":"state_dir","value":"/root/gpuworks"},{"name":"fix","value":"chmod o+x /root"}],` +
	`"recheck":"namei -m /root/gpuworks"}]}`

// Unified checks preserve the Agent's detailed evidence, including legacy text.
func TestNodeDoctorEndpointPreservesStructuredEvidence(t *testing.T) {
	const structured = `{"report":"text\n","host":{"hostname":"172-24-0-210","os":"linux","arch":"amd64","kernel":"4.19","cpu_model":"Xeon","cpu_cores":192,"mem_total_bytes":1,"mem_avail_bytes":1,"disks":[{"kind":"root","path":"/","total_bytes":100,"avail_bytes":5}]},` +
		`"runtimes":[{"name":"docker","status":"ok","version":"26.1.3","selected":true,"command":"docker version --format {{.Server.Version}}"}],` +
		`"accelerators":{"vendor":"ppu","driver":"1.6.1","gpus":[{"index":5,"name":"PPU-ZW810E","memory_bytes":103079215104,"pcie_bdf":"0000:7e:00.0","numa_node":0,"health_status":"Warning","health_code":"memory_repair_pending"}]},` +
		`"findings":[{"code":"memory_repair_pending","level":"warning","targets":[{"index":5,"pcie_bdf":"0000:7e:00.0","evidence":[{"name":"remapped_rows.pending","value":"Yes"}],"probe":"ppu-smi --query-ppu=pci.bus_id,remapped_rows.pending --format=csv -i 0000:7e:00.0","recheck":"ppu-smi -q -d ECC,PAGE_RETIREMENT,ROW_REMAPPER -i 0000:7e:00.0","actions":["ppu-smi -r -i 0000:7e:00.0"],"processes":[{"pid":135707,"name":"sglang::scheduler"}]}]},` +
		`{"code":"disk_low","level":"warning","disk":{"kind":"root","path":"/","total_bytes":100,"avail_bytes":5},"probe":"statfs(/)","recheck":"df -h /"}],` +
		`"facts":[{"code":"device_injection","value":"explicit"}]}`
	for _, tc := range []struct{ name, agent, want string }{
		{"structured", structured, structured},
		{"legacy agent", `{"report":"== gpuworks agent doctor ==\n"}`, `{"report":"== gpuworks agent doctor ==\n"}`},
		{"runtime setup and mounts", runtimeAndMountsReport, runtimeAndMountsReport},
		{"state directory under a private ancestor", stateUntraversableReport, stateUntraversableReport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := apiHandlers{d: Deps{CallNode: func(context.Context, string, string, any) (json.RawMessage, error) {
				return json.RawMessage(tc.agent), nil
			}}}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/node-doctor/doctor", nil)
			req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"uuid": "node-doctor"}))
			rec := httptest.NewRecorder()
			h.nodeDoctor(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body =\n%s\nwant\n%s", rec.Code, rec.Body.String(), tc.want)
			}
			var got, want map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			checks, ok := got["checks"].([]any)
			if !ok || len(checks) < 3 || got["checked_at"] == nil || got["summary"] == nil || got["access"] != nil {
				t.Fatalf("missing unified report: %s", rec.Body)
			}
			var findings []any
			for _, raw := range checks {
				check := raw.(map[string]any)
				if finding, ok := check["finding"]; ok {
					findings = append(findings, finding)
				}
			}
			delete(got, "checks")
			delete(got, "checked_at")
			delete(got, "summary")
			if (len(findings) > 0 || want["findings"] != nil) && !reflect.DeepEqual(findings, want["findings"]) {
				t.Fatalf("unified checks changed findings: got=%v want=%v", findings, want["findings"])
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("legacy response fields changed: got=%v want=%v", got, want)
			}
		})
	}
}

func TestNodeDoctorEndpointRejectsMalformedAgentReply(t *testing.T) {
	h := apiHandlers{d: Deps{CallNode: func(context.Context, string, string, any) (json.RawMessage, error) {
		return json.RawMessage(`{"report":42}`), nil
	}}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/node-doctor/doctor", nil)
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"uuid": "node-doctor"}))
	rec := httptest.NewRecorder()
	h.nodeDoctor(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestNodeDoctorEndpointRequiresRPC(t *testing.T) {
	h := apiHandlers{d: Deps{}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/node-doctor/doctor", nil)
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"uuid": "node-doctor"}))
	rec := httptest.NewRecorder()

	h.nodeDoctor(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestNodeDoctorEndpointMapsRPCError(t *testing.T) {
	h := apiHandlers{d: Deps{
		CallNode: func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
			return nil, errors.New("node not connected")
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/node-doctor/doctor", nil)
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"uuid": "node-doctor"}))
	rec := httptest.NewRecorder()

	h.nodeDoctor(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDeleteNodeEndpoint(t *testing.T) {
	st, err := store.Open("file:api_delete_node_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("node-delete", store.HashToken("t"), "node-delete"); err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/nodes/node-delete?force=true", nil)
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"uuid": "node-delete"}))
	rec := httptest.NewRecorder()

	h.deleteNode(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := st.NodeByUUID("node-delete"); err != store.ErrNotFound {
		t.Fatalf("NodeByUUID err = %v, want ErrNotFound", err)
	}
}

func TestDeleteNodeEndpointRejectsBusyNode(t *testing.T) {
	st, err := store.Open("file:api_delete_busy_node_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	n, err := st.RegisterNode("node-busy", store.HashToken("t"), "node-busy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "inst-busy", NodeID: n.ID, State: proto.InstRunning}); err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/nodes/node-busy", nil)
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"uuid": "node-busy"}))
	rec := httptest.NewRecorder()

	h.deleteNode(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}
