package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// setYieldPolicy sets how the deployment shares its GPUs with others and the
// parameters that go with it (docs/explanation/service-levels.md). A policy
// other than never needs a deployment planning.YieldPolicyRefusal allows: one
// that can sleep, single-node and not PD. It answers the
// policy as stored, naming the other idle-policy deployments on the same
// cards when the policy is idle: the two may share, but neither gives way
// to the other while busy.
func (a *apiHandlers) setYieldPolicy(w http.ResponseWriter, r *http.Request) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var req dto.YieldPolicyRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	p, v := req.Parse()
	if v != nil {
		writeErr(w, http.StatusBadRequest, deployment.YieldPolicyViolation(v))
		return
	}
	if err := deployment.YieldPolicyRefusal(p, planning.YieldPolicyRefusal(dep), dep.Engine); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.d.Store.SetDeploymentYieldPolicy(dep.ID, p); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := dto.YieldPolicyOf(p)
	if p.Yield == yieldpolicy.Idle {
		if out.IdleNeighbours, err = a.idleNeighbours(dep); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// idleNeighbours names the other idle-policy deployments with an instance on
// one of dep's cards.
func (a *apiHandlers) idleNeighbours(dep *store.Deployment) ([]string, error) {
	mine, err := a.d.Store.InstancesForDeployment(dep.ID)
	if err != nil {
		return nil, err
	}
	cards := planning.InstanceCards(mine)
	if len(cards) == 0 {
		return nil, nil
	}
	sharing, err := a.d.Store.SharingDeployments()
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range sharing {
		other := &sharing[i]
		if other.ID == dep.ID || !yieldpolicy.IdleNeighbours(yieldpolicy.Idle, other.YieldPolicy) {
			continue
		}
		insts, err := a.d.Store.InstancesForDeployment(other.ID)
		if err != nil {
			return nil, err
		}
		for card := range planning.InstanceCards(insts) {
			if cards[card] {
				out = append(out, other.Name)
				break
			}
		}
	}
	return out, nil
}

// yieldPolicyDictionary answers the yield policies with their defaults,
// which parameters apply and the parameters' ranges, for a client's editor.
func (a *apiHandlers) yieldPolicyDictionary(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, dto.YieldPolicyDictionaryOf())
}
