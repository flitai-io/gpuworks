package app

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentmodelsync "gpuworks.ai/gpuworks/internal/agent/modelsync"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestDirectDialFailureNegotiatesRelayOnlyWhenAllowed(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := listener.Addr().String()
	listener.Close()
	n := NewNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), recording.New(t.TempDir()+"/runtime.json"), t.TempDir())
	for _, method := range []string{proto.RPCModelSyncSend, proto.RPCModelSyncProbe, proto.RPCImageSyncSend, proto.RPCImageSyncProbe} {
		t.Run(method, func(t *testing.T) {
			for _, allow := range []bool{false, true} {
				var p any = proto.ModelSyncSendParams{TransferID: "t", SourcePath: "unused", Token: "secret", Fingerprint: strings.Repeat("a", 64), Endpoints: []string{endpoint}, AllowRelay: allow}
				if method == proto.RPCImageSyncSend || method == proto.RPCImageSyncProbe {
					p = proto.ImageSyncSendParams{TransferID: "t", ImageRef: "image", Token: "secret", Fingerprint: strings.Repeat("a", 64), Endpoints: []string{endpoint}, AllowRelay: allow}
				}
				params, _ := json.Marshal(p)
				value, err := n.OnRPC(context.Background(), method, params)
				if !allow {
					if err == nil {
						t.Fatal("legacy caller lost dial error")
					}
					continue
				}
				if err != nil {
					t.Fatalf("relay negotiation: %v", err)
				}
				wire, _ := json.Marshal(value)
				var result struct {
					DirectDialError string `json:"direct_dial_error"`
				}
				if err := json.Unmarshal(wire, &result); err != nil {
					t.Fatal(err)
				}
				if result.DirectDialError == "" {
					t.Fatal("missing structured direct failure")
				}
			}
		})
	}
}

func TestPeerRelayReceiverRestrictsTransfersAndPreservesProtocol(t *testing.T) {
	n := NewNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), recording.New(t.TempDir()+"/runtime.json"), t.TempDir())
	payload := []byte("an authenticated archive")
	got := make(chan []byte, 1)
	receiver, err := agentmodelsync.StartReceiver(context.Background(), agentmodelsync.ReceiveOptions{BindHost: "0.0.0.0", AdvertiseHosts: []string{"203.0.113.250"}, Token: "secret", Receive: func(_ context.Context, r io.Reader) error { data, err := io.ReadAll(r); got <- data; return err }})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Stop()
	active := &activeModelSync{transferID: "owned", receiver: receiver}
	n.modelSyncs = map[string]*activeModelSync{"key": active}
	for _, kind := range []string{"image", "other"} {
		if c, err := n.PeerRelayReceiver(context.Background(), kind, "owned"); err == nil {
			c.Close()
			t.Fatalf("accepted wrong kind %s", kind)
		}
	}
	if c, err := n.PeerRelayReceiver(context.Background(), "model", "missing"); err == nil {
		c.Close()
		t.Fatal("accepted unknown transfer")
	}
	raw, err := n.PeerRelayReceiver(context.Background(), "model", "owned")
	if err != nil {
		t.Fatal(err)
	}
	if c, err := n.PeerRelayReceiver(context.Background(), "model", "owned"); err == nil {
		c.Close()
		t.Fatal("accepted duplicate attachment")
	}
	if err := agentmodelsync.ProbeOnConnWithMagic(context.Background(), raw, "secret", receiver.Fingerprint, "GPUWORKS-MODEL/1"); err != nil {
		t.Fatal(err)
	}
	raw, err = n.PeerRelayReceiver(context.Background(), "model", "owned")
	if err != nil {
		t.Fatal(err)
	}
	result, err := agentmodelsync.SendOnConnWithMagic(context.Background(), raw, "secret", receiver.Fingerprint, "GPUWORKS-MODEL/1", func(w io.Writer) error { _, err := w.Write(payload); return err })
	if err != nil || result.Bytes != int64(len(payload)) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if data := <-got; !bytes.Equal(data, payload) {
		t.Fatalf("archive = %q", data)
	}
	active.canceled.Store(true)
	if c, err := n.PeerRelayReceiver(context.Background(), "model", "owned"); err == nil {
		c.Close()
		t.Fatal("accepted canceled transfer")
	}
}

