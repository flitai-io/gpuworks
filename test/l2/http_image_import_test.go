//go:build multiproc

package l2

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
	"gpuworks.ai/gpuworks/test/ocifixture"
)

type archiveImportVersion struct {
	ID              uint     `json:"id"`
	SeriesID        uint     `json:"series_id"`
	ImageRef        string   `json:"image_ref"`
	RegistrySources []string `json:"registry_sources"`
	Nodes           []struct {
		NodeUUID         string `json:"node_uuid"`
		State            string `json:"state"`
		Source           string `json:"source"`
		Message          string `json:"message"`
		SizeBytes        int64  `json:"size_bytes"`
		TransferredBytes int64  `json:"transferred_bytes"`
		TransferID       string `json:"transfer_id"`
	} `json:"nodes"`
}

// TestHTTPImageImportDirect is l2-http-image-import-direct: a real server and a
// real agent, a `docker save` archive served over plain HTTP, and the whole
// path from "paste a URL" to "the image is on that node". There is no docker
// here, so the archive is built in-process in the layout docker save writes and
// the agent runs the explicit recording runtime.
// archiveImportCluster is one server, one agent on the recording runtime, and
// one archive site: the fixture both archive-import cases are told from.
type archiveImportCluster struct {
	base       string
	adminToken string
	node       processNode
	agentState string
	dataDir    string
	archive    []byte
	archiveURL string
	site       *ocifixture.ArchiveServer
	imageRef   string
}

func startArchiveImportCluster(t *testing.T, imageRef string) *archiveImportCluster {
	t.Helper()
	ensureEmbeddedAgentFor(t, runtime.GOARCH)
	serverBin := buildBinary(t, "./cmd/gpuworks")
	agentBin := buildBinary(t, "./cmd/gpuworks-agent")

	archive, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{RepoTags: []string{imageRef}, LayerBytes: 512 << 10})
	if err != nil {
		t.Fatal(err)
	}
	site, siteServer := ocifixture.StartArchiveServer(archive)
	t.Cleanup(siteServer.Close)

	port := freePort(t)
	base := "http://127.0.0.1:" + port
	dataDir := t.TempDir()
	server := serverCommand(t, "multiproc-password", serverBin, "serve", "--config", loopbackOutboundConfig(t), "--addr", "127.0.0.1:"+port, "--data", dataDir)
	server.Stdout, server.Stderr = io.Discard, io.Discard
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(server) })
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout, func(response *http.Response) bool {
		return response.StatusCode == http.StatusUnauthorized
	})
	adminToken := login(t, base)
	seed, nodeToken := createNode(t, base, adminToken, "http-image-import")

	agentState := filepath.Join(t.TempDir(), "agent-state")
	agent := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken, "--state", agentState)
	agent.Env = append(os.Environ(),
		"GPUWORKS_FAKE_GPU=A100x1,81920",
		"GPUWORKS_FAKE_RUNTIME_RECORD="+filepath.Join(t.TempDir(), "workload.json"),
		// The node must start without this image, or there would be nothing to
		// observe arriving.
		"GPUWORKS_FAKE_RUNTIME_ABSENT_IMAGES="+imageRef,
		"GPUWORKS_LOG_LEVEL=warn",
	)
	agent.Stdout, agent.Stderr = io.Discard, io.Discard
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(agent) })
	node := waitNode(t, base, adminToken, func(candidate processNode) bool {
		return candidate.UUID == seed.UUID && candidate.State == "online"
	})
	return &archiveImportCluster{
		base: base, adminToken: adminToken, node: node, agentState: agentState, dataDir: dataDir,
		archive: archive, archiveURL: siteServer.URL + "/image.tar", site: site, imageRef: imageRef,
	}
}

// importArchiveImage registers the archive's reference in the catalog with the
// archive URL declared on it, the way the import dialog does after a preview.
func (c *archiveImportCluster) importArchiveImage(t *testing.T, name string) archiveImportVersion {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"series": []map[string]any{{
		"name": name, "engine": "vllm", "purpose": "inference", "source": "custom",
		"versions": []map[string]any{{
			"image_ref": c.imageRef, "source": "custom", "arch": "multi",
			"command_mode": "manual", "registry_sources": []string{c.archiveURL},
		}},
	}}})
	response, err := authedPost(t, c.base, c.adminToken, "/api/v1/images/import", body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("import status=%d body=%s", response.StatusCode, raw)
	}
	response.Body.Close()
	return findArchiveImportVersion(t, c.base, c.adminToken, c.imageRef)
}

