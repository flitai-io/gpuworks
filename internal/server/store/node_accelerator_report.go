package store

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// What a node's status report says about its accelerators, kept apart from
// the rest of the report: the cards, whether the probe that read them
// succeeded, and the topology between them.
//
// None of it touches the node's connection state. A node whose driver
// stopped answering still has an agent on the other end — and is the one an
// operator most needs a terminal on — so a failed probe only records that
// what is shown is as the last successful probe left it.

// acceleratorProbeOK reports whether the agent could read its accelerators.
// A report from an agent that predates the flag counts as a success, which
// is what it meant before the flag existed.
func acceleratorProbeOK(st proto.NodeStatus) bool {
	return st.GPUProbeOK == nil || *st.GPUProbeOK
}

// cardAdoption is how much of a report's card list may be written.
type cardAdoption int

const (
	// adoptNothing: the report says nothing about the cards. A failed
	// probe with no roster at all — a container that does not map PCI
	// sysfs — leaves every card as the last report left it.
	adoptNothing cardAdoption = iota
	// adoptPresence: the probe failed, but it still read the PCI bus, and
	// about that it is authoritative: which cards are in the machine, who
	// holds each one, where it sits. What only the vendor CLI can say —
	// memory, identity, health — keeps the last answer the CLI gave.
	adoptPresence
	// adoptEverything: the probe succeeded and its report is the truth.
	adoptEverything
)

// adoptCards decides how much of this report's card list may be written.
// It is asked once and answers for both the card rows and the card count,
// which have to agree: a node showing two cards and a count of none is a
// node an operator cannot make sense of.
//
// The middle case is the one real hardware taught. A host whose vendor CLI
// never answers used to have its cards written once and then frozen: on
// 198.51.100.31 the rows stood untouched for half an hour while the node
// reported every minute, so a card later handed to a virtual machine, or
// unbound, or pulled out, would never have been noticed. The sibling host
// .32 only escaped that by chance — its npu-smi happened to succeed once
// in the minute its last two cards were passed through, and the capacity
// fell from 2 to 0 on that one lucky report.
func adoptCards(st proto.NodeStatus) cardAdoption {
	switch {
	case acceleratorProbeOK(st):
		return adoptEverything
	case len(st.GPUs) == 0:
		return adoptNothing
	default:
		return adoptPresence
	}
}

// acceleratorReportUpdates adds the node columns the accelerator part of a
// status report decides. adopt is adoptCards' answer: without it the card
// count keeps the value the last adopted report left, because a probe that
// may not write the cards may not restate how many there are either.
//
// failingSince is when the node's probes started failing, if they are
// failing: a failure keeps it, so it says how long the vendor tool has not
// answered rather than restating the time of the latest report.
func acceleratorReportUpdates(updates map[string]any, st proto.NodeStatus, now time.Time, adopt cardAdoption, failingSince *time.Time) {
	if acceleratorProbeOK(st) {
		updates["gpu_probe_error"] = ""
		updates["gpu_probe_failed_at"] = nil
	} else {
		updates["gpu_probe_error"] = truncate(st.GPUProbeError, 512)
		if failingSince == nil {
			updates["gpu_probe_failed_at"] = now
		}
	}
	if adopt == adoptNothing {
		delete(updates, "gpu_count")
		return
	}
	// The count follows ownership, which PCI is authoritative for, so it
	// is written even when the CLI could not be asked.
	if adopt == adoptPresence {
		return
	}
	if st.TopoRaw != "" {
		updates["topo_raw"] = st.TopoRaw
		if tj := parseTopoJSON(st.TopoRaw, st.GPUs); tj != "" {
			updates["topo_json"] = tj
		}
	}
}

