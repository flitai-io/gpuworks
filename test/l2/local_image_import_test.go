//go:build multiproc

package l2

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestLocalImageImportRetriesNodeFileWithoutFetchingURL(t *testing.T) {
	c := startArchiveImportCluster(t, "registry.example/l2-local-runner:v1")
	version := c.importArchiveImage(t, "l2-local-runner")
	path := filepath.Join(t.TempDir(), "node image.tar")
	if err := os.WriteFile(path, c.archive, 0o600); err != nil {
		t.Fatal(err)
	}
	syncPath := fmt.Sprintf("/api/v1/images/%d/nodes/%s/sync", version.ID, c.node.UUID)
	post := func(route string, body any, want int) []byte {
		t.Helper()
		payload, _ := json.Marshal(body)
		resp, err := authedPost(t, c.base, c.adminToken, route, payload)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != want {
			t.Fatalf("POST %s: status=%d body=%s err=%v", route, resp.StatusCode, data, err)
		}
		return data
	}
	post(syncPath, map[string]string{"source": "local", "archive_path": filepath.Dir(path)}, http.StatusConflict)
	failed := c.nodeRow(t, findArchiveImportVersion(t, c.base, c.adminToken, c.imageRef))
	if failed.State != "failed" || failed.Source != "local" || failed.TransferID == "" {
		t.Fatalf("invalid node file did not leave a retryable local failure: %+v", failed)
	}
	data := post(syncPath+"/retry", map[string]any{}, http.StatusBadRequest)
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(data, &problem); err != nil || problem.Code != "IMAGE_ARCHIVE_PATH_REQUIRED" {
		t.Fatalf("empty retry must require the file again: %s", data)
	}
	post(syncPath+"/retry", map[string]string{"source": "local", "archive_path": "  " + path + "  "}, http.StatusAccepted)
	deadline := time.Now().Add(30 * time.Second)
	for {
		current := findArchiveImportVersion(t, c.base, c.adminToken, c.imageRef)
		row := c.nodeRow(t, current)
		if row.TransferID != failed.TransferID && row.State == "present" {
			if row.Source != "local" || row.SizeBytes != int64(len(c.archive)) || row.TransferredBytes != row.SizeBytes {
				t.Fatalf("local import result = %+v", row)
			}
			if current.ID != version.ID || current.SeriesID != version.SeriesID || !reflect.DeepEqual(current.RegistrySources, version.RegistrySources) {
				t.Fatalf("node file import changed catalog identity or acquisition addresses: %+v", current)
			}
			break
		}
		if (row.TransferID != failed.TransferID && row.State == "failed") || time.Now().After(deadline) {
			t.Fatalf("local retry did not complete: %+v", row)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if original, err := os.ReadFile(path); err != nil || !bytes.Equal(original, c.archive) {
		t.Fatalf("the original archive was removed or changed: %v", err)
	}
	if requests := c.site.Requests(); len(requests) != 0 {
		t.Fatalf("local import or retry fell back to the declared URL: %v", requests)
	}
	assertNoStagedArchives(t, c.agentState)
}
