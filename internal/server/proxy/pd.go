// pd.go is the gateway half of PD disaggregation (ADR 0028). A PD
// deployment's decode instances are its ordinary candidates, picked by the
// deployment's routing policy; its prefill instances are picked second, by
// prefix affinity, and asked to prefill a copy of the request. The
// kv_transfer_params the prefill instance answers with go into the request the
// decode instance receives, which then pulls the KV itself. The gateway never
// reads inside kv_transfer_params: its shape differs between engine versions.

package proxy

import (
	"net/http"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

// kvTransferField is the request and response field that carries the KV
// handoff between the two roles.
const kvTransferField = "kv_transfer_params"

// Fallback reasons, the label of gpuworks_proxy_pd_fallback_total.
const (
	pdFallbackNoPrefill     = "no_prefill"
	pdFallbackUnreachable   = "prefill_unreachable"
	pdFallbackServerError   = "prefill_5xx"
	pdFallbackNoKVParams    = "prefill_no_kv_params"
	pdFallbackPrefillFailed = "prefill_failed"

	// pdFallbackNoBootstrap: no prefill instance's bootstrap server reported
	// every TP rank of it registered (pdmode.WireSequentialBootstrap).
	pdFallbackNoBootstrap = "prefill_no_bootstrap"
)

// stripClientKVTransfer neutralises a client-supplied kv_transfer_params on
// every JSON request, PD or not: the field steers the engine's KV connector,
// and a malformed one was measured to kill the vLLM engine core with a single
// request. The value becomes null, which every engine reads as absent. Bodies
// that do not mention the field are left untouched without a copy.
func stripClientKVTransfer(r *http.Request, maxBody int64, named bool) (func(), error) {
	if !named {
		return func() {}, nil
	}
	return setRequestJSONField(r, kvTransferField, []byte("null"), false, maxBody)
}

// pickPrefill chooses the prefill instance: by prefix affinity when the
// request has a prefix, else the least loaded one on the decode instance's
// node, else the least loaded anywhere. KV then travels the shortest path.
// One ordered to sleep since the route snapshot was built is passed over, as
// for decode instances (pickAwake).
func (p *Proxy) pickPrefill(route *pdRoute, affinity, decodeNode string, excluded map[string]bool, adm admission) *lease {
	return leaseUntil(func(skip map[string]bool) *lease {
		return p.pickPrefillOnce(route, affinity, decodeNode, skip, adm)
	}, excluded, p.awake)
}

func (p *Proxy) pickPrefillOnce(route *pdRoute, affinity, decodeNode string, excluded map[string]bool, adm admission) *lease {
	if strings.TrimSpace(affinity) != "" {
		l, _ := p.balancer.pickAffinity(route.prefill, affinity, excluded, p.hub.Online, adm)
		return l
	}
	var local []candidate
	for _, c := range route.prefill {
		if c.nodeUUID == decodeNode {
			local = append(local, c)
		}
	}
	if l, _ := p.balancer.pick(local, excluded, p.hub.Online, adm); l != nil {
		return l
	}
	l, _ := p.balancer.pick(route.prefill, excluded, p.hub.Online, adm)
	return l
}

// servePrefillStage runs the prefill stage of a two-stage request and leaves
// the request ready for the decode instance: with the prefill instance's
// kv_transfer_params, or, on fallback, without any. It reports false when it
// already answered the client (a 4xx from prefill, a strict fallback, or a
// client that went away).
func (p *Proxy) servePrefillStage(w http.ResponseWriter, r *http.Request, tgtp *target, deploymentID uint) (bool, func()) {
	route := tgtp.pd
	if route == nil || !route.mode.TwoStage(r.URL.Path) || !route.wantsPrefill() {
		return true, func() {}
	}
	switch route.mode.Wire {
	case pdmode.WireConcurrent:
		return p.serveConcurrentPrefill(w, r, tgtp, deploymentID)
	default:
		return p.serveSequentialPrefill(w, r, tgtp, deploymentID)
	}
}

// releaseTarget gives back the decode lease a request held while its prefill
// stage ran, when the request ends before reaching the decoder.
func (p *Proxy) releaseTarget(tgt target) {
	if tgt.attempt != nil && tgt.attempt.current != nil {
		tgt.attempt.current.release(outcomeCanceled)
	} else if tgt.lease != nil {
		tgt.lease.release(outcomeCanceled)
	}
}
