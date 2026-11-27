package gpu

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// metaxMetricsTimeout bounds one metrics read: mx-smi opens every card, and
// a card wedged in the driver can make it hang rather than fail.
const metaxMetricsTimeout = 20 * time.Second

// Metrics reads the header-named CSV. mx-smi writes it to a path and, when
// given /dev/stdout, interleaves its banner with the header line (measured
// 2026-11-20: the banner lands mid-header), so the table is written to a
// temporary file and read whole.
func (p *MetaXProber) Metrics(ctx context.Context) ([]Metric, error) {
	if _, err := p.lookup("mx-smi"); err != nil {
		return nil, fmt.Errorf("find mx-smi: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, metaxMetricsTimeout)
	defer cancel()
	dir := ""
	if CLIsOnHost() {
		dir = filepath.Join(hostRoot, "tmp")
	}
	file, err := os.CreateTemp(dir, "gpuworks-mx-smi-*.csv")
	if err != nil {
		return nil, fmt.Errorf("create mx-smi output: %w", err)
	}
	localPath := file.Name()
	defer os.Remove(localPath)
	path := localPath
	if CLIsOnHost() {
		path = filepath.Join("/tmp", filepath.Base(localPath))
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("create mx-smi output: %w", err)
	}
	if _, err := p.exec(ctx, "-o", path, "--show-memory", "--show-usage", "--show-temperature"); err != nil {
		return nil, err
	}
	raw, err := p.read(path)
	if err != nil {
		return nil, fmt.Errorf("read mx-smi output: %w", err)
	}
	// The CSV names cards by the index mx-smi gave them this round, and an
	// index is exactly what moves when a card leaves the host. The listing
	// is the authority on which card an index names, so the samples are
	// joined to it rather than trusted on their own.
	listing, err := p.exec(ctx, "-L")
	if err != nil {
		return nil, err
	}
	gpus, valid := validatedMetaXList(listing)
	if !valid || len(gpus) == 0 {
		return nil, fmt.Errorf("mx-smi returned an empty or invalid metrics inventory")
	}
	if !p.matchesBaseline(gpus) {
		return nil, fmt.Errorf("MetaX inventory changed since probe")
	}
	metrics := identifyMetaXMetrics(string(raw), gpus)
	if len(metrics) == 0 {
		// Every sample was dropped, either because the CSV cannot name its
		// cards or because none of them is in the listing. Reported as a
		// failure so the reason is in the agent's log instead of the node
		// silently having no per-card metrics.
		return nil, fmt.Errorf("mx-smi metrics name no card of the current inventory")
	}
	return metrics, nil
}

// identifyMetaXMetrics gives each sample its inventory card's UUID, and drops
// every sample whose identity the two outputs do not agree on.
//
// The server groups per-card statistics by gpu_uuid, so a sample without one
// is not merely unlabeled: it lands in a different series from the card's
// inventory row and splits that card's history in two. The agreement asked
// for here is deliberately narrow — the same index, the same PCI address,
// and a UUID unique in this listing — because the alternative to a labeled
// sample is a missing sample, while the alternative to an unlabeled one is a
// card whose numbers are wrong.
func identifyMetaXMetrics(raw string, gpus []proto.GPUInfo) []Metric {
	metrics := parseMetaXCSV(raw)
	if len(metrics) == 0 {
		return nil
	}
	bdfOf, ambiguous := metaxCSVAddresses(raw)
	identities := make(map[int]proto.GPUInfo, len(gpus))
	ambiguousIdentity := make(map[int]bool)
	bdfCounts := make(map[string]int, len(gpus))
	uuids := make(map[string]int, len(gpus))
	for _, g := range gpus {
		if _, exists := identities[g.Index]; exists {
			ambiguousIdentity[g.Index] = true
		}
		identities[g.Index] = g
		if g.PCIeBDF != "" {
			bdfCounts[g.PCIeBDF]++
		}
		if g.UUID != "" {
			uuids[g.UUID]++
		}
	}
	result := metrics[:0]
	for _, m := range metrics {
		g, ok := identities[m.Index]
		if !ok || ambiguousIdentity[m.Index] || ambiguous[m.Index] || g.UUID == "" || uuids[g.UUID] != 1 || g.PCIeBDF == "" || bdfCounts[g.PCIeBDF] != 1 {
			continue
		}
		if bdfOf[m.Index] == "" || bdfOf[m.Index] != g.PCIeBDF {
			continue
		}
		m.UUID = g.UUID
		result = append(result, m)
	}
	return result
}

// metaxCSVAddresses reads the PCI address each row of the metrics CSV claims
// for its device index, and which indexes the rows disagree about.
func metaxCSVAddresses(raw string) (map[int]string, map[int]bool) {
	addresses := map[int]string{}
	ambiguous := map[int]bool{}
	lines := strings.Split(raw, "\n")
	headerIndex := -1
	for i, line := range lines {
		if strings.Contains(line, metaxCSVDeviceColumn) {
			headerIndex = i
			break
		}
	}
	if headerIndex < 0 {
		return addresses, ambiguous
	}
	column := make(map[string]int)
	for i, name := range splitMetaXCSVRow(lines[headerIndex]) {
		column[strings.TrimSpace(name)] = i
	}
	indexCol, ok := column[metaxCSVDeviceColumn]
	if !ok {
		return addresses, ambiguous
	}
	bdfCol, ok := column[metaxCSVBDFColumn]
	if !ok {
		return addresses, ambiguous
	}
	bdfIndexes := map[string]int{}
	for _, line := range lines[headerIndex+1:] {
		fields := splitMetaXCSVRow(line)
		if len(fields) <= indexCol || len(fields) <= bdfCol {
			continue
		}
		index, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(fields[indexCol]), "GPU#"))
		if err != nil || index < 0 {
			continue
		}
		bdf := accelerator.NormalizeBDF(fields[bdfCol])
		if previous, seen := addresses[index]; seen && previous != bdf {
			ambiguous[index] = true
		}
		addresses[index] = bdf
		if bdf == "" {
			continue
		}
		if other, seen := bdfIndexes[bdf]; seen && other != index {
			ambiguous[index], ambiguous[other] = true, true
		} else {
			bdfIndexes[bdf] = index
		}
	}
	return addresses, ambiguous
}

