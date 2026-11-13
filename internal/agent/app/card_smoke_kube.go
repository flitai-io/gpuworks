package app

import (
	"context"
	"sync"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/gpu/cardsmoke"
	"gpuworks.ai/gpuworks/internal/proto"
)

// cardHolder is the runtime that decides which cards the platform may ask
// (the kube runtime): a card the kubelet may give to another pod is not one to
// put a context on, and a card the platform holds must stay held while it is
// asked.
type cardHolder interface {
	HoldCardForCheck(ctx context.Context, card string) (release func(), why string)
}

// holdCardsWhileAsking makes a host runtime check ask a card only while the
// platform holds it. A card it does not hold is not asked at all: the attempt
// ends in cardsmoke.NotHeld, which the periodic check records as skipped
// (cardsmoke.SkipNotHeld) and Doctor lists as not checked, each with the
// reason. The card is kept from being returned to Kubernetes until the answer
// is in.
func (n *NodeRuntime) holdCardsWhileAsking(check gpu.HostRuntimeCheck) gpu.HostRuntimeCheck {
	var mu sync.Mutex
	cards := map[string]string{} // how the check names a card -> its UUID
	held := check
	held.Visible = func(g proto.GPUInfo) (string, bool) {
		name, named := check.Visible(g)
		if named {
			mu.Lock()
			cards[name] = g.UUID
			mu.Unlock()
		}
		return name, named
	}
	held.Try = func(ctx context.Context, visible string) error {
		holder, ok := n.runtimeDriver().(cardHolder)
		if !ok {
			return cardsmoke.NotHeld("this node's runtime cannot say which cards the platform holds")
		}
		mu.Lock()
		uuid := cards[visible]
		mu.Unlock()
		release, why := holder.HoldCardForCheck(ctx, uuid)
		if why != "" {
			return cardsmoke.NotHeld(why)
		}
		defer release()
		return check.Try(ctx, visible)
	}
	return held
}
