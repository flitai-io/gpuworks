// Package recovery restores an offline agent through its independent SSH side
// channel. It is intentionally outside controller: SSH can block for tens of
// seconds and must not serialize deployment reconciliation.
package recovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type Config struct {
	Enabled           bool
	GracePeriod       time.Duration
	InitialInterval   time.Duration
	BackoffMultiplier float64
	MaxInterval       time.Duration
	RestartWait       time.Duration
	MaxConcurrent     int
}

type Hub interface {
	Online(string) bool
	ReconnectServerDial(context.Context, string) error
	ServerDialRecoveryReady(string) bool
	// ResetServerDialBackoff makes the server dial a node again at once, at
	// an address the provider just moved it to.
	ResetServerDialBackoff(string)
}

type Provisioner interface {
	Probe(context.Context, provision.SSHProbeRequest) provision.ProbeResult
	RestartAgent(context.Context, provision.SSHProbeRequest) error
	InstallSSH(context.Context, provision.SSHInstallRequest) (provision.SSHInstallReport, error)
}

type ProviderSyncer interface {
	Sync(context.Context, uint) ([]provider.Instance, error)
	// ServerDialAddress is where the provider maps a server-dialed agent's
	// listen port now; provider.ErrUnsupported for providers that do not
	// map ports.
	ServerDialAddress(ctx context.Context, providerID uint, externalID, listen string) (string, error)
	// InstanceDetail is the provider's description of one instance, which
	// carries the connection data some providers leave out of their listing;
	// provider.ErrUnsupported for providers whose listing is complete.
	InstanceDetail(ctx context.Context, providerID uint, externalID string) (provider.Instance, error)
}

type Outcome struct {
	NodeUUID      string                `json:"node_uuid"`
	Phase         string                `json:"phase,omitempty"`
	Action        string                `json:"action"`
	Probe         provision.ProbeResult `json:"probe"`
	Recovered     bool                  `json:"recovered"`
	SkippedReason string                `json:"skipped_reason,omitempty"`
	Error         string                `json:"error,omitempty"`
}

type Recoverer struct {
	store       *store.Store
	hub         Hub
	providers   ProviderSyncer
	provisioner Provisioner
	log         *slog.Logger
	cfg         Config

	mu       sync.Mutex
	inFlight map[string]bool
	sem      chan struct{}
	now      func() time.Time
	// wg tracks the per-node goroutines runDue spawns, so Run can wait for
	// every in-flight recovery round to finish before it returns instead of
	// leaving them running past ctx cancellation.
	wg sync.WaitGroup
	// syncs shares one provider listing among a round's nodes.
	syncs syncCache
}

func New(st *store.Store, hub Hub, providers ProviderSyncer, provisioner Provisioner, log *slog.Logger, cfg Config) *Recoverer {
	if log == nil {
		log = slog.Default()
	}
	if cfg.InitialInterval <= 0 {
		cfg.InitialInterval = 30 * time.Second
	}
	if cfg.MaxInterval <= 0 {
		cfg.MaxInterval = 10 * time.Minute
	}
	if cfg.BackoffMultiplier < 1 {
		cfg.BackoffMultiplier = 2
	}
	if cfg.RestartWait <= 0 {
		cfg.RestartWait = time.Minute
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 4
	}
	return &Recoverer{
		store: st, hub: hub, providers: providers, provisioner: provisioner,
		log: log, cfg: cfg, inFlight: map[string]bool{}, sem: make(chan struct{}, cfg.MaxConcurrent), now: time.Now,
	}
}

func (r *Recoverer) Run(ctx context.Context) {
	if r == nil || !r.cfg.Enabled {
		return
	}
	r.runDue(ctx)
	ticker := time.NewTicker(r.cfg.InitialInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// runDue's per-node goroutines are in-flight work, not fire-and-
			// forget: wait for them so Run never returns while one is still
			// probing/restarting/reinstalling a node.
			r.wg.Wait()
			return
		case <-ticker.C:
			r.runDue(ctx)
		}
	}
}

