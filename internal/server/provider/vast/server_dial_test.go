package vast

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// The ports field of a Vast instance as the API returned it (56735911,
// vastai/base-image, 2026-10-19).
const realPorts = `{"10100/tcp": [{"HostIp": "0.0.0.0", "HostPort": "18211"}, {"HostIp": "::", "HostPort": "18211"}], "10200/tcp": [{"HostIp": "0.0.0.0", "HostPort": "18685"}, {"HostIp": "::", "HostPort": "18685"}], "1111/tcp": [{"HostIp": "0.0.0.0", "HostPort": "18222"}, {"HostIp": "::", "HostPort": "18222"}], "18204/tcp": [{"HostIp": "0.0.0.0", "HostPort": "18204"}, {"HostIp": "::", "HostPort": "18204"}], "22/tcp": [{"HostIp": "0.0.0.0", "HostPort": "18607"}, {"HostIp": "::", "HostPort": "18607"}], "6006/tcp": [{"HostIp": "0.0.0.0", "HostPort": "18335"}, {"HostIp": "::", "HostPort": "18335"}], "8080/tcp": [{"HostIp": "0.0.0.0", "HostPort": "18847"}, {"HostIp": "::", "HostPort": "18847"}], "8384/tcp": [{"HostIp": "0.0.0.0", "HostPort": "18168"}, {"HostIp": "::", "HostPort": "18168"}], "5353/udp": [{"HostIp": "0.0.0.0", "HostPort": "18999"}]}`

func TestTCPMappingsReadTheInstancesPorts(t *testing.T) {
	in := instance{Ports: json.RawMessage(realPorts)}
	got := in.tcpMappings()
	want := map[int]int{10100: 18211, 10200: 18685, 1111: 18222, 18204: 18204, 22: 18607, 6006: 18335, 8080: 18847, 8384: 18168}
	if len(got) != len(want) {
		t.Fatalf("mappings = %v", got)
	}
	for container, public := range want {
		if got[container] != public {
			t.Fatalf("mappings = %v, want %v", got, want)
		}
	}
	if (&instance{}).tcpMappings() == nil || len((&instance{Ports: json.RawMessage(`"x"`)}).tcpMappings()) != 0 {
		t.Fatal("an instance without readable ports maps nothing")
	}
}

// After a restart Vast may map the agent's container port to another host
// port; the server learns the new address from the instance list.
func TestServerDialAddressForReadsTheCurrentMapping(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/instances/" || r.Header.Get("Authorization") != "Bearer key" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		writeFilteredInstanceFixture(t, w, r, `{"instances":[{"id":56735911,"public_ipaddr":"47.224.206.51","ports":`+realPorts+`}]}`)
	})
	creds := provider.Credentials{APIKey: "key"}
	got, err := d.ServerDialAddressFor(context.Background(), creds, "56735911", "0.0.0.0:10100")
	if err != nil || got != "47.224.206.51:18211" {
		t.Fatalf("ServerDialAddressFor = %q, %v", got, err)
	}
	if _, err := d.ServerDialAddressFor(context.Background(), creds, "56735911", "0.0.0.0:7000"); err == nil || !strings.Contains(err.Error(), "no longer maps container port 7000") {
		t.Fatalf("unmapped port: %v", err)
	}
	if _, err := d.ServerDialAddressFor(context.Background(), creds, "1", "0.0.0.0:10100"); err == nil {
		t.Fatal("a missing instance resolved")
	}
}
