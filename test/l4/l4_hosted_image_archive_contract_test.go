//go:build cloudgpu && (compshare || aliyun)

package l4

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/download"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
	"gpuworks.ai/gpuworks/test/ocifixture"
)

const archiveTestImage = "gpuworks/gpuworks-runner:cuda12.9-vllm0.31.0-r2"

func requireTool(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			skipOrFailExternal(t, "%s is required to run the archive server contract: %v", name, err)
		}
	}
}

// startArchiveServerProcess runs hostedArchiveServerScript exactly as the VM
// does, on a loopback port it picks, serving dir/runner.tar.
func startArchiveServerProcess(t *testing.T, dir string) string {
	t.Helper()
	requireTool(t, "python3")
	script := filepath.Join(dir, "serve.py")
	if err := os.WriteFile(script, []byte(hostedArchiveServerScript), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", script, dir, "127.0.0.1", "0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "GPUWORKS_ARCHIVE_HTTP listening 127.0.0.1:") {
		t.Fatalf("archive server did not announce its address: %q err=%v stderr=%s", line, err, stderr.String())
	}
	return "http://" + strings.TrimSpace(strings.TrimPrefix(line, "GPUWORKS_ARCHIVE_HTTP listening "))
}

func TestHostedArchiveServerAnswersHeadAndRanges(t *testing.T) {
	dir := t.TempDir()
	body := bytes.Repeat([]byte("0123456789abcdef"), 1<<14) // 256 KiB
	if err := os.WriteFile(filepath.Join(dir, hostedArchiveFile), body, 0o644); err != nil {
		t.Fatal(err)
	}
	base := startArchiveServerProcess(t, dir)
	size := len(body)
	do := func(method, path, rangeHeader string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp, data
	}

	resp, data := do(http.MethodHead, "/runner.tar", "")
	if resp.StatusCode != 200 || resp.ContentLength != int64(size) || resp.Header.Get("Accept-Ranges") != "bytes" || len(data) != 0 {
		t.Fatalf("HEAD: status=%d length=%d accept-ranges=%q body=%d", resp.StatusCode, resp.ContentLength, resp.Header.Get("Accept-Ranges"), len(data))
	}
	resp, data = do(http.MethodGet, "/runner.tar", "")
	if resp.StatusCode != 200 || !bytes.Equal(data, body) {
		t.Fatalf("GET: status=%d bytes=%d", resp.StatusCode, len(data))
	}
	for _, tc := range []struct {
		name, header, contentRange string
		want                       []byte
		status                     int
	}{
		{"first four bytes", "bytes=0-3", fmt.Sprintf("bytes 0-3/%d", size), body[:4], 206},
		{"open ended", fmt.Sprintf("bytes=%d-", size-5), fmt.Sprintf("bytes %d-%d/%d", size-5, size-1, size), body[size-5:], 206},
		{"suffix", "bytes=-6", fmt.Sprintf("bytes %d-%d/%d", size-6, size-1, size), body[size-6:], 206},
		{"end past the file is clamped", fmt.Sprintf("bytes=%d-%d", size-2, size+100), fmt.Sprintf("bytes %d-%d/%d", size-2, size-1, size), body[size-2:], 206},
		{"start past the file", fmt.Sprintf("bytes=%d-", size), fmt.Sprintf("bytes */%d", size), nil, 416},
		{"inverted", "bytes=9-3", fmt.Sprintf("bytes */%d", size), nil, 416},
	} {
		resp, data := do(http.MethodGet, "/runner.tar", tc.header)
		if resp.StatusCode != tc.status || resp.Header.Get("Content-Range") != tc.contentRange || !bytes.Equal(data, tc.want) {
			t.Fatalf("%s: status=%d content-range=%q body=%q, want %d %q %q", tc.name, resp.StatusCode, resp.Header.Get("Content-Range"), data, tc.status, tc.contentRange, tc.want)
		}
	}
	// A ranged HEAD describes the same slice without a body.
	resp, data = do(http.MethodHead, "/runner.tar", "bytes=0-9")
	if resp.StatusCode != 206 || resp.ContentLength != 10 || len(data) != 0 {
		t.Fatalf("ranged HEAD: status=%d length=%d body=%d", resp.StatusCode, resp.ContentLength, len(data))
	}
	// Exactly one file is served, and nothing outside the directory.
	for _, path := range []string{"/other.tar", "/", "/../etc/passwd", "/%2e%2e/etc/passwd", "/serve.py"} {
		if resp, _ := do(http.MethodGet, path, ""); resp.StatusCode != 404 {
			t.Fatalf("GET %s answered %d, want 404", path, resp.StatusCode)
		}
	}
}

