//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv/testport"
)

// A member that serves is backed up without stopping it, and the archive
// rebuilds that member with exactly the archive's data: what was written
// after the backup is gone, and the member answers at its own address again.
func TestClusterBackupAndRestoreOfARunningMember(t *testing.T) {
	cfg, enrollmentAddress := enrollmentNativeConfig(t)
	if err := os.Chmod(cfg.DataDir, 0750); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if _, err := InitializeEnrollment(ctx, cfg, enrollmentAddress, "https://"+enrollmentAddress); err != nil {
		t.Fatal(err)
	}
	address := testport.Address(t)
	if _, err := ActivateDatabaseAddress(ctx, cfg, address); err != nil {
		t.Fatal(err)
	}
	bundle, err := readEnrollmentBundle(cfg.DataDir, false)
	if err != nil {
		t.Fatal(err)
	}
	memberKey, err := bundle.trust.VerifyMember(bundle.tls.Certificates[0].Leaf, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.DataDir, "gpuworks.db")
	running, err := store.OpenRuntime(ctx, path, bus.New(), store.ReplicatedBackend())
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = running.Close()
		}
	}()
	before, _, err := running.CreateNodePending("before-backup")
	if err != nil {
		t.Fatal(err)
	}
	logo := filepath.Join(cfg.DataDir, "model-logos", "fixture.png")
	if err := os.MkdirAll(filepath.Dir(logo), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logo, []byte("logo bytes"), 0600); err != nil {
		t.Fatal(err)
	}

	backups := t.TempDir()
	if _, err := BackupCluster(ctx, cfg, ClusterBackupOptions{Dir: backups}); err == nil {
		t.Fatal("a backup without a deadline was accepted")
	}
	backup, err := BackupCluster(ctx, cfg, ClusterBackupOptions{Dir: backups, Timeout: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	m := backup.Manifest
	if m.ClusterID != bundle.info.ClusterID || m.CAFingerprint != bundle.info.CAFingerprint || m.MemberAddress != address || m.MemberKey != memberKey || m.EnrollmentListen != enrollmentAddress {
		t.Fatalf("manifest identity = %+v", m)
	}
	if len(m.Members) != 1 || m.Members[0].Address != address || m.Members[0].Role != "voter" || m.Term == 0 || m.IndexFrom == 0 || m.IndexTo < m.IndexFrom {
		t.Fatalf("manifest membership and barriers = %+v term %d index %d..%d", m.Members, m.Term, m.IndexFrom, m.IndexTo)
	}
	if len(m.Migrations) == 0 || m.Migrations[len(m.Migrations)-1] != store.LatestMigration() {
		t.Fatalf("manifest migrations end at %v, the schema is at %d", m.Migrations[len(m.Migrations)-1:], store.LatestMigration())
	}
	archived := make(map[string]bool)
	for _, file := range m.Files {
		archived[file.Name] = true
	}
	for _, name := range []string{"database/gpuworks.db", "master.key", "cluster-enrollment/ca.key", "cluster-enrollment/server.key", "data/model-logos/fixture.png"} {
		if !archived[name] {
			t.Errorf("the archive lacks %s", name)
		}
	}
	if info, err := os.Stat(backup.Path); err != nil || info.Mode().Perm() != 0600 || info.Size() != backup.Size {
		t.Fatalf("archive file = %v, %v", info, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(backups, ".cluster-backup-*")); len(leftovers) != 0 {
		t.Fatalf("a finished backup left %v", leftovers)
	}
	listed, err := ListClusterBackups(backups)
	if err != nil || len(listed) != 1 || listed[0].Path != backup.Path {
		t.Fatalf("listed = %+v, %v", listed, err)
	}

	after, _, err := running.CreateNodePending("after-backup")
	if err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "data")
	entries := func() []string {
		names, _ := filepath.Glob(filepath.Join(filepath.Dir(restored), "*"))
		hidden, _ := filepath.Glob(filepath.Join(filepath.Dir(restored), ".*"))
		return append(names, hidden...)
	}
	options := ClusterRestoreOptions{Timeout: 120 * time.Second}

	// The member still runs: its database port answers.
	if _, err := RestoreClusterMember(ctx, Config{DataDir: restored}, backup.Path, options); !errors.Is(err, ErrClusterRestoreRefused) || !strings.Contains(err.Error(), address) {
		t.Fatalf("a restore beside a running member: %v", err)
	}
	if left := entries(); len(left) != 0 {
		t.Fatalf("a refused restore left %v", left)
	}
	if err := running.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true

	// The original data directory is not empty.
	if _, err := RestoreClusterMember(ctx, cfg, backup.Path, options); !errors.Is(err, ErrClusterRestoreRefused) || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("a restore over an existing data directory: %v", err)
	}
	// An archive with one changed byte is refused before anything is built.
	raw, err := os.ReadFile(backup.Path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0x01
	altered := filepath.Join(t.TempDir(), "altered.tar")
	if err := os.WriteFile(altered, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreClusterMember(ctx, Config{DataDir: restored}, altered, options); !errors.Is(err, ErrClusterBackupInvalid) {
		t.Fatalf("a restore from an altered archive: %v", err)
	}
	if left := entries(); len(left) != 0 {
		t.Fatalf("a restore from an altered archive left %v", left)
	}
	if _, err := RestoreClusterMember(ctx, Config{DataDir: restored}, backup.Path, ClusterRestoreOptions{}); err == nil {
		t.Fatal("a restore without a deadline was accepted")
	}

	result, err := RestoreClusterMember(ctx, Config{DataDir: restored}, backup.Path, options)
	if err != nil {
		t.Fatal(err)
	}
	if result.Member.Address != address || result.Member.Role != "voter" || len(result.Rejoin) != 0 {
		t.Fatalf("restore result = %+v rejoin %v", result.Member, result.Rejoin)
	}
	if left := entries(); len(left) != 1 || left[0] != restored {
		t.Fatalf("after a restore the parent holds %v, want only the data directory", left)
	}
	if got, err := os.ReadFile(filepath.Join(restored, "model-logos", "fixture.png")); err != nil || string(got) != "logo bytes" {
		t.Fatalf("restored material = %q, %v", got, err)
	}
	if info, err := os.Stat(restored); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("restored data directory mode = %v, %v", info.Mode(), err)
	}
	// A second restore onto the now complete directory is refused.
	if _, err := RestoreClusterMember(ctx, Config{DataDir: restored}, backup.Path, options); !errors.Is(err, ErrClusterRestoreRefused) {
		t.Fatalf("a second restore: %v", err)
	}

	reopened, err := store.OpenRuntime(ctx, filepath.Join(restored, "gpuworks.db"), bus.New(), store.ReplicatedBackend())
	if err != nil {
		t.Fatalf("the restored member does not open: %v", err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()
	if found, err := reopened.NodeByUUID(before.UUID); err != nil || found.Name != "before-backup" {
		t.Fatalf("the node written before the backup = %+v, %v", found, err)
	}
	if _, err := reopened.NodeByUUID(after.UUID); err == nil {
		t.Fatal("a node written after the backup survived the restore")
	}
	admissions, err := reopened.ClusterTransportAdmissions(ctx, bundle.info.ClusterID)
	if err != nil || len(admissions) != 1 || admissions[0].KeyFingerprint != memberKey || admissions[0].Address != address || admissions[0].RevokedAt != nil {
		t.Fatalf("restored admissions = %+v, %v", admissions, err)
	}
	identity, err := reopened.ClusterIdentity(ctx)
	if err != nil || identity.ClusterID != bundle.info.ClusterID {
		t.Fatalf("restored cluster identity = %+v, %v", identity, err)
	}
	// A restored member starts as a single member: its open store answers
	// the upgrade status, and a step that changes the cluster is refused
	// with a reason that says what to do in either case.
	observation, ok := reopened.SingleMemberObservation(nil)
	if !ok {
		t.Fatal("the restored member's open store does not answer the member questions")
	}
	if status, leader, _, err := observation.ClusterUpgrade(ctx, identity, memberKey, enrollment.ClusterUpgradeStatus); err != nil || !leader || status.Known == 0 || status.Applied != status.Known {
		t.Fatalf("restored member's upgrade status = %+v, leader %v, %v", status, leader, err)
	}
	_, _, _, err = observation.ClusterUpgrade(ctx, identity, memberKey, enrollment.ClusterUpgradeBegin)
	if !errors.Is(err, store.ErrClusterUpgradeRefused) || !errors.Is(err, store.ErrClusterUpgradeSingleMember) ||
		!strings.Contains(err.Error(), "restart it once") || !strings.Contains(err.Error(), "a cluster of one member applies its migrations when its new program starts and needs no upgrade step") {
		t.Fatalf("a step on the restored member: %v; want the single-member refusal", err)
	}
}
