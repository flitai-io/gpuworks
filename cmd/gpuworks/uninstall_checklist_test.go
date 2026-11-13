package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	_ "github.com/glebarez/go-sqlite"

	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// This fixture uses SQLite's real wire schema and WAL rather than permissive
// inventory stubs. Secret-bearing columns ensure checklist projections stay safe.
func serverChecklistFixture(t *testing.T) (selfinstall.Layout, *sql.DB) {
	t.Helper()
	layout := selfinstall.NewLayout(t.TempDir(), "", "")
	layout.UnitDir, layout.LaunchdDir = t.TempDir(), t.TempDir()
	if err := os.MkdirAll(layout.DataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := serverconfig.Write(layout.ConfigPath(), serverconfig.File{DataDir: layout.DataDir, Log: serverconfig.Log{File: layout.LogFile()}}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.Binary(), []byte("fixture binary"), 0755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(layout.DataDir, "gpuworks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, query := range []string{
		`PRAGMA journal_mode=WAL`,
		`CREATE TABLE nodes (id integer, uuid text, name text, state text, token text)`,
		`CREATE TABLE deployments (id integer, name text, desired_state text)`,
		`CREATE TABLE instances (id integer, uuid text, deployment_id integer, node_id integer, state text, container_id text, api_port integer, gpu_indexes text)`,
		`CREATE TABLE provider_instances (id integer, provider_id integer, external_id text, name text, power text, node_id integer, ssh_connections text)`,
		`CREATE TABLE cloud_resource_leases (id integer, provider_id integer, cloud_host_id integer, node_uuid text, instance_uuid text, resource_type text, resource_id text, amount integer, unit text, state text)`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	return layout, db
}

func TestUninstallCLIPreviewsPersistedWorkloadsWithoutChangingDatabase(t *testing.T) {
	layout, db := serverChecklistFixture(t)
	for _, query := range []string{
		`INSERT INTO nodes VALUES (7,'node-7','gpu node','offline','DO-NOT-LEAK-node-token')`,
		`INSERT INTO deployments VALUES (8,'chat model','active')`,
		`INSERT INTO instances VALUES (9,'instance-9',8,7,'running','gpuworks-instance',8000,'[0]')`,
		`INSERT INTO provider_instances VALUES (1,2,'vendor-vm','rented vm','running',7,'DO-NOT-LEAK-ssh')`,
		`INSERT INTO cloud_resource_leases VALUES (1,2,3,'node-7','instance-9','gpu','gpu-id',1,'card','reserved')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(filepath.Join(layout.DataDir, "gpuworks.db"))
	if err != nil {
		t.Fatal(err)
	}
	streams, out := uninstallStreams("", false)
	err = runUninstallCLI(context.Background(), []string{"--dir", layout.Dir, "--dry-run", "--json"}, streams, func(ctx context.Context, _ selfinstall.Layout, req selfinstall.UninstallRequest, streams *selfinstall.IO) (selfinstall.UninstallReport, error) {
		return selfinstall.New(layout, selfinstall.Deps{Run: new(uninstallRecordingRunner)}, streams).Uninstall(ctx, req, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Checklist []struct {
			ID, Status, Summary string
			Details             []string
		} `json:"checklist"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Checklist) == 0 {
		t.Fatalf("missing resource checklist: %s", out)
	}
	for id, want := range map[string]string{"server.nodes": "unknown", "server.deployments": "blocked", "server.instances": "blocked", "server.provider_instances": "unknown", "server.cloud_leases": "blocked"} {
		found := false
		for _, item := range result.Checklist {
			if item.ID == id {
				found = true
				if item.Status != want {
					t.Errorf("%s=%s", id, item.Status)
				}
			}
		}
		if !found {
			t.Errorf("missing %s: %s", id, out)
		}
	}
	if !strings.Contains(out.String(), "instance-9") || !strings.Contains(out.String(), "persisted") || strings.Contains(out.String(), "DO-NOT-LEAK") {
		t.Fatalf("unsafe or incomplete evidence: %s", out)
	}
	after, _ := os.ReadFile(filepath.Join(layout.DataDir, "gpuworks.db"))
	if string(before) != string(after) {
		t.Fatal("preview changed database")
	}
	if _, err := os.Stat(layout.LockPath()); !os.IsNotExist(err) {
		t.Fatalf("preview created lock: %v", err)
	}
}

// All external commands have explicit fixture behavior. A ps failure must not
// accidentally turn the default resource gate into successful cleanup.
type checklistRunner struct{ commands []selfinstall.Command }

func (r *checklistRunner) Run(_ context.Context, cmd selfinstall.Command) error {
	r.commands = append(r.commands, cmd)
	return fmt.Errorf("unexpected mutating command: %s", cmd.Path)
}
func (r *checklistRunner) Output(_ context.Context, cmd selfinstall.Command) (string, error) {
	if cmd.Path == "ps" {
		return fmt.Sprintf("%d fixture-test-process\n", os.Getpid()), nil
	}
	if len(cmd.Args) == 1 && cmd.Args[0] == "version" {
		return "gpuworks fixture (test, 2026-01-01)\n", nil
	}
	return "", fmt.Errorf("unexpected observation command: %s", cmd.Path)
}

func runChecklistFixture(t *testing.T, layout selfinstall.Layout, args []string) ([]byte, error) {
	t.Helper()
	streams, out := uninstallStreams("", false)
	err := runUninstallCLI(t.Context(), append([]string{"--dir", layout.Dir, "--json"}, args...), streams, func(ctx context.Context, _ selfinstall.Layout, req selfinstall.UninstallRequest, streams *selfinstall.IO) (selfinstall.UninstallReport, error) {
		return selfinstall.New(layout, selfinstall.Deps{Run: new(checklistRunner), Euid: func() int { return 1000 }}, streams).Uninstall(ctx, req, nil)
	})
	return out.Bytes(), err
}

func TestUninstallCLIBlocksActiveOrUnreadableResourcesBeforeCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		breakDB     bool
	}{
		{name: "running instance", query: `INSERT INTO instances VALUES (9,'instance-active',8,7,'running','workload',8000,'[0]')`},
		{name: "draining instance", query: `INSERT INTO instances VALUES (9,'instance-active',8,7,'draining','workload',8000,'[0]')`},
		{name: "removing instance", query: `INSERT INTO instances VALUES (9,'instance-active',8,7,'removing','workload',8000,'[0]')`},
		{name: "active deployment", query: `INSERT INTO deployments VALUES (8,'chat','active')`},
		{name: "offline node unverified", query: `INSERT INTO nodes VALUES (7,'node-7','node','offline','secret')`},
		{name: "unreleased reservation", query: `INSERT INTO cloud_resource_leases VALUES (1,2,3,'node-7','instance-9','gpu','gpu-id',1,'card','release_pending')`},
		{name: "incomplete schema", query: `DROP TABLE instances`},
		{name: "unknown instance state", query: `INSERT INTO instances VALUES (9,'instance-active',8,7,'future-state','workload',8000,'[0]')`},
		{name: "damaged database", breakDB: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layout, db := serverChecklistFixture(t)
			if tc.query != "" {
				if _, err := db.Exec(tc.query); err != nil {
					t.Fatal(err)
				}
			}
			if tc.breakDB {
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(layout.DataDir, "gpuworks.db"), []byte("not sqlite"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := checklistTree(t, layout.Dir)
			raw, err := runChecklistFixture(t, layout, []string{"--yes", "--purge"})
			if err == nil || !strings.Contains(err.Error(), "--keep-resources") {
				t.Fatalf("resource gate bypassed: err=%v output=%s", err, raw)
			}
			if !strings.Contains(string(raw), `"success":false`) || !strings.Contains(string(raw), `"checklist":[`) {
				t.Fatalf("missing failure inventory: %s", raw)
			}
			if after := checklistTree(t, layout.Dir); !reflect.DeepEqual(before, after) {
				t.Fatalf("blocked uninstall changed files: before=%v after=%v", before, after)
			}
		})
	}
}

func checklistTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := info.Mode().String()
		if !entry.IsDir() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += fmt.Sprintf("%x", sha256.Sum256(raw))
		}
		out[path] = value
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestUninstallCLIKeepResourcesRetainsEvidenceAfterPurge(t *testing.T) {
	layout, db := serverChecklistFixture(t)
	if _, err := db.Exec(`INSERT INTO instances VALUES (9,'instance-kept',8,7,'running','workload-kept',8000,'[0]')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := runChecklistFixture(t, layout, []string{"--yes", "--purge", "--keep-resources"})
	if err != nil {
		t.Fatalf("explicit retention failed: %v %s", err, raw)
	}
	for _, fact := range []string{`"success":true`, `"keep_resources":true`, `instance-kept`, `workload-kept`, `"status":"blocked"`, `"id":"server.purge"`} {
		if !strings.Contains(string(raw), fact) {
			t.Errorf("lost retained-resource fact %q: %s", fact, raw)
		}
	}
	if _, err := os.Stat(layout.Binary()); !os.IsNotExist(err) {
		t.Fatalf("program remains: %v", err)
	}
	if _, err := os.Stat(layout.DataDir); !os.IsNotExist(err) {
		t.Fatalf("purge did not remove confirmed data directory: %v", err)
	}
}

func TestUninstallCLIEmptyInventoryCanRemoveComponent(t *testing.T) {
	layout, db := serverChecklistFixture(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := runChecklistFixture(t, layout, []string{"--yes"})
	if err != nil {
		t.Fatalf("empty inventory failed: %v %s", err, raw)
	}
	var result selfinstall.UninstallReport
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	expected := map[string]bool{"server.nodes": false, "server.deployments": false, "server.instances": false, "server.provider_instances": false, "server.cloud_leases": false}
	for _, item := range result.Checklist {
		if _, exists := expected[item.ID]; exists {
			if item.Status != "pass" {
				t.Fatalf("empty resource inventory not established: %+v", item)
			}
			expected[item.ID] = true
		}
	}
	for id, found := range expected {
		if !found {
			t.Fatalf("missing empty inventory check: %s", id)
		}
	}
	if _, err := os.Stat(filepath.Join(layout.DataDir, "gpuworks.db")); err != nil {
		t.Fatalf("ordinary uninstall removed DB: %v", err)
	}
}

type rejectUninstallPlan struct{}

func (rejectUninstallPlan) Write(raw []byte) (int, error) {
	if strings.Contains(string(raw), "installation:") {
		return 0, errors.New("plan output unavailable")
	}
	return len(raw), nil
}

func TestUninstallCLIStopsWhenCleanupPlanCannotBeDisplayed(t *testing.T) {
	layout, db := serverChecklistFixture(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	streams, _ := uninstallStreams("", false)
	streams.Out = rejectUninstallPlan{}
	err := runUninstallCLI(t.Context(), []string{"--dir", layout.Dir, "--yes"}, streams, func(ctx context.Context, _ selfinstall.Layout, req selfinstall.UninstallRequest, streams *selfinstall.IO) (selfinstall.UninstallReport, error) {
		return selfinstall.New(layout, selfinstall.Deps{Run: new(checklistRunner), Euid: func() int { return 1000 }}, streams).Uninstall(ctx, req, nil)
	})
	if err == nil || !strings.Contains(err.Error(), "plan") {
		t.Fatalf("output failure hidden: %v", err)
	}
	if _, err := os.Stat(layout.Binary()); err != nil {
		t.Fatalf("unreviewable plan removed binary: %v", err)
	}
	if _, err := os.Stat(layout.LockPath()); !os.IsNotExist(err) {
		t.Fatalf("output failure reached uninstall lock: %v", err)
	}
}

func TestUninstallCLIRechecksInventoryAfterConfirmation(t *testing.T) {
	layout, db := serverChecklistFixture(t)
	streams, out := uninstallStreams("", true)
	streams.In = &checklistConfirmation{Reader: strings.NewReader("yes\n"), beforeRead: func() {
		if _, err := db.Exec(`INSERT INTO instances VALUES (15,'arrived-during-confirmation',8,7,'starting','new-workload',8001,'[1]')`); err != nil {
			t.Fatal(err)
		}
	}}
	err := runUninstallCLI(t.Context(), []string{"--dir", layout.Dir, "--purge=false"}, streams, func(ctx context.Context, _ selfinstall.Layout, req selfinstall.UninstallRequest, streams *selfinstall.IO) (selfinstall.UninstallReport, error) {
		return selfinstall.New(layout, selfinstall.Deps{Run: new(checklistRunner), Euid: func() int { return 1000 }}, streams).Uninstall(ctx, req, nil)
	})
	if err == nil || !strings.Contains(err.Error(), "--keep-resources") || !strings.Contains(out.String(), "arrived-during-confirmation") {
		t.Fatalf("confirmation race not caught: %v %s", err, out)
	}
	if _, err := os.Stat(layout.Binary()); err != nil {
		t.Fatalf("new workload lost control plane: %v", err)
	}
}

type checklistConfirmation struct {
	io.Reader
	beforeRead func()
}

func (r *checklistConfirmation) Read(buf []byte) (int, error) {
	if r.beforeRead != nil {
		r.beforeRead()
		r.beforeRead = nil
	}
	return r.Reader.Read(buf)
}

func TestUninstallCLICancelledConfirmationPreservesInstallation(t *testing.T) {
	layout, _ := serverChecklistFixture(t)
	before := checklistTree(t, layout.Dir)
	streams, _ := uninstallStreams("no\n", true)
	err := runUninstallCLI(t.Context(), []string{"--dir", layout.Dir, "--purge=false"}, streams, func(ctx context.Context, _ selfinstall.Layout, req selfinstall.UninstallRequest, streams *selfinstall.IO) (selfinstall.UninstallReport, error) {
		return selfinstall.New(layout, selfinstall.Deps{Run: new(checklistRunner)}, streams).Uninstall(ctx, req, nil)
	})
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("expected cancellation: %v", err)
	}
	if after := checklistTree(t, layout.Dir); !reflect.DeepEqual(before, after) {
		t.Fatal("cancelled confirmation changed installation")
	}
}

func TestUninstallCLIPrivateWALSnapshotIsRemovedAndDoesNotTouchSource(t *testing.T) {
	layout, db := serverChecklistFixture(t)
	if _, err := db.Exec(`INSERT INTO instances VALUES (20,'only-in-WAL',8,7,'running','workload',8000,'[0]')`); err != nil {
		t.Fatal(err)
	}
	before := checklistTree(t, layout.Dir)
	snapshots := t.TempDir()
	t.Setenv("TMPDIR", snapshots)
	raw, err := runChecklistFixture(t, layout, []string{"--dry-run"})
	if err != nil || !strings.Contains(string(raw), "only-in-WAL") {
		t.Fatalf("lost WAL-only workload: %v %s", err, raw)
	}
	if after := checklistTree(t, layout.Dir); !reflect.DeepEqual(before, after) {
		t.Fatal("preview wrote installed DB/WAL/SHM, files or permissions")
	}
	entries, err := os.ReadDir(snapshots)
	if err != nil || len(entries) != 0 {
		t.Fatalf("private DB snapshot leaked: entries=%v err=%v", entries, err)
	}
}

func TestUninstallCLIDoesNotTreatRollbackTransactionAsEmptyInventory(t *testing.T) {
	layout, db := serverChecklistFixture(t)
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, query := range []string{`PRAGMA journal_mode=DELETE`, `PRAGMA cache_size=1`, `CREATE TABLE padding (payload blob)`} {
		if _, err := conn.ExecContext(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := conn.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO instances VALUES (30,'not-committed',8,7,'running','workload',8000,'[0]')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO padding VALUES (zeroblob(1048576))`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(layout.DataDir, "gpuworks.db-journal")); err != nil {
		t.Fatalf("fixture did not create rollback journal: %v", err)
	}
	raw, err := runChecklistFixture(t, layout, []string{"--dry-run"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"id":"server.database","status":"unknown"`) {
		t.Fatalf("rollback journal treated as authoritative inventory: %s", raw)
	}
}

func TestUninstallCLIChangingWALProducesUnknownAndCleansSnapshot(t *testing.T) {
	layout, db := serverChecklistFixture(t)
	for _, query := range []string{`PRAGMA wal_autocheckpoint=0`, `CREATE TABLE padding (payload blob)`, `INSERT INTO padding VALUES (zeroblob(16777216))`, `INSERT INTO nodes VALUES (1,'node','changing','online','secret')`} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	snapshots := t.TempDir()
	t.Setenv("TMPDIR", snapshots)
	stop, done, started := make(chan struct{}), make(chan error, 1), make(chan struct{})
	go func() {
		for sequence := 0; ; sequence++ {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			if _, err := db.Exec(`UPDATE nodes SET name=? WHERE id=1`, fmt.Sprintf("changing-%d", sequence)); err != nil {
				done <- err
				return
			}
			if sequence == 0 {
				close(started)
			}
		}
	}()
	select {
	case <-started:
	case writeErr := <-done:
		t.Fatalf("writer fixture failed before inventory: %v", writeErr)
	}
	raw, err := runChecklistFixture(t, layout, []string{"--dry-run"})
	close(stop)
	if writeErr := <-done; writeErr != nil {
		t.Fatal(writeErr)
	}
	if err != nil || !strings.Contains(string(raw), `"id":"server.database","status":"unknown"`) {
		t.Fatalf("changing database was authoritative: %v %s", err, raw)
	}
	entries, err := os.ReadDir(snapshots)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed snapshot leaked: entries=%v err=%v", entries, err)
	}
}

func TestUninstallCLISnapshotBudgetProducesUnknown(t *testing.T) {
	layout, db := serverChecklistFixture(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(layout.DataDir, "gpuworks.db"), 257<<20); err != nil {
		t.Fatal(err)
	}
	snapshots := t.TempDir()
	t.Setenv("TMPDIR", snapshots)
	raw, err := runChecklistFixture(t, layout, []string{"--dry-run"})
	if err != nil || !strings.Contains(string(raw), `"id":"server.database","status":"unknown"`) {
		t.Fatalf("snapshot size budget not enforced: %v %s", err, raw)
	}
	entries, err := os.ReadDir(snapshots)
	if err != nil || len(entries) != 0 {
		t.Fatalf("oversize snapshot leaked: entries=%v err=%v", entries, err)
	}
}

func TestUninstallCLIKeepResourcesDoesNotBypassPurgeOwnership(t *testing.T) {
	layout, _ := serverChecklistFixture(t)
	if err := serverconfig.Write(layout.ConfigPath(), serverconfig.File{DataDir: layout.Dir, Log: serverconfig.Log{File: layout.LogFile()}}, 0600); err != nil {
		t.Fatal(err)
	}
	before := checklistTree(t, layout.Dir)
	raw, err := runChecklistFixture(t, layout, []string{"--yes", "--purge", "--keep-resources"})
	if err == nil || !strings.Contains(err.Error(), "refusing recursive cleanup") {
		t.Fatalf("retention bypassed unsafe directory guard: %v %s", err, raw)
	}
	if after := checklistTree(t, layout.Dir); !reflect.DeepEqual(before, after) {
		t.Fatal("unsafe purge changed installation despite ownership guard")
	}
}
