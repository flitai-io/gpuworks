package expose

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"gpuworks.ai/gpuworks/internal/logging"
)

func (s *Supervisor) launchAttempt(ctx context.Context, cfg *Config, provider Provider, origin string) {
	s.mu.Lock()
	s.starting = true
	s.revision = cfg.Revision
	s.providerID = cfg.Provider
	s.provider = provider
	s.state = StateStarting
	s.message = "preparing the connector"
	s.lastErr = ""
	s.mu.Unlock()
	s.writeStatus()

	// The attempt's context is cancelled by whatever supersedes it — a new
	// revision, disabled, shutdown — so a slow registration or an open probe
	// window never keeps a stale attempt (and the single worker slot) alive.
	attemptCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancelAttempt = cancel
	s.mu.Unlock()
	go func() {
		defer cancel()
		err := s.attempt(attemptCtx, cfg, provider, origin)
		s.mu.Lock()
		s.starting = false
		s.cancelAttempt = nil
		s.mu.Unlock()
		if err != nil && !errors.Is(err, errSuperseded) {
			s.recordFailure(err)
		}
	}()
}

// attempt acquires, launches and verifies one connector, walking the automatic
// network ladder when the edge connection fails. The operator is never asked
// about IP version or protocol.

func (s *Supervisor) attempt(ctx context.Context, cfg *Config, provider Provider, origin string) error {
	tried := map[Network]bool{}
	net := Network{}
	var lastErr error
	for step := 0; step < 3; step++ {
		if step > 0 {
			s.note("retrying with %s", describeNetwork(net))
		}
		tried[net] = true
		err := s.attemptWith(ctx, cfg, provider, origin, net)
		if err == nil {
			return nil
		}
		if errors.Is(err, errSuperseded) {
			// Someone else decided: there is nothing to retry.
			return err
		}
		lastErr = err
		if ctx.Err() != nil {
			// Cancelled from outside — a newer revision, a disable, shutdown.
			// That is not this connector's failure, so it must not spend the
			// failure budget (and "context canceled" must never be the reason
			// an operator is shown).
			return errSuperseded
		}
		next, ok := nextNetwork(net, err)
		if !ok || tried[next] {
			// Not every failure deserves a different edge setting — a
			// registration call that cannot reach the network is reported for
			// what it is instead of being retried with a knob that cannot help.
			break
		}
		net = next
	}
	return lastErr
}

// errSuperseded ends an attempt that a configuration change made pointless. It
// is not a failure: nothing is wrong with the connector, the operator simply
// asked for something else.
var errSuperseded = errors.New("the configuration changed while this attempt was running")

// superseded reports whether this attempt is still the current one. Every write
// an attempt makes goes through here: the plan requires that nothing an old
// revision observed can land on top of a newer one.
func (s *Supervisor) superseded(revision string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision != revision
}

