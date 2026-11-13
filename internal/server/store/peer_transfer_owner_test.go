package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestPeerTransferOwnerFences(t *testing.T) {
	s := newTestStore(t)
	mf := &ModelFile{ModelID: 991, StorageID: s.DefaultLocalStorageID(), NodeID: 992, State: proto.MFSyncing, TransferID: "old"}
	if err := s.db.Create(mf).Error; err != nil {
		t.Fatal(err)
	}
	ref := FileRef{ModelID: mf.ModelID, StorageID: mf.StorageID, NodeID: mf.NodeID}
	changed, err := s.RestartModelSyncIfActive(ref, "old", "new", 9, 10)
	if err != nil || !changed {
		t.Fatalf("restart: %v %v", changed, err)
	}
	changed, err = s.RestartModelSyncIfActive(ref, "old", "late", 9, 10)
	if err != nil || changed {
		t.Fatalf("stale restart: %v %v", changed, err)
	}
	if err := s.SetModelSyncDownloadRequiredCode(ref, "old", "late failure", "PEER_RELAY_BUSY"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ModelFileByID(mf.ID)
	if got.TransferID != "new" || got.State != proto.MFSyncing {
		t.Fatalf("late failure: %+v", got)
	}
	row := &ImageNode{ImageVersionID: 991, NodeID: 992, State: ImageStateSyncing, TransferID: "new"}
	if err := s.db.Create(row).Error; err != nil {
		t.Fatal(err)
	}
	changed, err = s.CancelImageNodeTransferIfActive(991, 992, "old", "stale cancel", "PEER_TRANSFER_CANCELED")
	if err != nil || changed {
		t.Fatalf("stale cancel: %v %v", changed, err)
	}
	if err := s.FailImageSyncIfActiveCode(991, 992, "new", "busy", "PEER_RELAY_BUSY"); err != nil {
		t.Fatal(err)
	}
	gotImage, _ := s.ImageNode(991, 992)
	if gotImage.TransferErrorCode != "PEER_RELAY_BUSY" {
		t.Fatalf("code lost: %+v", gotImage)
	}
}

func TestPeerTransferErrorCodeMigrationAtomic(t *testing.T) {
	s := newTestStore(t)
	m := migrationByName(t, "peer_transfer_ownership")
	for _, table := range []string{"model_files", "image_nodes"} {
		if err := dropColumns(s.db, table, "transfer_error_code"); err != nil {
			t.Fatal(err)
		}
	}
	if err := dropColumns(s.db, "image_nodes", "last_transfer_id"); err != nil {
		t.Fatal(err)
	}
	s.db.Delete(&SchemaMigration{}, m.Version)
	broken := m
	boom := errors.New("after schema")
	broken.Apply = func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return boom
	}
	if err := applyMigration(s.db, broken); !errors.Is(err, boom) {
		t.Fatalf("rollback: %v", err)
	}
	for _, table := range []string{"model_files", "image_nodes"} {
		if s.db.Migrator().HasColumn(table, "transfer_error_code") {
			t.Fatalf("retained %s column", table)
		}
	}
	if s.db.Migrator().HasColumn("image_nodes", "last_transfer_id") {
		t.Fatal("retained last attempt after rollback")
	}
	var n int64
	s.db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&n)
	if n != 0 {
		t.Fatal("retained marker")
	}
	for range 2 {
		if err := migrateDB(s.db, []migration{m}); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"model_files", "image_nodes"} {
		if !s.db.Migrator().HasColumn(table, "transfer_error_code") {
			t.Fatalf("missing %s column", table)
		}
	}
	if !s.db.Migrator().HasColumn("image_nodes", "last_transfer_id") {
		t.Fatal("missing completed attempt identity")
	}

}

