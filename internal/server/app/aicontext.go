package app

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/estimate"
	"gpuworks.ai/gpuworks/internal/server/logstore"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// What an error diagnosis is seeded with: the newest lines, where a crash
// prints its traceback, and error-level lines from further back, where the
// first cause often is. The manifest tells the model where the rest is.
const (
	seedTailLines  = 60
	seedErrorLines = 40
	seedEvents     = 12
)

// errorContext gathers the first hop of a diagnosis: the instance, its launch
// command, node, schedule, recent events and logs, and the known-failure
// findings as hypotheses.
func (a *aiService) errorContext(instanceUUID string) (aihelper.ErrorContext, error) {
	in, err := a.store.InstanceByUUID(instanceUUID)
	if err != nil {
		return aihelper.ErrorContext{}, err
	}
	command, _ := instanceLaunchCommand(*in)
	c := aihelper.ErrorContext{
		InstanceUUID: in.UUID, DeploymentID: in.DeploymentID, State: in.State, Phase: in.Phase,
		Message: in.Message, Failure: in.Failure(), Image: in.Image, Command: command, GPUIndexes: instanceGPUs(*in),
		TailLines: seedTailLines, ErrorLines: seedErrorLines,
	}
	if dep, err := a.store.GetDeployment(in.DeploymentID); err == nil {
		c.DeploymentName = dep.Name
	}
	if n, err := a.store.NodeByID(in.NodeID); err == nil {
		c.NodeUUID = n.UUID
		// A node that cannot be profiled is a smaller seed, not a failed answer.
		if profile, err := a.NodeProfile(n.UUID); err == nil {
			c.Node = profile
		}
	}
	if in.ScheduleExpl != "" {
		var expl any
		if json.Unmarshal([]byte(in.ScheduleExpl), &expl) == nil {
			c.Schedule = expl
		}
	}
	if evs, err := a.store.InstanceTimeline(in.UUID, 0, seedEvents); err == nil {
		c.Events = eventLines(evs)
	}
	logs, err := a.seedLogs(in.UUID)
	if err != nil {
		return aihelper.ErrorContext{}, err
	}
	c.Logs = logs
	c.Findings = aihelper.Findings(c.Failure, findingLines(in.Message, logs))
	return c, nil
}

// seedLogs merges an instance's newest lines with its newest error-level
// lines, oldest first. Two reads of one instance's own rows, bounded by their
// limits: what a seed costs does not grow with the fleet.
func (a *aiService) seedLogs(instanceUUID string) ([]aihelper.LogLine, error) {
	if a.obs == nil {
		return nil, nil
	}
	tail, err := a.obs.Logs().Tail(instanceUUID, seedTailLines)
	if err != nil {
		return nil, err
	}
	errs, err := a.obs.Logs().Search(logstore.Filter{Instance: instanceUUID, Level: logstore.LevelError, Limit: seedErrorLines})
	if err != nil {
		return nil, err
	}
	type key struct {
		ts, id int64
	}
	seen := map[key]bool{}
	var merged []logstore.Entry
	for _, e := range append(tail, errs.Entries...) {
		k := key{e.TimeMS, e.ID}
		if seen[k] {
			continue
		}
		seen[k] = true
		merged = append(merged, e)
	}
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].TimeMS != merged[j].TimeMS {
			return merged[i].TimeMS < merged[j].TimeMS
		}
		return merged[i].ID < merged[j].ID
	})
	out := make([]aihelper.LogLine, len(merged))
	for i, e := range merged {
		out[i] = aihelper.LogLine{TimeMS: e.TimeMS, Level: e.Level, Line: e.Line}
	}
	return out, nil
}

// findingLines is what the known-failure table reads for an instance without
// a structured failure reason: the failure message the agent reported, then
// the log lines.
func findingLines(message string, logs []aihelper.LogLine) []string {
	lines := make([]string, 0, len(logs)+1)
	if message != "" {
		lines = append(lines, message)
	}
	for _, l := range logs {
		lines = append(lines, l.Line)
	}
	return lines
}

func eventLines(evs []store.InstanceEvent) []aihelper.EventLine {
	out := make([]aihelper.EventLine, len(evs))
	for i, e := range evs {
		out[i] = aihelper.EventLine{TimeMS: e.CreatedAt.UnixMilli(), Kind: e.Kind, Code: e.Code, Message: e.Message}
	}
	return out
}

// modelContext seeds a model question with its metadata and the fleet fit.
func (a *aiService) modelContext(modelID uint) (aihelper.ModelContext, error) {
	meta, err := a.ModelMeta(modelID)
	if err != nil {
		return aihelper.ModelContext{}, err
	}
	fit, err := a.ModelFit(modelID)
	if err != nil {
		return aihelper.ModelContext{}, err
	}
	return aihelper.ModelContext{ModelID: modelID, Meta: meta, Fit: fit}, nil
}

