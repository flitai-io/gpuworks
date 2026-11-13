// Package backupexclude keeps the agent's bulk data out of a Mac's Time
// Machine backups. Models and environment packages run to tens of gigabytes
// each and are downloaded again on demand, while Time Machine includes both
// /opt and home directories by default.
package backupexclude

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"
	"time"
)

// run executes tmutil; tests substitute it.
var run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// excluded reports whether path carries excludeAttr; tests substitute it.
var excluded = hasXattr

// tmutilTimeout bounds one tmutil call. tmutil asks backupd and took 11 s on
// a busy Mac Studio, so the bound is generous; Exclude runs off the start path.
const tmutilTimeout = time.Minute

var onMac = goruntime.GOOS == "darwin"

// Exclude marks each existing path as excluded from Time Machine. The
// exclusion is "sticky": an attribute of the item itself, so it follows the
// directory if it moves and needs no administrator rights for items the agent
// owns (`tmutil addexclusion` without -p). An item already excluded is left
// alone. Failures are logged, never fatal: a backup that includes models is
// wasteful, not wrong. It can take a while (tmutil is slow), so the agent
// calls it without waiting.
func Exclude(log *slog.Logger, paths ...string) {
	if !onMac {
		return
	}
	seen := map[string]bool{}
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		if _, err := os.Stat(path); err != nil || excluded(path) {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), tmutilTimeout)
		out, err := run(ctx, "tmutil", "addexclusion", path)
		cancel()
		if err != nil {
			log.Warn("could not exclude from Time Machine", "path", path, "err", err, "output", strings.TrimSpace(string(out)))
			continue
		}
		log.Info("excluded from Time Machine", "path", path)
	}
}
