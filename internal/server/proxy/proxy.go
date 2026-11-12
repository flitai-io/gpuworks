// Package proxy is the data plane: it reverse-proxies OpenAI-compatible /v1
// requests to the right inference instance. It parses the model name from the
// request body, resolves an active deployment → running instance → node, and
// forwards the request through a hub tunnel to the instance's local port.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"

	// Aliased: New's own "hub" parameter (the Tunneler) shadows the package
	// name for the rest of this file, so the sentinel below is only reachable
	// through this alias.
	hubpkg "gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/tracecontext"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// dataLaneUnavailableRetryAfterSeconds is the Retry-After hint sent with the
// 503 a client gets when a tunnel open failed because the node's data lane
// was detached and did not reattach within the hub's own wait budget
// (hub.ErrDataLaneUnavailable). That wait already covered the common case, so
// a client that retries needs only a short pause, not the hub's full budget
// again.
const dataLaneUnavailableRetryAfterSeconds = 2

// Tunneler opens a data-plane connection to a node's agent. ctx bounds a
// detached data lane's wait for reattachment; the proxy passes the request's
// (or dial's) context so a client disconnect or deadline aborts the wait
// rather than outliving it.
type Tunneler interface {
	OpenTunnel(ctx context.Context, nodeUUID, dst string) (net.Conn, error)
	Online(nodeUUID string) bool
}

// Proxy serves /v1/*.
type Proxy struct {
	// recorder keeps raw exchanges during a debug session (capture.go).
	recorder     RequestRecorder
	store        *store.Store
	hub          Tunneler
	log          *slog.Logger
	rp           *httputil.ReverseProxy
	maxBodyBytes int64
	// acct is proxy request accounting (accounting.go). A nil *Accountant is
	// valid and every method on it no-ops, so a Proxy built without
	// SetAccountant behaves exactly as it did before accounting existed.
	acct       *Accountant
	routes     *routeTable
	serving    *servingDialer
	balancer   *balancer
	load       *engineLoad
	bufferPool *bufferPool
	handoffs   handoffs
	// bootstrap caches prefill instances' engine ids (pd_bootstrap.go).
	bootstrap bootstrapEngines
	// transport is the tunnel or serving-path transport under the retry
	// layer; the PD prefill stage sends its own requests over it.
	transport http.RoundTripper
	// live is the in-flight table (inflight.go); sink, when set, receives
	// each forwarded request's record as its reply ends (ADR 0042).
	live  liveTable
	guard *instanceGuard
	// hold keeps requests for deployments the platform put to sleep while
	// they wake.
	hold *wakeHold
	sink func(uxrecord.Request)
	// sampleRoot decides whether a trace the gateway starts is sampled
	// (request_identity.go); nil samples none.
	sampleRoot func(tracecontext.TraceID) bool
	// openAccess caches the security switch that lets /v1 accept a request
	// with no API key (open_access.go); the zero value reads as closed.
	openAccess inferenceOpenAccessCache
}

// SetRequestSink wires where each forwarded request's record goes. It must
// not block: it runs on the request's goroutine as the reply ends. Call it
// at most once, before the Proxy serves traffic.
func (p *Proxy) SetRequestSink(sink func(uxrecord.Request)) {
	p.sink = sink
}

// SetAccountant wires proxy request accounting. It is safe to call at most
// once, before the Proxy starts serving traffic; app assembly owns starting
// the Accountant's own flush loop (Accountant.Run) with the app's lifetime
// context.
func (p *Proxy) SetAccountant(a *Accountant) {
	p.acct = a
}

// Start resumes route-table refreshes after an App restart. New already starts
// the table, so this is idempotent for the normal first run.
func (p *Proxy) Start() {
	if p != nil && p.routes != nil {
		p.routes.start()
	}
}

// Close releases the route-table subscription and refresh goroutine.
func (p *Proxy) Close() {
	if p != nil && p.routes != nil {
		p.routes.close()
	}
}

