package scheduler

import (
	"errors"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestTargetRefusalPreservesUnknownErrors(t *testing.T) {
	for _, err := range []error{nil, errors.New("disk read failed"), messages.NewProductError("OTHER_PRODUCT_ERROR", "other", nil)} {
		if reason, ok := TargetLaunchRefusal(err); ok || reason != nil {
			t.Fatalf("unrelated error became refusal: %v / %v", err, reason)
		}
	}
	product := messages.NewProductError(messages.ErrorAcceleratorFeatureUnsupported, "unsupported", map[string]any{"vendor": "AMD", "feature": "pipeline_parallelism"})
	reason, ok := TargetLaunchRefusal(fmt.Errorf("launch: %w", product))
	if !ok || reason == nil || reason.Code != ReasonAcceleratorFeatureUnsupported || reason.Params["feature"] != "pipeline_parallelism" || reason.Params["vendor"] != "AMD" {
		t.Fatalf("wrapped target refusal lost facts: %+v", reason)
	}
}
func TestUnsupportedFeatureKeepsGenericInterpolationFacts(t *testing.T) {
	for _, feature := range []accelerator.Feature{accelerator.FeaturePipelineParallel, accelerator.FeatureDataParallel, accelerator.Feature("partition"), accelerator.Feature("future_feature")} {
		reason := UnsupportedFeature(accelerator.VendorAMD, feature)
		if reason.Code != ReasonAcceleratorFeatureUnsupported || reason.Params["vendor"] != "AMD" || reason.Params["feature"] != string(feature) {
			t.Fatalf("generic refusal lost feature %s: %+v", feature, reason)
		}
	}
}
