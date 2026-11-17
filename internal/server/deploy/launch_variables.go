package deploy

import (
	"fmt"
	"strings"

	placementeval "gpuworks.ai/gpuworks/internal/server/placement"
)

// launchVariables is the variable set a template's args, env, entrypoint and
// preparation are expanded against: the placement resolver's own variables,
// then the facts only this launch knows (docs/reference/inference-catalog-spec.md
// lists them). The launch-owned names cannot come from a resolver
// (placement.LaunchOwned), so the order only matters for GPU_COUNT and
// TP_SIZE, which the placement result states whenever it has one.
//
// An unresolved preview has no GPU count or degree yet: those two stay absent
// so the preview shows them symbolically instead of a placeholder value. The
// master address and port, and this rank's own address, exist only for a
// distributed launch; a template that references them on a single node fails
// rather than dialing nothing.
//
// SERVED_MODEL_NAME is the name the gateway and the health check send as the
// request's model. Only vLLM and SGLang have that request rewritten to the
// engine's own name, so a callable adapter that checks the model has to
// answer under exactly this name.
func launchVariables(engine Engine, params Params, modelMount string, placement map[string]any, allowUnresolved bool) map[string]any {
	variables := make(map[string]any, len(placement)+12)
	for name, value := range placement {
		variables[name] = value
	}
	if !allowUnresolved {
		if _, ok := variables[placementeval.VarGPUCount]; !ok && params.AssignedGPUs > 0 {
			variables[placementeval.VarGPUCount] = int64(params.AssignedGPUs)
		}
		if _, ok := variables[placementeval.VarTPSize]; !ok && params.TensorParallel > 0 {
			variables[placementeval.VarTPSize] = int64(params.TensorParallel)
		}
	}
	host := params.Host
	if host == "" {
		host = hooks(engine).APIBindHost()
	}
	nnodes := params.NNodes
	if nnodes < 1 {
		nnodes = 1
	}
	headless := int64(0)
	if params.Headless {
		headless = 1
	}
	variables[placementeval.VarModelPath] = modelMount
	if params.ServedName != "" {
		variables[placementeval.VarServedModelName] = params.ServedName
	}
	variables[placementeval.VarAPIHost] = host
	// A headless rank serves no API; zero says so instead of a port nobody
	// reserved for it.
	variables[placementeval.VarAPIPort] = int64(params.APIPort)
	variables[placementeval.VarNNodes] = int64(nnodes)
	variables[placementeval.VarNodeRank] = int64(params.NodeRank)
	variables[placementeval.VarHeadless] = headless
	if nnodes > 1 {
		if params.NodeAddress != "" {
			variables[placementeval.VarNodeAddr] = params.NodeAddress
		}
		if params.MasterAddr != "" {
			variables[placementeval.VarMasterAddr] = params.MasterAddr
		}
		if params.MasterPort > 0 {
			variables[placementeval.VarMasterPort] = int64(params.MasterPort)
		}
	}
	if params.PD != nil {
		variables[placementeval.VarPDRole] = params.PD.Role
		if params.PD.SideChannelHost != "" {
			variables[placementeval.VarPDSideChannelHost] = params.PD.SideChannelHost
		}
		if params.PD.SideChannelPort > 0 {
			variables[placementeval.VarPDSideChannelPort] = int64(params.PD.SideChannelPort)
		}
	}
	if params.Symbolic {
		// A template that places a rank-owned fact itself hands the draft
		// the reference a manual command understands.
		for _, name := range rankOwnedVariables {
			variables[name] = variableReference(name)
		}
	}
	return variables
}

// expandValues expands argv-like template values. Nothing evaluates them
// after this, so a name nobody states is an error; an unresolved preview
// keeps the names it cannot fill in the shell-neutral symbolic form.
func expandValues(values []string, variables map[string]any, allowUnresolved bool) ([]string, error) {
	if allowUnresolved {
		expanded, err := placementeval.Expand(values, variables, true)
		if err != nil {
			return nil, err
		}
		return placementeval.Symbolize(expanded), nil
	}
	return placementeval.Expand(values, variables, false)
}

// expandScript expands a preparation script. The script runs in a shell, so a
// ${NAME} the platform never states belongs to the shell and stays as written;
// a platform variable this launch does not have is still an error, because the
// shell would quietly turn it into an empty string.
func expandScript(script string, variables map[string]any, allowUnresolved bool) (string, error) {
	if !allowUnresolved {
		for _, name := range placementeval.References(script) {
			if _, ok := placementeval.Lookup(variables, name); !ok && placementeval.PlatformVariable(name) {
				return "", fmt.Errorf("template variable %s is unresolved", name)
			}
		}
	}
	expanded, err := placementeval.Expand([]string{script}, variables, true)
	if err != nil {
		return "", err
	}
	return expanded[0], nil
}

// requireTemplateVariables enforces an engine's RequiredTemplateVariables over
// the template's own launch fields. Only an engine-mode launch is checked: a
// deployment command override owns the whole command and places the port
// itself.
func requireTemplateVariables(engine Engine, templateFields ...[]string) error {
	required := hooks(engine).RequiredTemplateVariables()
	if len(required) == 0 {
		return nil
	}
	var values []string
	for _, field := range templateFields {
		values = append(values, field...)
	}
	referenced := map[string]struct{}{}
	for _, name := range placementeval.References(values...) {
		referenced[placementeval.Canonical(name)] = struct{}{}
	}
	var missing []string
	for _, name := range required {
		if _, ok := referenced[name]; !ok {
			missing = append(missing, "${"+name+"}")
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s template must reference %s: the engine adds no platform argument of its own", engine.Name(), strings.Join(missing, ", "))
	}
	return nil
}

func envValues(env map[string]string) []string {
	values := make([]string, 0, len(env))
	for _, value := range env {
		values = append(values, value)
	}
	return values
}