func (s *Supervisor) attemptWith(ctx context.Context, cfg *Config, provider Provider, origin string, net Network) error {
	env := &ProviderEnv{Store: s.store, Origin: origin, Deps: s.deps, Logf: s.note}
	launch, err := provider.Launch(ctx, env, cfg, net)
	if err != nil {
		return err
	}
	output, err := s.newOutput()
	if err != nil {
		return err
	}
	child, err := StartChild(s.deps.Helper, Spec{Binary: launch.Binary, Args: launch.Args, Env: launch.Env}, output)
	if err != nil {
		return err
	}
	if s.superseded(cfg.Revision) {
		_ = child.Stop(stopGrace)
		return errSuperseded
	}
	s.mu.Lock()
	s.child = child
	s.output = output
	s.binary = launch.Binary
	s.version = launch.Version
	s.url = ""
	s.candidates = nil
	s.mu.Unlock()
	s.writeStatus()

	stream, err := s.waitRegistration(ctx, output)
	if err != nil {
		s.stopFailedChild("the attempt failed")
		return err
	}
	if s.superseded(cfg.Revision) {
		s.stopCurrent("the configuration changed")
		return errSuperseded
	}
	s.mu.Lock()
	s.edge, s.protocol = stream.Registration.Edge, stream.Registration.Protocol
	s.mu.Unlock()

	ready, err := provider.Ready(ctx, env, cfg, stream)
	if err != nil {
		s.stopFailedChild("the attempt failed")
		return err
	}
	if ready.State == StateAwaitingRoute {
		// The connector is up and will pick up the route the operator is about
		// to publish; that is not a failure and must not count as one.
		s.mu.Lock()
		s.state = StateAwaitingRoute
		s.candidates = ready.Candidates
		s.message = ready.Message
		s.remote = ready.Remote
		s.mu.Unlock()
		s.writeStatus()
		s.log.Info("public entry point is waiting for a route", "provider", cfg.Provider)
		return nil
	}
	// Ready means the address really serves this server: the local origin
	// answers, and so does the public one. A server that is down right now is
	// not the connector's failure — the server restarts independently of the
	// connector service — so the connector stays and the entry point is
	// degraded until the periodic probe sees the server again.
	if err := s.deps.Prober.CheckLocal(ctx, origin); err != nil {
		if ctx.Err() != nil || s.superseded(cfg.Revision) {
			s.stopCurrent("the configuration changed")
			return errSuperseded
		}
		s.mu.Lock()
		s.state = StateDegraded
		s.url = ready.URL
		s.candidates = ready.Candidates
		s.remote = ready.Remote
		s.message = "the connector is up, but the server does not answer at " + origin + ": " + err.Error()
		s.readySince = time.Time{}
		s.mu.Unlock()
		s.writeStatus()
		s.log.Warn("public entry point is up but the server does not answer", "origin", origin, "err", err)
		return nil
	}
	// The connector is up; whether the address already answers is a different
	// question, and a fresh quick-tunnel hostname needs a moment to exist in
	// DNS. Retry it here, and if it still does not answer, keep the connector:
	// the periodic probe promotes it once it does, which is far better than
	// failing and minting yet another address.
	if err := s.probeEventually(ctx, ready.URL); err != nil {
		if ctx.Err() != nil || s.superseded(cfg.Revision) {
			// Cancelled while probing: whoever cancelled it owns the next
			// observation, and a degraded one written here would outlive the
			// decision that ended this attempt (shutdown does not change the
			// revision, so the ctx has to be checked as well).
			s.stopCurrent("the configuration changed")
			return errSuperseded
		}
		s.mu.Lock()
		s.state = StateDegraded
		s.url = ready.URL
		s.candidates = ready.Candidates
		s.remote = ready.Remote
		s.message = "the connector is up but the address does not answer yet: " + err.Error()
		s.readySince = time.Time{}
		s.mu.Unlock()
		s.writeStatus()
		s.log.Warn("public entry point is not reachable yet", "url", ready.URL, "err", err)
		return nil
	}
	if s.superseded(cfg.Revision) {
		s.stopCurrent("the configuration changed")
		return errSuperseded
	}
	now := s.deps.Now()
	s.mu.Lock()
	s.state = StateReady
	s.url = ready.URL
	s.candidates = ready.Candidates
	s.message = ready.Message
	s.remote = ready.Remote
	s.readySince = now
	s.lastProbe = now
	s.mu.Unlock()
	s.writeStatus()
	s.log.Info("public entry point is ready", "provider", cfg.Provider, "url", ready.URL, "connector", launch.Version)
	return nil
}

// probeContext is the supervisor's own lifetime, falling back to the attempt's
// when Run has not stored one yet (tests call the pieces directly).

func (s *Supervisor) probeContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runCtx != nil {
		return s.runCtx
	}
	return context.Background()
}

// probeEventually retries the public probe for the propagation window, so a
// hostname that does not exist yet is not mistaken for a broken setup.