// fetchOnNode issues the distribution request the node table sends and waits
// for the ledger row to reach a terminal state.
func (c *archiveImportCluster) fetchOnNode(t *testing.T, versionID uint, relay bool) archiveImportVersion {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"source": "url", "archive_url": c.archiveURL, "relay": relay})
	response, err := authedPost(t, c.base, c.adminToken, fmt.Sprintf("/api/v1/images/%d/nodes/%s/sync", versionID, c.node.UUID), body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("sync status=%d body=%s", response.StatusCode, raw)
	}
	response.Body.Close()

	deadline := time.Now().Add(60 * time.Second)
	var final archiveImportVersion
	for time.Now().Before(deadline) {
		final = findArchiveImportVersion(t, c.base, c.adminToken, c.imageRef)
		for _, row := range final.Nodes {
			if row.NodeUUID == c.node.UUID && (row.State == "present" || row.State == "failed") {
				return final
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return final
}

// nodeRow returns this node's ledger row, failing when there is none.
func (c *archiveImportCluster) nodeRow(t *testing.T, version archiveImportVersion) struct {
	NodeUUID         string `json:"node_uuid"`
	State            string `json:"state"`
	Source           string `json:"source"`
	Message          string `json:"message"`
	SizeBytes        int64  `json:"size_bytes"`
	TransferredBytes int64  `json:"transferred_bytes"`
	TransferID       string `json:"transfer_id"`
} {
	t.Helper()
	for _, row := range version.Nodes {
		if row.NodeUUID == c.node.UUID {
			return row
		}
	}
	t.Fatalf("no ledger row for %s: %+v", c.node.UUID, version.Nodes)
	return version.Nodes[0]
}

// TestHTTPImageImportDirect is l2-http-image-import-direct: a real server and a
// real agent, a `docker save` archive served over plain HTTP, and the whole
// path from "paste a URL" to "the image is on that node". There is no docker
// here, so the archive is built in-process in the layout docker save writes and
// the agent runs the explicit recording runtime.
func TestHTTPImageImportDirect(t *testing.T) {
	const imageRef = "gpustack/l2-runner:cuda13.0-vllm0.25.1"
	c := startArchiveImportCluster(t, imageRef)
	base, adminToken, archive, archiveURL, site := c.base, c.adminToken, c.archive, c.archiveURL, c.site

	// 1. Preview reads the real reference out of the archive's tail.
	preview := struct {
		URL         string   `json:"url"`
		SizeBytes   int64    `json:"size_bytes"`
		RepoTags    []string `json:"repo_tags"`
		Previewable bool     `json:"previewable"`
	}{}
	previewBody, _ := json.Marshal(map[string]string{"url": archiveURL})
	response, err := authedPost(t, base, adminToken, "/api/v1/images/preview-archive-url", previewBody)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&preview) != nil {
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("preview status=%d body=%s", response.StatusCode, raw)
	}
	response.Body.Close()
	if !preview.Previewable || len(preview.RepoTags) != 1 || preview.RepoTags[0] != imageRef {
		t.Fatalf("preview = %+v, want the archive's own reference", preview)
	}
	if preview.SizeBytes != int64(len(archive)) {
		t.Fatalf("preview size = %d, want %d", preview.SizeBytes, len(archive))
	}
	for _, request := range site.Requests() {
		if strings.HasPrefix(request, "GET") && !strings.Contains(request, "bytes=") {
			t.Fatalf("preview downloaded the whole archive: %q", request)
		}
	}

	// 2. Import that reference with the archive URL declared on it, the way the
	// import dialog does after a successful preview.
	version := c.importArchiveImage(t, "l2-runner")
	if len(version.RegistrySources) != 1 || version.RegistrySources[0] != archiveURL {
		t.Fatalf("registry sources = %v, want the archive URL", version.RegistrySources)
	}

	// 3. Ask the node to fetch it from that URL itself.
	row := c.nodeRow(t, c.fetchOnNode(t, version.ID, false))
	if row.State != "present" {
		t.Fatalf("image node = %+v, want present", row)
	}
	if row.Source != "url" {
		t.Fatalf("image node source = %q, want url", row.Source)
	}
	if row.SizeBytes != int64(len(archive)) {
		t.Fatalf("image node size = %d, want %d", row.SizeBytes, len(archive))
	}

	// 4. The archive really was downloaded from the site, and the staged copy
	// is gone: a multi-gigabyte tar must not survive a successful load.
	downloaded := false
	for _, request := range site.Requests() {
		if strings.HasPrefix(request, "GET /image.tar") {
			downloaded = true
		}
	}
	if !downloaded {
		t.Fatalf("the node never downloaded the archive: %v", site.Requests())
	}
	assertNoStagedArchives(t, c.agentState)
}

// TestHTTPImageImportRelay is l2-http-image-import-relay: the same site and the
// same archive, fetched by the management server and streamed into the node's
// load, for a node that cannot reach the mirror itself. The point of the case
// is that the bytes pass through the server without ever landing on its disk.
func TestHTTPImageImportRelay(t *testing.T) {
	const imageRef = "gpustack/l2-relay-runner:cuda13.0-vllm0.25.1"
	c := startArchiveImportCluster(t, imageRef)

	version := c.importArchiveImage(t, "l2-relay-runner")
	row := c.nodeRow(t, c.fetchOnNode(t, version.ID, true))
	if row.State != "present" {
		t.Fatalf("image node = %+v, want present", row)
	}
	// The recorded source is what tells an operator which way the archive came,
	// and it is the only durable difference between the two paths.
	if row.Source != "url-relay" {
		t.Fatalf("image node source = %q, want url-relay", row.Source)
	}

	// The server fetched the archive itself: the node never contacted the site
	// on this path, so exactly one whole-body GET was made, by the server.
	wholeBodyGets := 0
	for _, request := range c.site.Requests() {
		if strings.HasPrefix(request, "GET /image.tar") && !strings.Contains(request, "bytes=") {
			wholeBodyGets++
		}
	}
	if wholeBodyGets != 1 {
		t.Fatalf("whole-body GETs = %d, want exactly the relay's one: %v", wholeBodyGets, c.site.Requests())
	}

	// Nothing was staged: the relay copies from the HTTP response straight into
	// the load stream, so the server's data directory gained no archive.
	assertNoStagedArchives(t, c.dataDir)
	// And the node did not stage one either: it loaded from the stream.
	assertNoStagedArchives(t, c.agentState)
}

// findArchiveImportVersion reads the version with its node rows: the catalog
// list names the version's series, and the series detail carries the rows.
func findArchiveImportVersion(t *testing.T, base, token, imageRef string) archiveImportVersion {
	t.Helper()
	get := func(path string, decode func([]byte) error) {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := processHTTPClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status=%d err=%v", path, response.StatusCode, err)
		}
		if err := decode(data); err != nil {
			t.Fatalf("GET %s: decode: %v (%s)", path, err, data)
		}
	}
	var listed []archiveImportVersion
	get("/api/v1/images/versions", func(data []byte) error { return apiwire.UnmarshalList(data, &listed) })
	for _, version := range listed {
		if version.ImageRef != imageRef {
			continue
		}
		var series struct {
			Versions []archiveImportVersion `json:"versions"`
		}
		get(fmt.Sprintf("/api/v1/images/%d", version.SeriesID), func(data []byte) error { return json.Unmarshal(data, &series) })
		for _, detailed := range series.Versions {
			if detailed.ImageRef == imageRef {
				return detailed
			}
		}
	}
	t.Fatalf("image %s is not in the catalog", imageRef)
	return archiveImportVersion{}
}

