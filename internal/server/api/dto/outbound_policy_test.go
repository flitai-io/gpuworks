package dto

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestServerOutboundPolicyWireContract(t *testing.T) {
	const wire = `{"restrict_private_hosts":false,"allow_private_hosts":[],"editable":true,"revision":"opaque"}`
	original := ServerOutboundPolicy{AllowPrivateHosts: []string{}, Editable: true, Revision: "opaque"}
	raw, err := json.Marshal(original)
	if err != nil || string(raw) != wire {
		t.Fatalf("wire %s err %v", raw, err)
	}
	var decoded ServerOutboundPolicy
	if err = json.Unmarshal([]byte(wire), &decoded); err != nil || !reflect.DeepEqual(original, decoded) {
		t.Fatalf("round trip %+v err %v", decoded, err)
	}
	var update ServerOutboundPolicyUpdate
	if err = json.Unmarshal([]byte(`{"restrict_private_hosts":false,"allow_private_hosts":[],"revision":"opaque"}`), &update); err != nil || update.RestrictPrivateHosts == nil || *update.RestrictPrivateHosts || update.AllowPrivateHosts == nil || update.Revision != "opaque" {
		t.Fatalf("update %+v %v", update, err)
	}
	if err = json.Unmarshal([]byte(`{}`), &update); err != nil {
		t.Fatal(err)
	}
}

func TestServerOutboundHostCheckWireContract(t *testing.T) {
	const wire = `[{"host":"models.corp","status":"needs_allow"},{"host":"169.254.169.254","status":"blocked"}]`
	original := []ServerOutboundHostCheck{{Host: "models.corp", Status: "needs_allow"}, {Host: "169.254.169.254", Status: "blocked"}}
	raw, err := json.Marshal(original)
	if err != nil || string(raw) != wire {
		t.Fatalf("wire=%s err=%v", raw, err)
	}
	var decoded []ServerOutboundHostCheck
	if err := json.Unmarshal([]byte(wire), &decoded); err != nil || !reflect.DeepEqual(original, decoded) {
		t.Fatalf("round trip=%+v err=%v", decoded, err)
	}
	var request ServerOutboundPolicyCheck
	if err := json.Unmarshal([]byte(`{"hosts":["models.corp","::1"]}`), &request); err != nil || !reflect.DeepEqual(request.Hosts, []string{"models.corp", "::1"}) {
		t.Fatalf("request=%+v err=%v", request, err)
	}
}
