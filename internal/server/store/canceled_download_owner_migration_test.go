package store

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// A row canceled before migration 247 cannot say whether its service withdrew
// it or an operator canceled it: both kinds kept the owner then. The migration
// reads every such row as the operator's, so the row stays canceled and no
// deployment starts it again; a person still can, from the models page.
func TestCanceledDownloadWithoutOwnerIsNotStartedAgainByADeployment(t *testing.T) {
	st, model, node, ref := downloadTransferFixture(t)
	withdrawn, err := st.UpsertModelFile(&ModelFile{ModelID: model.ID, NodeID: node.ID, StorageID: ref.StorageID, Path: "Qwen/withdrawn", State: proto.MFCanceled, Acquisition: proto.ModelAcquireDownload, TransferID: "old-transfer", DownloadDeploymentID: 17, Message: "Download canceled by user"})
	if err != nil {
		t.Fatal(err)
	}
	// A node holds one row per model: the active row needs a model of its own.
	other, err := st.UpsertModel(&Model{Source: "huggingface", SourceID: "test/other", Revision: "main", Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	running, err := st.UpsertModelFile(&ModelFile{ModelID: other.ID, NodeID: node.ID, StorageID: ref.StorageID, Path: "Qwen/running", State: proto.MFPending, Acquisition: proto.ModelAcquireDownload, TransferID: "active-transfer", DownloadDeploymentID: 17})
	if err != nil {
		t.Fatal(err)
	}
	// The store opened at this build has already recorded the migration; the
	// rows above stand in for an installation that upgrades with them. Run it
	// as that upgrade would, and again to show a second run changes nothing.
	migration := migrationByName(t, "canceled_downloads_without_owner")
	for attempt := 0; attempt < 2; attempt++ {
		if err := st.db.Where("version = ?", migration.Version).Delete(&SchemaMigration{}).Error; err != nil {
			t.Fatal(err)
		}
		if err := applyMigration(st.db, migration); err != nil {
			t.Fatalf("apply %d: %v", attempt, err)
		}
	}
	var markers int64
	if err := st.db.Model(&SchemaMigration{}).Where("version = ?", migration.Version).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("marker rows = %d, err = %v", markers, err)
	}
	canceled, err := st.ModelFileByID(withdrawn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if canceled.DownloadDeploymentID != 0 || canceled.State != proto.MFCanceled || canceled.WithdrawnWithService() {
		t.Fatalf("canceled row after the migration = %+v", canceled)
	}
	active, err := st.ModelFileByID(running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if active.DownloadDeploymentID != 17 {
		t.Fatalf("an active download lost its owner: %+v", active)
	}
	if _, err := st.ResumeWithdrawnDownload(canceled, 42); !errors.Is(err, ErrModelDownloadInactive) {
		t.Fatalf("a deployment started an operator's canceled download again: %v", err)
	}
	// The models page still starts it, as an independent request.
	retried, err := st.PrepareModelDownload(ref, "Qwen/withdrawn")
	if err != nil {
		t.Fatalf("the models page cannot download it again: %v", err)
	}
	if retried.State != proto.MFPending || retried.DownloadDeploymentID != 0 {
		t.Fatalf("models page retry = %+v", retried)
	}
}

func TestMigrationNumberForCanceledDownloadOwners(t *testing.T) {
	migration := migrationByName(t, "canceled_downloads_without_owner")
	if migration.Version != 247 {
		t.Fatalf("version = %d, want 247", migration.Version)
	}
	if LatestMigration() < migration.Version {
		t.Fatalf("latest migration %d is below %d", LatestMigration(), migration.Version)
	}
}
