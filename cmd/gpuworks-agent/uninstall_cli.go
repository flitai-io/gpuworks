package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"gpuworks.ai/gpuworks/internal/uninstallcheck"
)

type agentUninstallDeps struct {
	resolve func(string) ([]string, error)
	observe func(context.Context, string) []uninstallcheck.Item
	remove  func(context.Context, []string) error
}

type agentUninstallReport struct {
	StateDirs     []string              `json:"state_dirs"`
	DryRun        bool                  `json:"dry_run"`
	KeepResources bool                  `json:"keep_resources"`
	Checklist     []uninstallcheck.Item `json:"checklist"`
	Success       bool                  `json:"success"`
	Error         string                `json:"error,omitempty"`
}

func uninstall(args []string) error {
	// This standalone CLI owns the logger lifecycle. Runtime/vendor helpers
	// may log raw command output; only the safe checklist is part of this command.
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(previousLogger)
	ctx := context.Background()
	d := agentUninstallDeps{resolve: resolveAgentUninstallStates, observe: agentResourceChecklist}
	d.remove = func(ctx context.Context, states []string) error {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if runtime.GOOS == "darwin" {
			return uninstallLaunchd(ctx, states, io.Discard)
		}
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		u := linuxUninstaller{binary: binary, procDir: "/proc", unitPath: agentUnitPath,
			run: func(ctx context.Context, args ...string) error {
				err := exec.CommandContext(ctx, "systemctl", args...).Run()
				if err != nil {
					return fmt.Errorf("systemctl %s failed", strings.Join(args, " "))
				}
				return nil
			},
			signal: func(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) },
		}
		return u.uninstall(ctx, states[0])
	}
	return runAgentUninstall(ctx, args, os.Stdin, os.Stdout, term.IsTerminal(int(os.Stdin.Fd())), d)
}

func runAgentUninstall(ctx context.Context, args []string, in io.Reader, out io.Writer, terminal bool, d agentUninstallDeps) error {
	var state string
	var yes, dry, jsonOutput, keep bool
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	var flagOutput bytes.Buffer
	fs.SetOutput(&flagOutput)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(&flagOutput, "Usage of uninstall:\n  gpuworks-agent uninstall [--state DIR] [--yes] [--dry-run] [--json] [--keep-resources]")
		fs.PrintDefaults()
	}
	fs.StringVar(&state, "state", "", "agent state directory")
	fs.BoolVar(&yes, "yes", false, "confirm component removal; does not bypass workload checks")
	fs.BoolVar(&yes, "y", false, "alias for --yes")
	fs.BoolVar(&dry, "dry-run", false, "show the complete checklist without changes")
	fs.BoolVar(&jsonOutput, "json", false, "write a structured report")
	fs.BoolVar(&keep, "keep-resources", false, "explicitly retain workloads and remove only this agent service")
	r := agentUninstallReport{DryRun: dry, Checklist: []uninstallcheck.Item{}}
	finish := func(err error) error {
		r.Success = err == nil
		if err != nil {
			r.Error = err.Error()
		}
		if jsonOutput {
			if e := json.NewEncoder(out).Encode(r); e != nil {
				return errors.Join(err, e)
			}
		}
		return err
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if _, writeErr := io.Copy(out, &flagOutput); writeErr != nil {
				return writeErr
			}
			return err
		}
		for _, arg := range args {
			if arg == "--json" {
				jsonOutput = true
			}
		}
		return finish(errors.New("invalid uninstall options; use --help"))
	}
	r.DryRun, r.KeepResources = dry, keep
	if fs.NArg() != 0 {
		return finish(errors.New("unexpected positional argument"))
	}
	states, err := d.resolve(state)
	r.StateDirs = states
	if err != nil {
		r.Checklist = append(r.Checklist, uninstallcheck.Item{ID: "installation", Status: uninstallcheck.Unknown, Summary: "Agent installation identity could not be established", Action: "Inspect service configuration and explicitly select the correct --state directory."})
		if !jsonOutput {
			if err := uninstallcheck.Print(out, r.Checklist); err != nil {
				return finish(err)
			}
		}
		if dry {
			return finish(nil)
		}
		return finish(errors.New("cannot establish agent installation ownership"))
	}
	ready := false
	var printErr error
	observe := func() {
		r.Checklist = nil
		for _, dir := range states {
			r.Checklist = append(r.Checklist, agentRemovalChecklist(dir)...)
			r.Checklist = append(r.Checklist, d.observe(ctx, dir)...)
		}
		ready = !agentChecklistBlocking(r.Checklist, keep)
		if !jsonOutput {
			printErr = uninstallcheck.Print(out, r.Checklist)
		}
	}
	observe()
	if printErr != nil {
		return finish(printErr)
	}
	if dry {
		return finish(nil)
	}
	if !ready {
		return finish(errors.New("uninstall blocked: stop or migrate workloads and resolve unknown checks; --keep-resources explicitly retains them"))
	}
	if !yes {
		if !terminal || jsonOutput {
			return finish(errors.New("unattended uninstall requires --yes; use --dry-run to inspect"))
		}
		if _, err := fmt.Fprint(out, "Remove this agent service and its identity files? [y/N]: "); err != nil {
			return finish(err)
		}
		scan := bufio.NewScanner(in)
		if !scan.Scan() {
			if err := scan.Err(); err != nil {
				return finish(err)
			}
			return finish(errors.New("uninstall cancelled"))
		}
		answer := strings.TrimSpace(scan.Text())
		if !strings.EqualFold(answer, "yes") && !strings.EqualFold(answer, "y") {
			return finish(errors.New("uninstall cancelled"))
		}
	}
	// Confirmations may remain open while a new deployment starts.
	observe()
	if printErr != nil {
		return finish(printErr)
	}
	if !ready {
		return finish(errors.New("resources changed: uninstall blocked before stopping the agent"))
	}
	fresh, err := d.resolve(state)
	if err != nil || strings.Join(fresh, "\x00") != strings.Join(states, "\x00") {
		return finish(errors.New("installation changed; inspect the checklist again"))
	}
	if err := ctx.Err(); err != nil {
		return finish(err)
	}
	if err := d.remove(ctx, states); err != nil {
		return finish(errors.New("agent component removal failed; inspect the service and retained installation files locally"))
	}
	if !jsonOutput {
		_, _ = fmt.Fprintln(out, "gpuworks-agent service removed; model files, logs and runtime workloads are retained. GPU resource release is not implied.")
	}
	return finish(nil)
}

// GPU process observation is advisory: unsupported vendor tools do not prove
// idle GPUs and do not prevent removing a component whose workload inventory
// is known. Runtime/configuration uncertainty remains a hard gate.
func agentChecklistBlocking(items []uninstallcheck.Item, keep bool) bool {
	for _, item := range items {
		if strings.HasPrefix(item.ID, "gpu.") || (keep && !strings.HasPrefix(item.ID, "agent.safety.")) {
			continue
		}
		if uninstallcheck.Blocking([]uninstallcheck.Item{item}) {
			return true
		}
	}
	return false
}
