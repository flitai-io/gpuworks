package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// A database whose nodes recorded recovery errors before codes gains the
// code column; the old rows keep their text and read with no code.
func TestNodeLastRecoveryCodeMigration(t *testing.T) {
	s := newTestStore(t)
	if err := s.db.Create(&Node{UUID: "legacy", RecoveryPhase: RecoveryPhaseUnreachable, RecoveryAttempts: 2, LastRecoveryError: "network: ssh dial: connection refused"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := dropColumns(s.db, "nodes", "last_recovery_code"); err != nil {
		t.Fatal(err)
	}
	m := migrationByName(t, "node_last_recovery_code")
	if err := s.db.Exec("DELETE FROM schema_migrations WHERE version = ?", m.Version).Error; err != nil {
		t.Fatal(err)
	}

	boom := errors.New("after column")
	failed := m
	failed.Apply = func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return boom
	}
	if err := applyMigration(s.db, failed); !errors.Is(err, boom) {
		t.Fatalf("rollback = %v", err)
	}
	if s.db.Migrator().HasColumn(&Node{}, "LastRecoveryCode") {
		t.Fatal("the failed transaction kept the column")
	}
	var count int64
	if err := s.db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("the failed transaction kept the marker: %d %v", count, err)
	}

	for range 2 {
		if err := migrateDB(s.db, []migration{m}); err != nil {
			t.Fatal(err)
		}
	}
	if !s.db.Migrator().HasColumn(&Node{}, "LastRecoveryCode") {
		t.Fatal("nodes.last_recovery_code missing")
	}
	var marker SchemaMigration
	if err := s.db.First(&marker, m.Version).Error; err != nil || marker.Name != "node_last_recovery_code" {
		t.Fatalf("marker = %+v %v", marker, err)
	}
	node, err := s.NodeByUUID("legacy")
	if err != nil || node.LastRecoveryCode != "" || node.LastRecoveryError != "network: ssh dial: connection refused" || node.RecoveryAttempts != 2 {
		t.Fatalf("legacy node = %q / %q / %d (%v)", node.LastRecoveryCode, node.LastRecoveryError, node.RecoveryAttempts, err)
	}

	// A new failure records its code; clearing removes both.
	if err := s.SetNodeRecoveryPhase("legacy", RecoveryPhaseProbeFailed, 3, nil, RecoveryError{Code: messages.ErrorSSHAuthFailed, Message: "ssh: unable to authenticate"}); err != nil {
		t.Fatal(err)
	}
	node, _ = s.NodeByUUID("legacy")
	if node.LastRecoveryCode != messages.ErrorSSHAuthFailed || node.LastRecoveryError != "ssh: unable to authenticate" {
		t.Fatalf("recorded = %q / %q", node.LastRecoveryCode, node.LastRecoveryError)
	}
	if err := s.ClearNodeRecovery("legacy"); err != nil {
		t.Fatal(err)
	}
	node, _ = s.NodeByUUID("legacy")
	if node.LastRecoveryCode != "" || node.LastRecoveryError != "" {
		t.Fatalf("cleared = %q / %q", node.LastRecoveryCode, node.LastRecoveryError)
	}

	// After a rollback the next start applies it.
	if err := dropColumns(s.db, "nodes", "last_recovery_code"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Exec("DELETE FROM schema_migrations WHERE version = ?", m.Version).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(s.db, []migration{m}); err != nil || !s.db.Migrator().HasColumn(&Node{}, "LastRecoveryCode") {
		t.Fatalf("restart after rollback: %v", err)
	}
}