func (r *Recoverer) runDue(ctx context.Context) {
	nodes, err := r.store.NodesNeedingRecovery()
	if err != nil {
		r.log.Warn("list nodes needing out-of-band recovery failed", "err", err)
		return
	}
	for i := range nodes {
		node := nodes[i]
		if !r.begin(node.UUID) {
			continue
		}
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer r.end(node.UUID)
			select {
			case r.sem <- struct{}{}:
				defer func() { <-r.sem }()
			case <-ctx.Done():
				return
			}
			if _, err := r.recoverNode(ctx, &node, false); err != nil && !errors.Is(err, context.Canceled) {
				r.log.Warn("out-of-band recovery round failed", "node", node.UUID, "err", err)
			}
		}()
	}
}

// Trigger immediately runs one round for an operator request. It ignores the
// automatic grace/backoff schedule but preserves safety skips such as online,
// pending, stopped, or missing credentials.
func (r *Recoverer) Trigger(ctx context.Context, uuid string) (Outcome, error) {
	if r == nil || !r.cfg.Enabled {
		return Outcome{NodeUUID: uuid, Action: "skipped", SkippedReason: "recovery is disabled"}, nil
	}
	if !r.begin(uuid) {
		return Outcome{NodeUUID: uuid, Action: "skipped", SkippedReason: "recovery already in progress"}, nil
	}
	defer r.end(uuid)
	node, err := r.store.NodeByUUID(uuid)
	if err != nil {
		return Outcome{NodeUUID: uuid}, err
	}
	return r.recoverNode(ctx, node, true)
}

func (r *Recoverer) begin(uuid string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inFlight[uuid] {
		return false
	}
	r.inFlight[uuid] = true
	return true
}

func (r *Recoverer) end(uuid string) {
	r.mu.Lock()
	delete(r.inFlight, uuid)
	r.mu.Unlock()
}

