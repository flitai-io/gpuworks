package app

import (
	"context"
	"time"
)

// eventRetentionLoop bounds the deployment event timeline. Events are history
// and are deliberately kept when their deployment is deleted, so this is what
// stops the table growing for the life of the installation. The bounds (180
// days / 50 GiB by default) are operator-tunable in settings.
func (a *App) eventRetentionLoop(ctx context.Context) {
	const interval = 6 * time.Hour
	sweep := func() {
		n, err := a.store.SweepEventRetention(ctx, time.Now())
		if err != nil {
			// What it removed before it stopped is kept; the next sweep goes on.
			if ctx.Err() != nil {
				a.log.Info("event retention sweep stopped with the server", "removed", n)
			} else {
				a.log.Warn("event retention sweep failed", "removed", n, "err", err)
			}
			return
		}
		if n > 0 {
			a.log.Info("event retention sweep", "removed", n)
		}
	}
	sweep()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}

func (a *App) staleLoop(ctx context.Context) {
	t := time.NewTicker(a.cfg.StaleCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Nodes with a live session heartbeat within 90s stay online.
			_ = a.store.MarkStaleOffline(ctx, a.cfg.StaleAfter)
		}
	}
}
