package controller

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// routeFleet is a cluster where every deployment is settled except the ones
// a test makes otherwise: one running replica each, spread over the nodes.
type routeFleet struct {
	st    *store.Store
	c     *Controller
	nodes []*store.Node
	deps  []uint // deps[i] runs on nodes[i%len(nodes)] and uses model i+1
}

func newRouteFleet(t *testing.T, name string, nodes, deps int) *routeFleet {
	t.Helper()
	st, err := store.Open("file:controller_routes_"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &routeFleet{st: st}
	for i := 0; i < nodes; i++ {
		uuid := fmt.Sprintf("%s-node-%d", name, i)
		n, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		f.nodes = append(f.nodes, n)
	}
	rows := make([]store.Deployment, deps)
	for i := range rows {
		rows[i] = store.Deployment{Name: fmt.Sprintf("%s-%d", name, i), ModelID: uint(i + 1), NodeID: f.nodes[i%nodes].ID, State: store.DeployActive, Replicas: 1}
	}
	if err := st.DB().CreateInBatches(&rows, 500).Error; err != nil {
		t.Fatal(err)
	}
	insts := make([]store.Instance, len(rows))
	for i, d := range rows {
		f.deps = append(f.deps, d.ID)
		insts[i] = store.Instance{UUID: fmt.Sprintf("%s-inst-%d", name, i), DeploymentID: d.ID, NodeID: d.NodeID, State: proto.InstRunning}
	}
	if err := st.DB().CreateInBatches(&insts, 500).Error; err != nil {
		t.Fatal(err)
	}
	f.c = New(st, &distributedTestHub{online: map[string]bool{}, updates: map[string][]proto.StateUpdate{}}, nil)
	return f
}

func sortedPending(c *Controller) []string {
	keys := pendingKeys(c)
	sort.Strings(keys)
	return keys
}

func depKeys(ids ...uint) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, depKey(id))
	}
	sort.Strings(out)
	return out
}

// countQueries counts the statements run against the database from now on.
func countQueries(t *testing.T, st *store.Store) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	name := "count-route-queries-" + strings.ReplaceAll(t.Name(), "/", "-")
	if err := st.DB().Callback().Query().Before("gorm:query").Register(name, func(*gorm.DB) { n.Add(1) }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DB().Callback().Query().Remove(name) })
	return &n
}

// A node reconnecting reconciles the deployments with a replica on it, not
// the whole fleet; a settled deployment elsewhere is left alone.
func TestNodeEventReconcilesOnlyItsReplicasAndUnsettled(t *testing.T) {
	f := newRouteFleet(t, "node_scope", 3, 6)
	// deps[5] waits for a second replica: unsettled, so any node may unblock it.
	if err := f.st.DB().Model(&store.Deployment{}).Where("id = ?", f.deps[5]).Update("replicas", 2).Error; err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{"state:online", "registered", "agent_info", "labels", "name", store.NodeChangeScheduling} {
		drainQueue(f.c)
		f.c.onEvent(bus.Event{Topic: "node", Key: f.nodes[0].UUID, Reason: reason})
		if keys := pendingKeys(f.c); len(keys) != 0 {
			t.Fatalf("%s queued %v before the routing tick", reason, keys)
		}
		f.c.flushEventRoutes()
		// nodes[0] runs deps[0] and deps[3].
		if got, want := sortedPending(f.c), depKeys(f.deps[0], f.deps[3], f.deps[5]); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s queued %v, want %v", reason, got, want)
		}
	}
}

// A deleted node's instances can no longer be found through its UUID, so the
// event widens to every deployment rather than risk missing one.
func TestNodeEventForUnknownNodeReconcilesEverything(t *testing.T) {
	f := newRouteFleet(t, "node_unknown", 2, 4)
	f.c.onEvent(bus.Event{Topic: "node", Key: "no-such-node", Reason: "deleted"})
	f.c.flushEventRoutes()
	if got, want := sortedPending(f.c), depKeys(f.deps...); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("queued %v, want every deployment %v", got, want)
	}
}

// A model file changing reconciles the active deployments that use the model
// and the unsettled ones, never a settled deployment of another model.
func TestModelFileEventReconcilesItsDependents(t *testing.T) {
	f := newRouteFleet(t, "model_scope", 2, 4)
	mf, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: 2, NodeID: f.nodes[1].ID, State: proto.MFPresent})
	if err != nil {
		t.Fatal(err)
	}
	drainQueue(f.c)
	f.c.onEvent(bus.Event{Topic: "modelfile", Key: store.ModelFileKey(mf.ID), Reason: proto.MFPresent})
	f.c.flushEventRoutes()
	if got, want := sortedPending(f.c), depKeys(f.deps[1]); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("queued %v, want model 2's deployment %v", got, want)
	}
	// A key that resolves to no row (its file was deleted) no longer says
	// whose model it was.
	drainQueue(f.c)
	f.c.onEvent(bus.Event{Topic: "modelfile", Key: store.ModelFileKey(mf.ID + 1000), Reason: "deleted"})
	f.c.flushEventRoutes()
	if got := pendingKeys(f.c); len(got) != len(f.deps) {
		t.Fatalf("unresolvable model file queued %v, want all %d", got, len(f.deps))
	}
}

