package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/proxy"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// trackingListener remembers accepted connections so shutdown can force-close
// them. WebSocket connections are hijacked, and neither http.Server.Shutdown
// nor Close closes hijacked connections — without this an agent's socket would
// survive a server stop, and in-process tests could never exercise the
// offline → reconnect path.
type trackingListener struct {
	net.Listener
	// functions are what this socket answers (serverconfig.Function*).
	functions []string
	mu        sync.Mutex
	conns     map[net.Conn]struct{}
}

func newTrackingListener(inner net.Listener, functions []string) *trackingListener {
	return &trackingListener{Listener: inner, functions: functions, conns: map[net.Conn]struct{}{}}
}

// listenAll opens every binding, or none: a server that could bind only some
// of its addresses would serve a subset of its functions without saying so.
func listenAll(bindings []serverconfig.Binding) ([]*trackingListener, error) {
	var lns []*trackingListener
	for _, b := range bindings {
		raw, err := net.Listen("tcp", b.Addr)
		if err != nil {
			closeAll(lns)
			return nil, fmt.Errorf("listen on %s: %w", b.Addr, err)
		}
		lns = append(lns, newTrackingListener(raw, b.Functions))
	}
	return lns, nil
}

func closeAll(lns []*trackingListener) {
	for _, ln := range lns {
		ln.closeConns()
		_ = ln.Close()
	}
}

func closeConns(lns []*trackingListener) {
	for _, ln := range lns {
		ln.closeConns()
	}
}

// exposeOrigin is the local address a public tunnel forwards to. Loopback
// serves every function, so it is preferred over the first address.
func exposeOrigin(lns []*trackingListener) string {
	for _, ln := range lns {
		if tcp, ok := ln.Addr().(*net.TCPAddr); ok && serverconfig.ServesEverything(tcp.IP) {
			return ln.Addr().String()
		}
	}
	return lns[0].Addr().String()
}

type listenerFunctionsKey struct{}

// connFunctions tags each connection with the functions of the socket that
// accepted it; routeByFunction reads the tag.
func connFunctions(ctx context.Context, c net.Conn) context.Context {
	if secure, ok := c.(*tls.Conn); ok {
		c = secure.NetConn()
	}
	if tc, ok := c.(*trackedConn); ok {
		fns := tc.l.functions
		if tcp, ok := c.LocalAddr().(*net.TCPAddr); ok && serverconfig.ServesEverything(tcp.IP) {
			// A wildcard socket also accepts loopback, which serves everything.
			fns = serverconfig.Functions
		}
		return context.WithValue(ctx, listenerFunctionsKey{}, fns)
	}
	return ctx
}

// routeByFunction answers 404 for a path whose function this socket does not
// serve, as if the route did not exist.
func (a *App) routeByFunction(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fns, ok := r.Context().Value(listenerFunctionsKey{}).([]string)
		fn := serverconfig.FunctionOf(r.URL.Path)
		if ok && fn != "" && !slices.Contains(fns, fn) {
			notServedHere(w, r, fn)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// notServedHere answers 404 for a path whose function this socket does not
// serve, in the shape its function answers errors with: the gateway's for
// /v1, the management API's for /api/, plain text for downloads and pages.
func notServedHere(w http.ResponseWriter, r *http.Request, fn string) {
	switch {
	case fn == serverconfig.FunctionGateway:
		proxy.WriteRouteNotFound(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/"):
		api.WriteError(w, http.StatusNotFound, fmt.Errorf("%s is not served on this address", r.URL.Path))
	default:
		http.NotFound(w, r)
	}
}

func (l *trackingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.conns[c] = struct{}{}
	l.mu.Unlock()
	return &trackedConn{Conn: c, l: l}, nil
}

func (l *trackingListener) closeConns() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for c := range l.conns {
		_ = c.Close()
	}
	l.conns = map[net.Conn]struct{}{}
}

func (l *trackingListener) forget(c net.Conn) {
	l.mu.Lock()
	delete(l.conns, c)
	l.mu.Unlock()
}

// trackedConn drops itself from the tracking set on Close so a long-running
// server does not accumulate an entry per connection ever accepted.
type trackedConn struct {
	net.Conn
	l    *trackingListener
	once sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(func() { c.l.forget(c.Conn) })
	return c.Conn.Close()
}

// requestRestart stops Run shortly after the current response is written, so
// the service manager starts the process again with the saved configuration.
func (a *App) requestRestart() {
	a.restartMu.Lock()
	stop := a.stopForRestart
	a.restartMu.Unlock()
	if stop == nil {
		return
	}
	a.log.Warn("restarting on request from the console")
	time.AfterFunc(500*time.Millisecond, stop)
}

// Addr returns the address currently bound by the server. Before Run it
// returns the configured address; after Run starts it returns the concrete
// address (including the allocated port for "127.0.0.1:0").
func (a *App) Addr() string {
	a.lnMu.RLock()
	defer a.lnMu.RUnlock()
	if len(a.lns) > 0 {
		return a.lns[0].Addr().String()
	}
	return a.cfg.bindings()[0].Addr
}

// SetAddr changes the configured listener address while the app is stopped.
// It is primarily useful to restart an ephemeral test server on the same port
// so an already-running agent can exercise reconnect and adoption.
func (a *App) SetAddr(addr string) error {
	a.lnMu.Lock()
	defer a.lnMu.Unlock()
	if a.lns != nil {
		return errors.New("server app is running")
	}
	if !a.cfg.Listen.Empty() {
		return errors.New("server app binds its listen configuration, not a single address")
	}
	a.cfg.Addr = addr
	return nil
}
