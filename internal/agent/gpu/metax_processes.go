package gpu

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// metaxProcessRow retains the vendor's ephemeral device index until the
// process table has been matched against a validated mx-smi inventory.
type metaxProcessRow struct {
	Index       int
	PID         int
	Name        string
	MemoryBytes int64
}

// ComputeProcesses lists compute processes via mx-smi. Its text table is the
// one view of them: --show-process is refused together with -o, so it is read
// from the text table rather than the CSV.
func (p *MetaXProber) ComputeProcesses(ctx context.Context) ([]ComputeProcess, error) {
	before, err := p.exec(ctx, "-L")
	if err != nil {
		return nil, err
	}
	beforeCards, valid := validatedMetaXList(before)
	if !valid || len(beforeCards) == 0 {
		return nil, fmt.Errorf("MetaX process inventory is empty or invalid")
	}
	if !p.matchesBaseline(beforeCards) {
		return nil, fmt.Errorf("MetaX inventory changed since probe")
	}

	out, err := p.exec(ctx, "--show-process")
	if err != nil {
		return nil, err
	}
	if !strings.Contains(out, "Process:") || !strings.Contains(out, "PID") || !strings.Contains(out, "End of Log") {
		return nil, fmt.Errorf("MetaX process table is incomplete")
	}
	rows, err := parseMetaXProcesses(out)
	if err != nil {
		return nil, err
	}

	after, err := p.exec(ctx, "-L")
	if err != nil {
		return nil, err
	}
	afterCards, valid := validatedMetaXList(after)
	if !valid {
		return nil, fmt.Errorf("MetaX inventory is invalid after process query")
	}
	addresses := make(map[int]metaxIdentity, len(beforeCards))
	for _, card := range beforeCards {
		if _, exists := addresses[card.Index]; exists {
			return nil, fmt.Errorf("duplicate MetaX device index %d", card.Index)
		}
		addresses[card.Index] = metaxIdentity{bdf: card.PCIeBDF, uuid: card.UUID}
	}
	if len(afterCards) != len(addresses) {
		return nil, fmt.Errorf("MetaX inventory changed during process query")
	}
	seen := make(map[int]bool, len(afterCards))
	for _, card := range afterCards {
		identity, ok := addresses[card.Index]
		if !ok || seen[card.Index] || identity.bdf != card.PCIeBDF || identity.uuid != card.UUID {
			return nil, fmt.Errorf("MetaX inventory changed during process query")
		}
		seen[card.Index] = true
	}

	procs := make([]ComputeProcess, 0, len(rows))
	for _, row := range rows {
		identity, ok := addresses[row.Index]
		if !ok || identity.bdf == "" {
			return nil, fmt.Errorf("MetaX process references unmapped device index %d", row.Index)
		}
		procs = append(procs, ComputeProcess{
			PID:         row.PID,
			Name:        row.Name,
			PCIeBDF:     identity.bdf,
			MemoryBytes: row.MemoryBytes,
		})
	}
	return procs, nil
}

// parseMetaXProcesses reads the table mx-smi --show-process prints:
//
//	|  3                   879033         sgl_diffusion::              31276          |
//
// A row is an index, a pid, a name that may contain spaces, and MiB. The
// caller resolves the index against a stable inventory before publishing it.
func parseMetaXProcesses(out string) ([]metaxProcessRow, error) {
	var rows []metaxProcessRow
	inTable := false
	started := false
	header := false
	ended := false
	emptyMarker := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "| Process:") {
			if started || ended {
				return nil, fmt.Errorf("MetaX process table contains malformed rows")
			}
			inTable = true
			started = true
			continue
		}
		if !inTable {
			if ended && line != "" && line != "End of Log" {
				return nil, fmt.Errorf("MetaX process table contains trailing data")
			}
			continue
		}
		if strings.HasPrefix(line, "+") {
			if !header || ended {
				return nil, fmt.Errorf("MetaX process table contains malformed rows")
			}
			ended = true
			inTable = false
			continue
		}
		if strings.HasPrefix(line, "End of Log") {
			break
		}
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
			return nil, fmt.Errorf("MetaX process table contains malformed rows")
		}
		fields := strings.Fields(strings.Trim(line, "|"))
		if len(fields) > 0 && strings.Join(fields, " ") == "GPU PID Process Name GPU Memory" {
			header = true
			continue
		}
		if strings.Join(fields, " ") == "Usage(MiB)" || (len(fields) == 1 && strings.Trim(fields[0], "=") == "") {
			continue
		}
		if strings.Join(fields, " ") == "no process found" {
			if emptyMarker || len(rows) != 0 {
				return nil, fmt.Errorf("MetaX process table contains conflicting rows")
			}
			emptyMarker = true
			continue
		}
		if emptyMarker {
			return nil, fmt.Errorf("MetaX process table contains conflicting rows")
		}
		if len(fields) < 4 {
			return nil, fmt.Errorf("MetaX process table contains malformed rows")
		}
		index, indexErr := strconv.Atoi(fields[0])
		pid, pidErr := strconv.Atoi(fields[1])
		mib, memoryErr := strconv.ParseInt(fields[len(fields)-1], 10, 64)
		if indexErr != nil || pidErr != nil || memoryErr != nil || index < 0 || pid <= 0 || mib < 0 || mib > (int64(^uint64(0)>>1)>>20) {
			return nil, fmt.Errorf("MetaX process table contains malformed rows")
		}
		rows = append(rows, metaxProcessRow{
			Index:       index,
			PID:         pid,
			Name:        strings.Join(fields[2:len(fields)-1], " "),
			MemoryBytes: mib << 20,
		})
	}
	if !started || !header || !ended || (!emptyMarker && len(rows) == 0) {
		return nil, fmt.Errorf("MetaX process table is incomplete")
	}
	return rows, nil
}
