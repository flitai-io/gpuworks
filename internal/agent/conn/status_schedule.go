package conn

import "context"

// requestStatus keeps at most one status collection running and retains one
// trailing request. Bursty catalog updates therefore cannot create an
// unbounded number of GPU probes or goroutines, while the final ref set is
// still observed after an in-flight collection completes. A trailing
// collection runs under the latest request's context: after a reconnect the
// old session's collection may still be running, and the new session's first
// report must not be collected under the context that reconnect cancelled.
func (c *Conn) requestStatus(ctx context.Context) {
	c.statusMu.Lock()
	c.statusCtx = ctx
	if c.statusRunning {
		c.statusTrailing = true
		c.statusMu.Unlock()
		return
	}
	c.statusRunning = true
	c.statusMu.Unlock()

	go func() {
		for {
			c.reportStatus(ctx)
			c.statusMu.Lock()
			if c.statusTrailing {
				c.statusTrailing = false
				ctx = c.statusCtx
				c.statusMu.Unlock()
				continue
			}
			c.statusRunning = false
			c.statusMu.Unlock()
			return
		}
	}()
}
