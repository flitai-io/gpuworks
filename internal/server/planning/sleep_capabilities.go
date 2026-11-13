package planning

import (
	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/sleep"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// SleepEligibility answers whether a stored deployment may be ordered to sleep
// or wake, as the sleep policy's stable refusal code (empty when it may). It
// pairs the deployment with its engine's declared capability so callers that
// hold the entity need not assemble the policy input themselves.
func SleepEligibility(dep *store.Deployment) string {
	return sleep.Eligible(sleep.Deployment{Sleepable: dep.Sleepable, Caps: deploy.SleepCapsForEngine(dep.Engine)})
}

// SleepDeclarable answers whether a deployment request may declare itself
// sleepable on the engine it names; a request that does not ask to sleep is
// always fine.
func SleepDeclarable(sleepable bool, engine, vendor string) error {
	if !sleepable {
		return nil
	}
	caps := deploy.SleepCapsForEngine(engine)
	if code := sleep.Declarable(sleep.Deployment{Sleepable: true, Caps: caps}); code != "" {
		return messages.NewProductError(code, sleepRefusalText(code), map[string]any{"engine": engine})
	}
	if !accelerator.Supports(vendor, accelerator.FeatureEngineSleep) {
		return messages.NewProductError(messages.ErrorAcceleratorFeatureUnsupported, "target does not support engine sleep", map[string]any{"vendor": accelerator.VendorBrand(vendor), "feature": string(accelerator.FeatureEngineSleep)})
	}
	return nil
}
func sleepRefusalText(code string) string { return code }
