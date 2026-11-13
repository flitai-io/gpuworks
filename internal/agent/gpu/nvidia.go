// NVIDIA detection via the nvidia-smi CLI (no NVML cgo, per plan). The
// implementation degrades gracefully: on a host with no driver it returns an
// empty slice and no error, so a CPU-only dev box or a not-yet-provisioned
// node still reports cleanly. Pure output parsing lives in nvidia_parse.go;
// this file is the SMIProber itself — command dispatch, caching, state.
package gpu

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// SMIProber implements Prober via the nvidia-smi CLI.
type SMIProber struct {
	runner nvidiaCommandRunner
	// hostRoot is where /proc, /sys and /dev are read; empty means "/".
	// Tests point it at a fixture tree.
	hostRoot string

	driverMu sync.RWMutex
	driver   string

	frameworkMu     sync.Mutex
	framework       string
	frameworkDriver string // the driver version framework was read under
	frameworkAt     time.Time
	frameworkTTL    time.Duration

	layout nvidiaLayout
	nvlink nvlinkState

	// inventoryTier is the first of nvidiaInventoryQueries this driver
	// answered, found by a search that started at the top at
	// inventoryFullAt.
	inventoryMu     sync.Mutex
	inventoryTier   int
	inventoryFullAt time.Time

	// metricsTierMu/metricsTier/lastFullProbeAt cache which --query-gpu tier
	// (see metricsQueryTiers) this driver actually accepts, the same pattern
	// DriverVersion uses to cache the driver string. Without this, a driver
	// that rejects the newest fields would pay 2-3 nvidia-smi processes on
	// every single 10s tick forever instead of one.
	//
	// lastFullProbeAt is deliberately a separate field from "the last time
	// Metrics succeeded": it is only set when a call actually started its
	// search at tier 0 (a "full probe"). Stamping it on every success instead
	// would keep pushing metricsTierRecheckTTL out on every single tick
	// forever, since a cached-tier success is itself a success — and then a
	// driver upgrade that unlocks a higher tier would never be picked up.
	metricsTierMu   sync.RWMutex
	metricsTier     int
	lastFullProbeAt time.Time

	// now is the clock metricsTierAt/lastFullProbeAt logic reads. Nil means
	// time.Now; tests inject a fake clock to exercise metricsTierRecheckTTL
	// without an actual 10-minute sleep.
	now func() time.Time
}

func (p *SMIProber) clockNow() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

type nvidiaCommandRunner func(context.Context, ...string) (string, error)

func (p *SMIProber) Vendor() string { return accelerator.VendorNVIDIA }

// DetectRuntimePlugin reports the NVIDIA Container Toolkit, the userspace
// integration consumed both by Docker's nvidia runtime and by nerdctl's
// --gpus support. Unlike Docker, nerdctl needs no daemon runtime
// registration, but the toolkit still has to be installed.
func (p *SMIProber) DetectRuntimePlugin(ctx context.Context, driver string) runtime.PluginInfo {
	if out, err := cliCommand(ctx, "nvidia-ctk", "--version").CombinedOutput(); err == nil {
		return runtime.PluginInfo{Name: accelerator.PluginNVIDIAContainerToolkit, Version: runtime.FirstVersion(string(out)), Installed: true}
	}
	if out, err := cliCommand(ctx, "nvidia-container-runtime", "--version").CombinedOutput(); err == nil {
		return runtime.PluginInfo{Name: accelerator.PluginNVIDIAContainerToolkit, Version: runtime.FirstVersion(string(out)), Installed: true}
	}
	return runtime.PluginInfo{Name: accelerator.PluginNVIDIAContainerToolkit}
}

func (p *SMIProber) Available() bool {
	if _, err := lookCLI("nvidia-smi"); err != nil && p.runner == nil {
		return false
	}
	_, err := p.query(context.Background(), "--query-gpu=index", "--format=csv,noheader,nounits")
	return err == nil
}

// Topology answers for the cards the last probe listed (see nvidiaLayout).
// Without an answer for them it returns "", and the server keeps the
// topology it has.
func (p *SMIProber) Topology(ctx context.Context) string {
	p.layout.mu.Lock()
	inventory := p.layout.inventory
	p.layout.mu.Unlock()
	out, _ := p.layoutQuery(ctx, &p.layout.topology, inventory, "topo", "-m")
	return out
}