// TestHostedArchiveServerIsReadByThePlatformsOwnClients runs the platform's
// preview and its node download against the file server with a real
// docker-save layout: this is the pair of readers the case depends on.
func TestHostedArchiveServerIsReadByThePlatformsOwnClients(t *testing.T) {
	archive, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{RepoTags: []string{archiveTestImage}, LayerBytes: 6 << 20})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, hostedArchiveFile), archive, 0o644); err != nil {
		t.Fatal(err)
	}
	base := startArchiveServerProcess(t, dir)

	peek, err := imagecatalog.PeekArchiveURL(t.Context(), imagecatalog.ArchiveHTTPClient(), base+"/"+hostedArchiveFile)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if !peek.Previewable || len(peek.RepoTags) != 1 || peek.RepoTags[0] != archiveTestImage || peek.SizeBytes != int64(len(archive)) {
		t.Fatalf("preview = %+v, want previewable %s of %d bytes", peek, archiveTestImage, len(archive))
	}

	dest := filepath.Join(t.TempDir(), "runner.tar")
	if err := download.Download(t.Context(), download.Options{URL: base + "/" + hostedArchiveFile, Dest: dest, Concurrency: 4}, nil); err != nil {
		t.Fatalf("node download: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != sha256.Sum256(archive) {
		t.Fatalf("downloaded %d bytes that differ from the %d-byte archive", len(got), len(archive))
	}
}

// fakeDocker stands in for the docker CLI: `docker save <image> -o <path>`
// writes the archive for the one image the VM holds, fails like the daemon for
// any other, and leaves a truncated file behind for "partial".
func fakeDocker(t *testing.T, image string) []string {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
case "$1" in
  info) exit 0 ;;
  save)
    [ "$3" = "-o" ] || { echo "docker save: missing -o" >&2; exit 2; }
    case "$2" in
      partial) head -c 10 /dev/zero > "$4"; echo "write failed: no space left on device" >&2; exit 1 ;;
      ` + image + `) head -c 8192 /dev/zero > "$4" ;;
      *) echo "Error response from daemon: No such image: $2" >&2; exit 1 ;;
    esac ;;
  *) exit 3 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
}

func runArchiveSave(t *testing.T, env []string, dir, image string) hostedArchiveSave {
	t.Helper()
	requireTool(t, "sh")
	if err := os.WriteFile(filepath.Join(dir, "save.sh"), []byte(hostedArchiveSaveScript(dir, image)), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", filepath.Join(dir, "save.sh"))
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("save.sh: %v\n%s", err, out)
	}
	poll := exec.Command("sh", "-c", hostedArchiveSavePollCommand(dir))
	out, err := poll.CombinedOutput()
	if err != nil {
		t.Fatalf("poll: %v\n%s", err, out)
	}
	save, parseErr := parseHostedArchiveSave(string(out))
	if parseErr != nil {
		t.Logf("save failed as the poll reports it: %v", parseErr)
		save.Detail = parseErr.Error()
	}
	return save
}

func TestHostedArchiveSaveLeavesAWholeFileOrNone(t *testing.T) {
	env := fakeDocker(t, archiveTestImage)

	dir := t.TempDir()
	save := runArchiveSave(t, env, dir, archiveTestImage)
	if !save.Done || save.SizeBytes != 8192 {
		t.Fatalf("save of the image the VM holds = %+v, want done with 8192 bytes", save)
	}
	if info, err := os.Stat(filepath.Join(dir, hostedArchiveFile)); err != nil || info.Size() != 8192 || info.Mode().Perm() != 0o644 {
		t.Fatalf("archive = %v err=%v, want a 0644 file of 8192 bytes", info, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "runner.tar.part")); !os.IsNotExist(err) {
		t.Fatalf("the .part file is still there: %v", err)
	}

	for _, image := range []string{"gpuworks/gpuworks-runner:not-on-this-vm", "partial"} {
		dir := t.TempDir()
		save := runArchiveSave(t, env, dir, image)
		if save.Done || !strings.Contains(save.Detail, "docker save "+image+" failed") {
			t.Fatalf("save of %q = %+v, want the failure reported", image, save)
		}
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "runner.tar") {
				t.Fatalf("a failed save of %q left %s behind, so a half archive could be served", image, entry.Name())
			}
		}
	}
	dir = t.TempDir()
	runArchiveSave(t, env, dir, "partial")
	if data, _ := os.ReadFile(filepath.Join(dir, "save.fail")); !strings.Contains(string(data), "no space left on device") {
		t.Fatalf("save.fail = %q, want the docker error", data)
	}
}

