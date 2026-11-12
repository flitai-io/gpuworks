package proto

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestPeerRelayWireAndLaneContracts(t *testing.T) {
	for _, kind := range []string{StreamPeerRelaySend, StreamPeerRelayReceive} {
		if LaneFor(kind) != LaneData {
			t.Fatalf("%s shares ctrl", kind)
		}
	}
	cases := []struct {
		wire  string
		value any
	}{
		{`{"allow_relay":true,"transfer_id":"t","source_path":"p","token":"x","endpoints":["h:1"],"fingerprint":"f"}`, &ModelSyncSendParams{}},
		{`{"allow_relay":true,"transfer_id":"t","image_ref":"image","token":"x","endpoints":["h:1"],"fingerprint":"f"}`, &ImageSyncSendParams{}},
		{`{"direct_dial_error":"unreachable","bytes":0,"sha256":""}`, &ModelSyncTransferResult{}},
		{`{"direct_dial_error":"unreachable","bytes":0,"sha256":""}`, &ImageSyncTransferResult{}},
		{`{"transport":"relay","transfer_id":"t","bytes":1,"sha256":"s"}`, &ModelSyncCopyResult{}},
		{`{"transport":"direct","transfer_id":"t"}`, &ImageSyncCopyResult{}},
		{`{}`, &PeerRelayReady{}},
		{`{"error":"stopped"}`, &PeerRelayReady{}},
	}
	for _, tc := range cases {
		if err := json.Unmarshal([]byte(tc.wire), tc.value); err != nil {
			t.Fatal(err)
		}
		out, err := json.Marshal(tc.value)
		if err != nil {
			t.Fatal(err)
		}
		var before, after any
		json.Unmarshal([]byte(tc.wire), &before)
		json.Unmarshal(out, &after)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("roundtrip %T: %s != %s", tc.value, out, tc.wire)
		}
	}
	for _, value := range []any{ModelSyncSendParams{}, ImageSyncSendParams{}, ModelSyncTransferResult{}, ImageSyncTransferResult{}} {
		out, _ := json.Marshal(value)
		var wire map[string]any
		json.Unmarshal(out, &wire)
		if _, ok := wire["allow_relay"]; ok {
			t.Fatalf("legacy request changed: %s", out)
		}
		if _, ok := wire["direct_dial_error"]; ok {
			t.Fatalf("legacy result changed: %s", out)
		}
	}
}
