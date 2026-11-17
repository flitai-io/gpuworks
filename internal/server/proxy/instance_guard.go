package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

var errInstanceUnavailable = errors.New("instance stopped or is restarting; retry when a replica is ready")

// instanceGuard watches only instances with active upstream attempts. The
// route table's existing instance subscription supplies changes; it does not
// add a subscription or timer for each request.
type instanceGuard struct {
	store *store.Store
	mu    sync.Mutex
	calls map[string]map[*guardedCall]struct{}
}

type guardedCall struct {
	port   int
	cancel context.CancelCauseFunc
}

func (g *instanceGuard) add(uuid string, call *guardedCall) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.calls == nil {
		g.calls = map[string]map[*guardedCall]struct{}{}
	}
	if g.calls[uuid] == nil {
		g.calls[uuid] = map[*guardedCall]struct{}{}
	}
	g.calls[uuid][call] = struct{}{}
}

func (g *instanceGuard) remove(uuid string, call *guardedCall) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.calls[uuid], call)
	if len(g.calls[uuid]) == 0 {
		delete(g.calls, uuid)
	}
}

// check cancels only definitive loss of the generation serving an attempt.
// Draining, sleeping, node availability and database errors do not prove an
// existing request cannot finish and must not interrupt it.
func (g *instanceGuard) check(uuid string) {
	g.mu.Lock()
	calls := make([]*guardedCall, 0, len(g.calls[uuid]))
	for call := range g.calls[uuid] {
		calls = append(calls, call)
	}
	g.mu.Unlock()
	if len(calls) == 0 {
		return
	}
	in, err := g.store.InstanceByUUID(uuid)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return
	}
	for _, call := range calls {
		unavailable := errors.Is(err, store.ErrNotFound)
		if err == nil {
			switch in.State {
			case proto.InstFailed, proto.InstStopped, proto.InstPendingRemoval, proto.InstRemoving,
				proto.InstPending, proto.InstPulling, proto.InstStarting, proto.InstOptimizing:
				unavailable = true
			}
			unavailable = unavailable || in.Headless || in.APIPort != call.port
		}
		if unavailable {
			call.cancel(errInstanceUnavailable)
		}
	}
}

// checkMissing covers a dropped status event on the bounded subscription.
// Absence from routing alone proves nothing: check re-reads the instance and
// preserves healthy draining, sleep and rollout requests.
func (g *instanceGuard) checkMissing(active map[string]bool) {
	g.mu.Lock()
	var missing []string
	for uuid := range g.calls {
		if !active[uuid] {
			missing = append(missing, uuid)
		}
	}
	g.mu.Unlock()
	for _, uuid := range missing {
		g.check(uuid)
	}
}

// roundTrip keeps the registration until the body is closed, covering both
// waiting for headers and reading a streamed reply. Cancellation is local to
// this attempt: it is an upstream failure, not a client disconnect.
func (g *instanceGuard) roundTrip(base http.RoundTripper, req *http.Request, candidate candidate) (*http.Response, error) {
	if g == nil || g.store == nil || candidate.instanceUUID == "" || req.Header.Get("Upgrade") != "" {
		return base.RoundTrip(req)
	}
	ctx, cancel := context.WithCancelCause(req.Context())
	call := &guardedCall{port: candidate.port, cancel: cancel}
	g.add(candidate.instanceUUID, call)
	var once sync.Once
	release := func() { once.Do(func() { g.remove(candidate.instanceUUID, call); cancel(nil) }) }
	// Close registration versus state-event race: a change announced before
	// the call was registered is checked here, one announced after reaches
	// the registered call through the route table's subscription. With no
	// change at all since the candidate was read, there is nothing to check.
	if !instanceRowsUnchanged(g.store, candidate.routedAt) {
		g.check(candidate.instanceUUID)
	}
	resp, err := base.RoundTrip(req.Clone(ctx))
	if err != nil {
		if errors.Is(context.Cause(ctx), errInstanceUnavailable) {
			err = errInstanceUnavailable
		}
		release()
		return nil, err
	}
	if resp.StatusCode == http.StatusSwitchingProtocols {
		release() // Upgraded connections retain their raw ReadWriteCloser.
		return resp, nil
	}
	resp.Body = &guardedBody{ReadCloser: resp.Body, ctx: ctx, release: release}
	return resp, nil
}

type guardedBody struct {
	io.ReadCloser
	ctx     context.Context
	release func()
}

func (b *guardedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && errors.Is(context.Cause(b.ctx), errInstanceUnavailable) {
		err = errInstanceUnavailable
	}
	return n, err
}
func (b *guardedBody) Close() error {
	defer b.release()
	return b.ReadCloser.Close()
}