func TestParseHostedArchiveSave(t *testing.T) {
	good, err := parseHostedArchiveSave("done 1000 1347 53687091200\n")
	if err != nil || !good.Done || good.Seconds != 347 || good.SizeBytes != 53687091200 {
		t.Fatalf("done = %+v err=%v", good, err)
	}
	running, err := parseHostedArchiveSave("running 1073741824 10737418240\n")
	if err != nil || running.Done || !strings.Contains(running.Detail, "1.0 GiB written, 10.0 GiB free") {
		t.Fatalf("running = %+v err=%v", running, err)
	}
	if _, err := parseHostedArchiveSave("fail\ndocker save x failed\nno space"); err == nil || !strings.Contains(err.Error(), "no space") {
		t.Fatalf("fail err = %v", err)
	}
	for _, bad := range []string{"", "done 1 2", "done 5 3 100", "done 1 2 0", "running x y", "weird 1 2 3"} {
		if got, err := parseHostedArchiveSave(bad); err == nil {
			t.Fatalf("parseHostedArchiveSave(%q) = %+v, want an error", bad, got)
		}
	}
}

// TestHostedArchiveServeAndCleanupCommands runs the commands the VM runs: serve
// brings the file server up and proves Range works; cleanup stops it and
// removes the tar, twice without error.
func TestHostedArchiveServeAndCleanupCommands(t *testing.T) {
	requireTool(t, "python3", "curl", "sh", "seq")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "serve.py"), []byte(hostedArchiveServerScript), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, hostedArchiveFile), bytes.Repeat([]byte("x"), 100000), 0o644); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	t.Cleanup(func() { _ = exec.Command("sh", "-c", hostedArchiveCleanupCommand(dir)).Run() })

	if out, err := exec.Command("sh", "-c", hostedArchiveServeCommand(dir, "127.0.0.1", port)).CombinedOutput(); err != nil {
		t.Fatalf("serve command: %v\n%s", err, out)
	}
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/" + hostedArchiveFile
	out, err := exec.Command("curl", "-fsS", "-r", "10-19", url).Output()
	if err != nil || string(out) != "xxxxxxxxxx" {
		t.Fatalf("ranged read through the started server = %q err=%v", out, err)
	}
	pid, err := os.ReadFile(filepath.Join(dir, "http.pid"))
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if out, err := exec.Command("sh", "-c", hostedArchiveCleanupCommand(dir)).CombinedOutput(); err != nil {
			t.Fatalf("cleanup %d: %v\n%s", attempt, err, out)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("cleanup left %s: %v", dir, err)
	}
	waitFor(t, "the archive server to stop", func() bool {
		return exec.Command("kill", "-0", strings.TrimSpace(string(pid))).Run() != nil
	})
	if _, err := exec.Command("curl", "-fsS", "--max-time", "2", url).Output(); err == nil {
		t.Fatal("the archive is still served after cleanup")
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestHostedServerLaunchAllowsPrivateArchiveHostsOnlyWhenAsked(t *testing.T) {
	plain, err := hostedServerLaunchScript(hostedServerLaunch{Root: "/opt/run", PrivateIP: "10.42.0.8", Port: hostedControlPlanePort, AdminPassword: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, "--config") || strings.Contains(plain, "server.yaml") {
		t.Fatalf("a launch that did not ask for archive hosts still configures the server:\n%s", plain)
	}
	with, err := hostedServerLaunchScript(hostedServerLaunch{Root: "/opt/run", PrivateIP: "10.42.0.8", Port: hostedControlPlanePort, AdminPassword: "secret",
		OutboundAllowPrivateHosts: []string{"10.42.0.8", "10.42.0.9"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`--config "$root/server.yaml"`, `>"$root/server.yaml"`, `"10.42.0.9"`, "--addr '10.42.0.8:18880'", "--recovery-enabled=false"} {
		if !strings.Contains(with, required) {
			t.Fatalf("launcher missing %q:\n%s", required, with)
		}
	}
	if strings.Index(with, "server.yaml") > strings.Index(with, "nohup") {
		t.Fatalf("the config file is written after the server starts:\n%s", with)
	}
	for _, public := range []string{"8.8.8.8", "example.com", "169.254.169.254", "0.0.0.0", ""} {
		if _, err := hostedServerLaunchScript(hostedServerLaunch{Root: "/opt/run", PrivateIP: "10.42.0.8", Port: hostedControlPlanePort, AdminPassword: "secret",
			OutboundAllowPrivateHosts: []string{public}}); err == nil {
			t.Fatalf("outbound_allow_private_hosts accepted %q", public)
		}
	}
}

func TestHostedServerConfigYAMLIsWhatTheServerLoads(t *testing.T) {
	yaml, err := hostedServerConfigYAML([]string{"10.42.0.8", "192.168.1.5"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := serverconfig.Load(path)
	if err != nil {
		t.Fatalf("serverconfig.Load: %v\n%s", err, yaml)
	}
	if got := strings.Join(file.OutboundAllowPrivateHosts, ","); got != "10.42.0.8,192.168.1.5" {
		t.Fatalf("outbound_allow_private_hosts = %q", got)
	}
}

func TestHostedPrivateAddressesRejectsAVMWithoutOne(t *testing.T) {
	vm := func(ip string) *cloudTestEnv { return &cloudTestEnv{Instance: &cloudprovider.Instance{PrivateIP: ip}} }
	got, err := hostedPrivateAddresses([]*cloudTestEnv{vm("10.0.0.4"), vm("10.0.0.5")})
	if err != nil || strings.Join(got, ",") != "10.0.0.4,10.0.0.5" {
		t.Fatalf("addresses = %v err=%v", got, err)
	}
	if _, err := hostedPrivateAddresses([]*cloudTestEnv{vm("10.0.0.4"), vm("47.1.2.3")}); err == nil {
		t.Fatal("a public address was accepted as a private one")
	}
}

func TestHostedImageNodeProgressSplitsDownloadAndLoad(t *testing.T) {
	var progress hostedImageNodeProgress
	at := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	step := func(offset time.Duration, row dto.ImageNode) (bool, error) {
		return progress.Observe(at.Add(offset), row)
	}
	if done, err := step(0, dto.ImageNode{State: "absent"}); done || err != nil || !progress.Started.IsZero() {
		t.Fatalf("an absent row before the transfer started must be ignored: done=%v err=%v progress=%+v", done, err, progress)
	}
	step(time.Second, dto.ImageNode{State: "syncing", Message: "downloading image archive from 10.0.0.4:18881"})
	step(30*time.Second, dto.ImageNode{State: "syncing", Message: "downloading 10.0.0.4:18881"})
	step(95*time.Second, dto.ImageNode{State: "syncing", Message: "loading image archive"})
	step(100*time.Second, dto.ImageNode{State: "syncing", Message: "loading image archive"})
	done, err := step(400*time.Second, dto.ImageNode{State: "present", Message: "image archive downloaded and loaded"})
	if !done || err != nil {
		t.Fatalf("present: done=%v err=%v", done, err)
	}
	if progress.Download() != 94*time.Second || progress.Load() != 305*time.Second || progress.Total() != 399*time.Second {
		t.Fatalf("download=%s load=%s total=%s, want 94s/305s/399s", progress.Download(), progress.Load(), progress.Total())
	}

	var failed hostedImageNodeProgress
	failed.Observe(at, dto.ImageNode{State: "syncing", Message: "downloading"})
	if _, err := failed.Observe(at.Add(time.Second), dto.ImageNode{State: "failed", Message: "insufficient disk space at /opt: available=1 required=2"}); err == nil || !strings.Contains(err.Error(), "insufficient disk space") {
		t.Fatalf("failed row error = %v", err)
	}
	var vanished hostedImageNodeProgress
	vanished.Observe(at, dto.ImageNode{State: "syncing"})
	if _, err := vanished.Observe(at.Add(time.Second), dto.ImageNode{State: "absent", Message: "cancelled"}); err == nil {
		t.Fatal("a row that went back to absent after the transfer began was accepted")
	}
	var instant hostedImageNodeProgress
	if done, _ := instant.Observe(at, dto.ImageNode{State: "present"}); !done || instant.Load() != 0 || instant.Download() != 0 {
		t.Fatalf("an import seen only once finished: %+v", instant)
	}
}

func TestHostedCustomImageBodyIsWhatTheConsoleSends(t *testing.T) {
	body, err := hostedCustomImageBody(archiveTestImage, []string{"http://10.0.0.4:18881/runner.tar", "http://10.0.0.5:18881/runner.tar"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(body)
	const want = `{"series":[{"engine":"vllm","framework":"cuda","name":"gpuworks/gpuworks-runner","source":"custom","versions":[` +
		`{"arch":"multi","engine_version":"0.31.0","framework_version":"12.9","image_ref":"gpuworks/gpuworks-runner:cuda12.9-vllm0.31.0-r2",` +
		`"registry_sources":["http://10.0.0.4:18881/runner.tar","http://10.0.0.5:18881/runner.tar"],"rolling_tag":false,"source":"custom"}]}]}`
	if string(got) != want {
		t.Fatalf("body:\n got %s\nwant %s", got, want)
	}
	for _, ref := range []string{"gpuworks/gpuworks-runner", "gpuworks/gpuworks-runner:rocm7.2.3-vllm0.30.0-r1", "gpuworks/gpuworks-runner:cuda12.9", ""} {
		if _, err := hostedCustomImageBody(ref, nil); err == nil {
			t.Fatalf("hostedCustomImageBody(%q) was accepted", ref)
		}
	}
}

func TestHostedMergeSourcesKeepsRegistryReferencesAndDoesNotDuplicate(t *testing.T) {
	got := hostedMergeSources([]string{"quay.io/gpuworks/gpuworks-runner:x", "http://a/x.tar"}, []string{"http://a/x.tar", "http://b/x.tar"})
	if strings.Join(got, " ") != "quay.io/gpuworks/gpuworks-runner:x http://a/x.tar http://b/x.tar" {
		t.Fatalf("merged = %v", got)
	}
}

func TestHostedRequireArchiveCarries(t *testing.T) {
	ok := dto.ImageArchivePreview{URL: "http://a/runner.tar", SizeBytes: 10, RepoTags: []string{"docker.io/gpuworks/gpuworks-runner:cuda12.9-vllm0.31.0-r2"}, Previewable: true}
	if warning, err := hostedRequireArchiveCarries(ok, archiveTestImage); warning != "" || err != nil {
		t.Fatalf("the same reference spelled with docker.io: warning=%q err=%v", warning, err)
	}
	other := ok
	other.RepoTags = []string{"gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4"}
	if _, err := hostedRequireArchiveCarries(other, archiveTestImage); err == nil {
		t.Fatal("an archive of another image was accepted")
	}
	blind := dto.ImageArchivePreview{URL: "http://a/runner.tar", SizeBytes: 10, RepoTags: []string{}, Previewable: false}
	if warning, err := hostedRequireArchiveCarries(blind, archiveTestImage); warning == "" || err != nil {
		t.Fatalf("an unpreviewable archive must warn, not fail: warning=%q err=%v", warning, err)
	}
	if _, err := hostedRequireArchiveCarries(dto.ImageArchivePreview{URL: "http://a/runner.tar"}, archiveTestImage); err == nil {
		t.Fatal("an archive of unknown size was accepted")
	}
}

// fakeImageAPI answers the image routes the case calls with the wire shapes
// the real handlers write (internal/server/api/images*.go, images_archive_url.go):
// list envelopes, {"error","code","params"} failures, 202 on sync. Behaviours
// that matter are kept: the PATCH takes only enabled/registry_sources and
// normalizes URLs; import insists on a tag, an engine and a framework version
// when a framework is declared; sync takes only a declared archive URL.
type fakeImageAPI struct {
	mu          sync.Mutex
	versions    []*fakeImageVersion
	nodes       []string
	previewFail map[string]int
	polls       map[string]int
	syncBodies  []string
	imports     []string
}

type fakeImageVersion struct {
	id, seriesID uint
	ref          string
	source       string
	sources      []string
	rows         map[string][]string // node uuid -> states served on successive polls
}

func (f *fakeImageAPI) writeError(w http.ResponseWriter, status int, code, message string, params string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if params == "" {
		params = "{}"
	}
	fmt.Fprintf(w, `{"error":%q,"code":%q,"params":%s}`, message, code, params)
}

func (f *fakeImageAPI) versionJSON(v *fakeImageVersion, withNodes bool) string {
	sources, _ := json.Marshal(v.sources)
	nodes := ""
	if withNodes {
		var rows []string
		for _, uuid := range f.nodes {
			state, message := "absent", ""
			if seq := v.rows[uuid]; len(seq) > 0 {
				index := f.polls[uuid]
				if index >= len(seq) {
					index = len(seq) - 1
				}
				state, message, _ = strings.Cut(seq[index], "|")
			}
			rows = append(rows, fmt.Sprintf(`{"node_uuid":%q,"node_name":%q,"node_state":"online","state":%q,"size_bytes":100,"transferred_bytes":40,"rate_bps":1048576,"source":"url","message":%q,"updated_at":""}`, uuid, "node-"+uuid, state, message))
		}
		nodes = `,"nodes":[` + strings.Join(rows, ",") + `]`
	}
	return fmt.Sprintf(`{"id":%d,"series_id":%d,"purpose":"inference","name":"","engine":"vllm","framework":"cuda","image_ref":%q,"source":%q,"deprecated":false,"enabled":true,"registry_sources":%s,"rolling_tag":false,"upstream_gone":false,"effective_enabled":true,"reference_count":0,"present_nodes":0,"total_nodes":%d%s}`,
		v.id, v.seriesID, v.ref, v.source, sources, len(f.nodes), nodes)
}

func (f *fakeImageAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"token":"t"}`))
	})
	mux.HandleFunc("GET /api/v1/images/versions", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		items := make([]string, 0, len(f.versions))
		for _, v := range f.versions {
			items = append(items, f.versionJSON(v, false))
		}
		fmt.Fprintf(w, `{"items":[%s],"total":%d}`, strings.Join(items, ","), len(items))
	})
	mux.HandleFunc("POST /api/v1/images/import", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		raw, _ := io.ReadAll(r.Body)
		f.imports = append(f.imports, string(raw))
		var body struct {
			Series []struct {
				Engine, Framework, Source string
				Versions                  []struct {
					FrameworkVersion string   `json:"framework_version"`
					ImageRef         string   `json:"image_ref"`
					RegistrySources  []string `json:"registry_sources"`
				}
			}
		}
		if json.Unmarshal(raw, &body) != nil || len(body.Series) == 0 || len(body.Series[0].Versions) == 0 {
			f.writeError(w, 400, "IMAGE_SERIES_VERSION_REQUIRED", "series with at least one version is required", "")
			return
		}
		series := body.Series[0]
		for _, version := range series.Versions {
			name := version.ImageRef[strings.LastIndex(version.ImageRef, "/")+1:]
			switch {
			case !strings.Contains(name, ":"):
				f.writeError(w, 400, "IMAGE_TAG_REQUIRED", "image_ref must include an explicit tag", "")
				return
			case series.Engine == "":
				f.writeError(w, 400, "IMAGE_ENGINE_REQUIRED", "engine is required", "")
				return
			case series.Framework != "" && version.FrameworkVersion == "":
				f.writeError(w, 400, "IMAGE_FRAMEWORK_VERSION_REQUIRED", "framework_version is required when a framework is declared", "")
				return
			}
			f.versions = append(f.versions, &fakeImageVersion{id: uint(len(f.versions) + 1), seriesID: 7, ref: version.ImageRef, source: series.Source, sources: version.RegistrySources, rows: map[string][]string{}})
		}
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	})
	mux.HandleFunc("PATCH /api/v1/images/versions/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body struct {
			Enabled         *bool    `json:"enabled"`
			RegistrySources []string `json:"registry_sources"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&body) != nil {
			f.writeError(w, 400, "IMAGE_VERSION_PATCH_INVALID", "invalid image version patch", "")
			return
		}
		id, _ := strconv.Atoi(r.PathValue("id"))
		for _, v := range f.versions {
			if int(v.id) == id {
				v.sources = body.RegistrySources
				_, _ = w.Write([]byte(f.versionJSON(v, true)))
				return
			}
		}
		f.writeError(w, 404, "NOT_FOUND", "not found", "")
	})
	mux.HandleFunc("POST /api/v1/images/preview-archive-url", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body dto.ImageArchivePreviewRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.URL == "" {
			f.writeError(w, 400, "IMAGE_ARCHIVE_URL_REQUIRED", "an image archive URL is required", "")
			return
		}
		if !strings.HasPrefix(body.URL, "http://") {
			f.writeError(w, 400, "IMAGE_ARCHIVE_URL_INVALID", "invalid image archive URL: scheme is not http or https", `{"detail":"scheme is not http or https"}`)
			return
		}
		if status := f.previewFail[body.URL]; status != 0 {
			f.writeError(w, status, "IMAGE_ARCHIVE_URL_UNREACHABLE", "the image archive could not be read", `{"host":"10.0.0.5:18881","detail":"the image archive could not be read"}`)
			return
		}
		fmt.Fprintf(w, `{"url":%q,"size_bytes":53687091200,"repo_tags":["gpuworks/gpuworks-runner:cuda12.9-vllm0.31.0-r2"],"previewable":true}`, body.URL)
	})
	mux.HandleFunc("POST /api/v1/images/{id}/nodes/{uuid}/sync", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		raw, _ := io.ReadAll(r.Body)
		f.syncBodies = append(f.syncBodies, string(raw))
		var body struct{ Source, ArchiveURL string }
		var wire map[string]string
		_ = json.Unmarshal(raw, &wire)
		body.Source, body.ArchiveURL = wire["source"], wire["archive_url"]
		if body.Source != "url" {
			f.writeError(w, 400, "IMAGE_SOURCE_REMOVED", "a registry can no longer be named for a pull; nodes pull through the pull source rules", "")
			return
		}
		id, _ := strconv.Atoi(r.PathValue("id"))
		for _, v := range f.versions {
			if int(v.id) != id {
				continue
			}
			for _, source := range v.sources {
				if source == body.ArchiveURL {
					v.rows[r.PathValue("uuid")] = []string{"syncing|downloading image archive from host", "syncing|downloading host", "syncing|loading image archive", "present|image archive downloaded and loaded"}
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(`{"state":"syncing","transfer_id":"11111111-2222-3333-4444-555555555555","message":"downloading image archive from host"}`))
					return
				}
			}
			f.writeError(w, 400, "IMAGE_ARCHIVE_URL_NOT_DECLARED", "this image archive URL is not declared on the image; add it to the image's registry sources first", fmt.Sprintf(`{"url":%q,"image":%q}`, body.ArchiveURL, v.ref))
			return
		}
		f.writeError(w, 404, "NOT_FOUND", "not found", "")
	})
	mux.HandleFunc("GET /api/v1/images/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var versions []string
		for _, v := range f.versions {
			versions = append(versions, f.versionJSON(v, true))
		}
		for _, uuid := range f.nodes {
			f.polls[uuid]++
		}
		fmt.Fprintf(w, `{"id":7,"name":"gpuworks/gpuworks-runner","engine":"vllm","purpose":"inference","framework":"cuda","source":"custom","enabled":true,"version_count":%d,"present_nodes":0,"total_nodes":%d,"versions":[%s]}`, len(versions), len(f.nodes), strings.Join(versions, ","))
	})
	return mux
}

