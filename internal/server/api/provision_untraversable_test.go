package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

// blockedInstaller answers like the real installer does when the install
// directory sits under an ancestor the dedicated runtime user cannot search.
type blockedInstaller struct{ req provision.SSHInstallRequest }

const untraversableDetail = "/root (mode 700, owner 0:0) cannot be searched by the dedicated runtime user (udocker, uid 1001); run `chmod o+x /root` or choose an install directory outside it, such as /opt/gpuworks"

func untraversableError() error {
	return &provision.InstallDirUntraversableError{InstallDir: "/root/gpuworks", Ancestor: "/root", Mode: "700", Detail: untraversableDetail}
}

func (b *blockedInstaller) InstallSSH(_ context.Context, req provision.SSHInstallRequest) (provision.SSHInstallReport, error) {
	b.req = req
	return provision.SSHInstallReport{Checks: []provision.CheckResult{{ID: "install_dir_traversal", Status: "fail", Message: untraversableDetail}}},
		untraversableError()
}

// The literal wire answer of a blocked install: a stable code, the reason in
// English, and the parameters the client localizes it with.
const untraversableWire = `{"code":"NODE_INSTALL_DIR_UNTRAVERSABLE",` +
	`"error":"the dedicated runtime user cannot reach the install directory: install directory path access: ` +
	"/root (mode 700, owner 0:0) cannot be searched by the dedicated runtime user (udocker, uid 1001); run `chmod o+x /root` or choose an install directory outside it, such as /opt/gpuworks" + `",` +
	`"params":{"dir":"/root","fix":"chmod o+x /root","install_dir":"/root/gpuworks","mode":"700"}}` + "\n"

// A blocked install answers 400 with the code and reason in the body, leaves
// no pending node behind, and hands the installer the runtime the caller named
// (the verdict depends on it).
func TestInstallBlockedByAnUntraversableInstallDirectoryReturnsTheReason(t *testing.T) {
	fake := &blockedInstaller{}
	st := existingNodeStore(t, "unused", "")
	h := apiHandlers{d: Deps{NodeInstaller: fake, Store: st}}
	body := `{"target":"root@example.com","password":"secret","server_url":"http://gpuworks.example","connect_mode":"agent_dial","install_dir":"/root/gpuworks","runtime_driver":"udocker"}`
	rec := httptest.NewRecorder()
	h.installNode(rec, httptest.NewRequest(http.MethodPost, "/api/v1/nodes/install", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest || rec.Body.String() != untraversableWire {
		t.Fatalf("status = %d body =\n%s\nwant\n%s", rec.Code, rec.Body.String(), untraversableWire)
	}
	if fake.req.InstallDir != "/root/gpuworks" || fake.req.RuntimeDriver != "udocker" {
		t.Fatalf("installer request = %+v", fake.req)
	}
	if nodeCount(t, st) != 0 {
		t.Fatalf("the blocked install left %d node(s) behind", nodeCount(t, st))
	}
}

type apiUntraversableProviderDriver struct{ apiInstallProviderDriver }

func (d *apiUntraversableProviderDriver) Name() string { return "api-untraversable-provider" }

func (d *apiUntraversableProviderDriver) InstallAgentWithReport(context.Context, provider.Credentials, provider.InstallRequest) (provider.InstallReport, error) {
	return provider.InstallReport{}, fmt.Errorf("runpod: upload/install agent on 203.0.113.20:2222: %w", untraversableError())
}

// A provider's one-click install returns the same code and reason, as a 400
// (the operator can fix the path; the vendor did not fail), and leaves no node
// behind.
func TestProviderInstallBlockedByAnUntraversableInstallDirectoryReturnsTheReason(t *testing.T) {
	h, st, providerID := providerInstallHandlers(t, &apiUntraversableProviderDriver{}, "api_provider_install_untraversable_test", "api-untraversable-provider")
	rec := postProviderInstall(h, providerID, `{"external_id":"inst-1","server_url":"https://control.example","password":"pw","install_dir":"/root/gpuworks"}`)
	if rec.Code != http.StatusBadRequest || rec.Body.String() != untraversableWire {
		t.Fatalf("status = %d body =\n%s\nwant\n%s", rec.Code, rec.Body.String(), untraversableWire)
	}
	if nodeCount(t, st) != 0 {
		t.Fatalf("the blocked install left %d node(s) behind", nodeCount(t, st))
	}
}

// runtimePrechecker records the request it was given.
type runtimePrechecker struct{ req provision.SSHPrecheckRequest }

func (p *runtimePrechecker) PrecheckSSH(_ context.Context, req provision.SSHPrecheckRequest) (provision.SSHPrecheckReport, error) {
	p.req = req
	return provision.SSHPrecheckReport{Target: req.Target, User: "root", Host: "203.0.113.7", Port: 22, Checks: []provision.CheckResult{
		{ID: "install_dir_traversal", Status: "warn", Code: messages.CheckInstallDirPathClosedUncertain,
			Params:  map[string]string{"dir": "/root/gpuworks", "ancestor": "/root", "mode": "700", "reason": "docker"},
			Message: "/root (mode 700, owner 0:0) cannot be searched; not stopping the install: this node has docker"},
	}}, nil
}

// The precheck takes the runtime the install will name, so its verdict is the
// one the install will reach, and reports the row on the wire.
func TestPrecheckPassesTheRuntimeDriverAndReportsThePathAccessRow(t *testing.T) {
	fake := &runtimePrechecker{}
	h := apiHandlers{d: Deps{NodePrecheck: fake}}
	rec := httptest.NewRecorder()
	h.precheckNode(rec, httptest.NewRequest(http.MethodPost, "/api/v1/nodes/precheck",
		strings.NewReader(`{"target":"root@203.0.113.7","password":"pw","install_dir":"/root/gpuworks","runtime_driver":"udocker"}`)))
	if rec.Code != http.StatusOK || fake.req.RuntimeDriver != "udocker" || fake.req.InstallDir != "/root/gpuworks" {
		t.Fatalf("status %d, request %+v, body %s", rec.Code, fake.req, rec.Body.String())
	}
	want := `{"target":"root@203.0.113.7","user":"root","host":"203.0.113.7","port":22,"checks":[` +
		`{"id":"install_dir_traversal","status":"warn","code":"CHECK_INSTALL_DIR_PATH_CLOSED_UNCERTAIN",` +
		`"params":{"ancestor":"/root","dir":"/root/gpuworks","mode":"700","reason":"docker"},` +
		`"message":"/root (mode 700, owner 0:0) cannot be searched; not stopping the install: this node has docker"}]}` + "\n"
	if rec.Body.String() != want {
		t.Fatalf("wire JSON =\n%s\nwant\n%s", rec.Body.String(), want)
	}
}
