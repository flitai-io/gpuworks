package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestCanceledDownloadNeverResumesOnReconnectOrLateReport(t *testing.T) {
	st, model, node, ref := downloadTransferFixture(t)
	row, err := st.UpsertModelFile(&ModelFile{ModelID: model.ID, NodeID: node.ID, StorageID: ref.StorageID, Path: "Qwen/a", State: "canceled", Acquisition: proto.ModelAcquireDownload, TransferID: "cancel-fence", Message: "Download canceled by user"})
	if err != nil {
		t.Fatal(err)
	}
	task, ok := st.ModelFileTask(row, model)
	if !ok || task.DesiredState == "present" {
		t.Fatalf("canceled download resumes on reconnect: task=%+v ok=%v", task, ok)
	}
	for _, state := range []string{proto.MFDownloading, proto.MFPresent, proto.MFFailed} {
		if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{Key: ModelFileKey(row.ID), State: state, Acquisition: proto.ModelAcquireDownload}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := st.ModelFileByID(row.ID)
	if got.State != "canceled" {
		t.Fatalf("late legacy report revived canceled download: %+v", got)
	}
}

func TestCancelModelDownloadFencesAndRetries(t *testing.T) {
	st, _, node, ref := downloadTransferFixture(t)
	row, err := st.PrepareModelDownload(ref, "Qwen/a")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDownloadDeployment(row.ID, row.TransferID, 17); err != nil {
		t.Fatal(err)
	}
	canceled, err := st.CancelModelDownload(row.ID, row.TransferID, "Download canceled by user")
	if err != nil {
		t.Fatal(err)
	}
	if canceled.State != proto.MFCanceled || canceled.TransferID != row.TransferID || canceled.Message == "" || canceled.RateBPS != 0 {
		t.Fatalf("canceled=%+v", canceled)
	}
	if _, err := st.CancelModelDownload(row.ID, row.TransferID, "again"); err != nil {
		t.Fatalf("cancel not idempotent: %v", err)
	}
	if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{Key: ModelFileKey(row.ID), State: proto.MFPresent, TransferID: row.TransferID}); err != nil {
		t.Fatal(err)
	}
	retried, err := st.PrepareModelDownload(ref, "Qwen/a")
	if err != nil {
		t.Fatal(err)
	}
	if retried.State != proto.MFPending || retried.TransferID == canceled.TransferID || retried.DownloadDeploymentID != 0 {
		t.Fatalf("retry=%+v", retried)
	}
	if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{Key: ModelFileKey(row.ID), State: proto.MFPresent, TransferID: row.TransferID}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.ModelFileByID(row.ID)
	if got.State != proto.MFPending {
		t.Fatalf("stale completion replaced retry: %+v", got)
	}
}

func TestManualAdoptionFencesServiceDeletion(t *testing.T) {
	st, _, _, ref := downloadTransferFixture(t)
	row, err := st.PrepareModelDownload(ref, "Qwen/a")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDownloadDeployment(row.ID, row.TransferID, 17); err != nil {
		t.Fatal(err)
	}
	// An explicit user request adopts the exact running transfer. A service
	// deletion that already read the old owner must not cancel it afterwards.
	if err := st.SetDownloadDeployment(row.ID, row.TransferID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CancelModelDownload(row.ID, row.TransferID, "service deleted", 17); err != ErrModelDownloadInactive {
		t.Fatalf("deletion canceled adopted task: %v", err)
	}
	if err := st.SetDownloadDeployment(row.ID, row.TransferID, 18, 17); err != nil {
		t.Fatal(err)
	}
	got, _ := st.ModelFileByID(row.ID)
	if got.State != proto.MFPending || got.DownloadDeploymentID != 0 {
		t.Fatalf("stale ownership write changed user task: %+v", got)
	}
}
