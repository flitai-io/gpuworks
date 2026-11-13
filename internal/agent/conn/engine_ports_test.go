package conn

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// sentStatuses decodes the status.node envelopes a Conn wrote so far.
func sentStatuses(t *testing.T, ctrl *nopRWC) []proto.NodeStatus {
	t.Helper()
	var out []proto.NodeStatus
	sc := bufio.NewScanner(bytes.NewReader(ctrl.BytesCopy()))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var env proto.Envelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil || env.T != proto.TStatusNode {
			continue
		}
		var st proto.NodeStatus
		if err := json.Unmarshal(env.D, &st); err != nil {
			t.Fatalf("status.node body %s: %v", env.D, err)
		}
		out = append(out, st)
	}
	return out
}

// statusIdle waits until no status collection is running or queued.
func statusIdle(t *testing.T, c *Conn) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.statusMu.Lock()
		running := c.statusRunning
		c.statusMu.Unlock()
		if !running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("status collection did not finish")
}

func syncEnvelope(t *testing.T, ports *proto.PortRange) proto.Envelope {
	t.Helper()
	raw, err := json.Marshal(proto.DesiredState{Network: proto.NetworkConfig{EnginePorts: ports}})
	if err != nil {
		t.Fatal(err)
	}
	return proto.Envelope{T: proto.TStateSync, D: raw}
}

func TestStateSyncReportsStatusOnlyWhenEnginePortRangeChanges(t *testing.T) {
	ctrl := &nopRWC{}
	c := New(Credentials{NodeUUID: "node-1"}, nil, &inboundHandlers{}, &gpu.FixedProber{VendorName: "test"}, slog.Default())
	c.ctrl = ctrl
	first := &proto.PortRange{Start: 30000, End: 30010}
	steps := []struct {
		name  string
		ports *proto.PortRange
		want  int // status reports sent so far
	}{
		{"first range", first, 1},
		{"same range again", &proto.PortRange{Start: 30000, End: 30010}, 1},
		{"range changed", &proto.PortRange{Start: 31000, End: 31010}, 2},
		{"server stopped sending a range", nil, 3},
		{"still none", nil, 3},
	}
	for _, step := range steps {
		c.handleInbound(syncEnvelope(t, step.ports))
		statusIdle(t, c)
		if got := len(sentStatuses(t, ctrl)); got != step.want {
			t.Fatalf("%s: status reports = %d, want %d", step.name, got, step.want)
		}
	}
	if got := c.enginePorts.Load(); got != nil {
		t.Fatalf("engine ports after a sync without one = %+v, want nil", got)
	}
}

func TestStatusOmitsEnginePortListenersWithoutARange(t *testing.T) {
	ctrl := &nopRWC{}
	c := New(Credentials{NodeUUID: "node-1"}, nil, &inboundHandlers{}, &gpu.FixedProber{VendorName: "test"}, slog.Default())
	c.ctrl = ctrl
	c.reportStatus(context.Background())
	statuses := sentStatuses(t, ctrl)
	if len(statuses) != 1 || statuses[0].Network == nil {
		t.Fatalf("statuses = %+v, want one with a network inventory", statuses)
	}
	if got := statuses[0].Network.EnginePortListeners; got != nil {
		t.Fatalf("listeners before any range = %v, want none", got)
	}
	if bytes.Contains(ctrl.BytesCopy(), []byte("engine_port_listeners")) {
		t.Fatalf("status without a range carries the listeners key: %s", ctrl.BytesCopy())
	}
}

func TestNetworkUpdateRefreshesEnginePortInventory(t *testing.T) {
	ctrl := &nopRWC{}
	c := New(Credentials{NodeUUID: "node-1"}, nil, &inboundHandlers{}, &gpu.FixedProber{VendorName: "test"}, slog.Default())
	c.ctrl = ctrl
	for _, step := range []struct {
		wire string
		want int
	}{
		{`{"network":{"engine_ports":{"start":30000,"end":30010}}}`, 1},
		{`{"network":{"engine_ports":{"start":30000,"end":30010}}}`, 1},
		{`{}`, 1},
		{`{"network":{"engine_ports":{"start":31000,"end":31010}}}`, 2},
		{`{"network":{}}`, 3},
		{`{"network":{}}`, 3},
	} {
		c.handleInbound(proto.Envelope{T: proto.TStateUpdate, D: json.RawMessage(step.wire)})
		statusIdle(t, c)
		if got := len(sentStatuses(t, ctrl)); got != step.want {
			t.Fatalf("%s: status reports=%d, want %d", step.wire, got, step.want)
		}
	}
}

