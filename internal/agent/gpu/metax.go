package gpu

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// MetaXProber implements the MetaX MACA backend via mx-smi v3.100.
//
// Observed on an eight-card MXC550 node on 2026-11-20 with mx-smi 3.100.0;
// The copied fixtures replace GPU UUIDs and serial numbers, while preserving
// timestamps and process names. Altered error cases are synthetic unless noted.
//
//   - `mx-smi -L` reports each card index, model, PCI address, state and UUID.
//   - `mx-smi -o <path> --show-memory --show-usage --show-temperature`
//     writes a CSV with a header naming every column ("utilization.vram.used
//     [KB]", "temperature.hotspot [C]", "utilization.GPU [%]"), which is why
//     this prober parses by header name instead of by position: the column
//     set differs by flag combination, and the values are in KB while the
//     platform's metric is bytes.
//   - `mx-smi --show-process` lists which card each host PID holds memory on.
//     The private process parser maps its indexes to stable PCI addresses
//     before the shared occupancy check receives them.
//
// Indexes are ephemeral: a card leaving the host can renumber the others.
// Nonempty runtime device injection is refused until selectors are verified.
type MetaXProber struct {
	run      metaxCommandRunner
	lookPath func(string) (string, error)
	readFile func(string) ([]byte, error)

	// hostRoot is where the host's /sys is read for the PCI roster; empty
	// means "/". A test sets it so the roster describes its fixture rather
	// than whatever is in the machine running the test.
	hostRoot string
	seen     cardBaseline
}

type metaxIdentity struct {
	uuid string
	bdf  string
}

// metaxCommandRunner runs one mx-smi invocation and returns its stdout.
type metaxCommandRunner func(ctx context.Context, args ...string) (string, error)

var _ Prober = (*MetaXProber)(nil)

func (p *MetaXProber) Vendor() string { return accelerator.VendorMetaX }

func (p *MetaXProber) HostRuntimeCheck() HostRuntimeCheck {
	return HostRuntimeCheck{
		CLI:          "mx-smi",
		Prerequisite: MetaXSmokePrerequisite,
		Visible:      func(g proto.GPUInfo) (string, bool) { return g.PCIeBDF, g.PCIeBDF != "" },
		Try:          MetaXRuntimeSmokeOn,
	}
}

func (p *MetaXProber) matchesBaseline(gpus []proto.GPUInfo) bool {
	previous := p.seen.all()
	if previous == nil {
		return true
	}
	if len(previous) != len(gpus) {
		return false
	}
	for _, g := range gpus {
		old, ok := previous[g.Index]
		if !ok || old.PCIeBDF != g.PCIeBDF || old.UUID != g.UUID {
			return false
		}
	}
	return true
}

func (p *MetaXProber) lookup(name string) (string, error) {
	if p.lookPath != nil {
		return p.lookPath(name)
	}
	return lookCLI(name)
}

