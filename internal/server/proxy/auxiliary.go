package proxy

import (
	"context"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

// An auxiliary route (inferenceapi.AuxiliaryRoute: /v1/tokenize and
// /v1/detokenize) is chosen by the request's model and goes the way any /v1
// request does — the same replica pick, sleep and 503 answers, model rewrite
// and retries — but to the engine path the table names, and, like a probe
// (probe.go), it is not traffic: nothing accounts it, it is not in the
// in-flight table, it leaves no request record and does not move the
// deployment's last request time. A deployment can close the routes
// (store.Deployment.TokenizeDisabled); the route table carries the switch, so
// the check costs no store read.

func (p *Proxy) serveAuxiliary(w http.ResponseWriter, r *http.Request, route inferenceapi.AuxiliaryRoute) {
	if r.Method != route.Method {
		w.Header().Set("Allow", route.Method)
		writeAPIError(w, r, ErrorMethodNotAllowed, map[string]any{"path": route.Path, "method": route.Method})
		return
	}
	modelName, cleanup, err := extractModel(r, p.maxBodyBytes)
	defer cleanup()
	if err != nil {
		writeRefusal(w, r, requestRefusal(err))
		return
	}
	if modelName == "" {
		writeAPIError(w, r, ErrorModelRequired, nil)
		return
	}
	dep, tgt, rejection, _ := p.selectForRequest(r.Context(), modelName, p.affinityKey(modelName, r))
	if rejection != nil {
		writeRefusal(w, r, rejection)
		return
	}
	if dep.TokenizeDisabled {
		p.releaseTarget(tgt)
		writeAPIError(w, r, ErrorTokenizeDisabled, map[string]any{"model": modelName, "method": r.Method, "path": route.Path})
		return
	}
	tgt.upstreamPath = route.Upstream
	r = r.WithContext(context.WithValue(r.Context(), probeKey, &probeTrace{}))
	p.serveSelectedTarget(w, r, modelName, tgt, dep.ID)
}