func newFakeImageAPI(t *testing.T, nodes ...string) (*fakeImageAPI, *hostedAPIClient) {
	t.Helper()
	fake := &fakeImageAPI{nodes: nodes, previewFail: map[string]int{}, polls: map[string]int{}}
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)
	api := newHostedAPIClient(func(context.Context) (hostedForward, error) {
		return &hostedRetryTestForward{baseURL: server.URL}, nil
	}, "admin", "password")
	t.Cleanup(api.Close)
	return fake, api
}

func TestHostedDeclareArchiveImageCreatesACustomImageWithBothAddresses(t *testing.T) {
	fake, api := newFakeImageAPI(t, "n0", "n1")
	urls := []string{"http://10.0.0.4:18881/runner.tar", "http://10.0.0.5:18881/runner.tar"}
	image, err := hostedDeclareArchiveImage(t.Context(), api, archiveTestImage, urls)
	if err != nil {
		t.Fatal(err)
	}
	if image.ID != 1 || image.SeriesID != 7 || image.Source != "custom" || strings.Join(image.RegistrySources, " ") != strings.Join(urls, " ") {
		t.Fatalf("image = %+v", image)
	}
	if len(fake.imports) != 1 {
		t.Fatalf("imports = %v, want one", fake.imports)
	}
	// A second declaration finds the image and changes nothing.
	again, err := hostedDeclareArchiveImage(t.Context(), api, "docker.io/"+archiveTestImage, urls)
	if err != nil || again.ID != image.ID || len(fake.imports) != 1 {
		t.Fatalf("second declaration: %+v err=%v imports=%d", again, err, len(fake.imports))
	}
}