// Operator edits that change what every deployment may do reach all of them;
// a node's storage turning ready reaches only those still waiting.
func TestStorageAndPullSettingsEventsRouting(t *testing.T) {
	f := newRouteFleet(t, "storage_scope", 2, 4)
	if err := f.st.DB().Model(&store.Deployment{}).Where("id = ?", f.deps[2]).Update("replicas", 3).Error; err != nil {
		t.Fatal(err)
	}
	for _, ev := range []bus.Event{
		{Topic: store.PullSettingsTopic, Key: "global", Reason: "updated"},
		{Topic: "storage", Key: "1", Reason: "updated"},
		{Topic: "storage", Key: "1", Reason: "node_overrides"},
	} {
		drainQueue(f.c)
		f.c.onEvent(ev)
		f.c.flushEventRoutes()
		if got := pendingKeys(f.c); len(got) != len(f.deps) {
			t.Fatalf("%s/%s queued %v, want all %d", ev.Topic, ev.Reason, got, len(f.deps))
		}
	}
	drainQueue(f.c)
	f.c.onEvent(bus.Event{Topic: "storage", Key: "1", Reason: "node_state"})
	f.c.flushEventRoutes()
	if got, want := sortedPending(f.c), depKeys(f.deps[2]); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("storage node_state queued %v, want the waiting %v", got, want)
	}
}

// A reconnect storm - a thousand agents each registering, coming online,
// reporting agent info, storage and model files - costs a bounded number of
// reads per tick, not one deployment scan per event, and queues only what the
// events touch.
func TestReconnectStormCostsReadsPerTickNotPerEvent(t *testing.T) {
	f := newRouteFleet(t, "storm", 50, 1000)
	waiting := f.deps[7]
	if err := f.st.DB().Model(&store.Deployment{}).Where("id = ?", waiting).Update("replicas", 2).Error; err != nil {
		t.Fatal(err)
	}
	// Only nodes[0] and nodes[1] actually reconnect; the rest of the storm is
	// storage reports, which concern only unsettled deployments.
	queries := countQueries(t, f.st)
	events := 0
	for i := 0; i < 1000; i++ {
		node := f.nodes[i%2]
		for _, reason := range []string{"registered", "state:online", "agent_info"} {
			f.c.onEvent(bus.Event{Topic: "node", Key: node.UUID, Reason: reason})
			events++
		}
		f.c.onEvent(bus.Event{Topic: "storage", Key: "1", Reason: "node_state"})
		events++
	}
	if q := queries.Load(); q != 0 {
		t.Fatalf("%d events ran %d queries before the tick", events, q)
	}
	tick := time.Now()
	f.c.flushEventRoutes()
	t.Logf("one tick over %d events took %v", events, time.Since(tick))
	if q := queries.Load(); q > 6 {
		t.Fatalf("one tick over %d events ran %d queries", events, q)
	}
	// nodes[0] and nodes[1] each run 20 of the 1000 deployments; waiting runs
	// on nodes[7].
	if got := len(pendingKeys(f.c)); got != 41 {
		t.Fatalf("queued %d deployments, want the 40 on the reconnected nodes and the waiting one", got)
	}
}

// An instance event reconciles its deployment and retries only the cache
// operations on its node, with one narrow read for the instance.
func TestInstanceEventReadsTheInstanceOnce(t *testing.T) {
	f := newRouteFleet(t, "instance_once", 2, 2)
	uuid := "instance_once-inst-1"
	reads := 0
	if err := f.st.DB().Callback().Query().Before("gorm:query").Register("count-instance-reads", func(db *gorm.DB) {
		if db.Statement.Table == "instances" {
			reads++
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.st.DB().Callback().Query().Remove("count-instance-reads") })
	f.c.onEvent(bus.Event{Topic: "instance", Key: uuid, Reason: proto.InstStopped})
	if reads != 1 {
		t.Fatalf("an instance event read the instance %d times", reads)
	}
	if got, want := sortedPending(f.c), depKeys(f.deps[1]); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("queued %v, want %v", got, want)
	}
	f.c.routes.mu.Lock()
	_, noted := f.c.routes.readers[f.nodes[1].ID]
	f.c.routes.mu.Unlock()
	if !noted {
		t.Fatal("the instance's node was not noted for its cache readers")
	}
}

