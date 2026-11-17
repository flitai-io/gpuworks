package estimate

import (
	"math"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// What a serving engine holds back besides the weights, one entry per
// combination that moves the number by more than a GiB. Measured on real
// hardware; scripts/estimate-calib/ holds the measurements and the harness.
//
// Two things pick the entry, and both are known before the deployment starts:
// whether vLLM captures attention inside its CUDA graphs, which for vLLM 0.25
// means architectures from Hopper up, and whether the deployment turns graphs
// off at all. Attention is where most of it goes - a full graph per batch size
// captures the attention op too, where a piecewise graph leaves attention
// running eagerly beside it. Turning graphs off costs 2.5 GiB of Hopper's total.
//
// This is the default model, not the only one. An engine that does not fit it -
// one that carves its pool out of a fraction of the card rather than a profiled
// peak, and so holds back a flat amount whatever the architecture and the graph
// switch - declares that amount in its own profile
// (deploy.Profile.ReserveBase) and is charged exactly it. Applying the vLLM
// number to such an engine was wrong in both directions at once: it
// over-charged the flat one by about a GiB and under-charged Hopper by 2.7.
//
// The values are the measured reserve with the shortfall below taken out, in
// whole MiB: rounded rather than exact, so that "this is a conservative choice"
// stays visible to whoever reads it next.
const (
	baseVLLMFullGraph      int64 = 4300 << 20
	baseVLLMPiecewise      int64 = 1330 << 20
	baseVLLMFullGraphEager int64 = 1540 << 20
	baseVLLMPiecewiseEager int64 = 410 << 20
)

// baseMetal is what vllm-metal holds besides the weights on Apple silicon:
// the API server process (about 0.8 GiB, which on unified memory comes out of
// the same pool) and the MLX engine's runtime. There are no CUDA graphs to
// size, so neither the graph switch nor the architecture moves it. An engine
// that declares no holdback of its own (the callable servers: mlx-vlm,
// llama.cpp) is charged the same figure, a server process plus a runtime
// being what they hold too. Measured
// 2.4 GiB for Qwen3-0.6B on a 64 GiB MacBook Pro with vllm-metal 0.31
// (artifacts/work/2026-10-13-apple-silicon-nodes/evidence/spike.md).
const baseMetal int64 = 2560 << 20

// shortfallBytes is the slice at the top of the card the driver never lets a
// CUDA process allocate. An engine sizes its pool against cudaMemGetInfo();
// gpuworks against the total the platform reads. It measured 0.47-0.75 GiB
// across four NVIDIA cards, and unlike the bases above it is spent in proportion
// to utilization - which is why it is a term rather than part of the table.
const shortfallBytes = 768 << 20

// ReserveBytes is the per-GPU memory gpuworks charges a deployment besides its
// weights: what separates the capacity a card reports from the capacity a
// deployment may use. reserveBase is the engine's own declared holdback
// (deploy.Profile.ReserveBase); zero means the engine declares none and the
// measured default model below applies to it.
//
// This is the one place that decides it. The scheduler, the GPU ledger and the
// deploy form all read the value from here rather than deriving their own, so
// what a user is shown and what admission enforces cannot disagree.
func ReserveBytes(reserveBase int64, archFamily string, graphs bool, gpuMemUtil float64) int64 {
	if accelerator.IsAppleFamily(archFamily) {
		// Metal has no driver slice a process cannot reach: the working set
		// the card reports is what an engine may use.
		if reserveBase > 0 {
			return reserveBase
		}
		return baseMetal
	}
	base := reserveBase
	if base <= 0 {
		base = defaultReserveBase(archFamily, graphs)
	}
	return base + int64(float64(shortfallBytes)*gpuMemUtil)
}

// nvidiaDGPUOutsideLimitBytes is what an NVIDIA dGPU instance holds on its card
// beyond its memory limit. HAMi-core intercepts the engine's allocations and
// shrinks what cudaMemGetInfo reports to the limit, so the engine's own
// holdback (ReserveBytes) is spent inside the limit; only the CUDA context the
// interception cannot see lands outside it. Measured 398 MiB per process
// (nvidia-smi minus the HAMi-accounted total) at 8 and 16 GiB limits on an
// RTX 2080 Ti, driver 580, vLLM 0.30
// (artifacts/work/2026-10-18-dgpu-auto-placement/README.md); rounded up.
const nvidiaDGPUOutsideLimitBytes int64 = 512 << 20

// PartitionClaimBytes is the per-GPU memory a dGPU memory-limited instance
// takes from its card: the limit plus what the partition backend lets the
// engine hold outside it. reserveBytes yields the ReserveBytes the same engine
// would be charged on a whole card; a backend whose overhead nobody has
// measured is charged that, which over-charges a co-tenant rather than
// oversubscribing the card. It is only called for such a backend: the ledger's
// reserve reads the instance's deployment, which an NVIDIA slice never needs.
// The scheduler's admission and the GPU ledger both read the claim from here,
// so they cannot disagree about what a slice costs.
func PartitionClaimBytes(limitBytes int64, backend string, reserveBytes func() int64) int64 {
	if backend == accelerator.BackendNVIDIALogical {
		return limitBytes + nvidiaDGPUOutsideLimitBytes
	}
	return limitBytes + reserveBytes()
}

// defaultReserveBase is the measured graph-shaped holdback: how much of the card
// a vLLM-shaped engine keeps for itself, per architecture and graph switch.
func defaultReserveBase(archFamily string, graphs bool) int64 {
	base := baseVLLMPiecewise
	switch {
	case capturesAttention(archFamily):
		base = baseVLLMFullGraph
		if !graphs {
			base = baseVLLMFullGraphEager
		}
	case !graphs:
		base = baseVLLMPiecewiseEager
	}
	return base
}

// capturesAttention reports whether vLLM captures attention inside its CUDA
// graphs on this architecture family. The list is a denylist on purpose: a
// platform nobody has measured takes the larger branch, because under-reserving
// is the direction that admits a deployment the engine then cannot start.
// Teaching this about a new platform is adding its name, once it has been
// measured.
func capturesAttention(archFamily string) bool {
	switch strings.ToLower(strings.TrimSpace(archFamily)) {
	case "turing", "ampere", "ada", "volta", "pascal", "maxwell":
		return false
	}
	return true
}

// Memory that stays outside an engine's share, expressed as what
// gpu_memory_utilization must leave behind rather than as part of the holdback
// above. The two are different quantities and must not be added:
// shortfallBytes is memory a CUDA process physically cannot get, while this is
// memory the platform declines to hand over so that something else can have
// it - the driver context, a display output, another process, headroom for an
// allocation spike. The shortfall lives inside this reserve.
const (
	// cardReserveBytes is left on a card with its own device memory. It is an
	// absolute figure because what competes for it is absolute: a display
	// output and a driver context cost what they cost, and do not shrink with
	// the card. At the fixed 0.9 that preceded this, a 12 GiB card was left
	// 1.2 GiB for all of it.
	cardReserveBytes int64 = 2 << 30
	// unifiedMemoryReserveFraction is left on a card whose memory is the
	// host's pool (accelerator.UnifiedMemory). Here the other consumer is the
	// whole host OS and everything running on the CPU side, whose needs scale
	// with the machine, so the reserve is a share rather than a constant.
	unifiedMemoryReserveFraction = 0.2
)

// MaxGPUMemUtil is the highest gpu_memory_utilization that still leaves a
// card's reserve untouched, rounded down onto the step the control moves in.
// It reports false when totalBytes says nothing (an unprobed or unreported
// card), which is not a cap of 1: a caller with no capacity fact must keep
// whatever default it already had rather than invent a share.
//
// Rounding down is what keeps the promise rather than a matter of taste. On a
// 12 GiB card the exact answer is 0.8333: 0.83 leaves 2.04 GiB and 0.84 leaves
// 1.92 GiB, so rounding to nearest would quietly break the reserve it was
// asked to protect.
func MaxGPUMemUtil(totalBytes int64, unified bool, step float64) (float64, bool) {
	if totalBytes <= 0 {
		return 0, false
	}
	share := 1 - float64(cardReserveBytes)/float64(totalBytes)
	if unified {
		share = 1 - unifiedMemoryReserveFraction
	}
	if step > 0 {
		// The epsilon absorbs binary representation error so a share that is
		// exactly on the grid (0.80 on a 10 GiB card) is not pushed a step
		// below it.
		share = onGrid(math.Floor(share/step+1e-9), step)
	}
	if share < 0 {
		return 0, true
	}
	return share, true
}

// onGrid multiplies a step count back into a value with the error of that
// multiplication removed: 83 steps of 0.01 is 0.83, not the 0.8300000000000001
// the bare product is. The difference is invisible in arithmetic and loud
// everywhere the number is written down - it reaches a deployment record, a
// form field and an engine's command line as text.
func onGrid(steps, step float64) float64 {
	perUnit := math.Round(1 / step)
	if perUnit <= 0 {
		return steps * step
	}
	return math.Round(steps*step*perUnit) / perUnit
}

// ForeignHeadroomBytes is the memory left free beside processes outside the
// platform on a card a memory-limited partition shares with them: 5% of the
// card, at least 4 GiB. What they hold is a reading taken once a minute, not
// a limit they keep to, and an out-of-memory failure there costs both their
// workload and the partition; the same shape as the host-memory headroom an
// L1 sleep keeps (sleep.HostMemoryHeadroom). See ADR 0068.
func ForeignHeadroomBytes(totalBytes int64) int64 {
	return max(totalBytes/20, 4<<30)
}
