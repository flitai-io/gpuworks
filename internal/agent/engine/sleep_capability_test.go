package engine

import (
	"context"
	"log/slog"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// The server admits a deployment for its selected cards. A test's or an
// agent's host prober must not silently replace the instance's sleep protocol.
func TestSleepProtocolDoesNotDependOnHostProber(t *testing.T) {
	for _, prober := range []gpu.Prober{&gpu.AppleProber{}, &gpu.KunlunProber{}, &gpu.SMIProber{}} {
		t.Run(prober.Vendor(), func(t *testing.T) {
			h := newSleepHTTPServer(t)
			var status proto.InstanceStatus
			e := NewWithProber(&headlessTestDriver{}, func(s proto.InstanceStatus) { status = s }, slog.Default(), t.TempDir(), prober)
			spec := proto.InstanceDesiredState{UUID: "protocol", Sleepable: true, APIPort: h.port(t), DesiredSleep: 1}
			e.reconcileSleep(context.Background(), spec)
			if e.sleepLevel[spec.UUID] != 1 || status.SleepLevel != 1 || len(h.calls) != 1 || h.calls[0] != "POST /sleep?level=1&mode=wait" {
				t.Fatalf("protocol short-circuited: status=%+v calls=%v", status, h.calls)
			}
		})
	}
}
