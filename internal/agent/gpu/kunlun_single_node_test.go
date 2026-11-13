package gpu

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// A host with native XPU devices but no XDR/RDMA devices must still be usable
// for local one-card and multi-card scheduling. Unknown filesystem paths fail.
func TestKunlunSingleNodeUsesOnlyNativeDevices(t *testing.T) {
	for _, indexes := range [][]int{{1}, {0, 1}} {
		t.Run(fmt.Sprint(indexes), func(t *testing.T) {
			p, _ := kunlunFixture(t)
			if _, err := p.Probe(t.Context()); err != nil {
				t.Fatal(err)
			}
			inspect := p.stat
			p.stat = func(path string) (os.FileInfo, error) {
				if path == "/dev/xdrdrv" || strings.HasPrefix(path, "/dev/infiniband") {
					t.Fatalf("single-node injection inspected a transport device: %s", path)
				}
				return inspect(path)
			}
			var prober Prober = p
			injection := prober.(DriverAwareInjector).RuntimeInjectionForDriver(indexes, "docker")
			if injection.UnsupportedReason != "" {
				t.Fatal(injection.UnsupportedReason)
			}
			want := []string{"--runtime", "runc"}
			for _, index := range indexes {
				path := fmt.Sprintf("/dev/xpu%d", index)
				want = append(want, "--device", path+":"+path+":rwm")
			}
			want = append(want, "--device", "/dev/xpuctrl:/dev/xpuctrl:rwm")
			logical := make([]string, len(indexes))
			for i := range indexes {
				logical[i] = strconv.Itoa(i)
			}
			wantEnv := map[string]string{"XPU_VISIBLE_DEVICES": strings.Join(logical, ",")}
			if !reflect.DeepEqual(injection.DockerArgs, want) || !reflect.DeepEqual(injection.Env, wantEnv) || len(injection.Mounts) != 0 {
				t.Fatalf("unexpected local device contract: %+v", injection)
			}
		})
	}
}
