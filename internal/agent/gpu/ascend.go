package gpu

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

const (
	ascendInfoCacheTTL   = 5 * time.Second
	ascendInfoFailureTTL = 250 * time.Millisecond
)

var _ DriverAwareInjector = (*AscendProber)(nil)

type AscendProber struct {
	infoMu         sync.Mutex
	infoOut        string
	infoErr        error
	infoAt         time.Time
	infoTTL        time.Duration
	infoFailureTTL time.Duration
	runner         ascendCommandRunner

	// hccn runs hccn_tool; nil runs the host's. ports is the state kept
	// about the cards' network ports (ascend_ports.go).
	hccn     ascendCommandRunner
	ports    ascendCardPorts
	portsTTL time.Duration

	// hostRoot is where the host's /sys is read for the PCI roster; empty
	// means "/". A test sets it so the roster describes its fixture
	// rather than whatever is in the machine running the test.
	hostRoot string
}

var ascendFrameworkProbeMiss sync.Once

func (p *AscendProber) Vendor() string { return accelerator.VendorAscend }

// DetectRuntimePlugin reports the Ascend Docker Runtime, a Docker-only
// plugin; GPUWorks injects Ascend devices and driver mounts explicitly for
// nerdctl.
func (p *AscendProber) DetectRuntimePlugin(ctx context.Context, driver string) runtime.PluginInfo {
	if !strings.EqualFold(strings.TrimSpace(driver), "docker") {
		return runtime.PluginInfo{}
	}
	// The package name differs between Ascend releases. Keep the lookup
	// conservative and report the first executable's version when exposed.
	for _, binary := range []string{"ascend-docker-runtime", "ascend-container-runtime"} {
		if _, err := lookCLI(binary); err != nil {
			continue
		}
		out, err := cliCommand(ctx, binary, "--version").CombinedOutput()
		if err == nil {
			return runtime.PluginInfo{Name: accelerator.PluginAscendDockerRuntime, Version: runtime.FirstVersion(string(out)), Installed: true}
		}
		return runtime.PluginInfo{Name: accelerator.PluginAscendDockerRuntime}
	}
	// Some Ascend packages install the runtime outside PATH and only expose
	// the registration through Docker's runtime list. This still proves the
	// plugin is installed, although its package version is unavailable.
	if out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{json .Runtimes}}").CombinedOutput(); err == nil && strings.Contains(strings.ToLower(string(out)), "ascend") {
		return runtime.PluginInfo{Name: accelerator.PluginAscendDockerRuntime, Installed: true}
	}
	return runtime.PluginInfo{Name: accelerator.PluginAscendDockerRuntime}
}

func (p *AscendProber) Available() bool {
	if _, err := lookCLI("npu-smi"); err != nil && p.runner == nil {
		return false
	}
	_, err := p.info(context.Background())
	return err == nil
}

func (p *AscendProber) DriverVersion(ctx context.Context) string {
	out, err := p.info(ctx)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(strings.ToLower(line), "npu-smi") {
			// npu-smi pads its banner out to the table width:
			// "| npu-smi 26.1.1      Version: 26.1.1        |".
			// Collapsing the runs keeps it a version string rather than a
			// column of spaces, which is what the node's driver field and
			// the doctor report show.
			return strings.Join(strings.Fields(strings.Trim(line, "| ")), " ")
		}
	}
	return ""
}

func (p *AscendProber) Framework(ctx context.Context) (string, string) {
	// CANN toolkit publishes version.info/install metadata independently of
	// npu-smi. Prefer that signal; a driver-only host intentionally reports an
	// empty framework version so catalog selection can show “未检测到”.
	paths := []string{
		"/usr/local/Ascend/ascend-toolkit/latest/version.info",
		"/usr/local/Ascend/ascend-toolkit/latest/aarch64-linux/ascend_toolkit_install.info",
		"/usr/local/Ascend/ascend-toolkit/latest/x86_64-linux/ascend_toolkit_install.info",
		"/usr/local/Ascend/ascend-toolkit/latest/arm64-linux/ascend_toolkit_install.info",
	}
	if home := strings.TrimSpace(os.Getenv("ASCEND_HOME_PATH")); home != "" {
		paths = append([]string{
			filepath.Join(home, "version.info"),
			filepath.Join(home, "ascend_toolkit_install.info"),
		}, paths...)
	}
	for _, pattern := range []string{
		"/usr/local/Ascend/cann-*/version.info",
		"/usr/local/Ascend/cann-*/aarch64-linux/ascend_toolkit_install.info",
		"/usr/local/Ascend/cann-*/x86_64-linux/ascend_toolkit_install.info",
	} {
		paths = append(paths, globTool(pattern)...)
	}
	for _, path := range paths {
		if raw, err := readTool(path); err == nil {
			if version := parseCANNFrameworkVersion(string(raw)); version != "" {
				return "cann", version
			}
		}
	}
	// Newer npu-smi releases expose richer info subcommands but driver-only
	// hosts still do not necessarily carry a CANN version. Probe quietly and
	// never invoke the obsolete `npu-smi version.info` pseudo-command (26.x
	// exits 211 and previously produced a warning every status interval).
	for _, args := range [][]string{{"info", "-t", "version"}, {"info", "-t", "common"}} {
		if out, err := runNPUQuiet(ctx, args...); err == nil {
			if version := parseCANNFrameworkVersion(out); version != "" {
				return "cann", version
			}
		}
	}
	ascendFrameworkProbeMiss.Do(func() {
		slog.Debug("CANN toolkit version unavailable; treating host as driver-only", "vendor", accelerator.VendorAscend)
	})
	return "cann", ""
}

