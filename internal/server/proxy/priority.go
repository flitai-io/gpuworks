package proxy

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/priority"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// Request priority (docs/explanation/request-priority.md): the gateway
// decides each request's class from its key's binding and the class the
// client named, and writes the engine's "priority" field for a deployment
// that runs priority scheduling. A client's own field and vLLM's header
// never reach an engine (priority_strip.go).
const (
	// priorityClassHeader names the class a client asks for; the answer
	// carries the class the request ran at under the same name.
	priorityClassHeader = "X-GPUWorks-Priority-Class"
	// priorityClassMaxAge bounds how stale the class snapshot may be when
	// another server changed the classes; this server's own changes reload
	// it at once (store.PriorityClassGeneration).
	priorityClassMaxAge = 5 * time.Second
)

// priorityClasses is the gateway's snapshot of the classes. A request reads
// it without a lock or the database; the request that finds it stale
// reloads it while the others keep using the old one.
type priorityClasses struct {
	store  *store.Store
	now    func() time.Time
	snap   atomic.Pointer[prioritySnapshot]
	reload sync.Mutex
}

type prioritySnapshot struct {
	table priority.Table
	gen   uint64
	at    time.Time
}

func (c *priorityClasses) clock() time.Time {
	if c.now == nil {
		return time.Now()
	}
	return c.now()
}

func (c *priorityClasses) fresh(s *prioritySnapshot, gen uint64) bool {
	return s != nil && s.gen == gen && c.clock().Sub(s.at) < priorityClassMaxAge
}

func (c *priorityClasses) get() priority.Table {
	gen := c.store.PriorityClassGeneration()
	cur := c.snap.Load()
	if c.fresh(cur, gen) {
		return cur.table
	}
	if cur != nil && !c.reload.TryLock() {
		// Another request is reloading; this one uses the last snapshot.
		return cur.table
	}
	if cur == nil {
		c.reload.Lock()
	}
	defer c.reload.Unlock()
	if latest := c.snap.Load(); c.fresh(latest, gen) {
		return latest.table
	}
	table, err := c.store.PriorityClassTable()
	if err != nil {
		if latest := c.snap.Load(); latest != nil {
			// Keep serving the last good snapshot; a later request retries.
			return latest.table
		}
		// No snapshot yet: run at the neutral default for a while rather
		// than have every request queue on the lock to retry the database.
		empty := priority.NewTable(nil, 0)
		c.snap.Store(&prioritySnapshot{table: empty, gen: gen, at: c.clock()})
		return empty
	}
	c.snap.Store(&prioritySnapshot{table: table, gen: gen, at: c.clock()})
	return table
}

type priorityBindingKey struct{}

// withPriorityBinding carries the request's key binding from authentication
// to dispatch.
func withPriorityBinding(ctx context.Context, binding priority.Binding) context.Context {
	return context.WithValue(ctx, priorityBindingKey{}, binding)
}

// requestPriority decides the request's class: batch requests run at the
// class of their batch (the batch class by default), playground requests at
// the class the signed-in user names, other requests without a key (open
// access, probes) at the default, a key's requests by its binding and the
// class asked for.
// ok is false when the client named a class that does not exist. A request
// the gateway already decided (decidePriority) keeps that decision, so a
// replay after preemption runs at the class the first attempt did.
func (p *Proxy) requestPriority(r *http.Request) (priority.Decision, bool) {
	if decided, ok := r.Context().Value(priorityDecisionKey{}).(priority.Decision); ok {
		return decided, true
	}
	table := p.priority.get()
	asked := r.Header.Get(priorityClassHeader)
	r.Header.Del(priorityClassHeader)
	source := priority.SourceAnonymous
	caller, _ := uxrecord.CallerFrom(r.Context())
	binding, keyed := r.Context().Value(priorityBindingKey{}).(priority.Binding)
	switch {
	case caller.Source == uxrecord.SourceBatch:
		source = priority.SourceBatch
	case caller.Source == uxrecord.SourcePlayground, caller.Source == uxrecord.SourceEvaluation:
		source = priority.SourceConsole
	case caller.Source == uxrecord.SourceAPI && caller.APIKeyID != 0 && keyed:
		source = priority.SourceKey
	}
	return priority.Resolve(table, source, binding, asked)
}

type priorityDecisionKey struct{}

// decidePriority decides the request's class once, before a deployment is
// chosen: a class that does not exist is refused before the request takes a
// lease or wakes a sleeping deployment, and every attempt of the request
// (retries, a replay after preemption) runs at the same class.
func (p *Proxy) decidePriority(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	decision, ok := p.requestPriority(r)
	if !ok {
		writeAPIError(w, r, ErrorPriorityClassUnknown, map[string]any{"header": priorityClassHeader})
		return r, false
	}
	return r.WithContext(context.WithValue(r.Context(), priorityDecisionKey{}, decision)), true
}

