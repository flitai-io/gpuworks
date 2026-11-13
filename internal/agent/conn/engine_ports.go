package conn

import (
	"context"

	"gpuworks.ai/gpuworks/internal/agent/netinfo"
	"gpuworks.ai/gpuworks/internal/proto"
)

// setEnginePorts records the engine port range a state sync carries. The
// first status of a connection goes out before the sync arrives, so a changed
// range reports again at once instead of waiting a status interval.
func (c *Conn) setEnginePorts(ctx context.Context, r *proto.PortRange) {
	if c.creds.EnginePorts != nil {
		return // this node allocates from its own range
	}
	if old := c.enginePorts.Swap(r); !samePortRange(old, r) {
		c.requestStatus(ctx)
	}
}

func samePortRange(a, b *proto.PortRange) bool {
	return a == b || (a != nil && b != nil && *a == *b)
}

// enginePortListeners lists the host's listeners inside the engine port range;
// nil until a server supplies one.
func (c *Conn) enginePortListeners() []int {
	r := c.enginePorts.Load()
	if r == nil {
		return nil
	}
	if ports, ok := c.creds.Simulated.EnginePortListeners(*r); ok {
		return ports
	}
	ports, err := netinfo.ListenPorts(*r)
	if err != nil {
		c.log.Debug("engine port listeners incomplete", "err", err)
	}
	return ports
}

// handlerSend is the sender the engine and downloader report through. A
// launch refused because another process holds one of its host ports
// (proto.EnginePortRefused) is followed by a node status at once: the
// server retries such a launch without waiting, and the status carries the
// listeners its next allocation must avoid, which would otherwise wait for
// the next periodic report.
func (c *Conn) handlerSend(ctx context.Context) SendFunc {
	return func(t string, d any) error {
		err := c.send(t, 0, d)
		if err == nil && refusedLaunch(t, d) {
			c.requestStatus(ctx)
		}
		return err
	}
}

func refusedLaunch(t string, d any) bool {
	st, ok := d.(proto.InstanceStatus)
	return ok && t == proto.TStatusInstance && st.State == proto.InstFailed && proto.IsEnginePortRefusal(st.Message)
}
