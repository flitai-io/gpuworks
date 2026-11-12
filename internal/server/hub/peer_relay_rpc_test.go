package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestPeerRelayRPCRequiresIndependentDataLane(t *testing.T) {
	ctrl, _ := yamuxPairData(t)
	pc, _ := net.Pipe()
	t.Cleanup(func() { pc.Close() })
	s := newSession(newTestHub(t), ctrl, pc)
	_, err := s.CallOnDataStream(context.Background(), proto.StreamPeerRelaySend, proto.RPCModelSyncProbe, nil, nil)
	if !errors.Is(err, ErrDataLaneUnavailable) {
		t.Fatalf("single lane error = %v", err)
	}
}

func TestPeerRelayRPCUsesControlResultAndCleansPending(t *testing.T) {
	for _, cancelCall := range []bool{false, true} {
		t.Run(map[bool]string{false: "result", true: "cancel"}[cancelCall], func(t *testing.T) {
			ctrl, _ := yamuxPairData(t)
			pc, other := net.Pipe()
			t.Cleanup(func() { pc.Close(); other.Close() })
			s := newSession(newTestHub(t), ctrl, pc)
			data, peer := yamuxPairData(t)
			s.attachData(data)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			peerDone := make(chan error, 1)
			go func() {
				stream, err := peer.AcceptStream()
				if err != nil {
					peerDone <- err
					return
				}
				defer stream.Close()
				reader := bufio.NewReader(stream)
				line, err := reader.ReadBytes('\n')
				if err != nil {
					peerDone <- err
					return
				}
				var hdr proto.StreamHeader
				if err = json.Unmarshal(line, &hdr); err != nil || hdr.Kind != proto.StreamPeerRelaySend {
					peerDone <- errors.New("wrong stream header")
					return
				}
				line, err = reader.ReadBytes('\n')
				if err != nil {
					peerDone <- err
					return
				}
				var env proto.Envelope
				if err = json.Unmarshal(line, &env); err != nil || env.ID == 0 || env.T != proto.TRPCReq {
					peerDone <- errors.New("wrong envelope")
					return
				}
				if cancelCall {
					cancel()
				} else {
					raw, _ := json.Marshal(rpcResponse{Result: json.RawMessage(`{"bytes":12}`)})
					s.dispatchRPCResp(proto.Envelope{T: proto.TRPCResp, ID: env.ID, D: raw})
				}
				_, err = reader.ReadByte()
				if err == nil {
					peerDone <- errors.New("stream left open")
					return
				}
				peerDone <- nil
			}()
			value, err := s.CallOnDataStream(ctx, proto.StreamPeerRelaySend, proto.RPCModelSyncSend, map[string]bool{"allow_relay": true}, func(net.Conn) error { return nil })
			if cancelCall {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel error=%v", err)
				}
			} else if err != nil || string(value) != `{"bytes":12}` {
				t.Fatalf("value=%s err=%v", value, err)
			}
			if err := <-peerDone; err != nil {
				t.Fatal(err)
			}
			s.pendMu.Lock()
			count := len(s.pending)
			s.pendMu.Unlock()
			if count != 0 {
				t.Fatalf("pending calls=%d", count)
			}
		})
	}
}

func TestPeerRelayRPCDataLossReturnsWithoutProducerResult(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "closed", true: "replaced"}[replacement], func(t *testing.T) {
			ctrl, _ := yamuxPairData(t)
			pc, other := net.Pipe()
			t.Cleanup(func() { pc.Close(); other.Close() })
			s := newSession(newTestHub(t), ctrl, pc)
			data, peer := yamuxPairData(t)
			s.attachData(data)
			next, _ := yamuxPairData(t)
			received := make(chan error, 1)
			go func() {
				stream, err := peer.AcceptStream()
				if err != nil {
					received <- err
					return
				}
				defer stream.Close()
				reader := bufio.NewReader(stream)
				if _, err = reader.ReadBytes('\n'); err == nil {
					_, err = reader.ReadBytes('\n')
				}
				received <- err
				// The producer supplies no ctrl result, even after physical data death.
				<-peer.CloseChan()
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := s.CallOnDataStream(ctx, proto.StreamPeerRelaySend, proto.RPCImageSyncSend, nil, func(net.Conn) error {
					if err := <-received; err != nil {
						return err
					}
					_ = data.Close()
					if replacement {
						s.attachData(next)
					}
					return nil
				})
				result <- err
			}()
			select {
			case err := <-result:
				if !errors.Is(err, ErrDataLaneUnavailable) {
					t.Fatalf("data loss error=%v", err)
				}
			case <-time.After(time.Second):
				cancel()
				<-result
				t.Fatal("server waits for blocked producer after physical data loss")
			}
			if ctrl.IsClosed() {
				t.Fatal("data loss closed control")
			}
			s.pendMu.Lock()
			pending := len(s.pending)
			s.pendMu.Unlock()
			if pending != 0 {
				t.Fatalf("pending calls=%d", pending)
			}
		})
	}
}

