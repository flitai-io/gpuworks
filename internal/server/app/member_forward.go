//go:build linux && dqlite

package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/hub"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// A control-plane cluster serves business from one member: the one that
// leads the database, where the controller already runs. Agent sessions, the
// event bus, metrics and every in-memory observation live in that one
// process, as they do on a single server. The other members stay ready to
// take over and hand each business request they receive to the leader over
// the member route (mutual TLS between admitted members).
//
// memberForward is both ends of that hand-over for one business generation:
// on a member that does not lead it forwards, and on the leader it serves
// what the others forward. Probes stay local, so an entry layer keeps seeing
// each member's own readiness.

const (
	// The leader is observed on this interval; an observation older than
	// memberLeaderFresh, stretched on a tier that elects later
	// (observedLeader), is not acted on.
	memberLeaderObserveInterval = 500 * time.Millisecond
	memberLeaderObserveTimeout  = 2 * time.Second
	memberLeaderFresh           = 3 * time.Second
	// A forwarded request carries the client's address and whether it
	// arrived over TLS, so the leader sees the request as the member did.
	// Both headers exist only on the member route: a member strips them from
	// what a client sends before it serves or forwards the request.
	memberForwardRemoteHeader = "X-Gpuworks-Member-Remote-Addr"
	memberForwardTLSHeader    = "X-Gpuworks-Member-Client-Tls"
	// A relayed agent session names the session identity its agent already
	// holds (member_agent_relay.go).
	memberForwardSessionHeader = "X-Gpuworks-Member-Agent-Session"
	agentConnectPath           = "/api/v1/agent/connect"
)

type memberForward struct {
	log *slog.Logger
	// refusals is the member's record of what it refused; nil counts nothing.
	refusals *businessRefusals
	// leader observes the member that leads the database now.
	leader func(context.Context) (uint64, error)
	// local serves a forwarded request: this generation's business handler
	// behind the member's own admission, set before the route is served.
	// sessions reports how many agent sessions this process holds and drop
	// closes them all.
	local    http.Handler
	sessions func() int
	drop     func()
	// answers is the module that answers a peer's question by name
	// (member_ask.go); nil answers none.
	answers func(name string) MemberAnswerer
	// agents keeps agent connections this member accepted attached to the
	// member that leads; nil serves them like any other request.
	agents *memberAgentRelay

	mu        sync.Mutex
	id        uint64
	routes    map[uint64]string
	serverTLS *tls.Config
	peerTLS   func(context.Context, uint64, string) (*tls.Config, error)
	authorize func(context.Context, *x509.Certificate) error
	proxies   map[uint64]*httputil.ReverseProxy
	server    *http.Server
	stop      context.CancelFunc
	done      chan struct{}

	observed atomic.Pointer[memberLeaderObservation]
}

type memberLeaderObservation struct {
	leader uint64
	at     time.Time
}

func newMemberForward(log *slog.Logger, refusals *businessRefusals, leader func(context.Context) (uint64, error), sessions func() int, drop func()) *memberForward {
	return &memberForward{log: log, refusals: refusals, leader: leader, sessions: sessions, drop: drop, routes: map[uint64]string{}, proxies: map[uint64]*httputil.ReverseProxy{}}
}

// SetMemberIdentity is the route of a store that is not a native cluster. It
// has no certificate-bound transport, so nothing is forwarded on it.
func (f *memberForward) SetMemberIdentity(memberID uint64, _ []byte) {
	f.mu.Lock()
	f.id = memberID
	f.mu.Unlock()
}

func (f *memberForward) SetMemberSecureTransport(memberID uint64, server *tls.Config, peerTLS func(context.Context, uint64, string) (*tls.Config, error), authorize func(context.Context, *x509.Certificate) error) error {
	if memberID == 0 || server == nil || peerTLS == nil || authorize == nil {
		return errors.New("member route requires an identity and its certificate-bound transport")
	}
	server = server.Clone()
	// Upgrades (the agent session, terminals) need a hijackable connection.
	server.NextProtos = []string{"http/1.1"}
	f.mu.Lock()
	f.id, f.serverTLS, f.peerTLS, f.authorize = memberID, server, peerTLS, authorize
	f.mu.Unlock()
	return nil
}