func parseCANNFrameworkVersion(text string) string {
	for _, pattern := range []string{
		`(?im)CANN(?:\s+Version)?\s*[:=]\s*([0-9]+(?:\.[0-9]+){1,2})`,
		`(?im)version\s*[:=]\s*([0-9]+(?:\.[0-9]+){1,2})`,
	} {
		if version := parseFrameworkVersion(text, pattern); version != "" {
			return version
		}
	}
	return ""
}

func (p *AscendProber) Probe(ctx context.Context) ([]proto.GPUInfo, error) {
	if _, err := lookCLI("npu-smi"); err != nil && p.runner == nil {
		return p.roster(nil, false), nil
	}
	out, err := p.info(ctx)
	if err != nil {
		// npu-smi could not be asked (on 198.51.100.31 it answers "dcmi
		// module initialize failed"), so nothing is known about any card.
		// The eight 910B4 in the machine are still a fact.
		return p.roster(nil, false), err
	}
	chips := parseAscendChips(out)
	enrichAscendHealthDetails(ctx, chips, p.command)
	gpus := make([]proto.GPUInfo, len(chips))
	for i, c := range chips {
		gpus[i] = c.gpu
	}
	return p.roster(gpus, true), nil
}

// roster places what npu-smi listed on the roster of what the host has.
func (p *AscendProber) roster(listed []proto.GPUInfo, answered bool) []proto.GPUInfo {
	return Roster(accelerator.VendorAscend, p.hostRoot, listed, answered)
}

// chipAddress is how npu-smi addresses the chip a device index names, read
// from the card table (ascendChipAddress).
func (p *AscendProber) chipAddress(ctx context.Context, index int) (npu, chip int) {
	out, err := p.info(ctx)
	if err != nil {
		return index, 0
	}
	return ascendChipAddress(out, index)
}

func (p *AscendProber) Topology(ctx context.Context) string {
	out, err := p.command(ctx, "info", "-t", "topo")
	if err != nil {
		return ""
	}
	return out
}

func (p *AscendProber) Metrics(ctx context.Context) ([]Metric, error) {
	out, err := p.info(ctx)
	if err != nil {
		return nil, err
	}
	return parseAscendMetrics(out), nil
}

func (p *AscendProber) info(ctx context.Context) (string, error) {
	p.infoMu.Lock()
	defer p.infoMu.Unlock()
	ttl := p.infoTTL
	if ttl <= 0 {
		ttl = ascendInfoCacheTTL
	}
	if p.infoErr != nil {
		ttl = p.infoFailureTTL
		if ttl <= 0 {
			ttl = ascendInfoFailureTTL
		}
	}
	if !p.infoAt.IsZero() && time.Since(p.infoAt) < ttl {
		return p.infoOut, p.infoErr
	}
	out, err := p.command(ctx, "info")
	p.infoOut, p.infoErr, p.infoAt = out, err, time.Now()
	return out, err
}

// command runs a read-only npu-smi query, which a slow driver may answer
// from the last result (see QueryVendorCLI). Commands that change state, and
// the reads that verify them, use runNPU.
func (p *AscendProber) command(ctx context.Context, args ...string) (string, error) {
	if p.runner != nil {
		return p.runner(ctx, args...)
	}
	return queryLogged(ctx, accelerator.VendorAscend, "npu-smi", args...)
}

