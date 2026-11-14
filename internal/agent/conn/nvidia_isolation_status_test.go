package conn

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

type nvidiaIsolationHandlers struct {
	inboundHandlers
	isolation *proto.NVIDIAIsolation
}

func (h *nvidiaIsolationHandlers) NVIDIAIsolation(context.Context) *proto.NVIDIAIsolation {
	return h.isolation
}

// Every status report states the isolation afresh: a soft node carries it as
// literal wire JSON, a strict node (or an agent without the hook) carries
// nothing, which is how the server learns it stopped being soft.
func TestNodeStatusCarriesTheNVIDIAIsolationWhileItIsSoft(t *testing.T) {
	soft := nodeStatusWire(t, &nvidiaIsolationHandlers{isolation: &proto.NVIDIAIsolation{
		Level: "soft", Reason: "cdi_names_missing", Toolkit: "gpu_operator", Cards: 2,
	}})
	if got, want := string(soft["nvidia_isolation"]), `{"level":"soft","reason":"cdi_names_missing","toolkit":"gpu_operator","cards":2}`; got != want {
		t.Fatalf("nvidia_isolation = %s, want %s", got, want)
	}
	for name, h := range map[string]Handlers{
		"strict node":              &nvidiaIsolationHandlers{},
		"handler without the hook": &inboundHandlers{},
	} {
		if raw, present := nodeStatusWire(t, h)["nvidia_isolation"]; present {
			t.Errorf("%s reported nvidia_isolation %s", name, raw)
		}
	}
}

type ucxNetlinkHandlers struct {
	inboundHandlers
	hang *proto.UCXNetlinkHang
}

func (h *ucxNetlinkHandlers) UCXNetlinkHang(context.Context) *proto.UCXNetlinkHang { return h.hang }

// An affected host carries the fact as literal wire JSON on every status; an
// unaffected one (or an agent without the hook) carries nothing, which is how
// the server learns the sysctl was fixed.
func TestNodeStatusCarriesTheUCXNetlinkHangWhileTheHostIsAffected(t *testing.T) {
	affected := nodeStatusWire(t, &ucxNetlinkHandlers{hang: &proto.UCXNetlinkHang{Kernel: "5.15.0-203-generic"}})
	if got, want := string(affected["ucx_netlink_hang"]), `{"kernel":"5.15.0-203-generic"}`; got != want {
		t.Fatalf("ucx_netlink_hang = %s, want %s", got, want)
	}
	for name, h := range map[string]Handlers{
		"unaffected host":          &ucxNetlinkHandlers{},
		"handler without the hook": &inboundHandlers{},
	} {
		if raw, present := nodeStatusWire(t, h)["ucx_netlink_hang"]; present {
			t.Errorf("%s reported ucx_netlink_hang %s", name, raw)
		}
	}
}