func TestHostedDeclareArchiveImageAppendsToAnExistingImageAndKeepsItsRegistrySources(t *testing.T) {
	fake, api := newFakeImageAPI(t, "n0")
	fake.versions = []*fakeImageVersion{{id: 4, seriesID: 7, ref: archiveTestImage, source: "official",
		sources: []string{"quay.io/gpuworks/gpuworks-runner:cuda12.9-vllm0.31.0-r2"}, rows: map[string][]string{}}}
	urls := []string{"http://10.0.0.4:18881/runner.tar"}
	image, err := hostedDeclareArchiveImage(t.Context(), api, archiveTestImage, urls)
	if err != nil {
		t.Fatal(err)
	}
	want := "quay.io/gpuworks/gpuworks-runner:cuda12.9-vllm0.31.0-r2 http://10.0.0.4:18881/runner.tar"
	if image.ID != 4 || strings.Join(image.RegistrySources, " ") != want || len(fake.imports) != 0 {
		t.Fatalf("image = %+v imports=%v, want sources %q and no import", image, fake.imports, want)
	}
}

func TestHostedFetchArchiveSendsTheURLSourceAndSurfacesTheErrorCodes(t *testing.T) {
	fake, api := newFakeImageAPI(t, "n0")
	if _, err := hostedDeclareArchiveImage(t.Context(), api, archiveTestImage, []string{"http://10.0.0.4:18881/runner.tar"}); err != nil {
		t.Fatal(err)
	}
	response, err := hostedFetchArchive(t.Context(), api, 1, "n0", "http://10.0.0.4:18881/runner.tar")
	if err != nil || response.State != "syncing" || response.TransferID == "" {
		t.Fatalf("fetch = %+v err=%v", response, err)
	}
	if got := fake.syncBodies[0]; got != `{"source":"url","archive_url":"http://10.0.0.4:18881/runner.tar"}` {
		t.Fatalf("sync body = %s", got)
	}
	_, err = hostedFetchArchive(t.Context(), api, 1, "n0", "http://10.0.0.9:18881/runner.tar")
	if hostedAPIErrorCode(err) != "IMAGE_ARCHIVE_URL_NOT_DECLARED" {
		t.Fatalf("an undeclared URL: code=%q err=%v", hostedAPIErrorCode(err), err)
	}
}

