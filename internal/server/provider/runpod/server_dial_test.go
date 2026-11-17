package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

// podJSON is a pod as GET /v2/pods/{id} returned it on 2026-10-19 (pod
// f401mvcppqmnhy, addresses replaced): TCP 22 and 18801 on the public
// address, UDP 22 beside them, HTTP ports through RunPod's proxy network.
func podJSON(ports []string, runtimePorts string, sshPort int) string {
	exposed, _ := json.Marshal(ports)
	return `{"id":"pod-1","name":"gpu","desiredStatus":"RUNNING","ports":` + string(exposed) + `,
"runtime":{"ports":` + runtimePorts + `},
"ssh":{"direct":{"host":"203.0.113.158","port":` + itoa(sshPort) + `,"username":"root"},"proxy":{"host":"ssh.runpod.io","port":22,"username":"pod-1-64411b6e"}}}`
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

const spareRuntimePorts = `[{"ip":"203.0.113.158","private":22,"public":15757,"type":"udp"},
{"ip":"100.65.27.110","private":8888,"public":60758,"type":"http"},
{"ip":"100.65.27.110","private":18800,"public":60759,"type":"http"},
{"ip":"203.0.113.158","private":18801,"public":15756,"type":"tcp"},
{"ip":"203.0.113.158","private":22,"public":15755,"type":"tcp"}]`

const noSpareRuntimePorts = `[{"ip":"203.0.113.158","private":22,"public":10035,"type":"udp"},
{"ip":"100.65.27.110","private":8888,"public":60713,"type":"http"},
{"ip":"100.65.27.110","private":18800,"public":60714,"type":"http"},
{"ip":"203.0.113.158","private":22,"public":10033,"type":"tcp"}]`

func TestPodTCPMappingsSkipUDPAndProxiedHTTP(t *testing.T) {
	var p pod
	if err := json.Unmarshal([]byte(podJSON([]string{"8888/http", "18800/http", "22/tcp", "18801/tcp", "22/udp"}, spareRuntimePorts, 15755)), &p); err != nil {
		t.Fatal(err)
	}
	ip, mapped := p.tcpMappings()
	if ip != "203.0.113.158" || len(mapped) != 2 || mapped[22] != 15755 || mapped[18801] != 15756 {
		t.Fatalf("mappings = %s %v", ip, mapped)
	}
	if got := newAgentPort(p.exposedPorts(), 0); got != 18802 {
		t.Fatalf("new agent port = %d, want 18802 (18800 and 18801 are exposed)", got)
	}
}

type fakePodAPI struct {
	mu      sync.Mutex
	pods    []string // successive GET answers; the last repeats
	gets    int
	patches []map[string]any
}

func (f *fakePodAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path != "/pods/pod-1" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			i := f.gets
			if i >= len(f.pods) {
				i = len(f.pods) - 1
			}
			f.gets++
			_, _ = w.Write([]byte(f.pods[i]))
		case http.MethodPatch:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.patches = append(f.patches, body)
			_, _ = w.Write([]byte(f.pods[len(f.pods)-1]))
		}
	}
}

func serverDialRequest(allowRestart bool) provider.InstallRequest {
	return provider.InstallRequest{
		Instance: provider.Instance{ExternalID: "pod-1"}, Token: "t", ConnectMode: "server_dial",
		ServerDialListen: "0.0.0.0:18800", AllowInstanceRestart: allowRestart, SSHKeyPEM: "key",
	}
}

// A pod that already exposes a spare TCP port is left as it is.
func TestServerDialUsesASpareExposedPortWithoutTouchingThePod(t *testing.T) {
	api := &fakePodAPI{pods: []string{podJSON([]string{"8888/http", "18800/http", "22/tcp", "18801/tcp", "22/udp"}, spareRuntimePorts, 15755)}}
	d := newTestDriver(t, api.handler(t))
	d.listening = func(_ context.Context, inst provider.Instance, _ provision.SSHAuth) (map[int]bool, error) {
		if inst.PublicIP != "203.0.113.158" || inst.SSHPort != 15755 {
			t.Errorf("listening ports read through %s:%d", inst.PublicIP, inst.SSHPort)
		}
		return map[int]bool{22: true}, nil
	}
	listen, address, inst, check, err := d.serverDialPort(context.Background(), provider.Credentials{APIKey: "k"}, serverDialRequest(false))
	if err != nil {
		t.Fatal(err)
	}
	if listen != "0.0.0.0:18801" || address != "203.0.113.158:15756" || inst.SSHPort != 15755 || !strings.Contains(checkText(check), "container port 18801") {
		t.Fatalf("got %s %s %d %+v", listen, address, inst.SSHPort, check)
	}
	if len(api.patches) != 0 {
		t.Fatalf("the pod was edited: %v", api.patches)
	}
}

