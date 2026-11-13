package recovery

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type fakeHub struct {
	online      map[string]bool
	serverReady bool
	resets      []string
}

func (h *fakeHub) Online(uuid string) bool             { return h.online[uuid] }
func (h *fakeHub) ServerDialRecoveryReady(string) bool { return h.serverReady }
func (h *fakeHub) ResetServerDialBackoff(uuid string)  { h.resets = append(h.resets, uuid) }

func (h *fakeHub) ReconnectServerDial(context.Context, string) error {
	return errors.New("unexpected server_dial reconnect")
}

type fakeProvisioner struct {
	probeResult   provision.ProbeResult
	restartErr    error
	installErr    error
	restarts      int
	installs      int
	probes        atomic.Int32
	activeProbe   atomic.Int32
	maxProbe      atomic.Int32
	probeBlock    <-chan struct{}
	onRestart     func()
	onInstall     func()
	installPhases []string
	installReq    provision.SSHInstallRequest
	reqMu         sync.Mutex // Probe runs concurrently in the scan tests
	probeReq      provision.SSHProbeRequest
	restartReq    provision.SSHProbeRequest
}

func (f *fakeProvisioner) Probe(ctx context.Context, req provision.SSHProbeRequest) provision.ProbeResult {
	f.probes.Add(1)
	f.reqMu.Lock()
	f.probeReq = req
	f.reqMu.Unlock()
	active := f.activeProbe.Add(1)
	for {
		maximum := f.maxProbe.Load()
		if active <= maximum || f.maxProbe.CompareAndSwap(maximum, active) {
			break
		}
	}
	defer f.activeProbe.Add(-1)
	if f.probeBlock != nil {
		select {
		case <-f.probeBlock:
		case <-ctx.Done():
		}
	}
	return f.probeResult
}
func (f *fakeProvisioner) RestartAgent(_ context.Context, req provision.SSHProbeRequest) error {
	f.restarts++
	f.restartReq = req
	if f.onRestart != nil {
		f.onRestart()
	}
	return f.restartErr
}
func (f *fakeProvisioner) InstallSSH(_ context.Context, req provision.SSHInstallRequest) (provision.SSHInstallReport, error) {
	f.installs++
	f.installReq = req
	for _, phase := range f.installPhases {
		if req.OnPhase != nil {
			if err := req.OnPhase(phase); err != nil {
				return provision.SSHInstallReport{}, err
			}
		}
	}
	if f.onInstall != nil {
		f.onInstall()
	}
	return provision.SSHInstallReport{}, f.installErr
}

type fakeProviders struct {
	instances []provider.Instance
	err       error
	calls     int
	// address is where the provider maps the agent's port now; "" answers
	// provider.ErrUnsupported.
	address    string
	addressErr error
	// detail is the provider's description of the instance; nil answers
	// provider.ErrUnsupported, as a provider whose listing is complete does.
	detail    *provider.Instance
	detailErr error
	details   int
}

func (f *fakeProviders) InstanceDetail(context.Context, uint, string) (provider.Instance, error) {
	f.details++
	if f.detailErr != nil {
		return provider.Instance{}, f.detailErr
	}
	if f.detail == nil {
		return provider.Instance{}, provider.ErrUnsupported
	}
	return *f.detail, nil
}

func (f *fakeProviders) ServerDialAddress(context.Context, uint, string, string) (string, error) {
	if f.addressErr != nil {
		return "", f.addressErr
	}
	if f.address == "" {
		return "", provider.ErrUnsupported
	}
	return f.address, nil
}

func (f *fakeProviders) Sync(context.Context, uint) ([]provider.Instance, error) {
	f.calls++
	return f.instances, f.err
}

func TestBackoffSequenceAndCap(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for i, expected := range want {
		if got := Backoff(i+1, 30*time.Second, 2, 10*time.Minute); got != expected {
			t.Fatalf("attempt %d delay=%s, want %s", i+1, got, expected)
		}
	}
	if got := Backoff(1_000_000, 30*time.Second, 2, 10*time.Minute); got != 10*time.Minute {
		t.Fatalf("large attempt delay=%s", got)
	}
}