// assertNoStagedArchives walks the agent's whole state directory, so the check
// holds wherever the runtime's image store turned out to be.
func assertNoStagedArchives(t *testing.T, stateDir string) {
	t.Helper()
	var leftovers []string
	err := filepath.WalkDir(stateDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".tar") || strings.HasSuffix(name, ".tar.part") || strings.HasSuffix(name, ".tar.part.state") {
			leftovers = append(leftovers, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk agent state: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("staged image archives left behind: %v", leftovers)
	}
}

// TestHTTPImageDeployAcquireURL is l2-http-image-deploy-acquire-url: an image
// imported from an archive URL is loaded onto the node from the images page,
// and a deployment of it then finds it there. A deployment no longer names
// where its image comes from (ADR 0048); the archive is loaded the one way an
// operator asks for it, and the node fetches it itself.
func TestHTTPImageDeployAcquireURL(t *testing.T) {
	const imageRef = "gpustack/l2-deploy-runner:cuda13.0-vllm0.25.1"
	c := startArchiveImportCluster(t, imageRef)
	version := c.importArchiveImage(t, "l2-deploy-runner")
	loaded := c.fetchOnNode(t, version.ID, false)
	if row := c.nodeRow(t, loaded); row.State != "present" || row.Source != "url" {
		t.Fatalf("image node = %+v, want a present row acquired from the URL", row)
	}

	modelDir := filepath.Join(t.TempDir(), "model")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"), []byte(`{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"max_position_embeddings":2048}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "model.safetensors"), []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	localBody, _ := json.Marshal(map[string]any{"node_uuid": c.node.UUID, "path": modelDir, "name": "archive-url-deploy"})
	localResp, err := authedPost(t, c.base, c.adminToken, "/api/v1/models/local", localBody)
	if err != nil {
		t.Fatal(err)
	}
	if localResp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(localResp.Body)
		localResp.Body.Close()
		t.Fatalf("register local model status=%d body=%s", localResp.StatusCode, raw)
	}
	var model struct {
		ID uint `json:"id"`
	}
	if err := json.NewDecoder(localResp.Body).Decode(&model); err != nil {
		t.Fatal(err)
	}
	localResp.Body.Close()

	// The vRAM limit names no backend, so the create settles it from the
	// node's reported dGPU capabilities. Online only means the agent
	// connected; the capabilities arrive with its status report.
	if !waitNodeUntil(t, c.base, c.adminToken, serverHealthTimeout, func(n processNode) bool {
		if n.UUID != c.node.UUID || n.State != "online" || len(n.GPUs) != 1 {
			return false
		}
		for _, capability := range n.GPUs[0].PartitionCapabilities {
			if capability.Backend == "nvidia-dgpu" && capability.Status == "ready" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("node %s never reported a ready nvidia-dgpu capability within %s", c.node.UUID, serverHealthTimeout)
	}

	deployBody, _ := json.Marshal(map[string]any{
		"name": "archive-url-deploy", "served_name": "archive-url-deploy",
		"model_id": model.ID, "node_uuid": c.node.UUID,
		"engine": "vllm", "image": imageRef, "vram_limit_gib": 8,
		"quick_params": map[string]any{"max_model_len": 128},
	})
	deployResp, err := authedPost(t, c.base, c.adminToken, "/api/v1/deployments", deployBody)
	if err != nil {
		t.Fatal(err)
	}
	if deployResp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(deployResp.Body)
		deployResp.Body.Close()
		t.Fatalf("create deployment status=%d body=%s", deployResp.StatusCode, raw)
	}
	deployResp.Body.Close()

	// The deployment found the loaded image: the ledger row is still the
	// URL load, not replaced by a pull.
	deadline := time.Now().Add(60 * time.Second)
	var row struct {
		NodeUUID         string `json:"node_uuid"`
		State            string `json:"state"`
		Source           string `json:"source"`
		Message          string `json:"message"`
		SizeBytes        int64  `json:"size_bytes"`
		TransferredBytes int64  `json:"transferred_bytes"`
		TransferID       string `json:"transfer_id"`
	}
	for time.Now().Before(deadline) {
		row = c.nodeRow(t, findArchiveImportVersion(t, c.base, c.adminToken, imageRef))
		if row.State == "present" || row.State == "failed" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if row.State != "present" || row.Source != "url" {
		t.Fatalf("image node = %+v, want a present row acquired from the URL", row)
	}
	// The node fetched it itself, which is what image.load_url does: the
	// downloader asks for ranges, so a relayed whole-body GET is not what this
	// path should produce.
	downloaded := false
	for _, request := range c.site.Requests() {
		if strings.HasPrefix(request, "GET /image.tar") {
			downloaded = true
		}
	}
	if !downloaded {
		t.Fatalf("the deployment never fetched the archive: %v", c.site.Requests())
	}
	assertNoStagedArchives(t, c.dataDir)
}
