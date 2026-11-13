package deployment

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestCheckPDSpec(t *testing.T) {
	pdOnly := templateSelection{Spec: &store.InferenceSpecRow{Name: "bf16-pd", PDMode: "vllm-nixl"}}
	ordinary := templateSelection{Spec: &store.InferenceSpecRow{Name: "bf16"}}
	nixl := &planning.PDPlacement{Mode: "vllm-nixl"}
	other := &planning.PDPlacement{Mode: "sglang-mooncake"}
	for _, c := range []struct {
		name      string
		selection templateSelection
		pd        *planning.PDPlacement
		code      string
	}{
		{"pd spec as pd", pdOnly, nixl, ""},
		{"pd spec as ordinary", pdOnly, nil, messages.ErrorPDSpecModeMismatch},
		{"pd spec in another mode", pdOnly, other, messages.ErrorPDSpecModeMismatch},
		{"ordinary spec as pd", ordinary, nixl, ""},
		{"ordinary spec as ordinary", ordinary, nil, ""},
		{"no template", templateSelection{}, nixl, ""},
	} {
		err := checkPDSpec(c.selection, c.pd)
		var product *messages.ProductError
		got := ""
		if errors.As(err, &product) {
			got = product.Code()
		}
		if got != c.code {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
}
