package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

// PlaygroundHandler routes deployed models through the normal internal data
// plane and namespaced external models through encrypted AI endpoint configs.
// Callers must mount it below management JWT authentication.
func (p *Proxy) PlaygroundHandler(box *auth.Secretbox) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The management API hands over the signed-in user's claims; the
		// request record attributes the request to them.
		caller := uxrecord.Caller{Source: uxrecord.SourcePlayground, ReceivedMS: time.Now().UnixMilli()}
		claims, signedIn := auth.ClaimsFrom(r.Context())
		if signedIn {
			caller.UserID = claims.UserID
		}
		// Any class may be named here because only an administrator may bind
		// a key to any class; anyone else runs at the default.
		if !signedIn || claims.Role != "admin" {
			r.Header.Del(priorityClassHeader)
		}
		// A server-origin evaluation's requests say they are one, so the
		// request records tell them from the playground's. Only the
		// administrator who may start one is believed; an engine never
		// sees the header.
		if r.Header.Get(loadgen.BenchmarkRunHeader) != "" && signedIn && claims.Role == "admin" {
			caller.Source = uxrecord.SourceEvaluation
		}
		r.Header.Del(loadgen.BenchmarkRunHeader)
		r = r.WithContext(uxrecord.WithCaller(r.Context(), caller))
		modelName, cleanup, err := extractModel(r, p.maxBodyBytes)
		defer cleanup()
		if err != nil {
			writeRefusal(w, r, requestRefusal(err))
			return
		}
		if strings.HasPrefix(modelName, "external:") {
			p.serveExternal(w, r, modelName, box)
			return
		}
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api/v1/playground")
		// A class that does not exist is refused before a deployment is
		// chosen or woken, as on /v1.
		r, ok := p.decidePriority(w, r)
		if !ok {
			return
		}
		// A pinned refusal for sleep is the same event the load balancing path
		// accounts as asleep_503, not a generic 5xx.
		deployment, tgt, pinned, rejection, asleep := p.pinnedTarget(r, modelName)
		if !pinned {
			affinity := p.affinityKey(modelName, r)
			deployment, tgt, rejection, asleep = p.selectForRequest(r.Context(), modelName, affinity)
			if rejection != nil && asleep {
				deployment, tgt, rejection, asleep = p.holdForWake(r.Context(), w, modelName, affinity, deployment)
			}
		}
		if rejection != nil {
			p.acct.RecordImmediate(deploymentIDOrZero(deployment), rejection.status(), asleep)
			writeRefusal(w, r, rejection)
			return
		}
		contract := inferenceapi.ResolveDeploymentContract(p.store, deployment)
		if !inferenceapi.AllowsContractRequest(contract, r.Method, r.URL.Path) {
			p.acct.RecordImmediate(deployment.ID, http.StatusNotFound, false)
			writeAPIError(w, r, ErrorModelRouteNotExposed, map[string]any{"model": modelName, "method": r.Method, "path": r.URL.Path})
			return
		}
		p.serveSelectedTarget(w, r, modelName, tgt, deployment.ID)
	}
}

// deploymentIDOrZero returns dep.ID, or 0 when dep is nil (a model that never
// resolved to any deployment at all — accounting attributes that under
// deployment 0, per the proxy metrics contract).
func deploymentIDOrZero(dep *store.Deployment) uint {
	if dep == nil {
		return 0
	}
	return dep.ID
}

func (p *Proxy) serveExternal(w http.ResponseWriter, r *http.Request, selected string, box *auth.Secretbox) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/v1/playground/v1")
	if tail != "/chat/completions" {
		writeAPIError(w, r, ErrorExternalRouteUnsupported, nil)
		return
	}
	parts := strings.SplitN(selected, ":", 3)
	if len(parts) != 3 {
		writeAPIError(w, r, ErrorExternalModelInvalid, nil)
		return
	}
	id, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		writeAPIError(w, r, ErrorExternalModelInvalid, nil)
		return
	}
	configs, err := p.store.ListAIConfigs()
	if err != nil {
		writeAPIError(w, r, ErrorGatewayInternal, map[string]any{"detail": err.Error()})
		return
	}
	var config *store.AIConfig
	for i := range configs {
		if uint64(configs[i].ID) == id {
			config = &configs[i]
			break
		}
	}
	if config == nil {
		writeAPIError(w, r, ErrorExternalModelNotFound, nil)
		return
	}
	key, err := store.DecryptAIKey(box, config.APIKey)
	if err != nil {
		writeAPIError(w, r, ErrorExternalKeyUnreadable, map[string]any{"detail": err.Error()})
		return
	}
	base, err := url.Parse(strings.TrimRight(config.BaseURL, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		writeAPIError(w, r, ErrorExternalEndpointInvalid, nil)
		return
	}
	var body map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&body); err != nil {
		writeAPIError(w, r, ErrorExternalRequestInvalid, map[string]any{"detail": err.Error()})
		return
	}
	body["model"] = config.Model
	encoded, err := json.Marshal(body)
	if err != nil {
		writeAPIError(w, r, ErrorExternalRequestInvalid, map[string]any{"detail": err.Error()})
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(encoded))
	r.ContentLength = int64(len(encoded))
	rp := &httputil.ReverseProxy{
		Director: func(out *http.Request) {
			out.URL.Scheme = base.Scheme
			out.URL.Host = base.Host
			out.URL.Path = strings.TrimRight(base.Path, "/") + "/" + strings.TrimLeft(tail, "/")
			out.Host = base.Host
			stripBrowserContext(out.Header)
			// The class is the gateway's own; an outside provider has no use for it.
			out.Header.Del(priorityClassHeader)
			out.Header.Set("Authorization", "Bearer "+key)
			out.Header.Set("Content-Type", "application/json")
		},
		FlushInterval:  -1,
		ModifyResponse: boundUpstreamError,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.log.Warn("external playground upstream error", "err", err)
			writeAPIError(w, r, ErrorExternalUpstreamUnavailable, map[string]any{"detail": err.Error()})
		},
	}
	rp.ServeHTTP(w, r)
}
