package gpu

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	metaxSmokeOKMarker          = "MACA_RUNTIME_SMOKE_OK"
	metaxSmokeDieMarker         = "MACA_RUNTIME_SMOKE_DIE_FAILED:"
	metaxSmokeUnavailableMarker = "MACA_RUNTIME_SMOKE_UNAVAILABLE:"
)

var metaxSmokeCommand = runMetaXSmokeCommand

// MetaXRuntimeSmokeOn checks whether MACA can use the card identified by its
// PCI address. Current smoke script failures do not establish that a device
// itself is faulty, so only an explicit successful completion passes.
func MetaXRuntimeSmokeOn(ctx context.Context, bdf string) error {
	if strings.TrimSpace(bdf) == "" {
		return &HostRuntimeUnavailableError{Reason: "the MACA runtime check cannot identify the card: no PCI address supplied"}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := metaxSmokeCommand(probeCtx, bdf)
	return metaXSmokeVerdict(out, err)
}

// MetaXSmokePrerequisite reports whether the host has Python and a MACA
// runtime library in one of the layouts currently supported by the script.
func MetaXSmokePrerequisite() string {
	if _, err := lookCLI("python3"); err != nil {
		return "the MACA runtime check needs python3, which is not installed on this node"
	}
	if _, _, ok := metaXSmokeLibrary(); !ok {
		return "the MACA runtime check needs libmcruntime.so in a supported MACA library directory"
	}
	return ""
}

func runMetaXSmokeCommand(ctx context.Context, bdf string) ([]byte, error) {
	env := environmentWith([]string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}, map[string]string{"MACA_SMOKE_BDF": bdf})
	if dir, library, ok := metaXSmokeLibrary(); ok {
		env = environmentWith(env, map[string]string{"LD_LIBRARY_PATH": dir, "MACA_SMOKE_LIBRARY": library})
	}
	return runRuntimeCheck(ctx, env, "python3", "-c", metaxHostSmokeScript)
}

// metaXSmokeLibrary mirrors the script's known layouts. It intentionally
// does not derive version-specific paths or search arbitrary directories.
func metaXSmokeLibrary() (string, string, bool) {
	paths := make([]string, 0, 4)
	if root := strings.TrimSpace(os.Getenv("MACA_PATH")); root != "" {
		paths = append(paths, filepath.Join(root, "lib", "libmcruntime.so"))
	}
	paths = append(paths,
		"/opt/maca/lib/libmcruntime.so",
		"/opt/maca/lib64/libmcruntime.so",
		"/usr/local/maca/lib/libmcruntime.so",
	)
	for _, path := range paths {
		if _, err := statTool(path); err == nil {
			return filepath.Dir(path), path, true
		}
	}
	return "", "", false
}

func metaXSmokeVerdict(out []byte, err error) error {
	if refused := stillRunning(err); refused != nil {
		return refused
	}
	text := string(out)
	if detail, ok := metaXSmokeMarkerDetail(text, metaxSmokeUnavailableMarker); ok {
		return &HostRuntimeUnavailableError{Reason: "the MACA runtime could not be used on this host: " + metaXSmokeDetail(detail, err)}
	}
	if detail, ok := metaXSmokeMarkerDetail(text, metaxSmokeDieMarker); ok {
		// The script's marker includes mapping, device-selection, memory-query,
		// allocation and free errors. None proves a hardware-attributed fault.
		return &HostRuntimeUnavailableError{Reason: "the MACA runtime could not establish whether the card is usable: " + metaXSmokeDetail(detail, err)}
	}
	if err == nil && metaXSmokeHasLine(text, metaxSmokeOKMarker) {
		return nil
	}
	detail := metaXSmokeDetail(strings.TrimSpace(text), err)
	return &HostRuntimeUnavailableError{Reason: "the MACA runtime check returned no recognized result: " + detail}
}

func metaXSmokeDetail(detail string, err error) string {
	if detail != "" {
		return detail
	}
	if err != nil {
		return err.Error()
	}
	return "the check printed nothing"
}

func metaXSmokeMarkerDetail(out, marker string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if detail, found := strings.CutPrefix(line, marker); found {
			return strings.TrimSpace(detail), true
		}
	}
	return "", false
}

func metaXSmokeHasLine(out, marker string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == marker {
			return true
		}
	}
	return false
}