// layoutSample bounds how many nodes of one card type have their topology
// read for a fit: nodes of one type are usually built alike, and a fit must
// cost the same at a thousand nodes as at ten.
const layoutSample = 8

// fleetFit sizes a model against each accelerator type of the online nodes
// with the estimator's own requirement formula - the figure admission
// enforces - at the default engine's memory utilization, and states what the
// hardware is: usable and broken cards, dtype support, fast interconnect. It
// reads grouped counts (one row per card model per node) and the layouts of
// at most layoutSample nodes per type.
func fleetFit(m *store.Model, groups []store.OnlineGPUGroup, layouts map[uint]store.NodeCardLayout) aihelper.Fit {
	util := deploy.DefaultGPUMemUtil(deploy.DefaultEngine)
	type chip struct{ vendor, name string }
	type agg struct {
		row   aihelper.FitRow
		arch  accelerator.Arch
		cc    string
		nodes []uint
	}
	byChip := map[chip]*agg{}
	var order []chip
	for _, g := range groups {
		c := chip{g.Vendor, g.Name}
		e := byChip[c]
		if e == nil {
			node := store.Node{Framework: g.Framework}
			gpu := store.GPU{Vendor: g.Vendor, Name: g.Name, ArchID: g.ArchID, ComputeCapability: g.ComputeCapability}
			e = &agg{row: aihelper.FitRow{Vendor: node.VendorOf(gpu), Name: g.Name}, arch: node.ArchOf(gpu), cc: g.ComputeCapability}
			byChip[c] = e
			order = append(order, c)
		}
		e.row.Cards += g.Cards
		e.row.Unusable += g.Unusable
		e.row.Nodes++
		e.row.MemoryBytes = max(e.row.MemoryBytes, g.MaxMemory)
		e.row.MaxCardsPerNode = max(e.row.MaxCardsPerNode, g.Cards)
		e.nodes = append(e.nodes, g.NodeID)
	}
	fit := aihelper.Fit{ModelID: m.ID, WeightBytes: m.WeightBytes, Dtype: m.Dtype, GPUMemUtil: util, Rows: make([]aihelper.FitRow, 0, len(order))}
	for _, c := range order {
		e := byChip[c]
		row := e.row
		cc, _ := strconv.ParseFloat(e.cc, 64)
		row.DtypeSupported, row.DtypeNote = estimate.DtypeSupportForGPU(row.Vendor, e.arch.ID, e.arch.Family, m.Dtype, strings.ToLower(m.QuantLabel), cc)
		row.FastLinkCards = fastLinkCards(c.name, e.nodes, layouts)
		row.UsableBytes = int64(float64(row.MemoryBytes) * util)
		reserve := estimate.ReserveBytes(0, e.arch.Family, true, util)
		for tp := 1; tp <= row.MaxCardsPerNode; tp *= 2 {
			need := estimate.RequiredPerGPUBytes(m.WeightBytes, 0, reserve, tp)
			row.TP, row.PerGPUBytes = tp, need
			if need <= row.UsableBytes {
				row.Fits = true
				break
			}
		}
		if !row.Fits {
			row.TP = 0
		}
		row.HeadroomBytes = row.UsableBytes - row.PerGPUBytes
		fit.Rows = append(fit.Rows, row)
	}
	sort.SliceStable(fit.Rows, func(i, j int) bool { return fit.Rows[i].MemoryBytes > fit.Rows[j].MemoryBytes })
	return fit
}

// fastLinkCards is the largest group of one card model a single sampled node
// links by a fast interconnect; 1 when the sampled nodes link them by PCIe
// only, 0 when none reported a topology.
func fastLinkCards(name string, nodes []uint, layouts map[uint]store.NodeCardLayout) int {
	best, known := 0, false
	for _, id := range nodes {
		layout, ok := layouts[id]
		if !ok || layout.Topology == nil {
			continue
		}
		known = true
		for _, clique := range layout.Topology.FastCliques {
			n := 0
			for _, idx := range clique {
				if layout.Names[idx] == name {
					n++
				}
			}
			best = max(best, n)
		}
	}
	if known && best < 1 {
		best = 1
	}
	return best
}

// fitSample picks, per card model, the first layoutSample nodes that carry it.
func fitSample(groups []store.OnlineGPUGroup) []uint {
	perModel := map[string]int{}
	seen := map[uint]bool{}
	var ids []uint
	for _, g := range groups {
		key := g.Vendor + "/" + g.Name
		if perModel[key] >= layoutSample {
			continue
		}
		perModel[key]++
		if !seen[g.NodeID] {
			seen[g.NodeID] = true
			ids = append(ids, g.NodeID)
		}
	}
	return ids
}
