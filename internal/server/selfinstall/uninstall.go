package selfinstall

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"gpuworks.ai/gpuworks/internal/uninstallcheck"
)

// UninstallRequest selects the explicitly authorized local cleanup scope.
type UninstallRequest struct {
	Purge         bool
	Yes           bool
	DryRun        bool
	KeepResources bool
	// Resources must only observe persisted/local resources; nil is unknown.
	Resources func(context.Context, Layout) []uninstallcheck.Item
}

// UninstallReport describes actual removals (or planned removals for DryRun).
// Kept includes data and resources whose ownership cannot be established.
type UninstallReport struct {
	Layout        Layout                `json:"layout"`
	Mode          string                `json:"mode"`
	ServiceUser   string                `json:"service_user"`
	Purge         bool                  `json:"purge"`
	DryRun        bool                  `json:"dry_run"`
	AlreadyAbsent bool                  `json:"already_absent"`
	Removed       []string              `json:"removed"`
	Kept          []string              `json:"kept"`
	KeepResources bool                  `json:"keep_resources"`
	Checklist     []uninstallcheck.Item `json:"checklist"`
}

// Uninstall removes one local control plane. beforeStop closes resources
// managed outside selfinstall (the public connector) while the upgrade lock
// is held. It is never called for a preview or before confirmation.
func (i *Installer) Uninstall(ctx context.Context, req UninstallRequest, beforeStop func(context.Context, Layout) error) (report UninstallReport, err error) {
	report = UninstallReport{Layout: i.layout, Purge: req.Purge, DryRun: req.DryRun, KeepResources: req.KeepResources, Checklist: []uninstallcheck.Item{}, Removed: []string{}, Kept: []string{}}
	if !req.Yes && !req.DryRun && !i.io.Terminal() {
		return report, fmt.Errorf("uninstall requires confirmation; pass --yes for unattended use")
	}
	inst, err := i.readInstallation(ctx)
	if errors.Is(err, ErrNotInstalled) {
		if absentErr := i.confirmUninstalled(); absentErr != nil {
			return report, absentErr
		}
		report.AlreadyAbsent = true
		report.Checklist = []uninstallcheck.Item{{ID: "server.scope", Status: uninstallcheck.Warn, Summary: "No local installation configuration, program or autostart definition remains"}, {ID: "server.resources", Status: uninstallcheck.Unknown, Summary: "Remote resource state cannot be inferred from an absent control plane", Action: "Inspect agents, inference workloads and cloud accounts separately; no resources were removed."}}
		if err := i.printUninstallChecklist(report.Checklist); err != nil {
			return report, err
		}
		i.io.Say("gpuworks is already uninstalled from %s", i.layout.Dir)
		if _, statErr := os.Stat(i.layout.Dir); statErr == nil {
			report.Kept = append(report.Kept, i.layout.Dir)
		}
		return report, nil
	}
	if err != nil {
		return report, err
	}
	i.adoptLayout(inst.Config, false, false)
	report.Layout, report.Mode, report.ServiceUser = i.layout, inst.Mode, inst.ServiceUser
	paths, err := i.uninstallPaths(req.Purge, inst.Config.Log.File)
	if err != nil {
		return report, err
	}
	if err := i.validateUninstall(paths, req.Purge); err != nil {
		return report, err
	}
	if req.Purge {
		report.Kept = append(report.Kept, i.layout.LockPath(), i.layout.Dir)
	} else {
		report.Kept = append(report.Kept, i.layout.ConfigPath(), i.layout.DataDir, i.layout.LogsDir, i.layout.BackupsDir)
	}
	if inst.ServiceUser != "" {
		report.Kept = append(report.Kept, "service account and group: "+inst.ServiceUser+" (existing accounts may be shared)")
	}
	var resources []uninstallcheck.Item
	report.Checklist, resources = i.uninstallChecklist(ctx, req, report, inst)
	if err := i.printUninstallChecklist(report.Checklist); err != nil {
		return report, err
	}
	if err := i.printUninstallPlan(report, paths); err != nil {
		return report, err
	}
	if req.DryRun {
		report.Removed = paths
		if inst.Mode == ModeSystemd {
			report.Removed = append(report.Removed, i.layout.UnitPath())
		} else if inst.Mode == ModeLaunchd {
			report.Removed = append(report.Removed, i.layout.LaunchdPath())
		}
		return report, nil
	}
	if err := checkUninstallResources(req, resources); err != nil {
		return report, err
	}
	if !req.Yes {
		ok, err := i.io.Confirm("Uninstall this control plane?", false)
		if err != nil {
			return report, err
		}
		if !ok {
			return report, errors.New("uninstall cancelled; no changes made")
		}
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	unlock, err := i.lockUpgrade(ctx)
	if err != nil {
		return report, err
	}
	defer unlock()
	// The operator may have spent minutes reading the plan. Changes during
	// that interval must not silently change the confirmed deletion targets.
	fresh, err := i.readInstallation(ctx)
	if err != nil {
		return report, err
	}
	if fresh.Mode != inst.Mode || !reflect.DeepEqual(fresh.Config, inst.Config) || fresh.ServiceUser != inst.ServiceUser {
		return report, errors.New("installation changed during confirmation; run uninstall again")
	}
	if err := i.validateUninstall(paths, req.Purge); err != nil {
		return report, err
	}
	report.Checklist, resources = i.uninstallChecklist(ctx, req, report, fresh)
	if err := i.printUninstallChecklist(report.Checklist); err != nil {
		return report, err
	}
	if err := checkUninstallResources(req, resources); err != nil {
		return report, err
	}
	if beforeStop != nil {
		if err := beforeStop(ctx, i.layout); err != nil {
			return report, fmt.Errorf("close the public entry point: %w", err)
		}
	}
	svc := i.service(inst.Mode, inst.ServiceUser, i.unitConfig(inst.ServiceUser, inst.Mode))
	if err := svc.Stop(ctx); err != nil {
		return report, fmt.Errorf("stop the service: %w", err)
	}
	if err := i.checkUninstallProcesses(ctx); err != nil {
		return report, err
	}
	// A missing/stale PID file cannot prove an unknown listener is this
	// installation. Refuse to delete its program while an address is held.
	if err := i.checkAddrsReleased(ctx, inst.Config, "uninstall"); err != nil {
		return report, err
	}
	// Shutdown itself can rotate logs after the operator confirmed the plan.
	// The confirmed scope includes numbered rotations, but every new target
	// still needs the same path validation before any file is removed.
	if req.Purge {
		paths, err = i.uninstallPaths(true, inst.Config.Log.File)
		if err != nil {
			return report, err
		}
		if err := i.validateUninstall(paths, true); err != nil {
			return report, err
		}
	}
	if inst.Mode == ModeSystemd || inst.Mode == ModeLaunchd {
		path := i.layout.UnitPath()
		if inst.Mode == ModeLaunchd {
			path = i.layout.LaunchdPath()
		}
		definition, err := os.ReadFile(path)
		if err != nil {
			return report, err
		}
		if err := svc.RemoveUnit(ctx); err != nil {
			if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
				// A failed daemon-reload must not erase how a retry discovers
				// the service and its account. Restore its disabled definition.
				if restoreErr := svc.writeFile(ctx, path, string(definition), 0o644); restoreErr != nil {
					report.Removed = append(report.Removed, path)
					err = errors.Join(err, fmt.Errorf("restore service definition for retry: %w", restoreErr))
				}
			}
			return report, fmt.Errorf("remove the service definition: %w", err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return report, fmt.Errorf("could not confirm removal of service definition %s", path)
		}
		report.Removed = append(report.Removed, path)
	}
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if err := i.removeUninstallPath(ctx, path); err != nil {
			return report, err
		}
		report.Removed = append(report.Removed, path)
		i.io.Say("removed: %s", path)
	}
	// Keep the lock inode: unlinking it while held would let a concurrent
	// upgrade take a new inode's lock. Unknown install-tree files stay too.
	i.io.Say("gpuworks uninstalled (%s)", inst.Mode)
	return report, nil
}

