package conn

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/proto"
)

// connectionFailure keeps internal cause identity for retry/exit decisions,
// while Error is always a controlled safe class suitable for every sink.
type connectionFailure struct {
	reason         string
	cause          error
	stage, attempt string
}

func (e *connectionFailure) Error() string { return "connection check: " + e.reason }
func (e *connectionFailure) Unwrap() error { return e.cause }

func connectionReason(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var prior *connectionFailure
	if errors.As(err, &prior) {
		return prior.reason
	}
	var rejected *RejectError
	if errors.As(err, &rejected) {
		switch rejected.Reason {
		case "bad_token", "proto_mismatch", "stale_session":
			return rejected.Reason
		}
		return "transport"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	var cert *tls.CertificateVerificationError
	var invalid x509.CertificateInvalidError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	if errors.As(err, &cert) || errors.As(err, &invalid) || errors.As(err, &authority) || errors.As(err, &hostname) {
		return "tls"
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return "connect"
	}
	return "transport"
}
func connectionErrorState(err error) string {
	switch connectionReason(err) {
	case "timeout":
		return "timeout"
	case "cancelled":
		return "cancelled"
	}
	return "failed"
}
func safeConnectionError(err error) error {
	if err == nil {
		return nil
	}
	var safe *connectionFailure
	if errors.As(err, &safe) {
		return err
	}
	return &connectionFailure{reason: connectionReason(err), cause: err}
}

func (c *Conn) finishConnection(ctx context.Context, err error) error {
	c.initConnectionObservation()
	if c.creds.Smoke && err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	reason := connectionReason(err)
	if err == nil && ctx.Err() != nil {
		reason = connectionReason(ctx.Err())
	}
	o := &c.observation
	o.mu.Lock()
	if err != nil || ctx.Err() != nil {
		o.facts["reason"] = reason
		o.facts["control"] = "not_connected"
		o.facts["outcome"] = "failed"
		if err == nil {
			o.facts["outcome"] = "unknown"
		}
		stage := o.active
		if stage == "" {
			stage = "dial"
		}
		p := o.phases[stage]
		p.state = connectionErrorState(err)
		if err == nil {
			p.state = connectionErrorState(ctx.Err())
		}
		p.observed = time.Now()
		if !p.started.IsZero() {
			p.elapsed = p.observed.Sub(p.started)
		}
		o.phases[stage] = p
	}
	accepted := o.facts["connect_mode"] == agentconf.ServerDial && err != nil
	o.mu.Unlock()
	if accepted {
		c.emitConnectionEvent(c.connectionLane())
	}
	return safeConnectionError(err)
}

// DiagnoseConnectivity performs one bounded real Smoke handshake. It does not
// start an Agent, discover GPUs, register a normal session or open a data lane.
func DiagnoseConnectivity(ctx context.Context, creds Credentials, log *slog.Logger) (proto.DoctorReport, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	creds.Smoke = true
	if creds.ConnectMode == "" {
		creds.ConnectMode = agentconf.AgentDial
	}
	c := New(creds, nil, nil, nil, log)
	c.observation.mu.Lock()
	c.observation.strictProbe = true
	c.observation.mu.Unlock()
	c.beginConnectionPhase("configuration", true)
	conf := agentconf.Config{Server: creds.ServerURL, Token: creds.NodeToken, ConnectMode: creds.ConnectMode}
	if err := conf.Validate(); err != nil {
		safe := &connectionFailure{reason: "invalid_configuration", cause: errors.New("invalid connection configuration")}
		c.endConnectionPhase("configuration", safe)
		c.observationFact("outcome", "invalid_configuration")
		c.observationFact("reason", "invalid_configuration")
		c.observationFact("control", "not_connected")
		return c.ConnectionReport(), safe
	}
	c.endConnectionPhase("configuration", nil)
	if conf.Mode() == agentconf.ServerDial {
		for _, stage := range connectionStages {
			if stage != "configuration" {
				c.observationPhaseState(stage, "not_applicable")
			}
		}
		c.observationFact("outcome", "not_applicable")
		c.observationFact("control", "not_applicable")
		c.observationFact("data_lane", "not_applicable")
		return c.ConnectionReport(), nil
	}
	c.observationFact("control", "probing")
	c.observationFact("outcome", "probing")
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := c.Run(ctx)
	return c.ConnectionReport(), err
}

func safeRejectReason(raw string) string {
	switch strings.TrimSpace(raw) {
	case "bad_token", "proto_mismatch", "stale_session":
		return strings.TrimSpace(raw)
	}
	return "transport"
}

func validateConnectionCredentials(creds Credentials) error {
	conf := agentconf.Config{Server: creds.ServerURL, Token: creds.NodeToken, ConnectMode: creds.ConnectMode}
	if conf.Validate() != nil {
		return &connectionFailure{reason: "invalid_configuration", cause: errors.New("invalid connection configuration")}
	}
	return nil
}