func TestRecoveryGracePendingMissingCredentialAndServerDialLayering(t *testing.T) {
	r, st, hub, fake := newRecoveryHarness(t)
	node := newOfflineNode(t, st, true)
	node.LastSeenAt = timePtr(r.now().Add(-time.Minute))
	if outcome, err := r.recoverNode(context.Background(), node, false); err != nil || outcome.SkippedReason != "within reconnect grace period" {
		t.Fatalf("grace outcome=%+v err=%v", outcome, err)
	}
	if fake.probes.Load() != 0 {
		t.Fatalf("grace triggered %d probes", fake.probes.Load())
	}

	pending, _, err := st.CreateNodePending("pending", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := r.recoverNode(context.Background(), pending, true); err != nil || outcome.SkippedReason != "node has never connected" {
		t.Fatalf("pending outcome=%+v err=%v", outcome, err)
	}

	withoutCredential := newOfflineNode(t, st, false)
	withoutCredential.HasSSHCredential = false
	if outcome, err := r.recoverNode(context.Background(), withoutCredential, true); err != nil || outcome.SkippedReason != "ssh credential is not configured" {
		t.Fatalf("credential outcome=%+v err=%v", outcome, err)
	}

	serverDial := newOfflineNode(t, st, true)
	serverDial.ConnectMode = "server_dial"
	hub.serverReady = false
	if outcome, err := r.recoverNode(context.Background(), serverDial, false); err != nil || !strings.Contains(outcome.SkippedReason, "server_dial") {
		t.Fatalf("server_dial outcome=%+v err=%v", outcome, err)
	}
	if fake.probes.Load() != 0 {
		t.Fatalf("skip paths triggered %d probes", fake.probes.Load())
	}
	hub.serverReady = true
	fake.probeResult = provision.ProbeResult{Reachable: false, Detail: "dial failed", Err: context.DeadlineExceeded}
	if outcome, err := r.recoverNode(context.Background(), serverDial, false); err != nil || outcome.Phase != store.RecoveryPhaseUnreachable {
		t.Fatalf("server_dial ready outcome=%+v err=%v", outcome, err)
	}
	if fake.probes.Load() != 1 {
		t.Fatalf("server_dial at maximum tier probes=%d, want 1", fake.probes.Load())
	}
}

func TestUnreachableErrorsBackoffForeverWithoutStopping(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "timeout", err: &provision.SSHConnectionError{Err: context.DeadlineExceeded}},
		{name: "authentication", err: &provision.SSHConnectionError{Err: errors.New("ssh handshake: unable to authenticate")}},
		{name: "host-key", err: &provision.SSHConnectionError{Err: errors.New("ssh host key mismatch for node.example:22")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, st, _, fake := newRecoveryHarness(t)
			node := newOfflineNode(t, st, true)
			fake.probeResult = provision.ProbeResult{Reachable: false, Detail: tc.err.Error(), Err: tc.err}
			outcome, err := r.recoverNode(context.Background(), node, false)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Phase != store.RecoveryPhaseUnreachable || !strings.Contains(outcome.Error, strings.Split(tc.err.Error(), ":")[0]) {
				t.Fatalf("outcome=%+v", outcome)
			}
			updated, _ := st.NodeByUUID(node.UUID)
			if updated.RecoveryAttempts != 1 || updated.RecoveryPhase != store.RecoveryPhaseUnreachable {
				t.Fatalf("updated node=%+v", updated)
			}
			if outcome, err := r.recoverNode(context.Background(), updated, false); err != nil || outcome.SkippedReason != "recovery backoff has not elapsed" {
				t.Fatalf("backoff outcome=%+v err=%v", outcome, err)
			}
			if updated.RecoveryPhase == store.RecoveryPhaseStopped {
				t.Fatal("unreachable node stopped without provider deletion")
			}
		})
	}
}

