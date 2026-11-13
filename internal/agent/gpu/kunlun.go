package gpu

import (
	"context"
	"encoding/xml"
	"log/slog"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// KunlunProber uses P800's observed xpu-smi XML API, not NVIDIA/Intel XPU APIs.
// The agent never links to the vendor's user-space SDK.
type KunlunProber struct {
	hostRoot string
	platform func() accelerator.HostRuntime
	command  func(context.Context, ...string) (string, error)
	lookPath func(string) (string, error)
	readFile func(string) ([]byte, error)
	stat     func(string) (os.FileInfo, error)
	// mu protects publication of lastSeen. Published maps are immutable;
	// readers may retain a snapshot after releasing the lock.
	mu       sync.RWMutex
	lastSeen map[int]proto.GPUInfo
}

type kunlunInventory struct {
	XMLName  xml.Name     `xml:"xpu_smi_log"`
	Driver   string       `xml:"driver_version"`
	Attached int          `xml:"attached_xpus"`
	Cards    []kunlunCard `xml:"xpu"`
}
type kunlunCard struct {
	invalid   string
	Name      string              `xml:"product_name"`
	Brand     string              `xml:"product_brand"`
	Arch      string              `xml:"product_architecture"`
	UUID      string              `xml:"uuid"`
	Minor     string              `xml:"minor_number"`
	BDF       string              `xml:"pci>pci_bus_id"`
	Total     string              `xml:"fb_memory_usage>total"`
	Used      string              `xml:"fb_memory_usage>used"`
	Util      string              `xml:"utilization>xpu_util"`
	Temp      string              `xml:"temperature>xpu_temp"`
	Power     string              `xml:"power_readings>power_draw"`
	Limit     string              `xml:"power_readings>enforced_power_limit"`
	ECC       string              `xml:"ecc_errors>volatile>dram_uncorrectable"`
	Processes []kunlunProcessInfo `xml:"processes>process_info"`
}

type kunlunProcessInfo struct {
	PID        string `xml:"pid"`
	Name       string `xml:"process_name"`
	UsedMemory string `xml:"used_memory"`
}

var kunlunBDF = regexp.MustCompile(`^[0-9a-fA-F]{4,8}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)
var kunlunProcessInfoBlock = regexp.MustCompile(`(?s)<process_info>.*?</process_info>`)
var kunlunBadGPUInstanceClose = regexp.MustCompile(`(<gpu_instance_id>[^<]*)</xpu_instance_id>`)

// xpu-smi 5.0.21.8 closes gpu_instance_id with xpu_instance_id while an
// XPU is busy. Repair only that observed typo inside a
// process_info block; xml.Unmarshal remains strict for every other error.
func repairKunlunProcessInfoTagTypos(raw []byte) []byte {
	text := kunlunProcessInfoBlock.ReplaceAllStringFunc(string(raw), func(block string) string {
		return kunlunBadGPUInstanceClose.ReplaceAllString(block, `${1}</gpu_instance_id>`)
	})
	return []byte(text)
}

func (p *KunlunProber) Vendor() string { return accelerator.VendorKunlunxin }
func (p *KunlunProber) executable() (string, error) {
	if p.lookPath != nil {
		return p.lookPath("xpu-smi")
	}
	// On Kubernetes the CLI is the host's (lookCLI searches its install
	// directories through the host root).
	return lookCLI("xpu-smi")
}
func (p *KunlunProber) run(ctx context.Context, args ...string) (string, error) {
	if p.command != nil {
		bounded, cancel := context.WithTimeout(ctx, vendorCLITimeout)
		defer cancel()
		return p.command(bounded, args...)
	}
	exe, err := p.executable()
	if err != nil {
		return "", err
	}
	return QueryVendorCLIStdout(ctx, exe, args...)
}
func (p *KunlunProber) read(path string) ([]byte, error) {
	if p.readFile != nil {
		return p.readFile(path)
	}
	return os.ReadFile(path)
}
func (p *KunlunProber) inspect(path string) (os.FileInfo, error) {
	if p.stat != nil {
		return p.stat(path)
	}
	// A privileged pod's /dev is a copy made at container start; lstatTool
	// checks the host's live /dev when CLIs run on the host.
	return lstatTool(path)
}

var kunlunNumberUnit = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([A-Za-z%]+)$`)

func kunlunValue(raw, unit string) (float64, bool) {
	m := kunlunNumberUnit.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return 0, false
	}
	v, e := strconv.ParseFloat(m[1], 64)
	if e != nil || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, false
	}
	actual := strings.ToLower(m[2])
	expected := strings.ToLower(unit)
	if expected == "mib" {
		switch actual {
		case "kib":
			v /= 1024
		case "mb":
			v *= 1000000.0 / (1 << 20)
		case "mib":
		case "gib":
			v *= 1024
		default:
			return 0, false
		}
	} else if actual != expected {
		return 0, false
	}
	return v, !math.IsInf(v, 0) && v >= 0
}
func (p *KunlunProber) inventory(ctx context.Context) (kunlunInventory, error) {
	return p.inventoryWithFreshness(ctx, false)
}
func (p *KunlunProber) inventoryWithFreshness(ctx context.Context, fresh bool) (kunlunInventory, error) {
	var out string
	var e error
	if fresh && p.command == nil {
		exe, pathErr := p.executable()
		if pathErr != nil {
			return kunlunInventory{}, pathErr
		}
		out, e = RunVendorCLIStdout(ctx, nil, exe, "-q", "-x")
	} else {
		out, e = p.run(ctx, "-q", "-x")
	}
	if e != nil {
		return kunlunInventory{}, e
	}
	var inv kunlunInventory
	if e = xml.Unmarshal(repairKunlunProcessInfoTagTypos([]byte(out)), &inv); e != nil {
		return inv, e
	}
	markKunlunCards(&inv)
	if inv.Attached != len(inv.Cards) {
		slog.Warn("Kunlun attached card count differs from inventory rows", "attached_xpus", inv.Attached, "rows", len(inv.Cards))
	}

	return inv, nil
}

