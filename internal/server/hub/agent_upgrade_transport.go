package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/agentupgrade"
)

// SetAgentUpgradeObserver installs the non-blocking upgrade observation sink.
// The sink must enqueue observations rather than do database I/O in this call.
func (h *Hub) SetAgentUpgradeObserver(observe func(agentupgrade.Observation)) {
	h.upgradeMu.Lock()
	h.upgradeObserver = observe
	h.upgradeMu.Unlock()
}

func (h *Hub) observeAgentUpgrade(event agentupgrade.Observation) {
	h.upgradeMu.RLock()
	observe := h.upgradeObserver
	h.upgradeMu.RUnlock()
	if observe != nil {
		observe(event)
	}
}

// AgentUpgradeConnection is a snapshot of the current authenticated control
// session. Its identity works for both dual-lane and legacy connections.
func (h *Hub) AgentUpgradeConnection(uuid string) (agentupgrade.Connection, bool) {
	s := h.session(uuid)
	if s == nil || s.upgradeSessionID == "" || s.upgradeConnectionClosed() {
		return agentupgrade.Connection{}, false
	}
	return agentupgrade.Connection{SessionID: s.upgradeSessionID, Version: s.upgradeVersion, Caps: s.Caps(), Ready: s.upgradeReady.Load()}, true
}

func (h *Hub) agentUpgradeSession(uuid, sessionID string) (*Session, error) {
	s := h.session(uuid)
	if s == nil || s.upgradeConnectionClosed() {
		return nil, fmt.Errorf("node not connected: %s", uuid)
	}
	if sessionID == "" || s.upgradeSessionID != sessionID {
		return nil, errors.New("agent control session changed before upgrade dispatch")
	}
	return s, nil
}

// PushAgentUpgrade waits only for transfer verification, never for installation
// or a confirmed replacement. The runner observes a new formal session later.
func (h *Hub) PushAgentUpgrade(ctx context.Context, uuid, sessionID, name string, binary []byte) error {
	s, err := h.agentUpgradeSession(uuid, sessionID)
	if err != nil {
		return err
	}
	if !s.HasCap(proto.CapUpgradePush) {
		return errors.New("agent does not support pushed upgrades")
	}
	if len(binary) == 0 {
		return errors.New("agent upgrade binary is empty")
	}
	return s.pushAgentUpgrade(ctx, name, binary)
}

// RequestAgentUpgrade preserves the old Agent RPC and waits for its acceptance.
// In particular it does not route through a newer session after reconnection.
func (h *Hub) RequestAgentUpgrade(ctx context.Context, uuid, sessionID string) error {
	s, err := h.agentUpgradeSession(uuid, sessionID)
	if err != nil {
		return err
	}
	_, err = s.Call(ctx, proto.RPCAgentUpgrade, map[string]any{})
	var refused *rpcRemoteError
	if errors.As(err, &refused) {
		return &agentupgrade.RejectedError{Detail: refused.Error(), Busy: errors.Is(refused, proto.ErrAgentUpgradeInProgress)}
	}
	return err
}

func (s *Session) observeUpgradeControl(env proto.Envelope) {
	if !s.upgradeHandshakeComplete.Load() || s.upgradeReady.Load() || s.upgradeConnectionClosed() || s.hub.session(s.NodeUUID) != s {
		return
	}
	switch env.T {
	case proto.THeartbeat:
	case proto.TStatusNode:
		var report proto.NodeStatus
		if json.Unmarshal(env.D, &report) != nil || strings.TrimSpace(string(env.D)) == "null" {
			return
		}
	default:
		return
	}
	if s.upgradeReady.CompareAndSwap(false, true) {
		s.hub.observeAgentUpgrade(agentupgrade.Observation{NodeUUID: s.NodeUUID, SessionID: s.upgradeSessionID, Version: s.upgradeVersion, State: "registered", At: s.hub.now().UTC()})
	}
}

func (s *Session) observeUpgradeFailure(status proto.AgentUpgradeStatus) {
	if !s.upgradeHandshakeComplete.Load() || s.upgradeSessionID == "" {
		return
	}
	// Preserve the reporting session, including a superseded session. The
	// service uses this identity to keep late failures off a newer attempt.
	s.hub.observeAgentUpgrade(agentupgrade.Observation{NodeUUID: s.NodeUUID, SessionID: s.upgradeSessionID, Version: s.upgradeVersion, State: "failed",
		Error: status.Error, Code: upgradeStatusCode(status), At: s.hub.now().UTC()})
}

func (s *Session) upgradeConnectionClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}