// Without a spare port, exposing one restarts the pod: refused until the
// operator agrees, and nothing is changed meanwhile.
func TestServerDialAsksBeforeRestartingThePod(t *testing.T) {
	api := &fakePodAPI{pods: []string{podJSON([]string{"8888/http", "18800/http", "22/tcp", "22/udp"}, noSpareRuntimePorts, 10033)}}
	d := newTestDriver(t, api.handler(t))
	d.listening = func(context.Context, provider.Instance, provision.SSHAuth) (map[int]bool, error) {
		return map[int]bool{22: true}, nil
	}
	_, _, _, _, err := d.serverDialPort(context.Background(), provider.Credentials{APIKey: "k"}, serverDialRequest(false))
	if !errors.Is(err, provider.ErrInstanceRestartRequired) || !strings.Contains(err.Error(), "restarts its container") {
		t.Fatalf("err = %v", err)
	}
	if len(api.patches) != 0 {
		t.Fatalf("the pod was edited without consent: %v", api.patches)
	}
}

// With consent the port is added to the pod's ports (the edit replaces the
// list, so the others are kept), and the install waits for the restarted
// pod: its new mapping and its renumbered SSH.
func TestServerDialExposesAPortAndWaitsForTheRestartedPod(t *testing.T) {
	before := podJSON([]string{"8888/http", "18800/http", "22/tcp", "22/udp"}, noSpareRuntimePorts, 10033)
	restarting := podJSON([]string{"8888/http", "18800/http", "22/tcp", "22/udp", "18801/tcp"}, `[]`, 10033)
	after := podJSON([]string{"8888/http", "18800/http", "22/tcp", "22/udp", "18801/tcp"}, spareRuntimePorts, 15755)
	api := &fakePodAPI{pods: []string{before, restarting, after}}
	d := newTestDriver(t, api.handler(t))
	d.portPollInterval, d.portWaitTimeout = time.Millisecond, 5*time.Second
	var probed []int
	d.listening = func(_ context.Context, inst provider.Instance, _ provision.SSHAuth) (map[int]bool, error) {
		probed = append(probed, inst.SSHPort)
		return map[int]bool{22: true}, nil
	}
	listen, address, inst, check, err := d.serverDialPort(context.Background(), provider.Credentials{APIKey: "k"}, serverDialRequest(true))
	if err != nil {
		t.Fatal(err)
	}
	if len(api.patches) != 1 {
		t.Fatalf("patches = %v", api.patches)
	}
	got, _ := json.Marshal(api.patches[0]["ports"])
	if string(got) != `["8888/http","18800/http","22/tcp","22/udp","18801/tcp"]` {
		t.Fatalf("patched ports = %s", got)
	}
	if listen != "0.0.0.0:18801" || address != "203.0.113.158:15756" || inst.SSHPort != 15755 || !strings.Contains(checkText(check), "the pod restarted") {
		t.Fatalf("got %s %s %d %+v", listen, address, inst.SSHPort, check)
	}
	if len(probed) != 2 || probed[0] != 10033 || probed[1] != 15755 {
		t.Fatalf("SSH probed at %v, want the old port, then the renumbered one", probed)
	}
}

func TestServerDialGivesUpWhenThePodNeverComesBack(t *testing.T) {
	before := podJSON([]string{"22/tcp"}, noSpareRuntimePorts, 10033)
	api := &fakePodAPI{pods: []string{before}}
	d := newTestDriver(t, api.handler(t))
	d.portPollInterval, d.portWaitTimeout = time.Millisecond, 20*time.Millisecond
	d.listening = func(context.Context, provider.Instance, provision.SSHAuth) (map[int]bool, error) {
		return map[int]bool{22: true}, nil
	}
	_, _, _, _, err := d.serverDialPort(context.Background(), provider.Credentials{APIKey: "k"}, serverDialRequest(true))
	if err == nil || !strings.Contains(err.Error(), "did not come back with port 18800 mapped") {
		t.Fatalf("err = %v", err)
	}
}

