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
