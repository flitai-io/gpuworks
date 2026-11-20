package deploy

import (
	"encoding/json"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestValidatePlacementNodes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		placement json.RawMessage
		nodes     int
		rejected  bool
	}{
		{name: "explicit zero", placement: json.RawMessage(`{"max_nodes":0}`), nodes: 5},
		{name: "omitted limit", placement: json.RawMessage(`{"language":"starlark"}`), nodes: 5},
		{name: "at limit", placement: json.RawMessage(`{"max_nodes":1}`), nodes: 1},
		{name: "above limit", placement: json.RawMessage(`{"max_nodes":1}`), nodes: 2, rejected: true},
		{name: "no placement", nodes: 5},
		{name: "malformed placement", placement: json.RawMessage(`{"max_nodes":1,`), nodes: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (CatalogSelection{Placement: tc.placement}).ValidatePlacementNodes(tc.nodes)
			if !tc.rejected {
				if err != nil {
					t.Fatalf("ValidatePlacementNodes(%d) = %v, want nil", tc.nodes, err)
				}
				return
			}
			product := requireLaunchProductError(t, err, messages.ErrorTemplatePlacementNodeLimit)
			want := map[string]any{"maximum": 1, "actual": 2}
			if got := product.Params(); !reflect.DeepEqual(got, want) {
				t.Fatalf("product error params = %#v, want %#v", got, want)
			}
		})
	}
}
