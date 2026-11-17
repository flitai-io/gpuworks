package conn

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

type kubeSettingsHandlers struct {
	inboundHandlers
	got []proto.KubeSettings
}

func (h *kubeSettingsHandlers) OnKubeSettings(_ context.Context, s proto.KubeSettings) {
	h.got = append(h.got, s)
}

// kube.settings reaches a handler that takes them, exactly as the server's
// wire form says; a bad body is dropped, and a handler that does not take
// them (an agent build without Kubernetes support) ignores the message.
func TestHandleInboundDeliversKubeSettings(t *testing.T) {
	h := &kubeSettingsHandlers{}
	c := &Conn{handlers: h, log: slog.New(slog.DiscardHandler)}
	c.handleInbound(proto.Envelope{T: proto.TKubeSettings, D: json.RawMessage(`{"idle_return_seconds":-1,"runtime_class":"nvidia"}`)})
	c.handleInbound(proto.Envelope{T: proto.TKubeSettings, D: json.RawMessage(`{"idle_return_seconds":"soon"}`)})
	c.handleInbound(proto.Envelope{T: proto.TKubeSettings, D: json.RawMessage(`{}`)})
	want := []proto.KubeSettings{{IdleReturnSeconds: -1, RuntimeClass: "nvidia"}, {}}
	if len(h.got) != len(want) || h.got[0] != want[0] || h.got[1] != want[1] {
		t.Fatalf("settings delivered = %+v, want %+v", h.got, want)
	}

	plain := &Conn{handlers: &inboundHandlers{}, log: slog.New(slog.DiscardHandler)}
	plain.handleInbound(proto.Envelope{T: proto.TKubeSettings, D: json.RawMessage(`{"idle_return_seconds":5}`)})
}

func TestKubeSettingsWireForm(t *testing.T) {
	raw, err := json.Marshal(proto.KubeSettings{IdleReturnSeconds: 60})
	if err != nil || string(raw) != `{"idle_return_seconds":60,"runtime_class":""}` {
		t.Fatalf("wire = %s %v", raw, err)
	}
	if proto.TKubeSettings != "kube.settings" {
		t.Fatalf("message type = %q", proto.TKubeSettings)
	}
}
