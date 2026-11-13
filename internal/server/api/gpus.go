package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/partitioncap"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// MetricLatest is the API-independent current-point contract supplied by the
// observability store.
type MetricLatest struct {
	Labels map[string]string
	Value  float64
	TimeMS int64
}

type gpuInventoryCache struct {
	physical dto.GPUListResponse
	dgpu     dto.DGPUListResponse
}

// The GPU inventory is rebuilt at most every gpuInventoryCacheTTL, and sooner
// - but no more often than gpuInventoryRefreshFloor - once an instance or a
// deployment changed since it was built: an instance claims or frees a card,
// and a deployment's dGPU limit is what a slice still under an older one
// shows it gets at restart. The page is refreshed on those events, so a 20 s
// cache that ignored them answered the refresh with the cards as they were
// before the change. The floor bounds a fleet whose instances change state
// every second to one rebuild per interval.
const (
	gpuInventoryCacheTTL     = 20 * time.Second
	gpuInventoryRefreshFloor = 2 * time.Second
)

// gpuInventoryTopics are the bus topics whose changes invalidate it.
var gpuInventoryTopics = []string{"instance", "deployment"}

// listGPUs answers GET /gpus. With page or page_size it returns one
// dto.GPUPage (see gpuPage); without them, every GPU with the overview.
func (a *apiHandlers) listGPUs(w http.ResponseWriter, r *http.Request) {
	query, err := parseListQuery(r, nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	filter, err := parseGPUListFilter(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	order, err := gpuListSorters.parse(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	inventory, err := a.currentGPUInventory()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !query.paged {
		writeJSON(w, http.StatusOK, inventory.physical)
		return
	}
	writeJSON(w, http.StatusOK, gpuPage(inventory.physical, query, filter, order))
}

func (a *apiHandlers) listDGPUs(w http.ResponseWriter, _ *http.Request) {
	inventory, err := a.currentGPUInventory()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, inventory.dgpu)
}

func (a *apiHandlers) currentGPUInventory() (*gpuInventoryCache, error) {
	a.gpuCacheMu.Lock()
	defer a.gpuCacheMu.Unlock()
	generation := a.inventoryGeneration()
	if a.gpuCache != nil {
		age := time.Since(a.gpuCacheAt)
		if age < gpuInventoryCacheTTL && (generation == a.gpuCacheGeneration || age < gpuInventoryRefreshFloor) {
			return a.gpuCache, nil
		}
	}
	inventory, err := a.buildGPUInventory()
	if err != nil {
		return nil, err
	}
	a.gpuCache = inventory
	a.gpuCacheAt = time.Now().UTC()
	a.gpuCacheGeneration = generation
	return inventory, nil
}

// instanceGeneration is the bus's count of instance state changes, 0 without a
// bus.
func (a *apiHandlers) inventoryGeneration() uint64 {
	if a.d.Bus == nil {
		return 0
	}
	var generation uint64
	for _, topic := range gpuInventoryTopics {
		generation += a.d.Bus.Generation(topic)
	}
	return generation
}

func (a *apiHandlers) buildGPUInventory() (*gpuInventoryCache, error) {
	nodes, err := a.d.Store.ListNodes()
	if err != nil {
		return nil, err
	}
	metrics, err := a.loadLatestGPUMetrics()
	if err != nil {
		return nil, err
	}
	deployments := map[uint]*store.Deployment{}
	deployment := func(id uint) *store.Deployment {
		if found, ok := deployments[id]; ok {
			return found
		}
		found, _ := a.d.Store.GetDeployment(id)
		deployments[id] = found
		return found
	}

	out := &gpuInventoryCache{
		physical: dto.GPUListResponse{Items: []dto.PhysicalGPU{}},
		dgpu:     dto.DGPUListResponse{Items: []dto.DGPU{}},
	}
	for nodeIndex := range nodes {
		node := &nodes[nodeIndex]
		allocations := planning.GPUAllocations(a.d.Store, node)
		partitionReports := partitioncap.Decode(node)
		gpuByIndex := map[int]store.GPU{}
		// Every card the node has, not only the ones it may use. A card
		// lent to a virtual machine or held by another driver is hardware
		// somebody has to be able to find, and leaving it out of the one
		// page called "GPU" sent operators looking for cards the platform
		// could see all along. Why it takes no work is on the card
		// itself — its ownership, its health, the processes holding it —
		// and the capacity counts still follow OwnedGPUs.
		for _, gpu := range node.GPUs {
			if gpu.Index >= 0 {
				// Only the cards the CLI numbered can be addressed by
				// index, which is how an instance names the cards it runs
				// on; the rest would collide on -1.
				gpuByIndex[gpu.Index] = gpu
			}
			allocation := allocations[gpu.Index]
			item := physicalGPU(node, partitionReports, &gpu, allocation, deployment, metrics)
			out.physical.Items = append(out.physical.Items, item)
			accumulateGPUOverview(&out.physical.Overview, &gpu, &item)
		}
		// A slice is listed from the moment it holds its quota on the card -
		// pulling, starting, running or asleep - which is the ledger's own
		// list of the card's claims: listing only running instances left the
		// page empty for the minutes a new slice spends starting, while the
		// card already showed it allocated.
		gpuIndexes := make([]int, 0, len(allocations))
		for gpuIndex := range allocations {
			gpuIndexes = append(gpuIndexes, gpuIndex)
		}
		sort.Ints(gpuIndexes)
		for _, gpuIndex := range gpuIndexes {
			gpu := gpuByIndex[gpuIndex]
			for instanceIndex := range allocations[gpuIndex].Instances {
				instance := &allocations[gpuIndex].Instances[instanceIndex]
				var spec proto.InstanceDesiredState
				if json.Unmarshal([]byte(instance.DesiredStateJSON), &spec) != nil || spec.VRAMLimitBytes <= 0 {
					continue
				}
				row := dto.DGPU{
					InstanceUUID: instance.UUID, DeploymentID: instance.DeploymentID, State: instance.State, SleepLevel: instance.SleepLevel,
					NodeUUID: node.UUID, NodeName: node.DisplayName(), GPUIndex: gpuIndex,
					GPUUUID: gpu.UUID, GPUName: gpu.Name, MemoryLimit: spec.VRAMLimitBytes, SMLimitPct: spec.SMLimitPct,
				}
				if dep := deployment(instance.DeploymentID); dep != nil {
					row.DeploymentName = dep.Name
					if dep.VRAMLimitBytes > 0 && dep.VRAMLimitBytes != spec.VRAMLimitBytes {
						row.PendingLimit = dep.VRAMLimitBytes
					}
				}
				applyDGPUMetrics(&row, metrics, node.UUID, gpuIndex, instance.UUID)
				out.dgpu.Items = append(out.dgpu.Items, row)
			}
		}
	}
	return out, nil
}

func physicalGPU(node *store.Node, partitionReports partitioncap.Reports, gpu *store.GPU, allocation planning.GPUAllocation, deployment func(uint) *store.Deployment, metrics map[string]map[string]MetricLatest) dto.PhysicalGPU {
	available := gpu.MemoryTotal - allocation.AllocatedBytes - allocation.SleepingBytes
	if available < 0 || node.State != store.NodeStateOnline {
		available = 0
	}
	arch := node.ArchOf(*gpu)
	evidence, guide := gpuHealthDetail(gpu)
	item := dto.PhysicalGPU{
		Index: gpu.Index, UUID: gpu.UUID, Name: gpu.Name, Vendor: gpu.Vendor,
		ArchFamily: arch.Family, ArchID: arch.ID, ComputeCapability: gpu.ComputeCapability,
		MemoryCapacityKnown: gpuMemoryCapacityKnown(gpu.MemoryTotal),
		MemoryTotalBytes:    gpu.MemoryTotal, PowerLimitWatts: gpu.PowerLimitWatts, PCIeBDF: gpu.PCIeBDF,
		NUMANode: gpu.NUMANode, PartitionCapabilities: partitionReports.ForGPU(gpu),
		Ownership: gpu.Ownership, ForeignProcesses: gpu.ForeignUse(),
		SchedulingStopped: gpu.SchedulingStopped, SchedulingAllowed: gpu.SchedulingAllowedAt != nil,
		SchedulingActions: gpu.SchedulingActions(),
		Schedulable:       gpu.CanTakeWork(),
		HealthStatus:      gpu.HealthStatus, HealthCode: gpu.HealthCode, HealthMessage: gpu.HealthMessage,
		HealthEvidence: evidence, HealthGuide: guide,
		NodeUUID: node.UUID, NodeName: node.DisplayName(), NodeState: node.ConnectionState(), DriverVersion: node.GPUDriver,
		AllocatedBytes: allocation.AllocatedBytes, AvailableBytes: available, Occupants: []dto.GPUOccupant{},
	}
	switch {
	case node.State != store.NodeStateOnline:
		// Nothing is being asked of this card, and nothing can be: every
		// figure beside it is as the node's last report left it. Saying
		// "available" of a card nobody can reach is the stale reading
		// dressed up as a fact.
		item.AllocationState = "unknown"
	case !gpu.CanTakeWork():
		// Not only an unhealthy card: one lent to a virtual machine, or
		// held by a process outside the platform, takes no work either and
		// must not read as available.
		item.AllocationState = "unusable"
	case allocation.AllocatedBytes <= 0 && allocation.SleepingBytes <= 0:
		item.AllocationState = "available"
	case available > 0:
		item.AllocationState = "partial"
	default:
		item.AllocationState = "allocated"
	}
	for instanceIndex := range allocation.Instances {
		instance := &allocation.Instances[instanceIndex]
		occupant := dto.GPUOccupant{InstanceUUID: instance.UUID, DeploymentID: instance.DeploymentID, State: instance.State, SleepLevel: instance.SleepLevel}
		var spec proto.InstanceDesiredState
		if json.Unmarshal([]byte(instance.DesiredStateJSON), &spec) == nil {
			occupant.LimitBytes = spec.VRAMLimitBytes
			occupant.SMLimitPct = spec.SMLimitPct
		}
		if dep := deployment(instance.DeploymentID); dep != nil {
			occupant.DeploymentName = dep.Name
		}
		item.Occupants = append(item.Occupants, occupant)
	}
	applyPhysicalMetrics(&item, metrics, node.UUID, gpu.Index)
	return item
}

// accumulateGPUOverview counts one card into the totals. Every count is
// taken from the item the list shows, so a donut can never disagree with
// the rows beneath it: a node nobody can reach has every card at
// "unknown", and counting those as allocatable offered capacity the
// allocation_state filter returns nothing for.
func accumulateGPUOverview(overview *dto.GPUOverview, gpu *store.GPU, item *dto.PhysicalGPU) {
	overview.Total++
	switch accelerator.NormalizeHealthLevel(gpu.HealthStatus) {
	case accelerator.HealthWarning:
		overview.Health.Warning++
	case accelerator.HealthError:
		overview.Health.Error++
	default:
		overview.Health.OK++
	}
	switch item.AllocationState {
	case "unknown":
		overview.Allocation.Unknown++
	case "unusable":
		overview.Allocation.Unusable++
	case "available", "partial":
		overview.Allocation.Allocatable++
	case "allocated":
		overview.Allocation.Allocated++
	}
}

var gpuSnapshotMetrics = []string{
	"gpuworks_gpu_utilization", "gpuworks_gpu_memory_used_bytes", "gpuworks_gpu_temperature_celsius",
	"gpuworks_gpu_power_watts", "gpuworks_gpu_fan_speed_percent", "gpuworks_gpu_pcie_rx_bytes_per_sec",
	"gpuworks_gpu_pcie_tx_bytes_per_sec", "gpuworks_dgpu_memory_used_bytes", "gpuworks_dgpu_sm_util",
}

func (a *apiHandlers) loadLatestGPUMetrics() (map[string]map[string]MetricLatest, error) {
	out := map[string]map[string]MetricLatest{}
	if a.d.MetricsLatest == nil {
		return out, nil
	}
	since := time.Now().UTC().Add(-2 * time.Minute).UnixMilli()
	for _, metric := range gpuSnapshotMetrics {
		points, err := a.d.MetricsLatest(metric, nil, since)
		if err != nil {
			return nil, err
		}
		values := map[string]MetricLatest{}
		for _, point := range points {
			values[metricKey(point.Labels["node"], point.Labels["gpu"], point.Labels["instance"])] = point
		}
		out[metric] = values
	}
	return out, nil
}

func applyPhysicalMetrics(item *dto.PhysicalGPU, metrics map[string]map[string]MetricLatest, node string, gpu int) {
	key := metricKey(node, strconv.Itoa(gpu), "")
	assign := func(metric string, target **float64) {
		if point, ok := metrics[metric][key]; ok {
			value := point.Value
			*target = &value
			updated := time.UnixMilli(point.TimeMS).UTC()
			if item.Load.UpdatedAt == nil || updated.After(*item.Load.UpdatedAt) {
				item.Load.UpdatedAt = &updated
			}
		}
	}
	assign("gpuworks_gpu_utilization", &item.Load.UtilizationPct)
	assign("gpuworks_gpu_memory_used_bytes", &item.Load.MemoryUsed)
	assign("gpuworks_gpu_temperature_celsius", &item.Load.TemperatureC)
	assign("gpuworks_gpu_power_watts", &item.Load.PowerWatts)
	assign("gpuworks_gpu_fan_speed_percent", &item.Load.FanSpeedPct)
	assign("gpuworks_gpu_pcie_rx_bytes_per_sec", &item.Load.PCIeRXBPS)
	assign("gpuworks_gpu_pcie_tx_bytes_per_sec", &item.Load.PCIeTXBPS)
}

func applyDGPUMetrics(item *dto.DGPU, metrics map[string]map[string]MetricLatest, node string, gpu int, instance string) {
	key := metricKey(node, strconv.Itoa(gpu), instance)
	if point, ok := metrics["gpuworks_dgpu_memory_used_bytes"][key]; ok {
		value := point.Value
		item.MemoryUsed = &value
		updated := time.UnixMilli(point.TimeMS).UTC()
		item.UpdatedAt = &updated
	}
	if point, ok := metrics["gpuworks_dgpu_sm_util"][key]; ok {
		value := point.Value
		item.SMUtilPct = &value
		updated := time.UnixMilli(point.TimeMS).UTC()
		if item.UpdatedAt == nil || updated.After(*item.UpdatedAt) {
			item.UpdatedAt = &updated
		}
	}
}

func metricKey(node, gpu, instance string) string { return node + "\x00" + gpu + "\x00" + instance }
