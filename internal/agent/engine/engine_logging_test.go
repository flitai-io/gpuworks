package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
)

// stopProbeDriver forces every container to exist and running so
// removeContainer walks its stop path; the optional stopErr reproduces a stop
// that fails (for example a graceful-window timeout).
type stopProbeDriver struct {
	*recording.Driver
	stopErr error
	// hangStop makes Stop block until its context ends, as a wedged
	// `docker stop` does; removeErr records the context state Remove saw.
	hangStop  bool
	removeErr error
	removed   bool
}

func (d *stopProbeDriver) Remove(ctx context.Context, id string) error {
	d.removed, d.removeErr = true, ctx.Err()
	if d.removeErr != nil {
		return d.removeErr
	}
	return d.Driver.Remove(ctx, id)
}

func (d *stopProbeDriver) Status(context.Context, string) (runtime.Status, error) {
	return runtime.Status{Exists: true, Running: true}, nil
}

func (d *stopProbeDriver) Stop(ctx context.Context, id string) error {
	if d.hangStop {
		<-ctx.Done()
		return ctx.Err()
	}
	if d.stopErr != nil {
		return d.stopErr
	}
	return d.Driver.Stop(ctx, id)
}

// The stop inside removeContainer used to be silent and discarded its error,
// which is how an externally killed container looked like it had never existed.
// It must log at INFO on the way in and surface a stop failure as a warning
// while the forced removal still proceeds.
func TestRemoveContainerLogsStopAndSurfacesStopFailure(t *testing.T) {
	newEngine := func(driver runtime.Driver, logs *bytes.Buffer) *Engine {
		return newHermeticEngine(driver, func(proto.InstanceStatus) {}, slog.New(slog.NewTextHandler(logs, nil)), t.TempDir())
	}
	ctx := context.Background()

	var logs bytes.Buffer
	e := newEngine(&stopProbeDriver{Driver: recording.New(t.TempDir() + "/calls.json")}, &logs)
	if err := e.removeContainer(ctx, "instance-stop-ok"); err != nil {
		t.Fatal(err)
	}
	if got := logs.String(); !strings.Contains(got, "stopping instance container") || !strings.Contains(got, "gpuworks-instance-s") {
		t.Fatalf("expected INFO stop log with container name, got: %q", got)
	}
	if strings.Contains(logs.String(), "stop failed") {
		t.Fatalf("successful stop must not warn: %q", logs.String())
	}

	logs.Reset()
	e = newEngine(&stopProbeDriver{Driver: recording.New(t.TempDir() + "/calls.json"), stopErr: errors.New("stop timeout")}, &logs)
	if err := e.removeContainer(ctx, "instance-stop-fail"); err != nil {
		t.Fatalf("a failed stop must not fail the removal while rm still succeeds: %v", err)
	}
	if got := logs.String(); !strings.Contains(got, "instance container stop failed") || !strings.Contains(got, "stop timeout") {
		t.Fatalf("expected stop-failure warning, got: %q", got)
	}
}

// A stop that hangs used to spend the whole teardown deadline, so the forced
// removal ran on an expired context and the instance stayed pending until a
// later sync retried it. The stop must leave removeReserve for the removal.
func TestHungStopLeavesTheForcedRemovalTimeToRun(t *testing.T) {
	driver := &stopProbeDriver{Driver: recording.New(t.TempDir() + "/calls.json"), hangStop: true}
	e := newHermeticEngine(driver, func(proto.InstanceStatus) {}, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), removeReserve+200*time.Millisecond)
	defer cancel()
	if err := e.removeContainer(ctx, "instance-stop-hangs"); err != nil {
		t.Fatalf("removal after a hung stop: %v", err)
	}
	if !driver.removed || driver.removeErr != nil {
		t.Fatalf("forced removal removed=%v ctx=%v; want it to run on a live context", driver.removed, driver.removeErr)
	}

	// Without time to spare the stop is skipped and the removal still runs.
	driver = &stopProbeDriver{Driver: recording.New(t.TempDir() + "/calls.json"), hangStop: true}
	e = newHermeticEngine(driver, func(proto.InstanceStatus) {}, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir())
	short, cancelShort := context.WithTimeout(context.Background(), removeReserve/2)
	defer cancelShort()
	start := time.Now()
	if err := e.removeContainer(short, "instance-no-stop-budget"); err != nil {
		t.Fatalf("removal without a stop budget: %v", err)
	}
	if !driver.removed || time.Since(start) > time.Second {
		t.Fatalf("removal ran=%v after %v; want it at once", driver.removed, time.Since(start))
	}
}