func TestPeerRelayRPCControlConfirmationWinsDataDeath(t *testing.T) {
	ctrl, _ := yamuxPairData(t)
	pc, other := net.Pipe()
	t.Cleanup(func() { pc.Close(); other.Close() })
	s := newSession(newTestHub(t), ctrl, pc)
	data, peer := yamuxPairData(t)
	s.attachData(data)
	confirmed := make(chan error, 1)
	go func() {
		stream, err := peer.AcceptStream()
		if err != nil {
			confirmed <- err
			return
		}
		defer stream.Close()
		reader := bufio.NewReader(stream)
		if _, err = reader.ReadBytes('\n'); err != nil {
			confirmed <- err
			return
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			confirmed <- err
			return
		}
		var env proto.Envelope
		if err = json.Unmarshal(line, &env); err != nil {
			confirmed <- err
			return
		}
		response, _ := json.Marshal(rpcResponse{Result: json.RawMessage(`{"bytes":12}`)})
		s.dispatchRPCResp(proto.Envelope{T: proto.TRPCResp, ID: env.ID, D: response})
		confirmed <- nil
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := s.CallOnDataStream(ctx, proto.StreamPeerRelaySend, proto.RPCImageSyncSend, nil, func(net.Conn) error {
		if err := <-confirmed; err != nil {
			return err
		}
		_ = data.Close()
		return nil
	})
	if err != nil || string(result) != `{"bytes":12}` {
		t.Fatalf("confirmed result=%s err=%v", result, err)
	}
}

func TestPeerRelayRPCStreamEOFWaitsForControlConfirmation(t *testing.T) {
	ctrl, _ := yamuxPairData(t)
	pc, other := net.Pipe()
	t.Cleanup(func() { pc.Close(); other.Close() })
	s := newSession(newTestHub(t), ctrl, pc)
	data, peer := yamuxPairData(t)
	s.attachData(data)
	permit := make(chan struct{}, 1)
	defer close(permit)
	eofObserved := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		stream, err := peer.AcceptStream()
		if err != nil {
			return
		}
		reader := bufio.NewReader(stream)
		if _, err = reader.ReadBytes('\n'); err != nil {
			return
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var env proto.Envelope
		if json.Unmarshal(line, &env) != nil {
			return
		}
		_ = stream.Close() // StreamPeerSend closes raw before replying over ctrl.
		select {
		case <-permit:
		case <-ctx.Done():
			return
		}
		response, _ := json.Marshal(rpcResponse{Result: json.RawMessage(`{"bytes":12}`)})
		s.dispatchRPCResp(proto.Envelope{T: proto.TRPCResp, ID: env.ID, D: response})
	}()
	type callResult struct {
		raw json.RawMessage
		err error
	}
	result := make(chan callResult, 1)
	go func() {
		raw, err := s.CallOnDataStream(ctx, proto.StreamPeerRelaySend, proto.RPCImageSyncSend, nil, func(raw net.Conn) error {
			_, err := raw.Read(make([]byte, 1))
			if err != io.EOF {
				return err
			}
			close(eofObserved)
			return nil
		})
		result <- callResult{raw, err}
	}()
	select {
	case <-eofObserved:
	case <-ctx.Done():
		t.Fatal("normal stream EOF not observed")
	}
	select {
	case got := <-result:
		t.Fatalf("stream EOF completed call before ctrl confirmation: %s %v", got.raw, got.err)
	case <-time.After(20 * time.Millisecond):
	}
	permit <- struct{}{}
	select {
	case got := <-result:
		if got.err != nil || string(got.raw) != `{"bytes":12}` {
			t.Fatalf("confirmed result=%s err=%v", got.raw, got.err)
		}
	case <-ctx.Done():
		t.Fatal("ctrl confirmation did not complete call")
	}
}
