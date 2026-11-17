package proxy

import (
	"maps"

	"gpuworks.ai/gpuworks/internal/proto"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// pickBy leases a candidate by the deployment's routing policy, passing over
// the excluded ones.
func (b *balancer) pickBy(policy string, candidates []candidate, affinity string, excluded map[string]bool, online func(string) bool) *lease {
	switch policy {
	case deploy.Failover:
		return b.pickFailover(candidates, excluded, online)
	case deploy.PrefixAffinity:
		return b.pickAffinity(candidates, affinity, excluded, online)
	default:
		return b.pick(candidates, excluded, online)
	}
}

// leaseUntil leases with pick until accept takes the lease, giving back and
// excluding each one it turns down; nil when none is left. excluded is not
// modified.
func leaseUntil(pick func(excluded map[string]bool) *lease, excluded map[string]bool, accept func(*lease) bool) *lease {
	skip := maps.Clone(excluded)
	for {
		l := pick(skip)
		if l == nil || accept(l) {
			return l
		}
		l.release(outcomeCanceled)
		if skip == nil {
			skip = map[string]bool{}
		}
		skip[l.c.instanceUUID] = true
	}
}

// awake reports whether a leased instance may still take the request: still
// running on the same port and not ordered to sleep since its candidate was read. A sleep order takes effect
// for routing at once, before the engine reports the new level, and a route
// snapshot is refreshed only asynchronously. A candidate no instance change
// has passed since its snapshot was built is as it was read; any other is
// one keyed read of the leased instance.
func (p *Proxy) awake(l *lease) bool {
	if instanceRowsUnchanged(p.store, l.c.routedAt) {
		return true
	}
	in, err := p.store.InstanceByUUID(l.c.instanceUUID)
	return err == nil && in.State == proto.InstRunning && !in.Headless && in.APIPort > 0 &&
		in.APIPort == l.c.port && !planning.InstanceAsleep(in)
}

// pickAwake leases an awake candidate by the routing policy. Candidates built
// from rows read for this request (fresh) are taken as read; those from the
// route snapshot are checked, and when none passes the caller falls back to
// reading the rows, which tells asleep from not ready.
func (p *Proxy) pickAwake(candidates []candidate, policy, affinity string, fresh bool) *lease {
	accept := func(*lease) bool { return true }
	if !fresh {
		accept = p.awake
	}
	return leaseUntil(func(excluded map[string]bool) *lease {
		return p.balancer.pickBy(policy, candidates, affinity, excluded, p.hub.Online)
	}, nil, accept)
}

// routingGeneration counts the instance and deployment changes the store's
// bus has announced; 0 without a bus. Every write of an instance row's
// state, port, sleep level or desired state announces one of them after it
// commits and before it returns: most as an instance change, a
// deployment-wide sleep order (which rewrites its instances' sleep intent)
// as a deployment change. Both counters only grow, so the sum stands still
// exactly while neither moves.
func routingGeneration(st *store.Store) uint64 {
	if st == nil || st.Bus == nil {
		return 0
	}
	return st.Bus.Generation("instance") + st.Bus.Generation("deployment")
}

// instanceRowsUnchanged reports whether nothing that can change an instance
// row was announced since a route snapshot was built at routedAt
// (candidate.routedAt): its candidates are then exactly the rows a read
// would return. A change announced before the caller looks is always seen,
// so a writer that has returned is never overtaken.
func instanceRowsUnchanged(st *store.Store, routedAt uint64) bool {
	return routedAt != 0 && st != nil && st.Bus != nil && routingGeneration(st)+1 == routedAt
}
