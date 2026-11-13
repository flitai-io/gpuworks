package gpu

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// HostRuntimeCheck describes how one vendor's host runtime is asked about one
// card. Listing a card and using one are different questions, and only the
// vendor's own runtime answers the second. Vendors whose runtime exists only
// inside inference images keep the per-card verdict from their CLI instead.
type HostRuntimeCheck struct {
	// CLI is the vendor tool whose health gates a round: while it is not
	// answering, every attempt would queue on the same driver lock.
	CLI string
	// Timeout bounds one card's attempt, or zero for the scheduler's default.
	Timeout time.Duration
	// Prerequisite says why the check cannot run on this host at all, or "".
	Prerequisite func() string
	// Visible identifies the card to Try and is false when the card cannot be
	// named safely. NVIDIA uses a UUID directly; Ascend supplies the physical
	// die index from which Try resolves the driver's logical ACL device ID.
	Visible func(proto.GPUInfo) (string, bool)
	// Try asks the runtime to use only the card identified by Visible.
	Try func(ctx context.Context, visible string) error
}

var hostRuntimeChecks = map[string]HostRuntimeCheck{
	accelerator.VendorNVIDIA: {
		CLI:          "nvidia-smi",
		Prerequisite: CUDASmokePrerequisite,
		// A UUID cannot drift to another card when the set of working cards
		// changes, while a device index can.
		Visible: func(g proto.GPUInfo) (string, bool) { return g.UUID, g.UUID != "" },
		Try:     CUDARuntimeSmokeOn,
	},
	accelerator.VendorAscend: {
		CLI:          "npu-smi",
		Prerequisite: AscendCLSmokePrerequisite,
		// ACL initialization on a die that cannot create a context can spend
		// almost the scheduler's default 30 seconds before returning its
		// vendor error.
		Timeout: 45 * time.Second,
		Visible: func(g proto.GPUInfo) (string, bool) {
			if g.Index < 0 {
				return "", false
			}
			return strconv.Itoa(g.Index), true
		},
		Try: AscendRuntimeSmokeOn,
	},
	accelerator.VendorHygon: {
		CLI:          "hy-smi",
		Prerequisite: HygonHIPSmokePrerequisite,
		// The script finds the card by its address among every card HIP
		// lists, so the address is the name; a card without one cannot be
		// told apart from its neighbours.
		Visible: func(g proto.GPUInfo) (string, bool) { return g.PCIeBDF, g.PCIeBDF != "" },
		Try:     HygonRuntimeSmokeOn,
	},
}

// RuntimeCheckKey is the one key every runtime check on this node runs under
// (RunTracked): the periodic per-card checks of every vendor, and Doctor's.
// They all open the same driver, so they take turns, and once one is stuck in
// it the node has exactly one such process until the driver lets it go.
const RuntimeCheckKey = "accelerator-runtime-check"

// runRuntimeCheck runs a runtime check process under RuntimeCheckKey with env
// as its environment (nil inherits the agent's).
func runRuntimeCheck(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	out, err := RunTracked(ctx, RuntimeCheckKey, 0, func(ctx context.Context) *exec.Cmd {
		// On the host when the agent runs the vendor's CLIs there: python3
		// is not in the agent's image, and the runtime libraries are the
		// host's. The environment (visible devices, library paths) passes
		// through nsenter to the program.
		cmd := cliCommand(ctx, name, args...)
		cmd.Env = env
		return cmd
	})
	return []byte(out), err
}

// stillRunning is what a check refused because an earlier one is stuck says:
// the host's problem, not the card's.
func stillRunning(err error) error {
	if errors.Is(err, ErrStillRunning) {
		return &HostRuntimeUnavailableError{Reason: "an earlier runtime check on this node was killed and has not exited: " + err.Error()}
	}
	return nil
}

// HostRuntimeUnavailableError reports that a check never reached the card:
// the vendor runtime would not load or initialize, the device mapping could
// not be read, or the answer could not be graded. It is not a verdict about
// the card and must not be recorded as one.
type HostRuntimeUnavailableError struct{ Reason string }

func (e *HostRuntimeUnavailableError) Error() string { return e.Reason }

// HostRuntimeUnavailable returns why a check never reached the card, or "" for
// an error that is a verdict about the card.
func HostRuntimeUnavailable(err error) string {
	var unavailable *HostRuntimeUnavailableError
	if errors.As(err, &unavailable) {
		return unavailable.Reason
	}
	return ""
}

// HostRuntimeCheckFor returns how to ask this vendor's runtime about a card,
// and false for a vendor that cannot be asked on the host.
func HostRuntimeCheckFor(vendor string) (HostRuntimeCheck, bool) {
	check, ok := hostRuntimeChecks[accelerator.NormalizeVendor(vendor)]
	return check, ok
}
