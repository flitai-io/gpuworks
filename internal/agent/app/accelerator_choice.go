package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// A node manages one vendor's cards; on a host with cards of more than one,
// the operator chooses which (proto.AcceleratorPreferenceAuto leaves it to
// gpu.New's order). Everything vendor-specific — the prober, device
// injection, metrics, partition probes, the domestic runtime assets, the
// runtime's ability to give containers the cards — is built once when the
// agent starts, so a change of vendor restarts the agent: it execs itself,
// and the new process reads the choice from its state directory before it
// probes anything. Reading it there rather than waiting for the server
// means the first report after any restart already lists the right cards.

// acceleratorPreferenceFile holds the choice in the state directory; absent
// means automatic.
const acceleratorPreferenceFile = "accelerator-preference"

// acceleratorRestartDelay lets the RPC reply reach the server before the
// process is replaced.
var acceleratorRestartDelay = time.Second

// errAcceleratorBusy refuses a switch while the node runs something: its
// cards would vanish from under it.
var errAcceleratorBusy = errors.New("instances are running on this node; stop them before switching accelerators")

type acceleratorChoice struct {
	mu sync.Mutex
	// preference is the choice this process was started with.
	preference string
	// census lists the vendors on the host's bus; nil when the agent was
	// given its prober (a test) and the host's bus is none of its business.
	census AcceleratorHost
	// restart replaces the process; reexecAgent unless a test observes it.
	restart func() error
	// switching is set once a restart is scheduled, so a second request
	// cannot schedule another.
	switching bool
	// deferred is the server's differing choice last left waiting because
	// the node was busy, so that is logged once rather than every sync.
	deferred string
}

// AcceleratorHost is a host's cards by vendor: the prober for the chosen
// vendor's, and every vendor's for the operator to choose from.
type AcceleratorHost interface {
	ProberFor(preference string) gpu.Prober
	Vendors() []proto.AcceleratorPresence
}

// realAcceleratorHost is the machine the agent runs on.
type realAcceleratorHost struct{ census *gpu.VendorCensus }

func (h realAcceleratorHost) ProberFor(preference string) gpu.Prober { return gpu.NewFor(preference) }
func (h realAcceleratorHost) Vendors() []proto.AcceleratorPresence   { return h.census.Vendors() }

// normalizeAcceleratorPreference is the stored spelling of a choice, or ""
// when it is not one: empty means automatic.
func normalizeAcceleratorPreference(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch {
	case value == "" || value == proto.AcceleratorPreferenceAuto:
		return proto.AcceleratorPreferenceAuto
	case gpu.ChoosableVendor(value):
		return value
	default:
		return ""
	}
}

// readAcceleratorPreference is the choice stored in stateDir; a missing,
// unreadable or unknown one is automatic.
func readAcceleratorPreference(stateDir string) string {
	raw, err := os.ReadFile(filepath.Join(stateDir, acceleratorPreferenceFile))
	if err != nil {
		return proto.AcceleratorPreferenceAuto
	}
	if preference := normalizeAcceleratorPreference(string(raw)); preference != "" {
		return preference
	}
	return proto.AcceleratorPreferenceAuto
}

// ClearAcceleratorPreference resets a node's saved accelerator choice to
// automatic. A Kubernetes node's preference file is keyed by state
// directory (a hostPath), not by cluster entry: reusing the same directory
// under another entry (a reinstall, or the node moved to a different
// entry) must not carry over the old entry's choice. main.go calls this
// when a kube enrollment's identity gets a new node UUID for this state
// directory.
func ClearAcceleratorPreference(stateDir string) error {
	return writeAcceleratorPreference(stateDir, proto.AcceleratorPreferenceAuto)
}

// writeAcceleratorPreference stores a choice for the next start: automatic
// removes the file, anything else replaces it in one rename.
func writeAcceleratorPreference(stateDir, preference string) error {
	path := filepath.Join(stateDir, acceleratorPreferenceFile)
	if preference == proto.AcceleratorPreferenceAuto {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(preference+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// reexecAgent replaces this process with a fresh start of the same binary
// and arguments, as an in-place upgrade does.
func reexecAgent() error {
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	// An upgrade leaves the binary it replaced as <bin>.old; a process
	// started from that must come back as the binary now at <bin>.
	for strings.HasSuffix(bin, ".old") {
		bin = strings.TrimSuffix(bin, ".old")
	}
	return syscall.Exec(bin, os.Args, os.Environ())
}

// AcceleratorVendors is every vendor whose cards are in the host, for the
// status report (SysInfo.AcceleratorVendors).
func (n *NodeRuntime) AcceleratorVendors() []proto.AcceleratorPresence {
	n.accelerator.mu.Lock()
	census := n.accelerator.census
	n.accelerator.mu.Unlock()
	if census == nil {
		return nil
	}
	return census.Vendors()
}

// SetAccelerator applies an operator's choice now: it refuses while the
// node runs anything, and otherwise stores the choice and restarts the
// agent to manage that vendor's cards. The same choice again does nothing.
func (n *NodeRuntime) SetAccelerator(preference string) (string, error) {
	return n.switchAccelerator(preference, false)
}

// applyAcceleratorPreference brings a replayed choice into effect: one that
// differs from what this process runs with switches only while nothing
// runs on the node, and otherwise waits for a later sync.
func (n *NodeRuntime) applyAcceleratorPreference(preference string) {
	if strings.TrimSpace(preference) == "" {
		return
	}
	if _, err := n.switchAccelerator(preference, true); err != nil {
		n.log.Warn("accelerator choice could not be applied", "preference", preference, "err", err)
	}
}

func (n *NodeRuntime) switchAccelerator(requested string, replay bool) (string, error) {
	preference := normalizeAcceleratorPreference(requested)
	if preference == "" {
		return "", fmt.Errorf("accelerator must be %s or one of nvidia, ascend, hygon, ppu (got %q)", proto.AcceleratorPreferenceAuto, requested)
	}
	choice := &n.accelerator
	choice.mu.Lock()
	defer choice.mu.Unlock()
	if preference == choice.preference || choice.switching {
		choice.deferred = ""
		return preference, nil
	}
	if n.engine != nil && !n.engine.Idle() {
		if !replay {
			return "", errAcceleratorBusy
		}
		if choice.deferred != preference {
			choice.deferred = preference
			n.log.Info("accelerator choice waits until nothing runs on the node", "current", choice.preference, "chosen", preference)
		}
		return preference, nil
	}
	if err := writeAcceleratorPreference(n.stateDir, preference); err != nil {
		return "", fmt.Errorf("store accelerator choice: %w", err)
	}
	restart := choice.restart
	if restart == nil {
		restart = reexecAgent
	}
	choice.switching = true
	n.log.Info("restarting to manage another accelerator", "from", choice.preference, "to", preference)
	time.AfterFunc(acceleratorRestartDelay, func() {
		if err := restart(); err != nil {
			n.log.Error("agent restart for the accelerator choice failed; it applies on the next start", "preference", preference, "err", err)
			choice.mu.Lock()
			choice.switching = false
			choice.mu.Unlock()
		}
	})
	return preference, nil
}

// handleSetAcceleratorRPC is the node.set_accelerator RPC.
func (n *NodeRuntime) handleSetAcceleratorRPC(params []byte) (any, error) {
	var req struct {
		Accelerator string `json:"accelerator"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, fmt.Errorf("decode accelerator request: %w", err)
	}
	preference, err := n.SetAccelerator(req.Accelerator)
	if err != nil {
		return nil, err
	}
	return map[string]string{"accelerator": preference}, nil
}
