package proxy

import (
	"encoding/json"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type routeSnapshot struct {
	builtAt  time.Time
	byName   map[string][]*routeDeployment
	byID     map[uint]*routeDeployment
	nodeUUID map[uint]string
	// serving maps node UUID to its serving forwarder; absent nodes are only
	// reachable through the agent tunnel.
	serving map[string]network.ServingEndpoint
	// running holds the instances the snapshot saw running: a change to one
	// of them may take a candidate out of routing, so it rebuilds at once
	// (urgentOn).
	running map[string]bool
}

type routeDeployment struct {
	dep        *store.Deployment
	id         uint
	servedName string
	nnodes     int
	policy     string
	candidates []candidate
	nodes      map[uint]string
	// pd is set on a PD deployment: candidates then holds its decode
	// instances and pd its prefill ones.
	pd *pdRoute
	// reading is how the deployment's replies are read (ADR 0042).
	reading replyReading
	// injectUsage: the engine is asked for usage on streamed requests.
	injectUsage bool
}

type routeTable struct {
	store    *store.Store
	networks Networks
	hub      Tunneler
	log      *slog.Logger
	current  atomic.Pointer[routeSnapshot]
	dirty    chan struct{}
	// urgent is dirty for a change that may alter which instances route
	// (urgentOn): it rebuilds without waiting out routeEventRebuildGap.
	urgent           chan struct{}
	lastRebuild      atomic.Int64
	onRebuild        func(map[string]bool)
	onInstanceChange func(string)
	callbackMu       sync.RWMutex
	mu               sync.Mutex
	stop             chan struct{}
	done             chan struct{}
	running          bool
}

func newRouteTable(st *store.Store, networks Networks, hub Tunneler, log *slog.Logger) *routeTable {
	r := &routeTable{store: st, networks: networks, hub: hub, log: log, dirty: make(chan struct{}, 1), urgent: make(chan struct{}, 1)}
	if log == nil {
		r.log = slog.Default()
	}
	if st == nil {
		r.current.Store(&routeSnapshot{builtAt: time.Now(), byName: map[string][]*routeDeployment{}, nodeUUID: map[uint]string{}})
		return r
	}
	if snap, err := r.build(); err == nil {
		r.current.Store(snap)
	} else {
		r.current.Store(&routeSnapshot{builtAt: time.Now(), byName: map[string][]*routeDeployment{}, nodeUUID: map[uint]string{}})
		r.log.Warn("build proxy route table", "err", err)
	}
	r.start()
	return r
}

func (r *routeTable) start() {
	if r == nil || r.store == nil || r.store.Bus == nil {
		return
	}
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	r.stop, r.done, r.running = stop, done, true
	r.mu.Unlock()
	// A template change may change what its deployments' replies are read
	// for (api.request_stats).
	sub := r.store.Bus.Subscribe(64, "deployment", "instance", "node", "network", store.InferenceTemplateTopic)
	go r.run(sub, stop, done)
}

// close stops the table's event subscription and refresh ticker. A route table
// can be started again when an App test harness restarts the same server.
func (r *routeTable) close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return
	}
	stop, done := r.stop, r.done
	r.running = false
	close(stop)
	r.mu.Unlock()
	<-done
}

func (r *routeTable) run(sub *bus.Subscription, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(routeRefreshInterval)
	defer t.Stop()
	// deferred fires the rebuild a dirty signal inside routeEventRebuildGap
	// postponed; signals meanwhile fold into it.
	var deferred <-chan time.Time
	for {
		select {
		case e := <-sub.C():
			if e.Topic == "instance" && e.Reason != bus.ReasonProgress {
				r.callbackMu.RLock()
				callback := r.onInstanceChange
				r.callbackMu.RUnlock()
				if callback != nil {
					callback(e.Key)
				}
			}
			switch {
			case !rebuildsOn(e):
			case r.urgentOn(e):
				select {
				case r.urgent <- struct{}{}:
				default:
				}
			default:
				r.requestEventRebuild()
			}
		case <-r.urgent:
			r.rebuild()
		case <-r.dirty:
			if wait := r.eventRebuildWait(); wait > 0 {
				if deferred == nil {
					deferred = time.After(wait)
				}
				continue
			}
			r.rebuild()
		case <-deferred:
			deferred = nil
			r.rebuild()
		case <-t.C:
			r.rebuild()
		case <-stop:
			sub.Close()
			return
		}
	}
}

