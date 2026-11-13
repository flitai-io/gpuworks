package planning

import (
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/parallelism"
)

// ResolveAllocatedLaunch resolves executable fields for an existing allocation.
// It does no scheduling, estimates, I/O or resource admission. PlanLaunch calls
// it after scheduling; template export calls it with the instance's captured
// allocation. Only Params/Profile/Resolved/Warnings of the returned plan are set.
func ResolveAllocatedLaunch(req LaunchRequest, indexes []int, assigned int, parallel parallelism.Spec, apiPort int, variables map[string]any, weightBytes int64, unresolved bool) (LaunchPlan, error) {
	plan := LaunchPlan{}
	gpu, err := launchGPU(req.Node, indexes)
	if err != nil {
		return plan, err
	}
	vendor := gpu.Vendor
	if vendor == "" {
		vendor = req.Node.GPUVendor
	}
	plan.Profile, err = deploy.ProfileForTarget(req.Engine, vendor)
	if err != nil {
		return plan, err
	}
	plan.Params = launchParams(req, gpu, assigned, parallel, apiPort)
	engine, err := deploy.SelectEngine(req.Engine)
	if err != nil {
		return plan, err
	}
	plan.Resolved, plan.Warnings, err = deploy.ResolveLaunchWithWarnings(deploy.ResolveLaunchInput{
		StartTimeoutSec: req.StartTimeoutSec, DefaultStartTimeoutSec: req.DefaultStartTimeoutSec,
		Engine: engine, Params: plan.Params, Image: req.Source.Image, TargetValidation: deploy.ValidateAssignedTarget,
		ContainerOverrides: req.Source.Overrides, ComponentModelMount: componentModelMount(req),
		Entrypoint: req.Selection.Entrypoint, PreStartArgv: req.Selection.PreStartArgv,
		PreStartScript: req.Selection.PreStartScript, RuntimeAssets: req.Selection.RuntimeAssets,
		ComponentMounts: req.ComponentMounts, TemplateShmBytes: req.Selection.ShmBytes, TemplateMemlockUnlimited: req.Selection.MemlockUnlimited,
		APIContract: req.APIContract, PlacementVariables: variables, AllowUnresolved: unresolved,
		HealthPath: plan.Profile.HealthPath, ModelWeightBytes: weightBytes,
	})
	return plan, err
}