// metricsQueryTiers is tried in order, cheapest-first-in-capability terms
// (most fields to fewest), so a driver is always given every field it can
// answer. R535+ renamed clocks_throttle_reasons.* to clocks_event_reasons.*;
// the two top tiers differ only in that one field's spelling so both the
// current and the deprecated-but-still-shipped name are tried before falling
// back to the pre-monitor-plan field set.
const (
	metricsQueryEventReasons    = "--query-gpu=index,uuid,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw,fan.speed,utilization.memory,clocks.sm,clocks.max.sm,clocks_event_reasons.active"
	metricsQueryThrottleReasons = "--query-gpu=index,uuid,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw,fan.speed,utilization.memory,clocks.sm,clocks.max.sm,clocks_throttle_reasons.active"
	metricsQueryExtended        = "--query-gpu=index,uuid,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw,fan.speed"
	metricsQueryBasic           = "--query-gpu=index,uuid,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw"
)

var metricsQueryTiers = [...]string{metricsQueryEventReasons, metricsQueryThrottleReasons, metricsQueryExtended, metricsQueryBasic}

// metricsTierRecheckTTL bounds how long a cached tier is trusted before
// Metrics tries the top tier again. A driver upgrade mid-lifetime (rare, but
// long-lived nodes exist) can only unlock a higher tier at this cadence, in
// exchange for never paying more than one extra probe burst per interval.
const metricsTierRecheckTTL = 10 * time.Minute