func (p *MetaXProber) exec(ctx context.Context, args ...string) (string, error) {
	if p.run != nil {
		return p.run(ctx, args...)
	}
	out, err := RunVendorCLIStdout(ctx, nil, "mx-smi", args...)
	if err != nil {
		return "", fmt.Errorf("mx-smi %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func (p *MetaXProber) read(path string) ([]byte, error) {
	if p.readFile != nil {
		return p.readFile(path)
	}
	return readTool(path)
}

// Available asks the driver for its listing. Looking the binary up is not
// enough: a stale mx-smi is left on hosts of other vendors, and answering is
// what separates a working stack from an installed one.
func (p *MetaXProber) Available() bool {
	if _, err := p.lookup("mx-smi"); err != nil {
		return false
	}
	out, err := p.exec(context.Background(), "-L")
	gpus, valid := validatedMetaXList(out)
	return err == nil && valid && len(gpus) > 0
}

const (
	// metaxMemoryMetric is the CSV column holding the card's used HBM. The
	// "vram" pair is what mx-smi reports for a card in every state; the
	// "vis_vram" pair is what the driver has handed to containers, and the
	// "xtt" pair is the driver's own allocation pool, which stays near-empty
	// on an idle card and counts the runtime's reservations on a busy one —
	// neither is the card's occupancy.
	metaxMemoryUsedColumn  = "utilization.vram.used [KB]"
	metaxMemoryTotalColumn = "utilization.vram.total [KB]"
	metaxTemperatureColumn = "temperature.hotspot [C]"
	metaxUtilizationColumn = "utilization.GPU [%]"
	// metaxCSVDeviceColumn and metaxCSVBDFColumn are the card's index and PCI
	// address in the same CSV: the pair the two mx-smi outputs are joined on.
	metaxCSVDeviceColumn = "deviceId"
	metaxCSVBDFColumn    = "bdfId"
)

func (p *MetaXProber) Probe(ctx context.Context) ([]proto.GPUInfo, error) {
	_, err := p.lookup("mx-smi")
	return probeCards(ctx, accelerator.VendorMetaX, p.hostRoot, err == nil, p.list, &p.seen)
}

func (p *MetaXProber) list(ctx context.Context) ([]proto.GPUInfo, error) {
	out, err := p.exec(ctx, "-L")
	if err != nil {
		return nil, err
	}
	gpus, valid := validatedMetaXList(out)
	if !valid || len(gpus) == 0 {
		return nil, fmt.Errorf("mx-smi returned an empty or invalid inventory")
	}
	driver := p.DriverVersion(ctx)
	// The listing carries identity and state; memory and the SKU are in the
	// summary. Asking for both is what keeps a card's total memory from
	// being reported as zero, which is what a scheduler compares against.
	summary, summaryErr := p.summary(ctx)
	missing := len(summary) != len(gpus)
	for i := range gpus {
		gpus[i].DriverVersion = driver
		if detail, ok := summary[gpus[i].PCIeBDF]; ok && detail.index == gpus[i].Index && detail.memoryTotalBytes > 0 {
			gpus[i].MemoryTotalBytes = detail.memoryTotalBytes
		} else {
			missing = true
		}
	}
	if summaryErr != nil || missing {
		slog.Warn("MetaX memory capacity unavailable", "err", summaryErr)
		return nil, fmt.Errorf("MetaX memory capacity unavailable")
	}
	return gpus, nil
}

// metaxCardDetail is what the summary says about one card beyond its
// identity: the memory a scheduler compares against. The SKU is not here —
// it is mx-smi's "-L" model name, which the listing already carries.
type metaxCardDetail struct {
	index            int
	memoryTotalBytes int64
}

// summary reads mx-smi -s, keyed by normalized PCI address.
//
// The summary repeats a block per card, each headed "GPU#<i>  <model>
// <bdf>" exactly as -L prints it, so the address is what the blocks are
// keyed by: an index renumbers when a card leaves the host, and a card's
// memory must not move to its neighbour with it.
func (p *MetaXProber) summary(ctx context.Context) (map[string]metaxCardDetail, error) {
	out, err := p.exec(ctx, "-s")
	if err != nil {
		return nil, err
	}
	return parseMetaXSummary(out), nil
}

// parseMetaXSummary reads the per-card blocks of mx-smi -s:
//
//	GPU#0  MXC550  0000:0f:00.0
//	    Memory
//	        vram total                            : 67108864 KB
//
// "vis_vram" is what the driver has handed to containers and "vram" is the
// card's own accounting; the total is the same number in both, and the card's
// memory is the latter's.
func parseMetaXSummary(out string) map[string]metaxCardDetail {
	cards := make(map[string]metaxCardDetail)
	seenHeaders := make(map[string]bool)
	seenTotals := make(map[string]bool)
	ambiguous := make(map[string]bool)
	bdf := ""
	index := -1
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.HasPrefix(fields[0], "GPU#") {
			// Every header ends the block before it, so a header whose
			// address is unreadable must not leave the previous card's
			// identity standing for the values that follow.
			bdf = ""
			if len(fields) >= 3 {
				parsedIndex, err := strconv.Atoi(strings.TrimPrefix(fields[0], "GPU#"))
				if candidate := accelerator.NormalizeBDF(fields[2]); err == nil && parsedIndex >= 0 && metaxBDFPattern.MatchString(candidate) {
					bdf, index = candidate, parsedIndex
					if seenHeaders[bdf] {
						ambiguous[bdf] = true
						delete(cards, bdf)
					}
					seenHeaders[bdf] = true
				}
			}
			continue
		}
		if bdf == "" {
			continue
		}
		// The value is the field before the unit ("67108864 KB").
		key := strings.TrimSpace(strings.SplitN(line, ":", 2)[0])
		if key != "vram total" {
			continue
		}
		if seenTotals[bdf] {
			ambiguous[bdf] = true
			delete(cards, bdf)
		}
		seenTotals[bdf] = true
		if ambiguous[bdf] || len(fields) < 3 || fields[len(fields)-1] != "KB" {
			continue
		}
		kb, err := strconv.ParseInt(fields[len(fields)-2], 10, 64)
		if err != nil || kb <= 0 || kb > (1<<63-1)>>10 {
			continue
		}
		detail := cards[bdf]
		detail.index = index
		detail.memoryTotalBytes = kb << 10
		cards[bdf] = detail
	}
	return cards
}

// parseMetaXList reads mx-smi -L:
//
//	GPU#0    MXC550      0000:0f:00.0   Available (UUID: <redacted>)
//
// The third field is the driver's state, the only word the card's health
// turns on: "Available" is the driver having brought it up, and anything else
// is a card it cannot use. The parenthesis after it is identity (the UUID),
// never health.
func parseMetaXList(out string) []proto.GPUInfo {
	var gpus []proto.GPUInfo
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 4 || !strings.HasPrefix(fields[0], "GPU#") {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimPrefix(fields[0], "GPU#"))
		if err != nil {
			continue
		}
		name, bdf, state := fields[1], accelerator.NormalizeBDF(fields[2]), fields[3]
		if !metaxBDFPattern.MatchString(bdf) {
			continue
		}
		gpu := proto.GPUInfo{
			Index: idx, Name: name, PCIeBDF: bdf, Vendor: accelerator.VendorMetaX,
			ArchID: name, UUID: parseMetaXUUID(line),
		}
		code, evidence := metaxHealth(state)
		SetCardFinding(&gpu, code, evidence)
		gpus = append(gpus, gpu)
	}
	sort.Slice(gpus, func(i, j int) bool { return gpus[i].Index < gpus[j].Index })
	return gpus
}

