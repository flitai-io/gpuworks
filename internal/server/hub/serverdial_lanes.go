package hub

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
)

// serverDialAuth is what the server learned and sent while proving itself.
// ackNonce is set only on a data lane: the agent must answer it before the
// lane is attached.
type serverDialAuth struct {
	dualLane    bool
	proofSecret string
	ackNonce    string
}

// serverDialServerAuthLane negotiates a purpose-bound proof only when the
// agent advertises it. Old agents receive the original nonce-only proof.
func (h *Hub) serverDialServerAuthLane(raw net.Conn, secret, lane, sessionID, nodeUUID string) (bool, error) {
	auth, err := h.serverDialAuthenticate(raw, secret, lane, sessionID, nodeUUID)
	return auth.dualLane, err
}

func (h *Hub) serverDialAuthenticate(raw net.Conn, secret, lane, sessionID, nodeUUID string) (serverDialAuth, error) {
	var auth serverDialAuth
	line, err := readLine(raw)
	if err != nil {
		return auth, fmt.Errorf("read server_dial challenge: %w", err)
	}
	var challenge proto.ServerDialChallenge
	if err := json.Unmarshal(line, &challenge); err != nil {
		return auth, fmt.Errorf("decode server_dial challenge: %w", err)
	}
	if strings.TrimSpace(challenge.Nonce) == "" {
		return auth, errors.New("server_dial challenge missing nonce")
	}
	if lane == proto.LaneData && (!challenge.DataLane || challenge.NodeUUID != nodeUUID || nodeUUID == "" || sessionID == "") {
		return auth, errors.New("server_dial data challenge does not match control identity")
	}
	// The legacy proof signs a bare nonce. Restrict it to the actual agent's
	// random format so this endpoint cannot be used to sign a lane message via
	// a crafted legacy challenge.
	nonceBytes, nonceErr := hex.DecodeString(challenge.Nonce)
	if nonceErr != nil || len(nonceBytes) != 32 {
		return auth, errors.New("server_dial challenge invalid nonce")
	}
	proofSecret := secret
	if n, err := h.store.NodeByUUID(challenge.NodeUUID); err == nil && n.Token != "" && hmac.Equal([]byte(secret), []byte(n.Token)) {
		proofSecret = proto.ServerDialNodeSecret(secret)
	}
	proof := proto.ServerDialProof{HMAC: proto.ServerDialHMAC(proofSecret, challenge.Nonce)}
	if challenge.DataLane {
		proof.Lane = lane
		proof.SessionID = sessionID
		proof.HMAC = proto.ServerDialLaneHMAC(proofSecret, challenge.Nonce, lane, sessionID)
	}
	if lane == proto.LaneData {
		nonce := make([]byte, 32)
		if _, err := rand.Read(nonce); err != nil {
			return auth, err
		}
		proof.AckNonce = hex.EncodeToString(nonce)
	}
	response, err := json.Marshal(proof)
	if err != nil {
		return auth, err
	}
	if _, err := raw.Write(append(response, '\n')); err != nil {
		return auth, fmt.Errorf("write server_dial proof: %w", err)
	}
	return serverDialAuth{dualLane: challenge.DataLane, proofSecret: proofSecret, ackNonce: proof.AckNonce}, nil
}

// runServerDialDataLane belongs to the established ctrl session, not the
// short-lived HTTP/onboarding request that created it.
func (h *Hub) runServerDialDataLane(s *Session, addr, initialSecret string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-s.closed:
			cancel()
		case <-ctx.Done():
		}
	}()
	delay := 200 * time.Millisecond
	for ctx.Err() == nil {
		secret := initialSecret
		if node, err := h.store.NodeByUUID(s.NodeUUID); err == nil {
			if token, ok := node.PlaintextToken(); ok {
				secret = token
			}
		}
		attached, err := h.dialServerDialDataLane(ctx, s, addr, secret)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			h.log.Debug("server_dial data lane ended", "node", s.NodeUUID, "err", err)
		}
		if attached {
			delay = 200 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if !attached {
			delay = min(delay*2, 10*time.Second)
		}
	}
}

func (h *Hub) dialServerDialDataLane(ctx context.Context, s *Session, addr, secret string) (bool, error) {
	dialer := net.Dialer{Timeout: 10 * time.Second}
	raw, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false, err
	}
	defer raw.Close()
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()
	if err := raw.SetDeadline(h.now().Add(15 * time.Second)); err != nil {
		return false, err
	}
	auth, err := h.serverDialAuthenticate(raw, secret, proto.LaneData, s.sessionID, s.NodeUUID)
	if err != nil {
		return false, err
	}
	line, err := readLine(raw)
	if err != nil {
		return false, err
	}
	var ack proto.Envelope
	if err := json.Unmarshal(line, &ack); err != nil {
		return false, err
	}
	if ack.T != proto.TDataAck {
		return false, errors.New("server_dial data parent rejected")
	}
	// Whoever answers this address must prove the node's secret too, as the
	// control lane's node token does; otherwise any host reachable at the
	// stored address could take the session's data traffic.
	var agentProof proto.ServerDialDataAck
	if len(ack.D) == 0 || json.Unmarshal(ack.D, &agentProof) != nil ||
		!proto.VerifyServerDialDataAck(auth.proofSecret, auth.ackNonce, s.sessionID, agentProof.HMAC) {
		return false, errors.New("server_dial data lane agent proof invalid")
	}
	if err := raw.SetDeadline(time.Time{}); err != nil {
		return false, err
	}
	data, err := yamux.Server(raw, laneYamuxConfig(proto.LaneData, &slogWriter{log: h.log}), nil)
	if err != nil {
		return false, err
	}
	defer data.Close()
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	s.attachData(data)
	select {
	case <-ctx.Done():
		return true, ctx.Err()
	case <-data.CloseChan():
		return true, nil
	}
}
