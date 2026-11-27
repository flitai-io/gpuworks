package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

// UpdateNodeStatus applies a status.node report: host summary + GPU set.
// GPUs are reconciled (upsert by node+index, delete missing) in one transaction.
func (s *Store) UpdateNodeStatus(uuid string, st proto.NodeStatus) error {
	cordoned, uncordoned := false, false
	cordonedFor := ""
	var heldBy SchedulingSource
	placementChanged := false
	var displayNameChanged bool
	var factsChangedOf uint // the node's id when the report changed its or its cards' hold facts
	cardHoldsChanged := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var n Node
		if err := tx.Where("uuid = ?", uuid).First(&n).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		gpuVendor := nodeGPUVendor(st)
		gpuDriver := accelerator.NormalizeDriverVersion(gpuVendor, nodeGPUDriver(st))
		nvidiaDriver := st.Sys.NvidiaDriver
		if nvidiaDriver == "" && gpuVendor == "nvidia" {
			nvidiaDriver = gpuDriver
		}
		updates := map[string]any{
			"hostname":                  st.Sys.Hostname,
			"os":                        st.Sys.OS,
			"kernel":                    st.Sys.Kernel,
			"arch":                      st.Sys.Arch,
			"cpu_model":                 st.Sys.CPUModel,
			"cpu_cores":                 st.Sys.CPUCores,
			"mem_total_kb":              st.Sys.MemTotalKB,
			"disk_total_kb":             st.Sys.DiskTotalKB,
			"gpu_vendor":                gpuVendor,
			"gpu_driver":                gpuDriver,
			"nvidia_driver":             nvidiaDriver,
			"framework":                 st.Sys.Framework,
			"framework_version":         st.Sys.FrameworkVersion,
			"gpu_count":                 ownedCardCount(st.GPUs),
			"partition_safety_protocol": st.Sys.PartitionSafetyProtocol,
			"state":                     NodeStateOnline,
			"last_seen_at":              now,
		}
		if len(st.PathUsage) > 0 {
			if b, err := json.Marshal(st.PathUsage); err == nil {
				updates["path_usage"] = string(b)
				updates["path_usage_observed_at"] = now
			}
		}
		adopt := adoptCards(st)
		acceleratorReportUpdates(updates, st, now, adopt, n.GPUProbeFailedAt)
		if st.Sys.RuntimeDriver != "" {
			updates["runtime_driver"] = st.Sys.RuntimeDriver
		}
		updates["runtime_plugin"] = st.Sys.RuntimePlugin
		updates["runtime_gpu_blocked"] = truncate(st.Sys.RuntimeGPUBlocked, 64)
		updates["peer_memory"] = truncate(st.Sys.PeerMemory, 16)
		updates["runtime_fallback_from"] = truncate(st.Sys.RuntimeFallbackFrom, 16)
		updates["runtime_fallback_reason"] = truncate(st.Sys.RuntimeFallbackReason, 64)
		runtimeFallbackSocketUpdates(updates, st.Sys)
		updates["accelerator_vendors"] = encodeAcceleratorVendors(st.Sys.AcceleratorVendors)
		updates["kube_unschedulable"] = st.Sys.KubeUnschedulable
		if n.SlurmClusterID == 0 {
			// An enrolled node's Slurm name is what enrollment found it by:
			// a report that cannot say it (Slurm not asked yet) must not
			// blank it, nor may one that says another.
			updates["slurm_node"] = truncate(st.Sys.SlurmNode, 64)
		}
		updates["slurm_unavailable"] = truncate(st.Sys.SlurmUnavailable, 32)
		updates["slurm_reason"] = truncate(st.Sys.SlurmReason, 256)
		// The platform's and the holders' half of NodeScheduling: a holder
		// cordons a node while it holds it, the platform while it has a
		// reason of its own, and each lifts only the cordons it set
		// (node_cordon.go). A node whose runtime cannot be asked at all
		// keeps the platform's cordon: an agent that cannot say is not a
		// reason to let work onto the node, nor to take it off. Holds are
		// decided on the facts this report writes, whatever the runtime.
		runtimeKnown := st.Sys.RuntimeDriver != "" || st.Sys.RuntimeGPUBlocked != ""
		cordonReason := PlatformCordonReason(st.Sys.RuntimeGPUBlocked, st.Sys.KubeUnschedulable, st.Sys.SlurmUnavailable, st.GPUs)
		facts := nodeHoldFacts(&n)
		facts.GPUVendor = gpuVendor
		if count, ok := updates["gpu_count"].(int); ok {
			facts.OwnedCards = count
		}
		if facts.OwnedCards != n.GPUCount || facts.GPUVendor != n.GPUVendor {
			factsChangedOf = n.ID
		}
		hold, _ := s.admission.holdOf(facts)
		if d := decideScheduling(n.Scheduling, n.SchedulingBy, cordonReason, runtimeKnown, hold); d.changed {
			updates["scheduling"] = d.scheduling
			updates["scheduling_by"] = d.source
			cordoned, uncordoned, cordonedFor = d.scheduling == NodeCordoned, d.scheduling == NodeSchedulable, d.reason
			if isHolderSource(d.source) {
				heldBy = d.source
			}
		}
		if len(st.Sys.Runtimes) > 0 {
			if b, err := json.Marshal(st.Sys.Runtimes); err == nil {
				updates["runtimes"] = string(b)
			}
		}
		if b, err := json.Marshal(st.Sys.PartitionAssets); err == nil {
			// Asset reports are complete snapshots. Persisting [] when an old or
			// asset-less agent reports prevents stale readiness after a restart.
			updates["partition_assets"] = string(b)
		}
		if b, err := json.Marshal(st.Sys.PartitionProbes); err == nil {
			// Like assets, probe results are a complete snapshot. A nil slice is
			// deliberately persisted as null so the server can recognize an old
			// agent instead of retaining stale readiness.
			updates["partition_probes"] = string(b)
		}
		if st.Network != nil {
			if b, err := json.Marshal(st.Network); err == nil {
				updates["network_inventory"] = string(b)
			}
		}
		displayNameChanged = strings.TrimSpace(n.Name) == "" && n.Hostname != st.Sys.Hostname
		placementChanged = nodePlacementFactsChanged(n, updates)
		if err := tx.Model(&Node{}).Where("id = ?", n.ID).Updates(updates).Error; err != nil {
			return err
		}
		if adopt != adoptNothing {
			// The card holders answer on the facts this report writes,
			// as the node's holders do above.
			var holdOf func(*GPU) SchedulingSource
			if len(s.admission.cardSchedulingHolders()) > 0 {
				holdOf = func(g *GPU) SchedulingSource { return s.admission.cardHoldSource(facts, g) }
			}
			cards, err := reconcileAcceleratorReport(tx, n.ID, st, adopt, holdOf)
			if err != nil {
				return err
			}
			placementChanged = placementChanged || cards.changed
			if cards.factsChanged {
				factsChangedOf = n.ID
			}
			cardHoldsChanged = cards.holdsChanged
		}
		return reconcileImagePresence(tx, n.ID, st.Images)
	})
	if err != nil {
		return err
	}
	// An agent that predates storages reports only its models root; record it
	// as the default-directory local storage's state on the node. Newer agents
	// report storages on their own and leave the field empty.
	if root := strings.TrimSpace(st.Sys.ModelsRoot); root != "" {
		if node, nodeErr := s.NodeByUUID(uuid); nodeErr == nil {
			_ = s.RecordLegacyModelsRoot(node.ID, root)
		}
	}
	if displayNameChanged {
		s.fleetNodes.displayNames.Add(1)
	}
	if placementChanged {
		// What placement decides on changed: the node snapshot's next read
		// sees it, not one after the report window (fleet_nodes.go). The
		// event stays a routine report for its subscribers, which batch
		// those on their own.
		s.fleetNodes.changes.Add(1)
	}
	s.publishNode(uuid, NodeChangeStatus)
	s.publishNodeFleet(factsChangedOf, NodeFactsChanged)
	if cordoned {
		// The platform or a holder, not an operator, took the node out of
		// scheduling: say so where an operator looks, and to every reader
		// of scheduling changes.
		if heldBy != SchedulingBySelf {
			slog.Warn("node held out of scheduling", "node", uuid, "holder", heldBy)
		} else {
			slog.Warn("node cordoned by the platform", "node", uuid, "reason", cordonedFor,
				"runtime_gpu_blocked", st.Sys.RuntimeGPUBlocked)
		}
		s.publishNode(uuid, NodeChangeScheduling)
	}
	if uncordoned {
		slog.Info("node schedulable again: the reason for cordoning it is gone",
			"node", uuid, "runtime", st.Sys.RuntimeDriver)
		s.publishNode(uuid, NodeChangeScheduling)
	}
	if cardHoldsChanged && !cordoned && !uncordoned {
		// A card holder held or released a card: placing work reads the
		// cards through the node, as after an operator's card decision.
		s.publishNode(uuid, NodeChangeScheduling)
	}
	return nil
}