// reconcileAcceleratorReport writes the cards of a report adoptCards
// accepted and keeps the instances placed on them pointing at the same
// hardware.
func reconcileAcceleratorReport(tx *gorm.DB, nodeID uint, st proto.NodeStatus, adopt cardAdoption) error {
	renumbered, err := reconcileGPUs(tx, nodeID, st.GPUs, adopt)
	if err != nil {
		return err
	}
	if !renumbered {
		// Only a probe that moved a card can have moved an instance's
		// cards, so a fleet whose cards stay put pays nothing for this.
		return nil
	}
	return realignInstanceGPUs(tx, nodeID, st.GPUs)
}

// newCardRow is the one place a card's row is built from a probe's report.
// It fills everything the row derives — its key, its normalized health
// level, its verdict — so no write path can be the one that forgets.
//
// Not a gorm BeforeSave hook, which was how this was first written: gorm
// runs hooks on the statement's model, and an update written as
// Model(&GPU{}).Updates(&row) runs the hook on that empty model rather
// than on row. The derived columns then went out as their zero values, so
// every card of every node became unschedulable one status report after it
// was first seen — the authoritative fields correct, and a fleet nothing
// would schedule.
func newCardRow(nodeID uint, ident string, g proto.GPUInfo) GPU {
	// The model name is stored normalized and the architecture as raw
	// facts only (GPU's doc); an old agent's arch_family is not read.
	row := GPU{
		NodeID:            nodeID,
		Ident:             ident,
		Index:             g.Index,
		UUID:              g.UUID,
		Name:              accelerator.NormalizeGPUName(g.Name),
		ComputeCapability: g.ComputeCapability,
		Vendor:            g.Vendor,
		ArchID:            accelerator.StoredArchID(g.Vendor, g.ArchID, g.ComputeCapability),
		MemoryTotal:       g.MemoryTotalBytes,
		PowerLimitWatts:   g.PowerLimitWatts,
		PCIeBDF:           g.PCIeBDF,
		NUMANode:          g.NUMANode,
		MIGEnabled:        g.MIGEnabled,
		Ownership:         g.Ownership,
		ForeignProcesses:  encodeCardProcesses(g.ForeignProcesses),
		// The report's Healthy boolean is read here and nowhere else: an
		// agent predating health levels states only that, and this is
		// where it becomes the level every reader uses.
		HealthStatus:  accelerator.CardHealthLevel(g.Ownership, g.HealthStatus, g.Healthy),
		HealthCode:    g.HealthCode,
		HealthMessage: g.HealthMessage,
	}
	return row
}

// cardRowColumns are the fields newCardRow fills, and so the ones an
// update of a card already known writes. A field added to newCardRow and
// forgotten here would be written when a card is first seen and never
// again — TestAnUpdateWritesEveryFieldAReportFills is the guard.
var cardRowColumns = []string{
	"NodeID", "Ident", "Index", "UUID", "Name", "ComputeCapability",
	"Vendor", "ArchID", "MemoryTotal", "PowerLimitWatts", "PCIeBDF", "NUMANode",
	"MIGEnabled", "Ownership", "ForeignProcesses",
	"HealthStatus", "HealthCode", "HealthMessage",
}

