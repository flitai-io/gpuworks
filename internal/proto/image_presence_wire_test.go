package proto

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNodeStatusImagePresenceWireIdentityCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, image, canonical string
	}{
		{
			name:      "runtime identity and measured size",
			image:     `{"image_ref":"registry.example/team/runner:v1","present":true,"image_id":"sha256:1111111111111111111111111111111111111111111111111111111111111111","runtime_size_bytes":8192}`,
			canonical: `{"image_ref":"registry.example/team/runner:v1","present":true,"image_id":"sha256:1111111111111111111111111111111111111111111111111111111111111111","runtime_size_bytes":8192}`,
		},
		{
			name:      "legacy agent without identity",
			image:     `{"image_ref":"registry.example/team/runner:v1","present":true,"size_bytes":4096}`,
			canonical: `{"image_ref":"registry.example/team/runner:v1","present":true,"size_bytes":4096}`,
		},
		{
			name:      "unobserved identity is omitted",
			image:     `{"image_ref":"registry.example/team/runner:v1","present":true,"image_id":""}`,
			canonical: `{"image_ref":"registry.example/team/runner:v1","present":true}`,
		},
		{
			name:      "explicit absence",
			image:     `{"image_ref":"registry.example/team/runner:v1","present":false}`,
			canonical: `{"image_ref":"registry.example/team/runner:v1","present":false}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var status NodeStatus
			if err := json.Unmarshal([]byte(`{"images":[`+tc.image+`]}`), &status); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err := json.Unmarshal(envelope["images"], &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(`[`+tc.canonical+`]`), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("status.node image report changed on the wire: got %s, want [%s]", envelope["images"], tc.canonical)
			}
		})
	}
}