func TestPeerTransferExplicitZeroRateSurvivesTerminalStatusAndRepeatedCancel(t *testing.T) {
	s := newTestStore(t)
	mf := &ModelFile{ModelID: 991, StorageID: s.DefaultLocalStorageID(), NodeID: 992, State: proto.MFSyncing, TransferID: "zero", RateLimit: 0, LastRateLimit: 99}
	if err := s.db.Create(mf).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateModelFileStatus(992, proto.ModelFileStatus{Key: ModelFileKey(mf.ID), TransferID: "zero", State: proto.MFFailed}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ModelFileByID(mf.ID)
	if got.LastRateLimit != 0 {
		t.Fatalf("model unlimited rate resurrected old limit: %+v", got)
	}
	if err := s.BeginModelSync(FileRef{ModelID: 991, StorageID: mf.StorageID, NodeID: 992}, 1, "limited", 10); err != nil {
		t.Fatal(err)
	}
	_ = s.SetModelSyncRate(FileRef{ModelID: 991, StorageID: mf.StorageID, NodeID: 992}, 88)
	for range 2 {
		if err := s.SetModelSyncDownloadRequiredCode(FileRef{ModelID: 991, StorageID: mf.StorageID, NodeID: 992}, "limited", "stop", "PEER_TRANSFER_CANCELED"); err != nil {
			t.Fatal(err)
		}
	}
	got, _ = s.ModelFileByID(mf.ID)
	if got.LastRateLimit != 88 {
		t.Fatalf("repeated cancel lost current limit: %+v", got)
	}
	image := &ImageNode{ImageVersionID: 991, NodeID: 992, State: ImageStateSyncing, Source: "peer", TransferID: "zero", LastRateLimitKiB: 99}
	if err := s.db.Create(image).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyImageNodeStatus(991, 992, proto.ImageNodeStatus{TransferID: "zero", State: ImageStateFailed}); err != nil {
		t.Fatal(err)
	}
	row, _ := s.ImageNode(991, 992)
	if row.LastRateLimitKiB != 0 {
		t.Fatalf("image unlimited rate resurrected old limit: %+v", row)
	}
}

func TestPeerTransferCompletedImageRejectsRetiredTerminalButKeepsCurrentLoadFailure(t *testing.T) {
	s := newTestStore(t)
	row := &ImageNode{ImageVersionID: 991, NodeID: 992, State: ImageStateSyncing, TransferID: "old", Source: "peer"}
	if err := s.db.Create(row).Error; err != nil {
		t.Fatal(err)
	}
	if changed, err := s.RestartImageSyncIfActive(991, 992, "old", "current", 1, 10, 0); err != nil || !changed {
		t.Fatalf("restart=%v %v", changed, err)
	}
	if err := s.ApplyImageNodeStatus(991, 992, proto.ImageNodeStatus{TransferID: "current", State: ImageStatePresent}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyImageNodeStatus(991, 992, proto.ImageNodeStatus{TransferID: "old", State: ImageStateFailed, Message: "retired"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ImageNode(991, 992)
	if got.State != ImageStatePresent || got.LastTransferID != "current" || got.TransferID != "" {
		t.Fatalf("retired terminal overrode completed retry: %+v", got)
	}
	if err := s.ApplyImageNodeStatus(991, 992, proto.ImageNodeStatus{TransferID: "current", State: ImageStateFailed, Message: "current load failure"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ImageNode(991, 992)
	if got.State != ImageStateFailed || got.Message != "current load failure" {
		t.Fatalf("current terminal rejected: %+v", got)
	}
	if err := s.MarkImageNodeAbsent(991, 992, "removed"); err != nil {
		t.Fatal(err)
	}
	_ = s.ApplyImageNodeStatus(991, 992, proto.ImageNodeStatus{TransferID: "old", State: ImageStatePresent})
	got, _ = s.ImageNode(991, 992)
	if got.State != ImageStateAbsent || got.LastTransferID != "current" {
		t.Fatalf("removal erased completed attempt fence: %+v", got)
	}

}
