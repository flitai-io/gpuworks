package apiclient

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func TestProtocolFailuresRequireExactCloudEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		status int
		body   string
		want   ProtocolFailure
	}{
		{"missing route", "/gpu-device/gpu-devices?limit=1", 404, "no api mapping to /v1/gpu-device/gpu-devices", APIRouteMissing},
		{"proxy 404", "/gpu-device/gpu-devices", 404, "404 page not found", ""},
		{"other route", "/gpu-device/gpu-devices", 404, "no api mapping to /v1/hosts", ""},
		{"permission failure", "/gpu-device/gpu-devices", 403, "no api mapping to /v1/gpu-device/gpu-devices", ""},
		{"gateway failure", "/gpu-device/gpu-devices", 502, "no api mapping to /v1/gpu-device/gpu-devices", ""},
		{"unbound config", "/resource-configurations/host/kvm/reservedCpu", 503, `{"error":{"code":"SYS.1007","details":"global config[category:kvm, name:kvm.reservedCpu] cannot bind resource"}}`, ResourceConfigUnbound},
		{"Chinese unbound config", "/resource-configurations/host/kvm/reservedCpu", 503, `{"error":{"code":"SYS.1007","details":"全局配置[类别：kvm，名称：kvm.reservedCpu]无法绑定资源"}}`, ResourceConfigUnbound},
		{"Chinese other config", "/resource-configurations/host/kvm/reservedCpu", 503, `{"error":{"code":"SYS.1007","details":"全局配置[类别：kvm，名称：kvm.reservedMemory]无法绑定资源"}}`, ""},
		{"Chinese other category", "/resource-configurations/host/kvm/reservedCpu", 503, `{"error":{"code":"SYS.1007","details":"全局配置[类别：host，名称：kvm.reservedCpu]无法绑定资源"}}`, ""},
		{"Chinese wrong code", "/resource-configurations/host/kvm/reservedCpu", 503, `{"error":{"code":"SYS.1006","details":"全局配置[类别：kvm，名称：kvm.reservedCpu]无法绑定资源"}}`, ""},
		{"Chinese config forbidden", "/resource-configurations/host/kvm/reservedCpu", 403, `{"error":{"code":"SYS.1007","details":"全局配置[类别：kvm，名称：kvm.reservedCpu]无法绑定资源"}}`, ""},
		{"other config", "/resource-configurations/host/kvm/reservedCpu", 503, `{"error":{"code":"SYS.1007","details":"global config[category:kvm, name:kvm.reservedMemory] cannot bind resource"}}`, ""},
		{"other operation", "/hosts", 503, `{"error":{"code":"SYS.1007","details":"global config[category:kvm, name:kvm.reservedCpu] cannot bind resource"}}`, ""},
		{"other argument", "/resource-configurations/host/kvm/reservedCpu", 503, `{"error":{"code":"SYS.1007","details":"invalid host secret-should-not-leak"}}`, ""},
		{"wrong code", "/resource-configurations/host/kvm/reservedCpu", 503, `{"error":{"code":"SYS.1006","details":"global config[category:kvm, name:kvm.reservedCpu] cannot bind resource"}}`, ""},
		{"config forbidden", "/resource-configurations/host/kvm/reservedCpu", 403, `{"error":{"code":"SYS.1007","details":"global config[category:kvm, name:kvm.reservedCpu] cannot bind resource"}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !validTestAccessKey(r) {
					t.Error("invalid signature")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			c, err := New(provider.Credentials{Endpoint: server.URL + "/zstack", PublicKey: "test-access-key", PrivateKey: "test-access-secret"}, WithReadRetries(0))
			if err != nil {
				t.Fatal(err)
			}
			var out map[string]any
			err = c.Get(t.Context(), tc.path, &out)
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.ProtocolFailure != tc.want || apiErr.StatusCode != tc.status {
				t.Fatalf("error = %#v, want HTTP %d / %q", err, tc.status, tc.want)
			}
			if strings.Contains(err.Error(), "secret-should-not-leak") {
				t.Fatal("remote details leaked")
			}
		})
	}
}