// parseMetaXCSV reads the header-named table mx-smi -o writes. Every column
// is addressed by name: the order changes with the flags given, and a parser
// that assumed one produced the wrong card's numbers silently.
func parseMetaXCSV(out string) []Metric {
	lines := strings.Split(out, "\n")
	headerIndex := -1
	for i, line := range lines {
		if strings.Contains(line, "deviceId") {
			headerIndex = i
			break
		}
	}
	if headerIndex < 0 {
		return nil
	}
	header := splitMetaXCSVRow(lines[headerIndex])
	column := make(map[string]int, len(header))
	for i, name := range header {
		name = strings.TrimSpace(name)
		if _, exists := column[name]; exists {
			return nil
		}
		column[name] = i
	}
	deviceColumn, ok := column[metaxCSVDeviceColumn]
	if !ok {
		return nil
	}
	bdfColumn, hasBDF := column[metaxCSVBDFColumn]
	type row struct {
		metric Metric
		bdf    string
		valid  bool
	}
	byIndex := make(map[int]row)
	for _, line := range lines[headerIndex+1:] {
		fields := splitMetaXCSVRow(line)
		if len(fields) <= deviceColumn {
			continue
		}
		index, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(fields[deviceColumn]), "GPU#"))
		if err != nil || index < 0 {
			continue
		}
		if len(fields) < len(header) {
			byIndex[index] = row{valid: false}
			continue
		}
		bdf := ""
		if hasBDF {
			bdf = accelerator.NormalizeBDF(fields[bdfColumn])
		}
		m := Metric{Index: index, Vendor: accelerator.VendorMetaX, Values: map[string]float64{}}
		valid := true
		for _, field := range []struct {
			column, name string
			nonnegative  bool
		}{
			{metaxMemoryUsedColumn, "gpuworks_gpu_memory_used_bytes", true},
			{metaxMemoryTotalColumn, "gpuworks_gpu_memory_total_bytes", true},
			{metaxUtilizationColumn, "gpuworks_gpu_utilization", true},
			{metaxTemperatureColumn, "gpuworks_gpu_temperature_celsius", false},
		} {
			fieldIndex, present := column[field.column]
			if !present {
				continue
			}
			v, validNumber := parseMetaXMetric(fields[fieldIndex], field.nonnegative)
			if !validNumber {
				valid = false
				continue
			}
			if field.column == metaxMemoryUsedColumn || field.column == metaxMemoryTotalColumn {
				v *= 1024
			}
			if math.IsInf(v, 0) {
				valid = false
				continue
			}
			m.Values[field.name] = v
		}
		if previous, exists := byIndex[index]; exists {
			if previous.bdf != bdf || !previous.valid || !valid || !sameMetaXMetricValues(previous.metric.Values, m.Values) {
				previous.valid = false
				byIndex[index] = previous
			}
			continue
		}
		byIndex[index] = row{metric: m, bdf: bdf, valid: valid}
	}
	order := make([]int, 0, len(byIndex))
	for index := range byIndex {
		order = append(order, index)
	}
	sort.Ints(order)
	metrics := make([]Metric, 0, len(order))
	for _, index := range order {
		r := byIndex[index]
		if r.valid && len(r.metric.Values) > 0 {
			metrics = append(metrics, r.metric)
		}
	}
	return metrics
}

func parseMetaXMetric(value string, nonnegative bool) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || (nonnegative && v < 0) {
		return 0, false
	}
	return v, true
}

func sameMetaXMetricValues(a, b map[string]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for name, value := range a {
		if b[name] != value {
			return false
		}
	}
	return true
}

func splitMetaXCSVRow(line string) []string {
	if strings.TrimSpace(line) == "" {
		return nil
	}
	return strings.Split(line, ",")
}