// ObserveEngineMetrics is the proxy side of the hub metrics tee. It keeps
// only the two routing gauges and never blocks the hub ingest path.
func (p *Proxy) ObserveEngineMetrics(samples []proto.MetricSample) {
	if p != nil && p.load != nil {
		p.load.observe(samples)
	}
}

type ctxKey int

const targetKey ctxKey = 0
const attemptKey ctxKey = 1

// target identifies the selected instance's node + local port.
type target struct {
	nodeUUID     string
	port         int
	modelName    string
	instanceUUID string
	lease        *lease
	candidates   []candidate
	attempt      *routeAttempt
	// pd is set when the target is a PD deployment's decode instance; the
	// prefill stage runs before the request reaches it. affinity is the
	// request's prefix key, which picks the prefill instance.
	pd       *pdRoute
	affinity string
	// firstByte runs when the upstream's first byte reaches the client.
	firstByte func()
	// stage is a PD request's prefill stage (pd_stage.go) and live its
	// in-flight entry, made when the stage began; both nil otherwise.
	stage *pdStage
	live  *liveRequest
	// upstreamPath, when set, is the engine path the request goes to in
	// place of its own (an auxiliary route, auxiliary.go).
	upstreamPath string
}

// Networks tells the gateway which nodes serve over a serving network of their
// own; the app wires internal/server/network's Service in.
type Networks interface {
	ServingEndpoints() (map[string]network.ServingEndpoint, error)
}