func TestReachableStoppedAgentRestartsAndReconnectClearsRecovery(t *testing.T) {
	r, st, hub, fake := newRecoveryHarness(t)
	node := newOfflineNode(t, st, true)
	fake.probeResult = provision.ProbeResult{Reachable: true, AgentInstalled: true, Detail: "not-running"}
	fake.onRestart = func() { hub.online[node.UUID] = true }
	outcome, err := r.recoverNode(context.Background(), node, false)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Recovered || fake.restarts != 1 || fake.installs != 0 {
		t.Fatalf("outcome=%+v restart=%d install=%d", outcome, fake.restarts, fake.installs)
	}
	updated, _ := st.NodeByUUID(node.UUID)
	if updated.RecoveryPhase != "" || updated.RecoveryAttempts != 0 || updated.LastProbeAt != nil {
		t.Fatalf("recovery fields not cleared: %+v", updated)
	}
}

func TestRestartWithoutReconnectEscalatesToIdentityPreservingReinstall(t *testing.T) {
	r, st, hub, fake := newRecoveryHarness(t)
	node := newOfflineNode(t, st, true)
	if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Updates(map[string]any{
		"runtime_driver": "udocker", "server_dial_listen": ":19191", "install_dir": "/data/agent2",
	}).Error; err != nil {
		t.Fatal(err)
	}
	node, _ = st.NodeByUUID(node.UUID)
	fake.probeResult = provision.ProbeResult{Reachable: true, AgentInstalled: false, Detail: "not-running"}
	fake.onInstall = func() { hub.online[node.UUID] = true }
	outcome, err := r.recoverNode(context.Background(), node, false)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Recovered || fake.restarts != 1 || fake.installs != 1 {
		t.Fatalf("outcome=%+v restart=%d install=%d", outcome, fake.restarts, fake.installs)
	}
	if fake.installReq.Token != node.Token || fake.installReq.ServerURL != node.ServerURL || fake.installReq.ConnectMode != node.ConnectMode ||
		fake.installReq.RuntimeDriver != "udocker" || fake.installReq.ServerDialListen != ":19191" || fake.installReq.HostAddress != "node.example" {
		t.Fatalf("reinstall identity/settings=%+v node=%+v", fake.installReq, node)
	}
	// The node's install chose systemd or nohup; recovery must not flip it.
	if !fake.installReq.KeepSupervisor {
		t.Fatalf("reinstall does not keep the host's supervisor: %+v", fake.installReq)
	}
	// The agent already runs from there: a path the runtime user cannot reach
	// must not keep a recovering node offline.
	if !fake.installReq.KeepUntraversableInstallDir {
		t.Fatalf("reinstall would stop on an untraversable install directory: %+v", fake.installReq)
	}
	// A host may run several agents: every step reaches this node's own.
	if fake.probeReq.InstallDir != "/data/agent2" || fake.restartReq.InstallDir != "/data/agent2" || fake.installReq.InstallDir != "/data/agent2" {
		t.Fatalf("install dir probe=%q restart=%q install=%q, want /data/agent2", fake.probeReq.InstallDir, fake.restartReq.InstallDir, fake.installReq.InstallDir)
	}
}

func TestRunDueEnforcesGlobalConcurrencyAcrossOverlappingScans(t *testing.T) {
	r, st, _, fake := newRecoveryHarness(t)
	r.sem = make(chan struct{}, 2)
	block := make(chan struct{})
	fake.probeBlock = block
	released := false
	defer func() {
		if !released {
			close(block)
		}
	}()

	for range 2 {
		newOfflineNode(t, st, true)
	}
	r.runDue(context.Background())
	waitForRecoveryTest(t, time.Second, func() bool { return fake.activeProbe.Load() == 2 })
	for range 2 {
		newOfflineNode(t, st, true)
	}
	r.runDue(context.Background())
	time.Sleep(50 * time.Millisecond)
	if got := fake.activeProbe.Load(); got != 2 {
		t.Fatalf("active probes across overlapping scans = %d, want 2", got)
	}
	if got := fake.maxProbe.Load(); got != 2 {
		t.Fatalf("maximum concurrent probes = %d, want 2", got)
	}
	if got := fake.probes.Load(); got != 2 {
		t.Fatalf("probes started before capacity was released = %d, want 2", got)
	}

	close(block)
	released = true
	waitForRecoveryTest(t, time.Second, func() bool { return fake.probes.Load() == 4 && fake.activeProbe.Load() == 0 })
}

