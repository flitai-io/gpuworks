package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestCancelPeerImageTransferStopsDirectReceiver(t *testing.T) {
	st, err := store.Open("file:hub_cancel_peer_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	image, err := st.UpsertImage(&store.Image{ImageRef: "quay.io/team/runner:peer-cancel", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("peer-cancel", store.HashToken("peer-cancel"), "peer-cancel")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImageNode(&store.ImageNode{
		ImageVersionID: image.ID, ImageID: image.ID, NodeID: node.ID,
		State: store.ImageStateSyncing, Source: "peer", TransferID: "peer-transfer",
	}); err != nil {
		t.Fatal(err)
	}

	h := New(st, slog.Default())
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	session := newSession(h, nil, server)
	session.NodeUUID = node.UUID
	h.register(session)
	t.Cleanup(func() { h.deregister(session) })
	go func() {
		decoder := json.NewDecoder(server)
		for {
			var env proto.Envelope
			if decoder.Decode(&env) != nil {
				return
			}
			session.handle(env)
		}
	}()
	request := make(chan struct {
		method string
		params proto.ImageSyncStopParams
	}, 1)
	go func() {
		var env proto.Envelope
		if err := json.NewDecoder(client).Decode(&env); err != nil {
			return
		}
		var req struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(env.D, &req) != nil {
			return
		}
		var params proto.ImageSyncStopParams
		if json.Unmarshal(req.Params, &params) != nil {
			return
		}
		request <- struct {
			method string
			params proto.ImageSyncStopParams
		}{req.Method, params}
		response := proto.Envelope{T: proto.TRPCResp, ID: env.ID, D: json.RawMessage(`{"result":{}}`)}
		line, _ := json.Marshal(response)
		_, _ = client.Write(append(line, '\n'))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	row, err := h.CancelImageTransfer(ctx, image.ID, node.UUID)
	if ctx.Err() != nil {
		t.Fatal("cancellation timed out instead of receiving an RPC response")
	}
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.ImageStateFailed || row.TransferErrorCode != "PEER_TRANSFER_CANCELED" {
		t.Fatalf("after cancel = state %q code %q, want failed and marked cancelled", row.State, row.TransferErrorCode)
	}
	// The stopped receiver's docker load dies of the signal and says so,
	// after the row was cancelled; the operator's reason stays.
	if err := st.ApplyImageNodeStatus(image.ID, node.ID, proto.ImageNodeStatus{ImageRef: image.ImageRef, State: store.ImageStateFailed, TransferID: "peer-transfer", Message: "load image archive: docker load: signal: killed"}); err != nil {
		t.Fatal(err)
	}
	if row, err = st.ImageNode(image.ID, node.ID); err != nil || row.Message != "the transfer was cancelled" {
		t.Fatalf("after the receiver's late report: %+v %v", row, err)
	}
	select {
	case got := <-request:
		if got.method != proto.RPCImageSyncStop || got.params.TransferID != "peer-transfer" {
			t.Fatalf("cancel RPC = %q %+v, want image.sync.stop with transfer id", got.method, got.params)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("peer cancellation did not send image.sync.stop")
	}
}

// Cancelling has to end with a row the operator can retry, even when the node
// cannot be reached to stop the work itself: the agent may have died, and a row
// that stays "in progress" would keep refusing the next attempt.
func TestCancelImageTransferFailsTheRowWithoutANode(t *testing.T) {
	st, err := store.Open("file:hub_cancel_offline_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	image, err := st.UpsertImage(&store.Image{ImageRef: "quay.io/team/runner:cancel", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := st.CreateNodePending("gpu3-cancel")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImageNode(&store.ImageNode{
		ImageVersionID: image.ID, ImageID: image.ID, NodeID: node.ID,
		State: store.ImageStateSyncing, Source: "url", TransferID: "t-1",
		SizeBytes: 5 << 30, Message: "downloading image archive",
	}); err != nil {
		t.Fatal(err)
	}

	h := New(st, slog.Default())
	row, err := h.CancelImageTransfer(context.Background(), image.ID, node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.ImageStateFailed {
		t.Fatalf("state after cancel = %q, want failed", row.State)
	}
	if !strings.Contains(row.Message, "cancelled") {
		t.Fatalf("message after cancel = %q", row.Message)
	}
	if row.TransferErrorCode != "" {
		t.Fatalf("URL cancel code = %q; the peer cancel code would show peer-transfer text for a download", row.TransferErrorCode)
	}
	if row.TransferID != "" {
		t.Fatalf("a cancelled row keeps its transfer id: %q", row.TransferID)
	}
}

// Cancelling something that is not running is not an error and must not rewrite
// a finished row.
func TestCancelImageTransferLeavesAFinishedRowAlone(t *testing.T) {
	st, err := store.Open("file:hub_cancel_finished_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	image, err := st.UpsertImage(&store.Image{ImageRef: "quay.io/team/runner:done", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := st.CreateNodePending("gpu3-done")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImageNode(&store.ImageNode{
		ImageVersionID: image.ID, ImageID: image.ID, NodeID: node.ID,
		State: store.ImageStatePresent, Source: "url", SizeBytes: 42, Message: "loaded",
	}); err != nil {
		t.Fatal(err)
	}

	h := New(st, slog.Default())
	row, err := h.CancelImageTransfer(context.Background(), image.ID, node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.ImageStatePresent || row.Message != "loaded" {
		t.Fatalf("a finished row was rewritten by a cancel: %+v", row)
	}
}
