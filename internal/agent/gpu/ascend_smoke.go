package gpu

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// ascendSmokeTarget is the host CANN installation used to create an ACL
// context. The library can live under lib64 or a target-specific arch
// directory, depending on how the toolkit was installed.
type ascendSmokeTarget struct {
	Home    string
	Library string
}

// These indirections are the test seams: the offline suite never starts a
// process or assumes the test host has CANN unless it builds the real stub.
var (
	ascendSmokeTargetFinder  = findAscendSmokeTarget
	ascendSmokeCommand       = runAscendSmokeCommand
	ascendSmokeLogicalDevice = ascendLogicalDeviceForSmoke
)

// AscendRuntimeSmokeOn asks CANN's ACL runtime to create a context and make a
// small allocation on one physical die. The driver's physical-to-logical
// mapping selects exactly that die even when IDs are sparse or remapped by
// passthrough. ASCEND_RT_VISIBLE_DEVICES then remaps the logical die to zero
// inside the child, so the script always addresses device zero.
func AscendRuntimeSmokeOn(ctx context.Context, physical string) error {
	visible, err := ascendSmokeLogicalDevice(physical)
	if err != nil {
		return &HostRuntimeUnavailableError{Reason: "the Ascend ACL runtime could not identify the die on this host: " + err.Error()}
	}
	target, ok := ascendSmokeTargetFinder()
	if !ok {
		return &HostRuntimeUnavailableError{Reason: "the Ascend ACL runtime could not be used on this host: CANN ACL runtime is not installed on the host"}
	}
	out, err := ascendSmokeCommand(ctx, visible, target)
	return ascendSmokeVerdict(out, err)
}

func ascendLogicalDeviceForSmoke(physical string) (string, error) {
	index, err := strconv.Atoi(physical)
	if err != nil || index < 0 {
		return "", fmt.Errorf("invalid physical die index %q", physical)
	}
	return (&AscendProber{}).rootlessVisibleDevices([]int{index})
}

// AscendCLSmokePrerequisite says why the per-card ACL check cannot run, or
// "". A driver-only host has npu-smi but no libascendcl, so it must keep the
// vendor CLI's verdict rather than be reported as a failed card.
func AscendCLSmokePrerequisite() string {
	if _, err := lookCLI("python3"); err != nil {
		return "the Ascend runtime check needs python3, which is not installed on this node"
	}
	if _, ok := ascendSmokeTargetFinder(); !ok {
		return "the Ascend runtime check needs libascendcl.so from a host CANN installation"
	}
	return ""
}

func runAscendSmokeCommand(ctx context.Context, visible string, target ascendSmokeTarget) ([]byte, error) {
	return runRuntimeCheck(ctx, ascendSmokeEnv(os.Environ(), visible, target), "python3", "-c", accelerator.AscendACLSmokeScript)
}

// ascendSmokeEnv is the environment the ACL check runs in. Only the one die
// named by ASCEND_RT_VISIBLE_DEVICES is visible to the child, which then
// addresses it as device zero, so the check cannot drift onto another die.
//
// The library path is built rather than appended to: an empty element in
// LD_LIBRARY_PATH means the current directory to the dynamic linker, and the
// agent runs as root from a directory it does not control, so a trailing
// colon would have the child search there.
func ascendSmokeEnv(base []string, visible string, target ascendSmokeTarget) []string {
	paths := []string{filepath.Join(target.Home, "lib64"), filepath.Dir(target.Library)}
	// libascendcl resolves against the driver's own libraries, which a host
	// whose ld.so.conf does not already name them cannot find. Without them
	// the library fails to load and every die would answer the same way, so
	// the check would read as a host of broken cards.
	paths = append(paths, strings.Split(accelerator.AscendDriverLibraryPath, ":")...)
	for _, entry := range base {
		if name, value, _ := strings.Cut(entry, "="); name == "LD_LIBRARY_PATH" {
			paths = append(paths, strings.Split(value, ":")...)
		}
	}
	seen := make(map[string]struct{}, len(paths))
	kept := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		kept = append(kept, path)
	}
	return environmentWith(base, map[string]string{
		accelerator.AscendEnvRTVisibleDevices: visible,
		"ASCEND_HOME_PATH":                    target.Home,
		"ASCEND_ACL_LIBRARY":                  target.Library,
		"LD_LIBRARY_PATH":                     strings.Join(kept, ":"),
	})
}

