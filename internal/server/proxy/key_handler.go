package proxy

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/priority"
)

// KeyPath is the key information route: what the presenting API key's
// requests run at and what the models it may call do with that, for a
// client (a gateway in front of GPUWorks, say) that wants to know before it
// sends. Like /v1/models it is not traffic: it is not accounted, leaves no
// request record, and answers from the route table and the class snapshot
// without reading deployment rows.
const KeyPath = "/v1/key"

// Features a /v1/key answer may list: what this server does that a client
// can rely on. A feature, once listed, is never taken away.
const (
	// FeaturePriorityScheduling: every answer the gateway routes to a model
	// says in X-GPUWorks-Priority-Scheduling whether the engine scheduled
	// the request by its class.
	FeaturePriorityScheduling = "priority_scheduling_header"
	// FeatureBusyReject: a model request declaring X-GPUWorks-If-Busy:
	// reject is answered at once with 429 MODEL_BUSY when the gateway
	// judges it cannot take it now (busy_reject.go).
	FeatureBusyReject = "busy_reject"
)

// priorityScheduling says in the answer whether the engine scheduled the
// request by its class ("on") or ignored it ("off").
const priorityScheduling = "X-GPUWorks-Priority-Scheduling"

// Coverage of a model's running replicas by priority scheduling.
const (
	SchedulingAll  = "all"
	SchedulingSome = "some"
	SchedulingNone = "none"
)

// KeyInfo is the /v1/key answer.
type KeyInfo struct {
	Object   string   `json:"object"`
	Features []string `json:"features"`
	// Priority is nil for a request without a key (open access).
	Priority *KeyPriority    `json:"priority"`
	Models   []KeyModelState `json:"models"`
}

// KeyPriority is the key's priority binding as a client sees it
// (priority.Reach): a class header naming a Reachable class runs at it, any
// other class at Ceiling (a name that is no class is refused); no header runs
// at Own.
type KeyPriority struct {
	Own       KeyClass   `json:"own"`
	Ceiling   KeyClass   `json:"ceiling"`
	Reachable []KeyClass `json:"reachable"`
}

// KeyClass is a priority class by name and value (larger is more important).
type KeyClass struct {
	Name  string `json:"name"`
	Value int    `json:"value"`
}

// KeyModelState is what a model the key may call does with its requests.
type KeyModelState struct {
	// ID is the model's name, as /v1/models lists it.
	ID string `json:"id"`
	// PriorityScheduling is how many of its running replicas schedule by
	// priority: SchedulingAll, SchedulingSome or SchedulingNone.
	PriorityScheduling string `json:"priority_scheduling"`
	// BusyReject says whether it refuses at once a request that asks not to
	// wait when it is busy: every running replica of it states its run
	// slots, and none is PD-disaggregated or multi-node.
	BusyReject bool `json:"busy_reject"`
}

// KeyHandler serves GET /v1/key (KeyPath). Authentication and the key's
// scope are those of /v1/models.
func (p *Proxy) KeyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, identity, ok := p.authenticate(w, r)
		if !ok {
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, r, ErrorMethodNotAllowed, map[string]any{"path": KeyPath, "method": http.MethodGet})
			return
		}
		info := KeyInfo{Object: "key", Features: []string{FeaturePriorityScheduling, FeatureBusyReject}, Models: p.routes.modelStates(identity.Allows)}
		if identity.KeyID != 0 {
			reach := priority.KeyReach(p.priority.get(), identity.Priority)
			info.Priority = &KeyPriority{Own: keyClass(reach.Own), Ceiling: keyClass(reach.Ceiling), Reachable: make([]KeyClass, 0, len(reach.Reachable))}
			for _, c := range reach.Reachable {
				info.Priority.Reachable = append(info.Priority.Reachable, keyClass(c))
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(info)
	}
}

func keyClass(c priority.Class) KeyClass { return KeyClass{Name: c.Name, Value: c.Value} }

// modelStates are the served names allow accepts, in name order as
// servedNames lists them, each with how its running replicas schedule and
// whether a busy one refuses a request that asked it to. A name several
// deployments share counts the replicas of those allow accepts.
func (r *routeTable) modelStates(allow func(deploymentID uint) bool) []KeyModelState {
	out := []KeyModelState{}
	if r == nil {
		return out
	}
	s := r.current.Load()
	if s == nil {
		return out
	}
	for name, deps := range s.byName {
		if name == "" {
			continue
		}
		replicas, prioritized, allowed, rejects := 0, 0, false, true
		for _, d := range deps {
			if allow(d.id) {
				allowed = true
				replicas += d.replicas
				prioritized += d.prioritizedReplicas
				rejects = rejects && d.rejectsBusy()
			}
		}
		if !allowed {
			continue
		}
		coverage := SchedulingNone
		switch {
		case prioritized > 0 && prioritized == replicas:
			coverage = SchedulingAll
		case prioritized > 0:
			coverage = SchedulingSome
		}
		out = append(out, KeyModelState{ID: name, PriorityScheduling: coverage, BusyReject: rejects && replicas > 0})
	}
	slices.SortFunc(out, func(a, b KeyModelState) int { return strings.Compare(a.ID, b.ID) })
	return out
}
