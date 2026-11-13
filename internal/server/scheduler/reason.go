package scheduler

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Reason constructors. Every rejection code's params are built in exactly one
// place: clients render them by name, so a param written at one call site and
// read at another is a contract rather than an implementation detail. Keeping the
// constructors here also keeps the two meanings of "insufficient" apart — not
// enough GPUs is a count, not enough memory per GPU is a shortfall — which the
// callers used to express with the same code and different param meanings.

func gib(bytes int64) string {
	return fmt.Sprintf("%.1f", float64(bytes)/(1<<30))
}

// groupParams names a candidate group of gpus GPUs on this node: its size and
// the deployment's TP and PP that run on it. The group is TP × PP on one node
// (TP × PP ÷ nodes across nodes), so the size and the TP differ whenever PP or
// more nodes are involved; a reason that said "tp" for the size read TP 4 for
// a TP 2 × PP 2 draft.
func groupParams(req Request, gpus int) map[string]any {
	return map[string]any{"gpus": gpus, "tp": headTP(req, gpus), "pp": req.Parallel.Normalized().PP}
}

// reasonTPNotDivisible rejects a tensor-parallel size that cannot shard the
// model's attention heads.
func reasonTPNotDivisible(tp, headCount int) Reason {
	return Reason{Code: ReasonTPHeadCountNotDivisible, Params: map[string]any{"tp": tp, "head_count": headCount}}
}

// reasonInsufficientGPUs rejects tp because too few GPUs qualify. qualifies is a
// count; need is the per-GPU memory requirement the qualifier was measured
// against.
func reasonInsufficientGPUs(req Request, tp int, need int64, qualifies int) Reason {
	code := ReasonInsufficientEmptyGPUs
	if req.PerGPULimitBytes > 0 {
		code = ReasonInsufficientSharedGPUs
	}
	params := groupParams(req, tp)
	params["required_gib"], params["available"] = gib(need), qualifies
	return Reason{Code: code, Params: params}
}

// reasonMemoryShortfall reports that the qualifying GPUs offer less memory per
// card than this candidate needs. The numbers come from the admission check that
// failed — the requirement at this tp and the capacity of the binding card of
// the roomiest group (bindingUsableCapacity) — so the verdict and its figures
// cannot disagree.
func reasonMemoryShortfall(req Request, gpus []GPU, tp int, need int64) Reason {
	binding, count, ok := bindingUsableCapacity(req, gpus, tp)
	if !ok {
		// Nothing qualifies at all, which is the count problem, not a shortfall.
		return reasonInsufficientGPUs(req, tp, need, count)
	}
	available := binding.CapacityFor(req)
	// The requirement is what admission compared: on a GPU shared by memory
	// that is the claim, which can exceed need.
	required := binding.Claim(req, need)
	params := groupParams(req, tp)
	params["gpu_count"] = count
	params["required_bytes"], params["required_gib"] = required, gib(required)
	params["available_bytes"], params["available_gib"] = available, gib(available)
	params["gpu_memory_utilization"] = req.GPUMemUtil
	// A card hosting sleepers offers less than its utilization share; naming the
	// residue is what lets the reader reconcile the figure with the card's size.
	if binding.SleepingBytes > 0 {
		params["sleeping_bytes"] = binding.SleepingBytes
		params["sleeping_gib"] = gib(binding.SleepingBytes)
	}
	return Reason{Code: ReasonNoFit, Params: params}
}

// reasonSleepResidueBlocksStartup explains the distinct whole-GPU failure
// where the request itself fits the utilization capacity, but the engine cannot
// reserve that capacity while a sleeping instance still owns its residue.
func reasonSleepResidueBlocksStartup(req Request, g GPU) Reason {
	startup := int64(float64(g.MemoryTotal) * req.GPUMemUtil)
	available := g.MemoryTotal - g.SleepingBytes
	return Reason{Code: ReasonSleepResidueBlocksStartup, Params: map[string]any{
		"startup_bytes":          startup,
		"startup_gib":            gib(startup),
		"available_bytes":        available,
		"available_gib":          gib(available),
		"sleeping_bytes":         g.SleepingBytes,
		"sleeping_gib":           gib(g.SleepingBytes),
		"gpu_memory_utilization": req.GPUMemUtil,
	}}
}

// sleepResidueBlock returns a specific reason only when every card needed for
// this tensor-parallel candidate is blocked by the same startup reservation.
// Mixed failures remain the generic memory/count explanation.
func sleepResidueBlock(req Request, gpus []GPU, tp int, need int64) (Reason, bool) {
	if req.PerGPULimitBytes > 0 || tp <= 0 {
		return Reason{}, false
	}
	blocked := 0
	var first GPU
	for _, g := range gpus {
		if !g.QualifiesFor(req) || g.SleepingBytes <= 0 || g.CapacityFor(req) < need {
			continue
		}
		startup := int64(float64(g.MemoryTotal) * req.GPUMemUtil)
		if startup <= g.MemoryTotal-g.SleepingBytes {
			continue
		}
		if blocked == 0 {
			first = g
		}
		blocked++
	}
	if blocked < tp {
		return Reason{}, false
	}
	return reasonSleepResidueBlocksStartup(req, first), true
}

