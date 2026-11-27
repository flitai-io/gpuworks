package proxy

import (
	"context"
	"net/http"
	"strconv"
	"strings"
)

// Busy rejection (docs/reference/openai-gateway.md, 繁忙婉拒): a caller that
// has somewhere else to send a request, such as a gateway in front of
// GPUWorks, declares with X-GPUWorks-If-Busy: reject that it would rather be
// told at once that the model is busy than have the request wait in an
// engine's queue. Without the declaration nothing changes. The gateway judges
// when it leases a replica, under the balancer's lock, so two requests never
// both take the last free run slot.
const (
	// ifBusyHeader carries the declaration; it never reaches an engine.
	ifBusyHeader = "X-GPUWorks-If-Busy"
	// ifBusyReject is the one value that declares it; any other is as if
	// the header were absent.
	ifBusyReject = "reject"
	// busyRetryAfterSeconds is the Retry-After of a busy refusal: a run
	// slot frees as soon as one answer ends.
	busyRetryAfterSeconds = 1
)

type busyDeclarationKey struct{}

// takeBusyDeclaration removes the declaration from r, so no engine sees it,
// and says whether r made it.
func takeBusyDeclaration(r *http.Request) bool {
	declared := strings.TrimSpace(r.Header.Get(ifBusyHeader)) == ifBusyReject
	r.Header.Del(ifBusyHeader)
	return declared
}

// withBusyDeclaration carries whether the request's selections may refuse it
// when busy: set for a request that declared it, cleared for one the gateway
// has already taken (a replay after preemption) or holds for a wake.
func withBusyDeclaration(ctx context.Context, declared bool) context.Context {
	return context.WithValue(ctx, busyDeclarationKey{}, declared)
}

func busyDeclared(ctx context.Context) bool {
	declared, _ := ctx.Value(busyDeclarationKey{}).(bool)
	return declared
}

// waiting is adm without the declaration: for a resend of a request a
// replica already took, and for the deployments that never refuse when busy
// (PD-disaggregated and multi-node ones, whose run slots the gateway does not
// count as a whole).
func (a admission) waiting() admission {
	a.rejectBusy = false
	return a
}

// hasRoom reports whether c may take a request of adm: always without the
// declaration, and with it when c's launch states no run slots (the gateway
// does not guess an engine's default) or fewer of the leases it waits behind
// than c's slots are held. On a replica that schedules by priority those are
// the leases at adm's value or above, since the engine runs this request
// before lower ones; on any other the engine runs requests in arrival order,
// so every lease counts. The caller holds b.mu.
func (b *balancer) hasRoom(c candidate, adm admission) bool {
	if !adm.rejectBusy || c.slots <= 0 {
		return true
	}
	counts := &b.state(c.instanceUUID).inflight
	ahead := counts.total
	if c.prioritized {
		ahead = counts.atLeast(adm.priority)
	}
	return ahead < c.slots
}

// withRoom is cs without the candidates that have no room for adm,
// filtered in place. The caller holds b.mu.
func (b *balancer) withRoom(cs []candidate, adm admission) []candidate {
	if !adm.rejectBusy {
		return cs
	}
	out := cs[:0]
	for _, c := range cs {
		if b.hasRoom(c, adm) {
			out = append(out, c)
		}
	}
	return out
}

// busyRefusal refuses a request that asked not to wait, for modelName, none
// of whose eligible replicas had a free run slot for it.
func busyRefusal(modelName string) *refusal {
	return refuse(ErrorModelBusy, map[string]any{"model": modelName})
}

// answerRefusal accounts (under deploymentID, 0 for a model no deployment
// serves) and answers a request refused before it reached an engine. A busy
// refusal is counted in the 429 class without being a request
// of the deployment's (RecordBusy), and says when to ask again.
func (p *Proxy) answerRefusal(w http.ResponseWriter, r *http.Request, deploymentID uint, rejection *refusal, asleep bool) {
	if rejection.code == ErrorModelBusy {
		p.acct.RecordBusy(deploymentID)
		w.Header().Set("Retry-After", strconv.Itoa(busyRetryAfterSeconds))
	} else {
		p.acct.RecordImmediate(deploymentID, rejection.status(), asleep)
	}
	writeRefusal(w, r, rejection)
}

// RecordBusy counts a busy refusal in the deployment's 429 class. It leaves
// the deployment's last request time alone: the refused request was never
// the deployment's to serve, and a busy deployment has requests in flight
// that keep it from counting as idle anyway.
func (a *Accountant) RecordBusy(deploymentID uint) {
	if a == nil {
		return
	}
	a.mu.Lock()
	c := a.deploymentLocked(deploymentID)
	c.classTotals[class429]++
	c.dirty = true
	a.mu.Unlock()
}