func (p *AscendProber) RuntimeInjection(indexes []int) RuntimeInjection {
	if len(indexes) == 0 {
		return RuntimeInjection{}
	}
	idx := make([]string, len(indexes))
	for i, g := range indexes {
		idx[i] = strconv.Itoa(g)
	}
	hostDevices := strings.Join(idx, ",")
	containerDevices := compactVisibleDevices(len(indexes))
	return RuntimeInjection{
		Env: map[string]string{
			// Ascend Docker Runtime consumes host logical indexes and injects
			// only those devices. Torch-NPU then sees a compact 0..N-1 list.
			accelerator.AscendEnvVisibleDevices:   hostDevices,
			accelerator.AscendEnvRTVisibleDevices: containerDevices,
			accelerator.AscendEnvNPUVisible:       containerDevices,
		},
		// Ascend Docker Runtime is the preferred path on the 910B4 hosts. It
		// handles /dev/davinci* and driver mounts; per-device visibility is env.
		DockerArgs: []string{"--runtime", "ascend"},
	}
}

// ascendSharedDeviceNodes are the per-host device nodes an Ascend container
// needs beyond its own /dev/davinciN. Which of them a host has depends on its
// driver generation, so each is passed only when it is there: asking a
// runtime for a device node that does not exist fails the container outright.
// The davinciN nodes themselves are not optional: a card the scheduler chose
// has to be handed over or the launch is wrong.
var ascendSharedDeviceNodes = []string{
	"/dev/davinci_manager",
	"/dev/devmm_svm",
	"/dev/hisi_hdc",
	"/dev/ummu",
	"/dev/uburma",
}

// deviceNodeExists is the test seam for the check above; the offline suite
// must not depend on what the build host happens to have under /dev. A
// privileged container's own /dev is a copy made at container start, so a
// device node created later (the driver finishing its init after the agent
// started) is missing there; statTool checks the host's live /dev instead
// when CLIs run on it.
var deviceNodeExists = func(path string) bool {
	_, err := statTool(path)
	return err == nil
}

// ascendNerdctlDriverMounts are the host driver/DCMI paths that the Ascend
// Docker Runtime plugin normally injects implicitly. nerdctl/containerd has
// no equivalent plugin, so they must be bind-mounted explicitly. Real-machine
// verified on 198.51.100.32 (nerdctl 1.7.6, CANN driver 26.0.rc1): raw
// /dev/davinciN passthrough plus these five mounts is sufficient for
// torch.npu.is_available() to return true — no CANN toolkit mount is needed
// beyond what the inference image itself bakes in.
//
// npu-smi is mounted from wherever the host has it: driver packages put it in
// /usr/local/bin or /usr/local/sbin (openEuler 24.03, driver 25.5.1).
func ascendNerdctlDriverMounts() []RuntimeMount {
	smi := "/usr/local/bin/npu-smi"
	if p, err := lookCLI("npu-smi"); err == nil && filepath.IsAbs(p) {
		smi = p
	}
	return append([]RuntimeMount{{Source: smi, Target: smi, ReadOnly: true}}, ascendDriverMounts...)
}

var ascendDriverMounts = []RuntimeMount{
	{Source: "/usr/local/dcmi", Target: "/usr/local/dcmi", ReadOnly: true},
	{Source: "/usr/local/Ascend/driver/lib64", Target: "/usr/local/Ascend/driver/lib64", ReadOnly: true},
	{Source: "/usr/local/Ascend/driver/version.info", Target: "/usr/local/Ascend/driver/version.info", ReadOnly: true},
	{Source: "/etc/ascend_install.info", Target: "/etc/ascend_install.info", ReadOnly: true},
}

// RuntimeInjectionForDriver implements DriverAwareInjector. Every driver
// other than nerdctl keeps the device-injection shape from RuntimeInjection.
// Rootless execution resolves host logical IDs for ACL; Docker keeps compact
// container ordinals. The kube runtime hands the --device nodes to the pod
// through the agent's device plugin and the mounts as host paths, so it
// takes the nerdctl form too, without needing the runtime on the node.
// nerdctl has no Ascend Docker Runtime plugin
// (docs/dev/ledgers/engineering-log.md "Ascend device injection for the nerdctl runtime driver"), so
// it gets raw device passthrough plus explicit driver/DCMI mounts instead of
// --runtime ascend. Device paths are passed unrenamed
// (--device /dev/davinciN, not /dev/davinciN:/dev/davinci0) because renaming
// fails outright on this nerdctl/containerd combination — confirmed on
// 198.51.100.32.
func (p *AscendProber) RuntimeInjectionForDriver(indexes []int, driverName string) RuntimeInjection {
	base := p.RuntimeInjection(indexes)
	if driverName == "udocker" && len(indexes) > 0 {
		visible, err := p.rootlessVisibleDevices(indexes)
		if err != nil {
			return RuntimeInjection{UnsupportedReason: err.Error()}
		}
		base.Env[accelerator.AscendEnvRTVisibleDevices] = visible
		base.Env[accelerator.AscendEnvNPUVisible] = visible
		return base
	}
	if (driverName != "nerdctl" && driverName != "kube") || len(indexes) == 0 {
		return base
	}
	containerDevices := compactVisibleDevices(len(indexes))
	args := make([]string, 0, len(indexes)*2+len(ascendSharedDeviceNodes)*2)
	for _, idx := range indexes {
		args = append(args, "--device", fmt.Sprintf("/dev/davinci%d", idx))
	}
	for _, node := range ascendSharedDeviceNodes {
		if deviceNodeExists(node) {
			args = append(args, "--device", node)
		}
	}
	return RuntimeInjection{
		Env: map[string]string{
			// No Ascend Docker Runtime plugin reads ASCEND_VISIBLE_DEVICES
			// here; the exact host devices are already chosen by the --device
			// args above, so it is omitted rather than set to a value nothing
			// consumes. ASCEND_RT_VISIBLE_DEVICES/NPU_VISIBLE_DEVICES are read
			// directly by the ACL/torch_npu userspace runtime independent of
			// the container runtime, so they still apply here.
			accelerator.AscendEnvRTVisibleDevices: containerDevices,
			accelerator.AscendEnvNPUVisible:       containerDevices,
		},
		DockerArgs: args,
		Mounts:     ascendNerdctlDriverMounts(),
	}
}

