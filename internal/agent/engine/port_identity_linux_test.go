package engine

import (
	"net"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// The engine binds every address. On Linux a listener on loopback alone
// already takes the port from it; BSD kernels, macOS included, let a wildcard
// bind share the port with a more specific one, so there the engine would
// start as well and the check rightly lets it.
func TestCheckEnginePortsRefusesALoopbackOnlyListener(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port
	err = checkEnginePorts(proto.InstanceDesiredState{APIPort: port, Ports: []int{port}})
	if err == nil || !strings.HasPrefix(err.Error(), proto.ErrorEnginePortRefused) {
		t.Fatalf("loopback listener: err = %v", err)
	}
}
