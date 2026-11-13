package gpu

import (
	"os"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestKunlunTransportFailureCarriesStableResource(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		xdr      bool
		resource string
	}{
		{nil, true, "/dev/infiniband"},
		{[]string{"--device", "/dev/infiniband", "--ulimit", "memlock=4096", "--cap-add", "IPC_LOCK"}, true, "memlock"},
		{[]string{"--device", "/dev/infiniband", "--ulimit", "memlock=-1"}, true, "IPC_LOCK"},
		{[]string{"--device", "/dev/infiniband", "--ulimit", "memlock=-1", "--cap-add", "IPC_LOCK"}, false, "/dev/xdrdrv"},
	} {
		p, _ := kunlunFixture(t)
		if _, err := p.Probe(t.Context()); err != nil {
			t.Fatal(err)
		}
		if tc.xdr {
			withKunlunXDR(p, os.ModeDevice|os.ModeCharDevice)
		}
		got := p.RuntimeInjectionForLaunch(t.Context(), InjectionRequest{Indexes: []int{0}, DriverName: "docker", NNodes: 2, DockerArgs: tc.args, Env: validKunlunMultiNodeEnv()})
		if got.Failure == nil || got.Failure.Code != proto.ErrorAcceleratorResourceUnavailable || got.Failure.Resource != tc.resource || got.UnsupportedReason == "" || len(got.DockerArgs) != 0 || len(got.Env) != 0 {
			t.Fatalf("resource=%s got=%+v", tc.resource, got)
		}
	}
}
