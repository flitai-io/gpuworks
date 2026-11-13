package gpu

import (
	"context"
	"fmt"
	"os"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// These indirections are the test seams: the offline suite never starts a
// process or assumes the test host has DTK.
var (
	hygonSmokeLibraryFinder = findHygonHIPLibrary
	hygonSmokeCommand       = runHygonSmokeCommand
)

const (
	hygonSmokeOKMarker          = "HYGON_HIP_SMOKE_OK"
	hygonSmokeCardMarker        = "HYGON_HIP_SMOKE_CARD_FAILED:"
	hygonSmokeUnavailableMarker = "HYGON_HIP_SMOKE_UNAVAILABLE:"
)

// HygonRuntimeSmokeOn asks DTK's HIP runtime to take the card at bdf and make
// a small allocation on it (accelerator.HygonHIPSmokeScript).
func HygonRuntimeSmokeOn(ctx context.Context, bdf string) error {
	library, ok := hygonSmokeLibraryFinder()
	if !ok {
		return &HostRuntimeUnavailableError{Reason: "the Hygon HIP runtime could not be used on this host: libamdhip64.so from a DTK installation is not installed on the host"}
	}
	out, err := hygonSmokeCommand(ctx, library, bdf)
	return hygonSmokeVerdict(out, err)
}

// HygonHIPSmokePrerequisite says why the per-card HIP check cannot run, or
// "". A host with the driver but without DTK keeps hy-smi's verdict rather
// than being reported as a failed card.
func HygonHIPSmokePrerequisite() string {
	if _, err := lookCLI("python3"); err != nil {
		return "the Hygon runtime check needs python3, which is not installed on this node"
	}
	if _, ok := hygonSmokeLibraryFinder(); !ok {
		return "the Hygon runtime check needs libamdhip64.so from a host DTK installation"
	}
	return ""
}

func findHygonHIPLibrary() (string, bool) {
	for _, path := range accelerator.HygonHIPLibraryPaths {
		if _, err := statTool(path); err == nil {
			return path, true
		}
	}
	return "", false
}

func runHygonSmokeCommand(ctx context.Context, library, bdf string) ([]byte, error) {
	return runRuntimeCheck(ctx, hygonSmokeEnv(os.Environ(), library, bdf), "python3", "-c", accelerator.HygonHIPSmokeScript)
}

// hygonSmokeEnv is the environment the HIP check runs in. The agent's own
// device visibility, if it has any, is removed: the script finds the card by
// its address among every card, and a visibility list inherited from the
// agent would hide the very card it was asked about.
func hygonSmokeEnv(base []string, library, bdf string) []string {
	var env []string
	for _, kv := range base {
		switch strings.SplitN(kv, "=", 2)[0] {
		case "HIP_VISIBLE_DEVICES", "ROCR_VISIBLE_DEVICES", "CUDA_VISIBLE_DEVICES", "LD_LIBRARY_PATH":
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"HYGON_HIP_LIBRARY="+library,
		"HYGON_SMOKE_BDF="+bdf,
		"LD_LIBRARY_PATH="+strings.Join(accelerator.HygonHIPLibraryDirs, ":"),
	)
}

// hygonSmokeVerdict grades the attempt's output: the card's failure is a
// verdict, the host's is not, and an attempt without the success marker never
// passes.
func hygonSmokeVerdict(out []byte, err error) error {
	if refused := stillRunning(err); refused != nil {
		return refused
	}
	text := string(out)
	if detail, ok := ascendSmokeMarkerDetail(text, hygonSmokeUnavailableMarker); ok {
		return &HostRuntimeUnavailableError{Reason: "the Hygon HIP runtime could not be used on this host: " + detail}
	}
	if detail, ok := ascendSmokeMarkerDetail(text, hygonSmokeCardMarker); ok {
		return fmt.Errorf("hygon hip runtime smoke failed: %s", detail)
	}
	if err == nil && strings.Contains(text, hygonSmokeOKMarker) {
		return nil
	}
	detail := strings.TrimSpace(text)
	if detail == "" && err != nil {
		detail = err.Error()
	}
	// No marker at all: python itself did not run the script to an answer.
	return &HostRuntimeUnavailableError{Reason: "the Hygon HIP runtime check gave no answer: " + detail}
}
