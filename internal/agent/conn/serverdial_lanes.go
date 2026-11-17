package conn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
)

func (c *Conn) authenticateServerDial(raw net.Conn) (proto.ServerDialProof, error) {
	var proof proto.ServerDialProof
	c.observationFact("connect_mode", "server_dial")
	for _, stage := range []string{"dial", "dns", "connect", "proxy_connect", "tls", "ws_upgrade"} {
		c.observationPhaseState(stage, "not_applicable")
	}
	c.beginConnectionPhase("hello", true)
	if err := raw.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return proof, err
	}
	nonce, err := randomNonce()
	if err != nil {
		return proof, err
	}
	challenge := proto.ServerDialChallenge{NodeUUID: c.creds.NodeUUID, Nonce: nonce, DataLane: c.creds.DataLane}
	if err := writeServerDialJSON(raw, challenge); err != nil {
		return proof, err
	}
	line, err := readLine(raw)
	if err != nil {
		return proof, err
	}
	if err = json.Unmarshal(line, &proof); err != nil {
		return proof, err
	}
	secret, err := c.serverDialSecret()
	if err != nil {
		return proof, err
	}
	if proof.Lane == "" {
		if proof.SessionID != "" || !proto.VerifyServerDialHMAC(secret, nonce, proof.HMAC) {
			return proof, errors.New("invalid server_dial proof")
		}
	} else {
		if !c.creds.DataLane || (proof.Lane != proto.LaneCtrl && proof.Lane != proto.LaneData) || !proto.VerifyServerDialLaneHMAC(secret, nonce, proof) {
			return proof, errors.New("invalid server_dial lane proof")
		}
		if (proof.Lane == proto.LaneCtrl && proof.SessionID != "") || (proof.Lane == proto.LaneData && proof.SessionID == "") {
			return proof, errors.New("invalid server_dial parent session")
		}
		if ackNonce, err := hex.DecodeString(proof.AckNonce); proof.Lane == proto.LaneData && (err != nil || len(ackNonce) != 32) {
			return proof, errors.New("invalid server_dial data lane challenge")
		}
	}
	if err := raw.SetDeadline(time.Time{}); err != nil {
		return proof, err
	}
	c.endConnectionPhase("hello", nil)
	return proof, nil
}

// serverDialSecret is the key both server_dial proofs use: the install token
// before registration, its SHA-256 after (what the server stores).
func (c *Conn) serverDialSecret() (string, error) {
	secret := strings.TrimSpace(c.creds.NodeToken)
	if secret == "" {
		return "", errors.New("server_dial requires node token")
	}
	if c.creds.NodeUUID != "" {
		secret = proto.ServerDialNodeSecret(secret)
	}
	return secret, nil
}

func writeServerDialJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// ServeServerDial accepts both physical lanes at the same fixed listener. A
// second authenticated control connection never replaces the active owner.
func ServeServerDial(ctx context.Context, ln net.Listener, factory func() *Conn, failed func(*Conn, error)) error {
	ctx, cancel := context.WithCancel(ctx)
	var mu sync.Mutex
	var active *Conn
	var parentCtx context.Context
	slots := make(chan struct{}, 32)
	var workers sync.WaitGroup
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	defer func() { cancel(); workers.Wait() }()
	for {
		raw, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = raw.Close()
			continue
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer raw.Close()
			defer func() { <-slots }()
			cancelRaw := context.AfterFunc(ctx, func() { _ = raw.Close() })
			defer cancelRaw()
			c := factory()
			proof, err := c.authenticateServerDial(raw)
			if err != nil {
				c.endConnectionPhase("hello", err)
				if failed != nil {
					failed(c, c.finishConnection(ctx, err))
				}
				return
			}
			mu.Lock()
			if proof.Lane == proto.LaneData {
				parent := active
				dataCtx := parentCtx
				mu.Unlock()
				if parent == nil {
					rejectServerDialData(raw)
					return
				}
				if err := parent.runAcceptedData(dataCtx, raw, proof); err != nil && ctx.Err() == nil && failed != nil {
					failed(c, err)
				}
				return
			}
			if active != nil {
				mu.Unlock()
				return
			}
			controlCtx, cancelControl := context.WithCancel(ctx)
			active = c
			parentCtx = controlCtx
			mu.Unlock()
			err = c.runAcceptedControl(controlCtx, raw, proof)
			cancelControl()
			mu.Lock()
			if active == c {
				active = nil
				parentCtx = nil
			}
			mu.Unlock()
			if err != nil && ctx.Err() == nil && failed != nil {
				failed(c, err)
			}
		}()
	}
}

func rejectServerDialData(raw net.Conn) {
	detail, _ := json.Marshal(proto.Reject{Reason: proto.RejectStaleSession})
	_ = writeServerDialJSON(raw, proto.Envelope{T: proto.TReject, D: detail})
}

func (c *Conn) runAcceptedData(ctx context.Context, raw net.Conn, proof proto.ServerDialProof) error {
	parentID := proof.SessionID
	c.dataMu.Lock()
	matches := ctx.Err() == nil && c.sessionID != "" && c.sessionID == parentID && c.ysess != nil && !c.ysess.IsClosed()
	c.dataMu.Unlock()
	if !matches {
		rejectServerDialData(raw)
		return errors.New("stale server_dial data session")
	}
	secret, err := c.serverDialSecret()
	if err != nil {
		rejectServerDialData(raw)
		return err
	}
	ack, err := json.Marshal(proto.ServerDialDataAck{HMAC: proto.ServerDialDataAckHMAC(secret, proof.AckNonce, parentID)})
	if err != nil {
		return err
	}
	if err := writeServerDialJSON(raw, proto.Envelope{T: proto.TDataAck, D: ack}); err != nil {
		return err
	}
	data, err := yamux.Client(raw, laneYamuxConfig(proto.LaneData, io.Discard), nil)
	if err != nil {
		return err
	}
	stopped := context.AfterFunc(ctx, func() { _ = data.Close() })
	defer stopped()
	c.dataMu.Lock()
	if ctx.Err() != nil || c.sessionID != parentID || c.ysess.IsClosed() {
		c.dataMu.Unlock()
		_ = data.Close()
		return errors.New("server_dial control session closed")
	}
	old := c.dataSess
	c.dataSess = data
	c.dataEver = true
	c.dataMu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	defer c.clearDataSess(data)
	for {
		stream, err := data.AcceptStream()
		if err != nil {
			return nil
		}
		go c.handleStream(ctx, stream)
	}
}

func randomNonce() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
