package store

import (
	"context"
	"encoding/json"
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
		updates["accelerator_vendors"] = encodeAcceleratorVendors(st.Sys.AcceleratorVendors)
		updates["kube_unschedulable"] = st.Sys.KubeUnschedulable
		// The platform's half of NodeScheduling: it cordons a schedulable
		// node while it has a reason of its own, and lifts only the cordons
		// it set (node_cordon.go). A node whose runtime cannot be asked at
		// all keeps whatever it has: an agent that cannot say is not a
		// reason to let work onto the node, nor to take it off.
		runtimeKnown := st.Sys.RuntimeDriver != "" || st.Sys.RuntimeGPUBlocked != ""
		cordonReason := PlatformCordonReason(st.Sys.RuntimeGPUBlocked, st.Sys.KubeUnschedulable, st.GPUs)
		if d := decideCordon(n.Scheduling, n.SchedulingBy, cordonReason); d.changed && (runtimeKnown || cordonReason != "") {
			updates["scheduling"] = d.scheduling
			updates["scheduling_by"] = d.source
			cordoned, uncordoned, cordonedFor = d.scheduling == NodeCordoned, d.scheduling == NodeSchedulable, d.reason
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
		if err := tx.Model(&Node{}).Where("id = ?", n.ID).Updates(updates).Error; err != nil {
			return err
		}
		if adopt != adoptNothing {
			if err := reconcileAcceleratorReport(tx, n.ID, st, adopt); err != nil {
				return err
			}
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
	s.publishNode(uuid, NodeChangeStatus)
	if cordoned {
		// The platform, not an operator, took the node out of scheduling:
		// say so where an operator looks, and to every reader of
		// scheduling changes.
		slog.Warn("node cordoned by the platform", "node", uuid, "reason", cordonedFor,
			"runtime_gpu_blocked", st.Sys.RuntimeGPUBlocked)
		s.publishNode(uuid, NodeChangeScheduling)
	}
	if uncordoned {
		slog.Info("node schedulable again: the platform's reason for cordoning it is gone",
			"node", uuid, "runtime", st.Sys.RuntimeDriver)
		s.publishNode(uuid, NodeChangeScheduling)
	}
	return nil
}

// NodeChangeStatus is the change reason of a node's routine status report
// (UpdateNodeStatus), which every agent sends once a minute.
const NodeChangeStatus = "status"

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