func TestSecondConsecutiveUnreachableFailureBlocksAutomaticRecovery(t *testing.T) {
	r, st, _, fake := newRecoveryHarness(t)
	node := newOfflineNode(t, st, true)
	fake.probeResult = provision.ProbeResult{Reachable: false, Detail: "ssh dial: timeout", Err: context.DeadlineExceeded}
	for wantAttempt := 1; wantAttempt <= 2; wantAttempt++ {
		node, _ = st.NodeByUUID(node.UUID)
		if node.LastProbeAt != nil {
			delay := Backoff(node.RecoveryAttempts, r.cfg.InitialInterval, r.cfg.BackoffMultiplier, r.cfg.MaxInterval)
			r.now = func() time.Time { return node.LastProbeAt.Add(delay + time.Nanosecond) }
		}
		outcome, err := r.recoverNode(context.Background(), node, false)
		if err != nil {
			t.Fatal(err)
		}
		node, _ = st.NodeByUUID(node.UUID)
		wantPhase := store.RecoveryPhaseUnreachable
		if wantAttempt == 2 {
			wantPhase = store.RecoveryPhaseBlocked
		}
		if outcome.Phase != wantPhase || node.RecoveryAttempts != wantAttempt || node.RecoveryPhase != wantPhase {
			t.Fatalf("attempt %d outcome=%+v node=%+v", wantAttempt, outcome, node)
		}
	}
	due, err := st.NodesNeedingRecovery()
	if err != nil || len(due) != 0 {
		t.Fatalf("blocked node remained due: %+v err=%v", due, err)
	}
}

func TestRunningAgentIsObservedWithoutRestart(t *testing.T) {
	r, st, _, fake := newRecoveryHarness(t)
	node := newOfflineNode(t, st, true)
	fake.probeResult = provision.ProbeResult{Reachable: true, AgentRunning: true, AgentInstalled: true, Detail: "systemd"}
	outcome, err := r.recoverNode(context.Background(), node, false)
	if err != nil {
		t.Fatal(err)
	}
	if fake.restarts != 0 || fake.installs != 0 || !strings.Contains(outcome.Error, "control-plane") {
		t.Fatalf("outcome=%+v restart=%d install=%d", outcome, fake.restarts, fake.installs)
	}
}

func TestAnotherServersAgentUnitIsReportedWithoutTakingItOver(t *testing.T) {
	r, st, _, fake := newRecoveryHarness(t)
	node := newOfflineNode(t, st, true)
	fake.probeResult = provision.ProbeResult{Reachable: true, AgentInstalled: true, ForeignServerURL: "http://192.0.2.156:8800", Detail: "foreign-systemd http://192.0.2.156:8800"}
	outcome, err := r.recoverNode(context.Background(), node, false)
	if err != nil {
		t.Fatal(err)
	}
	want := "foreign_agent: gpuworks-agent unit on the host is installed for another server (http://192.0.2.156:8800)"
	if fake.restarts != 0 || fake.installs != 0 || outcome.Phase != store.RecoveryPhaseProbeFailed || !strings.HasPrefix(outcome.Error, want) {
		t.Fatalf("outcome=%+v restart=%d install=%d", outcome, fake.restarts, fake.installs)
	}
	updated, _ := st.NodeByUUID(node.UUID)
	if updated.RecoveryAttempts != 1 || !strings.HasPrefix(updated.LastRecoveryError, want) {
		t.Fatalf("foreign unit not recorded: %+v", updated)
	}
}