// reasonPlacementRejected rejects a GPU group because the catalog's placement
// contract refused it.
func reasonPlacementRejected(detail string) Reason {
	return Reason{Code: ReasonPlacementRejected, Params: map[string]any{"detail": detail}}
}

// EnginePortsExhausted rejects a node on which every port of the engine port
// range [start, end] is held by an instance or a reported host listener.
func EnginePortsExhausted(node string, start, end int) Reason {
	return Reason{Code: ReasonEnginePortsExhausted, Params: map[string]any{"node": node, "start": start, "end": end}}
}

// ImageSourceForbidden is a node the pull source rules leave nothing to pull
// the image from.
func ImageSourceForbidden(node, image string) Reason {
	return Reason{Code: ReasonImageSourceForbidden, Params: map[string]any{"node": node, "image": image}}
}

// WeightsFetchForbidden is a node the pull source rules leave nowhere to
// download a model from.
func WeightsFetchForbidden(node, model string) Reason {
	return Reason{Code: ReasonWeightsFetchForbidden, Params: map[string]any{"node": node, "model": model}}
}

// ImageDeliveryFailed reports that an image the deployment waits to be
// delivered - a peer transfer or an archive URL, rather than a pull the node
// performs when it starts the container - failed on node. source and detail come
// from the image ledger row that recorded the failure; both are omitted when the
// row carries none, so the rendered text never shows a value nobody wrote.
func ImageDeliveryFailed(node, image, source, detail string) Reason {
	params := map[string]any{"node": node, "image": image}
	if s := strings.TrimSpace(source); s != "" {
		params["source"] = s
	}
	if d := strings.TrimSpace(detail); d != "" {
		params["detail"] = d
	}
	return Reason{Code: ReasonImageDeliveryFailed, Params: params}
}

// bindingUsableCapacity reports the card that limits the roomiest group of tp
// qualifying GPUs - the tp-th largest capacity - and how many GPUs qualify. That
// is how close the node comes to fitting: a tp=1 request the half-free card
// could nearly take must be described by that card, not by a full one beside
// it, or the message reads "0 GiB available" on a node with room to spare.
// ok is false when fewer than tp GPUs qualify, which is a count problem rather
// than a shortfall.
func bindingUsableCapacity(req Request, gpus []GPU, tp int) (GPU, int, bool) {
	qualifying := qualifyingByCapacity(req, gpus)
	if tp < 1 || len(qualifying) < tp {
		return GPU{}, len(qualifying), false
	}
	return qualifying[tp-1], len(qualifying), true
}

// qualifyingByCapacity is the GPUs that qualify for the request, roomiest
// first; equal capacities keep their order.
func qualifyingByCapacity(req Request, gpus []GPU) []GPU {
	qualifying := make([]GPU, 0, len(gpus))
	for _, gpu := range gpus {
		if gpu.QualifiesFor(req) {
			qualifying = append(qualifying, gpu)
		}
	}
	sort.SliceStable(qualifying, func(i, j int) bool {
		return qualifying[i].CapacityFor(req) > qualifying[j].CapacityFor(req)
	})
	return qualifying
}

// blockingReason explains why no candidate could be placed. The planner records
// one rejection per tensor-parallel option in the order it considered them, so
// the least demanding option — the one that needs the fewest GPUs per card and
// therefore the easiest to satisfy — carries the binding constraint. Reporting
// that rejection, instead of a separately derived verdict, is what keeps the
// message consistent with the decision that was actually made.
func blockingReason(rejected []Rejected) Reason {
	if len(rejected) == 0 {
		// Unreachable while allowedTPs always yields at least one option; a bare
		// code beats inventing numbers for a verdict nobody computed.
		return Reason{Code: ReasonNoFit}
	}
	least := rejected[0]
	for _, r := range rejected[1:] {
		if r.TP < least.TP {
			least = r
		}
	}
	return least.Reason
}

// TargetLaunchRefusal converts a structured target capability error into the
// scheduler vocabulary; unrelated product or system errors remain untouched.
func TargetLaunchRefusal(err error) (*Reason, bool) {
	var product *messages.ProductError
	if !errors.As(err, &product) {
		return nil, false
	}
	if product.Code() == messages.ErrorLaunchFeaturesUnknown {
		reason := Reason{Code: ReasonLaunchFeaturesUnknown, Params: product.Params()}
		return &reason, true
	}
	if product.Code() == messages.ErrorAcceleratorDeviceSelectionRequired {
		reason := Reason{Code: ReasonAcceleratorDeviceSelectionRequired, Params: product.Params()}
		return &reason, true
	}
	if product.Code() != messages.ErrorAcceleratorFeatureUnsupported {
		return nil, false
	}
	params := product.Params()
	feature, _ := params["feature"].(string)
	vendor, _ := params["vendor"].(string)
	reason := Reason{Code: ReasonAcceleratorFeatureUnsupported, Params: map[string]any{"vendor": vendor, "feature": feature}}
	return &reason, true
}