func TestHostedPreviewArchiveNamesTheRemedyForAnUnreachableArchive(t *testing.T) {
	fake, api := newFakeImageAPI(t, "n0")
	preview, err := hostedPreviewArchive(t.Context(), api, "http://10.0.0.4:18881/runner.tar")
	if err != nil || !preview.Previewable || preview.SizeBytes != 53687091200 || strings.Join(preview.RepoTags, ",") != archiveTestImage {
		t.Fatalf("preview = %+v err=%v", preview, err)
	}
	fake.previewFail["http://10.0.0.5:18881/runner.tar"] = http.StatusBadGateway
	_, err = hostedPreviewArchive(t.Context(), api, "http://10.0.0.5:18881/runner.tar")
	if hostedAPIErrorCode(err) != "IMAGE_ARCHIVE_URL_UNREACHABLE" || !strings.Contains(err.Error(), "outbound_allow_private_hosts") {
		t.Fatalf("unreachable: code=%q err=%v", hostedAPIErrorCode(err), err)
	}
	_, err = hostedPreviewArchive(t.Context(), api, "ftp://10.0.0.5/runner.tar")
	if hostedAPIErrorCode(err) != "IMAGE_ARCHIVE_URL_INVALID" || !strings.Contains(err.Error(), "rejected the archive URL") {
		t.Fatalf("invalid: code=%q err=%v", hostedAPIErrorCode(err), err)
	}
}