// ComputeProcesses reports the processes named by xpu-smi for Kunlun cards.
// Invalid or absent PIDs are skipped without discarding the GPU inventory.
func (p *KunlunProber) ComputeProcesses(ctx context.Context) ([]ComputeProcess, error) {
	inv, err := p.inventory(ctx)
	if err != nil {
		return nil, err
	}
	var out []ComputeProcess
	for _, card := range inv.Cards {
		for _, process := range card.Processes {
			pid, err := strconv.Atoi(strings.TrimSpace(process.PID))
			if err != nil || pid <= 0 {
				continue
			}
			out = append(out, ComputeProcess{PID: pid, Name: strings.TrimSpace(process.Name), PCIeBDF: card.BDF, MemoryBytes: kunlunMemoryBytes(process.UsedMemory)})
		}
	}
	return out, nil
}

func (p *KunlunProber) Available() bool {
	if _, e := p.executable(); e != nil {
		return false
	}
	i, e := p.inventory(context.Background())
	if e != nil {
		return false
	}
	for _, card := range i.Cards {
		if strings.EqualFold(strings.TrimSpace(card.Brand), "KUNLUNXIN") {
			return true
		}
	}
	return false
}
func (p *KunlunProber) Probe(ctx context.Context) ([]proto.GPUInfo, error) {
	if _, e := p.executable(); e != nil {
		return Roster(p.Vendor(), p.hostRoot, nil, false), nil //nolint:nilerr // Absence of this vendor CLI is an unavailable probe, not a node failure.
	}
	inv, e := p.inventory(ctx)
	if e != nil {
		return Roster(p.Vendor(), p.hostRoot, nil, false), e
	}
	out := make([]proto.GPUInfo, 0, len(inv.Cards))
	for _, c := range inv.Cards {
		ecc, eccErr := strconv.ParseUint(strings.TrimSpace(c.ECC), 10, 64)

		n, minorErr := strconv.Atoi(c.Minor)
		if minorErr != nil || n < 0 || n >= 255 {
			n = -1
		}
		limit, _ := kunlunValue(c.Limit, "W")
		numa := -1
		if b, e := p.read("/sys/bus/pci/devices/" + c.BDF + "/numa_node"); e == nil {
			if v, e := strconv.Atoi(strings.TrimSpace(string(b))); e == nil {
				numa = v
			}
		}
		g := proto.GPUInfo{
			Index: n, UUID: c.UUID, Name: c.Name, Vendor: p.Vendor(),
			ArchID: strings.ToLower(c.Arch), PCIeBDF: c.BDF, NUMANode: numa,
			MemoryTotalBytes: kunlunMemoryBytes(c.Total),
			DriverVersion:    inv.Driver, PowerLimitWatts: limit,
		}
		code := ""
		var evidence []accelerator.HealthField
		switch {
		case c.invalid != "":
			code = accelerator.HealthInventoryInvalid
			evidence = []accelerator.HealthField{{Name: "card_validation", Value: c.invalid}}
		case eccErr != nil:
			code = accelerator.HealthTelemetryUnavailable
		case ecc > 0:
			code = accelerator.HealthMemoryUncorrectable
		}
		if code != "" {
			evidence = append(evidence, accelerator.HealthField{Name: "ecc_errors.volatile.dram_uncorrectable", Value: c.ECC})
		}
		if inv.Attached != len(inv.Cards) {
			code = accelerator.MostSevereHealth(code, accelerator.HealthInventoryIncomplete)
			evidence = append(evidence, accelerator.HealthField{Name: "attached_xpus", Value: strconv.Itoa(inv.Attached)}, accelerator.HealthField{Name: "inventory_rows", Value: strconv.Itoa(len(inv.Cards))})
		}
		SetCardFinding(&g, code, evidence)
		out = append(out, g)
	}
	lastSeen := make(map[int]proto.GPUInfo, len(out))
	for _, g := range out {
		if g.Index >= 0 {
			lastSeen[g.Index] = g
		}
	}
	p.mu.Lock()
	p.lastSeen = lastSeen
	p.mu.Unlock()
	return Roster(p.Vendor(), p.hostRoot, out, true), nil
}
func (p *KunlunProber) DriverVersion(ctx context.Context) string {
	i, e := p.inventory(ctx)
	if e != nil {
		return ""
	}
	return i.Driver
}

