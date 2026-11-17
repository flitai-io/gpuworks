package conn

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// The first status collection asks the container runtime and the accelerator
// CLIs, which on a loaded host takes seconds. It must not run ahead of the
// inbound reader: a node reads as online from registration, and the server's
// first RPC (a model registration, an image sync) was left unanswered until
// the collection finished, past the caller's few-second deadline.
func TestAnRPCIsAnsweredWhileTheFirstStatusReportIsStillBeingCollected(t *testing.T) {
	clientRaw, serverRaw := net.Pipe()
	defer clientRaw.Close()
	defer serverRaw.Close()
	prober := &blockingStatusProber{
		FixedProber: &gpu.FixedProber{VendorName: "test"},
		started:     make(chan struct{}),
		release:     make(chan struct{}),
	}
	c := New(Credentials{NodeUUID: "node-1", NodeToken: "secret"}, nil, &inboundHandlers{}, prober, slog.Default())

	answered := make(chan error, 1)
	go func() {
		answered <- func() error {
			line, err := readLine(serverRaw)
			if err != nil {
				return err
			}
			var challenge proto.ServerDialChallenge
			if err := json.Unmarshal(line, &challenge); err != nil {
				return err
			}
			proof, _ := json.Marshal(proto.ServerDialProof{HMAC: proto.ServerDialHMAC(proto.ServerDialNodeSecret("secret"), challenge.Nonce)})
			if _, err := serverRaw.Write(append(proof, '\n')); err != nil {
				return err
			}
			ys, err := yamux.Server(serverRaw, yamux.DefaultConfig(), nil)
			if err != nil {
				return err
			}
			defer ys.Close()
			stream, err := ys.AcceptStream()
			if err != nil {
				return err
			}
			defer stream.Close()
			decoder := json.NewDecoder(bufio.NewReader(stream))
			var header proto.StreamHeader
			var hello proto.Envelope
			if err := decoder.Decode(&header); err != nil {
				return err
			}
			if err := decoder.Decode(&hello); err != nil {
				return err
			}
			welcome, _ := json.Marshal(proto.Envelope{T: proto.THello, D: json.RawMessage(`{}`)})
			request, _ := json.Marshal(proto.Envelope{T: proto.TRPCReq, ID: 7, D: json.RawMessage(`{"method":"test"}`)})
			if _, err := stream.Write(append(append(welcome, '\n'), append(request, '\n')...)); err != nil {
				return err
			}
			for {
				var env proto.Envelope
				if err := decoder.Decode(&env); err != nil {
					return err
				}
				if env.T == proto.TRPCResp && env.ID == 7 {
					return nil
				}
			}
		}()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- c.RunAccepted(ctx, clientRaw) }()

	select {
	case <-prober.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first status report never started")
	}
	// The collection is now held; the answer has to come regardless.
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("waiting for the RPC answer: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an RPC sent right after registration went unanswered while the first status report was being collected")
	}
	close(prober.release)
	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		_ = clientRaw.Close()
		t.Fatal("RunAccepted did not stop after context cancellation")
	}
}

// ctxRecordingProber holds its first probe until released and records, for
// every probe, whether the context it ran under was already cancelled.
type ctxRecordingProber struct {
	*gpu.FixedProber
	started   chan struct{}
	release   chan struct{}
	cancelled chan bool
}

func (p *ctxRecordingProber) Probe(ctx context.Context) ([]proto.GPUInfo, error) {
	select {
	case <-p.started:
	default:
		close(p.started)
		<-p.release
	}
	p.cancelled <- ctx.Err() != nil
	return p.FixedProber.Probe(ctx)
}

// After a reconnect the old session's status collection may still be
// running when the new session asks for its first report. That report is
// collected as the trailing run, and it must run under the new session's
// context: under the old, cancelled one every probe fails and the server
// hears nothing good until the next periodic report.
func TestATrailingStatusReportRunsUnderTheNewSessionsContext(t *testing.T) {
	prober := &ctxRecordingProber{
		FixedProber: &gpu.FixedProber{VendorName: "test"},
		started:     make(chan struct{}),
		release:     make(chan struct{}),
		cancelled:   make(chan bool, 4),
	}
	c := New(Credentials{NodeUUID: "node-1"}, nil, &inboundHandlers{}, prober, slog.Default())
	c.ctrl = &nopRWC{}

	oldSession, cancelOld := context.WithCancel(context.Background())
	c.requestStatus(oldSession)
	<-prober.started
	cancelOld()
	newSession, cancelNew := context.WithCancel(context.Background())
	defer cancelNew()
	c.requestStatus(newSession)
	close(prober.release)

	if first := <-prober.cancelled; !first {
		t.Fatal("the held collection should have seen the old session cancelled")
	}
	select {
	case cancelled := <-prober.cancelled:
		if cancelled {
			t.Fatal("the new session's report was collected under the old session's cancelled context")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the trailing status report never ran")
	}
	statusIdle(t, c)
}