func (r *Recoverer) recoverNode(ctx context.Context, node *store.Node, force bool) (Outcome, error) {
	out := Outcome{NodeUUID: node.UUID}
	if r.hub != nil && r.hub.Online(node.UUID) || node.State == store.NodeStateOnline {
		_ = r.store.ClearNodeRecovery(node.UUID)
		out.Action, out.Recovered, out.SkippedReason = "skipped", true, "node is online"
		return out, nil
	}
	if node.LastSeenAt == nil || node.State == store.NodeStateConnecting {
		out.Action, out.SkippedReason = "skipped", "node has never connected"
		return out, nil
	}
	if node.RecoveryPhase == store.RecoveryPhaseStopped {
		out.Action, out.SkippedReason = "skipped", store.RecoveryStoppedProviderInstanceGone
		return out, nil
	}
	if !node.HasSSHCredential {
		out.Action, out.SkippedReason = "skipped", "ssh credential is not configured"
		return out, nil
	}
	now := r.now()
	if !force {
		if r.cfg.GracePeriod > 0 && now.Sub(*node.LastSeenAt) < r.cfg.GracePeriod {
			out.Action, out.SkippedReason = "skipped", "within reconnect grace period"
			return out, nil
		}
		if node.LastProbeAt != nil && now.Before(node.LastProbeAt.Add(Backoff(node.RecoveryAttempts, r.cfg.InitialInterval, r.cfg.BackoffMultiplier, r.cfg.MaxInterval))) {
			out.Action, out.SkippedReason = "skipped", "recovery backoff has not elapsed"
			return out, nil
		}
		if node.ConnectMode == "server_dial" && r.hub != nil && !r.hub.ServerDialRecoveryReady(node.UUID) {
			out.Action, out.SkippedReason = "skipped", "server_dial reconnect has not reached maximum backoff"
			return out, nil
		}
	}

	// A provider may have renumbered the instance's ports (a RunPod pod whose
	// configuration was edited): follow it before logging in, and when the agent itself moved,
	// let the server dial it there first.
	if moved, err := r.refreshProviderEndpoints(ctx, node); err != nil {
		r.log.Warn("refresh provider endpoints failed", "node", node.UUID, "err", err)
	} else if moved {
		out.Action, out.SkippedReason = "redial", "the provider moved the agent's address; the server dials it there"
		return out, nil
	}

	if node.RecoveryAttempts > 0 && Backoff(node.RecoveryAttempts, r.cfg.InitialInterval, r.cfg.BackoffMultiplier, r.cfg.MaxInterval) >= r.cfg.MaxInterval {
		stopped, err := r.providerInstanceDestroyed(ctx, node)
		if err != nil {
			r.log.Warn("provider instance recovery check failed", "node", node.UUID, "err", err)
		} else if stopped {
			if err := r.store.SetNodeRecoveryPhase(node.UUID, store.RecoveryPhaseStopped, node.RecoveryAttempts, node.LastProbeAt, store.RecoveryStoppedProviderInstanceGone); err != nil {
				return out, err
			}
			out.Action, out.Phase, out.SkippedReason = "stopped", store.RecoveryPhaseStopped, store.RecoveryStoppedProviderInstanceGone
			return out, nil
		}
	}

	credential, secrets, err := r.store.GetNodeSSHCredential(node.UUID)
	if err != nil {
		return out, err
	}
	// Empty secrets mean the platform key; stored ones are tried after it.
	sshAuth := provision.SSHAuth{Password: secrets.Password, PrivateKeyPEM: secrets.PrivateKeyPEM, PlatformKey: r.platformKey()}
	probeReq := provision.SSHProbeRequest{
		Host: credential.Host, Port: credential.Port, User: credential.User,
		ServerURL: node.ServerURL, Token: node.Token, ConnectMode: node.ConnectMode,
		ServerDialListen: node.ServerDialListen,
		InstallDir:       node.InstallDir,
		RuntimeDriver:    restartRuntimeDriver(node),
		SSHAuth:          sshAuth,
	}
	if err := r.store.SetNodeRecoveryPhase(node.UUID, store.RecoveryPhaseProbing, node.RecoveryAttempts, &now, ""); err != nil {
		return out, err
	}
	out.Action, out.Phase = "probe", store.RecoveryPhaseProbing
	out.Probe = r.provisioner.Probe(ctx, probeReq)
	if !out.Probe.Reachable {
		message := classifyRecoveryFailure(store.RecoveryPhaseUnreachable, probeMessage(out.Probe, "SSH is unreachable"))
		phase, err := r.recordFailure(node, store.RecoveryPhaseUnreachable, r.now(), message)
		if err != nil {
			return out, err
		}
		out.Phase, out.Error = phase, message
		return out, nil
	}
	if out.Probe.Err != nil {
		message := classifyRecoveryFailure(store.RecoveryPhaseProbing, probeMessage(out.Probe, "SSH probe failed"))
		phase, err := r.recordFailure(node, store.RecoveryPhaseProbing, r.now(), message)
		if err != nil {
			return out, err
		}
		out.Phase = phase
		out.Error = message
		return out, nil
	}
	if out.Probe.ForeignServerURL != "" {
		// Another server's install replaced the shared unit. Restarting it
		// cannot bring this node back and reinstalling would take the host
		// away from that server, so leave the choice to an operator.
		message := classifyRecoveryFailure(store.RecoveryPhaseProbing, "gpuworks-agent unit on the host is installed for another server ("+out.Probe.ForeignServerURL+"); stop that unit or reinstall the agent for this server")
		phase, err := r.recordFailure(node, store.RecoveryPhaseProbing, r.now(), message)
		if err != nil {
			return out, err
		}
		out.Phase = phase
		out.Error = message
		return out, nil
	}
	if out.Probe.AgentRunning {
		message := "agent is running but has not established a control-plane session"
		if strings.TrimSpace(out.Probe.Detail) != "" {
			message += ": " + strings.TrimSpace(out.Probe.Detail)
		}
		if force && node.ConnectMode == agentconf.ServerDial && r.hub != nil {
			out.Action = "reconnect"
			if err := r.hub.ReconnectServerDial(ctx, node.UUID); err != nil {
				message = err.Error()
			}
		}
		if r.hub != nil && r.hub.Online(node.UUID) {
			out.Recovered, out.Phase = true, ""
			return out, r.store.ClearNodeRecovery(node.UUID)
		}
		message = classifyRecoveryFailure(store.RecoveryPhaseProbing, message)
		phase, err := r.recordFailure(node, store.RecoveryPhaseProbing, r.now(), message)
		if err != nil {
			return out, err
		}
		out.Phase = phase
		out.Error = message
		return out, nil
	}

	if err := r.store.SetNodeRecoveryPhase(node.UUID, store.RecoveryPhaseRestarting, node.RecoveryAttempts, &now, ""); err != nil {
		return out, err
	}
	out.Action, out.Phase = "restart", store.RecoveryPhaseRestarting
	restartErr := r.provisioner.RestartAgent(ctx, probeReq)
	if restartErr == nil {
		if err := r.store.SetNodeRecoveryPhase(node.UUID, store.RecoveryPhaseReconnecting, node.RecoveryAttempts, &now, ""); err != nil {
			return out, err
		}
		out.Phase = store.RecoveryPhaseReconnecting
		if r.waitOnline(ctx, node.UUID) {
			_ = r.store.ClearNodeRecovery(node.UUID)
			out.Recovered = true
			return out, nil
		}
	}

	if err := r.store.SetNodeRecoveryPhase(node.UUID, store.RecoveryPhaseReinstalling, node.RecoveryAttempts, &now, errorString(restartErr)); err != nil {
		return out, err
	}
	out.Action, out.Phase = "reinstall", store.RecoveryPhaseReinstalling
	installPhase := store.RecoveryPhaseReinstalling
	installReq := provision.SSHInstallRequest{
		Target: net.JoinHostPort(credential.Host, fmt.Sprint(credential.Port)), User: credential.User, Port: credential.Port,
		ServerURL: node.ServerURL, Token: node.Token, ConnectMode: node.ConnectMode,
		ServerDialListen: node.ServerDialListen, HostAddress: credential.Host, InstallDir: node.InstallDir,
		RuntimeDriver:  restartRuntimeDriver(node),
		KeepSupervisor: true, KeepUntraversableInstallDir: true,
		OnPhase: func(phase string) error {
			installPhase = phase
			out.Phase = phase
			return r.store.SetNodeRecoveryPhase(node.UUID, phase, node.RecoveryAttempts, &now, "")
		},
		SSHAuth: sshAuth,
	}
	_, installErr := r.provisioner.InstallSSH(ctx, installReq)
	if installErr == nil {
		installPhase = store.RecoveryPhaseReconnecting
		if err := r.store.SetNodeRecoveryPhase(node.UUID, installPhase, node.RecoveryAttempts, &now, ""); err != nil {
			return out, err
		}
		out.Phase = installPhase
		if r.waitOnline(ctx, node.UUID) {
			_ = r.store.ClearNodeRecovery(node.UUID)
			out.Recovered = true
			return out, nil
		}
	}
	message := errorString(installErr)
	if message == "" {
		message = "agent did not reconnect after reinstall"
	}
	if restartErr != nil {
		message = "restart failed: " + restartErr.Error() + "; reinstall: " + message
	}
	message = classifyRecoveryFailure(installPhase, message)
	recordedPhase, err := r.recordFailure(node, installPhase, r.now(), message)
	if err != nil {
		return out, err
	}
	out.Phase = recordedPhase
	out.Error = message
	return out, nil
}