// NodeChangeStatus is the change reason of a node's routine status report
// (UpdateNodeStatus), which every agent sends once a minute.
const NodeChangeStatus = "status"

// nodePlacementColumns are the node columns a status report writes that
// placement decides on: the runtime, partition and network facts and the
// accelerator probe's verdict. Left out are what changes on most reports and
// is only shown or read as a measurement (timestamps, the hostname and OS,
// statfs path usage: a disk that fills is seen after the report window), and
// the platform's cordon, which is announced on its own.
var nodePlacementColumns = []string{
	"gpu_count", "gpu_vendor", "gpu_driver", "framework", "framework_version",
	"mem_total_kb", "partition_safety_protocol", "runtime_driver", "runtime_plugin",
	"runtime_gpu_blocked", "peer_memory", "accelerator_vendors", "runtimes",
	"partition_assets", "partition_probes", "network_inventory", "gpu_probe_error",
}

// nodePlacementFactsChanged reports whether writing updates over n changes a
// column placement decides on (nodePlacementColumns). The row was read in the
// same transaction, so this costs no read.
func nodePlacementFactsChanged(n Node, updates map[string]any) bool {
	stored := map[string]any{
		"gpu_count": n.GPUCount, "gpu_vendor": n.GPUVendor, "gpu_driver": n.GPUDriver,
		"framework": n.Framework, "framework_version": n.FrameworkVersion,
		"mem_total_kb": n.MemTotalKB, "partition_safety_protocol": n.PartitionSafetyProtocol,
		"runtime_driver": n.RuntimeDriver, "runtime_plugin": n.RuntimePlugin,
		"runtime_gpu_blocked": n.RuntimeGPUBlocked, "peer_memory": n.PeerMemory,
		"accelerator_vendors": n.AcceleratorVendors, "runtimes": n.Runtimes,
		"partition_assets": n.PartitionAssets, "partition_probes": n.PartitionProbes,
		"network_inventory": n.NetworkInventory, "gpu_probe_error": n.GPUProbeError,
	}
	for _, col := range nodePlacementColumns {
		v, written := updates[col]
		if written && fmt.Sprint(v) != fmt.Sprint(stored[col]) {
			return true
		}
	}
	return false
}

