package agentupgrade

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Observe is called from the Hub's control reader. It never waits on SQLite
// or a worker. Polling live formal sessions recovers missed success signals.
func (s *Service) Observe(o Observation) {
	select {
	case s.events <- o:
		s.notify()
	default:
		s.cfg.Logger.Warn("agent upgrade observation queue full", "node", o.NodeUUID)
	}
}

func (s *Service) Run(ctx context.Context) {
	if !s.running.CompareAndSwap(false, true) {
		return
	}
	defer s.running.Store(false)
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := s.reconcile(ctx); err != nil {
			s.cfg.Logger.Error("agent upgrade reconcile failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

func (s *Service) reconcile(ctx context.Context) error {
	if err := s.retainHistory(); err != nil {
		return err
	}
	items, err := s.st.AgentUpgradePendingItems()
	if err != nil {
		return err
	}
	// Record expiry before consuming fresh evidence. A late formal reconnect
	// can verify the reservation, but cannot rewrite a missed deadline as success.
	for index := range items {
		i := &items[index]
		if activePhase(i.Phase) && (i.DeadlineAt == nil || !s.now().Before(*i.DeadlineAt)) {
			if err := s.unconfirmed(*i, "confirmation_timeout", "the agent upgrade was not confirmed before its deadline"); err != nil {
				return err
			}
			i.Phase = "unconfirmed"
		}
	}
	for _, i := range items {
		if activePhase(i.Phase) || (i.Phase == "unconfirmed" && i.VerifiedAt == nil) {
			if err := s.observeConnection(i); err != nil {
				return err
			}
		}
	}
	for {
		select {
		case o := <-s.events:
			if err := s.observeEvent(o); err != nil {
				return err
			}
		default:
			goto drained
		}
	}
drained:
	items, err = s.st.AgentUpgradePendingItems()
	if err != nil {
		return err
	}
	if b, err := s.st.AgentUpgradeBlocker(); err != nil || b != nil {
		return err
	}
	runs, err := s.st.AgentUpgradeRuns()
	if err != nil {
		return err
	}
	byID := map[uint]store.AgentUpgradeRun{}
	for _, r := range runs {
		byID[r.ID] = r
	}
	for _, i := range items {
		if i.Phase != "queued" {
			continue
		}
		r := byID[i.RunID]
		if r.PauseReason != "" || r.InterruptedReason != "" || r.StopRequestedAt != nil {
			continue
		}
		return s.execute(ctx, r, i)
	}
	return nil
}

func (s *Service) execute(ctx context.Context, run store.AgentUpgradeRun, item store.AgentUpgradeItem) error {
	conn, online := s.cfg.Transport.AgentUpgradeConnection(item.NodeUUID)
	now := s.now()
	skip := func(reason, detail string) error {
		_, err := s.st.UpdateAgentUpgradeItem(item.ID, []string{"queued"}, map[string]any{"phase": "skipped", "reason": reason, "error": detail, "finished_at": now})
		s.notify()
		return err
	}
	if !online || conn.SessionID == "" {
		return skip("offline", "node went offline before dispatch")
	}
	if conn.Version == run.TargetVersion {
		return skip("already_current", "the agent already matches the target")
	}
	if item.Transport == "push" && !slices.Contains(conn.Caps, proto.CapUpgradePush) {
		return skip("capabilities_changed", "agent capabilities changed; preview again")
	}
	node, err := s.st.NodeByUUID(item.NodeUUID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return skip("node_deleted", "node was deleted before dispatch")
		}
		return err
	}
	if proto.AgentBinaryName(node.OS, node.Arch) != item.ArtifactName {
		return skip("platform_changed", "the agent platform changed; preview again")
	}
	if node.KubeClusterID != 0 && !slices.Contains(conn.Caps, proto.CapKubeBinaryUpgrade) {
		return skip("kubernetes", "the agent could not start from the node's state directory and runs its image's own binary, which an upgrade cannot replace")
	}
	if item.Transport == "pull" && node.ConnectMode == "server_dial" {
		return skip("legacy_unverified", "this older server-dial agent cannot prove a download address")
	}
	deadline := now.Add(s.cfg.TransferTimeout + s.cfg.ConfirmTimeout)
	claimed, err := s.st.ClaimAgentUpgrade(item.ID, conn.SessionID, now, deadline)
	if err != nil || !claimed {
		return err
	}
	item.BaselineSessionID, item.StartedAt, item.DeadlineAt, item.Phase = conn.SessionID, &now, &deadline, "transferring"
	m, err := s.loadManifest(run.ID)
	if err != nil {
		return s.artifactFailure(item, err)
	}
	a, ok := m[item.ArtifactName]
	if !ok {
		return s.artifactFailure(item, errors.New("the fixed artifact is missing from the manifest"))
	}
	binary, err := s.readArtifact(a)
	if err != nil {
		return s.artifactFailure(item, err)
	}
	transferCtx, cancel := context.WithTimeout(ctx, s.cfg.TransferTimeout)
	defer cancel()
	if item.Transport == "push" {
		err = s.cfg.Transport.PushAgentUpgrade(transferCtx, item.NodeUUID, conn.SessionID, a.Name, binary)
	} else {
		err = s.cfg.Transport.RequestAgentUpgrade(transferCtx, item.NodeUUID, conn.SessionID)
	}
	if ctx.Err() != nil {
		_, saveErr := s.st.UpdateAgentUpgradeItem(item.ID, []string{"transferring"}, map[string]any{"phase": "observing_recovery", "reason": "server_stopped", "error": "server stopped while the upgrade outcome was pending"})
		return saveErr
	}
	if err != nil {
		var rejected *RejectedError
		if errors.As(err, &rejected) && !rejected.Busy {
			return s.finish(item, "failed", "agent_rejected", err.Error())
		}
		_, saveErr := s.st.UpdateAgentUpgradeItem(item.ID, []string{"transferring"}, map[string]any{"phase": "observing_recovery", "reason": "transport_uncertain", "error": err.Error()})
		s.notify()
		return saveErr
	}
	if item.Transport == "push" {
		confirmDeadline := s.now().Add(s.cfg.ConfirmTimeout)
		_, err = s.st.UpdateAgentUpgradeItem(item.ID, []string{"transferring"}, map[string]any{"phase": "waiting_registration", "deadline_at": confirmDeadline})
	}
	s.notify()
	return err
}

func (s *Service) artifactFailure(i store.AgentUpgradeItem, err error) error {
	if saveErr := s.finish(i, "failed", "artifact_unavailable", err.Error()); saveErr != nil {
		return saveErr
	}
	return s.st.PauseAgentUpgrade(i.RunID, "artifact_unavailable")
}
func (s *Service) finish(i store.AgentUpgradeItem, phase, reason, detail string) error {
	now := s.now()
	_, err := s.st.UpdateAgentUpgradeItem(i.ID, []string{"transferring", "waiting_registration", "observing_recovery"}, map[string]any{"phase": phase, "reason": reason, "error": detail, "finished_at": now})
	s.notify()
	return err
}
func (s *Service) unconfirmed(i store.AgentUpgradeItem, reason, detail string) error {
	if err := s.finish(i, "unconfirmed", reason, detail); err != nil {
		return err
	}
	return s.st.PauseAgentUpgrade(i.RunID, "confirmation_required")
}

func (s *Service) observeConnection(i store.AgentUpgradeItem) error {
	conn, ok := s.cfg.Transport.AgentUpgradeConnection(i.NodeUUID)
	if !ok || !conn.Ready || conn.SessionID == "" || conn.SessionID == i.BaselineSessionID {
		return nil
	}
	run, err := s.st.AgentUpgradeRun(i.RunID)
	if err != nil {
		return err
	}
	if conn.Version != run.TargetVersion {
		return nil
	}
	now := s.now()
	if i.Phase == "unconfirmed" {
		_, err = s.st.VerifyAgentUpgrade(i.ID, now, "system", "target_registered", "the original upgrade later reached the fixed target on a new formal session")
		return err
	}
	_, err = s.st.UpdateAgentUpgradeItem(i.ID, []string{"transferring", "waiting_registration", "observing_recovery"}, map[string]any{"phase": "succeeded", "reason": "target_registered", "registered_at": now, "observed_version": conn.Version, "finished_at": now})
	s.notify()
	return err
}

func (s *Service) observeEvent(o Observation) error {
	if o.State != "failed" {
		return nil
	}
	items, err := s.st.AgentUpgradePendingItems()
	if err != nil {
		return err
	}
	for _, i := range items {
		if i.NodeUUID != o.NodeUUID || (!activePhase(i.Phase) && !(i.Phase == "unconfirmed" && i.VerifiedAt == nil)) || i.StartedAt == nil {
			continue
		}
		if !o.At.IsZero() && o.At.Before(*i.StartedAt) {
			continue
		}
		conn, online := s.cfg.Transport.AgentUpgradeConnection(i.NodeUUID)
		if o.SessionID == "" || !online || conn.SessionID != o.SessionID {
			continue
		}
		if o.SessionID != i.BaselineSessionID && !strings.HasPrefix(o.Error, "rolled back:") {
			continue
		}
		if strings.Contains(strings.ToLower(o.Error), "already in progress") {
			return nil
		}
		run, err := s.st.AgentUpgradeRun(i.RunID)
		if err != nil {
			return err
		}
		if conn.Version == run.TargetVersion {
			continue
		}
		if i.Phase == "unconfirmed" {
			_, err = s.st.VerifyAgentUpgrade(i.ID, s.now(), "system", "agent_failed", o.Error)
			return err
		}
		return s.finish(i, "failed", "agent_failed", o.Error)
	}
	return nil
}
