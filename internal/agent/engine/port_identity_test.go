package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// A listener on every address takes the port on any platform. That one on
// loopback alone does as well is Linux behaviour, covered in
// port_identity_linux_test.go.
func TestCheckEnginePortsRefusesAPortSomeoneListensOn(t *testing.T) {
	busy, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	freePort := free.Addr().(*net.TCPAddr).Port
	free.Close()

	err = checkEnginePorts(proto.InstanceDesiredState{APIPort: port, Ports: []int{port}})
	if err == nil || !strings.HasPrefix(err.Error(), proto.ErrorEnginePortRefused) || !strings.Contains(err.Error(), strconv.Itoa(port)) {
		t.Fatalf("busy port: err = %v", err)
	}
	// A side-channel port of a PD instance counts as much as the API port.
	if err := checkEnginePorts(proto.InstanceDesiredState{APIPort: freePort, Ports: []int{freePort, port}}); err == nil {
		t.Fatal("busy side-channel port accepted")
	}
	if err := checkEnginePorts(proto.InstanceDesiredState{APIPort: freePort, Ports: []int{freePort}}); err != nil {
		t.Fatalf("free port: err = %v", err)
	}
}

type closer struct{}

func (closer) Close() error { return nil }

func TestCheckEnginePortsIgnoresBindErrorsThatSayNothingAboutListeners(t *testing.T) {
	var tried []int
	listen := func(port int) (io.Closer, error) {
		tried = append(tried, port)
		if port == 80 {
			return nil, &net.OpError{Op: "listen", Err: syscall.EACCES}
		}
		return closer{}, nil
	}
	if err := checkEnginePortsWith(proto.InstanceDesiredState{APIPort: 80, Ports: []int{80, 30001}}, listen); err != nil {
		t.Fatalf("permission denied is not a port in use: %v", err)
	}
	if len(tried) != 2 {
		t.Fatalf("tried %v, want both ports", tried)
	}
	inUse := func(int) (io.Closer, error) { return nil, &net.OpError{Op: "listen", Err: syscall.EADDRINUSE} }
	if err := checkEnginePortsWith(proto.InstanceDesiredState{APIPort: 30000}, inUse); err == nil {
		t.Fatal("EADDRINUSE accepted")
	}
}

func TestServedNameReadsTheLaunchCommand(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"vllm", "serve", "/model", "--served-model-name", "Qwen/Qwen3-0.6B", "--port", "30000"}, "Qwen/Qwen3-0.6B"},
		{[]string{"python3", "-m", "sglang.launch_server", "--served-model-name=q06"}, "q06"},
		{[]string{"sh", "-c", `vllm serve /model --served-model-name "my model"`}, "my model"},
		{[]string{"llama-server", "-m", "/model/x.gguf"}, ""},
	} {
		if got := servedName(proto.InstanceDesiredState{ResolvedLaunch: proto.ResolvedLaunch{Argv: c.argv}}); got != c.want {
			t.Errorf("servedName(%v) = %q, want %q", c.argv, got, c.want)
		}
	}
}

func modelsServer(t *testing.T, status int, body string) (int, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	port, _ := strconv.Atoi(srv.URL[strings.LastIndex(srv.URL, ":")+1:])
	return port, srv.Close
}

func TestCheckServesOwnModelOnlyRefusesAListWithoutOurName(t *testing.T) {
	spec := func(port int) proto.InstanceDesiredState {
		return proto.InstanceDesiredState{APIPort: port, ResolvedLaunch: proto.ResolvedLaunch{Argv: []string{"vllm", "serve", "/model", "--served-model-name", "q06"}}}
	}
	client := &http.Client{}
	ctx := context.Background()
	cases := []struct {
		name    string
		status  int
		body    string
		foreign bool
	}{
		{"our model", 200, `{"object":"list","data":[{"id":"q06"},{"id":"q06-lora"}]}`, false},
		{"another engine's model", 200, `{"object":"list","data":[{"id":"other-team-model"}]}`, true},
		{"no model list endpoint", 404, `not found`, false},
		{"another shape", 200, `{"models":["x"]}`, false},
		{"empty list", 200, `{"object":"list","data":[]}`, false},
	}
	for _, c := range cases {
		port, stop := modelsServer(t, c.status, c.body)
		err := checkServesOwnModel(ctx, client, spec(port))
		stop()
		if c.foreign != (err != nil) {
			t.Errorf("%s: err = %v, want foreign=%v", c.name, err, c.foreign)
		}
		if c.foreign && (!errors.Is(err, errForeignEngine) || !strings.Contains(err.Error(), "other-team-model")) {
			t.Errorf("%s: err = %v, want it to name what the port serves", c.name, err)
		}
	}
	// Nothing listening at all is the health check's business, not this one's.
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	port := closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	if err := checkServesOwnModel(ctx, client, spec(port)); err != nil {
		t.Fatalf("unreachable port: %v", err)
	}
	// A launch that names no served model is not checked.
	port, stop := modelsServer(t, 200, `{"object":"list","data":[{"id":"x"}]}`)
	defer stop()
	if err := checkServesOwnModel(ctx, client, proto.InstanceDesiredState{APIPort: port}); err != nil {
		t.Fatalf("no served name: %v", err)
	}
}

