package planning

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestSleepDeclarationDistinguishesEngineFromAccelerator(t *testing.T) {
	for _, tc := range []struct{ engine, vendor, code string }{
		{"vllm", "kunlunxin", messages.ErrorAcceleratorFeatureUnsupported},
		{"vllm", "apple", messages.ErrorAcceleratorFeatureUnsupported},
		{"sglang", "nvidia", messages.ErrorSleepEngineUnsupported},
		{"vllm", "nvidia", ""}, {"vllm", "", ""},
	} {
		err := SleepDeclarable(true, tc.engine, tc.vendor)
		if tc.code == "" {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		var product *messages.ProductError
		if !errors.As(err, &product) || product.Code() != tc.code {
			t.Fatalf("%+v: %v", tc, err)
		}
		if tc.code == messages.ErrorAcceleratorFeatureUnsupported && (product.Params()["feature"] != string(accelerator.FeatureEngineSleep) || product.Params()["engine"] != nil) {
			t.Fatal(product.Params())
		}
	}
}
