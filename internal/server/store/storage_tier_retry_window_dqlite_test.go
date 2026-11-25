//go:build linux && dqlite

package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

// A membership grant interrupted by a leadership change is repeated inside
// clusterMembershipRetryWindow. A follower campaigns up to two election
// timeouts after the leader is gone, and the repeated grant then needs one
// replicated commit. A storage tier whose election outlasts the window turns
// every leader change into refused requests, so a slower tier may only be
// added together with a longer window, and the window is capped by the time
// an isolated member must refuse within.
func TestEveryStorageTierElectsInsideTheMembershipRetryWindow(t *testing.T) {
	for _, tier := range storagetier.All() {
		if need := tier.GrantRetryNeed(); need > clusterMembershipRetryWindow() || need > GrantRetryWindowFor(tier) {
			t.Errorf("storage tier %s: longest election timer plus one commit is %s, beyond the retry window %s (%s for the tier); the retry window must grow with the tier", tier, need, clusterMembershipRetryWindow(), GrantRetryWindowFor(tier))
		}
	}
	if window := clusterMembershipRetryWindow(); window >= ClusterMembershipGrantTimeout {
		t.Fatalf("retry window %s must stay inside the grant timeout %s", window, ClusterMembershipGrantTimeout)
	}
}