// validatedMetaXList rejects malformed or duplicate card rows rather than
// silently treating a truncated inventory as authoritative.
func validatedMetaXList(out string) ([]proto.GPUInfo, bool) {
	gpus := parseMetaXList(out)
	rows := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "GPU#") {
			rows++
		}
	}
	if rows != len(gpus) {
		return nil, false
	}
	indexes := make(map[int]bool, len(gpus))
	addresses := make(map[string]bool, len(gpus))
	uuids := make(map[string]bool, len(gpus))
	for _, gpu := range gpus {
		if gpu.Index < 0 || indexes[gpu.Index] || addresses[gpu.PCIeBDF] || gpu.UUID == "" || uuids[gpu.UUID] {
			return nil, false
		}
		indexes[gpu.Index], addresses[gpu.PCIeBDF], uuids[gpu.UUID] = true, true, true
	}
	return gpus, true
}

// metaxBDFPattern is a PCI address mx-smi printed for a card: the domain in
// however many digits that host writes it, the bus, the device and the
// function. An address that does not match is a truncated or garbled line,
// and the card it was supposed to name is not one to key memory or metrics
// on.
var metaxBDFPattern = regexp.MustCompile(`^[0-9a-f]{4,}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)

var metaxUUIDPattern = regexp.MustCompile(`UUID:\s*([0-9A-Za-z-]+)`)

func parseMetaXUUID(line string) string {
	match := metaxUUIDPattern.FindStringSubmatch(line)
	if match == nil {
		return ""
	}
	return strings.TrimSpace(match[1])
}

// metaxHealth judges the state word mx-smi prints for a card. "Available" is
// the driver having brought the card up; anything else is a card the driver
// cannot use, which is HealthRuntimeUnusable's meaning (present, listed, not
// usable) rather than a fault the card itself reported.
//
// The word is carried as the evidence, so whoever looks at the card reads
// the driver's own state instead of a summary of it.
func metaxHealth(state string) (string, []accelerator.HealthField) {
	state = strings.TrimSpace(state)
	if state == "" || strings.EqualFold(state, "Available") {
		return "", nil
	}
	return accelerator.HealthRuntimeUnusable, []accelerator.HealthField{
		{Name: "mx-smi state", Value: state},
	}
}

func (p *MetaXProber) DriverVersion(ctx context.Context) string {
	out, err := p.exec(ctx, "--show-version")
	if err != nil {
		return ""
	}
	return parseMetaXDriverVersion(out)
}

// parseMetaXDriverVersion reads the "KMD" line of --show-version, the kernel
// mode driver the MACA userspace talks to. The MACA line beside it is the
// userspace runtime, which the container image carries itself and which
// therefore says nothing about what this host can run.
func parseMetaXDriverVersion(out string) string {
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(key) == "KMD" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (p *MetaXProber) Framework(ctx context.Context) (string, string) {
	out, err := p.exec(ctx, "--show-version")
	if err != nil {
		return "maca", ""
	}
	return "maca", parseMetaXFrameworkVersion(out)
}

// parseMetaXFrameworkVersion reads the "MACA" line of --show-version.
func parseMetaXFrameworkVersion(out string) string {
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(key) == "MACA" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (p *MetaXProber) Topology(ctx context.Context) string {
	out, err := p.exec(ctx, "topo", "-m")
	if err != nil || strings.TrimSpace(out) == "" {
		return ""
	}
	return out
}
