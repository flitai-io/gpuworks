package app

import (
	"context"
	"encoding/json"
	"time"

	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/healthcheck"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/monitor"
	"gpuworks.ai/gpuworks/internal/server/monitorapi"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// monitorInventoryAdapter is the only place in the app layer that turns
// store rows into monitor's plain Inventory shape and instance_events rows
// into monitorapi's plain EventRef shape. The actual snapshot computation
// (internal/server/monitor) and wire-shaping (internal/server/monitorapi)
// both live in leaf packages that never import store; this adapter is what
// bridges them to the real database, exactly the same role
// internal/server/api/gpus.go's planning.GPUAllocations reader plays for
// /gpus - and deliberately reuses that same allocation ledger, so a
// deployment's VRAM claim agrees between the two endpoints.
type monitorInventoryAdapter struct {
	store       *store.Store
	preparation deployment.ModelPreparation
	// lists is the server's list snapshot; nil reads the rows per request.
	lists *deployment.Lists
}

// newMonitorAPIService builds the monitorapi.Service app.go wires into
// api.Deps.Monitor. now defaults to time.Now (nil is only ever passed by
// tests that want a fixed clock).
func newMonitorAPIService(st *store.Store, ts tsdb.Store, now func() time.Time, requests monitorapi.RequestSource, health func(uint) healthcheck.Health, preparation deployment.ModelPreparation) *monitorapi.Service {
	return newMonitorAPIServiceOn(nil, st, ts, now, requests, health, preparation)
}

// newMonitorAPIServiceOn is newMonitorAPIService reading its inventory from
// lists, the server's snapshot of every deployment and instance; nil reads
// them per request.
func newMonitorAPIServiceOn(lists *deployment.Lists, st *store.Store, ts tsdb.Store, now func() time.Time, requests monitorapi.RequestSource, health func(uint) healthcheck.Health, preparation deployment.ModelPreparation) *monitorapi.Service {
	a := &monitorInventoryAdapter{store: st, preparation: preparation, lists: lists}
	return monitorapi.New(monitorapi.Deps{
		TSDB:             ts,
		Inventory:        a.inventory,
		DeploymentEvents: a.deploymentEvents,
		Now:              now,
		// NotFound/SeriesTooLarge are api's own sentinel error values,
		// injected rather than referenced directly by monitorapi so that
		// leaf package never has to import internal/server/api (see
		// monitorapi.Deps' doc comment) - api.go's errors.Is checks still
		// work because both sides share these exact values.
		NotFound:       api.ErrMonitorDeploymentNotFound,
		SeriesTooLarge: api.ErrMonitorSeriesTooLarge,
		Requests:       requests,
		BadQuery:       api.ErrMonitorRequestsQueryInvalid,
		Health:         health,
	})
}

// inventory reads the current deployments/instances/nodes/GPUs from the
// store into monitor's read-only snapshot shape. GPU allocation uses the
// planning ledger - the same arithmetic as planning.GPUAllocations, which
// internal/server/api/gpus.go reads for /gpus, here charged for the whole
// fleet in one pass (planning.FleetAllocations) - so a deployment's VRAM
// claim agrees between the two endpoints and the number of reads does not
// grow with the node count.
func (a *monitorInventoryAdapter) inventory(ctx context.Context) (monitor.Inventory, error) {
	if err := ctx.Err(); err != nil {
		return monitor.Inventory{}, err
	}
	rows, err := a.rows()
	if err != nil {
		return monitor.Inventory{}, err
	}
	deployments, instances, states := rows.Deployments(), rows.AllInstances(), rows.States()
	nodes, err := a.store.FleetNodes()
	if err != nil {
		return monitor.Inventory{}, err
	}

	inv := monitor.Inventory{
		Deployments: make([]monitor.DeploymentRef, 0, len(deployments)),
		Instances:   make([]monitor.InstanceRef, 0, len(instances)),
		Nodes:       make([]monitor.NodeRef, 0, len(nodes)),
	}
	online := make(map[uint]bool, len(nodes))
	for _, node := range nodes {
		online[node.ID] = node.State == store.NodeStateOnline
	}
	instancesByDeployment := make(map[uint][]store.Instance)
	for _, inst := range planning.OnlineInstances(instances, func(id uint) bool { return online[id] }) {
		instancesByDeployment[inst.DeploymentID] = append(instancesByDeployment[inst.DeploymentID], inst)
	}
	for i := range deployments {
		ref := deploymentRef(&deployments[i], instancesByDeployment[deployments[i].ID])
		ref.State = states[ref.ID]
		inv.Deployments = append(inv.Deployments, ref)
	}

	nodeUUIDByID := make(map[uint]string, len(nodes))
	for i := range nodes {
		nodeUUIDByID[nodes[i].ID] = nodes[i].UUID
	}
	for i := range instances {
		inv.Instances = append(inv.Instances, instanceRef(&instances[i], nodeUUIDByID))
	}
	// One pass charges every node from the instances and deployments read
	// above, instead of one instance read per node (planning.GPUAllocations).
	allocations, err := planning.FleetAllocations(a.store, nodes, instances, deployments)
	if err != nil {
		return monitor.Inventory{}, err
	}
	for i := range nodes {
		n := &nodes[i]
		inv.Nodes = append(inv.Nodes, nodeRef(n))
		for _, gpu := range n.OwnedGPUs() {
			alloc := allocations[n.ID][gpu.Index]
			inv.GPUs = append(inv.GPUs, monitor.GPURef{
				NodeUUID: n.UUID, Index: gpu.Index, UUID: gpu.UUID,
				MemoryTotalBytes: gpu.MemoryTotal, AllocatedBytes: alloc.AllocatedBytes, Healthy: gpu.Healthy(),
			})
		}
	}
	return inv, nil
}

// rows are every deployment and instance with their states: the server's
// list snapshot, or read for this request without one.
func (a *monitorInventoryAdapter) rows() (*deployment.ListRows, error) {
	if a.lists != nil {
		return a.lists.Rows()
	}
	return deployment.ReadListRows(a.store, a.preparation)
}

// deploymentEvents adapts store.DeploymentTimeline's page (newest-first,
// folded, cursor-paginated by exclusive id) into monitorapi's plain EventRef
// shape, so monitorapi never has to import store for the concrete
// InstanceEvent type.
func (a *monitorInventoryAdapter) deploymentEvents(_ context.Context, deploymentID uint, before uint, limit int) ([]monitorapi.EventRef, error) {
	evs, err := a.store.DeploymentTimeline(deploymentID, before, limit)
	if err != nil {
		return nil, err
	}
	out := make([]monitorapi.EventRef, 0, len(evs))
	for _, e := range evs {
		out = append(out, monitorapi.EventRef{
			ID: e.ID, Kind: e.Kind, Data: e.Data, InstanceUUID: e.InstanceUUID, CreatedAtMS: e.CreatedAt.UTC().UnixMilli(),
		})
	}
	return out, nil
}

func deploymentRef(d *store.Deployment, instances []store.Instance) monitor.DeploymentRef {
	count := 0
	incomplete := planning.GroupsIncomplete(instances)
	for _, inst := range instances {
		headless := inst.Headless
		if spec, ok := desiredState(inst.DesiredStateJSON); ok {
			headless = headless || spec.Headless
		}
		if inst.State == proto.InstRunning && !planning.InstanceAsleep(&inst) && !headless && !incomplete[inst.UUID] {
			count++
		}
	}
	ready := &count
	desired, pd, transfer := d.Replicas, false, false
	if placement := pdPlacement(d.Placement); placement != nil {
		// A PD deployment's desired instances are its units, every role.
		desired, pd = placement.InstanceCount(), true
		count = 0
		for _, role := range planning.PDStatusOf(placement, instances).Roles {
			count += role.Ready
		}
		ready = &count
		mode, _ := pdmode.Lookup(placement.Mode)
		transfer = mode.TransferBytesMetric != ""
	}
	return monitor.DeploymentRef{
		ID: d.ID, Name: d.Name, Engine: d.Engine, ReplicasDesired: desired, ReplicasReady: ready,
		ServedName: d.ServedName, SleepDesired: d.DesiredSleep,
		PD: pd, PDTransferMetrics: transfer, HealthCheck: d.HealthCheck, HealthCheckThresholdSec: d.HealthCheckThresholdSec,
		// NodeNames is left empty: DeploymentRef.NodeNames is only a
		// hint override, and monitor.resolveNodeNames already falls back
		// to the deployment's actual instances' node UUIDs (resolved via
		// nodeRef below) when it is empty - which is the placement that
		// actually matters once the deployment has replicas.
	}
}

// instanceRef parses the JSON-encoded columns store.Instance keeps
// (GPUIndexes); Headless comes from the instance's desired-state snapshot
// (proto.InstanceDesiredState.Headless).
func instanceRef(i *store.Instance, nodeUUIDByID map[uint]string) monitor.InstanceRef {
	headless := false
	if spec, ok := desiredState(i.DesiredStateJSON); ok {
		headless = spec.Headless
	}
	return monitor.InstanceRef{
		UUID: i.UUID, DeploymentID: i.DeploymentID, NodeUUID: nodeUUIDByID[i.NodeID],
		State: i.State, GPUIndexes: parseIntSliceJSON(i.GPUIndexes), SleepLevel: i.SleepLevel,
		// Asleep is the gateway's judgement, not the reported level: an
		// instance under a sleep order it has not carried out is refused too.
		Asleep:   planning.InstanceAsleep(i),
		Headless: headless,
		Role:     i.Role,
	}
}

// desiredState parses an Instance.DesiredStateJSON column. ok is false for
// an empty or unparseable value, in which case callers derive nothing from
// it (Headless stays false) rather than guessing.
func desiredState(raw string) (spec proto.InstanceDesiredState, ok bool) {
	if raw == "" {
		return proto.InstanceDesiredState{}, false
	}
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return proto.InstanceDesiredState{}, false
	}
	return spec, true
}

func nodeRef(n *store.Node) monitor.NodeRef {
	// The connection as a client is shown it, as everywhere a reader sees
	// a node's state; placement reads the stored column instead.
	return monitor.NodeRef{UUID: n.UUID, Name: nodeDisplayName(n), State: n.ConnectionState()}
}

// nodeDisplayName is store.Node.DisplayName, so a node reads the same
// everywhere in the product.
func nodeDisplayName(n *store.Node) string { return n.DisplayName() }

func parseIntSliceJSON(s string) []int {
	if s == "" {
		return nil
	}
	var v []int
	_ = json.Unmarshal([]byte(s), &v)
	return v
}

// pdPlacement is a stored placement's PD section; nil for any other, and
// for one that cannot be read (the controller reports that).
func pdPlacement(raw string) *planning.PDPlacement {
	pd, _ := planning.PDOf(raw)
	return pd
}

// monitorService is monitorapi.Service as the API takes it: its view as of a
// picked range's end is an api.MonitorService too.
type monitorService struct{ *monitorapi.Service }

func (m monitorService) At(end time.Time) api.MonitorService {
	return monitorService{m.Service.At(end)}
}
