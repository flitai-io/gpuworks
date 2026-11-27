package agentupgrade

import (
	"context"
	"errors"
	"slices"
	"sort"
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
	if err := s.settleRemovedNodes(items); err != nil {
		return err
	}
	s.forgetSettled(items)
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
	// After the events: a rolled-back agent's own report settles its attempt
	// first, with its words.
	for _, i := range items {
		if err := s.backOnOldVersion(i); err != nil {
			return err
		}
	}
	if b, err := s.st.AgentUpgradeBlocker(s.heldSince()); err != nil || b != nil {
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
	claimed, err := s.st.ClaimAgentUpgrade(item.ID, conn.SessionID, now, deadline, s.heldSince())
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

// unconfirmed records an attempt that missed its deadline. Its batch goes on:
// how this one node ended concerns only that node, and the platform settles
// it by itself when the node is seen again (observeConnection,
// backOnOldVersion, observeEvent) or is removed (settleRemovedNodes). The
// attempt holds the slot for UnconfirmedHold (heldSince), after which
// reconcile dispatches the batch's next node; nothing waits for an
// administrator.
func (s *Service) unconfirmed(i store.AgentUpgradeItem, reason, detail string) error {
	if err := s.finish(i, "unconfirmed", reason, detail); err != nil {
		return err
	}
	return s.pauseOnRepeatedUnknownOutcome(i.RunID)
}

// PauseRepeatedUnknownOutcome pauses a batch whose last two attempted nodes
// both ended with an unknown outcome.
const PauseRepeatedUnknownOutcome = "repeated_unknown_outcome"

// pauseOnRepeatedUnknownOutcome is the batch's circuit breaker. One node that
// goes silent is that node's matter and the batch goes on; two in a row say
// something about the target build or the network, and going on would take
// the fleet's agents away one by one with nobody watching. The batch then
// pauses, saying why, and an administrator continues it. An attempt later
// seen on the target version did succeed and does not count.
func (s *Service) pauseOnRepeatedUnknownOutcome(runID uint) error {
	items, err := s.st.AgentUpgradeItems(runID)
	if err != nil {
		return err
	}
	attempted := make([]store.AgentUpgradeItem, 0, len(items))
	for _, i := range items {
		if i.StartedAt != nil && !activePhase(i.Phase) {
			attempted = append(attempted, i)
		}
	}
	sort.SliceStable(attempted, func(a, b int) bool { return attempted[a].StartedAt.Before(*attempted[b].StartedAt) })
	if len(attempted) < 2 {
		return nil
	}
	for _, i := range attempted[len(attempted)-2:] {
		if i.Phase != "unconfirmed" || i.VerificationCode == "target_registered" {
			return nil
		}
	}
	return s.st.PauseAgentUpgrade(runID, PauseRepeatedUnknownOutcome)
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
	// A node can have an attempt in flight and an older one still
	// unconfirmed (once the older stops holding the slot, the node may be
	// upgraded again): a failure is the attempt in flight's first.
	sort.SliceStable(items, func(a, b int) bool { return activePhase(items[a].Phase) && !activePhase(items[b].Phase) })
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
		if o.SessionID != i.BaselineSessionID && o.Code != proto.ErrorAgentUpgradeRolledBack {
			continue
		}
		if o.Code == proto.ErrorAgentUpgradeInProgress {
			return nil
		}
		run, err := s.st.AgentUpgradeRun(i.RunID)
		if err != nil {
			return err
		}
		if conn.Version == run.TargetVersion {
			continue
		}
		// A rollback has its own reason, which the upgrade view already
		// explains; any other agent failure is agent_failed with its detail.
		reason := "agent_failed"
		if o.Code == proto.ErrorAgentUpgradeRolledBack {
			reason = "rolled_back"
		}
		if i.Phase == "unconfirmed" {
			_, err = s.st.VerifyAgentUpgrade(i.ID, s.now(), "system", reason, o.Error)
			return err
		}
		return s.finish(i, "failed", reason, o.Error)
	}
	return nil
}

// settleRemovedNodes settles the unconfirmed attempts whose node the
// platform no longer has (deleted by hand, or by a sweep once the cloud or
// the cluster dropped it): that node's agent will never come back to confirm
// or fail them, so nothing else would end the hold on the slot. Only an
// attempt past its deadline is settled, never one in flight: deleting a
// Kubernetes or Slurm node, or forcing a deletion, leaves its agent running,
// and it enrolls again under a new identity while the upgrade it was given
// may still be applying; the deadline bounds that, as it does for any
// attempt. The sweeps remove a node only after it has been gone longer than
// the deadline, so their attempts are always unconfirmed by then.
func (s *Service) settleRemovedNodes(items []store.AgentUpgradeItem) error {
	var uuids []string
	for _, i := range items {
		if i.Phase == "unconfirmed" && i.VerifiedAt == nil {
			uuids = append(uuids, i.NodeUUID)
		}
	}
	if len(uuids) == 0 {
		return nil
	}
	exists, err := s.st.ExistingNodeUUIDs(uuids)
	if err != nil {
		return err
	}
	for index := range items {
		i := &items[index]
		if exists[i.NodeUUID] {
			continue
		}
		if i.Phase == "unconfirmed" && i.VerifiedAt == nil {
			if _, err := s.st.VerifyAgentUpgrade(i.ID, s.now(), "system", ReasonNodeRemoved, ""); err != nil {
				return err
			}
			now := s.now()
			i.VerifiedAt = &now
			s.notify()
			s.cfg.Logger.Info("agent upgrade attempt settled: its node was removed", "run", i.RunID, "node", i.NodeUUID)
		}
	}
	return nil
}

// ReasonNodeRemoved is the verification code of an unconfirmed attempt whose
// node was removed from the platform.
const ReasonNodeRemoved = "node_removed"

// heldSince is the time from which an unconfirmed attempt still holds the
// slot (store.upgradeOutstanding): one that missed its deadline longer than
// UnconfirmedHold ago no longer does. By the deadline the agent has settled
// the upgrade itself: a new binary that does not register within its own
// confirmation window puts the previous one back, and one that was off then
// does so as it starts. What is left is not knowing how it ended, which only
// concerns that node, and that node is upgraded only once it is online again,
// on a confirmed binary. UnconfirmedHold covers clock skew between node and
// server and the legacy download path.
func (s *Service) heldSince() time.Time { return s.now().Add(-s.cfg.UnconfirmedHold) }

// oldVersionSession is a node seen back on a new session at a version other
// than the target, and when.
type oldVersionSession struct {
	session string
	at      time.Time
}

// backOnOldVersion settles an unconfirmed attempt whose node is back on a
// new session at a version other than the target: the upgrade did not take
// effect (it never arrived, or the agent rolled it back). A rolled-back agent
// reports that just after it registers, which settles the attempt with the
// agent's own words (observeEvent), so the node is watched for
// OldVersionSettle before it is settled here, as reconnected_old_version. An
// attempt still in flight is left to its deadline.
func (s *Service) backOnOldVersion(i store.AgentUpgradeItem) error {
	if i.Phase != "unconfirmed" || i.VerifiedAt != nil {
		return nil
	}
	conn, ok := s.cfg.Transport.AgentUpgradeConnection(i.NodeUUID)
	if !ok || !conn.Ready || conn.SessionID == "" {
		return nil
	}
	run, err := s.st.AgentUpgradeRun(i.RunID)
	if err != nil {
		return err
	}
	if conn.Version == run.TargetVersion {
		return nil // observeConnection settles it as target_registered
	}
	if conn.SessionID == i.BaselineSessionID {
		// The agent never restarted: it is the process that was asked to
		// upgrade, still on its old version. Once the attempt no longer
		// holds the slot the agent has given the upgrade up (heldSince), so
		// it did not take effect; without this the attempt would stay open
		// for as long as the node stays connected.
		if i.FinishedAt == nil || !i.FinishedAt.Before(s.heldSince()) {
			return nil
		}
		verified, err := s.st.VerifyAgentUpgrade(i.ID, s.now(), "system", ReasonStillOnOldVersion, "")
		if err != nil || !verified {
			return err
		}
		s.notify()
		s.cfg.Logger.Info("agent upgrade attempt settled: its node never left its old version", "run", i.RunID, "node", i.NodeUUID, "version", conn.Version)
		return nil
	}
	if s.oldVersion == nil {
		s.oldVersion = map[uint]oldVersionSession{}
	}
	seen, ok := s.oldVersion[i.ID]
	if !ok || seen.session != conn.SessionID {
		s.oldVersion[i.ID] = oldVersionSession{session: conn.SessionID, at: s.now()}
		return nil
	}
	if s.now().Sub(seen.at) < s.cfg.OldVersionSettle {
		return nil
	}
	delete(s.oldVersion, i.ID)
	verified, err := s.st.VerifyAgentUpgrade(i.ID, s.now(), "system", ReasonReconnectedOldVersion, "")
	if err != nil || !verified {
		return err
	}
	s.notify()
	s.cfg.Logger.Info("agent upgrade attempt settled: its node is back on its old version", "run", i.RunID, "node", i.NodeUUID, "version", conn.Version)
	return nil
}

// forgetSettled drops what backOnOldVersion watched of attempts no longer
// unconfirmed and unverified (settled since, by any path).
func (s *Service) forgetSettled(items []store.AgentUpgradeItem) {
	open := map[uint]bool{}
	for _, i := range items {
		if i.Phase == "unconfirmed" && i.VerifiedAt == nil {
			open[i.ID] = true
		}
	}
	for id := range s.oldVersion {
		if !open[id] {
			delete(s.oldVersion, id)
		}
	}
}

// ReasonReconnectedOldVersion is the verification code of an unconfirmed
// attempt whose node came back on a version other than the target.
const ReasonReconnectedOldVersion = "reconnected_old_version"

// ReasonStillOnOldVersion is the verification code of an unconfirmed attempt
// whose node stayed connected, on the session and version it had, past the
// hold: the upgrade never took effect.
const ReasonStillOnOldVersion = "still_old_version"
