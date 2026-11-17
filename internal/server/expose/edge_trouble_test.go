package expose

import (
	"errors"
	"fmt"
	"testing"
)

// Lines as cloudflared 2025.x prints them on stderr.
const (
	cfQUICTimeout     = `2026-11-14T08:00:01Z ERR Failed to dial to edge with quic: timeout: no recent network activity connIndex=0 event=0 ip=198.41.192.7`
	cfIPv6Unreachable = `2026-11-14T08:00:01Z ERR Unable to establish connection with Cloudflare edge error="DialContext error: dial tcp [2606:4700:a0::1]:7844: connect: network is unreachable" connIndex=0`
	cfRegistration    = `2026-11-14T08:00:01Z ERR Error unmarshaling QuickTunnel response: error="Post \"https://api.trycloudflare.com/tunnel\": dial tcp [2606:4700::6810:d1ae]:443: connect: network is unreachable"`
	cfOther           = `2026-11-14T08:00:01Z WRN Cannot determine default configuration path. No file [config.yml config.yaml]`
)

func TestEdgeTroubleOfReadsCloudflaredOutput(t *testing.T) {
	for line, want := range map[string]edgeTrouble{
		cfQUICTimeout:     edgeTroubleQUIC,
		cfIPv6Unreachable: edgeTroubleIPv6,
		cfRegistration:    edgeTroubleRegistration,
		cfOther:           edgeTroubleNone,
	} {
		if got := edgeTroubleOf(line); got != want {
			t.Errorf("edgeTroubleOf(%q) = %v, want %v", line, got, want)
		}
	}
}

func TestNextNetworkFollowsTheConnectorsLastTroubleLine(t *testing.T) {
	start := func(lines ...string) error {
		return newConnectorStartError(StreamState{Trouble: lines}, "the connector exited before it registered")
	}
	for _, tc := range []struct {
		name string
		err  error
		want Network
		ok   bool
	}{
		{"quic", start(cfOther, cfQUICTimeout), Network{EdgeIPVersion: "4", Protocol: "http2"}, true},
		{"ipv6 wrapped", fmt.Errorf("attempt: %w", start(cfIPv6Unreachable)), Network{EdgeIPVersion: "4"}, true},
		{"registration", start(cfQUICTimeout, cfRegistration), Network{}, false},
		{"no trouble", start(), Network{}, false},
		// An error this server wrote itself is never read for its words:
		// only a connector start failure says anything about the edge.
		{"own text", errors.New("quic network is unreachable over ipv6"), Network{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := nextNetwork(Network{}, tc.err)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("nextNetwork = %+v,%v want %+v,%v", got, ok, tc.want, tc.ok)
			}
		})
	}
	if err := start(cfQUICTimeout); err.Error() != "the connector exited before it registered: "+cfQUICTimeout {
		t.Fatalf("message = %q", err)
	}
}
