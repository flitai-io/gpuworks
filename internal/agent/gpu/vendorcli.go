package gpu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// vendorCLITimeout bounds one call of a vendor CLI (nvidia-smi, npu-smi,
// hy-smi, ppu-smi, dcgmi).
var vendorCLITimeout = 10 * time.Second

// vendorCLIReapGrace is how long a killed CLI may take to exit before the
// caller stops waiting for it. A CLI blocked inside a driver ioctl sits in
// uninterruptible sleep, where SIGKILL stays pending until the driver
// returns, and exec.Cmd.Wait blocks for exactly that long.
var vendorCLIReapGrace = 3 * time.Second

// ErrCLIUnresponsive marks a vendor CLI that did not answer in time, or whose
// previous call has still not exited. It says nothing about which fields or
// subcommands the driver supports, so callers must not treat it as a reason
// to fall back to a smaller query.
var ErrCLIUnresponsive = errors.New("vendor CLI unresponsive")

// IsCLIUnresponsive reports whether err came from a timed-out or stuck CLI.
func IsCLIUnresponsive(err error) bool { return errors.Is(err, ErrCLIUnresponsive) }

// stuckCLIs records commands whose killed process has not exited, by the key
// they run under. While one is stuck, new calls under the same key fail at
// once: each further process would block on the same driver lock, and a host
// under trouble would gather one more unkillable process every few seconds.
var stuckCLIs = struct {
	sync.Mutex
	since map[string]time.Time
}{since: map[string]time.Time{}}

// trackedTurns lets calls under one key run one at a time. Checking for a
// stuck process and starting the next one are then one step: two callers
// arriving together cannot both find the key clear and start a process each.
var trackedTurns = struct {
	sync.Mutex
	byKey map[string]chan struct{}
}{byKey: map[string]chan struct{}{}}

func trackedTurn(key string) chan struct{} {
	trackedTurns.Lock()
	defer trackedTurns.Unlock()
	turn := trackedTurns.byKey[key]
	if turn == nil {
		turn = make(chan struct{}, 1)
		trackedTurns.byKey[key] = turn
	}
	return turn
}

// ErrStillRunning marks a call refused because an earlier one under the same
// key was killed and has not exited. Nothing was started: it says nothing
// about the device the call was about, only that the driver is not letting
// go of an earlier process.
var ErrStillRunning = fmt.Errorf("an earlier call has not exited: %w", ErrCLIUnresponsive)

// RunTracked runs the command build makes (bound to the context it is given)
// under key and returns its combined output. Calls under
// one key take turns; one that outlives timeout (or ctx) has its whole process
// group killed, and one that has not exited reapGrace after that is left
// behind, marked stuck under key, and the call returns. Until it exits every
// call under key fails at once with ErrStillRunning without starting
// anything, so however often the callers ask, a key never has more than one
// process stuck in the driver. timeout <= 0 leaves the deadline to ctx.
func RunTracked(ctx context.Context, key string, timeout time.Duration, build func(context.Context) *exec.Cmd) (string, error) {
	turn := trackedTurn(key)
	select {
	case turn <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-turn }()

	stuckCLIs.Lock()
	since, stuck := stuckCLIs.since[key]
	stuckCLIs.Unlock()
	if stuck {
		return "", fmt.Errorf("%s: killed %s ago and still not gone: %w", key, time.Since(since).Round(time.Second), ErrStillRunning)
	}

	runCtx, cancel := ctx, context.CancelFunc(func() {})
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	cmd := build(runCtx)
	var out bytes.Buffer
	cmd.Stdout = &out
	if cmd.Stderr == nil {
		cmd.Stderr = &out
	}
	killGroupOnCancel(cmd)
	// Bounds the wait for output pipes held open by a child of the command.
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var err error
	select {
	case err = <-done:
	case <-runCtx.Done():
		select {
		case err = <-done:
		case <-time.After(vendorCLIReapGrace):
			markCLIStuck(key, cmd.Args[1:], done)
			return "", unresponsiveError(ctx, key, timeout)
		}
	}
	if runCtx.Err() != nil {
		return out.String(), unresponsiveError(ctx, key, timeout)
	}
	return out.String(), err
}

// RunVendorCLI runs a vendor CLI that changes driver state, or whose answer
// must be fresh, and returns its combined output. It never blocks the caller
// much past vendorCLITimeout (or ctx), even when the process cannot be
// killed (RunTracked, under the binary's name).
func RunVendorCLI(ctx context.Context, stdin io.Reader, name string, args ...string) (string, error) {
	return RunTracked(ctx, binaryKey(name), vendorCLITimeout, func(ctx context.Context) *exec.Cmd {
		cmd := cliCommand(ctx, name, args...)
		cmd.Stdin = stdin
		return cmd
	})
}

// unresponsiveError names why a call was cut short: the caller's own
// cancellation passes through unchanged, the CLI's time limit becomes
// ErrCLIUnresponsive.
func unresponsiveError(ctx context.Context, name string, timeout time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("%s: no answer within %s: %w", name, timeout, ErrCLIUnresponsive)
}

func markCLIStuck(name string, args []string, done <-chan error) {
	start := time.Now()
	stuckCLIs.Lock()
	stuckCLIs.since[name] = start
	stuckCLIs.Unlock()
	slog.Warn("gpu command did not exit after kill; further calls fail fast until it does",
		"command", name, "args", strings.Join(args, " "))
	go func() {
		<-done
		stuckCLIs.Lock()
		delete(stuckCLIs.since, name)
		stuckCLIs.Unlock()
		slog.Info("stuck gpu command exited", "command", name, "stuck_for", time.Since(start).Round(time.Second))
	}()
}
