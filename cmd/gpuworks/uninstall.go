package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/uninstallcheck"
)

type uninstallRunner func(context.Context, selfinstall.Layout, selfinstall.UninstallRequest, *selfinstall.IO) (selfinstall.UninstallReport, error)

func uninstallCommand(ctx context.Context, args []string) error {
	return runUninstallCLI(ctx, args, selfinstall.NewIO(), func(ctx context.Context, layout selfinstall.Layout, req selfinstall.UninstallRequest, streams *selfinstall.IO) (selfinstall.UninstallReport, error) {
		deps := selfinstall.NewDeps(streams.Err)
		var removed []string
		report, err := selfinstall.New(layout, deps, streams).Uninstall(ctx, req, func(ctx context.Context, actual selfinstall.Layout) error {
			var closeErr error
			removed, closeErr = uninstallExpose(ctx, actual, deps.Run, streams, actual.UnitDir)
			return closeErr
		})
		report.Removed = append(report.Removed, removed...)
		return report, err
	})
}

// runUninstallCLI shares the same execution path between the terminal and
// automation. Quiet controls narration; only yes authorizes skipping prompts.
func runUninstallCLI(ctx context.Context, args []string, streams *selfinstall.IO, run uninstallRunner) error {
	var req selfinstall.UninstallRequest
	var jsonOutput, quiet bool
	dir := selfinstall.RunningDir()
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(streams.Err)
	fs.StringVar(&dir, "dir", dir, "installation directory")
	fs.BoolVar(&req.KeepResources, "keep-resources", false, "remove only this component while explicitly retaining workloads and cloud resources")
	fs.BoolVar(&req.Purge, "purge", false, "also delete installation configuration, data, backups and logs")
	fs.BoolVar(&req.Yes, "yes", false, "skip interactive questions and confirm the selected cleanup")
	fs.BoolVar(&req.Yes, "y", false, "alias for --yes")
	fs.BoolVar(&req.DryRun, "dry-run", false, "show the cleanup checklist without changing anything")
	fs.BoolVar(&jsonOutput, "json", false, "write a JSON report; requires --yes unless --dry-run")
	fs.BoolVar(&quiet, "quiet", false, "suppress narration (does not skip confirmation)")
	parseErr := fs.Parse(args)
	// Parse may stop at a bad flag before reaching --json. Still honor the
	// requested result format so scripts can decode an argument failure.
	if parseErr != nil {
		for _, arg := range args {
			if arg == "--json" || arg == "--json=true" || arg == "-json" || arg == "-json=true" {
				jsonOutput = true
			}
		}
	}
	output := streams.Out
	finish := func(report selfinstall.UninstallReport, err error) error {
		if !jsonOutput || errors.Is(err, flag.ErrHelp) {
			return err
		}
		result := struct {
			selfinstall.UninstallReport
			Success bool   `json:"success"`
			Error   string `json:"error,omitempty"`
		}{UninstallReport: report, Success: err == nil}
		if err != nil {
			result.Error = err.Error()
		}
		if encodeErr := json.NewEncoder(output).Encode(result); encodeErr != nil {
			return errors.Join(err, fmt.Errorf("write uninstall result: %w", encodeErr))
		}
		return err
	}
	if parseErr != nil {
		return finish(selfinstall.UninstallReport{}, parseErr)
	}
	if fs.NArg() != 0 {
		return finish(selfinstall.UninstallReport{}, fmt.Errorf("unexpected argument %q", fs.Arg(0)))
	}
	dir = strings.TrimSpace(dir)
	if dir == "" || filepath.Clean(dir) == string(filepath.Separator) {
		return finish(selfinstall.UninstallReport{}, fmt.Errorf("--dir must name a specific, non-root installation directory"))
	}
	if !req.DryRun && !req.Yes && (jsonOutput || !streams.Terminal()) {
		return finish(selfinstall.UninstallReport{}, fmt.Errorf("unattended uninstall requires --yes (or --dry-run to inspect the checklist)"))
	}
	streams.Quiet = quiet || jsonOutput
	if jsonOutput {
		streams.Out = io.Discard
	}
	if !req.Yes && !req.DryRun && !flagProvided(fs, "purge") {
		purge, err := streams.Confirm("Also delete configuration, databases, master key, backups and logs?", false)
		if err != nil {
			return finish(selfinstall.UninstallReport{}, err)
		}
		req.Purge = purge
	}
	layout := selfinstall.NewLayout(dir, "", "")
	req.Resources = func(ctx context.Context, actual selfinstall.Layout) []uninstallcheck.Item {
		return store.ReadUninstallInventory(ctx, filepath.Join(actual.DataDir, "gpuworks.db"))
	}
	report, err := run(ctx, layout, req, streams)
	if err == nil && !jsonOutput {
		if req.DryRun {
			streams.Say("Dry run complete; no changes made.")
		} else {
			streams.Say("gpuworks uninstalled; service accounts and remote agents/resources are retained.")
		}
	}
	return finish(report, err)
}
