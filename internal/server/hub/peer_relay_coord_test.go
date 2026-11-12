package hub

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestModelCopyRelaysOnlyNegotiatedDirectDialFailure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		allowed     bool
		directError error
		badAck      bool
		wantRelay   bool
		wantOK      bool
	}{
		{name: "relay", allowed: true, wantRelay: true, wantOK: true},
		{name: "forbidden"},
		{name: "tls", allowed: true, directError: errors.New("certificate fingerprint mismatch")},
		{name: "relay-bad-ack", allowed: true, badAck: true, wantRelay: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			stops := 0
			call := func(_ context.Context, _ string, method string, _ any, _ bool) (json.RawMessage, error) {
				switch method {
				case proto.RPCModelSyncListen:
					return json.Marshal(proto.ModelSyncListenResult{Endpoints: []string{"192.0.2.1:1234"}, Fingerprint: strings.Repeat("1", 64)})
				case proto.RPCModelSyncSend:
					if tc.directError != nil {
						return nil, tc.directError
					}
					return json.Marshal(proto.ModelSyncTransferResult{DirectDialError: "dial timeout"})
				case proto.RPCModelSyncStop:
					stops++
				}
				return nil, nil
			}
			relay := func(_ context.Context, method, id string, params any) (json.RawMessage, error) {
				called = true
				if method != proto.RPCModelSyncSend || id != "transfer" {
					t.Fatalf("relay method/id: %s %s", method, id)
				}
				p := params.(proto.ModelSyncSendParams)
				if p.Token != "one-time" || p.SourcePath != "model" {
					t.Fatalf("lost authorized params: %+v", p)
				}
				if tc.badAck {
					return json.RawMessage(`{"bytes":0,"sha256":""}`), nil
				}
				return json.Marshal(proto.ModelSyncTransferResult{Bytes: 9, SHA256: strings.Repeat("a", 64)})
			}
			result, err := coordinateModelCopy(context.Background(), proto.ModelSyncCopyRequest{TransferID: "transfer", Key: "m", SourceNodeUUID: "source", TargetNodeUUID: "target", SourcePath: "model", TargetPath: "model", AllowRelay: tc.allowed}, "one-time", call, relay)
			if called != tc.wantRelay || (err == nil) != tc.wantOK {
				t.Fatalf("relay=%v result=%+v err=%v", called, result, err)
			}
			if tc.wantOK && result.Transport != "relay" {
				t.Fatalf("transport=%q", result.Transport)
			}
			if !tc.wantOK && stops != 1 {
				t.Fatalf("failed receiver not released: %d", stops)
			}
		})
	}
}

func TestImageCopyAndProbeCanUseAuthorizedRelay(t *testing.T) {
	for _, probe := range []bool{false, true} {
		t.Run(map[bool]string{false: "copy", true: "probe"}[probe], func(t *testing.T) {
			called := false
			stops := 0
			call := func(_ context.Context, _ string, method string, _ any, _ bool) (json.RawMessage, error) {
				switch method {
				case proto.RPCImageSyncListen:
					return json.Marshal(proto.ImageSyncListenResult{Endpoints: []string{"192.0.2.1:1234"}, Fingerprint: strings.Repeat("1", 64)})
				case proto.RPCImageSyncSend, proto.RPCImageSyncProbe:
					return json.Marshal(proto.ImageSyncTransferResult{DirectDialError: "unreachable"})
				case proto.RPCImageSyncStop:
					stops++
				}
				return nil, nil
			}
			relay := func(_ context.Context, method, id string, _ any) (json.RawMessage, error) {
				called = true
				want := proto.RPCImageSyncSend
				if probe {
					want = proto.RPCImageSyncProbe
				}
				if method != want || id != "transfer" {
					t.Fatalf("relay method/id=%s/%s", method, id)
				}
				return json.Marshal(proto.ImageSyncTransferResult{Bytes: 9, SHA256: strings.Repeat("a", 64)})
			}
			result, err := coordinateImageCopy(context.Background(), proto.ImageSyncCopyRequest{TransferID: "transfer", ImageRef: "fixture", SourceNodeUUID: "source", TargetNodeUUID: "target", AllowRelay: true}, "one-time", call, probe, relay)
			if err != nil || !called || result.Transport != "relay" {
				t.Fatalf("called=%v result=%+v err=%v", called, result, err)
			}
			if probe && stops != 1 {
				t.Fatalf("probe left receiver: %d", stops)
			}
		})
	}
}