func TestReachableProbeErrorIsRecordedWithoutMutatingAgent(t *testing.T) {
	r, st, _, fake := newRecoveryHarness(t)
	node := newOfflineNode(t, st, true)
	fake.probeResult = provision.ProbeResult{Reachable: true, AgentInstalled: true, Detail: "remote command failed", Err: errors.New("probe agent process: exit status 1")}
	outcome, err := r.recoverNode(context.Background(), node, false)
	if err != nil {
		t.Fatal(err)
	}
	if fake.restarts != 0 || fake.installs != 0 || outcome.Phase != store.RecoveryPhaseProbeFailed || !strings.Contains(outcome.Error, "probe agent process") {
		t.Fatalf("outcome=%+v restart=%d install=%d", outcome, fake.restarts, fake.installs)
	}
	updated, _ := st.NodeByUUID(node.UUID)
	if updated.RecoveryAttempts != 1 || updated.RecoveryPhase != store.RecoveryPhaseProbeFailed {
		t.Fatalf("probe failure not persisted: %+v", updated)
	}
}

func TestRestartAndReinstallFailuresAreCombinedAndBackedOff(t *testing.T) {
	r, st, _, fake := newRecoveryHarness(t)
	node := newOfflineNode(t, st, true)
	fake.probeResult = provision.ProbeResult{Reachable: true, AgentInstalled: false, Detail: "not-running"}
	fake.restartErr = errors.New("restart denied")
	fake.installErr = errors.New("upload failed")
	outcome, err := r.recoverNode(context.Background(), node, false)
	if err != nil {
		t.Fatal(err)
	}
	if fake.restarts != 1 || fake.installs != 1 || outcome.Phase != store.RecoveryPhaseReinstalling ||
		!strings.Contains(outcome.Error, "restart denied") || !strings.Contains(outcome.Error, "upload failed") {
		t.Fatalf("outcome=%+v restart=%d install=%d", outcome, fake.restarts, fake.installs)
	}
	updated, _ := st.NodeByUUID(node.UUID)
	if updated.RecoveryAttempts != 1 || updated.RecoveryPhase != store.RecoveryPhaseReinstalling || updated.LastProbeAt == nil {
		t.Fatalf("failed reinstall not backed off: %+v", updated)
	}
}

func TestRecoveryPersistsInstallSubphasesAndBlocksRepeatedStage(t *testing.T) {
	r, st, _, fake := newRecoveryHarness(t)
	node := newOfflineNode(t, st, true)
	fake.probeResult = provision.ProbeResult{Reachable: true, AgentInstalled: false, Detail: "not-running"}
	fake.restartErr = errors.New("restart denied")
	fake.installErr = errors.New("upload /opt/gpuworks/gpuworks-agent timed out after 10m")
	fake.installPhases = []string{store.RecoveryPhaseArtifactCheck, store.RecoveryPhaseUploading}

	first, err := r.recoverNode(context.Background(), node, false)
	if err != nil || first.Phase != store.RecoveryPhaseUploading || !strings.Contains(first.Error, "upload_stalled:") {
		t.Fatalf("first outcome=%+v err=%v", first, err)
	}
	node, _ = st.NodeByUUID(node.UUID)
	if node.RecoveryPhase != store.RecoveryPhaseUploading || node.RecoveryAttempts != 1 {
		t.Fatalf("first node=%+v", node)
	}
	r.now = func() time.Time {
		return node.LastProbeAt.Add(Backoff(node.RecoveryAttempts, r.cfg.InitialInterval, r.cfg.BackoffMultiplier, r.cfg.MaxInterval) + time.Second)
	}
	second, err := r.recoverNode(context.Background(), node, false)
	if err != nil || second.Phase != store.RecoveryPhaseBlocked {
		t.Fatalf("second outcome=%+v err=%v", second, err)
	}
	blocked, _ := st.NodeByUUID(node.UUID)
	if blocked.RecoveryPhase != store.RecoveryPhaseBlocked || !strings.Contains(blocked.LastRecoveryError, store.RecoveryPhaseUploading) {
		t.Fatalf("blocked node=%+v", blocked)
	}
}

