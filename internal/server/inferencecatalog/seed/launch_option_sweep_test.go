package seed

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/placement"
	"gpuworks.ai/gpuworks/internal/server/planning"
)

// launchOption is one combination of the server-owned launch switches a
// deployment of a built-in spec can carry without any user-layer value.
type launchOption struct {
	sleepable     bool
	runtimeDriver string
}

func (o launchOption) String() string {
	return fmt.Sprintf("sleep=%t runtime=%s", o.sleepable, o.runtimeDriver)
}

// runtimeDriverUniverse is every container runtime driver the server's own
// protocol names. Which of them a vendor runs under is asked of the
// accelerator package (RuntimeMismatchFor), never decided here.
var runtimeDriverUniverse = []string{
	proto.RuntimePreferenceDocker, proto.RuntimePreferenceNerdctl, proto.RuntimePreferenceUdocker, proto.RuntimeDriverHost,
}

// supportedLaunchOptions are the combinations the platform itself says the
// spec's engine and vendor support: sleep only where planning.SleepDeclarable
// accepts it (the create path's own gate), and a runtime driver only where
// the vendor's host contract accepts it. The host driver is the macOS runtime
// (engines as host processes), so it is judged against a darwin host and the
// container drivers against linux/amd64; the vendor's contract decides.
func supportedLaunchOptions(engine, vendor string) []launchOption {
	var drivers []string
	for _, driver := range runtimeDriverUniverse {
		host := accelerator.HostRuntime{OS: "linux", Arch: "amd64", Driver: driver}
		if driver == proto.RuntimeDriverHost {
			host = accelerator.HostRuntime{OS: "darwin", Arch: "arm64", Driver: driver}
		}
		if accelerator.RuntimeMismatchFor(vendor, host) == nil {
			drivers = append(drivers, driver)
		}
	}
	sleeps := []bool{false}
	if planning.SleepDeclarable(true, engine, vendor) == nil {
		sleeps = append(sleeps, true)
	}
	var out []launchOption
	for _, sleepable := range sleeps {
		for _, driver := range drivers {
			out = append(out, launchOption{sleepable: sleepable, runtimeDriver: driver})
		}
	}
	return out
}

// Every built-in spec must resolve a launch under every option combination
// the platform says its engine and vendor support. The sweep supplies no
// user-layer value (no extra args, quick params or container overrides), so a
// refusal can only come from a template-layer value the platform did not
// adapt to a switch it offers — the shape of 2b13b5e8c, where every Ascend
// vLLM template was refused once sleep was on.
//
// Left out: PD specs (a PD launch needs the PD group's ranks, which only a
// placed deployment has) and multi-node launches (master address and node
// ranks are placement output; a spec is multi-node when its own text names a
// variable only a distributed launch states). The dGPU memory limit is not a dimension:
// deploy.ResolveLaunch does not read Params.VRAMLimitBytes or Partitioned.
func TestBuiltinSpecsLaunchUnderEverySupportedOption(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	specs, combinations, skippedPD, skippedMultiNode := 0, 0, 0, 0
	perOption := map[string]int{}
	refusals := map[string][]string{}
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			where := model.ModelID + "/" + spec.Name
			if strings.TrimSpace(spec.PDMode) != "" {
				skippedPD++
				continue
			}
			if statesDistributedVariable(spec) {
				skippedMultiNode++
				continue
			}
			engine, err := deploy.SelectEngine(spec.Engine)
			if err != nil {
				t.Errorf("%s: engine %q: %v", where, spec.Engine, err)
				continue
			}
			vendor := accelerator.NormalizeVendor(spec.GPUs.Vendor)
			options := supportedLaunchOptions(engine.Name(), vendor)
			if len(options) == 0 {
				t.Errorf("%s: the platform offers no launch option for engine %s on %q", where, engine.Name(), spec.GPUs.Vendor)
				continue
			}
			specs++
			for _, option := range options {
				combinations++
				perOption[option.String()]++
				if err := resolveOnSomePlacement(engine, vendor, spec, option); err != nil {
					key := option.String() + ": " + refusalKey(err)
					refusals[key] = append(refusals[key], where)
				}
			}
		}
	}
	keys := make([]string, 0, len(perOption))
	for key := range perOption {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		t.Logf("option %s: %d specs", key, perOption[key])
	}
	t.Logf("checked %d specs in %d option combinations (%d PD and %d multi-node specs left out)", specs, combinations, skippedPD, skippedMultiNode)
	if specs < 100 || combinations < 2*specs {
		t.Fatalf("checked %d specs in %d combinations; the sweep is close to vacuous", specs, combinations)
	}
	refused := make([]string, 0, len(refusals))
	for key := range refusals {
		refused = append(refused, key)
	}
	sort.Strings(refused)
	for _, key := range refused {
		where := refusals[key]
		sample := where
		if len(sample) > 5 {
			sample = sample[:5]
		}
		t.Errorf("%d specs refused with %s; e.g. %s", len(where), key, strings.Join(sample, ", "))
	}
}

// statesDistributedVariable reports whether the spec's own launch text names a
// variable the platform states only for a distributed launch.
func statesDistributedVariable(spec inferencecatalog.InferenceSpec) bool {
	values := append(append(append([]string(nil), spec.Args...), spec.Entrypoint...), spec.PreStartArgv...)
	values = append(values, spec.PreStartScript)
	for _, value := range spec.Env {
		values = append(values, value)
	}
	for _, name := range placement.References(values...) {
		switch name {
		case placement.VarMasterAddr, placement.VarMasterPort, placement.VarNodeRank, placement.VarNodeAddr:
			return true
		}
	}
	return false
}