func TestRelayDoesNotTurnAuthenticationFailureIntoRetry(t *testing.T) {
	n := NewNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), recording.New(t.TempDir()+"/runtime.json"), t.TempDir())
	receiver, err := agentmodelsync.StartReceiver(context.Background(), agentmodelsync.ReceiveOptions{BindHost: "127.0.0.1", Token: "secret", Receive: func(context.Context, io.Reader) error { t.Error("bad authentication reached archive"); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Stop()
	p, _ := json.Marshal(proto.ModelSyncSendParams{SourcePath: "unused", Token: "wrong", Fingerprint: receiver.Fingerprint, Endpoints: receiver.Endpoints, AllowRelay: true})
	if value, err := n.rpcModelSyncSend(context.Background(), p); err == nil {
		t.Fatalf("authentication failure returned retry result: %+v", value)
	}
}

func TestPeerRelaySourceUsesExistingModelAndImageArchives(t *testing.T) {
	for _, kind := range []string{"model", "image"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			n := NewNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), recording.New(t.TempDir()+"/runtime.json"), root)
			if err := os.MkdirAll(filepath.Join(root, "source"), 0755); err != nil {
				t.Fatal(err)
			}
			for name, data := range map[string]string{"config.json": "{}", "model.safetensors": "weights"} {
				if err := os.WriteFile(filepath.Join(root, "source", name), []byte(data), 0644); err != nil {
					t.Fatal(err)
				}
			}
			magic := "GPUWORKS-MODEL/1"
			method := proto.RPCModelSyncSend
			if kind == "image" {
				magic = imageSyncMagic
				method = proto.RPCImageSyncSend
			}
			received := make(chan []byte, 1)
			receiver, err := agentmodelsync.StartReceiver(context.Background(), agentmodelsync.ReceiveOptions{BindHost: "127.0.0.1", Token: "secret", Magic: magic, Receive: func(_ context.Context, r io.Reader) error { data, err := io.ReadAll(r); received <- data; return err }})
			if err != nil {
				t.Fatal(err)
			}
			defer receiver.Stop()
			if kind == "model" {
				n.modelSyncs = map[string]*activeModelSync{"key": {transferID: "owned", receiver: receiver}}
			} else {
				n.imageSyncs = map[string]*activeImageSync{"image": {transferID: "owned", receiver: receiver}}
			}
			raw, err := n.PeerRelayReceiver(context.Background(), kind, "owned")
			if err != nil {
				t.Fatal(err)
			}
			var p any = proto.ModelSyncSendParams{TransferID: "owned", SourcePath: "source", Token: "secret", Fingerprint: receiver.Fingerprint, Endpoints: []string{"unreachable.invalid:1"}, AllowRelay: true}
			if kind == "image" {
				p = proto.ImageSyncSendParams{TransferID: "owned", ImageRef: "image", Token: "secret", Fingerprint: receiver.Fingerprint, Endpoints: []string{"unreachable.invalid:1"}, AllowRelay: true}
			}
			params, _ := json.Marshal(p)
			value, err := n.StreamPeerSend(context.Background(), method, params, raw)
			if err != nil {
				t.Fatal(err)
			}
			archive := <-received
			if kind == "image" {
				if string(archive) != "recording-image" || value.(proto.ImageSyncTransferResult).Bytes != int64(len(archive)) {
					t.Fatalf("image archive=%q result=%+v", archive, value)
				}
				n.imageSyncMu.Lock()
				active := len(n.imageSyncSends)
				n.imageSyncMu.Unlock()
				if active != 0 {
					t.Fatalf("image send ownership leaked: %d", active)
				}
			} else {
				reader := tar.NewReader(bytes.NewReader(archive))
				files := map[string]string{}
				for {
					hdr, err := reader.Next()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					data, err := io.ReadAll(reader)
					if err != nil {
						t.Fatal(err)
					}
					files[hdr.Name] = string(data)
				}
				if files["config.json"] != "{}" || files["model.safetensors"] != "weights" || value.(proto.ModelSyncTransferResult).Bytes != int64(len(archive)) {
					t.Fatalf("model files=%+v result=%+v", files, value)
				}
			}
		})
	}
}

func TestPeerRelaySourceRequiresExplicitAuthorizationAndClosesRaw(t *testing.T) {
	n := NewNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), recording.New(t.TempDir()+"/runtime.json"), t.TempDir())
	for _, method := range []string{proto.RPCModelSyncSend, proto.RPCImageSyncSend, proto.RPCModelSyncProbe, proto.RPCImageSyncProbe, "proxy"} {
		t.Run(method, func(t *testing.T) {
			raw, peer := net.Pipe()
			defer peer.Close()
			if value, err := n.StreamPeerSend(context.Background(), method, json.RawMessage(`{}`), raw); err == nil {
				t.Fatalf("unauthorized source accepted: %+v", value)
			}
			if _, err := peer.Read(make([]byte, 1)); err == nil {
				t.Fatal("rejected relay left raw transport open")
			}
		})
	}
}

func TestPeerRelayReleaseAcknowledgementWaitsForOwnedAttachment(t *testing.T) {
	n := NewNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), recording.New(t.TempDir()+"/runtime.json"), t.TempDir())
	done := make(chan struct{})
	n.modelSyncs = map[string]*activeModelSync{"key": {transferID: "owned", relayDone: done}}
	params, _ := json.Marshal(proto.PeerRelayWaitClosedParams{Kind: "model", TransferID: "owned"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := n.OnRPC(ctx, proto.RPCPeerRelayWaitClosed, params); !errors.Is(err, context.Canceled) {
		t.Fatalf("active attachment returned release ACK: %v", err)
	}
	close(done)
	if _, err := n.OnRPC(context.Background(), proto.RPCPeerRelayWaitClosed, params); err != nil {
		t.Fatal(err)
	}
	params, _ = json.Marshal(proto.PeerRelayWaitClosedParams{Kind: "model", TransferID: "already-removed"})
	if _, err := n.OnRPC(context.Background(), proto.RPCPeerRelayWaitClosed, params); err != nil {
		t.Fatal(err)
	}
}