func TestTriggerBypassesSchedulesButPreservesSafetyAndExclusion(t *testing.T) {
	r, st, _, fake := newRecoveryHarness(t)
	node := newOfflineNode(t, st, true)
	recent := r.now().Add(-time.Second)
	if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Update("last_seen_at", recent).Error; err != nil {
		t.Fatal(err)
	}
	fake.probeResult = provision.ProbeResult{Reachable: false, Detail: "offline", Err: context.DeadlineExceeded}
	outcome, err := r.Trigger(context.Background(), node.UUID)
	if err != nil || outcome.Phase != store.RecoveryPhaseUnreachable || fake.probes.Load() != 1 {
		t.Fatalf("forced trigger outcome=%+v probes=%d err=%v", outcome, fake.probes.Load(), err)
	}

	if !r.begin(node.UUID) {
		t.Fatal("failed to reserve node for exclusion test")
	}
	outcome, err = r.Trigger(context.Background(), node.UUID)
	r.end(node.UUID)
	if err != nil || outcome.SkippedReason != "recovery already in progress" {
		t.Fatalf("in-flight trigger outcome=%+v err=%v", outcome, err)
	}

	r.cfg.Enabled = false
	outcome, err = r.Trigger(context.Background(), node.UUID)
	if err != nil || outcome.SkippedReason != "recovery is disabled" {
		t.Fatalf("disabled trigger outcome=%+v err=%v", outcome, err)
	}
	r.cfg.Enabled = true
	if _, err := r.Trigger(context.Background(), "missing-node"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing trigger err=%v", err)
	}
}

func TestRunHonorsDisabledAndCanceledContexts(t *testing.T) {
	r, _, _, _ := newRecoveryHarness(t)
	r.cfg.Enabled = false
	done := make(chan struct{})
	go func() {
		r.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disabled recovery loop did not return")
	}

	r.cfg.Enabled = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done = make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled recovery loop did not return")
	}
}

func TestProviderInstanceDeletionStopsRecoveryAtMaximumBackoff(t *testing.T) {
	r, st, _, fake := newRecoveryHarness(t)
	providers := &fakeProviders{}
	r.providers = providers
	node := newOfflineNode(t, st, true)
	if err := st.DB().Create(&store.ProviderInstance{ProviderID: 42, ExternalID: "gone", NodeID: node.ID}).Error; err != nil {
		t.Fatal(err)
	}
	probeAt := r.now().Add(-11 * time.Minute)
	if err := st.SetNodeRecoveryPhase(node.UUID, store.RecoveryPhaseUnreachable, 6, &probeAt, "offline"); err != nil {
		t.Fatal(err)
	}
	node, _ = st.NodeByUUID(node.UUID)
	outcome, err := r.recoverNode(context.Background(), node, false)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Phase != store.RecoveryPhaseStopped || providers.calls != 1 || fake.probes.Load() != 0 {
		t.Fatalf("outcome=%+v provider_calls=%d probes=%d", outcome, providers.calls, fake.probes.Load())
	}
	stopped, _ := st.NodeByUUID(node.UUID)
	if stopped.RecoveryPhase != store.RecoveryPhaseStopped {
		t.Fatalf("node=%+v", stopped)
	}
}

func TestStoppedProviderInstanceStillExistsUsesFailureBlockInsteadOfStopped(t *testing.T) {
	r, st, _, fake := newRecoveryHarness(t)
	providers := &fakeProviders{instances: []provider.Instance{{ExternalID: "still-exists", Power: provider.PowerStopped}}}
	r.providers = providers
	node := newOfflineNode(t, st, true)
	if err := st.DB().Create(&store.ProviderInstance{ProviderID: 42, ExternalID: "still-exists", NodeID: node.ID}).Error; err != nil {
		t.Fatal(err)
	}
	probeAt := r.now().Add(-11 * time.Minute)
	if err := st.SetNodeRecoveryPhase(node.UUID, store.RecoveryPhaseUnreachable, 6, &probeAt, "offline"); err != nil {
		t.Fatal(err)
	}
	fake.probeResult = provision.ProbeResult{Reachable: false, Detail: "offline", Err: context.DeadlineExceeded}
	node, _ = st.NodeByUUID(node.UUID)
	outcome, err := r.recoverNode(context.Background(), node, false)
	if err != nil {
		t.Fatal(err)
	}
	if providers.calls != 1 || fake.probes.Load() != 1 || outcome.Phase != store.RecoveryPhaseBlocked {
		t.Fatalf("outcome=%+v provider_calls=%d probes=%d", outcome, providers.calls, fake.probes.Load())
	}
	updated, _ := st.NodeByUUID(node.UUID)
	if updated.RecoveryPhase != store.RecoveryPhaseBlocked {
		t.Fatalf("powered-off existing provider instance phase=%s, want blocked after repeated network failure", updated.RecoveryPhase)
	}
}

