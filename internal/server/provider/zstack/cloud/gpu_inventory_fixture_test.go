package cloud

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func gpuInventoryCloud(t *testing.T, devicePath string, devices http.HandlerFunc) provider.Credentials {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet || !validTestAccessKey(r) {
			t.Errorf("unexpected Cloud request: %s %s", r.Method, r.URL)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case devicePath:
			devices(w, r)
		case "/v1/hosts":
			fmt.Fprint(w, `{"inventories":[{"uuid":"host","name":"GPU host","hypervisorType":"KVM","state":"Enabled","status":"Connected"}]}`)
		case "/v1/gpu-device/gpu-devices":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "no api mapping to "+r.URL.Path)
		case "/v1/resource-configurations/host/kvm/reservedMemory":
			fmt.Fprint(w, `{"value":"1G"}`)
		case "/v1/resource-configurations/host/kvm/reservedCpu":
			fmt.Fprint(w, `{"value":"2"}`)
		default:
			t.Errorf("unexpected Cloud query: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return provider.Credentials{Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret"}
}
