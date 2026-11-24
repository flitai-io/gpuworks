package image

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

func TestImageRelayTransfersArchiveAndVerifiesLoad(t *testing.T) {
	env := testenv.New(t)
	source := env.AddNode("image-source", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("fake"), testenv.WithManagementAddress("127.0.0.1"))
	target := env.AddNode("image-target", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A10", VRAMMB: 16384}), testenv.WithDriverName("fake"), testenv.WithManagementAddress("127.0.0.1"))
	failing := env.AddNode("image-failing", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A10", VRAMMB: 16384}), testenv.WithDriverName("fake"), testenv.WithManagementAddress("127.0.0.1"))
	failing.Driver.SetImageAbsent(true)
	for _, name := range []string{"image-source", "image-target", "image-failing"} {
		env.WaitNodeOnline(t, name)
	}
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 3 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	// The pending-node UUID is the agent identity retained through enrollment;
	// resolve it directly so runtime handles and persisted rows cannot diverge.
	sourceRow, err := env.Store().NodeByUUID(source.UUID)
	if err != nil {
		t.Fatal(err)
	}
	targetRow, err := env.Store().NodeByUUID(target.UUID)
	if err != nil {
		t.Fatal(err)
	}
	failingRow, err := env.Store().NodeByUUID(failing.UUID)
	if err != nil {
		t.Fatal(err)
	}
	sourceNode, targetNode, failingNode := *sourceRow, *targetRow, *failingRow

	const archiveSize int64 = 8 << 20
	source.Driver.SetArchiveSize(archiveSize)
	img, err := env.Store().UpsertImage(&store.Image{ImageRef: "example/relay:test", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	// Keep the runtime inventory faithful to the persisted source row. Catalog
	// updates now trigger an immediate presence report, so a manually inserted
	// "present" row without a runtime image would correctly converge to absent.
	if err := source.Driver.PullImage(context.Background(), img.ImageRef, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertImageNode(&store.ImageNode{ImageID: img.ID, ImageVersionID: img.ID, NodeID: sourceNode.ID, State: store.ImageStatePresent, SizeBytes: archiveSize}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	sync := func(node store.Node) store.ImageNode {
		body, _ := json.Marshal(map[string]string{"source_node": sourceNode.UUID})
		resp, err := api.PostJSON("/api/v1/images/"+strconv.FormatUint(uint64(img.ID), 10)+"/nodes/"+node.UUID+"/sync", body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusAccepted {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("sync %s: %d %s", node.UUID, resp.StatusCode, b)
		}
		resp.Body.Close()
		var row store.ImageNode
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			got, getErr := env.Store().ImageNode(img.ID, node.ID)
			if getErr == nil {
				row = *got
				if row.State == store.ImageStatePresent || row.State == store.ImageStateFailed {
					return row
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("image sync did not finish: %+v", row)
		return store.ImageNode{}
	}
	got := sync(targetNode)
	if got.State != store.ImageStatePresent || got.TransferredBytes != archiveSize {
		t.Fatalf("relay row=%+v want present/%d", got, archiveSize)
	}
	failed := sync(failingNode)
	if failed.State != store.ImageStateFailed || !strings.Contains(failed.Message, "not present") {
		t.Fatalf("load verification row=%+v", failed)
	}
	// RetryImageSync must be a real recovery path, not merely an API alias for
	// the first attempt.  Make the runtime image available and ask the public
	// retry endpoint to re-run the source→target relay.
	// The image appears with the load the retry runs. Making it visible now
	// would let the node's next presence report mark the row present before
	// the retry arrives, and the retry would then pull instead of relaying.
	failing.Driver.FixImageAtNextLoad()
	retried, err := api.PostJSON("/api/v1/images/"+strconv.FormatUint(uint64(img.ID), 10)+"/nodes/"+failingNode.UUID+"/sync/retry", nil)
	if err != nil || retried.StatusCode != http.StatusAccepted {
		data := []byte{}
		if retried != nil {
			data, _ = io.ReadAll(retried.Body)
			retried.Body.Close()
		}
		t.Fatalf("retry sync status=%v body=%s err=%v", imageResponseStatus(retried), data, err)
	}
	retried.Body.Close()
	var recovered store.ImageNode
	env.Eventually(t, 60*time.Second, func() bool {
		got, getErr := env.Store().ImageNode(img.ID, failingNode.ID)
		if getErr != nil {
			return false
		}
		recovered = *got
		return recovered.State == store.ImageStatePresent || recovered.State == store.ImageStateFailed
	})
	if recovered.State != store.ImageStatePresent || recovered.TransferredBytes != archiveSize {
		t.Fatalf("retry relay row=%+v want present/%d", recovered, archiveSize)
	}
}

func imageResponseStatus(resp *http.Response) any {
	if resp == nil {
		return "<nil>"
	}
	return resp.StatusCode
}
