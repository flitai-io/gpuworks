package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// listenTCP is the bind checkEnginePorts tries; tests replace it through
// checkEnginePortsWith.
func listenTCP(port int) (io.Closer, error) {
	return net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(port)))
}

// defaultPortCheck is the check a new Engine runs before creating a
// container. The package's tests replace it once, before any test runs, since
// their stand-in engines listen on the port before the container "starts".
var defaultPortCheck = checkEnginePorts

// checkEnginePorts makes sure the ports the server allocated are still free
// before a container is created. The server avoids the listeners a node last
// reported, but that report can be a status interval old, or not there yet
// right after the agent connected: a port another process (another gpuworks
// on the host, any service) started listening on meanwhile would otherwise be
// found only once the engine fails to bind it, after loading its weights.
func checkEnginePorts(spec proto.InstanceDesiredState) error {
	return checkEnginePortsWith(spec, listenTCP)
}

// portCheckFor is the pre-create port check for driver: a runtime that binds
// host ports in its own terms (runtime.HostPortBinder) answers the bind, any
// other gets the real one.
func portCheckFor(driver runtime.Driver) func(proto.InstanceDesiredState) error {
	if binder, ok := driver.(runtime.HostPortBinder); ok {
		return func(spec proto.InstanceDesiredState) error {
			return checkEnginePortsWith(spec, binder.BindHostPort)
		}
	}
	return defaultPortCheck
}

func checkEnginePortsWith(spec proto.InstanceDesiredState, listen func(int) (io.Closer, error)) error {
	ports := append([]int{}, spec.Ports...)
	if spec.APIPort > 0 && !containsPort(ports, spec.APIPort) {
		ports = append(ports, spec.APIPort)
	}
	for _, port := range ports {
		if port <= 0 {
			continue
		}
		ln, err := listen(port)
		if err == nil {
			_ = ln.Close()
			continue
		}
		if errors.Is(err, syscall.EADDRINUSE) {
			return withFailure(proto.RuntimeFailure{Code: proto.ErrorEnginePortRefused, Port: port},
				fmt.Errorf("%s: port %d is already in use on this node; nothing was started", proto.ErrorEnginePortRefused, port))
		}
		// Anything else (a port the agent may not bind as a user, for one)
		// says nothing about another listener; the engine finds out itself.
	}
	return nil
}

func containsPort(ports []int, port int) bool {
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}

// servedName is the model name the engine is told to answer under: the
// server's ServedModelName. A desired state from a server that predates the
// field is read the old way (legacyServedName).
func servedName(spec proto.InstanceDesiredState) string {
	if spec.ServedModelName != "" {
		return spec.ServedModelName
	}
	return legacyServedName(spec)
}

var servedNameFlag = regexp.MustCompile(`--served-model-name(?:=|\s+)("[^"]*"|'[^']*'|\S+)`)

// legacyServedName reads --served-model-name off the command line, for a
// desired state from a server without ServedModelName. It can go once no
// server older than the field is supported.
func legacyServedName(spec proto.InstanceDesiredState) string {
	m := servedNameFlag.FindStringSubmatch(strings.Join(append(append([]string{}, spec.Entrypoint...), spec.Argv...), " "))
	if m == nil {
		return ""
	}
	return strings.Trim(m[1], `"'`)
}

// errForeignEngine says the API port answered for a model this instance does
// not serve: another engine owns the port.
var errForeignEngine = errors.New("another engine answers on this port")

// checkServesOwnModel asks the engine that answered the health check which
// models it serves. Only a well-formed OpenAI model list that lacks this
// instance's served name counts against it: an engine without /v1/models, or
// one that answers in another shape, is taken at its health check.
func checkServesOwnModel(ctx context.Context, client *http.Client, spec proto.InstanceDesiredState) error {
	want := servedName(spec)
	if want == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/v1/models", spec.APIPort), nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&list); err != nil || list.Object != "list" || len(list.Data) == 0 {
		return nil
	}
	served := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID == want {
			return nil
		}
		served = append(served, m.ID)
	}
	return withFailure(proto.RuntimeFailure{Code: proto.ErrorEnginePortInUse, Port: spec.APIPort, Detail: strings.Join(served, ", ")},
		fmt.Errorf("%s: %w (port %d serves %s, not %s)", proto.ErrorEnginePortInUse, errForeignEngine, spec.APIPort, strings.Join(served, ", "), want))
}