// EnsureDeviceShare makes the host driver state required for more than one
// logical container on a chip explicit and idempotent. Query and set address
// the chip the device index names (-i <npu> -c <chip>): chip 0 of the card on
// one-chip cards, the chip itself on multi-chip ones.
func (p *AscendProber) EnsureDeviceShare(ctx context.Context, index int) (bool, error) {
	npu, chip := p.chipAddress(ctx, index)
	target := []string{"-t", "device-share", "-i", strconv.Itoa(npu), "-c", strconv.Itoa(chip)}
	manual := fmt.Sprintf("npu-smi set -t device-share -i %d -c %d -d 1", npu, chip)
	out, err := runNPU(ctx, append([]string{"info"}, target...)...)
	if err == nil && strings.Contains(strings.ToLower(out), "true") {
		return false, nil
	}
	setOut, setErr := runNPU(ctx, append(append([]string{"set"}, target...), "-d", "1")...)
	if setErr != nil {
		return false, fmt.Errorf("enable Ascend device-share for card %d: %w%s; run: %s", index, setErr, npuOutputDetail(setOut), manual)
	}
	verified, verifyErr := runNPU(ctx, append([]string{"info"}, target...)...)
	if verifyErr != nil || !strings.Contains(strings.ToLower(verified), "true") {
		return false, fmt.Errorf("Ascend device-share for card %d did not become enabled; run: %s", index, manual)
	}
	return true, nil
}

func npuOutputDetail(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	const limit = 400
	if len(out) > limit {
		out = "..." + out[len(out)-limit:]
	}
	return ": " + strings.Join(strings.Fields(out), " ")
}

// PartitionHardwareID returns the VDie ID format required by vcann-rt's
// shared-memory ledger. Some driver-only 910B4 hosts reject the board query;
// callers deliberately fall back to a stable physical-card identifier there.
// The query addresses the chip the device index names, whose VDie differs
// from its sibling's on a multi-chip card.
func (p *AscendProber) PartitionHardwareID(ctx context.Context, index int) string {
	npu, chip := p.chipAddress(ctx, index)
	out, err := runNPUQuiet(ctx, "info", "-t", "board", "-i", strconv.Itoa(npu), "-c", strconv.Itoa(chip))
	if err != nil {
		return ""
	}
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(strings.Trim(raw, "|"))
		key, value, ok := strings.Cut(line, ":")
		if !ok || !strings.Contains(strings.ToLower(key), "vdie") {
			continue
		}
		return strings.Join(strings.Fields(strings.TrimSpace(value)), "-")
	}
	return ""
}

func compactVisibleDevices(count int) string {
	values := make([]string, count)
	for i := range values {
		values[i] = strconv.Itoa(i)
	}
	return strings.Join(values, ",")
}

func runNPU(ctx context.Context, args ...string) (string, error) {
	out, err := runNPUQuiet(ctx, args...)
	if err != nil {
		slog.Warn("gpu command failed", "vendor", accelerator.VendorAscend, "args", args, "err", err)
	}
	return out, err
}

func runNPUQuiet(ctx context.Context, args ...string) (string, error) {
	slog.Debug("gpu command", "vendor", accelerator.VendorAscend, "args", args)
	var stdin io.Reader
	if len(args) > 0 && args[0] == "set" {
		// npu-smi 26.x prompts "Are you sure ... (Y/N)" for persistent
		// device-share mutations and exits 200 on EOF. Supplying confirmation
		// here preserves direct argv execution without involving a shell.
		stdin = strings.NewReader("Y\n")
	}
	return RunVendorCLI(ctx, stdin, "npu-smi", args...)
}