// A pass that cannot read the deployments is kept for the next tick instead
// of being dropped.
func TestFailedRouteReadIsRetriedNextTick(t *testing.T) {
	f := newRouteFleet(t, "route_retry", 1, 3)
	var fail atomic.Bool
	if err := f.st.DB().Callback().Query().Before("gorm:query").Register("fail-deployment-reads", func(db *gorm.DB) {
		if fail.Load() && db.Statement.Table == "deployments" {
			_ = db.AddError(errors.New("injected read failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.st.DB().Callback().Query().Remove("fail-deployment-reads") })
	fail.Store(true)
	f.c.onEvent(bus.Event{Topic: store.PullSettingsTopic, Key: "global", Reason: "updated"})
	f.c.flushEventRoutes()
	if got := pendingKeys(f.c); len(got) != 0 {
		t.Fatalf("a failed read queued %v", got)
	}
	fail.Store(false)
	f.c.flushEventRoutes()
	if got, want := sortedPending(f.c), depKeys(f.deps...); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("retry queued %v, want %v", got, want)
	}
	// Nothing was left behind once the retry succeeded.
	drainQueue(f.c)
	f.c.flushEventRoutes()
	if got := pendingKeys(f.c); len(got) != 0 {
		t.Fatalf("an empty tick queued %v", got)
	}
}

// A model nobody runs reconciles only the unsettled; the model file's storage
// cache is still refreshed, once however many events named it.
func TestModelFileEventWithoutDependentsRefreshesItsCacheOnce(t *testing.T) {
	f := newRouteFleet(t, "model_none", 2, 3)
	mf, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: 99, NodeID: f.nodes[0].ID, State: proto.MFPresent})
	if err != nil {
		t.Fatal(err)
	}
	drainQueue(f.c)
	reads := 0
	if err := f.st.DB().Callback().Query().Before("gorm:query").Register("count-model-file-reads", func(db *gorm.DB) {
		if db.Statement.Table == "model_files" {
			reads++
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.st.DB().Callback().Query().Remove("count-model-file-reads") })
	for i := 0; i < 50; i++ {
		f.c.onEvent(bus.Event{Topic: "modelfile", Key: store.ModelFileKey(mf.ID), Reason: proto.MFPresent})
	}
	if reads != 0 {
		t.Fatalf("50 events read the model file %d times before the tick", reads)
	}
	f.c.flushEventRoutes()
	if reads != 1 {
		t.Fatalf("the tick read the model file %d times, want once", reads)
	}
	if got := pendingKeys(f.c); len(got) != 0 {
		t.Fatalf("a model no deployment uses queued %v", got)
	}
	want := "refresh/" + cacheKey(mf.StorageID, f.nodes[0].ID)
	if keys := queuedCacheKeys(f.c); len(keys) != 1 || !keys[want] {
		t.Fatalf("cache refresh queued %v, want %s", keys, want)
	}
}

// The model index follows deployment writes: a deployment created after it
// was built is found at the next tick.
func TestModelFileRoutingSeesDeploymentsCreatedSince(t *testing.T) {
	f := newRouteFleet(t, "model_fresh", 1, 2)
	mf, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: 77, NodeID: f.nodes[0].ID, State: proto.MFPresent})
	if err != nil {
		t.Fatal(err)
	}
	f.c.onEvent(bus.Event{Topic: "modelfile", Key: store.ModelFileKey(mf.ID), Reason: proto.MFPresent})
	f.c.flushEventRoutes() // builds the index without model 77's user
	drainQueue(f.c)
	dep, err := f.st.CreateDeployment(&store.Deployment{Name: "model_fresh-late", ModelID: 77, NodeID: f.nodes[0].ID, State: store.DeployActive, Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.CreateInstance(&store.Instance{UUID: "model_fresh-late-0", DeploymentID: dep.ID, NodeID: f.nodes[0].ID, State: proto.InstRunning}); err != nil {
		t.Fatal(err)
	}
	f.c.onEvent(bus.Event{Topic: "modelfile", Key: store.ModelFileKey(mf.ID), Reason: proto.MFPresent})
	f.c.flushEventRoutes()
	if got, want := sortedPending(f.c), depKeys(dep.ID); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("queued %v, want the new user %v", got, want)
	}
}

// A deployment whose launch document cannot be decoded is reconciled with
// every model event instead of making each of them reconcile the fleet.
func TestModelFileRoutingKeepsWorkingPastAnUndecodableDeployment(t *testing.T) {
	f := newRouteFleet(t, "model_broken", 1, 4)
	if err := f.st.DB().Model(&store.Deployment{}).Where("id = ?", f.deps[3]).Update("template_snapshot", "{not json").Error; err != nil {
		t.Fatal(err)
	}
	mf, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: 1, NodeID: f.nodes[0].ID, State: proto.MFPresent})
	if err != nil {
		t.Fatal(err)
	}
	drainQueue(f.c)
	f.c.onEvent(bus.Event{Topic: "modelfile", Key: store.ModelFileKey(mf.ID), Reason: proto.MFPresent})
	f.c.flushEventRoutes()
	if got, want := sortedPending(f.c), depKeys(f.deps[0], f.deps[3]); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("queued %v, want model 1's user and the undecodable one %v", got, want)
	}
	// Handing a download to a successor still refuses to guess.
	if _, err := f.c.downloadDependents([]uint{1}); err == nil {
		t.Fatal("downloadDependents ignored an undecodable deployment")
	}
}
