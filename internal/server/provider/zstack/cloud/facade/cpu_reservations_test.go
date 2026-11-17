package facade_test

import (
	"fmt"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/facade"
)

func TestCPUReservationBindingAndGlobalFallback(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		global      string
		value       int64
		bound       bool
		globalCalls int32
		wantError   bool
	}{
		{"host override", 200, `{"value":"5"}`, "3", 5, true, 0, false},
		{"host inherits global", 200, `{}`, "3", 3, true, 1, false},
		{"Cloud unbound resource config", 503, `{"error":{"code":"SYS.1007","details":"global config[category:kvm, name:kvm.reservedCpu] cannot bind resource"}}`, "3", 3, false, 1, false},
		{"Chinese unbound resource config", 503, `{"error":{"code":"SYS.1007","details":"全局配置[类别：kvm，名称：kvm.reservedCpu]无法绑定资源"}}`, "3", 3, false, 1, false},
		{"existing diagnostic", 503, `{"diagnostic":{"code":"20007"},"error":{"code":"SYS.1007"}}`, "3", 3, false, 1, false},
		{"forbidden", 403, `{"error":{"code":"SYS.1007"}}`, "3", 0, false, 0, true},
		{"other invalid argument", 503, `{"error":{"code":"SYS.1007","details":"invalid host"}}`, "3", 0, false, 0, true},
		{"invalid host value", 200, `{"value":"-1"}`, "3", 0, true, 0, true},
		{"invalid global value", 200, `{}`, "unknown", 0, true, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var globalCalls atomic.Int32
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/resource-configurations/host/kvm/reservedCpu":
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
				case "/v1/global-configurations":
					globalCalls.Add(1)
					if !reflect.DeepEqual(r.URL.Query()["q"], []string{"category=kvm", "name=reservedCpu"}) {
						t.Errorf("global configuration filters changed: %s", r.URL)
					}
					fmt.Fprintf(w, `{"inventories":[{"name":"reservedCpu","category":"kvm","value":%q}]}`, tc.global)
				default:
					t.Errorf("unexpected request: %s", r.URL)
					http.NotFound(w, r)
				}
			})
			got, err := facade.NewCPUReservations(client).Get(t.Context(), "host")
			if (err != nil) != tc.wantError || got.HostOverrideAvailable != tc.bound || globalCalls.Load() != tc.globalCalls {
				t.Fatalf("reservation=%+v err=%v global calls=%d", got, err, globalCalls.Load())
			}
			if !tc.wantError && got.Value != tc.value {
				t.Fatalf("effective reservation=%d, want %d", got.Value, tc.value)
			}
		})
	}
}