func (f *memberForward) SetMemberRoute(memberID uint64, addr string) {
	if memberID == 0 || addr == "" {
		return
	}
	f.mu.Lock()
	f.routes[memberID] = addr
	delete(f.proxies, memberID)
	f.mu.Unlock()
}

// ServeMemberSessions serves what other members forward until the listener
// closes, and observes the leader meanwhile.
func (f *memberForward) ServeMemberSessions(ln net.Listener) error {
	f.mu.Lock()
	if f.serverTLS == nil || f.server != nil {
		f.mu.Unlock()
		return errors.New("member route is not configured, or is already served")
	}
	ctx, stop := context.WithCancel(context.Background())
	server := &http.Server{
		Handler:           http.HandlerFunc(f.serveForwarded),
		ReadHeaderTimeout: 15 * time.Second,
		ConnContext: func(ctx context.Context, _ net.Conn) context.Context {
			return context.WithValue(ctx, memberPeerKey{}, &memberPeer{})
		},
		// HTTP/2 cannot hand a connection over to an upgrade.
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
	}
	f.server, f.stop, f.done = server, stop, make(chan struct{})
	config, done := f.serverTLS, f.done
	f.mu.Unlock()
	go func() {
		defer close(done)
		f.observeLeader(ctx)
	}()
	err := server.Serve(tls.NewListener(ln, config))
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// CloseMemberSessions stops serving forwarded requests. Requests in flight
// end with their generation's context; an upgraded connection is closed.
func (f *memberForward) CloseMemberSessions(ctx context.Context) error {
	f.mu.Lock()
	server, stop, done := f.server, f.stop, f.done
	f.server, f.stop, f.done = nil, nil, nil
	proxies := f.proxies
	f.proxies = map[uint64]*httputil.ReverseProxy{}
	f.mu.Unlock()
	if f.agents != nil {
		f.agents.relay.Close()
	}
	if server == nil {
		return nil
	}
	stop()
	<-done
	for _, proxy := range proxies {
		if transport, ok := proxy.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
	return server.Close()
}

type memberPeerKey struct{}

// memberPeer is one member connection's authorization: decided once, by the
// first answer. An attempt that brought no answer decides nothing.
type memberPeer struct {
	mu      sync.Mutex
	decided bool
	err     error
}

func (p *memberPeer) authorize(decide func() error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.decided {
		return p.err
	}
	err := decide()
	if err != nil && store.GrantUndecided.Unanswered(err) {
		return err
	}
	p.decided, p.err = true, err
	return err
}

// serveForwarded is the leader's end: an admitted member's request is served
// as if the client had connected here.
func (f *memberForward) serveForwarded(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		f.refusals.refuse(w, r, refusalPeerUnidentified)
		return
	}
	peer, _ := r.Context().Value(memberPeerKey{}).(*memberPeer)
	f.mu.Lock()
	authorize, local := f.authorize, f.local
	f.mu.Unlock()
	if peer == nil || authorize == nil || local == nil {
		f.refusals.refuse(w, r, refusalForwardNotReady)
		return
	}
	if err := peer.authorize(func() error { return authorize(r.Context(), r.TLS.PeerCertificates[0]) }); err != nil {
		if store.GrantUndecided.Unanswered(err) {
			// No answer about the peer: not ready, and the next request on
			// this connection asks again.
			f.refusals.refuse(w, r, refusalPeerUndecided)
			return
		}
		f.refusals.mark(w, r, refusalPeerNotAdmitted)
		http.Error(w, "member is not admitted", http.StatusForbidden)
		return
	}
	// A question to a module is answered here, by whichever member it
	// reaches (member_ask.go).
	if strings.HasPrefix(r.URL.Path, memberAskPrefix) {
		f.serveAsk(w, r)
		return
	}
	// A member forwards to the leader it observed. One that is no longer the
	// leader answers as a closed member does and never forwards again, so a
	// request crosses the member route at most once.
	if leader, known := f.observedLeader(); !known {
		f.refusals.refuse(w, r, refusalNoKnownLeader)
		return
	} else if leader != f.memberID() {
		f.refusals.refuse(w, r, refusalNotTheLeader)
		return
	}
	remote := r.Header.Get(memberForwardRemoteHeader)
	if _, _, err := net.SplitHostPort(remote); err != nil {
		http.Error(w, "forwarded request names no client address", http.StatusBadRequest)
		return
	}
	served := r.Clone(r.Context())
	served.RemoteAddr = remote
	if r.Header.Get(memberForwardTLSHeader) == "" {
		served.TLS = nil
	} else {
		// The client's own TLS state stays with the member that accepted it;
		// the leader only needs to know the request arrived over TLS.
		served.TLS = &tls.ConnectionState{HandshakeComplete: true}
	}
	if id := r.Header.Get(memberForwardSessionHeader); id != "" {
		served = served.WithContext(hub.WithRelayedSessionID(served.Context(), id))
	}
	served.Header.Del(memberForwardRemoteHeader)
	served.Header.Del(memberForwardTLSHeader)
	served.Header.Del(memberForwardSessionHeader)
	local.ServeHTTP(w, served)
}

// wrap is the public end. next is the generation's business handler: the
// leader serves it, any other member forwards the request to the leader.
func (f *memberForward) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only a member may say who the client is.
		r.Header.Del(memberForwardRemoteHeader)
		r.Header.Del(memberForwardTLSHeader)
		r.Header.Del(memberForwardSessionHeader)
		if memberProbe(r) {
			next.ServeHTTP(w, r)
			return
		}
		if f.agents != nil && f.routed() && agentControlConnect(r) {
			// The agent's connection stays with this member whoever leads.
			f.agents.serve(w, r)
			return
		}
		proxy, forward := f.forwardTarget()
		if !forward {
			next.ServeHTTP(w, r)
			return
		}
		if proxy == nil {
			f.refusals.refuse(w, r, refusalNoKnownLeader)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

// memberProbe reports a request that asks about this member itself.
func memberProbe(r *http.Request) bool {
	return r.Method == http.MethodGet && (r.URL.Path == "/healthz" || r.URL.Path == "/api/v1/healthz" || r.URL.Path == "/api/v1/readyz")
}

// forwardTarget decides one request. forward is false when this member
// serves it: it leads, or the route is not in use. A nil proxy with forward
// set means no leader is known now, and the request is refused as a closed
// member refuses it, for the entry layer to retry.
func (f *memberForward) forwardTarget() (proxy *httputil.ReverseProxy, forward bool) {
	f.mu.Lock()
	id, serving, peerTLS := f.id, f.server != nil, f.peerTLS
	f.mu.Unlock()
	if id == 0 || !serving || peerTLS == nil {
		return nil, false
	}
	leader, known := f.observedLeader()
	if !known {
		return nil, true
	}
	if leader == id {
		return nil, false
	}
	return f.proxyTo(leader), true
}

// routed reports that this member takes part in the member route.
func (f *memberForward) routed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.id != 0 && f.server != nil && f.peerTLS != nil
}

func (f *memberForward) memberID() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.id
}