func (i *Installer) printUninstallPlan(report UninstallReport, paths []string) error {
	if i.io.Quiet || i.io.Out == nil {
		return nil
	}
	var plan strings.Builder
	say := func(format string, args ...any) { _, _ = fmt.Fprintf(&plan, format+"\n", args...) }
	say("installation: %s (%s)", i.layout.Dir, report.Mode)
	say("stop: the local server and its public entry point; remove their autostart definitions")
	for _, path := range paths {
		say("remove: %s", path)
	}
	if report.Purge {
		say("remove: numbered server log rotations, including those created during shutdown")
	}
	for _, path := range report.Kept {
		say("keep: %s", path)
	}
	say("remote agents, inference workloads and rented cloud instances require separate management")
	if _, err := io.WriteString(i.io.Out, plan.String()); err != nil {
		return fmt.Errorf("write uninstall plan: %w", err)
	}
	return nil
}

// confirmUninstalled permits retry after a completed purge, but never treats
// an orphan binary or service whose configuration is missing as safe to erase.
func (i *Installer) confirmUninstalled() error {
	for _, path := range []string{i.layout.Binary(), i.layout.PIDFile()} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("cannot identify installation without configuration; inspect %s before retrying", path)
		}
	}
	if unit, err := os.ReadFile(i.layout.UnitPath()); err == nil && unitRuns(unit, i.layout.Binary()) {
		return fmt.Errorf("configuration is missing but %s still belongs to this installation", i.layout.UnitPath())
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if raw, err := os.ReadFile(i.layout.LaunchdPath()); err == nil && strings.Contains(string(raw), i.layout.Binary()) {
		return fmt.Errorf("configuration is missing but %s may belong to this installation", i.layout.LaunchdPath())
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (i *Installer) removeUninstallPath(ctx context.Context, path string) error {
	if err := os.RemoveAll(path); err != nil {
		if !i.canSudo() {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		if err := i.deps.Run.Run(ctx, Command{Path: "rm", Args: []string{"-rf", "--", path}, Sudo: true}); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("could not confirm removal of %s", path)
	}
	return nil
}

// uninstallPaths orders data first and configuration last, so a partial
// failure keeps the information required to retry with the same scope.
func (i *Installer) uninstallPaths(purge bool, logFile string) ([]string, error) {
	paths := []string{i.layout.TmpDir(), i.layout.PIDFile(), i.layout.Binary()}
	if !purge {
		return paths, nil
	}
	paths = []string{i.layout.DataDir, i.layout.BackupsDir, i.layout.TmpDir()}
	// The log directory may contain unrelated programs' logs. Delete only
	// the server log and its numbered rotations, never the whole directory.
	if strings.TrimSpace(logFile) == "" {
		logFile = i.layout.LogFile()
	}
	for _, base := range []string{logFile, i.layout.StderrLog()} {
		paths = append(paths, base)
		entries, err := os.ReadDir(filepath.Dir(base))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		for _, entry := range entries {
			suffix, ok := strings.CutPrefix(entry.Name(), filepath.Base(base)+".")
			if ok && suffix != "" && strings.Trim(suffix, "0123456789") == "" {
				paths = append(paths, filepath.Join(filepath.Dir(base), entry.Name()))
			}
		}
	}
	paths = append(paths, i.layout.PIDFile(), i.layout.Binary(), filepath.Join(i.layout.Dir, installJSONFile), i.layout.ConfigPath())
	return paths, nil
}
