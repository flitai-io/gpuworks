package hub

import (
	"errors"

	"gpuworks.ai/gpuworks/internal/proto"
)

// PushUpdate sends an incremental desired-state update, or buffers it while
// the session's first full replay is still building (see pushInitialState).
func (s *Session) PushUpdate(su proto.StateUpdate) error {
	// Before the session's first full replay the agent has no baseline to
	// apply an incremental update to; buffer it so the replay lands first.
	if !s.initialPushFinished() {
		s.stateMu.Lock()
		if s.initialPushFinished() { // completed while we waited for the lock
			s.stateMu.Unlock()
		} else {
			if len(s.pendingUpdates) >= pendingUpdatesMax {
				s.stateMu.Unlock()
				return errors.New("session initial state replay still pending")
			}
			s.pendingUpdates = append(s.pendingUpdates, su)
			s.stateMu.Unlock()
			return nil
		}
	}
	return s.sendStateUpdate(su)
}

func (s *Session) sendStateUpdate(su proto.StateUpdate) error {
	if err := s.hydrateRuntimeAssetContent(su.Instances); err != nil {
		return err
	}
	s.omitCachedAssetContent(su.Instances)
	return s.send(proto.TStateUpdate, 0, su)
}

// pushInitialState sends the full desired state to a freshly connected agent.
// The agent clears its caches and reconciles against this.
//
// A connect storm builds thousands of these concurrently, each a multi-query
// read that can take seconds behind the store's pool; running it on the
// session goroutine delayed the ctrl read loop (and with it every RPC) until
// the build finished, which is what starved management RPCs at fleet scale
// (2026-10 l2-fleet regression). Callers therefore run it on its own
// goroutine; fullPushMu serializes full pushes per session so a full replay
// can never interleave with another one, and the first completion releases
// the incremental updates buffered by PushUpdate.
func (s *Session) pushInitialState() (err error) {
	s.fullPushMu.Lock()
	defer s.fullPushMu.Unlock()
	// Updates buffered from HERE on may postdate the snapshot: the reads'
	// transactions see the store as of their start, so anything committed
	// while they run can be missing from the snapshot yet already buffered.
	s.stateMu.Lock()
	baseline := len(s.pendingUpdates)
	s.stateMu.Unlock()
	defer func() {
		if err != nil {
			// No snapshot reached the agent, so nothing it would supersede:
			// keep every buffered update.
			baseline = 0
		}
		s.finishInitialPush(baseline)
	}()
	// Replays from a mass (re)connect are capped: see Hub.pushLimiter. A
	// session that dies while waiting for a slot (agents churning through a
	// storm) gives up instead of spending one on a replay nobody reads.
	select {
	case s.hub.pushLimiter <- struct{}{}:
	case <-s.closed:
		return errSessionClosed
	}
	defer func() { <-s.hub.pushLimiter }()
	node, err := s.hub.store.NodeByUUID(s.NodeUUID)
	if err != nil {
		return err
	}
	ds, err := s.hub.store.DesiredStateForNode(node.ID)
	if err != nil {
		return err
	}
	if err := s.hub.planReplay(node, &ds); err != nil {
		return err
	}

	if err := s.completeNetworkConfig(node, &ds.Network); err != nil {
		return err
	}
	if err := s.hydrateRuntimeAssetContent(ds.Instances); err != nil {
		return err
	}
	s.omitCachedAssetContent(ds.Instances)
	if err := s.send(proto.TStateSync, 0, ds); err != nil {
		return err
	}
	// The replay is on the wire: a failure to follow it with the entry's
	// settings must not read as a failed replay (which would keep buffered
	// updates the snapshot already contains). The next connection, or the
	// next edit of the entry, sends them again.
	if s.kubeSettingsSent.CompareAndSwap(false, true) {
		if err := s.pushKubeSettings(node.KubeClusterID); err != nil {
			s.kubeSettingsSent.Store(false)
			if !errors.Is(err, errSessionClosed) {
				s.hub.log.Warn("kubernetes settings push failed", "node", s.NodeUUID, "err", err)
			}
		}
	}
	return nil
}

// errSessionClosed reports a replay abandoned because its session ended first.
var errSessionClosed = errors.New("session closed before its initial replay")

// pendingUpdatesMax bounds the incremental updates buffered while a freshly
// connected session's initial replay is still building. Past it the update is
// refused and the caller's retry path (the controller already has one)
// re-sends once the session is ready; an unbounded buffer would silently
// delay desired state behind a connect storm.
const pendingUpdatesMax = 128

// initialPushConcurrency bounds simultaneous initial replays fleet-wide.
// Sixteen keeps the store responsive for everything else (the status worker
// above all) while a 1000-node connect storm still finishes its replays in
// seconds.
const initialPushConcurrency = 16

func (s *Session) initialPushFinished() bool {
	select {
	case <-s.initialPushDone:
		return true
	default:
		return false
	}
}

// finishInitialPush releases exactly once. The first baseline updates were
// buffered before the replay's store read, so the snapshot already contains
// them and they are dropped — replaying them would walk the agent's state
// backwards. Everything buffered after the read is newer than the snapshot
// and is flushed in submission order once the replay is on the wire.
func (s *Session) finishInitialPush(baseline int) {
	s.stateMu.Lock()
	if s.initialPushFinished() {
		s.stateMu.Unlock()
		return
	}
	close(s.initialPushDone)
	if baseline > len(s.pendingUpdates) {
		baseline = len(s.pendingUpdates)
	}
	dropped := s.pendingUpdates[:baseline]
	newer := s.pendingUpdates[baseline:]
	s.pendingUpdates = nil
	s.stateMu.Unlock()
	if len(dropped) > 0 {
		s.hub.log.Info("discarded state updates superseded by the initial replay",
			"node", s.NodeUUID, "count", len(dropped))
	}
	for _, su := range newer {
		if err := s.sendStateUpdate(su); err != nil {
			s.hub.log.Warn("flush buffered state update", "node", s.NodeUUID, "err", err)
		}
	}
}