// xpu-smi's cuda_version is a compatibility facade, not the native XRE version.
func (p *KunlunProber) Framework(context.Context) (string, string) { return "xre", "" }
func (p *KunlunProber) Topology(ctx context.Context) string {
	o, e := p.run(ctx, "topo", "-m")
	if e != nil {
		return ""
	}
	return o
}
func (p *KunlunProber) Metrics(ctx context.Context) ([]Metric, error) {
	i, e := p.inventory(ctx)
	if e != nil {
		return nil, e
	}
	out := make([]Metric, 0, len(i.Cards))
	for _, c := range i.Cards {
		if strings.Contains(c.invalid, "minor_number") {
			continue
		}
		n, _ := strconv.Atoi(c.Minor)
		m := Metric{Index: n, UUID: c.UUID, Vendor: p.Vendor(), Values: map[string]float64{}}
		total, _ := kunlunValue(c.Total, "MiB")
		readings := []struct {
			key, raw, unit string
			scale          float64
		}{
			{"gpuworks_gpu_memory_total_bytes", c.Total, "MiB", 1 << 20},
			{"gpuworks_gpu_memory_used_bytes", c.Used, "MiB", 1 << 20},
			{"gpuworks_gpu_utilization", c.Util, "%", 1},
			{"gpuworks_gpu_temperature_celsius", c.Temp, "C", 1},
			{"gpuworks_gpu_power_watts", c.Power, "W", 1},
		}
		for _, reading := range readings {
			value, valid := kunlunValue(reading.raw, reading.unit)
			if !valid || value*reading.scale > (1<<63-1) {
				continue
			}
			if reading.unit == "%" && value > 100 {
				continue
			}
			if reading.key == "gpuworks_gpu_memory_used_bytes" && value > total {
				continue
			}
			m.Values[reading.key] = value * reading.scale
		}
		out = append(out, m)
	}
	return out, nil
}
