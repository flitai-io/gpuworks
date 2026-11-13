package hub

import (
	"context"
	"errors"
	"time"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type serverDialReconnectAttempt struct {
	done  chan struct{}
	err   error
	reset bool
}

// ReconnectServerDial bypasses backoff for an operator's request, joining an
// existing attempt for this node instead of replacing its session twice.
func (h *Hub) ReconnectServerDial(ctx context.Context, uuid string) error {
	node, err := h.store.NodeByUUID(uuid)
	if err != nil {
		return err
	}
	if node.ConnectMode != agentconf.ServerDial || node.ServerDialAddress == "" {
		return errors.New("node has no server_dial endpoint")
	}
	return h.reconnectServerDialNode(ctx, node, true)
}

func (h *Hub) reconnectServerDialNode(ctx context.Context, node *store.Node, force bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h.reconnectMu.Lock()
	if attempt := h.reconnectAttempts[node.UUID]; attempt != nil {
		h.reconnectMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-attempt.done:
			return attempt.err
		}
	}
	if h.Online(node.UUID) {
		delete(h.reconnect, node.UUID)
		h.reconnectMu.Unlock()
		return nil
	}
	state := h.reconnect[node.UUID]
	if !force && !state.next.IsZero() && time.Now().Before(state.next) {
		h.reconnectMu.Unlock()
		return nil
	}
	secret, ok := node.PlaintextToken()
	if !ok {
		h.reconnectMu.Unlock()
		h.log.Warn("server_dial reconnect skipped: plaintext token unavailable", "node", node.UUID, "state", node.State)
		return errors.New("server_dial reconnect requires a recoverable node token")
	}
	attempt := &serverDialReconnectAttempt{done: make(chan struct{})}
	if h.reconnectAttempts == nil {
		h.reconnectAttempts = make(map[string]*serverDialReconnectAttempt)
	}
	h.reconnectAttempts[node.UUID] = attempt
	h.reconnectMu.Unlock()

	err := h.dialAgentWithSecret(ctx, node.ServerDialAddress, secret, 5*time.Second)
	if err != nil && ctx.Err() != nil {
		err = ctx.Err()
	}

	h.reconnectMu.Lock()
	if !attempt.reset {
		if err == nil {
			delete(h.reconnect, node.UUID)
		} else {
			state.failures++
			state.next = time.Now().Add(serverDialReconnectDelay(state.failures))
			h.reconnect[node.UUID] = state
		}
	}
	attempt.err = err
	delete(h.reconnectAttempts, node.UUID)
	close(attempt.done)
	h.reconnectMu.Unlock()
	if err != nil {
		h.log.Debug("server_dial reconnect failed", "node", node.UUID, "addr", node.ServerDialAddress, "err", err)
	} else {
		h.log.Info("server_dial reconnected", "node", node.UUID, "addr", node.ServerDialAddress)
	}
	return err
}
