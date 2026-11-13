package conn

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestServerDialAuthenticationRejectsReplayAndPurposeSwitch(t *testing.T) {
	creds := Credentials{NodeUUID: "node", NodeToken: "secret", ConnectMode: "server_dial", DataLane: true}
	var captured proto.ServerDialProof
	firstNonce, err := serverDialProofExchange(t, creds, func(ch proto.ServerDialChallenge) proto.ServerDialProof {
		captured = proto.ServerDialProof{Lane: proto.LaneData, SessionID: "parent", AckNonce: strings.Repeat("ab", 32), HMAC: proto.ServerDialLaneHMAC(proto.ServerDialNodeSecret("secret"), ch.Nonce, proto.LaneData, "parent")}
		return captured
	})
	if err != nil {
		t.Fatal(err)
	}
	secondNonce, err := serverDialProofExchange(t, creds, func(proto.ServerDialChallenge) proto.ServerDialProof { return captured })
	if firstNonce == secondNonce {
		t.Fatal("challenge nonce reused")
	}
	if err == nil {
		t.Fatal("captured proof replayed on a fresh connection")
	}
	_, err = serverDialProofExchange(t, creds, func(ch proto.ServerDialChallenge) proto.ServerDialProof {
		return proto.ServerDialProof{Lane: proto.LaneData, SessionID: "parent", HMAC: proto.ServerDialLaneHMAC(proto.ServerDialNodeSecret("secret"), ch.Nonce, proto.LaneCtrl, "")}
	})
	if err == nil {
		t.Fatal("control proof relabeled as data")
	}
	// A data lane must carry the server's challenge for the agent's own proof.
	_, err = serverDialProofExchange(t, creds, func(ch proto.ServerDialChallenge) proto.ServerDialProof {
		return proto.ServerDialProof{Lane: proto.LaneData, SessionID: "parent", HMAC: proto.ServerDialLaneHMAC(proto.ServerDialNodeSecret("secret"), ch.Nonce, proto.LaneData, "parent")}
	})
	if err == nil {
		t.Fatal("data proof without an agent challenge accepted")
	}
}

func TestServerDialAuthenticationLegacyNegotiation(t *testing.T) {
	for _, dataEnabled := range []bool{false, true} {
		creds := Credentials{NodeToken: "bootstrap", DataLane: dataEnabled}
		_, err := serverDialProofExchange(t, creds, func(ch proto.ServerDialChallenge) proto.ServerDialProof {
			if ch.DataLane != dataEnabled {
				t.Errorf("data negotiation=%v want %v", ch.DataLane, dataEnabled)
			}
			return proto.ServerDialProof{HMAC: proto.ServerDialHMAC("bootstrap", ch.Nonce)}
		})
		if err != nil {
			t.Fatalf("legacy server rejected (data=%v): %v", dataEnabled, err)
		}
	}
	_, err := serverDialProofExchange(t, Credentials{NodeToken: "bootstrap"}, func(ch proto.ServerDialChallenge) proto.ServerDialProof {
		return proto.ServerDialProof{Lane: proto.LaneData, SessionID: "parent", HMAC: proto.ServerDialLaneHMAC("bootstrap", ch.Nonce, proto.LaneData, "parent")}
	})
	if err == nil {
		t.Fatal("disabled data lane accepted a data proof")
	}
}

func serverDialProofExchange(t *testing.T, creds Credentials, proof func(proto.ServerDialChallenge) proto.ServerDialProof) (string, error) {
	t.Helper()
	agent, server := net.Pipe()
	defer agent.Close()
	defer server.Close()
	c := New(creds, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan error, 1)
	go func() { _, err := c.authenticateServerDial(agent); done <- err }()
	line, err := readLine(server)
	if err != nil {
		t.Fatal(err)
	}
	var ch proto.ServerDialChallenge
	if err := json.Unmarshal(line, &ch); err != nil {
		t.Fatal(err)
	}
	if err := writeServerDialJSON(server, proof(ch)); err != nil {
		t.Fatal(err)
	}
	return ch.Nonce, <-done
}

func TestServerDialListenerCancellationClosesUnauthenticatedSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- ServeServerDial(ctx, listener, func() *Conn { return New(Credentials{NodeToken: "secret"}, nil, nil, nil, slog.Default()) }, nil)
	}()
	raw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := readLine(raw); err != nil {
		t.Fatal(err)
	} // leave the authenticated proof pending
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("listener did not cancel pending authentication")
	}
	_ = raw.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := raw.Read(make([]byte, 1)); err == nil {
		t.Fatal("pending auth socket survived listener cancellation")
	}
}

func TestRunAcceptedCancellationInterruptsAuthentication(t *testing.T) {
	agent, server := net.Pipe()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New(Credentials{NodeToken: "secret"}, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan error, 1)
	go func() { done <- c.RunAccepted(ctx, agent) }()
	if _, err := readLine(server); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt pending reverse authentication")
	}
}
