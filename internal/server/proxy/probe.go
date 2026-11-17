package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"
)

// A probe is the server's own request to a deployment (the health check,
// internal/server/healthcheck): it takes the path a client's request would
// — routing, retries, PD, the model rewrite — but is not traffic. Nothing
// accounts it, it is not in the in-flight table and leaves no request
// record, so the deployment's figures and its last request time are the
// clients' alone. An auxiliary route (auxiliary.go) carries the same mark
// and is left out the same way.

type probeKeyType struct{}

var probeKey probeKeyType

// probeTrace collects what the probe's caller wants back from the path.
type probeTrace struct {
	instanceUUID string
	message      string
}

func probeFrom(ctx context.Context) *probeTrace {
	t, _ := ctx.Value(probeKey).(*probeTrace)
	return t
}

// acctFor is the request's accountant: none for a probe or an auxiliary
// route.
func (p *Proxy) acctFor(ctx context.Context) *Accountant {
	if probeFrom(ctx) != nil {
		return nil
	}
	return p.acct
}

// ProbeReply is what came of a probe.
type ProbeReply struct {
	// NoReplica: no awake running replica to send it to; nothing was sent.
	NoReplica bool
	// HTTPStatus is the reply's status, 0 when none came.
	HTTPStatus int
	// InstanceUUID is the replica that answered, or the last one tried.
	InstanceUUID string
	// Message is the start of an error reply, or why none came.
	Message string
}

// probeMessageMax bounds the error text a probe keeps.
const probeMessageMax = 300

// Probe posts payload, with the deployment's served name as its model, to
// path on one of the deployment's replicas, as a client request would go
// but unaccounted. ctx bounds it.
func (p *Proxy) Probe(ctx context.Context, deploymentID uint, path string, payload map[string]any) ProbeReply {
	servedName, tgt, ok := p.probeTarget(deploymentID)
	if !ok {
		return ProbeReply{NoReplica: true}
	}
	return p.probeSelected(ctx, deploymentID, path, payload, servedName, tgt)
}

// ProbeForInstance is an internal, unaccounted probe of one exact replica.
// The existing pinned-target checks enforce deployment ownership, readiness,
// sleep and node liveness. Its target has no alternate-replica retry attempt.
func (p *Proxy) ProbeForInstance(ctx context.Context, deploymentID uint, instanceUUID, path string, payload map[string]any) ProbeReply {
	dep, err := p.store.GetDeployment(deploymentID)
	if err != nil {
		return ProbeReply{NoReplica: true}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, nil)
	if err != nil {
		return ProbeReply{Message: err.Error()}
	}
	req.Header.Set(instanceHeader, instanceUUID)
	selected, tgt, pinned, rejection, _ := p.pinnedTarget(req, dep.ServedName)
	if !pinned || rejection != nil || selected == nil || selected.ID != deploymentID {
		return ProbeReply{NoReplica: true, HTTPStatus: rejection.status(), Message: rejection.message()}
	}
	return p.probeSelected(ctx, deploymentID, path, payload, dep.ServedName, tgt)
}

func (p *Proxy) probeSelected(ctx context.Context, deploymentID uint, path string, payload map[string]any, servedName string, tgt target) (out ProbeReply) {
	body := make(map[string]any, len(payload)+1)
	for k, v := range payload {
		body[k] = v
	}
	body["model"] = servedName
	raw, err := json.Marshal(body)
	if err != nil {
		p.releaseTarget(tgt)
		return ProbeReply{Message: err.Error()}
	}
	trace := &probeTrace{instanceUUID: tgt.instanceUUID}
	req, err := http.NewRequestWithContext(context.WithValue(ctx, probeKey, trace), http.MethodPost, path, bytes.NewReader(raw))
	if err != nil {
		p.releaseTarget(tgt)
		return ProbeReply{Message: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	w := &probeWriter{header: http.Header{}}
	defer func() {
		if failure := recover(); failure != nil {
			if failure != http.ErrAbortHandler {
				panic(failure)
			}
			out = ProbeReply{HTTPStatus: w.status, InstanceUUID: trace.instanceUUID, Message: "the upstream response body was interrupted"}
		}
	}()
	p.serveSelectedTarget(w, req, servedName, tgt, deploymentID)
	out = ProbeReply{HTTPStatus: w.status, InstanceUUID: trace.instanceUUID, Message: trace.message}
	if w.status == 0 && w.wrote {
		out.HTTPStatus = http.StatusOK
	}
	switch {
	case ctx.Err() != nil:
		out.Message = ctx.Err().Error()
	case out.HTTPStatus < 200 || out.HTTPStatus >= 300:
		out.Message = clip(strings.TrimSpace(w.body.String()), probeMessageMax)
	}
	return out
}

// probeTarget picks the replica a probe of the deployment goes to: from the
// route snapshot when it has one ready, else from the store, as a request
// whose snapshot has none would.
func (p *Proxy) probeTarget(id uint) (servedName string, tgt target, ok bool) {
	if p.routes != nil {
		if rd := p.routes.deployment(id); rd != nil {
			if tgt, ok := p.pickRouted(rd, rd.servedName, "", p.hold.drainingOf(id)); ok {
				return rd.servedName, tgt, true
			}
		}
	}
	dep, err := p.store.GetDeployment(id)
	if err != nil || dep == nil {
		return "", target{}, false
	}
	tgt, rejection, _ := p.selectDeploymentInstances(dep, dep.ServedName, "")
	if rejection != nil {
		return "", target{}, false
	}
	return dep.ServedName, tgt, true
}

// probeWriter keeps a probe's reply status and the start of its body.
type probeWriter struct {
	header http.Header
	status int
	wrote  bool
	body   bytes.Buffer
}

func (w *probeWriter) Header() http.Header { return w.header }

func (w *probeWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *probeWriter) Write(b []byte) (int, error) {
	w.wrote = true
	if room := probeMessageMax*4 - w.body.Len(); room > 0 {
		w.body.Write(b[:min(len(b), room)])
	}
	return len(b), nil
}

func (w *probeWriter) Flush() {}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}
