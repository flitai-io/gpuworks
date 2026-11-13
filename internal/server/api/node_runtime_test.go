package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// fakeNodeRuntime is the hub's view of nodes' container runtimes: the proxy
// each last reported and what a restart does. Unlike a stub that always
// succeeds it fails for a node it was not told about, as the hub does.
type fakeNodeRuntime struct {
	mu      sync.Mutex
	proxies map[string]*proto.RuntimeProxyStatus
	// isolation is the soft-isolation notice each node last reported.
	isolation map[string]*proto.NVIDIAIsolation
	restarts  []string
	// afterRestart is what the daemon reports once restarted, per node.
	afterRestart map[string]*proto.RuntimeProxyStatus
	failWith     error
}

func (f *fakeNodeRuntime) NVIDIAIsolation(uuid string) *proto.NVIDIAIsolation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.isolation[uuid]
}

func (f *fakeNodeRuntime) RuntimeProxy(uuid string) *proto.RuntimeProxyStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.proxies[uuid]
}

func (f *fakeNodeRuntime) RestartRuntime(_ context.Context, uuid string) (*proto.RuntimeProxyStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts = append(f.restarts, uuid)
	if f.failWith != nil {
		return nil, f.failWith
	}
	status, ok := f.afterRestart[uuid]
	if !ok {
		return nil, errors.New("node " + uuid + " is not connected")
	}
	f.proxies[uuid] = status
	return status, nil
}

func (r *pullRig) runtimeNode(uuid, driver string, online bool) *store.Node {
	r.t.Helper()
	node := r.node(uuid, online, nil)
	if err := r.st.UpdateNodeStatus(uuid, proto.NodeStatus{Sys: proto.SysInfo{Hostname: uuid, RuntimeDriver: driver}}); err != nil {
		r.t.Fatal(err)
	}
	return node
}