func (p *SMIProber) Metrics(ctx context.Context) ([]Metric, error) {
	start, fullProbe := p.cachedMetricsTier()
	var out string
	var err error
	for i := start; i < len(metricsQueryTiers); i++ {
		out, err = p.query(ctx, metricsQueryTiers[i], "--format=csv,noheader,nounits")
		if err == nil {
			p.rememberMetricsTier(i, fullProbe)
			break
		}
		// A slow host says nothing about which fields the driver accepts:
		// a smaller query would only queue behind the same stall, and
		// caching it would drop fields for metricsTierRecheckTTL.
		if IsCLIUnresponsive(err) || ctx.Err() != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	metrics := parseNVIDIAMetrics(out)
	if p.slow() || !p.pcieDue() {
		return metrics, nil
	}
	if dmon, dmonErr := p.query(ctx, "dmon", "-c", "1", "-s", "t"); dmonErr == nil {
		mergeNVIDIAPCIeMetrics(metrics, parseNVIDIADmon(dmon))
	}
	return metrics, nil
}

// cachedMetricsTier returns the tier index to start from — the last winning
// tier while its cache is fresh, or 0 (retry every tier from the top) the
// first time and again once metricsTierRecheckTTL has elapsed since the last
// full probe — and whether this call is itself a full probe (started at 0),
// which the caller must know to decide whether a success should reset the
// TTL clock.
func (p *SMIProber) cachedMetricsTier() (tier int, fullProbe bool) {
	p.metricsTierMu.RLock()
	defer p.metricsTierMu.RUnlock()
	if p.lastFullProbeAt.IsZero() || p.clockNow().Sub(p.lastFullProbeAt) >= metricsTierRecheckTTL {
		return 0, true
	}
	return p.metricsTier, false
}

func (p *SMIProber) rememberMetricsTier(i int, fullProbe bool) {
	p.metricsTierMu.Lock()
	p.metricsTier = i
	if fullProbe {
		p.lastFullProbeAt = p.clockNow()
	}
	p.metricsTierMu.Unlock()
}

func (p *SMIProber) RuntimeInjection(indexes []int) RuntimeInjection {
	if len(indexes) == 0 {
		return RuntimeInjection{}
	}
	idx := make([]string, len(indexes))
	for i, g := range indexes {
		idx[i] = strconv.Itoa(g)
	}
	devices := strings.Join(idx, ",")
	return RuntimeInjection{
		Env: map[string]string{"CUDA_VISIBLE_DEVICES": devices},
		// Docker parses --gpus values as CSV. Without the inner quotes,
		// device=0,1 becomes DeviceIDs=[0] plus Count=1 and recent daemons
		// reject the request as "cannot set both Count and DeviceIDs". exec does
		// not add shell quoting for us, so the quote characters are intentional.
		DockerArgs: []string{"--gpus", "\"device=" + devices + "\""},
	}
}

// DriverVersion returns the driver version string, or "" if unavailable.
func (p *SMIProber) DriverVersion(ctx context.Context) string {
	p.driverMu.RLock()
	cached := p.driver
	p.driverMu.RUnlock()
	if cached != "" {
		return cached
	}
	out, err := p.query(ctx, "--query-gpu=driver_version", "--format=csv,noheader,nounits")
	if err != nil {
		// The loaded kernel module says the same without the driver lock
		// nvidia-smi waits on, which is why the query failed on a host whose
		// card failed to initialize.
		raw, _ := os.ReadFile(filepath.Join("/", p.hostRoot, "sys/module/nvidia/version"))
		return strings.TrimSpace(string(raw))
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if v := strings.TrimSpace(line); v != "" {
			p.rememberDriver(v)
			return v
		}
	}
	return ""
}

// Framework reports the runtime framework family and the best version signal
// available without linking vendor SDKs. NVIDIA's CLI exposes the driver
// version; callers use it as a conservative CUDA compatibility ceiling.
func (p *SMIProber) Framework(ctx context.Context) (string, string) {
	p.frameworkMu.Lock()
	defer p.frameworkMu.Unlock()
	// The CUDA version is the driver's: it is asked again when the driver
	// version the inventory reports changes, and only while that is
	// unknown, every frameworkTTL.
	p.driverMu.RLock()
	driver := p.driver
	p.driverMu.RUnlock()
	ttl := p.frameworkTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if p.framework != "" {
		if driver != "" && driver == p.frameworkDriver {
			return "cuda", p.framework
		}
		if driver == "" && p.frameworkDriver == "" && time.Since(p.frameworkAt) < ttl {
			return "cuda", p.framework
		}
	}
	out, err := p.query(ctx)
	if err != nil {
		return "cuda", ""
	}
	version := parseFrameworkVersion(out, `(?im)CUDA Version\s*:\s*([0-9]+(?:\.[0-9]+){0,2})`)
	if version != "" {
		p.framework, p.frameworkDriver, p.frameworkAt = version, driver, time.Now()
	}
	return "cuda", version
}

func parseFrameworkVersion(text, pattern string) string {
	re := regexp.MustCompile(pattern)
	match := re.FindStringSubmatch(text)
	if len(match) < 2 {
		return ""
	}
	return strings.TrimSpace(match[1])
}

// Probe returns the detected GPUs. Empty slice + nil error means "no GPU here".
func (p *SMIProber) Probe(ctx context.Context) ([]proto.GPUInfo, error) {
	slog.Debug("gpu probe started", "vendor", p.Vendor())
	if _, err := lookCLI("nvidia-smi"); err != nil && p.runner == nil {
		// No CLI to ask. The cards plugged in are still a fact, and saying
		// "this host has four cards and no driver for them" is the useful
		// answer; reporting none sends an operator looking for hardware
		// that is sitting right there.
		slog.Debug("gpu probe command unavailable", "vendor", p.Vendor(), "command", "nvidia-smi")
		return Roster(accelerator.VendorNVIDIA, p.hostRoot, nil, false), nil
	}
	// One query row per GPU; nounits keeps memory in MiB as bare integers.
	// Newer drivers require the explicit *.total suffix. The tiers degrade for
	// drivers that reject a field name outright; a field a card does not
	// support comes back as [N/A] and is simply not judged.
	out, err := "", error(nil)
	start := p.inventoryStart()
	for tier := start; tier < len(nvidiaInventoryQueries); tier++ {
		if out, err = p.query(ctx, nvidiaInventoryQueries[tier], "--format=csv,noheader,nounits"); err == nil || IsCLIUnresponsive(err) || ctx.Err() != nil {
			if err == nil {
				p.rememberInventoryTier(start, tier)
			}
			break
		}
	}
	if err != nil {
		// Every inventory query failed, so nothing is known about what
		// each card is. Which cards are in the machine is still a fact,
		// and reporting none of them would describe a host with no
		// accelerators — the opposite of what an operator needs to see on
		// a node whose driver has stopped answering. The error goes with
		// them so the server knows these cards carry only what PCI says.
		slog.Warn("gpu probe failed", "vendor", p.Vendor(), "err", err)
		return Roster(accelerator.VendorNVIDIA, p.hostRoot, nil, false), err
	}
	gpus := parseNVIDIAInfo(out)
	for _, gpu := range gpus {
		if gpu.DriverVersion != "" {
			p.rememberDriver(gpu.DriverVersion)
			break
		}
	}
	inventory := p.rememberInventory(gpus)
	if listing, ok := p.layoutQuery(ctx, &p.layout.listing, inventory, "-L"); ok {
		enrichSingleVisibleNVIDIAMIG(gpus, listing)
	}
	applyUnifiedMemoryFallback(gpus, nil)
	// What the driver listed, placed on the roster of what is actually
	// plugged in: a card it could not bring up, or that belongs to a VM,
	// is part of this node's hardware either way.
	gpus = Roster(accelerator.VendorNVIDIA, p.hostRoot, gpus, true)
	if len(gpus) == 0 {
		slog.Debug("gpu probe returned no devices", "vendor", p.Vendor())
	} else {
		slog.Info("gpu probe completed", "vendor", p.Vendor(), "count", len(gpus))
	}
	return gpus, nil
}

// nvidiaInventoryQueries are tried in order. The first seven fields are the
// inventory every driver answers; power.limit follows, then the health fields
// of accelerator.NVIDIAHealthMemoryFields in that order, then the recovery
// the driver asks for (see nvidiaHealth). The first one the driver answers is
// remembered (inventoryTier), so an older driver pays for the fields it does
// not know once, not every minute.
var nvidiaInventoryQueries = []string{
	"--query-gpu=" + nvidiaInventoryFields + ",power.limit," + strings.Join(accelerator.NVIDIAHealthMemoryFields, ",") + "," + accelerator.NVIDIARecoveryField,
	"--query-gpu=" + nvidiaInventoryFields + ",power.limit," + strings.Join(accelerator.NVIDIAHealthMemoryFields, ","),
	"--query-gpu=" + nvidiaInventoryFields + ",power.limit," + strings.Join(accelerator.NVIDIAHealthMemoryFields[:2], ","),
	"--query-gpu=" + nvidiaInventoryFields,
}

const nvidiaInventoryFields = "index,uuid,name,memory.total,compute_cap,pci.bus_id,driver_version"

// nvidiaInventoryRecheck is how long the inventory query this driver
// answered is trusted before the fullest one is tried again (a driver
// upgraded under a running agent).
const nvidiaInventoryRecheck = time.Hour

// inventoryStart is the first inventory query worth trying.
func (p *SMIProber) inventoryStart() int {
	p.inventoryMu.Lock()
	defer p.inventoryMu.Unlock()
	if p.inventoryFullAt.IsZero() || time.Since(p.inventoryFullAt) > nvidiaInventoryRecheck {
		return 0
	}
	return p.inventoryTier
}

func (p *SMIProber) rememberInventoryTier(start, tier int) {
	p.inventoryMu.Lock()
	defer p.inventoryMu.Unlock()
	p.inventoryTier = tier
	if start == 0 {
		p.inventoryFullAt = time.Now()
	}
}

func (p *SMIProber) rememberDriver(version string) {
	if strings.TrimSpace(version) == "" {
		return
	}
	p.driverMu.Lock()
	p.driver = strings.TrimSpace(version)
	p.driverMu.Unlock()
}

// slow reports whether nvidia-smi answers slower than callers wait. The
// optional queries (PCIe throughput, MIG names, topology) are then skipped:
// each would queue behind the inventory and metrics queries the node cannot
// do without, and delay their next answers.
func (p *SMIProber) slow() bool {
	return p.runner == nil && VendorCLISlow("nvidia-smi")
}

// query runs a read-only nvidia-smi query, which a slow driver may answer
// from the last result (see QueryVendorCLI).
func (p *SMIProber) query(ctx context.Context, args ...string) (string, error) {
	if p.runner != nil {
		return p.runner(ctx, args...)
	}
	return queryLogged(ctx, accelerator.VendorNVIDIA, "nvidia-smi", args...)
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
