package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/launchd"
)

// installService registers this binary as the Mac's agent job (package
// launchd): a launch daemon running as --user when run as root, otherwise a
// launch agent of the caller's session. The install script runs it once the
// binary is in place and `configure` has written the state directory's
// configuration, which the job runs with (`run --state DIR`).
func installService(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("install-service", flag.ContinueOnError)
	job := launchd.Job{}
	fs.StringVar(&job.StateDir, "state", "", "state directory, configured by `gpuworks-agent configure`")
	fs.StringVar(&job.User, "user", "", "the account a launch daemon runs as (required as root)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if job.StateDir == "" {
		return fmt.Errorf("--state is required")
	}
	conf, err := agentconf.Load(job.StateDir)
	if err != nil {
		return fmt.Errorf("read the agent configuration (run `gpuworks-agent configure` first): %w", err)
	}
	if err := conf.Validate(); err != nil {
		return fmt.Errorf("%s: %w", agentconf.Path(job.StateDir), err)
	}
	job.Server = conf.Server
	if job.Binary, err = os.Executable(); err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(job.Binary); err == nil {
		job.Binary = resolved
	}
	if goruntime.GOOS != "darwin" {
		return fmt.Errorf("install-service registers a launchd job and runs on macOS only")
	}
	host, err := launchd.NewHost(out)
	if err != nil {
		return err
	}
	pid, err := host.Install(ctx, job)
	if err != nil {
		return err
	}
	domain := "system"
	if !host.Admin {
		domain = fmt.Sprintf("gui/%d", host.Self.UID)
	}
	fmt.Fprintf(out, "gpuworks: agent running under launchd (%s/%s, pid %d); logs: %s\n", domain, launchd.AgentLabel, pid, filepath.Join(job.StateDir, "agent.log"))
	return nil
}

// uninstallLaunchd removes the Mac's agent job and the node identity in the
// state directory it named, so the Mac registers afresh if installed again.
func uninstallLaunchd(ctx context.Context, states []string, out io.Writer) error {
	host, err := launchd.NewHost(out)
	if err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	return uninstallLaunchdStates(ctx, states, host, binary)
}