// rebuildsOn reports whether an event rebuilds the table at once. Instance
// events cover readiness/sleep transitions; deployment and node events cover
// policy, rollout, and tunnel-availability changes. Progress events only
// update download counters and do not affect route eligibility. Nor does a
// node's routine status report (every agent sends one a minute, a fleet
// hundreds a second): what it can change, the node's serving address, the
// periodic refresh picks up, while rebuilding on each kept a 1000-node
// fleet's table rebuilding nonstop.
func rebuildsOn(e bus.Event) bool {
	if e.Reason == bus.ReasonProgress {
		return false
	}
	return e.Topic != "node" || e.Reason != store.NodeChangeStatus
}

// urgentOn reports whether an event may change which instances route, and so
// rebuilds without waiting out routeEventRebuildGap: any deployment, node or
// network change; a change to an instance the table saw running (draining
// one must stop its traffic at once); and an instance becoming running or
// changing its sleep. What is left - instances being created, pulled or
// started, which no request can reach yet - is the bulk of a fleet
// redeploying, and waits.
func (r *routeTable) urgentOn(e bus.Event) bool {
	if e.Topic == store.InferenceTemplateTopic {
		return false
	}
	if e.Topic != "instance" {
		return true
	}
	switch e.Reason {
	case proto.InstRunning, "sleep", "desired-state":
		return true
	}
	snap := r.current.Load()
	return snap == nil || snap.running[e.Key]
}

// eventRebuildWait is how long an event-started rebuild must still wait to
// keep routeEventRebuildGap after the last rebuild.
func (r *routeTable) eventRebuildWait() time.Duration {
	last := r.lastRebuild.Load()
	if last == 0 {
		return 0
	}
	return routeEventRebuildGap - time.Since(time.Unix(0, last))
}

func (r *routeTable) requestRebuild() {
	now := time.Now()
	last := r.lastRebuild.Load()
	if last != 0 && now.Sub(time.Unix(0, last)) < time.Second {
		return
	}
	r.enqueueRebuild()
}

// requestEventRebuild is used for bus notifications. Unlike the request-path
// fallback, an event is never suppressed by the rebuild interval: the buffered
// signal is the pending work that must be observed by the refresh loop.
func (r *routeTable) requestEventRebuild() {
	r.enqueueRebuild()
}

func (r *routeTable) enqueueRebuild() {
	select {
	case r.dirty <- struct{}{}:
	default:
	}
}
func (r *routeTable) rebuild() {
	snap, err := r.build()
	if err != nil {
		r.log.Warn("rebuild proxy route table", "err", err)
		return
	}
	r.current.Store(snap)
	r.lastRebuild.Store(time.Now().UnixNano())
	r.callbackMu.RLock()
	onRebuild := r.onRebuild
	r.callbackMu.RUnlock()
	if onRebuild != nil {
		active := make(map[string]bool)
		for _, deployments := range snap.byName {
			for _, deployment := range deployments {
				for _, candidate := range deployment.candidates {
					active[candidate.instanceUUID] = true
				}
			}
		}
		onRebuild(active)
	}
}

func (r *routeTable) setOnRebuild(fn func(map[string]bool)) {
	r.callbackMu.Lock()
	r.onRebuild = fn
	r.callbackMu.Unlock()
}