func TestServerDialAddressForFollowsTheMapping(t *testing.T) {
	api := &fakePodAPI{pods: []string{podJSON([]string{"22/tcp", "18801/tcp"}, spareRuntimePorts, 15755)}}
	d := newTestDriver(t, api.handler(t))
	got, err := d.ServerDialAddressFor(context.Background(), provider.Credentials{APIKey: "k"}, "pod-1", "0.0.0.0:18801")
	if err != nil || got != "203.0.113.158:15756" {
		t.Fatalf("ServerDialAddressFor = %q, %v", got, err)
	}
	if _, err := d.ServerDialAddressFor(context.Background(), provider.Credentials{APIKey: "k"}, "pod-1", "0.0.0.0:18800"); err == nil {
		t.Fatal("an HTTP-only port resolved to a TCP address")
	}
}

const platformPublic = "ssh-ed25519 AAAAC3platform gpuworks@server"

// podWithEnv is podJSON with an environment, as GET /v2/pods/{id} returns it.
func podWithEnv(ports []string, runtimePorts string, sshPort int, publicKey string) string {
	base := podJSON(ports, runtimePorts, sshPort)
	env, _ := json.Marshal(map[string]string{"JUPYTER_PASSWORD": "jp", "PUBLIC_KEY": publicKey})
	return strings.Replace(base, `"id":"pod-1",`, `"id":"pod-1","env":`+string(env)+`,`, 1)
}

func keyRequest(mode string, allow bool) provider.InstallRequest {
	req := serverDialRequest(allow)
	req.ConnectMode = mode
	req.SSHKeyPEM = ""
	req.PlatformKey = &provider.PlatformKey{PublicKey: platformPublic, PrivatePEM: "unused"}
	return req
}

func authFailure() error {
	return &provision.SSHAuthError{Err: errors.New("ssh: unable to authenticate")}
}

func TestPlatformKeyLeftAloneWhenThePodHasItOrLogsIn(t *testing.T) {
	for name, tc := range map[string]struct {
		publicKey string
		login     error
	}{
		"listed":          {publicKey: "ssh-ed25519 AAAAC3other me\n" + platformPublic + " another-comment", login: authFailure()},
		"logs in already": {publicKey: "ssh-ed25519 AAAAC3other me", login: nil},
		"unreachable":     {publicKey: "", login: errors.New("ssh dial: connection refused")},
	} {
		t.Run(name, func(t *testing.T) {
			api := &fakePodAPI{pods: []string{podWithEnv([]string{"22/tcp"}, noSpareRuntimePorts, 10033, tc.publicKey)}}
			d := newTestDriver(t, api.handler(t))
			d.loginHook = func(context.Context, provider.Instance, provision.SSHAuth) error { return tc.login }
			_, check, err := d.ensurePlatformKey(context.Background(), provider.Credentials{APIKey: "k"}, keyRequest("agent_dial", false))
			if err != nil || check.ID != "" || len(api.patches) != 0 {
				t.Fatalf("err=%v check=%+v patches=%v", err, check, api.patches)
			}
		})
	}
}

// A pod started before the key was in the account rejects it: adding it
// restarts the pod, so the operator is asked first, for the key alone, or,
// for a server-dialed agent on a pod with no spare port, for both at once.
func TestPlatformKeyAsksBeforeRestartingThePod(t *testing.T) {
	for _, tc := range []struct{ mode, runtime, need string }{
		{"agent_dial", noSpareRuntimePorts, "ssh_key"},
		{"server_dial", noSpareRuntimePorts, "ssh_key_port"},
		{"server_dial", spareRuntimePorts, "ssh_key"},
	} {
		api := &fakePodAPI{pods: []string{podWithEnv([]string{"22/tcp", "18801/tcp"}, tc.runtime, 10033, "ssh-ed25519 AAAAC3other me")}}
		d := newTestDriver(t, api.handler(t))
		d.loginHook = func(context.Context, provider.Instance, provision.SSHAuth) error { return authFailure() }
		_, _, err := d.ensurePlatformKey(context.Background(), provider.Credentials{APIKey: "k"}, keyRequest(tc.mode, false))
		var restart *provider.InstanceRestartError
		if !errors.As(err, &restart) || restart.Need != tc.need || !strings.Contains(restart.Reason, "restarts its container") {
			t.Fatalf("%s (spare port %v): err = %v, want need %s", tc.mode, tc.runtime == spareRuntimePorts, err, tc.need)
		}
		if len(api.patches) != 0 {
			t.Fatalf("the pod was edited without consent: %v", api.patches)
		}
	}
}

