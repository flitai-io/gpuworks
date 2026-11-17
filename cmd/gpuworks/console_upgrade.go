package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/version"
)

// consoleUpgradeConfirmTimeout is how long a new binary started by a console
// upgrade has to answer on /healthz as its own release, migrations included.
const consoleUpgradeConfirmTimeout = 15 * time.Minute

// guardConsoleUpgrade runs before the database is opened. When a console
// upgrade is waiting for this start it counts the start; when the new binary
// has used up its starts the previous version and its database come back and
// the process exits for the service manager to start them (ADR 0040).
func guardConsoleUpgrade(cfg app.Config, log *slog.Logger) (*selfinstall.PendingUpgrade, selfinstall.Layout, error) {
	if cfg.ConfigPath == "" {
		return nil, selfinstall.Layout{}, nil
	}
	layout := selfinstall.NewLayout(filepath.Dir(cfg.ConfigPath), cfg.DataDir, "")
	guard, err := selfinstall.GuardStart(layout, version.Version, serviceMode(), time.Now())
	if err != nil {
		log.Error("console upgrade guard", "error", err)
	}
	if d := guard.Discarded; d != nil {
		log.Warn("removed a console upgrade marker left for another release; this binary was installed some other way",
			"marker_to", d.To, "marker_from", d.From, "running", version.Version)
	}
	if guard.RolledBack {
		log.Error("console upgrade did not come up; the previous version and its database are restored",
			"from", guard.Pending.From, "to", guard.Pending.To, "backup", guard.Pending.Backup)
		return nil, layout, fmt.Errorf("upgrade to %s rolled back to %s", guard.Pending.To, guard.Pending.From)
	}
	if guard.Pending != nil {
		log.Info("console upgrade starting", "from", guard.Pending.From, "to", guard.Pending.To, "start", guard.Pending.Starts)
	}
	return guard.Pending, layout, nil
}

// confirmConsoleUpgrade waits for this server to answer as the new release and
// records the upgrade. A server that never answers exits, so the next start
// rolls back instead of leaving a binary that does not work in place.
func confirmConsoleUpgrade(ctx context.Context, log *slog.Logger, layout selfinstall.Layout, pending selfinstall.PendingUpgrade, loopback string) {
	err := selfinstall.WaitConfirmed(ctx, layout, pending, serviceMode(), loopback+"/healthz", consoleUpgradeConfirmTimeout, selfinstall.DefaultBackupKeep)
	switch {
	case err == nil:
		log.Info("console upgrade confirmed", "from", pending.From, "to", pending.To)
	case errors.Is(err, selfinstall.ErrHistoryNotRecorded):
		log.Warn("console upgrade confirmed without an install history entry", "from", pending.From, "to", pending.To, "error", err)
	case ctx.Err() != nil:
	default:
		log.Error("console upgrade did not become healthy; exiting so the next start rolls back", "to", pending.To, "error", err)
		os.Exit(1)
	}
}

// serviceMode is how this server is kept running, as the install history
// names it: a systemd unit, a launch daemon, or the detached supervisor of
// --no-systemd.
func serviceMode() string {
	if mode := selfinstall.ManagedMode(); mode != "" {
		return mode
	}
	return selfinstall.ModeNohup
}