func TestHostedWaitImageNodePresentFollowsTheRowToPresent(t *testing.T) {
	defer func(old time.Duration) { hostedArchivePollInterval = old }(hostedArchivePollInterval)
	hostedArchivePollInterval = time.Millisecond
	fake, api := newFakeImageAPI(t, "n0")
	if _, err := hostedDeclareArchiveImage(t.Context(), api, archiveTestImage, []string{"http://10.0.0.4:18881/runner.tar"}); err != nil {
		t.Fatal(err)
	}
	if _, err := hostedFetchArchive(t.Context(), api, 1, "n0", "http://10.0.0.4:18881/runner.tar"); err != nil {
		t.Fatal(err)
	}
	var logs []string
	var logMu sync.Mutex
	progress, row, err := hostedWaitImageNodePresent(t.Context(), func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}, api, 0, 7, 1, "n0", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "present" || row.Source != "url" || progress.Total() <= 0 || progress.LoadStarted.IsZero() {
		t.Fatalf("row=%+v progress=%+v", row, progress)
	}
	if fake.polls["n0"] < 3 {
		t.Fatalf("the row was read %d times, want it followed through download and load", fake.polls["n0"])
	}

	// A failed transfer ends the wait at once with the node's message.
	fake.versions[0].rows["n0"] = []string{"syncing|downloading", "failed|insufficient disk space at /opt/udocker: available=1 required=2"}
	fake.polls["n0"] = 0
	if _, _, err := hostedWaitImageNodePresent(t.Context(), func(string, ...any) {}, api, 0, 7, 1, "n0", 10*time.Second); err == nil || !strings.Contains(err.Error(), "insufficient disk space") {
		t.Fatalf("failed transfer error = %v", err)
	}
	// A deadline reports where the row was left.
	fake.versions[0].rows["n0"] = []string{"syncing|downloading host"}
	if _, _, err := hostedWaitImageNodePresent(t.Context(), func(string, ...any) {}, api, 0, 7, 1, "n0", 50*time.Millisecond); err == nil || !strings.Contains(err.Error(), "state=syncing") {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestHostedImageNodeRowReportsMissingVersionsAndNodes(t *testing.T) {
	_, api := newFakeImageAPI(t, "n0")
	if _, err := hostedDeclareArchiveImage(t.Context(), api, archiveTestImage, []string{"http://10.0.0.4:18881/runner.tar"}); err != nil {
		t.Fatal(err)
	}
	if row, err := hostedImageNodeRow(t.Context(), api, 7, 1, "n0"); err != nil || row.State != "absent" || row.NodeName != "node-n0" {
		t.Fatalf("a node without a row: %+v err=%v", row, err)
	}
	if _, err := hostedImageNodeRow(t.Context(), api, 7, 99, "n0"); err == nil {
		t.Fatal("an unknown version was found")
	}
	if _, err := hostedImageNodeRow(t.Context(), api, 7, 1, "nX"); err == nil {
		t.Fatal("an unknown node was found")
	}
}

func TestHostedAPIErrorCarriesTheProductCode(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", &hostedAPIError{Method: "POST", Path: "/x", Status: 400, Body: `{"error":"e","code":"IMAGE_ARCHIVE_URL_INVALID"}`})
	if hostedAPIErrorCode(err) != "IMAGE_ARCHIVE_URL_INVALID" {
		t.Fatalf("code = %q", hostedAPIErrorCode(err))
	}
	if !strings.Contains(err.Error(), "hosted API POST /x returned HTTP 400") {
		t.Fatalf("message changed: %v", err)
	}
	if hostedAPIErrorCode(&hostedAPIError{Body: "not json"}) != "" || hostedAPIErrorCode(io.EOF) != "" {
		t.Fatal("a non-product failure reported a code")
	}
}