// GET /nodes/{uuid}/pull-proxy carries the runtime the node pulls with. For a
// daemon runtime the daemon's proxy is compared with the proxy the platform
// set for the node: "applied" says whether they agree, a password compared in
// the masked form the daemon reports it in.
func TestNodePullProxyReportsRuntimeProxyAndWhetherItIsApplied(t *testing.T) {
	rig := newPullRig(t)
	rig.runtimeNode("docker-node", "docker", true)
	rig.runtimeNode("udocker-node", "udocker", true)
	rig.runtimeNode("silent-node", "docker", false)

	// No proxy configured, none reported: nothing differs.
	rig.runtime.proxies = map[string]*proto.RuntimeProxyStatus{
		"docker-node": {Privilege: "root", Systemd: true, LiveRestore: true},
	}
	rig.want("GET", "/api/v1/nodes/docker-node/pull-proxy", "", 200, `{
		"proxy":null,
		"runtime":{"driver":"docker","proxy":{"http_proxy":"","https_proxy":"","no_proxy":"","configured_http_proxy":"","configured_https_proxy":"","written_http_proxy":"","written_https_proxy":"","written_no_proxy":"",
			"live_restore":true,"daemon_json_proxy":false,"systemd":true,"privilege":"root","applied":true}}}`)

	// No proxy configured, but dockerd runs with an operator's own proxy
	// file: the platform has nothing to apply, so nothing is pending.
	rig.runtime.proxies["docker-node"] = &proto.RuntimeProxyStatus{HTTPProxy: "http://own.example:7890", HTTPSProxy: "http://own.example:7890", Privilege: "root", Systemd: true}
	if code, body := rig.do(rig.admin, "GET", "/api/v1/nodes/docker-node/pull-proxy", ""); code != 200 || !strings.Contains(body, `"http_proxy":"http://own.example:7890"`) || !strings.Contains(body, `"applied":true`) {
		t.Fatalf("an operator's own dockerd proxy with nothing set = %d %s, want applied", code, body)
	}
	// The same, with systemd's view of what dockerd starts with: it runs
	// what it is configured with.
	rig.runtime.proxies["docker-node"] = &proto.RuntimeProxyStatus{HTTPProxy: "http://own.example:7890", HTTPSProxy: "http://own.example:7890",
		ConfiguredHTTPProxy: "http://own.example:7890", ConfiguredHTTPSProxy: "http://own.example:7890", ConfiguredKnown: true, Privilege: "root", Systemd: true}
	if code, body := rig.do(rig.admin, "GET", "/api/v1/nodes/docker-node/pull-proxy", ""); code != 200 || !strings.Contains(body, `"configured_http_proxy":"http://own.example:7890"`) || !strings.Contains(body, `"applied":true`) {
		t.Fatalf("an operator's own proxy dockerd runs with = %d %s, want applied", code, body)
	}
	// The proxy was just cleared and the agent has not removed its drop-in
	// yet: the response to that very change must not call the old proxy
	// applied.
	rig.runtime.proxies["docker-node"] = &proto.RuntimeProxyStatus{HTTPProxy: "http://old.example:3128", HTTPSProxy: "http://old.example:3128",
		ConfiguredHTTPProxy: "http://old.example:3128", ConfiguredHTTPSProxy: "http://old.example:3128", ConfiguredKnown: true,
		WrittenHTTPProxy: "http://old.example:3128", WrittenHTTPSProxy: "http://old.example:3128", Privilege: "root", Systemd: true}
	if code, body := rig.do(rig.admin, "GET", "/api/v1/nodes/docker-node/pull-proxy", ""); code != 200 || !strings.Contains(body, `"applied":false`) {
		t.Fatalf("a cleared proxy whose drop-in is still written = %d %s, want not applied", code, body)
	}
	// The platform's proxy was cleared and its drop-in removed, but dockerd
	// still runs the old one until it restarts: that is pending.
	rig.runtime.proxies["docker-node"] = &proto.RuntimeProxyStatus{HTTPProxy: "http://old.example:3128", HTTPSProxy: "http://old.example:3128",
		ConfiguredKnown: true, Privilege: "root", Systemd: true}
	if code, body := rig.do(rig.admin, "GET", "/api/v1/nodes/docker-node/pull-proxy", ""); code != 200 || !strings.Contains(body, `"applied":false`) {
		t.Fatalf("dockerd still on a cleared proxy = %d %s, want not applied", code, body)
	}

	// A proxy is configured for the whole platform; the daemon has not taken
	// it yet: applied is false, and the agent's drop-in is reported beside it,
	// its password masked even when an older agent reports it in clear.
	if code, body := rig.do(rig.admin, "POST", "/api/v1/pull/proxies", `{"http_proxy":" http://proxy.example:3128 ","https_proxy":"http://proxy.example:3128","no_proxy":" 10.0.0.0/8, .corp ","username":"user","password":"secret"}`); code != 201 {
		t.Fatalf("create proxy = %d %s", code, body)
	}
	entry := `{"id":1,"position":0,"http_proxy":"http://proxy.example:3128","https_proxy":"http://proxy.example:3128","no_proxy":"10.0.0.0/8,.corp","username":"user","has_password":true,"control_plane":false,"note":"","node_requirements":{},"matched_nodes":3}`
	rig.runtime.proxies["docker-node"] = &proto.RuntimeProxyStatus{
		WrittenHTTPProxy: "http://user:secret@proxy.example:3128", WrittenHTTPSProxy: "http://user:secret@proxy.example:3128",
		Privilege: "sudo", Systemd: true,
	}
	rig.want("GET", "/api/v1/nodes/docker-node/pull-proxy", "", 200, `{
		"proxy":`+entry+`,
		"runtime":{"driver":"docker","proxy":{"http_proxy":"","https_proxy":"","no_proxy":"","configured_http_proxy":"","configured_https_proxy":"",
			"written_http_proxy":"http://xxxxx:xxxxx@proxy.example:3128","written_https_proxy":"http://xxxxx:xxxxx@proxy.example:3128","written_no_proxy":"",
			"live_restore":false,"daemon_json_proxy":false,"systemd":true,"privilege":"sudo","applied":false}}}`)

	// After a daemon restart it reports the proxy with its user and password
	// masked, as docker info does, and that counts as applied.
	rig.runtime.proxies["docker-node"] = &proto.RuntimeProxyStatus{
		HTTPProxy: "http://xxxxx:xxxxx@proxy.example:3128", HTTPSProxy: "http://xxxxx:xxxxx@proxy.example:3128", NoProxy: "10.0.0.0/8,.corp",
		Privilege: "sudo", Systemd: true,
	}
	rig.want("GET", "/api/v1/nodes/docker-node/pull-proxy", "", 200, `{
		"proxy":`+entry+`,
		"runtime":{"driver":"docker","proxy":{"http_proxy":"http://xxxxx:xxxxx@proxy.example:3128","https_proxy":"http://xxxxx:xxxxx@proxy.example:3128","no_proxy":"10.0.0.0/8,.corp",
			"configured_http_proxy":"","configured_https_proxy":"","written_http_proxy":"","written_https_proxy":"","written_no_proxy":"",
			"live_restore":false,"daemon_json_proxy":false,"systemd":true,"privilege":"sudo","applied":true}}}`)

	// An entry for this node ahead of the first changes what the daemon is
	// compared with.
	if code, body := rig.do(rig.admin, "POST", "/api/v1/pull/proxies", `{"http_proxy":"http://other.example:8080","https_proxy":"http://other.example:8080","node_requirements":{"gpuworks.ai/node":["docker-node"]}}`); code != 201 {
		t.Fatalf("create node proxy = %d %s", code, body)
	}
	if code, body := rig.do(rig.admin, "PUT", "/api/v1/pull/proxies/order", `{"ids":[2,1]}`); code != 200 {
		t.Fatalf("order proxies = %d %s", code, body)
	}
	_, body := rig.do(rig.admin, "GET", "/api/v1/nodes/docker-node/pull-proxy", "")
	if !strings.Contains(body, `"applied":false`) || !strings.Contains(body, `"http_proxy":"http://other.example:8080"`) {
		t.Fatalf("a daemon on the old proxy still reads as applied: %s", body)
	}

	// A runtime that pulls in the agent's own process has no daemon proxy; a
	// node that has reported none (offline, never seen) has none either.
	rig.want("GET", "/api/v1/nodes/udocker-node/pull-proxy", "", 200, `{"proxy":{"id":1,"position":1,"http_proxy":"http://proxy.example:3128","https_proxy":"http://proxy.example:3128","no_proxy":"10.0.0.0/8,.corp","username":"user","has_password":true,"control_plane":false,"note":"","node_requirements":{},"matched_nodes":3},"runtime":{"driver":"udocker"}}`)
	_, body = rig.do(rig.admin, "GET", "/api/v1/nodes/silent-node/pull-proxy", "")
	if !strings.Contains(body, `"runtime":{"driver":"docker"}`) {
		t.Fatalf("a node with no report: %s", body)
	}
}

