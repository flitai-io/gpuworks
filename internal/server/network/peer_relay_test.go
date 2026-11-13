package network

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestPeerRelayAllowed(t *testing.T) {
	allowed := &store.Node{}
	denied := &store.Node{ManagementRolesOff: `["storage_sync"]`}
	other := &store.Node{ManagementRolesOff: `["compute","serving"]`}
	for _, tc := range []struct {
		name           string
		source, target *store.Node
		want           bool
	}{
		{"both allowed", allowed, allowed, true}, {"other roles", other, allowed, true},
		{"source disabled", denied, allowed, false}, {"target disabled", allowed, denied, false},
		{"nil source", nil, allowed, false}, {"nil target", allowed, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PeerRelayAllowed(tc.source, tc.target); got != tc.want {
				t.Fatalf("allowed=%v want=%v", got, tc.want)
			}
		})
	}
}
