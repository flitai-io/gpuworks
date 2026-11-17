package hub

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/agentupgrade"
)

// agentUpgradePushTimeout bounds one pushed upgrade: the binary crosses the
// same link the node already uses, which may be a slow public tunnel.
const agentUpgradePushTimeout = 10 * time.Minute

// agentUpgradeAnswerTimeout bounds the wait for the agent's verdict once the
// last byte is sent: it hashes the file and runs it once.
const agentUpgradeAnswerTimeout = 30 * time.Second

// SetAgentBinaryReader wires the agent artifacts served under /dl/.
func (h *Hub) SetAgentBinaryReader(read func(name string) ([]byte, error)) {
	h.agentBinary = read
}

// UpgradeAgent starts an in-place upgrade of a connected agent by pushing the
// binary for its platform (goos, arch) over the node's own connection, so a node that cannot reach
// the server (server_dial) upgrades the same way as any other. It returns
// pushed=false, without error, when the agent predates pushed upgrades or the
// server lacks the artifact; the caller then falls back to the agent.upgrade
// RPC, which makes the agent pull /dl/. The push itself runs in the
// background: a failure lands in UpgradeErrors, and success shows as the node
// reconnecting with the new version.
func (h *Hub) UpgradeAgent(uuid, goos, arch string) (pushed bool, err error) {
	s := h.session(uuid)
	if s == nil {
		return false, errors.New("node not connected: " + uuid)
	}
	if h.agentBinary == nil || strings.TrimSpace(arch) == "" || !slices.Contains(s.Caps(), proto.CapUpgradePush) {
		return false, nil
	}
	name := proto.AgentBinaryName(strings.TrimSpace(goos), strings.TrimSpace(arch))
	binary, err := h.agentBinary(name)
	if err != nil {
		return false, fmt.Errorf("agent binary %s: %w", name, err)
	}
	h.setUpgradeError(uuid, "")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), agentUpgradePushTimeout)
		defer cancel()
		if err := s.pushAgentUpgrade(ctx, name, binary); err != nil {
			h.setUpgradeError(uuid, err.Error())
			h.log.Error("agent upgrade push failed", "node", uuid, "artifact", name, "err", err)
			return
		}
		h.log.Info("agent upgrade pushed", "node", uuid, "artifact", name, "bytes", len(binary))
	}()
	return true, nil
}

// pushAgentUpgrade sends binary on a StreamAgentUpgrade stream and returns
// the agent's verdict. Closing the stream after reading it is the agent's cue
// to install and exec.
func (s *Session) pushAgentUpgrade(ctx context.Context, name string, binary []byte) error {
	sum := sha256.Sum256(binary)
	stream, err := s.openStream(ctx, proto.StreamHeader{
		Kind: proto.StreamAgentUpgrade,
		Dst:  name,
		Ref:  hex.EncodeToString(sum[:]),
		Size: int64(len(binary)),
	})
	if err != nil {
		return fmt.Errorf("open upgrade stream: %w", err)
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()
	// The agent may answer early (busy, unsupported) and stop reading; read
	// the answer concurrently so the write does not block on a full window.
	answer := make(chan proto.AgentUpgradeStatus, 1)
	answerErr := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stream).ReadBytes('\n')
		if err != nil {
			answerErr <- err
			return
		}
		var status proto.AgentUpgradeStatus
		if err := json.Unmarshal(line, &status); err != nil {
			answerErr <- fmt.Errorf("bad upgrade answer %q: %w", strings.TrimSpace(string(line)), err)
			return
		}
		answer <- status
	}()
	writeErr := make(chan error, 1)
	go func() {
		_, err := stream.Write(binary)
		writeErr <- err
	}()
	verdict := func(status proto.AgentUpgradeStatus) error {
		if status.State == "verified" {
			return nil
		}
		if status.State == "failed" {
			detail := strings.TrimSpace(status.Error)
			if detail == "" {
				detail = "agent rejected upgrade"
			}
			return &agentupgrade.RejectedError{Detail: detail, Busy: errors.Is(remoteRPCError(proto.RPCAgentUpgrade, status.Code, detail, nil), proto.ErrAgentUpgradeInProgress)}
		}
		return fmt.Errorf("unrecognized agent upgrade verdict (state %q)", status.State)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case status := <-answer:
		return verdict(status)
	case err := <-answerErr:
		return fmt.Errorf("read upgrade answer: %w", err)
	case err := <-writeErr:
		if err != nil {
			select {
			case status := <-answer: // an early refusal explains the broken write
				return verdict(status)
			default:
			}
			return fmt.Errorf("send agent binary: %w", err)
		}
	}
	timer := time.NewTimer(agentUpgradeAnswerTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case status := <-answer:
		return verdict(status)
	case err := <-answerErr:
		return fmt.Errorf("read upgrade answer: %w", err)
	case <-timer.C:
		return errors.New("agent did not answer the upgrade push")
	}
}