func TestRestartNodeRuntime(t *testing.T) {
	rig := newPullRig(t)
	rig.runtimeNode("restart-ok", "docker", true)
	rig.runtimeNode("restart-off", "docker", false)
	rig.runtimeNode("restart-fail", "docker", true)
	if code, body := rig.do(rig.admin, "POST", "/api/v1/pull/proxies", `{"http_proxy":"http://proxy.example:3128","https_proxy":"http://proxy.example:3128"}`); code != 201 {
		t.Fatalf("create proxy = %d %s", code, body)
	}
	rig.runtime.proxies = map[string]*proto.RuntimeProxyStatus{"restart-ok": {Privilege: "root", Systemd: true}}
	rig.runtime.afterRestart = map[string]*proto.RuntimeProxyStatus{
		"restart-ok": {HTTPProxy: "http://proxy.example:3128", HTTPSProxy: "http://proxy.example:3128", Privilege: "root", Systemd: true, LiveRestore: true},
	}

	rig.want("POST", "/api/v1/nodes/restart-ok/runtime/restart", "", 200, `{"driver":"docker","proxy":{
		"http_proxy":"http://proxy.example:3128","https_proxy":"http://proxy.example:3128","no_proxy":"",
		"configured_http_proxy":"","configured_https_proxy":"","written_http_proxy":"","written_https_proxy":"","written_no_proxy":"",
		"live_restore":true,"daemon_json_proxy":false,"systemd":true,"privilege":"root","applied":true}}`)
	// The restart's result is what the next read shows.
	if got := rig.runtime.RuntimeProxy("restart-ok"); got == nil || got.HTTPProxy != "http://proxy.example:3128" {
		t.Fatalf("daemon report after restart = %+v", got)
	}

	// An offline node is not asked.
	rig.want("POST", "/api/v1/nodes/restart-off/runtime/restart", "", 409,
		`{"code":"RUNTIME_RESTART_FAILED","error":"the node is offline","params":{"detail":"the node is offline"}}`)
	// The agent refusing (no privilege, daemon failed to come back) is passed on.
	rig.runtime.failWith = errors.New("sudo needs a password")
	rig.want("POST", "/api/v1/nodes/restart-fail/runtime/restart", "", 409,
		`{"code":"RUNTIME_RESTART_FAILED","error":"sudo needs a password","params":{"detail":"sudo needs a password"}}`)
	rig.runtime.failWith = nil
	if got := rig.runtime.restarts; len(got) != 2 || got[0] != "restart-ok" || got[1] != "restart-fail" {
		t.Fatalf("restarts asked of %v, want only the online nodes", got)
	}
	if code, body := rig.do(rig.admin, "POST", "/api/v1/nodes/ghost/runtime/restart", ""); code != 404 {
		t.Fatalf("unknown node = %d %s", code, body)
	}
}

// A server wired without the hub's runtime hooks cannot restart anything and
// says so, instead of dereferencing a missing hook.
func TestRestartNodeRuntimeWithoutHooks(t *testing.T) {
	st := newAcquisitionStore(t)
	if _, err := st.RegisterNode("hookless", store.HashToken("t"), "hookless"); err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/hookless/runtime/restart", nil)
	h.restartNodeRuntime(rec, req.WithContext(chiRouteContext(req.Context(), map[string]string{"uuid": "hookless"})))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"RUNTIME_RESTART_FAILED"`) {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	// And the node's proxy read answers without a runtime proxy.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/nodes/hookless/pull-proxy", nil)
	h.getNodePullProxy(rec, req.WithContext(chiRouteContext(req.Context(), map[string]string{"uuid": "hookless"})))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"runtime":{"driver":""}`) {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
}
