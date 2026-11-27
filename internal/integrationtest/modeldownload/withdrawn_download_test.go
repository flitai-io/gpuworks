package modeldownload

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// createDeployment deploys the fixture's model on its node and returns the
// deployment's id.
func (f *evictionFixture) createDeployment(t *testing.T, name string) uint {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model_id": f.model.ID, "node_uuid": f.node.UUID, "name": name, "engine": "vllm"})
	resp, err := f.api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %s: status=%d body=%s", name, resp.StatusCode, data)
	}
	var created struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(data, &created); err != nil || created.ID == 0 {
		t.Fatalf("create %s: body=%s err=%v", name, data, err)
	}
	return created.ID
}

// Deleting a service while the download it started is still running cancels
// that download on the node. Deploying the same model on the node again must
// download it: the node may not stay without the model until an operator
// retries by hand.
func TestDeployingAgainDownloadsWhatADeletedServiceLeftCanceled(t *testing.T) {
	f := newEvictionFixture(t, "withdrawn-download")
	if err := f.env.Store().DB().Model(&store.Model{}).Where("id = ?", f.model.ID).Updates(map[string]any{
		"arch": "Qwen3ForCausalLM", "weight_bytes": 1 << 20,
		"meta": `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	}).Error; err != nil {
		t.Fatal(err)
	}
	stalled := f.hub.arm(isWeightTransfer, false)
	first := f.createDeployment(t, "withdrawn-first")
	waitClosed(t, stalled.held, "the first service's weight transfer to start")
	f.downloadingPath(t)

	resp, err := f.api.Delete(fmt.Sprintf("/api/v1/deployments/%d", first))
	if err != nil || resp.StatusCode/100 != 2 {
		t.Fatalf("delete status=%v err=%v", responseStatus(resp), err)
	}
	resp.Body.Close()
	var canceled store.ModelFile
	f.env.Eventually(t, 30*time.Second, func() bool {
		got, err := f.env.Store().ModelFileByModelNode(f.model.ID, f.node.ID)
		if err != nil {
			return false
		}
		canceled = *got
		return canceled.State == proto.MFCanceled
	})
	waitClosed(t, stalled.abandoned, "the node to stop the canceled transfer")

	second := f.createDeployment(t, "withdrawn-second")
	mf := f.waitTerminal(t)
	if mf.State != proto.MFPresent || mf.DownloadDeploymentID != second || mf.TransferID == canceled.TransferID {
		t.Fatalf("model after deploying again = %s (owner %d, transfer %s; canceled transfer %s): %s",
			mf.State, mf.DownloadDeploymentID, mf.TransferID, canceled.TransferID, mf.Message)
	}
}
