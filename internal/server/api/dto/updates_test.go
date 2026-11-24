package dto

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestPartialImageCatalogUpdateWire(t *testing.T) {
	for _, tc := range []struct {
		body    string
		skipped []string
	}{
		{`{"current":"v1","update_available":false,"compatible":true}`, nil},
		{`{"skipped":["registry/runner:r1"],"current":"v1","latest":"v1","update_available":true,"compatible":true}`, []string{"registry/runner:r1"}},
	} {
		var got CatalogUpdate
		if err := json.Unmarshal([]byte(tc.body), &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Skipped, tc.skipped) {
			t.Fatalf("skipped = %q", got.Skipped)
		}
		data, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var want, actual map[string]any
		if err := json.Unmarshal([]byte(tc.body), &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &actual); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(want, actual) {
			t.Fatalf("round trip = %s, want %s", data, tc.body)
		}
	}
}