// observedLeader is the leader this member last observed, while the
// observation is fresh. A leader that fails is replaced after one to two
// election timeouts and a vote; at slow-storage's timing that is inside
// memberLeaderFresh, and a tier that elects later keeps the observation
// longer in proportion, so that an ordinary election does not by itself make
// every member refuse business.
func (f *memberForward) observedLeader() (uint64, bool) {
	observation := f.observed.Load()
	fresh := cluster.ProcessStorageTier().Stretch(memberLeaderFresh)
	if observation == nil || observation.leader == 0 || time.Since(observation.at) > fresh {
		return 0, false
	}
	return observation.leader, true
}

// observeLeader keeps the leader observation current. A member that sees
// another member lead closes the agent sessions its hub still holds: the
// relays behind them (member_agent_relay.go) attach to the leader instead,
// and an agent the leader dialed itself is dialed again by the new leader.
func (f *memberForward) observeLeader(ctx context.Context) {
	ticker := time.NewTicker(memberLeaderObserveInterval)
	defer ticker.Stop()
	for {
		f.observeLeaderOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (f *memberForward) observeLeaderOnce(ctx context.Context) {
	if f.leader == nil {
		return
	}
	attempt, cancel := context.WithTimeout(ctx, memberLeaderObserveTimeout)
	leader, err := f.leader(attempt)
	cancel()
	if err != nil || leader == 0 {
		// No answer says nothing about who leads; the last observation
		// simply ages out.
		return
	}
	now := time.Now()
	previous := f.observed.Swap(&memberLeaderObservation{leader: leader, at: now})
	id := f.memberID()
	closed := 0
	if leader != id && f.sessions != nil && f.drop != nil {
		if closed = f.sessions(); closed > 0 {
			f.drop()
		}
	}
	if f.log == nil || (previous != nil && previous.leader == leader) {
		return
	}
	// One line per change of the leader this member sends business to. The
	// reason is in the database process' own records of the same moment
	// ("cluster database role event", "cluster database leader changed").
	attrs := []any{"new_leader", leader, "this_member", id, "leads", leader == id, "learned", "leader observation", "agent_sessions_closed", closed}
	if previous != nil {
		attrs = append(attrs, "old_leader", previous.leader, "since_last_observation_ms", now.Sub(previous.at).Milliseconds())
	}
	f.log.Info("business follows a new database leader", attrs...)
	if closed > 0 {
		f.log.Info("closed agent sessions held by a member that does not lead", "sessions", closed, "leader", leader)
	}
}

// proxyTo is the reverse proxy to one member's route, or nil when that
// member has no route.
func (f *memberForward) proxyTo(member uint64) *httputil.ReverseProxy {
	f.mu.Lock()
	defer f.mu.Unlock()
	if proxy := f.proxies[member]; proxy != nil {
		return proxy
	}
	address, peerTLS := f.routes[member], f.peerTLS
	if address == "" || peerTLS == nil {
		return nil
	}
	target := &url.URL{Scheme: "https", Host: address}
	transport := &http.Transport{
		DialTLSContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			config, err := peerTLS(ctx, member, address)
			if err != nil {
				return nil, err
			}
			config = config.Clone()
			config.NextProtos = []string{"http/1.1"}
			return (&tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: config}).DialContext(ctx, network, address)
		},
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       30 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		// Event streams and long downloads reach the client as they arrive.
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// The leader must see the request the client sent: its Host
			// decides the origin check and the links the server writes.
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Set(memberForwardRemoteHeader, pr.In.RemoteAddr)
			if pr.In.TLS != nil {
				pr.Out.Header.Set(memberForwardTLSHeader, "1")
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if f.log != nil && r.Context().Err() == nil {
				f.log.Debug("request not forwarded to the leader", "member", member, "err", err)
			}
			if r.Context().Err() != nil {
				// The client went away; that says nothing about the leader.
				answerNotReady(w)
				return
			}
			f.refusals.refuse(w, r, refusalLeaderUnreachable)
		},
		// The leader's answer, unless it is itself a refusal, ends a run of
		// refusals on this member.
		ModifyResponse: func(resp *http.Response) error {
			if resp.Header.Get(businessRefusalHeader) == "" {
				f.refusals.served()
			}
			return nil
		},
	}
	f.proxies[member] = proxy
	return proxy
}