// A launch refused for a host port another process holds is followed by a
// node status at once, carrying the listeners the server's retry must avoid;
// any other instance report leaves the node status to its schedule.
func TestARefusedLaunchReportsTheNodeStatusAtOnce(t *testing.T) {
	cases := []struct {
		name       string
		t          string
		st         proto.InstanceStatus
		wantStatus bool
	}{
		{"refused", proto.TStatusInstance, proto.InstanceStatus{UUID: "u", State: proto.InstFailed, Message: proto.EnginePortRefused + ": port 30000 is already in use on this node; nothing was started"}, true},
		{"foreign engine on the API port", proto.TStatusInstance, proto.InstanceStatus{UUID: "u", State: proto.InstFailed, Message: proto.EnginePortInUse + ": another engine answers on this port"}, false},
		{"other failure", proto.TStatusInstance, proto.InstanceStatus{UUID: "u", State: proto.InstFailed, Message: "image pull failed"}, false},
		{"refusal text on a running report", proto.TStatusInstance, proto.InstanceStatus{UUID: "u", State: proto.InstRunning, Message: proto.EnginePortRefused + ": stale"}, false},
		{"another envelope", proto.TStatusModelFile, proto.InstanceStatus{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := &nopRWC{}
			c := New(Credentials{NodeUUID: "node-1"}, nil, &inboundHandlers{}, &gpu.FixedProber{VendorName: "test"}, slog.Default())
			c.ctrl = ctrl
			c.enginePorts.Store(&proto.PortRange{Start: 30000, End: 30010})
			var d any = tc.st
			if tc.t == proto.TStatusModelFile {
				d = proto.ModelFileStatus{Key: "k", State: proto.MFFailed, Message: proto.EnginePortRefused + ": x"}
			}
			if err := c.handlerSend(context.Background())(tc.t, d); err != nil {
				t.Fatal(err)
			}
			statusIdle(t, c)
			statuses := sentStatuses(t, ctrl)
			if got := len(statuses) == 1; got != tc.wantStatus {
				t.Fatalf("node statuses sent = %d, want a status: %v", len(statuses), tc.wantStatus)
			}
			if tc.wantStatus && statuses[0].Network == nil {
				t.Fatalf("status after a refusal has no network inventory: %+v", statuses[0])
			}
			if !bytes.Contains(ctrl.BytesCopy(), []byte(`"t":"`+tc.t+`"`)) {
				t.Fatalf("the report itself was not sent: %s", ctrl.BytesCopy())
			}
		})
	}
}

func TestOwnEnginePortRangeOverridesTheServersAndIsReported(t *testing.T) {
	ctrl := &nopRWC{}
	own := &proto.PortRange{Start: 40000, End: 42767}
	c := New(Credentials{NodeUUID: "node-1", EnginePorts: own}, nil, &inboundHandlers{}, &gpu.FixedProber{VendorName: "test"}, slog.Default())
	c.ctrl = ctrl
	c.handleInbound(syncEnvelope(t, &proto.PortRange{Start: 30000, End: 32767}))
	statusIdle(t, c)
	if got := c.enginePorts.Load(); got == nil || *got != *own {
		t.Fatalf("engine ports after a server sync = %+v, want the node's own %+v", got, own)
	}
	c.reportStatus(context.Background())
	statuses := sentStatuses(t, ctrl)
	if len(statuses) == 0 || statuses[len(statuses)-1].Network == nil {
		t.Fatalf("statuses = %+v, want a network inventory", statuses)
	}
	if got := statuses[len(statuses)-1].Network.EnginePortRange; got == nil || *got != *own {
		t.Fatalf("reported engine_port_range = %+v, want %+v", got, own)
	}
}