// With consent one edit appends the key to PUBLIC_KEY (keeping the rest of
// the environment, which the edit replaces whole) and exposes a port for a
// server-dialed agent; the install then logs in to the renumbered SSH with
// the platform key.
func TestPlatformKeyAddedWithConsentInOneEdit(t *testing.T) {
	before := podWithEnv([]string{"22/tcp", "22/udp"}, noSpareRuntimePorts, 10033, "ssh-ed25519 AAAAC3other me")
	after := podWithEnv([]string{"22/tcp", "22/udp", "18800/tcp"}, strings.Replace(spareRuntimePorts, `"private":18801`, `"private":18800`, 1), 15755, "ssh-ed25519 AAAAC3other me\n"+platformPublic)
	api := &fakePodAPI{pods: []string{before, before, after}}
	d := newTestDriver(t, api.handler(t))
	d.portPollInterval, d.portWaitTimeout = time.Millisecond, 5*time.Second
	var logins []string
	d.loginHook = func(_ context.Context, inst provider.Instance, auth provision.SSHAuth) error {
		logins = append(logins, itoa(inst.SSHPort))
		if inst.SSHPort == 15755 && auth.PlatformKey != nil {
			return nil
		}
		return authFailure()
	}
	inst, check, err := d.ensurePlatformKey(context.Background(), provider.Credentials{APIKey: "k"}, keyRequest("server_dial", true))
	if err != nil {
		t.Fatal(err)
	}
	if len(api.patches) != 1 {
		t.Fatalf("patches = %v", api.patches)
	}
	env, _ := api.patches[0]["env"].(map[string]any)
	ports, _ := json.Marshal(api.patches[0]["ports"])
	if env["PUBLIC_KEY"] != "ssh-ed25519 AAAAC3other me\n"+platformPublic || env["JUPYTER_PASSWORD"] != "jp" || string(ports) != `["22/tcp","22/udp","18800/tcp"]` {
		t.Fatalf("edit = env %v ports %s", env, ports)
	}
	if inst.SSHPort != 15755 || check.ID != "platform_key_provider" || !strings.Contains(checkText(check), "exposed container port 18800") {
		t.Fatalf("instance port %d, check %+v", inst.SSHPort, check)
	}
	if logins[0] != "10033" || logins[len(logins)-1] != "15755" {
		t.Fatalf("logins = %v", logins)
	}
}

// The precheck learns that the install will add the key to a pod lacking
// it, and that a pod which lists it already cannot be helped that way.
func TestPlatformKeyAtInstallOnlyForAPodLackingTheKey(t *testing.T) {
	for name, tc := range map[string]struct {
		publicKey string
		ok        bool
	}{
		"lacks it": {publicKey: "ssh-ed25519 AAAAC3other me", ok: true},
		"lists it": {publicKey: platformPublic, ok: false},
	} {
		t.Run(name, func(t *testing.T) {
			api := &fakePodAPI{pods: []string{podWithEnv([]string{"22/tcp"}, noSpareRuntimePorts, 10033, tc.publicKey)}}
			d := newTestDriver(t, api.handler(t))
			check, ok := d.PlatformKeyAtInstall(context.Background(), provider.Credentials{APIKey: "k"}, provider.Instance{ExternalID: "pod-1"}, platformPublic)
			if ok != tc.ok || len(api.patches) != 0 {
				t.Fatalf("ok=%v patches=%v", ok, api.patches)
			}
			if ok && (check.Status != "warn" || !strings.Contains(checkText(check), "restarts the pod")) {
				t.Fatalf("check = %+v", check)
			}
		})
	}
}