func (s *Supervisor) probeEventually(ctx context.Context, url string) error {
	s.mu.Lock()
	window := s.probeWindow
	s.mu.Unlock()
	deadline := time.Now().Add(window)
	var lastErr error
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := s.deps.Prober.CheckPublic(probeCtx, url)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// waitRegistration waits for the connector to attach, watching the stream and
// the process together: a connector that exits tells us more than a timeout.

func (s *Supervisor) waitRegistration(ctx context.Context, output *ConnectorOutput) (StreamState, error) {
	s.mu.Lock()
	budget := s.startTimeout
	s.mu.Unlock()
	deadline := time.Now().Add(budget)
	for {
		stream := output.Snapshot()
		if stream.Registration != nil {
			return stream, nil
		}
		s.mu.Lock()
		child := s.child
		s.mu.Unlock()
		if child == nil || child.Exited() {
			return stream, fmt.Errorf("the connector exited before it registered%s", troubleSuffix(stream))
		}
		select {
		case <-ctx.Done():
			return stream, errSuperseded
		case <-time.After(200 * time.Millisecond):
		}
		// A connector that is alive but never registers would hold the single
		// worker slot for ever, so the start gets a budget of its own: the
		// failure then lands on the backoff ladder like any other.
		if time.Now().After(deadline) {
			return stream, fmt.Errorf("the connector did not register within %s%s", budget, troubleSuffix(stream))
		}
	}
}

// newOutput is the rotating log writer and the parser that share the
// connector's output. The server owns both because the server owns the pipe.
//
// The writer is created on first use: a server that never publishes an entry
// point should not leave a connector log (or its directories) behind.
func (s *Supervisor) newOutput() (*ConnectorOutput, error) {
	s.mu.Lock()
	writer := s.logWriter
	s.mu.Unlock()
	if writer == nil {
		if err := s.store.Ensure(); err != nil {
			s.log.Warn("cannot prepare the exposure state directory", "err", err)
		} else if created, err := logging.NewRotatingWriter(s.store.LogPath(), logging.RotationOptions{
			MaxBytes: logging.DefaultRotation().MaxBytes,
			Keep:     logging.DefaultRotation().Keep,
		}); err != nil {
			s.log.Warn("connector output will not be written to a file", "err", err)
		} else {
			s.mu.Lock()
			s.logWriter = created
			s.mu.Unlock()
			writer = created
		}
	}
	// A typed nil (*RotatingWriter)(nil) would satisfy io.Writer and panic on
	// the first write, so only a real writer is handed over.
	var out io.Writer
	if writer != nil {
		out = writer
	}
	return NewConnectorOutput(out), nil
}

// openLogWriter creates the rotating writer for the connector log. A log that
// cannot be opened degrades to parsing only: losing the log must not lose the
// entry point.

func (s *Supervisor) closeLogWriter() {
	s.mu.Lock()
	writer := s.logWriter
	s.logWriter = nil
	s.mu.Unlock()
	if writer != nil {
		_ = writer.Close()
	}
}

// disable takes the entry point down because the configuration says so.

func (s *Supervisor) probeAsync(ctx context.Context) {
	s.mu.Lock()
	if s.probing {
		s.mu.Unlock()
		return
	}
	s.probing = true
	url := s.url
	origin := s.originLocked()
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			s.probing = false
			s.mu.Unlock()
		}()
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		err := s.deps.Prober.CheckPublic(probeCtx, url)
		if err == nil {
			err = s.deps.Prober.CheckLocal(probeCtx, origin)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.lastProbe = s.deps.Now()
		if err == nil {
			if s.state == StateDegraded {
				s.state = StateReady
				s.message = ""
				s.writeStatusLocked()
			}
			if !s.readySince.IsZero() && s.deps.Now().Sub(s.readySince) >= s.stableWindow && s.failures > 0 {
				// It stood up and stayed up: the ladder starts over, and the
				// observation has to say so — a stale count is what the CLI
				// shows the operator.
				s.failures = 0
				s.writeStatusLocked()
			}
			return
		}
		if s.state == StateReady {
			s.state = StateDegraded
			s.message = "the address is not answering right now: " + err.Error()
			s.writeStatusLocked()
		}
	}()
}

func (s *Supervisor) originLocked() string {
	if s.origin != "" {
		return s.origin
	}
	if s.listenAddr == "" {
		return ""
	}
	origin, err := OriginFromListenAddr(s.listenAddr)
	if err != nil {
		return ""
	}
	return origin
}

// writeStatus publishes the current observation.