// New builds a Proxy. maxBodyBytes caps the request body spooled to disk before
// routing; a non-positive value falls back to DefaultMaxBodyBytes.
func New(st *store.Store, networks Networks, hub Tunneler, log *slog.Logger, maxBodyBytes int64) *Proxy {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if maxBodyBytes <= 0 {
		maxBodyBytes = DefaultMaxBodyBytes
	}
	p := &Proxy{store: st, hub: hub, log: log, maxBodyBytes: maxBodyBytes}
	p.load = newEngineLoad()
	p.balancer = newBalancer(p.load)
	p.routes = newRouteTable(st, networks, hub, log)
	p.serving = newServingDialer(log, func(nodeUUID string, status store.ServingForwardStatus) {
		if st == nil {
			return
		}
		if err := st.SetNodeServingForward(nodeUUID, status); err != nil {
			log.Warn("record serving forward status", "node", nodeUUID, "err", err)
		}
	})
	p.guard = &instanceGuard{store: st}
	p.hold = newWakeHold()
	p.routes.setOnRebuild(func(active map[string]bool) {
		p.hold.signal()
		p.balancer.gc(active)
		p.guard.checkMissing(active)
	})
	p.routes.setOnInstanceChange(p.guard.check)
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			t, ok := ctx.Value(targetKey).(target)
			if !ok {
				return nil, errors.New("proxy: no target in context")
			}
			if a, ok := ctx.Value(attemptKey).(*routeAttempt); ok && a != nil && a.current != nil {
				t.nodeUUID = a.current.c.nodeUUID
				t.port = a.current.c.port
			}
			instanceUUID := t.instanceUUID
			if a, ok := ctx.Value(attemptKey).(*routeAttempt); ok && a != nil && a.current != nil {
				instanceUUID = a.current.c.instanceUUID
			}
			// A node with a serving network serves over it only; the agent
			// tunnel is the path of nodes without one.
			if ep, ok := p.routes.servingEndpoint(t.nodeUUID); ok && instanceUUID != "" {
				conn, err := p.serving.open(ctx, ep, t.nodeUUID, instanceUUID)
				if err != nil {
					return nil, &tunnelOpenError{err: err}
				}
				return p.acct.countServing(conn, t.nodeUUID), nil
			}
			conn, err := p.hub.OpenTunnel(ctx, t.nodeUUID, fmt.Sprintf("127.0.0.1:%d", t.port))
			if err != nil {
				return nil, &tunnelOpenError{err: err}
			}
			return p.acct.countServing(conn, t.nodeUUID), nil
		},
		// The tunnel is already a single logical connection; keep it simple.
		DisableKeepAlives: true,
	}
	p.transport = transport
	p.rp = &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			t := r.Context().Value(targetKey).(target)
			r.URL.Scheme = "http"
			r.URL.Host = fmt.Sprintf("127.0.0.1:%d", t.port)
			r.Host = r.URL.Host
			if t.upstreamPath != "" {
				r.URL.Path, r.URL.RawPath = t.upstreamPath, ""
			}
			stripBrowserContext(r.Header)
			setTraceHeaders(r.Context(), r.Header)
		},
		FlushInterval: -1,
		Transport:     &retryTransport{base: transport, balancer: p.balancer, hub: p.hub, awake: p.awake, guard: p.guard},
		BufferPool:    nil,
		// ModifyResponse runs after the upstream status/headers arrive but before
		// the body is copied. It records the declared framing and wraps the body
		// so a mid-copy failure (tunnel/yamux death after headers) is logged with
		// the request id, declared length, copied bytes and the copy error — the
		// gap ErrorHandler cannot cover because it only fires before headers.
		ModifyResponse: func(resp *http.Response) error {
			ob, _ := resp.Request.Context().Value(obsKey).(*copyObservation)
			if ob != nil {
				ob.status, ob.declaredLen = resp.StatusCode, resp.ContentLength
			}
			if err := boundUpstreamError(resp); err != nil {
				return err
			}
			if ob == nil {
				return nil
			}
			// A 101 upgrade hands the raw connection to handleUpgradeResponse,
			// which type-asserts the body to io.ReadWriteCloser. Never wrap it,
			// or WebSocket/h2c upgrades break. There is no later hook for an
			// upgraded connection's true close, so finish (and the accounting
			// dispatch it closes out) fires now, at handshake time: duration and
			// in-flight for an upgraded request only ever cover the handshake,
			// never the live session.
			if resp.StatusCode == http.StatusSwitchingProtocols {
				ob.finish(nil)
				return nil
			}
			ob.firstByte = time.Now()
			ob.response = newResponseObserver(ob.path, resp.StatusCode, resp.Header, ob.started)
			if ob.response != nil {
				ob.response.reading, ob.response.live = ob.reading, ob.live
			}
			// A PD reply reports the prefill instance's cache figure, to the
			// client and to the observer alike (cached_rewrite.go).
			if ob.response != nil && ob.pdCachedKnown && len(ob.response.shape.Cached) > 0 {
				cached := ob.response.shape.Cached
				resp.Body = newCachedRewriter(resp.Body, cached[len(cached)-1], ob.pdCached)
			}
			if ob.capture != nil {
				ob.capture.event(map[string]any{"t": "response", "status": resp.StatusCode, "headers": capturedHeaders(resp.Header)})
				resp.Body = &captureBody{ReadCloser: resp.Body, capture: ob.capture}
			}
			counted := &countingBody{inner: resp.Body, obs: ob}
			resp.Body = counted
			// The observer reads the reply as the engine sent it (but for the
			// PD cache figure above); the client gets it without the usage event the gateway's own request for
			// usage added (usage_strip.go).
			if ob.injected && ob.response != nil && ob.response.stream {
				resp.Body = newUsageStripper(counted)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			id := requestID(r)
			p.log.Warn("proxy upstream error", "request_id", id, "method", r.Method, "path", r.URL.Path, "err", err)
			if w.Header().Get("X-Request-ID") == "" {
				w.Header().Set("X-Request-ID", id)
			}
			// A detached data lane that did not reattach within the hub's wait
			// budget is a distinct, retryable condition, not a dead upstream: the
			// node is online and the tunnel will likely open on the very next
			// attempt once the agent's lane reconnects. Report it as 503 with a
			// short Retry-After instead of the generic 502; the retry transport
			// (which already treats every tunnelOpenError as retryable) still gets
			// first chance to switch to another instance when one exists.
			status := http.StatusBadGateway
			if errors.Is(err, hubpkg.ErrDataLaneUnavailable) || errors.Is(err, errInstanceUnavailable) {
				status = http.StatusServiceUnavailable
				w.Header().Set("Retry-After", strconv.Itoa(dataLaneUnavailableRetryAfterSeconds))
			}
			// RoundTrip failed before any response arrived (e.g. the tunnel
			// dial itself failed), so ModifyResponse never ran and the normal
			// countingBody-close accounting hook never fires. Close out the
			// dispatch here instead, classified as the status the client is about
			// to receive.
			if ob, ok := r.Context().Value(obsKey).(*copyObservation); ok && ob != nil {
				ob.status = status
				ob.upstreamFailed = true
				ob.clientGone = r.Context().Err() != nil || errors.Is(err, context.Canceled)
				ob.finish(err)
			}
			writeErr(w, status, "upstream error: "+err.Error())
		},
	}
	p.bufferPool = newBufferPool()
	p.rp.BufferPool = p.bufferPool
	return p
}

