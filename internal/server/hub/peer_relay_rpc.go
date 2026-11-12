package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// CallOnDataStream carries an authorized peer sender invocation and TLS bytes
// on data while its final RPC result still returns over the independent ctrl.
// onOpen starts the bridge and must return without waiting for transfer EOF.
func (s *Session) CallOnDataStream(ctx context.Context, kind, method string, params any, onOpen func(net.Conn) error) (json.RawMessage, error) {
	if kind != proto.StreamPeerRelaySend {
		return nil, errors.New("unsupported peer RPC stream kind")
	}
	if s.DataLaneState() != "attached" {
		return nil, ErrDataLaneUnavailable
	}
	id := s.rpcSeq.Add(1)
	ch := make(chan proto.Envelope, 1)
	s.pendMu.Lock()
	s.pending[id] = ch
	s.pendMu.Unlock()
	defer func() { s.pendMu.Lock(); delete(s.pending, id); s.pendMu.Unlock() }()
	raw, err := s.openStream(ctx, proto.StreamHeader{Kind: kind})
	if err != nil {
		return nil, err
	}
	defer raw.Close()
	// Capture the physical lane used by this stream. Looking at s.data later
	// could observe a replacement and strand the call on the dead connection.
	lane, ok := raw.(*laneStream)
	if !ok || lane.ysess == nil {
		return nil, ErrDataLaneUnavailable
	}
	dataClosed := lane.ysess.CloseChan()
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	data, err := json.Marshal(rpcRequest{Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	line, err := json.Marshal(proto.Envelope{T: proto.TRPCReq, ID: id, D: data})
	if err != nil {
		return nil, err
	}
	if len(line) > 64*1024 {
		return nil, errors.New("peer relay RPC exceeds frame limit")
	}
	deadline := s.hub.now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = raw.SetWriteDeadline(deadline)
	_, err = raw.Write(append(line, '\n'))
	_ = raw.SetWriteDeadline(time.Time{})
	if err != nil {
		return nil, err
	}
	if onOpen != nil {
		if err := onOpen(raw); err != nil {
			return nil, err
		}
	}
	select {
	case env := <-ch:
		return peerRelayRPCResult(env)
	case <-dataClosed:
		select {
		case env := <-ch:
			return peerRelayRPCResult(env)
		default:
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, ErrDataLaneUnavailable
		}
	case <-ctx.Done():
		select {
		case env := <-ch:
			return peerRelayRPCResult(env)
		default:
			return nil, ctx.Err()
		}
	case <-s.closed:
		select {
		case env := <-ch:
			return peerRelayRPCResult(env)
		default:
			return nil, errors.New("node session closed: " + s.NodeUUID)
		}
	}
}
func peerRelayRPCResult(env proto.Envelope) (json.RawMessage, error) {
	var resp rpcResponse
	if err := json.Unmarshal(env.D, &resp); err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, &rpcRemoteError{detail: resp.Error}
	}
	return resp.Result, nil
}