func (r *Recoverer) recordFailure(node *store.Node, phase string, probeAt time.Time, message string) (string, error) {
	previousPhase := node.RecoveryPhase
	if phase == store.RecoveryPhaseProbing {
		phase = store.RecoveryPhaseProbeFailed
		if previousPhase == store.RecoveryPhaseProbing && node.LastRecoveryError != "" {
			previousPhase = store.RecoveryPhaseProbeFailed
		}
	}
	if previousPhase == phase && node.RecoveryAttempts >= 1 {
		return store.RecoveryPhaseBlocked, r.store.SetNodeRecoveryPhase(node.UUID, store.RecoveryPhaseBlocked, node.RecoveryAttempts+1, &probeAt, phase+": "+message)
	}
	return phase, r.store.SetNodeRecoveryPhase(node.UUID, phase, node.RecoveryAttempts+1, &probeAt, message)
}

func classifyRecoveryFailure(phase, message string) string {
	lower := strings.ToLower(message)
	class := "stage_failed"
	switch {
	case phase == store.RecoveryPhaseUnreachable, strings.Contains(lower, "ssh"), strings.Contains(lower, "connection"):
		class = "network"
	case phase == store.RecoveryPhaseArtifactCheck, strings.Contains(lower, "read agent binary"), strings.Contains(lower, "file does not exist"), strings.Contains(lower, "no such file"):
		class = "missing_artifact"
	case phase == store.RecoveryPhaseUploading && (strings.Contains(lower, "timed out") || strings.Contains(lower, "timeout")):
		class = "upload_stalled"
	case strings.Contains(lower, "installed for another server"):
		class = "foreign_agent"
	case strings.Contains(lower, "203/exec"), strings.Contains(lower, "not executable"), strings.Contains(lower, "binary is not installed"):
		class = "exec_missing"
	}
	return class + ": " + message
}