// A port another process took after the server allocated it fails the launch
// before anything is pulled or created, with a reason the server can act on.
func TestStartRefusesAPortTakenSinceAllocation(t *testing.T) {
	busy, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port
	driver := &imageToleranceDriver{exists: []bool{true, true}}
	reported := make(chan proto.InstanceStatus, 16)
	e := NewWithProber(driver, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir(), injectionProber{})
	e.checkPorts = checkEnginePorts
	e.startOrAdopt(context.Background(), proto.InstanceDesiredState{UUID: "port-taken", Image: "local/build:latest", APIPort: port, Ports: []int{port}, StartTimeout: 1})
	if driver.pullCalls != 0 || driver.runCalls != 0 {
		t.Fatalf("pull/run calls = %d/%d, want nothing started", driver.pullCalls, driver.runCalls)
	}
	var last proto.InstanceStatus
	for done := false; !done; {
		select {
		case last = <-reported:
		default:
			done = true
		}
	}
	if last.State != proto.InstFailed || last.RuntimeFailure == nil || last.RuntimeFailure.Code != proto.ErrorEnginePortRefused || last.RuntimeFailure.Port != port ||
		last.RuntimeFailure.Phase != proto.PhaseContainerPrepare {
		t.Fatalf("final status = %+v, want failed with %s", last, proto.ErrorEnginePortRefused)
	}
}

// The server's ServedModelName is the name checked; a desired state from a
// server without the field is read off the command line as before.
func TestServedNamePrefersTheServersField(t *testing.T) {
	spec := proto.InstanceDesiredState{ServedModelName: "from-server", ResolvedLaunch: proto.ResolvedLaunch{Argv: []string{"--served-model-name", "from-argv"}}}
	if got := servedName(spec); got != "from-server" {
		t.Fatalf("servedName = %q, want the server's", got)
	}
	spec.ServedModelName = ""
	if got := servedName(spec); got != "from-argv" {
		t.Fatalf("legacy servedName = %q", got)
	}
}

// portBinderDriver answers the pre-create bind itself (runtime.HostPortBinder).
type portBinderDriver struct {
	imageToleranceDriver
	taken map[int]bool
	asked []int
}

func (d *portBinderDriver) BindHostPort(port int) (io.Closer, error) {
	d.asked = append(d.asked, port)
	if d.taken[port] {
		return nil, &net.OpError{Op: "listen", Net: "tcp", Err: syscall.EADDRINUSE}
	}
	return closer{}, nil
}

// A runtime that binds host ports in its own terms answers the check: a port
// busy on the host is fine when the runtime says it is free, and one the
// runtime holds is refused (ENGINE_PORT_REFUSED).
func TestPortCheckAsksARuntimeThatBindsItsOwnPorts(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	hostBusy := busy.Addr().(*net.TCPAddr).Port
	driver := &portBinderDriver{taken: map[int]bool{31001: true}}
	check := portCheckFor(driver)
	if err := check(proto.InstanceDesiredState{APIPort: hostBusy, Ports: []int{hostBusy}}); err != nil {
		t.Fatalf("port free in the runtime's terms: err = %v", err)
	}
	err = check(proto.InstanceDesiredState{APIPort: 31001, Ports: []int{31001}})
	if err == nil || !strings.HasPrefix(err.Error(), proto.ErrorEnginePortRefused) {
		t.Fatalf("port the runtime holds: err = %v", err)
	}
	if len(driver.asked) != 2 {
		t.Fatalf("runtime asked about %v, want both ports", driver.asked)
	}
}

// A launch with no alias is named after its model mount ("/model"). An engine
// that answers under that name is ours; one that answers under the model's
// host path is what an agent that translated the name would have started, and
// the gateway, which sends "/model", could not reach it. The bodies are vLLM's
// /v1/models shape.
func TestCheckServesOwnModelUnderTheMountName(t *testing.T) {
	spec := func(port int) proto.InstanceDesiredState {
		return proto.InstanceDesiredState{
			APIPort: port, ServedModelName: "/model",
			ResolvedLaunch: proto.ResolvedLaunch{Entrypoint: []string{"vllm", "serve", "/model"}, Argv: []string{"--served-model-name", "/model"}},
		}
	}
	const model = `{"object":"list","data":[{"id":%q,"object":"model","created":1763337600,"owned_by":"vllm","root":"/Users/x/models/q06","parent":null,"max_model_len":2048,"permission":[]}]}`
	for _, c := range []struct {
		name    string
		served  string
		foreign bool
	}{
		{"named after the mount", "/model", false},
		{"named after the host path", "/Users/x/models/q06", true},
	} {
		port, stop := modelsServer(t, 200, fmt.Sprintf(model, c.served))
		err := checkServesOwnModel(context.Background(), &http.Client{}, spec(port))
		stop()
		if c.foreign != (err != nil) {
			t.Errorf("%s: err = %v, want foreign=%v", c.name, err, c.foreign)
		}
		if c.foreign && failureOf(err, "", "").Code != proto.ErrorEnginePortInUse {
			t.Errorf("%s: err = %v, want %s", c.name, err, proto.ErrorEnginePortInUse)
		}
	}
}
