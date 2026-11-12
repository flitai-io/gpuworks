package hub

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/prepobs"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestPeerTransferObservationUsesExistingSinksWithoutLockOrIdentityLabels(t *testing.T) {
	h := newTestHub(t)
	var logs []proto.LogLine
	var samples []proto.MetricSample
	h.SetSinks(func(_ string, batch []proto.MetricSample) {
		h.PeerTransferSnapshot()
		samples = append(samples, batch...)
	}, func(_ string, batch []proto.LogLine) { h.PeerTransferSnapshot(); logs = append(logs, batch...) })
	_, finish := h.beginObservedPeerTransfer(context.Background(), prepobs.PeerTransferSpec{TransferID: "needle", Kind: "model", Operation: "copy", SourceNodeUUID: "src", TargetNodeUUID: "dst"})
	h.updatePeerTransfer("needle", "relay", "forwarding", "", "direct_dial_failed")
	finish(errors.New("no verified ACK"), false)
	_, probeFinish := h.beginObservedPeerTransfer(context.Background(), prepobs.PeerTransferSpec{TransferID: "probe", Kind: "image", Operation: "probe"})
	probeFinish(nil, false)
	h.samplePeerTransferMetrics()
	view := h.PeerTransferSnapshot()
	if view.Totals.PeerConfirmed != 0 || view.Totals.Failed != 1 || view.Totals.ProbeOK != 1 {
		t.Fatalf("incorrect completion accounting: %+v", view.Totals)
	}
	if len(logs) != 4 || len(samples) != 15 {
		t.Fatalf("logs=%d metrics=%d", len(logs), len(samples))
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(logs[1].Line), &event); err != nil {
		t.Fatal(err)
	}
	if logs[1].Source != "server" || logs[1].Level != "info" || event["transfer_id"] != "needle" || event["event"] != "failed" || event["transport"] != "relay" {
		t.Fatalf("event=%+v log=%+v", event, logs[1])
	}
	for _, s := range samples {
		if s.Labels["node"] != "server" {
			t.Fatalf("node label missing: %+v", s)
		}
		for k := range s.Labels {
			if k != "node" && k != "direction" && k != "outcome" {
				t.Fatalf("unbounded label: %+v", s)
			}
		}
	}
}

func TestPeerRelayBusyImageCleanupCannotDestroyWaitingOwnership(t *testing.T) {
	h := newTestHub(t)
	image, err := h.store.UpsertImage(&store.Image{ImageRef: "example/wait:v1", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := h.store.CreateNodePending("target-wait")
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.store.UpsertImageNode(&store.ImageNode{ImageID: image.ID, NodeID: target.ID, State: store.ImageStateSyncing, TransferID: "old", Source: "peer", SourceNodeID: 999, RateLimitKiB: 0})
	if err != nil {
		t.Fatal(err)
	}
	onBusy := func() {
		if changed, e := h.store.MarkImageSyncWaiting(image.ID, target.ID, "old", "waiting", messages.ErrorPeerRelayBusy); e != nil || !changed {
			t.Fatalf("mark=%v %v", changed, e)
		}
	}
	relay := h.observeArchiveRelay("old", func(context.Context, string, string, any) (json.RawMessage, error) {
		return nil, messages.PeerRelayBusy()
	}, onBusy)
	call := func(_ context.Context, _ string, method string, _ any, _ bool) (json.RawMessage, error) {
		switch method {
		case proto.RPCImageSyncListen:
			return json.RawMessage(`{"endpoints":["127.0.0.1:1"],"fingerprint":"pin"}`), nil
		case proto.RPCImageSyncSend:
			return json.RawMessage(`{"direct_dial_error":"refused"}`), nil
		case proto.RPCImageSyncStop:
			return nil, h.store.ApplyImageNodeStatus(image.ID, target.ID, proto.ImageNodeStatus{TransferID: "old", State: store.ImageStateFailed, Message: "stopped"})
		}
		t.Fatalf("unexpected method %s", method)
		return nil, nil
	}
	_, err = coordinateImageCopy(context.Background(), proto.ImageSyncCopyRequest{TransferID: "old", SourceNodeUUID: "source", TargetNodeUUID: "target", ImageRef: image.ImageRef, AllowRelay: true}, "once", call, false, relay)
	if !errors.Is(err, messages.ErrPeerRelayBusy) {
		t.Fatalf("typed capacity lost: %v", err)
	}
	row, _ := h.store.ImageNode(image.ID, target.ID)
	if row.State != store.ImageStateSyncing || row.TransferID != "old" || row.TransferErrorCode != messages.ErrorPeerRelayBusy {
		t.Fatalf("cleanup overwrote wait: %+v", row)
	}
	changed, err := h.store.RestartImageSyncIfActive(image.ID, target.ID, "old", "new", 999, 10, 0)
	if err != nil || !changed {
		t.Fatalf("retry=%v %v", changed, err)
	}
	_ = h.store.ApplyImageNodeStatus(image.ID, target.ID, proto.ImageNodeStatus{TransferID: "old", State: store.ImageStateFailed})
	row, _ = h.store.ImageNode(image.ID, target.ID)
	if row.State != store.ImageStateSyncing || row.TransferID != "new" || row.TransferErrorCode != "" || row.RateLimitKiB != 0 {
		t.Fatalf("stale cleanup changed new task: %+v", row)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.imageSyncOwners = map[string]context.CancelFunc{"new": cancel}
	row, err = h.cancelImageTransferRow(context.Background(), image, target, row, "operator stop")
	if err != nil || ctx.Err() == nil || row.State != store.ImageStateFailed {
		t.Fatalf("owner cancel=%+v %v %v", row, err, ctx.Err())
	}
}