func (r *Recoverer) waitOnline(ctx context.Context, uuid string) bool {
	if r.hub == nil {
		return false
	}
	if r.hub.Online(uuid) {
		return true
	}
	deadline := time.NewTimer(r.cfg.RestartWait)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return r.hub.Online(uuid)
		case <-ticker.C:
			if r.hub.Online(uuid) {
				return true
			}
		}
	}
}

func (r *Recoverer) providerInstanceDestroyed(ctx context.Context, node *store.Node) (bool, error) {
	if r.providers == nil {
		return false, nil
	}
	linked, err := r.store.ProviderInstanceForNode(node.ID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	instances, err := r.syncProvider(ctx, linked.ProviderID)
	if err != nil {
		return false, err
	}
	for _, instance := range instances {
		if instance.ExternalID == linked.ExternalID {
			return false, nil
		}
	}
	return true, nil
}

func probeMessage(result provision.ProbeResult, fallback string) string {
	if result.Err != nil {
		return result.Err.Error()
	}
	if strings.TrimSpace(result.Detail) != "" {
		return strings.TrimSpace(result.Detail)
	}
	return fallback
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// platformKey loads the server's SSH key, or nil when there is none.
func (r *Recoverer) platformKey() *provision.PlatformKey {
	row, pem, err := r.store.PlatformSSHKey()
	if err != nil {
		return nil
	}
	return &provision.PlatformKey{PrivatePEM: pem, PublicKey: row.PublicKey, Fingerprint: row.Fingerprint}
}

// restartRuntimeDriver is the runtime a restarted or reinstalled agent is
// started with: the one an operator chose, or "" to let the agent detect it.
// A node with no recorded choice keeps the runtime it last ran — what it was
// installed with is not recorded, and that is known to start — unless the
// agent picked that runtime itself as a fallback: pinning it would keep the
// node there after the runtime it passed over can serve again.
func restartRuntimeDriver(node *store.Node) string {
	switch preference := strings.ToLower(strings.TrimSpace(node.RuntimePreference)); {
	case preference == proto.RuntimePreferenceAuto:
		return ""
	case proto.ValidRuntimePreference(preference):
		return preference
	case node.RuntimeFallbackFrom != "":
		return ""
	}
	return node.RuntimeDriver
}
