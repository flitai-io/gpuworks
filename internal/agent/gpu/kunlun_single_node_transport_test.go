package gpu

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// Adding cross-node support must not add XDR or RDMA requirements to local TP.
func TestKunlunSingleNodeTopologySkipsTransport(t *testing.T) {
	p, _ := kunlunFixture(t)
	if _, err := p.Probe(t.Context()); err != nil {
		t.Fatal(err)
	}
	inspect := p.stat
	p.stat = func(path string) (os.FileInfo, error) {
		if path == "/dev/xdrdrv" || strings.HasPrefix(path, "/dev/infiniband") {
			t.Fatalf("single-node launch inspected transport device: %s", path)
		}
		return inspect(path)
	}
	indexes := []int{1}
	native := p.RuntimeInjectionForDriver(indexes, "docker")
	launch := p.RuntimeInjectionForLaunch(t.Context(), InjectionRequest{Indexes: indexes, DriverName: "docker", NNodes: 1})
	if launch.UnsupportedReason != "" || !reflect.DeepEqual(launch, native) {
		t.Fatalf("local topology changed native injection: %+v", launch)
	}
}