// findAscendSmokeTarget looks where accelerator.AscendToolkitHomes says, in
// that order, so this and the command a person is shown
// (accelerator.AscendRuntimeSmokeCommand) cannot settle on different
// installations.
func findAscendSmokeTarget() (ascendSmokeTarget, bool) {
	var homes []string
	if home := strings.TrimSpace(os.Getenv("ASCEND_HOME_PATH")); home != "" {
		homes = append(homes, home)
	}
	for _, candidate := range accelerator.AscendToolkitHomes {
		if !strings.ContainsRune(candidate, '*') {
			homes = append(homes, candidate)
			continue
		}
		homes = append(homes, globTool(candidate)...)
	}
	for _, home := range homes {
		for _, relative := range accelerator.AscendACLLibraryPaths {
			library := filepath.Join(home, relative)
			if info, err := statTool(library); err == nil && !info.IsDir() {
				return ascendSmokeTarget{Home: home, Library: library}, true
			}
		}
	}
	return ascendSmokeTarget{}, false
}

// The markers the check reports itself with. Only the die marker blames the
// die that was being asked about; the script decides which one a failure
// earns, because only it knows whether the failure came before or after the
// die was named.
const (
	ascendSmokeOKMarker          = "ASCEND_ACL_SMOKE_OK"
	ascendSmokeDieMarker         = "ASCEND_ACL_SMOKE_DIE_FAILED:"
	ascendSmokeUnavailableMarker = "ASCEND_ACL_SMOKE_UNAVAILABLE:"
)

// ascendSmokeVerdict grades what the check printed. A die is only reported as
// unusable when the check says in so many words that it was the die that
// failed. The success marker passes the die whatever the exit status. A host whose CANN cannot be loaded or initialized, a crash, or
// output this agent does not recognize is not evidence about the card.
func ascendSmokeVerdict(out []byte, err error) error {
	if refused := stillRunning(err); refused != nil {
		return refused
	}
	text := string(out)
	if detail, ok := ascendSmokeMarkerDetail(text, ascendSmokeUnavailableMarker); ok {
		return &HostRuntimeUnavailableError{Reason: "the Ascend ACL runtime could not be used on this host: " + detail}
	}
	if detail, ok := ascendSmokeMarkerDetail(text, ascendSmokeDieMarker); ok {
		return fmt.Errorf("Ascend ACL runtime smoke failed: %s", detail)
	}
	if strings.Contains(text, ascendSmokeOKMarker) {
		// The marker is printed only after the die was taken and given back,
		// so the question is answered. A non-zero exit after it — CANN has
		// been seen to crash in its atexit handlers — is the runtime's
		// teardown, not the die's.
		if err != nil {
			slog.Info("Ascend ACL runtime check passed but the process exited abnormally", "exit", err.Error())
		}
		return nil
	}
	detail := strings.TrimSpace(text)
	if detail == "" && err != nil {
		detail = err.Error()
	}
	if detail == "" {
		detail = "the check printed nothing"
	}
	return &HostRuntimeUnavailableError{
		Reason: "the Ascend ACL runtime check did not reach the die: " + detail,
	}
}

// ascendSmokeMarkerDetail returns what the named marker reported, and whether
// it was there at all. The marker is matched on its own line so a detail that
// quotes another marker cannot be mistaken for one.
func ascendSmokeMarkerDetail(out, marker string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if detail, found := strings.CutPrefix(line, marker); found {
			return strings.TrimSpace(detail), true
		}
	}
	return "", false
}

func environmentWith(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[name]; !replaced {
			out = append(out, entry)
		}
	}
	for name, value := range overrides {
		out = append(out, name+"="+value)
	}
	return out
}