// placementGPUCounts are the per-node GPU counts a placement can hand a
// single-node launch. How many GPUs a spec gets is placement output, not a
// template value, so a spec passes when some count its placement resolver
// accepts launches it.
var placementGPUCounts = []int{1, 2, 4, 8, 16}

// sweepCardMemoriesGiB are the card sizes the sweep offers a placement
// resolver, so a resolver that gates a profile on card memory accepts one.
var sweepCardMemoriesGiB = []float64{16, 24, 32, 48, 64, 80, 96, 128, 141, 192}

// errNoPlacement says no GPU count the sweep offers was accepted by the
// spec's placement resolver, so nothing was resolved.
var errNoPlacement = errors.New("no offered GPU count is accepted by the placement resolver")

// resolveOnSomePlacement resolves spec's launch under option on every GPU
// count its placement resolver accepts, the way planning hands a scheduled
// GPU set to the launch: the resolver's variables, the GPU count, and the
// tensor-parallel degree the set gives each data-parallel replica. It answers
// nil when one of them launches, else the first refusal.
func resolveOnSomePlacement(engine deploy.Engine, vendor string, spec inferencecatalog.InferenceSpec, option launchOption) error {
	dp, err := deploy.PlacementDataParallel(engine.Name(), 0, spec.Args, nil)
	if err != nil {
		return err
	}
	var first error
	for _, gpus := range placementGPUCounts {
		if gpus%dp != 0 {
			continue
		}
		tp := gpus / dp
		var variables map[string]any
		if spec.Placement != nil && strings.TrimSpace(spec.Placement.Resolver) != "" {
			accepted := false
			for _, memory := range sweepCardMemoriesGiB {
				card := placement.GPU{Count: gpus, Vendor: vendor, Automatic: true, MinMemoryGiB: memory}
				for index := 0; index < gpus; index++ {
					card.Indexes = append(card.Indexes, index)
					card.Names = append(card.Names, spec.GPUs.Name)
					card.MemoriesGiB = append(card.MemoriesGiB, memory)
				}
				result, err := placement.Resolve(context.Background(), spec.Placement.Resolver, card)
				if err != nil {
					return err
				}
				if result.Reason != "" {
					continue
				}
				if value, ok := result.Variables[placement.VarTPSize].(int64); ok && value > 0 {
					tp = int(value)
				}
				variables, accepted = placement.RuntimeVariables(result, gpus, tp, deploy.DefaultModelMount), true
				break
			}
			if !accepted {
				continue
			}
		}
		_, _, err := deploy.ResolveLaunchWithWarnings(builtinLaunchInput(engine, vendor, spec, option, gpus, tp, variables))
		if err == nil {
			return nil
		}
		if first == nil {
			first = err
		}
	}
	if first == nil {
		return errNoPlacement
	}
	return first
}

// builtinLaunchInput is the launch a deployment of spec resolves with no
// user-layer value: the spec's own command, environment, preparation, API
// contract and component mounts, the GPU count, degree and resolver
// variables a placement settled, and the option's platform switches.
func builtinLaunchInput(engine deploy.Engine, vendor string, spec inferencecatalog.InferenceSpec, option launchOption, gpus, tp int, variables map[string]any) deploy.ResolveLaunchInput {
	var assets []proto.RuntimeAsset
	for _, asset := range spec.Assets {
		assets = append(assets, proto.RuntimeAsset{File: asset.File, SHA256: asset.SHA256, SizeBytes: asset.SizeBytes, UnpackTo: asset.UnpackTo})
	}
	var mounts []proto.MountSpec
	for _, component := range spec.Components {
		if component.Role == "primary" {
			continue
		}
		mounts = append(mounts, proto.MountSpec{Source: "/components/" + component.Name, Target: component.MountPath, ReadOnly: true, Purpose: "model_component:" + component.Role})
	}
	componentMount := ""
	for _, component := range spec.Components {
		if component.Role == "primary" {
			componentMount = component.MountPath
		}
	}
	var api any
	if spec.API != nil {
		api = *spec.API
	}
	return deploy.ResolveLaunchInput{
		Engine: engine, Image: spec.Image, Entrypoint: spec.Entrypoint,
		PreStartArgv: spec.PreStartArgv, PreStartScript: spec.PreStartScript, RuntimeAssets: assets,
		ComponentMounts: mounts, ComponentModelMount: componentMount, APIContract: api,
		TemplateShmBytes: spec.ShmBytes, TemplateMemlockUnlimited: spec.MemlockUnlimited,
		PlacementVariables: variables,
		Params: deploy.Params{
			ModelPath: deploy.DefaultModelMount, APIPort: 31417, NNodes: 1, RuntimeDriver: option.runtimeDriver,
			Host:         deploy.RuntimeBindHost(option.runtimeDriver),
			AssignedGPUs: gpus, TensorParallel: tp, Sleepable: option.sleepable, Vendor: vendor,
			TemplateArgs: spec.Args, TemplateEnv: spec.Env,
		},
	}
}

// refusalKey groups refusals by their stable code when they carry one.
func refusalKey(err error) string {
	message := err.Error()
	if len(message) > 160 {
		message = message[:160]
	}
	return message
}