func (r *routeTable) build() (*routeSnapshot, error) {
	deps, err := r.store.RoutingDeployments()
	if err != nil {
		return nil, err
	}
	insts, err := r.store.RoutingInstances()
	if err != nil {
		return nil, err
	}
	nodes, err := r.store.NodeUUIDsByID()
	if err != nil {
		return nil, err
	}
	byDep := make(map[uint]*routeDeployment, len(deps))
	byName := make(map[string][]*routeDeployment)
	for i := range deps {
		d := &deps[i]
		policy := deploy.NormalizeRoutingPolicy(d.RoutingPolicy)
		pd, err := pdRouteFor(d)
		if err != nil {
			// Not routed at all: its prefill instances cannot answer requests.
			r.log.Warn("PD deployment not routed: placement cannot be read", "dep", d.ID, "err", err)
			continue
		}
		rd := &routeDeployment{dep: d, id: d.ID, servedName: d.ServedName, nnodes: d.NNodes, policy: policy, nodes: nodes, pd: pd}
		byDep[d.ID] = rd
		byName[d.ServedName] = append(byName[d.ServedName], rd)
	}
	pdRunning := map[uint][]store.Instance{}
	running := map[string]bool{}
	for i := range insts {
		in := &insts[i]
		rd := byDep[in.DeploymentID]
		if rd == nil {
			continue
		}
		if in.State == proto.InstRunning {
			running[in.UUID] = true
		}
		// A multi-node replica sleeps with its leader's engine: only the
		// leader's intent and level say whether the replica is asleep, and a
		// follower is never skipped for its own.
		if rd.nnodes > 1 {
			continue
		}
		if !in.Headless && planning.InstanceAsleep(in) {
			// The router decides whether the deployment is asleep from the
			// instances it reads per request (selectDeploymentInstances), so the
			// snapshot only records a PD deployment's asleep prefill counts,
			// which wantsPrefill reads.
			if rd.pd != nil {
				rd.pd.noteAsleep(in)
			}
			continue
		}
		if rd.pd != nil && in.State == proto.InstRunning {
			// Followers too: a multi-node leader is routable only with them.
			pdRunning[rd.id] = append(pdRunning[rd.id], *in)
		}
		if in.State != proto.InstRunning || in.Headless || in.APIPort <= 0 {
			continue
		}
		nodeUUID := nodes[in.NodeID]
		if nodeUUID == "" {
			continue
		}
		c := candidate{instanceID: in.ID, instanceUUID: in.UUID, nodeUUID: nodeUUID, port: in.APIPort, modelName: instanceModelName(rd.dep, in), uuidHash: fnv64([]byte(in.UUID))}
		if rd.pd != nil {
			var ok bool
			if c, ok = rd.pd.admit(in, c); !ok {
				continue
			}
		}
		rd.candidates = append(rd.candidates, c)
	}
	for _, rd := range byDep {
		if rd.pd != nil {
			rd.candidates = rd.pd.withoutUnroutable(rd.candidates, pdRunning[rd.id], nodes)
		}
	}
	serving, err := r.networks.ServingEndpoints()
	if err != nil {
		// Routing must not depend on the optional direct path.
		r.log.Warn("resolve serving networks for the gateway", "err", err)
		serving = nil
	}
	r.resolveRequestStats(byDep)
	return &routeSnapshot{builtAt: time.Now(), byName: byName, byID: byDep, nodeUUID: nodes, serving: serving, running: running}, nil
}