// Handler returns the /v1 handler with API-key auth.
func (p *Proxy) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, identity, ok := p.authenticate(w, r)
		if !ok {
			return
		}
		r = r.WithContext(withKeyScope(uxrecord.WithCaller(r.Context(), caller), identity))
		r.Header.Del(instanceHeader) // pinning is an internal-entry feature
		p.serveAuthenticated(w, r)
	}
}

// AuthenticatedHandler serves a request whose caller has already passed
// management authentication. It is only intended for a JWT-protected internal
// route such as the playground; public /v1 must continue to use Handler.
func (p *Proxy) AuthenticatedHandler() http.HandlerFunc {
	return p.serveAuthenticated
}

// PlaygroundHandler routes deployed models through the normal internal data
// plane and namespaced external models through encrypted AI endpoint configs.
// Callers must mount it below management JWT authentication.
func (p *Proxy) PlaygroundHandler(box *auth.Secretbox) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The management API hands over the signed-in user's claims; the
		// request record attributes the request to them.
		caller := uxrecord.Caller{Source: uxrecord.SourcePlayground, ReceivedMS: time.Now().UnixMilli()}
		if claims, ok := auth.ClaimsFrom(r.Context()); ok {
			caller.UserID = claims.UserID
		}
		r = r.WithContext(uxrecord.WithCaller(r.Context(), caller))
		modelName, cleanup, err := extractModel(r, p.maxBodyBytes)
		defer cleanup()
		if err != nil {
			writeErr(w, statusForExtractError(err), err.Error())
			return
		}
		if strings.HasPrefix(modelName, "external:") {
			p.serveExternal(w, r, modelName, box)
			return
		}
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api/v1/playground")
		// A pinned refusal for sleep is the same event the load balancing path
		// accounts as asleep_503, not a generic 5xx.
		deployment, tgt, pinned, status, message, asleep := p.pinnedTarget(r, modelName)
		if !pinned {
			affinity := p.affinityKey(modelName, r)
			deployment, tgt, status, message, asleep = p.selectForRequest(r.Context(), modelName, affinity)
			if status != 0 && asleep {
				deployment, tgt, status, message, asleep = p.holdForWake(r.Context(), w, modelName, affinity, deployment)
			}
		}
		if status != 0 {
			p.acct.RecordImmediate(deploymentIDOrZero(deployment), status, asleep)
			writeErr(w, status, message)
			return
		}
		contract := inferenceapi.ResolveDeploymentContract(p.store, deployment)
		if !inferenceapi.AllowsContractRequest(contract, r.Method, r.URL.Path) {
			p.acct.RecordImmediate(deployment.ID, http.StatusNotFound, false)
			writeErr(w, http.StatusNotFound, fmt.Sprintf("deployment %s does not expose %s %s", modelName, r.Method, r.URL.Path))
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
		writeErr(w, http.StatusNotFound, "external playground models only support chat completions")
		return
	}
	parts := strings.SplitN(selected, ":", 3)
	if len(parts) != 3 {
		writeErr(w, http.StatusBadRequest, "invalid external playground model")
		return
	}
	id, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid external playground model")
		return
	}
	configs, err := p.store.ListAIConfigs()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
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
		writeErr(w, http.StatusNotFound, "external playground model no longer exists")
		return
	}
	key, err := store.DecryptAIKey(box, config.APIKey)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "decrypt external endpoint key: "+err.Error())
		return
	}
	base, err := url.Parse(strings.TrimRight(config.BaseURL, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		writeErr(w, http.StatusBadRequest, "external endpoint has an invalid base URL")
		return
	}
	var body map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "external playground requests must be JSON: "+err.Error())
		return
	}
	body["model"] = config.Model
	encoded, err := json.Marshal(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
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
			out.Header.Set("Authorization", "Bearer "+key)
			out.Header.Set("Content-Type", "application/json")
		},
		FlushInterval:  -1,
		ModifyResponse: boundUpstreamError,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			p.log.Warn("external playground upstream error", "err", err)
			writeErr(w, http.StatusBadGateway, "external upstream error: "+err.Error())
		},
	}
	rp.ServeHTTP(w, r)
}

