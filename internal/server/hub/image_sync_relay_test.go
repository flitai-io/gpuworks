package hub

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A copy started without the relay never asks for it, whatever the two nodes
// allow; a copy that may be relayed still needs both nodes to let the
// management network carry storage sync.
func TestImageCopyRequestAsksForTheRelayOnlyWhenTheCopyMayUseIt(t *testing.T) {
	image := &store.Image{ImageRef: "fixture/image:relay"}
	open := &store.Node{UUID: "open"}
	closed := &store.Node{UUID: "closed", ManagementRolesOff: `["` + netplan.RoleStorageSync + `"]`}
	path := netplan.StoragePath{Network: netplan.ManagementName, SourceIP: "10.0.0.1", TargetIP: "10.0.0.2"}
	for _, tc := range []struct {
		name           string
		source, target *store.Node
		relay, want    bool
	}{
		{"direct only", open, open, false, false},
		{"relay allowed by both nodes", open, open, true, true},
		{"relay refused by the target", open, closed, true, false},
		{"relay refused by the source", closed, open, true, false},
	} {
		req := imageCopyRequest(image, tc.source, tc.target, path, "transfer", 1024, 512, tc.relay)
		if req.AllowRelay != tc.want {
			t.Errorf("%s: AllowRelay = %v, want %v", tc.name, req.AllowRelay, tc.want)
		}
		if req.SourceIP != "10.0.0.1" || req.TargetIP != "10.0.0.2" || req.RateLimitKBs != 512 || req.ExpectedBytes != 1024 || req.ImageRef != image.ImageRef {
			t.Errorf("%s: request = %+v", tc.name, req)
		}
	}
}

// relayWiringFixture is two connected nodes whose source answers every send
// with a failed dial, as a source behind another NAT does. sent receives the
// parameters of each send.
func relayWiringFixture(t *testing.T, name string) (*Hub, *store.Image, *store.Node, *store.Node, chan proto.ImageSyncSendParams) {
	t.Helper()
	h, image, source, target := imageStallFixture(t, name)
	h.SetNetworks(imageEpochNetworks{})
	if _, err := h.store.UpsertImageNode(&store.ImageNode{ImageID: image.ID, NodeID: source.ID, State: store.ImageStatePresent, SizeBytes: 42}); err != nil {
		t.Fatal(err)
	}
	sent := make(chan proto.ImageSyncSendParams, 4)
	for _, node := range []*store.Node{source, target} {
		s := newSession(h, nil, nil)
		s.NodeUUID = node.UUID
		// Both agents could relay: only the copy's own choice keeps it direct.
		s.caps = []string{proto.CapPeerRelayV1}
		data, _ := yamuxPairData(t)
		s.attachData(data)
		s.sendFn = func(kind string, id uint64, value any) error {
			request := value.(rpcRequest)
			response := rpcResponse{Result: json.RawMessage(`{}`)}
			switch request.Method {
			case proto.RPCImageSyncListen:
				response.Result = json.RawMessage(`{"endpoints":["192.0.2.2:12001"],"fingerprint":"fixture-fingerprint"}`)
			case proto.RPCImageSyncSend:
				sent <- request.Params.(proto.ImageSyncSendParams)
				response.Result = json.RawMessage(`{"direct_dial_error":"dial tcp 192.0.2.2:12001: i/o timeout"}`)
			}
			body, _ := json.Marshal(response)
			s.dispatchRPCResp(proto.Envelope{T: proto.TRPCResp, ID: id, D: body})
			return nil
		}
		h.sessions[node.UUID] = s
	}
	if h.archiveRelayFor(source.UUID, target.UUID, "image") == nil {
		t.Fatal("the fixture's agents cannot relay")
	}
	return h, image, source, target, sent
}

// The copy a launch starts tells the source it may not fall back to the relay,
// and a failed dial ends it there with the code the launch reads: the row is
// failed, nothing was relayed.
func TestSyncImageDirectFromFailsOnAFailedDialInsteadOfRelaying(t *testing.T) {
	h, image, source, target, sent := relayWiringFixture(t, "direct_only_dial_failure")
	if _, err := h.SyncImageDirectFrom(context.Background(), image.ID, target.UUID, source.UUID, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case params := <-sent:
		if params.AllowRelay {
			t.Fatal("a launch's copy told the source it may be relayed")
		}
	case <-time.After(time.Second):
		t.Fatal("the copy did not reach the source")
	}
	waitFor(t, time.Second, func() bool {
		row, err := h.store.ImageNode(image.ID, target.ID)
		return err == nil && row.State == store.ImageStateFailed
	}, "failed copy")
	row, err := h.store.ImageNode(image.ID, target.ID)
	if err != nil || row.TransferErrorCode != messages.ErrorPeerDirectConnectFailed || !strings.Contains(row.Message, "i/o timeout") {
		t.Fatalf("row = %+v err=%v, want the direct connection failure and its dial error", row, err)
	}
	select {
	case params := <-sent:
		t.Fatalf("the copy was sent again: %+v", params)
	default:
	}
}

// The copy a person starts keeps the relay: the source is told it may use it.
func TestSyncImageFromRateStillOffersTheRelay(t *testing.T) {
	h, image, source, target, sent := relayWiringFixture(t, "relay_capable_copy")
	if _, err := h.SyncImageFromRate(context.Background(), image.ID, target.UUID, source.UUID, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case params := <-sent:
		if !params.AllowRelay {
			t.Fatal("a manual copy lost the relay")
		}
	case <-time.After(time.Second):
		t.Fatal("the copy did not reach the source")
	}
}

// The copy a launch starts is recorded like any other peer copy: the same
// ledger row, the same source, so the launch can wait on it and cool the pair
// when it fails.
func TestSyncImageDirectFromRecordsThePeerCopy(t *testing.T) {
	h, image, source, target := imageStallFixture(t, "direct_only_row")
	if _, err := h.store.UpsertImageNode(&store.ImageNode{ImageID: image.ID, NodeID: source.ID, State: store.ImageStatePresent, SizeBytes: 2048}); err != nil {
		t.Fatal(err)
	}
	h.sessions[target.UUID] = &Session{}
	h.sessions[source.UUID] = &Session{}
	row, err := h.SyncImageDirectFrom(context.Background(), image.ID, target.UUID, source.UUID, 256)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.ImageStateSyncing || row.Source != "peer" || row.SourceNodeID != source.ID || row.RateLimitKiB != 256 {
		t.Fatalf("row = %+v, want a syncing peer copy from the source at the given cap", row)
	}
	if _, err := h.SyncImageDirectFrom(context.Background(), image.ID, target.UUID, "", 0); err == nil {
		t.Fatal("a copy with no source was started")
	}
}
