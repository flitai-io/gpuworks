package deployment

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// A clone keeps what the request sent - empty maps stay empty, numbers keep
// their types - and shares nothing with it, so normalizing one attempt leaves
// the next one's request untouched.
func TestCloneRequestKeepsEmptyValuesAndSharesNothing(t *testing.T) {
	healthy := true
	req := dto.DeployRequest{
		QuickParams:        map[string]any{},
		NodeRequirements:   map[string][]string{"pool": {"a"}},
		Placement:          map[string]any{"pd": map[string]any{"tp": 2}},
		ContainerOverrides: map[string]any{},
		NodeUUIDs:          []string{},
		TokenizeEnabled:    &healthy,
	}
	clone := cloneRequest(req)
	if !reflect.DeepEqual(clone, req) {
		t.Fatalf("clone = %#v, want %#v", clone, req)
	}
	if clone.QuickParams == nil || clone.ContainerOverrides == nil || clone.NodeUUIDs == nil {
		t.Fatal("an empty value became nil")
	}
	if _, isInt := clone.Placement["pd"].(map[string]any)["tp"].(int); !isInt {
		t.Fatal("a number changed type")
	}
	clone.NodeRequirements["pool"][0] = "changed"
	clone.Placement["pd"].(map[string]any)["tp"] = 4
	*clone.TokenizeEnabled = false
	if req.NodeRequirements["pool"][0] != "a" || req.Placement["pd"].(map[string]any)["tp"] != 2 || !*req.TokenizeEnabled {
		t.Fatal("the clone shares state with the request")
	}
}
