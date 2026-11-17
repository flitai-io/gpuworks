//go:build kind

package l3kind

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
	"gpuworks.ai/gpuworks/test/ocifixture"
)

type localImageVersion struct {
	ID       uint   `json:"id"`
	SeriesID uint   `json:"series_id"`
	ImageRef string `json:"image_ref"`
	Nodes    []struct {
		NodeUUID  string `json:"node_uuid"`
		State     string `json:"state"`
		Source    string `json:"source"`
		Message   string `json:"message"`
		SizeBytes int64  `json:"size_bytes"`
	} `json:"nodes"`
}

// Run on the enrolled DaemonSet before deployment so no workload or registry
// pull can make this image present on behalf of the local import.
func testKubeLocalImageImport(t *testing.T, parentAPI *apiClient, k *kubectl, nodeUUID string) {
	api := &apiClient{t: t, base: parentAPI.base, token: parentAPI.token}
	const canonical = "registry.example/gpuworks/kind-local:catalog"
	archive, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{
		RepoTags: []string{"registry.example/gpuworks/kind-local:archive"}, LayerBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(t.TempDir(), "node image.tar")
	if err := os.WriteFile(localPath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	hostPath := "/tmp/gpuworks node image-" + nodeUUID + ".tar"
	mustRun(t, time.Minute, "docker", "cp", localPath, k.node+":"+hostPath)
	pods := k.agentPods()
	if len(pods) != 1 {
		t.Fatalf("agent pods = %d", len(pods))
	}
	// The archive exists only on the worker host, outside the state hostPath.
	k.mustRun(time.Minute, "-n", kubeNamespace, "exec", pods[0].Metadata.Name, "--", "sh", "-c", `test ! -e "$1"`, "check", hostPath)
	api.mustJSON(http.MethodPost, "/api/v1/images/import", map[string]any{"series": []map[string]any{{
		"name": "kind-local-image", "engine": "vllm", "purpose": "inference", "source": "custom",
		"versions": []map[string]any{{"image_ref": canonical, "source": "custom", "arch": "multi", "command_mode": "manual"}},
	}}}, 0, nil)
	status, body, err := api.do(http.MethodGet, "/api/v1/images/versions", nil)
	var versions []localImageVersion
	if err != nil || status != http.StatusOK || apiwire.UnmarshalList(body, &versions) != nil {
		t.Fatalf("list versions: status=%d err=%v body=%s", status, err, body)
	}
	index := slices.IndexFunc(versions, func(v localImageVersion) bool { return v.ImageRef == canonical })
	if index < 0 {
		t.Fatal("imported catalog version missing")
	}
	version := versions[index]
	syncPath := fmt.Sprintf("/api/v1/images/%d/nodes/%s/sync", version.ID, nodeUUID)
	api.mustJSON(http.MethodPost, syncPath, map[string]string{"source": "local", "archive_path": "/tmp"}, http.StatusConflict, nil)
	api.mustJSON(http.MethodPost, syncPath+"/retry", map[string]string{"source": "local", "archive_path": hostPath}, http.StatusAccepted, nil)
	eventually(t, "host-only archive imported by the kube agent", time.Minute, 200*time.Millisecond, func() (bool, string) {
		var series struct {
			Versions []localImageVersion `json:"versions"`
		}
		status, body, err := api.do(http.MethodGet, fmt.Sprintf("/api/v1/images/%d", version.SeriesID), nil)
		if err != nil || status != http.StatusOK || json.Unmarshal(body, &series) != nil {
			return false, fmt.Sprintf("status=%d err=%v body=%s", status, err, body)
		}
		for _, v := range series.Versions {
			if v.ID != version.ID {
				continue
			}
			for _, row := range v.Nodes {
				if row.NodeUUID == nodeUUID {
					return row.State == "present" && row.Source == "local" && row.SizeBytes == int64(len(archive)), fmt.Sprintf("%+v", row)
				}
			}
		}
		return false, "node image row missing"
	})
	images := mustRun(t, time.Minute, "docker", "exec", k.node, "ctr", "-n", "k8s.io", "images", "ls", "-q")
	if !slices.Contains(strings.Fields(images), canonical) {
		t.Fatalf("canonical image missing from kubelet's image store: %s", images)
	}
	other := mustRun(t, time.Minute, "docker", "exec", k.node, "ctr", "-n", "gpuworks", "images", "ls", "-q")
	if slices.Contains(strings.Fields(other), canonical) {
		t.Fatal("kube import wrote to the standalone nerdctl namespace")
	}
	checksum := mustRun(t, time.Minute, "docker", "exec", k.node, "sha256sum", hostPath)
	if fields := strings.Fields(checksum); len(fields) == 0 || fields[0] != fmt.Sprintf("%x", sha256.Sum256(archive)) {
		t.Fatalf("host archive changed or was removed: %s", checksum)
	}
}
