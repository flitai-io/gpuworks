package hub

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestPeerRelayServerOwnsUserRateInsteadOfSourceThrottle(t *testing.T) {
	for _, rate := range []int{1, 1024, 128 * 1024} {
		model := limitPeerRelay(proto.ModelSyncSendParams{RateLimitKBs: rate, AllowRelay: true}).(proto.ModelSyncSendParams)
		image := limitPeerRelay(proto.ImageSyncSendParams{RateLimitKBs: rate, AllowRelay: true}).(proto.ImageSyncSendParams)
		if model.RateLimitKBs != 0 || image.RateLimitKBs != 0 {
			t.Fatalf("source duplicates server cap: model=%d image=%d", model.RateLimitKBs, image.RateLimitKBs)
		}
	}
}
