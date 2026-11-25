package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
)

// The inventory runs every minute; only what changed is logged, and a present
// image carries the identity the runtime reported for it.
func TestImagePresenceLogsOnlyChanges(t *testing.T) {
	var out strings.Builder
	log := slog.New(slog.NewTextHandler(&out, nil))
	driver := recording.New("")
	driver.InjectInspection(runtime.ImageInspection{ID: "sha256:abc"})
	var seen imagePresenceLog
	ctx := context.Background()
	lines := func() []string {
		defer out.Reset()
		if out.Len() == 0 {
			return nil
		}
		return strings.Split(strings.TrimSpace(out.String()), "\n")
	}

	seen.observe(ctx, log, driver, "absent:v1", false)
	if got := lines(); got != nil {
		t.Fatalf("an image never seen and absent is not news: %q", got)
	}
	seen.observe(ctx, log, driver, "img:v1", true)
	if got := lines(); len(got) != 1 || !strings.Contains(got[0], `msg="image present" image=img:v1 runtime=docker image_id=sha256:abc`) {
		t.Fatalf("first present: %q", got)
	}
	seen.observe(ctx, log, driver, "img:v1", true)
	if got := lines(); got != nil {
		t.Fatalf("unchanged: %q", got)
	}
	seen.observe(ctx, log, driver, "img:v1", false)
	if got := lines(); len(got) != 1 || !strings.Contains(got[0], `msg="image no longer present" image=img:v1`) {
		t.Fatalf("gone: %q", got)
	}
	driver.InjectInspectFailure(errors.New("daemon gone"))
	seen.observe(ctx, log, driver, "img:v1", true)
	if got := lines(); len(got) != 1 || !strings.Contains(got[0], `inspect_err="daemon gone"`) {
		t.Fatalf("back, inspect failing: %q", got)
	}

	// A ref dropped from the catalog is forgotten: seen again, it is news.
	seen.forget([]string{"absent:v1"})
	seen.observe(ctx, log, driver, "img:v1", true)
	if got := lines(); len(got) != 1 {
		t.Fatalf("after forget: %q", got)
	}
}
