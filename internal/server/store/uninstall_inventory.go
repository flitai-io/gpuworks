package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"
	"gpuworks.ai/gpuworks/internal/uninstallcheck"
)

// ReadUninstallInventory reads only whitelisted persisted facts. Unlike Open it
// never initializes a database, migrates a schema, changes permissions or sets
// journal mode. Missing/older schemas remain unknown instead of appearing empty.
// Persisted status is not evidence that remote processes or billing have stopped.
func ReadUninstallInventory(ctx context.Context, path string) []uninstallcheck.Item {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	unknown := func() []uninstallcheck.Item {
		return []uninstallcheck.Item{{ID: "server.database", Status: uninstallcheck.Unknown, Summary: "Persisted resource inventory could not be captured safely within the read-only snapshot limits", Action: "Check the local database, read permissions and temporary disk space; retry when database writes settle and no rollback journal remains. Snapshots allow 256 MiB per database/WAL file and 10 seconds. Inspect nodes and cloud accounts, or explicitly retain resources with --keep-resources."}}
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return unknown()
	}
	snapshot, cleanup, err := uninstallSnapshot(ctx, path)
	if err != nil {
		return unknown()
	}
	defer cleanup()
	dsn := readOnlyDSN("file:" + (&url.URL{Path: snapshot}).EscapedPath())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return unknown()
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return unknown()
	}
	defer func() { _ = tx.Rollback() }()
	checks := []struct {
		id, table, query, action string
		classify                 func([]string) string
	}{
		{"server.nodes", "nodes", `SELECT id, uuid, name, state FROM nodes ORDER BY id`, "On every listed node stop or migrate inference workloads, verify GPU processes and ports locally, then uninstall its agent separately. Offline status does not prove workloads stopped.", func([]string) string { return uninstallcheck.Unknown }},
		{"server.deployments", "deployments", `SELECT id, name, desired_state FROM deployments ORDER BY id`, "Stop or delete active deployments in GPUWorks and wait for instance cleanup before removing the control plane.", func(v []string) string {
			switch DeploymentState(v[2]) {
			case DeployStopped:
				return uninstallcheck.Warn
			case DeployActive, DeployDeleting:
				return uninstallcheck.Blocked
			default:
				return uninstallcheck.Unknown
			}
		}},
		{"server.instances", "instances", `SELECT id, uuid, deployment_id, node_id, state, container_id, api_port, gpu_indexes FROM instances ORDER BY id`, "Stop or migrate these instances and verify their containers, GPU allocations and API ports on the corresponding nodes. Database state alone cannot confirm remote cleanup.", func(v []string) string {
			switch v[4] {
			case proto.InstStopped, proto.InstFailed:
				return uninstallcheck.Warn
			case proto.InstPending, proto.InstPulling, proto.InstStarting, proto.InstOptimizing, proto.InstRunning, proto.InstDraining, proto.InstRemoving, proto.InstPendingRemoval:
				return uninstallcheck.Blocked
			default:
				return uninstallcheck.Unknown
			}
		}},
		{"server.provider_instances", "provider_instances", `SELECT id, provider_id, external_id, name, power, node_id FROM provider_instances ORDER BY id`, "Inspect these discovered provider snapshots in the cloud account. Confirm ownership and ongoing charges; stop or release rented resources separately. Uninstall does not call cloud deletion APIs.", func([]string) string { return uninstallcheck.Unknown }},
		{"server.cloud_leases", "cloud_resource_leases", `SELECT id, provider_id, cloud_host_id, node_uuid, instance_uuid, resource_type, resource_id, amount, unit, state FROM cloud_resource_leases WHERE state IS NULL OR state <> 'released' ORDER BY id`, "Release or reconcile these GPUWorks cloud reservations before uninstalling. Preserve their identifiers for manual reconciliation if retaining resources or purging the database.", func(v []string) string {
			switch v[9] {
			case cloud.CloudLeaseReserved, cloud.CloudLeaseReservePending, cloud.CloudLeaseReleasePending:
				return uninstallcheck.Blocked
			default:
				return uninstallcheck.Unknown
			}
		}},
	}
	items := make([]uninstallcheck.Item, 0, len(checks))
	for _, check := range checks {
		item := uninstallcheck.Item{ID: check.id, Status: uninstallcheck.Pass, Summary: "No records in persisted " + check.table, Action: check.action}
		rows, err := tx.QueryContext(ctx, check.query)
		if err != nil {
			item.Status = uninstallcheck.Unknown
			item.Summary = "Persisted " + check.table + " inventory unavailable (schema or read error)"
			items = append(items, item)
			continue
		}
		cols, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			item.Status = uninstallcheck.Unknown
			item.Summary = "Persisted " + check.table + " inventory unavailable"
			items = append(items, item)
			continue
		}
		for rows.Next() {
			values := make([]sql.NullString, len(cols))
			dest := make([]any, len(cols))
			for idx := range values {
				dest[idx] = &values[idx]
			}
			if err := rows.Scan(dest...); err != nil {
				item.Status = uninstallcheck.Unknown
				break
			}
			fields := make([]string, len(cols))
			raw := make([]string, len(cols))
			for idx, v := range values {
				raw[idx] = v.String
				fields[idx] = fmt.Sprintf("%s=%q", cols[idx], v.String)
			}
			item.Details = append(item.Details, strings.Join(fields, " "))
			status := check.classify(raw)
			if status == uninstallcheck.Unknown || item.Status == uninstallcheck.Pass || (status == uninstallcheck.Blocked && item.Status != uninstallcheck.Unknown) {
				item.Status = status
			}
		}
		if rows.Err() != nil {
			item.Status = uninstallcheck.Unknown
		}
		_ = rows.Close()
		if item.Status != uninstallcheck.Pass {
			item.Summary = fmt.Sprintf("%d persisted %s records; live remote status is unverified", len(item.Details), check.table)
		}
		items = append(items, item)
	}
	return items
}
