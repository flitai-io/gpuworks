package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func existingNodeStore(t *testing.T, name string, sshHost string) *store.Store {
	t.Helper()
	st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	enableTestCredentialEncryption(t, st)
	if sshHost != "" {
		n, _, err := st.CreateNodePending(name, "agent_dial")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetNodeSSHCredential(n.UUID, sshHost, 22, "root", store.NodeSSHSecrets{}); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func nodeCount(t *testing.T, st *store.Store) int {
	t.Helper()
	nodes, err := st.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	return len(nodes)
}

func TestPrecheckNamesTheNodeThatAlreadyUsesTheHost(t *testing.T) {
	for _, tc := range []struct {
		name, sshHost, status, detail string
	}{
		{name: "same-host", sshHost: "example.com", status: "warn", detail: "already added here as ppu-210"},
		{name: "other-host", sshHost: "other.example.com", status: "pass", detail: "no node here uses this SSH host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := apiHandlers{d: Deps{NodePrecheck: &fakePrechecker{}, Store: existingNodeStore(t, "ppu-210", tc.sshHost)}}
			rec := httptest.NewRecorder()
			h.precheckNode(rec, httptest.NewRequest(http.MethodPost, "/api/v1/nodes/precheck", strings.NewReader(`{"target":"root@example.com","password":"secret"}`)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			var report provision.SSHPrecheckReport
			if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			last := report.Checks[len(report.Checks)-1]
			if last.ID != "existing_node" || last.Status != tc.status || !strings.Contains(last.Message, tc.detail) {
				t.Fatalf("checks = %+v", report.Checks)
			}
		})
	}
}

func TestInstallOntoAHostThatIsAlreadyANodeAsksForConfirmation(t *testing.T) {
	fake := &fakeInstaller{}
	st := existingNodeStore(t, "ppu-210", "example.com")
	h := apiHandlers{d: Deps{NodeInstaller: fake, Store: st}}
	body := `{"target":"root@example.com","password":"secret","server_url":"http://gpuworks.example","connect_mode":"agent_dial"}`
	rec := httptest.NewRecorder()
	h.installNode(rec, httptest.NewRequest(http.MethodPost, "/api/v1/nodes/install", strings.NewReader(body)))
	want := `{"code":"NODE_INSTALL_HOST_ALREADY_NODE","error":"this host is already added as ppu-210; installing again replaces its agent","params":{"nodes":"ppu-210"}}` + "\n"
	if rec.Code != http.StatusConflict || rec.Body.String() != want {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if fake.req.Target != "" || nodeCount(t, st) != 1 {
		t.Fatalf("unconfirmed install reached the host or left a node: req=%+v nodes=%d", fake.req, nodeCount(t, st))
	}

	confirmed := strings.Replace(body, `"connect_mode"`, `"replace_existing_agent":true,"connect_mode"`, 1)
	rec = httptest.NewRecorder()
	h.installNode(rec, httptest.NewRequest(http.MethodPost, "/api/v1/nodes/install", strings.NewReader(confirmed)))
	if rec.Code != http.StatusOK || !fake.req.ReplaceExistingAgent || !fake.req.GuardExistingAgent {
		t.Fatalf("confirmed install status = %d req=%+v body=%s", rec.Code, fake.req, rec.Body.String())
	}
}

type existingAgentInstaller struct {
	req   provision.SSHInstallRequest
	found provision.ExistingAgent
}

func (f *existingAgentInstaller) InstallSSH(_ context.Context, req provision.SSHInstallRequest) (provision.SSHInstallReport, error) {
	f.req = req
	return provision.SSHInstallReport{}, &provision.ExistingAgentError{
		Found:     f.found,
		ServerURL: req.ServerURL,
	}
}

func TestInstallOntoAHostWithItsOwnOrAnUnreadableUnitAsksForConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		found provision.ExistingAgent
		want  string
	}{
		{name: "unreadable-unit", found: provision.ExistingAgent{Unit: true},
			want: `{"code":"NODE_INSTALL_EXISTING_AGENT_UNKNOWN","error":"a gpuworks-agent service whose server cannot be read is on the host; installing replaces it"}`},
		{name: "own-unit", found: provision.ExistingAgent{Unit: true, UnitServer: "http://198.51.100.122:8800", ProcessServers: []string{"http://198.51.100.122:8800"}},
			want: `{"code":"NODE_INSTALL_EXISTING_AGENT_SELF","error":"the gpuworks-agent service on the host already runs for this server; installing replaces it and the node it belongs to goes offline"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := apiHandlers{d: Deps{NodeInstaller: &existingAgentInstaller{found: tc.found}, Store: existingNodeStore(t, "", "")}}
			rec := httptest.NewRecorder()
			h.installNode(rec, httptest.NewRequest(http.MethodPost, "/api/v1/nodes/install", strings.NewReader(
				`{"target":"root@example.com","password":"secret","server_url":"http://198.51.100.122:8800","connect_mode":"agent_dial"}`)))
			if rec.Code != http.StatusConflict || rec.Body.String() != tc.want+"\n" {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestInstallOntoAHostWithAnotherServersAgentAsksForConfirmation(t *testing.T) {
	fake := &existingAgentInstaller{found: provision.ExistingAgent{Unit: true, UnitServer: "http://192.0.2.156:8800", ProcessServers: []string{"http://192.0.2.156:8800"}}}
	st := existingNodeStore(t, "", "")
	h := apiHandlers{d: Deps{NodeInstaller: fake, Store: st}}
	rec := httptest.NewRecorder()
	h.installNode(rec, httptest.NewRequest(http.MethodPost, "/api/v1/nodes/install", bytes.NewReader([]byte(
		`{"target":"root@example.com","password":"secret","server_url":"http://198.51.100.122:8800","connect_mode":"agent_dial"}`))))
	var payload struct {
		Code   string         `json:"code"`
		Error  string         `json:"error"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict || payload.Code != ErrorNodeInstallExistingAgent || payload.Params["server"] != "http://192.0.2.156:8800" ||
		!strings.Contains(payload.Error, "belongs to another server (http://192.0.2.156:8800)") {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !fake.req.GuardExistingAgent || fake.req.ReplaceExistingAgent {
		t.Fatalf("install request = %+v", fake.req)
	}
	if nodeCount(t, st) != 0 {
		t.Fatal("the pending node of a refused install was kept")
	}
}

// Without systemd the new agent runs beside the host's others, so a host that
// is already a node takes another agent without a prompt; the installer still
// guards the one directory it would replace an agent in.
func TestInstallWithoutSystemdRunsBesideTheHostsNodes(t *testing.T) {
	fake := &fakeInstaller{}
	st := existingNodeStore(t, "ppu-210", "example.com")
	h := apiHandlers{d: Deps{NodeInstaller: fake, Store: st}}
	rec := httptest.NewRecorder()
	h.installNode(rec, httptest.NewRequest(http.MethodPost, "/api/v1/nodes/install", strings.NewReader(
		`{"target":"root@example.com","password":"secret","server_url":"http://gpuworks.example","install_dir":"/data/agent2","no_systemd":true}`)))
	if rec.Code != http.StatusOK || !fake.req.NoSystemd || !fake.req.GuardExistingAgent || fake.req.ReplaceExistingAgent {
		t.Fatalf("status = %d req=%+v body=%s", rec.Code, fake.req, rec.Body.String())
	}
	nodes, err := st.ListNodes()
	if err != nil || len(nodes) != 2 {
		t.Fatalf("nodes = %+v err=%v, want the existing node and the new one", nodes, err)
	}
	// Recovery reaches the new agent through the directory it was put in.
	dirs := map[string]bool{}
	for _, n := range nodes {
		dirs[n.InstallDir] = true
	}
	if !dirs["/data/agent2"] {
		t.Fatalf("install dirs = %v, want /data/agent2 recorded", dirs)
	}
}

type sameDirInstaller struct{}

func (sameDirInstaller) InstallSSH(_ context.Context, req provision.SSHInstallRequest) (provision.SSHInstallReport, error) {
	return provision.SSHInstallReport{}, &provision.ExistingAgentError{
		Found:      provision.ExistingAgent{Unit: true, UnitServer: "http://192.0.2.156:8800", ProcessStates: []string{"/opt/gpuworks"}},
		ServerURL:  req.ServerURL,
		InstallDir: "/opt/gpuworks",
	}
}

func TestInstallWithoutSystemdIntoAnAgentsDirectoryAsksForConfirmation(t *testing.T) {
	st := existingNodeStore(t, "", "")
	h := apiHandlers{d: Deps{NodeInstaller: sameDirInstaller{}, Store: st}}
	rec := httptest.NewRecorder()
	h.installNode(rec, httptest.NewRequest(http.MethodPost, "/api/v1/nodes/install", strings.NewReader(
		`{"target":"root@example.com","password":"secret","server_url":"http://198.51.100.122:8800","no_systemd":true}`)))
	want := `{"code":"NODE_INSTALL_EXISTING_AGENT_SAME_DIR","error":"a gpuworks agent already runs from /opt/gpuworks; installing there replaces it","params":{"dir":"/opt/gpuworks"}}` + "\n"
	if rec.Code != http.StatusConflict || rec.Body.String() != want {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if nodeCount(t, st) != 0 {
		t.Fatal("the refused install left a pending node")
	}
}
