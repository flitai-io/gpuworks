package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

// installDirPrechecker answers with the rows the real probe produces for the
// RunPod NFS case, so the test pins their wire form.
type installDirPrechecker struct{ req provision.SSHPrecheckRequest }

func (p *installDirPrechecker) PrecheckSSH(_ context.Context, req provision.SSHPrecheckRequest) (provision.SSHPrecheckReport, error) {
	p.req = req
	return provision.SSHPrecheckReport{Target: req.Target, User: "root", Host: "203.0.113.7", Port: 51223, Checks: []provision.CheckResult{
		{ID: "install_dir", Status: "pass", Code: messages.CheckInstallDirWritable, Params: map[string]string{"dir": "/workspace/gpuworks", "user": "root", "filesystem": "nfs4"}, Message: "/workspace/gpuworks: filesystem nfs4"},
		{ID: "install_dir_ownership", Status: "warn", Code: messages.CheckInstallDirChownRefused, Message: "udocker workloads will run as the agent's own user", Evidence: "chown: Operation not permitted"},
		{ID: "install_dir_traversal", Status: "fail", Code: messages.CheckInstallDirPathClosed, Message: "/root (mode 700, owner 0:0) cannot be searched by the dedicated runtime user (udocker, uid 1001)"},
		{ID: "install_dir_persistence", Status: "skip", Code: messages.CheckInstallDirProbeFailed},
		{ID: "install_dir_names", Status: "na", Code: messages.CheckNotNeededOnMac},
	}}, nil
}

func TestPrecheckNodeAcceptsInstallDirAndReportsItsChecks(t *testing.T) {
	fake := &installDirPrechecker{}
	h := apiHandlers{d: Deps{NodePrecheck: fake}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/precheck", bytes.NewReader([]byte(`{"target":"root@203.0.113.7:51223","auth_mode":"password","password":"pw","server_url":"https://gpuworks.example","install_dir":"/workspace/gpuworks","instance_kind":"container","connect_mode":"agent_dial"}`)))
	rec := httptest.NewRecorder()
	h.precheckNode(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if fake.req.InstallDir != "/workspace/gpuworks" || fake.req.InstanceKind != "container" || fake.req.Target != "root@203.0.113.7:51223" {
		t.Fatalf("request = %+v", fake.req)
	}
	want := `{"target":"root@203.0.113.7:51223","user":"root","host":"203.0.113.7","port":51223,"checks":[` +
		`{"id":"install_dir","status":"pass","code":"CHECK_INSTALL_DIR_WRITABLE","params":{"dir":"/workspace/gpuworks","filesystem":"nfs4","user":"root"},"message":"/workspace/gpuworks: filesystem nfs4"},` +
		`{"id":"install_dir_ownership","status":"warn","code":"CHECK_INSTALL_DIR_CHOWN_REFUSED","message":"udocker workloads will run as the agent's own user","evidence":"chown: Operation not permitted"},` +
		`{"id":"install_dir_traversal","status":"fail","code":"CHECK_INSTALL_DIR_PATH_CLOSED","message":"/root (mode 700, owner 0:0) cannot be searched by the dedicated runtime user (udocker, uid 1001)"},` +
		`{"id":"install_dir_persistence","status":"skip","code":"CHECK_INSTALL_DIR_PROBE_FAILED"},` +
		`{"id":"install_dir_names","status":"na","code":"CHECK_NOT_NEEDED_ON_MAC"}]}`
	if got := bytes.TrimSpace(rec.Body.Bytes()); string(got) != want {
		t.Fatalf("wire JSON =\n%s\nwant\n%s", got, want)
	}
}

// Omitting install_dir is what the wizard sends for the default directory.
func TestPrecheckNodeWithoutInstallDirLeavesTheDefaultToTheProbe(t *testing.T) {
	fake := &installDirPrechecker{}
	h := apiHandlers{d: Deps{NodePrecheck: fake}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/precheck", bytes.NewReader([]byte(`{"target":"root@203.0.113.7","password":"pw"}`)))
	rec := httptest.NewRecorder()
	h.precheckNode(rec, req)
	if rec.Code != http.StatusOK || fake.req.InstallDir != "" {
		t.Fatalf("status %d, install dir %q", rec.Code, fake.req.InstallDir)
	}
}
