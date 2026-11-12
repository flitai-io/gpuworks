package proto

import (
	"encoding/json"
	"testing"
)

func TestServerDialProofPreservesLaneAndSession(t *testing.T) {
	var proof ServerDialProof
	if err := json.Unmarshal([]byte(`{"hmac":"proof","lane":"data","session_id":"current"}`), &proof); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["lane"] != "data" || got["session_id"] != "current" {
		t.Fatalf("data proof lost its parent session: %s", raw)
	}
}

func TestServerDialLaneHMACBindsPurposeAndGeneration(t *testing.T) {
	proof := ServerDialProof{Lane: LaneData, SessionID: "parent", HMAC: ServerDialLaneHMAC("node-secret", "nonce", LaneData, "parent")}
	if !VerifyServerDialLaneHMAC("node-secret", "nonce", proof) {
		t.Fatal("valid data proof rejected")
	}
	cases := []struct{ name, secret, nonce, lane, parent string }{
		{"token", "other", "nonce", LaneData, "parent"},
		{"replay", "node-secret", "fresh", LaneData, "parent"},
		{"lane", "node-secret", "nonce", LaneCtrl, "parent"},
		{"generation", "node-secret", "nonce", LaneData, "next-parent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			altered := proof
			altered.Lane = tc.lane
			altered.SessionID = tc.parent
			if VerifyServerDialLaneHMAC(tc.secret, tc.nonce, altered) {
				t.Fatal("altered proof accepted")
			}
		})
	}
	if VerifyServerDialHMAC("node-secret", "nonce", proof.HMAC) {
		t.Fatal("lane proof accepted as legacy control proof")
	}
}