func waitForRecoveryTest(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition timed out")
}

func newRecoveryHarness(t *testing.T) (*Recoverer, *store.Store, *fakeHub, *fakeProvisioner) {
	t.Helper()
	st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := auth.NewSecretbox("recovery-test")
	if err != nil {
		t.Fatal(err)
	}
	st.SetSecretbox(box)
	hub := &fakeHub{online: map[string]bool{}, serverReady: true}
	fake := &fakeProvisioner{}
	r := New(st, hub, nil, fake, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{
		Enabled: true, GracePeriod: 2 * time.Minute, InitialInterval: 30 * time.Second,
		BackoffMultiplier: 2, MaxInterval: 10 * time.Minute, RestartWait: time.Millisecond,
	})
	r.now = func() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) }
	return r, st, hub, fake
}

func newOfflineNode(t *testing.T, st *store.Store, withCredential bool) *store.Node {
	t.Helper()
	node, _, err := st.CreateNodePending("recover", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeServerURL(node.UUID, "https://control.example"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeState(node.UUID, store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeState(node.UUID, store.NodeStateOffline); err != nil {
		t.Fatal(err)
	}
	seen := time.Date(2026, 8, 24, 11, 0, 0, 0, time.UTC)
	if err := st.DB().Model(&store.Node{}).Where("uuid = ?", node.UUID).Update("last_seen_at", seen).Error; err != nil {
		t.Fatal(err)
	}
	if withCredential {
		if _, err := st.SetNodeSSHCredential(node.UUID, "node.example", 22, "root", store.NodeSSHSecrets{Password: "secret"}); err != nil {
			t.Fatal(err)
		}
	}
	node, err = st.NodeByUUID(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	return node
}

func timePtr(value time.Time) *time.Time { return &value }

// A reinstalled agent starts with the runtime an operator chose, or none so
// it detects one. A node with no recorded choice keeps the runtime it last
// ran, unless the agent fell back to it: pinning runveil then would keep
// the node off Docker after Docker can serve its cards again.
func TestReinstallStartsTheAgentWithTheChosenRuntimeNotItsFallback(t *testing.T) {
	for _, tc := range []struct {
		name, preference, runtime, fallbackFrom, want string
	}{
		{"operator chose docker", "docker", "udocker", "", "docker"},
		{"operator chose runveil", "udocker", "udocker", "", "udocker"},
		{"auto", "auto", "udocker", "docker", ""},
		{"auto on docker", "auto", "docker", "", ""},
		{"no choice, fell back", "", "udocker", "docker", ""},
		{"no choice, last ran runveil", "", "udocker", "", "udocker"},
		{"no choice, last ran docker", "", "docker", "", "docker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, st, hub, fake := newRecoveryHarness(t)
			node := newOfflineNode(t, st, true)
			if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Updates(map[string]any{
				"runtime_driver": tc.runtime, "runtime_preference": tc.preference, "runtime_fallback_from": tc.fallbackFrom,
			}).Error; err != nil {
				t.Fatal(err)
			}
			node, _ = st.NodeByUUID(node.UUID)
			fake.probeResult = provision.ProbeResult{Reachable: true, AgentInstalled: false, Detail: "not-running"}
			fake.onInstall = func() { hub.online[node.UUID] = true }
			if _, err := r.recoverNode(context.Background(), node, false); err != nil {
				t.Fatal(err)
			}
			if fake.installs != 1 || fake.installReq.RuntimeDriver != tc.want {
				t.Fatalf("installs=%d runtime=%q, want %q", fake.installs, fake.installReq.RuntimeDriver, tc.want)
			}
		})
	}
}
