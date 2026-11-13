package selfinstall

import (
	"context"
	"errors"
	"fmt"

	"gpuworks.ai/gpuworks/internal/uninstallcheck"
)

func (i *Installer) uninstallChecklist(ctx context.Context, req UninstallRequest, report UninstallReport, inst installation) (items, resources []uninstallcheck.Item) {
	items = []uninstallcheck.Item{
		{ID: "server.scope", Status: uninstallcheck.Pass, Summary: "Only this local control plane and its public entry point will be stopped", Details: []string{"Remote agents, inference containers, GPU processes, provider instances and cloud reservations are not stopped or released by this command."}, Action: "Stop or migrate workloads and verify GPUs and ports on each node before handing the host to another platform."},
		{ID: "server.retained_files", Status: uninstallcheck.Warn, Summary: "These local recovery files or shared resources will be retained", Details: report.Kept},
	}
	items = append(items, i.uninstallLocalChecklist(ctx, inst)...)
	if req.Purge {
		items = append(items, uninstallcheck.Item{ID: "server.purge", Status: uninstallcheck.Warn, Summary: "Purge removes the local database, master key, configuration, backups and logs", Action: "Save resource identifiers and a recovery backup before proceeding; removing these records cannot release remote resources or cancel cloud charges."})
	}
	if req.Resources == nil {
		resources = []uninstallcheck.Item{{ID: "server.database", Status: uninstallcheck.Unknown, Summary: "Resource inventory is unavailable", Action: "Inspect workloads and cloud reservations, or explicitly retain resources with --keep-resources."}}
	} else {
		resources = req.Resources(ctx, i.layout)
		if len(resources) == 0 {
			resources = []uninstallcheck.Item{{ID: "server.database", Status: uninstallcheck.Unknown, Summary: "Resource inventory returned no observations", Action: "Inspect the database and remote resources before retrying."}}
		}
	}
	items = append(items, resources...)
	if req.KeepResources {
		items = append(items, uninstallcheck.Item{ID: "server.keep_resources", Status: uninstallcheck.Warn, Summary: "Explicitly retaining workloads and remote resources, including unverified resources", Action: "Keep this checklist and manage remote agents, inference containers and cloud charges separately."})
	}
	return items, resources
}

func checkUninstallResources(req UninstallRequest, items []uninstallcheck.Item) error {
	if !req.KeepResources && uninstallcheck.Blocking(items) {
		return errors.New("uninstall blocked by active or unverified resources; follow checklist actions, or use --keep-resources to remove only this component")
	}
	return nil
}

func (i *Installer) printUninstallChecklist(items []uninstallcheck.Item) error {
	if i.io.Quiet || i.io.Out == nil {
		return nil
	}
	if err := uninstallcheck.Print(i.io.Out, items); err != nil {
		return fmt.Errorf("write uninstall checklist: %w", err)
	}
	return nil
}
