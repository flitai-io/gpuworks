package api

import (
	"net/http"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// dashboardSummary answers GET /dashboard/summary: the fleet counts and
// capacity groups the dashboard shows, so the page no longer fetches every
// node and deployment to count them. label_key picks the label the capacity
// is grouped by.
func (a *apiHandlers) dashboardSummary(w http.ResponseWriter, r *http.Request) {
	nodes, err := a.d.Store.ListNodes()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	deps, err := a.d.Store.ListDeployments()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	instances, err := a.d.Store.ListInstances()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	rows, err := deployment.NewListRows(a.d.Store, a.d.Preparation, deps, instances)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := summarizeNodes(nodes, strings.TrimSpace(r.URL.Query().Get("label_key")))
	out.CurrentMetrics = a.dashboardCurrentMetrics(nodes)
	out.Cards = a.dashboardCards()
	out.Tasks = a.dashboardTasks(r)
	out.Deployments.Total = len(deps)
	for i := range deps {
		d := &deps[i]
		if d.Sleepable {
			out.Deployments.Sleepable++
		}
		// A sleeping or asleep deployment serves nothing, so it is counted as
		// sleeping, not running; whatever no bucket names is Other.
		insts := rows.Instances(d.ID)
		switch effectiveDeploymentState(d, deployment.RuntimeState(rows, d, insts, deployment.RecordedModelProgress(rows, d)), deploymentFactsFor(rows, d, insts)) {
		case "running":
			out.Deployments.Running++
		case "pulling", "starting":
			out.Deployments.Warming++
		case deploymentStateSleeping, deploymentStateAsleep:
			out.Deployments.Sleeping++
		case "pending":
			out.Deployments.Pending++
		default:
			out.Deployments.Other++
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// dashboardCards is the scheduling overview's fleet summary, read from its
// cache so a dashboard poll does not rebuild the overview; nil when it cannot
// be built, which the dashboard shows as unknown rather than as no cards.
func (a *apiHandlers) dashboardCards() *dto.DashboardCards {
	snap, err := a.currentOverview()
	if err != nil {
		if a.d.Log != nil {
			a.d.Log.Warn("dashboard card summary unavailable", "error", err)
		}
		return nil
	}
	return &dto.DashboardCards{OverviewSummary: snap.summary, Allocated: snap.summary.Awake + snap.summary.Asleep}
}

// dashboardTasks counts the batches the caller may see by status, and the
// evaluation runs in flight, each in one count query rather than a list read.
func (a *apiHandlers) dashboardTasks(r *http.Request) dto.DashboardTasks {
	var out dto.DashboardTasks
	if counter, ok := a.d.Batches.(BatchCounter); ok {
		counts, err := counter.CountByStatus(a.batchCaller(r), []string{"validating", "in_progress", "finalizing"})
		if err == nil {
			out.Batches = &dto.DashboardBatches{Active: counts["in_progress"] + counts["finalizing"], Waiting: counts["validating"]}
		} else if a.d.Log != nil {
			a.d.Log.Warn("dashboard batch count unavailable", "error", err)
		}
	}
	if n, err := a.d.Store.CountActiveBenchmarkRuns(); err == nil {
		out.Benchmarks = int(n)
	} else if a.d.Log != nil {
		a.d.Log.Warn("dashboard benchmark count unavailable", "error", err)
	}
	return out
}

// BatchCounter is the count side of a BatchService: the batches the caller
// may see in each of the states, counted without reading them; a state with
// none is absent. The app's batch console implements it.
type BatchCounter interface {
	CountByStatus(c BatchCaller, states []string) (map[string]int, error)
}

// nodeCapacity is what one node adds to a capacity group.
type nodeCapacity struct {
	gpus, unusable int
	vramGiB        float64
}

// capacityOf sums what the fleet can actually run on. Cards owned
// elsewhere (passed through, or with no driver) are part of the node's
// hardware and are shown in its detail, but they are nobody's capacity and
// counting them as unhealthy would report a fleet-wide fault that is not
// there.
func capacityOf(n *store.Node) nodeCapacity {
	owned := n.OwnedGPUs()
	c := nodeCapacity{gpus: n.GPUCount}
	for i := range owned {
		c.vramGiB += float64(owned[i].MemoryTotal) / bytesPerGiB
		if !owned[i].CanTakeWork() {
			c.unusable++
		}
	}
	return c
}

// summarizeNodes counts the fleet and groups its capacity by vendor and by
// the label key asked for.
func summarizeNodes(nodes []store.Node, wantKey string) dto.DashboardSummary {
	out := dto.DashboardSummary{ByVendor: []dto.CapacityGroup{}, LabelKeys: []string{}, ByLabel: []dto.CapacityGroup{}}
	labels := make([]map[string]string, len(nodes))
	keyCounts := map[string]int{}
	byVendor := capacityGroups{}
	for i := range nodes {
		n := &nodes[i]
		switch n.ConnectionState() {
		case store.NodeStateOnline:
			out.Nodes.Online++
		case store.NodeStateOffline:
			out.Nodes.Offline++
		case store.NodeStateConnecting:
			out.Nodes.Connecting++
		}
		c := capacityOf(n)
		out.Nodes.GPUs += c.gpus
		out.Nodes.UnusableGPUs += c.unusable
		out.Nodes.VRAMGiB += c.vramGiB
		out.Nodes.MemoryGiB += float64(n.MemTotalKB) * 1024 / bytesPerGiB
		owned := n.OwnedGPUs()
		if len(owned) == 0 {
			vendor := nodeDTOGPUVendor(n)
			if vendor == "" {
				vendor = "unknown"
			}
			byVendor.add(dto.CapacityGroup{Key: vendor}, c)
		} else {
			vendorCapacity := map[string]nodeCapacity{}
			for _, gpu := range owned {
				vendor := gpu.Vendor
				if vendor == "" {
					vendor = nodeDTOGPUVendor(n)
				}
				if vendor == "" {
					vendor = "unknown"
				}
				card := vendorCapacity[vendor]
				card.gpus++
				card.vramGiB += float64(gpu.MemoryTotal) / bytesPerGiB
				if !gpu.CanTakeWork() {
					card.unusable++
				}
				vendorCapacity[vendor] = card
			}
			for vendor, card := range vendorCapacity {
				byVendor.add(dto.CapacityGroup{Key: vendor}, card)
			}
		}
		// A node whose labels cannot be read has none, as the DTO shows it.
		labels[i], _ = store.NodeLabels(n.Labels)
		for key := range labels[i] {
			keyCounts[key]++
		}
	}
	out.Nodes.Total = len(nodes)
	out.ByVendor = byVendor.sorted()
	for key := range keyCounts {
		out.LabelKeys = append(out.LabelKeys, key)
	}
	sort.Slice(out.LabelKeys, func(i, j int) bool {
		a, b := out.LabelKeys[i], out.LabelKeys[j]
		return keyCounts[a] > keyCounts[b] || keyCounts[a] == keyCounts[b] && a < b
	})
	// The picked key falls back to the most widely used one, also when a
	// label edit removed it from every node.
	if keyCounts[wantKey] > 0 {
		out.LabelKey = wantKey
	} else if len(out.LabelKeys) > 0 {
		out.LabelKey = out.LabelKeys[0]
	}
	if out.LabelKey != "" {
		byLabel := capacityGroups{}
		for i := range nodes {
			group := dto.CapacityGroup{Unlabeled: true}
			if value, ok := labels[i][out.LabelKey]; ok {
				group = dto.CapacityGroup{Key: out.LabelKey + "=" + value}
			}
			byLabel.add(group, capacityOf(&nodes[i]))
		}
		out.ByLabel = byLabel.sorted()
	}
	return out
}

// capacityGroups accumulates groups by key; the unlabeled group has its own
// slot so no label value can collide with it.
type capacityGroups map[string]*dto.CapacityGroup

func (g capacityGroups) add(group dto.CapacityGroup, c nodeCapacity) {
	slot := group.Key
	if group.Unlabeled {
		slot = "\x00unlabeled"
	}
	row := g[slot]
	if row == nil {
		row = &group
		g[slot] = row
	}
	row.Nodes++
	row.GPUs += c.gpus
	row.VRAMGiB += c.vramGiB
	row.UnusableGPUs += c.unusable
}

// sorted lists the groups most GPUs first, then by key.
func (g capacityGroups) sorted() []dto.CapacityGroup {
	out := make([]dto.CapacityGroup, 0, len(g))
	for _, row := range g {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GPUs != out[j].GPUs {
			return out[i].GPUs > out[j].GPUs
		}
		return out[i].Key < out[j].Key
	})
	return out
}