// applyRequestPriority says the request's class in the answer's header and
// writes the engine's priority field: the class's value, in the engine's
// direction, when the target instance runs priority scheduling and the path
// generates, and the neutral 0 otherwise (only rewriting a body that names
// the field; named says whether it does). It writes the error itself when it
// refuses the request.
func (p *Proxy) applyRequestPriority(w http.ResponseWriter, r *http.Request, deploymentID uint, tgt target, owned, named bool) (*http.Request, func(), bool) {
	decision, ok := p.requestPriority(r)
	if !ok {
		writeAPIError(w, r, ErrorPriorityClassUnknown, map[string]any{"header": priorityClassHeader})
		return r, func() {}, false
	}
	w.Header().Set(priorityClassHeader, decision.Class.Name)
	r = r.WithContext(context.WithValue(r.Context(), priorityClassKey{}, decision.Class.Name))
	r.Header.Del(priorityHeader)
	value := 0
	if dep := p.routedDeployment(deploymentID); dep != nil && tgt.upstreamPath == "" && p.instanceRunsPriority(tgt.instanceUUID) {
		if v, engineHas := deploy.PriorityFieldValue(dep.Engine, decision.Class.Value); engineHas {
			value = v
		}
	}
	if !owned || (value == 0 && !named) {
		return r, func() {}, true
	}
	encoded := []byte(strconv.Itoa(value))
	var cleanup func()
	var err error
	if named {
		cleanup, err = setRequestJSONField(r, priorityField, encoded, value != 0, p.maxBodyBytes)
	} else {
		// The body names no such key at all: it goes in first, unsearched.
		cleanup, err = insertRequestJSONField(r, priorityField, encoded, p.maxBodyBytes)
	}
	if err != nil {
		writeRefusal(w, r, requestRefusal(err))
		return r, cleanup, false
	}
	return r, cleanup, true
}

// ownGatewayFields sets the request fields only the gateway may set,
// found in one pass over the body: a client's kv_transfer_params is
// neutralized (ADR 0028) and the priority written (applyRequestPriority).
// fail answers a read or rewrite error; a refused class is answered and
// accounted here. ok is false when the request was answered.
func (p *Proxy) ownGatewayFields(w http.ResponseWriter, r *http.Request, tgt target, deploymentID uint, fail func(error)) (*http.Request, func(), bool) {
	cleanupBody, err := ensureReplayableBody(r, p.maxBodyBytes)
	if err != nil {
		fail(err)
		return r, func() {}, false
	}
	owned, err := ownedJSONBody(r)
	named := []bool{false, false}
	if err == nil && owned {
		named, err = bodyNames(r, kvTransferField, priorityField)
	}
	if err != nil {
		cleanupBody()
		fail(err)
		return r, func() {}, false
	}
	cleanupKV, err := stripClientKVTransfer(r, p.maxBodyBytes, named[0])
	if err != nil {
		cleanupKV()
		cleanupBody()
		fail(err)
		return r, func() {}, false
	}
	r, cleanupPriority, ok := p.applyRequestPriority(w, r, deploymentID, tgt, owned, named[1])
	cleanup := func() { cleanupPriority(); cleanupKV(); cleanupBody() }
	if !ok {
		p.releaseTarget(tgt)
		p.acctFor(r.Context()).RecordImmediate(deploymentID, http.StatusBadRequest, false)
	}
	return r, cleanup, ok
}

// instanceRunsPriority reports whether the instance was launched with the
// engine's priority scheduling on (routeSnapshot.prioritized): a deployment
// that asked for it may still run an instance without it, on a target that
// does not allow it.
func (p *Proxy) instanceRunsPriority(instanceUUID string) bool {
	if p.routes == nil || instanceUUID == "" {
		return false
	}
	snap := p.routes.current.Load()
	return snap != nil && snap.prioritized[instanceUUID]
}

type priorityClassKey struct{}

// priorityClassFrom is the class the request ran at ("" before the gateway
// decided one), for its record.
func priorityClassFrom(ctx context.Context) string {
	name, _ := ctx.Value(priorityClassKey{}).(string)
	return name
}

// routedDeployment is the deployment the route table holds under id, nil
// when the table has not caught up with it (the request then runs at the
// neutral level).
func (p *Proxy) routedDeployment(id uint) *store.Deployment {
	if p.routes == nil {
		return nil
	}
	snap := p.routes.current.Load()
	if snap == nil {
		return nil
	}
	if rd, ok := snap.byID[id]; ok && rd != nil {
		return rd.dep
	}
	return nil
}
