package planning

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/server/sleep"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// residues answers what each sleeper on a node is charged per GPU
// (sleep.ResidueBytes): the residue its agent measured when the instance had
// its cards to itself, else the estimate from the model's size and how many
// GPUs the engine spans. One ledger pass reads each deployment and model at
// most once.
type residues struct {
	st     *store.Store
	params map[uint]float64 // by deployment ID
	// known, when set, answers a deployment's parameter count from rows the
	// caller loaded in bulk (FleetCards), so a pass over the fleet reads
	// neither deployments nor models one at a time.
	known func(deploymentID uint) float64
	// gpuMemUtil, when set, answers a deployment's gpu_mem_util from rows
	// the caller loaded in bulk (FleetAllocations), for a memory-limited
	// slice's claim; ok is false for a deployment it does not hold, which is
	// read from the store as before.
	gpuMemUtil func(deploymentID uint) (util float64, ok bool)
}

func newResidues(st *store.Store) *residues {
	return &residues{st: st, params: map[uint]float64{}}
}

// of is the per-GPU residue of a sleeping instance.
func (r *residues) of(in *store.Instance) int64 {
	gpus := max(len(parseInts(in.GPUIndexes)), 1) * max(in.NNodes, 1)
	return sleep.ResidueBytes(in.SleepResidueBytes, sleep.EstimateResidueBytes(r.paramsOf(in.DeploymentID), gpus))
}

// paramsOf is the deployment's model's total parameter count, 0 when the
// deployment or its model is gone or its size is unknown.
func (r *residues) paramsOf(deploymentID uint) float64 {
	if params, ok := r.params[deploymentID]; ok {
		return params
	}
	var params float64
	if r.known != nil {
		params = r.known(deploymentID)
	} else if r.st != nil {
		if dep, err := r.st.GetDeployment(deploymentID); err == nil {
			if model, err := r.st.GetModel(dep.ModelID); err == nil {
				params = modelParams(model)
			}
		}
	}
	r.params[deploymentID] = params
	return params
}

// modelParams is a model's parameter count derived from its weight size and
// the bytes one parameter takes in its stored format. A format it does not
// recognize is read as 16-bit, the common case for an unquantized checkpoint.
func modelParams(model *store.Model) float64 {
	if model == nil || model.WeightBytes <= 0 {
		return 0
	}
	return float64(model.WeightBytes) / bytesPerParam(model.QuantLabel, model.Dtype)
}

// bytesPerParam is the storage size of one parameter in a quantization or
// dtype label, matched by the precision the label names.
func bytesPerParam(labels ...string) float64 {
	for _, label := range labels {
		l := strings.ToLower(label)
		switch {
		case l == "":
			continue
		case strings.Contains(l, "fp32") || strings.Contains(l, "float32"):
			return 4
		case strings.Contains(l, "bf16") || strings.Contains(l, "fp16") || strings.Contains(l, "float16") || strings.Contains(l, "half"):
			return 2
		case strings.Contains(l, "fp8") || strings.Contains(l, "int8") || strings.Contains(l, "w8a8") || strings.Contains(l, "8bit") || strings.Contains(l, "q8"):
			return 1
		case strings.Contains(l, "q6"):
			return 0.82
		case strings.Contains(l, "q5"):
			return 0.69
		case strings.Contains(l, "iq2") || strings.Contains(l, "q2"):
			return 0.32
		case strings.Contains(l, "iq3") || strings.Contains(l, "q3"):
			return 0.43
		case strings.Contains(l, "fp4") || strings.Contains(l, "int4") || strings.Contains(l, "w4a16") || strings.Contains(l, "awq") ||
			strings.Contains(l, "gptq") || strings.Contains(l, "4bit") || strings.Contains(l, "q4") || strings.Contains(l, "exl3"):
			return 0.56
		}
	}
	return 2
}