func (p *Proxy) serveAuthenticated(w http.ResponseWriter, r *http.Request) {
	if route, ok := inferenceapi.AuxiliaryRouteFor(r.URL.Path); ok {
		p.serveAuxiliary(w, r, route)
		return
	}
	modelName, cleanup, err := extractModel(r, p.maxBodyBytes)
	defer cleanup()
	if err != nil {
		writeErr(w, statusForExtractError(err), err.Error())
		return
	}
	if modelName == "" {
		writeErr(w, http.StatusBadRequest, "request body must specify a model")
		return
	}

	p.serveModel(w, r, modelName)
}

func (p *Proxy) serveModel(w http.ResponseWriter, r *http.Request, modelName string) {
	affinity := p.affinityKey(modelName, r)
	dep, tgt, status, msg, asleep := p.selectForRequest(r.Context(), modelName, affinity)
	if status != 0 && asleep {
		dep, tgt, status, msg, asleep = p.holdForWake(r.Context(), w, modelName, affinity, dep)
	}
	if status != 0 {
		p.acct.RecordImmediate(deploymentIDOrZero(dep), status, asleep)
		writeErr(w, status, msg)
		return
	}
	p.serveResolved(w, r, modelName, affinity, dep, tgt)
}

// serveSelectedTarget dispatches to the resolved target, rewriting the
// request body's model field first when the deployment serves under an
// internal name different from the one the client asked for. A rewrite
// failure never reaches Dispatch, so it is accounted as an immediate
// completion for the deployment it was resolved to.
func (p *Proxy) serveSelectedTarget(w http.ResponseWriter, r *http.Request, publicName string, tgt target, deploymentID uint) {
	r, _ = p.identify(r, publicName)
	fail := func(err error) {
		p.releaseTarget(tgt)
		status := statusForExtractError(err)
		p.acctFor(r.Context()).RecordImmediate(deploymentID, status, false)
		writeErr(w, status, err.Error())
	}
	if p.rejectChatInput(w, r, tgt, deploymentID) {
		return
	}
	if tgt.modelName != "" && tgt.modelName != publicName {
		cleanup, err := rewriteModel(r, tgt.modelName, p.maxBodyBytes)
		defer cleanup()
		if err != nil {
			fail(err)
			return
		}
	}
	// Only the gateway may hand KV between instances (ADR 0028).
	cleanup, err := stripClientKVTransfer(r, p.maxBodyBytes)
	defer cleanup()
	if err != nil {
		fail(err)
		return
	}
	proceed, cleanupPD := p.servePrefillStage(w, r, &tgt, deploymentID)
	defer cleanupPD()
	if !proceed {
		if tgt.live != nil {
			p.live.remove(tgt.live)
		}
		return
	}
	p.serveTarget(w, r, tgt, deploymentID)
}

// authenticate checks the platform API key and names the caller: the key
// and its owner, received now. A request without a key is refused unless an
// administrator turned open access on (open_access.go), in which case the
// caller is anonymous.
func (p *Proxy) authenticate(w http.ResponseWriter, r *http.Request) (uxrecord.Caller, store.APIKeyIdentity, bool) {
	caller, identity, ok := p.identifyKey(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "invalid or missing API key")
	}
	return caller, identity, ok
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": msg, "type": "gpuworks_proxy"},
	})
}