func nodeGPUVendor(st proto.NodeStatus) string {
	if st.Sys.GPUVendor != "" {
		return st.Sys.GPUVendor
	}
	if st.Sys.NvidiaDriver != "" {
		return "nvidia"
	}
	for _, g := range st.GPUs {
		if g.Vendor != "" {
			return g.Vendor
		}
	}
	return ""
}

func nodeGPUDriver(st proto.NodeStatus) string {
	if st.Sys.GPUDriver != "" {
		return st.Sys.GPUDriver
	}
	if st.Sys.NvidiaDriver != "" {
		return st.Sys.NvidiaDriver
	}
	for _, g := range st.GPUs {
		if g.DriverVersion != "" {
			return g.DriverVersion
		}
	}
	return ""
}

// publishNode tells the bus this node changed, naming what changed it.
func (s *Store) publishNode(uuid, reason string) {
	s.noteNodeChange(reason)
	if s.Bus == nil {
		return
	}
	s.Bus.Publish(bus.Event{Topic: "node", Key: uuid, Reason: reason})
}

// MarkStaleOffline flips nodes whose last-seen is older than the cutoff to
// offline. Used as a safety net; the hub also flips state on disconnect.
func (s *Store) MarkStaleOffline(ctx context.Context, olderThan time.Duration) error {
	cutoff := time.Now().UTC().Add(-olderThan)
	var nodes []Node
	if err := s.db.WithContext(ctx).Select("uuid").
		Where("state = ? AND (last_seen_at IS NULL OR last_seen_at < ?)", NodeStateOnline, cutoff).
		Find(&nodes).Error; err != nil {
		return err
	}
	for _, node := range nodes {
		if err := s.SetNodeState(node.UUID, NodeStateOffline); err != nil {
			return err
		}
	}
	return nil
}
