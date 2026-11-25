package store

import (
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
)

// baseGrantRetryWindow is the retry window of the tiers at their own timing.
const baseGrantRetryWindow = 2200 * time.Millisecond

// grantRetryHeadroom is what a grant keeps of its timeout after the retry
// window, for the attempt that succeeds.
const grantRetryHeadroom = 300 * time.Millisecond

// clusterMembershipRetryWindow bounds, from the start of a grant, how long an
// interrupted live observation may be repeated. An observation that spans a
// database leadership change can use its whole timeout, and the next one
// then answers in milliseconds; a member cut off from the majority sees the
// same interruption every time and must still refuse a direct write shortly
// after the window. At the tiers' own timing the window leaves room for one
// repeat after a full observation timeout and no more, and that refusal comes
// within three seconds. It follows the tier this process runs its database
// node with, read where the window is used.
var clusterMembershipRetryWindow = func() time.Duration {
	return GrantRetryWindowFor(cluster.ProcessStorageTier())
}

// GrantRetryWindowFor is the retry window of a cluster that runs this storage
// tier. A tier run at a longer latency elects later, so the window grows to
// the tier's need (two election timeouts and one commit), and a member cut
// off from the majority refuses that much later. The window never reaches the
// grant's own timeout: from a latency of about 140 ms on, a slow election can
// outlast it, and a grant that began just before the leader was lost is then
// refused although a leader follows.
func GrantRetryWindowFor(tier storagetier.Tier) time.Duration {
	return min(max(baseGrantRetryWindow, tier.GrantRetryNeed()), ClusterMembershipGrantTimeout-grantRetryHeadroom)
}