// presenceOnlyRow is the row to write when only PCI could be read: the
// card's stored facts, with what PCI read laid over them. It starts from
// the stored row, so writing it whole changes nothing the CLI alone knows —
// above all not the device index, which a probe that could not ask the CLI
// carries as -1 for every card: writing that over a card still serving
// loses the number its instances are bound by, and the card reads as free.
//
// Health travels with ownership rather than with the CLI's facts: a card
// just handed to a virtual machine is no longer this node's to judge, and
// leaving the old verdict on it would keep offering it. A card still this
// node's keeps the health its last successful probe found — the reading
// is stale, which the node's own probe error says, and a stale finding is
// better than none.
func presenceOnlyRow(old, fromPCI GPU) GPU {
	row := old
	row.PCIeBDF = fromPCI.PCIeBDF
	row.NUMANode = fromPCI.NUMANode
	row.Ownership = fromPCI.Ownership
	// A card the CLI has never described has no CLI facts to protect: its
	// UUID is what the CLI hands out, and without one every stored field
	// came from a PCI read. Keeping its name then freezes whatever the
	// first read called it — on 198.51.100.31, "19e5:d802" for cards an
	// agent that carries the model names now calls 910B4.
	describedByCLI := old.UUID != ""
	if !describedByCLI || !accelerator.CardUsable(fromPCI.Ownership) {
		row.Name = fromPCI.Name
	}
	switch {
	case !accelerator.CardUsable(fromPCI.Ownership):
		// Not this node's any more: its health is not ours to state.
		row.HealthStatus, row.HealthCode, row.HealthMessage = accelerator.HealthOK, "", ""
	case !accelerator.CardUsable(old.Ownership):
		// It has just come back — from a virtual machine, or a driver
		// that let go. The old row says nothing a finding explains, which
		// would read as a fault nobody can act on. What this probe knows
		// is that it could not ask.
		row.HealthStatus = fromPCI.HealthStatus
		row.HealthCode, row.HealthMessage = fromPCI.HealthCode, fromPCI.HealthMessage
	}
	row.HealthStatus = accelerator.NormalizeHealthLevel(row.HealthStatus)
	return row
}

// cardIdent is the key a card's row is stored under: its hardware identity
// (accelerator.CardIdent), falling back to the vendor CLI's device index
// for a prober that reports neither a PCI address nor a UUID.
//
// It is derived from the row's own columns, which a business fact may not
// be — it is stored because a key has to be a column to be indexed, and
// because being able to read it back is what makes a mis-keyed card
// debuggable. GPU.BeforeSave fills it for any caller that does not.
//
// The fallback is deliberately different from an instance's binding, which
// stores no identity at all in that case: there, "index:3" would be a
// binding to the very thing that drifts. Here every card needs some key to
// be stored under, and a prober that knows only indexes gives rows that
// behave exactly as they did before identities existed.
func cardIdent(g proto.GPUInfo) string {
	if ident := accelerator.CardIdent(g.UUID, g.PCIeBDF); ident != "" {
		return ident
	}
	return "index:" + strconv.Itoa(g.Index)
}

// reconcileGPUs writes the probe's cards and reports whether any card it
// already knew now carries a different device index, which is what makes
// the instances bound to those cards need realigning (realignInstanceGPUs).
//
// Rows are keyed by cardIdent, not by device index. An index is what the
// vendor CLI hands out: it is absent for a card the CLI does not list (one
// passed through, or one its driver could not bring up) and it shifts for
// every card after one that appears or goes. Keying on it is what used to
// make this function juggle rows through negative indexes to get past its
// own unique constraint.
func reconcileGPUs(tx *gorm.DB, nodeID uint, gpus []proto.GPUInfo, adopt cardAdoption) (renumbered bool, err error) {
	var current []GPU
	if err := tx.Where("node_id = ?", nodeID).Find(&current).Error; err != nil {
		return false, err
	}
	existing := make(map[string]GPU, len(current))
	for _, row := range current {
		existing[row.Ident] = row
	}
	reported := make(map[string]bool, len(gpus))
	for _, g := range gpus {
		ident := cardIdent(g)
		reported[ident] = true
		row := newCardRow(nodeID, ident, g)
		old, known := existing[ident]
		if !known {
			if err := tx.Create(&row).Error; err != nil {
				return false, err
			}
			// A second card reporting the same key would otherwise take
			// this path again and fail the unique index, and because the
			// cards are written inside the node's own status transaction
			// that would roll back the node's state and last-seen too:
			// one odd card row freezing the whole node.
			existing[ident] = row
			continue
		}
		if adopt == adoptPresence {
			// The CLI could not be asked, so the card keeps what it last
			// said about itself and takes only what PCI read.
			row = presenceOnlyRow(old, row)
		} else if old.Index != g.Index {
			renumbered = true
		}
		if sameCardRow(old, row) {
			// Every node reports every minute and a card's facts rarely
			// change between two reports: rewriting them anyway was one
			// write per card per minute across the fleet.
			continue
		}
		if err := tx.Model(&GPU{}).Where("id = ?", old.ID).Select(cardRowColumns).
			Updates(&row).Error; err != nil {
			return false, err
		}
	}
	for _, old := range current {
		if !reported[old.Ident] {
			if err := tx.Delete(&GPU{}, old.ID).Error; err != nil {
				return false, err
			}
		}
	}
	if err := expireCardAllowances(tx, current, time.Now().UTC()); err != nil {
		return false, err
	}
	return renumbered, nil
}

