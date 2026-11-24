package hub

import (
	"context"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// The target reports "present" as soon as the last byte reaches its load, and
// that report deactivates the row, so a final progress write that runs after
// the copy returns can lose the race and leave the first throttled sample as
// the recorded transfer size. The write that reaches the declared size must
// therefore be durable before the copy callback returns to the stream.
func TestRelayedTransferRecordsTheFullSizeBeforeTheLastChunkIsHandedOver(t *testing.T) {
	h, image, _, target := imageStallFixture(t, "relay_final_progress")
	const size = int64(534016)
	if _, err := h.store.UpsertImageNode(&store.ImageNode{ImageID: image.ID, NodeID: target.ID, Source: "url-relay", State: store.ImageStateSyncing, TransferID: "relay-1", SizeBytes: size}); err != nil {
		t.Fatal(err)
	}
	type observed struct {
		bytes int64
		err   error
	}
	seen := make(chan observed, 1)
	h.trackImageTransfer(context.Background(), image.ID, target.ID, "relay-1", size, func(_ context.Context, progress func(int64)) (int64, error) {
		progress(3935) // the first sample opens the one-second throttle window
		progress(size) // the last chunk arrives inside that same window
		row, err := h.store.ImageNode(image.ID, target.ID)
		if err != nil {
			seen <- observed{err: err}
		} else {
			seen <- observed{bytes: row.TransferredBytes}
		}
		return size, nil
	})
	select {
	case got := <-seen:
		if got.err != nil || got.bytes != size {
			t.Fatalf("transferred_bytes = %d err=%v when the last chunk was handed over, want %d", got.bytes, got.err, size)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the relayed copy never ran")
	}
}

func TestImageSyncProgressLimiterKeepsTimedAndTerminalUpdates(t *testing.T) {
	var limiter imageSyncProgressLimiter
	if !limiter.allow(0, false) {
		t.Fatal("first progress was suppressed")
	}
	if limiter.allow(100*time.Millisecond, false) {
		t.Fatal("small burst progress was not suppressed")
	}
	if limiter.allow(200*time.Millisecond, false) {
		t.Fatal("large byte delta bypassed the time throttle")
	}
	if !limiter.allow(1100*time.Millisecond, false) {
		t.Fatal("one-second progress threshold was suppressed")
	}
	if !limiter.allow(1250*time.Millisecond, true) {
		t.Fatal("forced terminal progress was suppressed")
	}
}
