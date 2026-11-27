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

// A service's withdrawal keeps the owner on the canceled row, which is what
// lets a later deployment start it again; an operator's cancel clears it.
// The resume names a new transfer and refuses a row that changed since it was
// read.
func TestOnlyADownloadWithdrawnWithItsServiceResumes(t *testing.T) {
	st, _, _, ref := downloadTransferFixture(t)
	row, err := st.PrepareModelDownload(ref, "Qwen/a")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDownloadDeployment(row.ID, row.TransferID, 17); err != nil {
		t.Fatal(err)
	}
	withdrawn, err := st.CancelModelDownload(row.ID, row.TransferID, "service deleted", 17)
	if err != nil || !withdrawn.WithdrawnWithService() {
		t.Fatalf("withdrawn = %+v, %v", withdrawn, err)
	}
	resumed, err := st.ResumeWithdrawnDownload(withdrawn, 18)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != proto.MFPending || resumed.Acquisition != proto.ModelAcquireAuto || resumed.DownloadDeploymentID != 18 ||
		resumed.TransferID != "" || resumed.Message != "" || resumed.Path != withdrawn.Path {
		t.Fatalf("resumed = %+v", resumed)
	}
	if _, err := st.ResumeWithdrawnDownload(withdrawn, 19); err != ErrModelDownloadInactive {
		t.Fatalf("a second resume of the same canceled row: %v", err)
	}

	again, err := st.PrepareModelDownload(ref, "Qwen/a")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDownloadDeployment(again.ID, again.TransferID, 18); err != nil {
		t.Fatal(err)
	}
	byOperator, err := st.CancelModelDownload(again.ID, again.TransferID, "Download canceled by user")
	if err != nil {
		t.Fatal(err)
	}
	if byOperator.DownloadDeploymentID != 0 || byOperator.WithdrawnWithService() {
		t.Fatalf("an operator's cancel kept the service as owner: %+v", byOperator)
	}
	if _, err := st.ResumeWithdrawnDownload(byOperator, 18); err != ErrModelDownloadInactive {
		t.Fatalf("resume of an operator's cancel: %v", err)
	}
}