// sameCardRow reports whether writing row over old would change any column
// a report writes (cardRowColumns). The bookkeeping columns are not facts
// of the card and are taken from old.
func sameCardRow(old, row GPU) bool {
	row.ID, row.CreatedAt, row.UpdatedAt = old.ID, old.CreatedAt, old.UpdatedAt
	row.SchedulingStopped, row.SchedulingAllowedAt = old.SchedulingStopped, old.SchedulingAllowedAt
	return row == old
}

// ownedCardCount is how many of the reported cards this node may use. The
// count is capacity, so a card passed through to a virtual machine or left
// without a driver does not add to it; the card itself is still stored, and
// the node's detail lists it.
func ownedCardCount(gpus []proto.GPUInfo) int {
	owned := 0
	for _, g := range gpus {
		if accelerator.CardUsable(g.Ownership) {
			owned++
		}
	}
	return owned
}

// cardOrder is how a node's cards are listed, wherever they are loaded.
//
// It has to be said out loud. The order used to fall out of the unique
// index on the device index, and when that key became the card's identity
// the lists silently reordered — a node's cards came back sorted by PCI
// address, which is not the order anyone reads them in. Device order is
// what an operator sees in nvidia-smi and what every log line names, so
// the cards the CLI numbered come first in that order, and the ones it
// does not number follow by PCI address.
const cardOrder = "CASE WHEN `index` < 0 THEN 1 ELSE 0 END, `index`, ident"

// preloadCards loads a node's cards in cardOrder.
func preloadCards(db *gorm.DB) *gorm.DB {
	return db.Preload("GPUs", func(cards *gorm.DB) *gorm.DB {
		return cards.Order(cardOrder)
	})
}

// withCards runs a node query with its cards preloaded as one read. The
// preload is a second statement, and between two separate statements a status
// report can commit, pairing a node row from before the report with the cards
// it wrote (a dGPU capability then derived from cards the node row does not
// know the runtime of yet). On the read pool both statements run in one
// transaction and so read one snapshot. The writer pool is a single connection
// that every write goes through, so holding it across both statements already
// keeps any write out, without a BEGIN and COMMIT on each lookup. A store
// bound to a transaction or a held connection (&Store{db: tx} inside a write
// transaction) already runs both statements on it: asking its pool for a
// connection there would wait forever on the one that transaction holds.
func (s *Store) withCards(db *gorm.DB, query func(*gorm.DB) error) error {
	run := func(tx *gorm.DB) error { return query(preloadCards(tx)) }
	if _, pool := db.Statement.ConnPool.(*sql.DB); !pool {
		return run(db)
	}
	if db == s.db {
		return db.Connection(run)
	}
	return db.Transaction(run)
}

// encodeCardProcesses stores the card's foreign processes as the snapshot
// they are: read and written whole, never queried into. An empty list is
// stored as "" so a card nobody else is using carries nothing.
func encodeCardProcesses(procs []proto.CardProcess) string {
	if len(procs) == 0 {
		return ""
	}
	raw, err := json.Marshal(procs)
	if err != nil {
		return ""
	}
	return string(raw)
}