// resolveRequestStats gives each routed deployment its request-stat rules,
// reading every template spec they use in one query. A spec that cannot be
// read falls back to the deployment's own snapshot of it.
func (r *routeTable) resolveRequestStats(byDep map[uint]*routeDeployment) {
	keys := make([]store.TemplateSpecKey, 0, len(byDep))
	for _, rd := range byDep {
		if rd.dep.TemplateID != 0 {
			keys = append(keys, store.TemplateSpecKey{TemplateID: rd.dep.TemplateID, Source: rd.dep.TemplateSpecSource, Name: rd.dep.TemplateSpecName})
		}
	}
	contracts, err := r.store.TemplateSpecAPIContracts(keys)
	if err != nil {
		r.log.Warn("read template api contracts for the gateway; using deployment snapshots", "err", err)
		contracts = nil
	}
	for _, rd := range byDep {
		raw, ok := contracts[store.TemplateSpecKey{TemplateID: rd.dep.TemplateID, Source: rd.dep.TemplateSpecSource, Name: rd.dep.TemplateSpecName}]
		if !ok && rd.dep.CatalogSelectionSnapshot != "" {
			if selection, err := deploy.DecodeCatalogSelection([]byte(rd.dep.CatalogSelectionSnapshot)); err == nil {
				raw, ok = string(selection.APIContract), true
			}
		}
		var declared inferenceapi.MetricMap
		if ok && raw != "" {
			var contract inferenceapi.Contract
			if json.Unmarshal([]byte(raw), &contract) == nil {
				declared = contract.RequestStats
			}
		}
		rd.reading = replyReading{
			rules:          inferenceapi.ResolveRequestStats(rd.dep.Engine, declared),
			cacheOmitsZero: cacheReportOmitsZero(rd.dep),
			fallbacks:      inferencemetrics.GatewayFallbacksFor(rd.dep.Engine),
		}
		rd.injectUsage = injectsUsage(rd.dep.Engine)
	}
}

// observationFor is how the gateway observes a deployment's requests in the
// current snapshot: how to read its replies and whether to ask for usage.
func (r *routeTable) observationFor(deploymentID uint) (replyReading, bool) {
	s := r.current.Load()
	if s == nil {
		return replyReading{}, false
	}
	if rd := s.byID[deploymentID]; rd != nil {
		return rd.reading, rd.injectUsage
	}
	return replyReading{}, false
}

// servingEndpoint is the node's serving forwarder in the current snapshot.
func (r *routeTable) servingEndpoint(nodeUUID string) (network.ServingEndpoint, bool) {
	s := r.current.Load()
	if s == nil {
		return network.ServingEndpoint{}, false
	}
	ep, ok := s.serving[nodeUUID]
	return ep, ok
}

// deployment is a deployment in the current snapshot, nil when it is not
// routed (stopped, or a snapshot not built yet).
func (r *routeTable) deployment(id uint) *routeDeployment {
	s := r.current.Load()
	if s == nil {
		return nil
	}
	return s.byID[id]
}

// servedNames are the served names of the current snapshot that some
// deployment allow accepts routes under, each once, in name order. A
// deployment the snapshot does not route (stopped, unreadable PD placement)
// has none.
func (r *routeTable) servedNames(allow func(deploymentID uint) bool) []string {
	if r == nil {
		return nil
	}
	s := r.current.Load()
	if s == nil {
		return nil
	}
	names := make([]string, 0, len(s.byName))
	for name, deps := range s.byName {
		if name == "" {
			continue
		}
		for _, d := range deps {
			if allow(d.id) {
				names = append(names, name)
				break
			}
		}
	}
	slices.Sort(names)
	return names
}

func (r *routeTable) snapshot(name string) ([]*routeDeployment, bool) {
	s := r.current.Load()
	if s == nil {
		return nil, false
	}
	deps, ok := s.byName[name]
	return deps, ok
}

func (r *routeTable) needsAffinity(name string) bool {
	s := r.current.Load()
	if s == nil {
		return false
	}
	for _, deployment := range s.byName[name] {
		// A PD deployment always picks its prefill instance by prefix.
		if deployment.policy == deploy.PrefixAffinity || deployment.pd != nil {
			return true
		}
	}
	return false
}

func (r *routeTable) setOnInstanceChange(fn func(string)) {
	r.callbackMu.Lock()
	r.onInstanceChange = fn
	r.callbackMu.Unlock()
}
