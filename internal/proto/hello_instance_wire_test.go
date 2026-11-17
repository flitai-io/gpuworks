package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

// instance_id names the agent process that sent a hello; older agents send
// none, and an empty one is omitted from the wire.
func TestHelloInstanceIDWireJSON(t *testing.T) {
	raw, err := json.Marshal(Hello{ProtoMin: 1, ProtoMax: 2, Version: "v1", InstanceID: "0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"proto_min":1,"proto_max":2,"version":"v1","instance_id":"0123456789abcdef0123456789abcdef"}`
	if string(raw) != want {
		t.Fatalf("wire = %s, want %s", raw, want)
	}
	raw, _ = json.Marshal(Hello{ProtoMin: 1, ProtoMax: 2, Version: "v1"})
	if strings.Contains(string(raw), "instance_id") {
		t.Fatalf("empty instance_id was not omitted: %s", raw)
	}
	var got Hello
	if err := json.Unmarshal([]byte(want), &got); err != nil || got.InstanceID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
}
