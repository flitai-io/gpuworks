package dto

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCommunityRepoAccessHostWireContract(t *testing.T) {
	const wire = `{"repo":"registry.corp/team/image","registry":"oci","access_host":"tags.corp","engine":"vllm"}`
	original := CommunityRepo{Repo: "registry.corp/team/image", Registry: "oci", AccessHost: "tags.corp", Engine: "vllm"}
	raw, err := json.Marshal(original)
	if err != nil || string(raw) != wire {
		t.Fatalf("wire=%s err=%v", raw, err)
	}
	var decoded CommunityRepo
	if err := json.Unmarshal([]byte(wire), &decoded); err != nil || !reflect.DeepEqual(original, decoded) {
		t.Fatalf("round trip=%+v err=%v", decoded, err)
	}
	const legacy = `{"repo":"registry.corp/team/image","registry":"oci","engine":"vllm"}`
	var legacyDecoded CommunityRepo
	if err := json.Unmarshal([]byte(legacy), &legacyDecoded); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(legacyDecoded)
	if err != nil || string(raw) != legacy {
		t.Fatalf("unknown host wire=%s err=%v", raw, err)
	}
}
