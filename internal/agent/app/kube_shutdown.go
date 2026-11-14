package app

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube"
)

// kubeShutdownTimeout bounds releaseUnselectedKubeCards: the pod's own
// terminationGracePeriodSeconds (the manifest renders 30) is the hard
// deadline Kubernetes enforces with SIGKILL, so this leaves it margin to
// actually return before that.
var kubeShutdownTimeout = 20 * time.Second

// releaseUnselectedKubeCards runs once, as Run leaves a Kubernetes node
// (SIGTERM, or Run returning for any other reason): kd reads its own
// DaemonSet, and when it no longer selects this node, returns every card
// the node holds — nothing else is left to do it — before the agent exits.
// An ordinary restart, rolling update or in-place upgrade (the DaemonSet
// still selects the node) leaves everything as it is. kd logs its own
// outcome through the *slog.Logger it was built with.
func releaseUnselectedKubeCards(kd *kube.Driver) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeShutdownTimeout)
	defer cancel()
	kd.ReleaseCardsIfNodeUnselected(ctx)
}
