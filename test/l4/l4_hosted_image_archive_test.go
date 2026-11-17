//go:build cloudgpu && (compshare || aliyun)

package l4

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/pullref"
	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

// The udocker runtime keeps its own image store, so a VM whose Docker already
// holds the runner image still has to get it a second time. Pulling ~30 GiB
// from a registry takes 25-40 minutes per rank; the golden image's Docker
// store is a local copy. This file hands that copy to udocker the way an
// operator moves an image between sites ("从 URL 导入",
// docs/how-to/import-from-http.md): `docker save` it on the VM, serve the tar
// over HTTP on the VM's private address, declare that address on the catalog
// image's fetch addresses, and ask each node to download it directly and load
// it. Every platform step is a management API call, the same ones the console
// makes; only the file server and the `docker save` are the harness's.

const (
	// hostedArchivePort is where each VM serves its own archive (private
	// address only; the control plane is on hostedControlPlanePort).
	hostedArchivePort = 18881
	hostedArchiveFile = "runner.tar"
	// hostedArchiveSaveTimeout bounds `docker save` of the runner image, which
	// reads the whole ~50 GiB layer store.
	hostedArchiveSaveTimeout = 30 * time.Minute
	// hostedArchiveImportTimeout bounds one node's download plus load. The pull
	// it replaces takes 25-40 minutes.
	hostedArchiveImportTimeout = 60 * time.Minute
	hostedArchiveSeriesName    = "gpuworks/gpuworks-runner"
)

// hostedArchivePollInterval is how often a node's image row is read; the unit
// tests shorten it.
var hostedArchivePollInterval = 3 * time.Second

// hostedArchiveServerScript is the file server each VM runs. python3 -m
// http.server cannot do the job: it ignores Range, and the platform previews
// an archive with HEAD plus Range requests and downloads it with several
// parallel ranges. It serves exactly one file, read-only, and prints the
// address it bound so a caller using port 0 learns the port.
const hostedArchiveServerScript = `#!/usr/bin/env python3
import email.utils
import http.server
import os
import re
import socketserver
import sys

NAME = "runner.tar"
CHUNK = 4 << 20


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "gpuworks-archive"

    def log_message(self, fmt, *args):
        sys.stderr.write("%s %s\n" % (self.address_string(), fmt % args))
        sys.stderr.flush()

    def do_HEAD(self):
        self.serve(False)

    def do_GET(self):
        self.serve(True)

    def reply(self, status, body, send_body, extra=None):
        self.send_response(status)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        for key, value in (extra or {}).items():
            self.send_header(key, value)
        self.end_headers()
        if send_body:
            self.wfile.write(body)

    def serve(self, send_body):
        if self.path.split("?", 1)[0].lstrip("/") != NAME:
            return self.reply(404, b"not found\n", send_body)
        path = os.path.join(self.server.root, NAME)
        try:
            info = os.stat(path)
        except OSError:
            return self.reply(404, b"not found\n", send_body)
        size = info.st_size
        start, end, status = 0, size - 1, 200
        match = re.fullmatch(r"bytes=(\d*)-(\d*)", (self.headers.get("Range") or "").strip())
        if match and (match.group(1) or match.group(2)):
            first, last = match.groups()
            if first == "":
                count = int(last)
                start, end = max(size - count, 0), size - 1
                if count == 0:
                    start = size
            else:
                start = int(first)
                end = min(int(last), size - 1) if last else size - 1
            if size == 0 or start >= size or start > end:
                return self.reply(416, b"", False, {"Content-Range": "bytes */%d" % size})
            status = 206
        self.send_response(status)
        self.send_header("Content-Type", "application/x-tar")
        self.send_header("Accept-Ranges", "bytes")
        self.send_header("Content-Length", str(end - start + 1))
        self.send_header("Last-Modified", email.utils.formatdate(info.st_mtime, usegmt=True))
        self.send_header("ETag", '"%x-%x"' % (int(info.st_mtime), size))
        if status == 206:
            self.send_header("Content-Range", "bytes %d-%d/%d" % (start, end, size))
        self.end_headers()
        if not send_body:
            return
        try:
            with open(path, "rb") as stream:
                stream.seek(start)
                remaining = end - start + 1
                while remaining > 0:
                    chunk = stream.read(min(CHUNK, remaining))
                    if not chunk:
                        break
                    self.wfile.write(chunk)
                    remaining -= len(chunk)
        except (BrokenPipeError, ConnectionResetError):
            self.close_connection = True


class Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    request_queue_size = 64


root, host, port = sys.argv[1], sys.argv[2], int(sys.argv[3])
server = Server((host, port), Handler)
server.root = root
print("GPUWORKS_ARCHIVE_HTTP listening %s:%d" % server.server_address, flush=True)
server.serve_forever()
`

func hostedArchiveDir(root string) string { return strings.TrimRight(root, "/") + "/archive" }

// hostedArchiveURL is the address a VM serves its archive at.
func hostedArchiveURL(privateIP string) string {
	return "http://" + net.JoinHostPort(privateIP, strconv.Itoa(hostedArchivePort)) + "/" + hostedArchiveFile
}

// hostedArchiveSaveScript writes the image to <dir>/runner.tar, through a
// ".part" name so a file that exists is complete, and leaves save.done
// ("<end epoch> <bytes>") or save.fail. It runs detached: the save reads the
// whole layer store and outlasts a quiet SSH session.
func hostedArchiveSaveScript(dir, image string) string {
	return fmt.Sprintf(`#!/bin/sh
set -u
dir=%s
image=%s
docker_cmd=docker; docker info >/dev/null 2>&1 || docker_cmd='sudo -n docker'
rm -f "$dir/save.done" "$dir/save.fail" "$dir/runner.tar" "$dir/runner.tar.part"
date +%%s > "$dir/save.start"
if $docker_cmd save "$image" -o "$dir/runner.tar.part" 2>"$dir/save.err" && chmod 0644 "$dir/runner.tar.part" && mv "$dir/runner.tar.part" "$dir/runner.tar"; then
  printf '%%s %%s\n' "$(date +%%s)" "$(stat -c %%s "$dir/runner.tar")" > "$dir/save.done"
else
  rm -f "$dir/runner.tar.part"
  { echo "docker save $image failed"; cat "$dir/save.err"; } > "$dir/save.fail"
fi
`, shellQuoteForTest(dir), shellQuoteForTest(image))
}

func hostedArchiveSaveStartCommand(dir string) string {
	return fmt.Sprintf(`set -eu
dir=%s
mkdir -p "$dir"
nohup sh "$dir/save.sh" >/dev/null 2>&1 </dev/null &
`, shellQuoteForTest(dir))
}

// hostedArchiveSavePollCommand prints one status line: "done <start> <end>
// <bytes>", "fail" followed by the reason, or "running <part bytes>
// <free bytes>".
func hostedArchiveSavePollCommand(dir string) string {
	return fmt.Sprintf(`dir=%s
if [ -f "$dir/save.done" ]; then
  echo "done $(cat "$dir/save.start") $(cat "$dir/save.done")"
elif [ -f "$dir/save.fail" ]; then
  echo fail; cat "$dir/save.fail"
else
  echo "running $(stat -c %%s "$dir/runner.tar.part" 2>/dev/null || echo 0) $(df -B1 --output=avail "$dir" | tail -n 1 | tr -d ' ')"
fi
`, shellQuoteForTest(dir))
}

// hostedArchiveSave is the outcome of `docker save` on one VM.
type hostedArchiveSave struct {
	Done      bool
	Seconds   int64
	SizeBytes int64
	// Detail is the failure reason, or the progress of a save still running.
	Detail string
}

// parseHostedArchiveSave reads hostedArchiveSavePollCommand's output.
func parseHostedArchiveSave(out string) (hostedArchiveSave, error) {
	out = strings.TrimSpace(out)
	line, rest, _ := strings.Cut(out, "\n")
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return hostedArchiveSave{}, errors.New("docker save status is empty")
	}
	number := func(index int) (int64, error) {
		if index >= len(fields) {
			return 0, fmt.Errorf("docker save status %q is incomplete", line)
		}
		return strconv.ParseInt(fields[index], 10, 64)
	}
	switch fields[0] {
	case "done":
		start, err := number(1)
		if err != nil {
			return hostedArchiveSave{}, err
		}
		end, err := number(2)
		if err != nil {
			return hostedArchiveSave{}, err
		}
		size, err := number(3)
		if err != nil {
			return hostedArchiveSave{}, err
		}
		if end < start || size <= 0 {
			return hostedArchiveSave{}, fmt.Errorf("docker save status %q is not a finished save", line)
		}
		return hostedArchiveSave{Done: true, Seconds: end - start, SizeBytes: size}, nil
	case "fail":
		return hostedArchiveSave{Detail: strings.TrimSpace(rest)}, errors.New(strings.TrimSpace(rest))
	case "running":
		part, partErr := number(1)
		free, freeErr := number(2)
		if partErr != nil || freeErr != nil {
			return hostedArchiveSave{}, fmt.Errorf("docker save status %q is incomplete", line)
		}
		return hostedArchiveSave{Detail: fmt.Sprintf("%.1f GiB written, %.1f GiB free", float64(part)/(1<<30), float64(free)/(1<<30))}, nil
	}
	return hostedArchiveSave{}, fmt.Errorf("unrecognised docker save status %q", line)
}

// hostedArchiveServeCommand starts the file server bound to the VM's private
// address and proves from the VM that it answers HEAD and honours Range before
// anyone asks the platform to use it.
func hostedArchiveServeCommand(dir, privateIP string, port int) string {
	return fmt.Sprintf(`set -eu
dir=%s
ip=%s
port=%d
command -v python3 >/dev/null || { echo "python3 is required to serve the image archive" >&2; exit 1; }
if [ -s "$dir/http.pid" ]; then kill "$(cat "$dir/http.pid")" 2>/dev/null || true; fi
nohup python3 "$dir/serve.py" "$dir" "$ip" "$port" >"$dir/http.log" 2>&1 </dev/null &
pid=$!
printf '%%s\n' "$pid" > "$dir/http.pid"
url="http://$ip:$port/%s"
for attempt in $(seq 1 30); do
  if curl -fsSI --max-time 3 "$url" >/dev/null 2>&1; then
    got="$(curl -fsS --max-time 10 -r 0-3 "$url" | wc -c)"
    [ "$got" -eq 4 ] || { echo "archive server ignored a Range request (got $got bytes)" >&2; exit 1; }
    exit 0
  fi
  if ! kill -0 "$pid" 2>/dev/null; then tail -n 50 "$dir/http.log" >&2; exit 1; fi
  sleep 1
done
tail -n 50 "$dir/http.log" >&2
exit 1
`, shellQuoteForTest(dir), shellQuoteForTest(privateIP), port, hostedArchiveFile)
}

// hostedArchiveCleanupCommand stops the file server and deletes the archive
// and its helpers. Safe to run twice and when nothing was set up.
func hostedArchiveCleanupCommand(dir string) string {
	return fmt.Sprintf(`set +e
dir=%s
if [ -s "$dir/http.pid" ]; then kill "$(cat "$dir/http.pid")" 2>/dev/null; fi
if [ -s "$dir/save.sh" ]; then pkill -f "$dir/save.sh" 2>/dev/null; fi
rm -rf "$dir"
`, shellQuoteForTest(dir))
}

// hostedImageNodeProgress tracks one node's import through the image row the
// platform shows an operator: when it started, when it went from downloading
// to loading, and when it finished. The phase boundary is read from the row's
// message ("downloading ..." then "loading image archive"), so it is as fine
// as the polling interval.
type hostedImageNodeProgress struct {
	Started, LoadStarted, Finished time.Time
}

// Download is the time from the first syncing row to the first loading one.
func (p hostedImageNodeProgress) Download() time.Duration {
	if p.LoadStarted.IsZero() || p.Started.IsZero() {
		return 0
	}
	return p.LoadStarted.Sub(p.Started)
}

// Load is the time from the first loading row to present.
func (p hostedImageNodeProgress) Load() time.Duration {
	if p.LoadStarted.IsZero() || p.Finished.IsZero() {
		return 0
	}
	return p.Finished.Sub(p.LoadStarted)
}

// Total is the time from the first syncing row to present.
func (p hostedImageNodeProgress) Total() time.Duration {
	if p.Started.IsZero() || p.Finished.IsZero() {
		return 0
	}
	return p.Finished.Sub(p.Started)
}

// Observe folds one poll of the node's image row in and reports whether the
// import finished. A failed row, or one that goes back to absent after the
// transfer began, is an error carrying the platform's own message.
func (p *hostedImageNodeProgress) Observe(now time.Time, row dto.ImageNode) (bool, error) {
	switch row.State {
	case "present":
		if p.Started.IsZero() {
			p.Started = now
		}
		p.Finished = now
		return true, nil
	case "failed":
		return false, fmt.Errorf("node image import failed: %s", row.Message)
	case "absent":
		if !p.Started.IsZero() {
			return false, fmt.Errorf("node image row went back to absent after the transfer started: %s", row.Message)
		}
	default:
		if p.Started.IsZero() {
			p.Started = now
		}
		if p.LoadStarted.IsZero() && strings.HasPrefix(strings.ToLower(row.Message), "loading") {
			p.LoadStarted = now
		}
	}
	return false, nil
}

// hostedImportedImage is the catalog image the case imported and the archive
// address each rank was given.
type hostedImportedImage struct {
	SeriesID, ImageID uint
	Ref               string
	URLs              []string
}

func hostedMergeSources(existing, add []string) []string {
	merged := append([]string(nil), existing...)
	for _, entry := range add {
		present := false
		for _, have := range merged {
			if have == entry {
				present = true
				break
			}
		}
		if !present {
			merged = append(merged, entry)
		}
	}
	return merged
}

func hostedFindImageVersion(ctx context.Context, api *hostedAPIClient, ref string) (*dto.Image, error) {
	var listed apiwire.List[dto.Image]
	if err := api.GetJSON(ctx, "/api/v1/images/versions", &listed); err != nil {
		return nil, err
	}
	for i := range listed.Items {
		if pullref.SameRef(listed.Items[i].ImageRef, ref) {
			return &listed.Items[i], nil
		}
	}
	return nil, nil
}

// hostedCustomImageBody is what the console's custom-image form sends
// (ui/src/pages/ImagesPage.tsx importCustom) for a vLLM CUDA runner whose
// fetch addresses are archive URLs.
func hostedCustomImageBody(ref string, archiveURLs []string) (map[string]any, error) {
	name, tag, ok := strings.Cut(ref[strings.LastIndex(ref, "/")+1:], ":")
	if !ok || name == "" || tag == "" {
		return nil, fmt.Errorf("image %q has no explicit tag", ref)
	}
	// Runner tags read <backend><version>-<engine><version>[-r<revision>].
	backend, engine, ok := strings.Cut(tag, "-")
	if !ok || !strings.HasPrefix(backend, "cuda") || !strings.HasPrefix(engine, "vllm") {
		return nil, fmt.Errorf("image tag %q is not a <cuda version>-vllm<version> runner tag", tag)
	}
	if version, revision, cut := strings.Cut(engine, "-r"); cut && revision != "" && strings.Trim(revision, "0123456789") == "" {
		engine = version
	}
	return map[string]any{"series": []any{map[string]any{
		"name": hostedArchiveSeriesName, "engine": "vllm", "framework": "cuda", "source": "custom",
		"versions": []any{map[string]any{
			"engine_version": strings.TrimPrefix(engine, "vllm"), "framework_version": strings.TrimPrefix(backend, "cuda"),
			"arch": "multi", "image_ref": ref, "source": "custom", "rolling_tag": false, "registry_sources": archiveURLs,
		}},
	}}}, nil
}

// hostedDeclareArchiveImage makes sure the catalog has the runner image with
// every archive URL on its fetch addresses, creating a custom image when the
// catalog has none and otherwise appending to what is there (an official
// version may take addresses too).
func hostedDeclareArchiveImage(ctx context.Context, api *hostedAPIClient, ref string, urls []string) (*dto.Image, error) {
	image, err := hostedFindImageVersion(ctx, api, ref)
	if err != nil {
		return nil, fmt.Errorf("list image versions: %w", err)
	}
	if image == nil {
		body, err := hostedCustomImageBody(ref, urls)
		if err != nil {
			return nil, err
		}
		if err := api.PostJSON(ctx, "/api/v1/images/import", body, nil); err != nil {
			return nil, fmt.Errorf("add the custom image %s: %w", ref, err)
		}
	} else if sources := hostedMergeSources(image.RegistrySources, urls); len(sources) != len(image.RegistrySources) {
		if err := api.PatchJSON(ctx, fmt.Sprintf("/api/v1/images/versions/%d", image.ID), map[string]any{"registry_sources": sources}, nil); err != nil {
			return nil, fmt.Errorf("declare the archive URLs on image %d: %w", image.ID, err)
		}
	}
	image, err = hostedFindImageVersion(ctx, api, ref)
	if err != nil {
		return nil, fmt.Errorf("list image versions: %w", err)
	}
	if image == nil {
		return nil, fmt.Errorf("image %s is not in the catalog after declaring it", ref)
	}
	have := map[string]bool{}
	for _, source := range image.RegistrySources {
		have[source] = true
	}
	for _, archive := range urls {
		if !have[archive] {
			return nil, fmt.Errorf("image %d fetch addresses %v lack %s", image.ID, image.RegistrySources, archive)
		}
	}
	return image, nil
}

func hostedPreviewArchive(ctx context.Context, api *hostedAPIClient, archiveURL string) (dto.ImageArchivePreview, error) {
	var preview dto.ImageArchivePreview
	if err := api.PostJSON(ctx, "/api/v1/images/preview-archive-url", dto.ImageArchivePreviewRequest{URL: archiveURL}, &preview); err != nil {
		switch hostedAPIErrorCode(err) {
		case "IMAGE_ARCHIVE_URL_UNREACHABLE":
			return preview, fmt.Errorf("the control plane cannot read %s (is the VM's private address in outbound_allow_private_hosts, and the port open between VMs?): %w", archiveURL, err)
		case "IMAGE_ARCHIVE_URL_INVALID", "IMAGE_ARCHIVE_URL_REQUIRED":
			return preview, fmt.Errorf("the control plane rejected the archive URL %s: %w", archiveURL, err)
		}
		return preview, err
	}
	return preview, nil
}

// hostedRequireArchiveCarries checks the previewed archive against the image
// being imported, as the console's dialog does: a previewable archive that
// does not carry the image is refused; an unpreviewable one only warns.
func hostedRequireArchiveCarries(preview dto.ImageArchivePreview, ref string) (warning string, err error) {
	if preview.SizeBytes <= 0 {
		return "", fmt.Errorf("archive %s reports size %d", preview.URL, preview.SizeBytes)
	}
	if !preview.Previewable {
		return fmt.Sprintf("archive %s cannot be previewed before the download (size %d bytes); the node reports the reference it loaded", preview.URL, preview.SizeBytes), nil
	}
	for _, tag := range preview.RepoTags {
		if pullref.SameRef(tag, ref) {
			return "", nil
		}
	}
	return "", fmt.Errorf("archive %s carries %v, not %s", preview.URL, preview.RepoTags, ref)
}

func hostedFetchArchive(ctx context.Context, api *hostedAPIClient, imageID uint, nodeUUID, archiveURL string) (dto.ImageNodeSyncResponse, error) {
	var response dto.ImageNodeSyncResponse
	err := api.PostJSON(ctx, fmt.Sprintf("/api/v1/images/%d/nodes/%s/sync", imageID, nodeUUID),
		dto.ImageNodeSyncRequest{Source: "url", ArchiveURL: archiveURL}, &response)
	if err != nil {
		return response, err
	}
	if response.State != "syncing" {
		return response, fmt.Errorf("fetching the archive started in state %q (%s), want syncing", response.State, response.Message)
	}
	return response, nil
}

// hostedImageNodeRow reads one node's row of one image version from the
// series detail, the endpoint the console's image page polls.
func hostedImageNodeRow(ctx context.Context, api *hostedAPIClient, seriesID, imageID uint, nodeUUID string) (dto.ImageNode, error) {
	var series dto.ImageSeries
	if err := api.GetJSON(ctx, fmt.Sprintf("/api/v1/images/%d", seriesID), &series); err != nil {
		return dto.ImageNode{}, err
	}
	for _, version := range series.Versions {
		if version.ID != imageID {
			continue
		}
		for _, row := range version.Nodes {
			if row.NodeUUID == nodeUUID {
				return row, nil
			}
		}
		return dto.ImageNode{}, fmt.Errorf("image %d has no row for node %s", imageID, nodeUUID)
	}
	return dto.ImageNode{}, fmt.Errorf("image series %d has no version %d", seriesID, imageID)
}

// hostedWaitImageNodePresent polls the node's image row until it is present,
// logging progress, and returns the phase timings.
func hostedWaitImageNodePresent(ctx context.Context, logf func(string, ...any), api *hostedAPIClient, rank int, seriesID, imageID uint, nodeUUID string, timeout time.Duration) (hostedImageNodeProgress, dto.ImageNode, error) {
	var progress hostedImageNodeProgress
	deadline := time.Now().Add(timeout)
	nextLog := time.Now()
	var row dto.ImageNode
	var lastErr error
	for time.Now().Before(deadline) {
		row, lastErr = hostedImageNodeRow(ctx, api, seriesID, imageID, nodeUUID)
		if lastErr == nil {
			done, err := progress.Observe(time.Now(), row)
			if err != nil {
				return progress, row, err
			}
			if done {
				return progress, row, nil
			}
			if time.Now().After(nextLog) {
				logf("rank %d image import: state=%s %.1f/%.1f GiB rate=%.0f MiB/s %q", rank, row.State,
					float64(row.TransferredBytes)/(1<<30), float64(row.SizeBytes)/(1<<30), float64(row.RateBPS)/(1<<20), row.Message)
				nextLog = time.Now().Add(20 * time.Second)
			}
		}
		select {
		case <-ctx.Done():
			return progress, row, ctx.Err()
		case <-time.After(hostedArchivePollInterval):
		}
	}
	return progress, row, fmt.Errorf("node image row not present within %s: state=%s message=%q last poll error=%v", timeout, row.State, row.Message, lastErr)
}

// hostedSaveArchive runs `docker save` of the image on the VM at rank and
// waits for it, returning how long the save took on the VM's clock.
func hostedSaveArchive(ctx context.Context, logf func(string, ...any), harness *hostedControlPlane, rank int, image string) (hostedArchiveSave, error) {
	instance := harness.VMs[rank].Instance
	dir := hostedArchiveDir(harness.RemoteRoot)
	if out, err := runCloudCommand(ctx, instance, "mkdir -p "+shellQuoteForTest(dir)); err != nil {
		return hostedArchiveSave{}, fmt.Errorf("create %s: %w (%s)", dir, err, strings.TrimSpace(out))
	}
	harness.archiveDirs.Store(rank, dir)
	if err := hostedUploadBytes(ctx, instance, []byte(hostedArchiveSaveScript(dir, image)), dir+"/save.sh", 0o700); err != nil {
		return hostedArchiveSave{}, fmt.Errorf("upload the save script: %w", err)
	}
	if err := hostedUploadBytes(ctx, instance, []byte(hostedArchiveServerScript), dir+"/serve.py", 0o600); err != nil {
		return hostedArchiveSave{}, fmt.Errorf("upload the archive server: %w", err)
	}
	if out, err := runCloudCommand(ctx, instance, hostedArchiveSaveStartCommand(dir)); err != nil {
		return hostedArchiveSave{}, fmt.Errorf("start docker save: %w (%s)", err, strings.TrimSpace(out))
	}
	deadline := time.Now().Add(hostedArchiveSaveTimeout)
	nextLog := time.Now()
	for time.Now().Before(deadline) {
		out, err := runCloudCommand(ctx, instance, hostedArchiveSavePollCommand(dir))
		if err == nil {
			save, parseErr := parseHostedArchiveSave(out)
			if parseErr != nil {
				return hostedArchiveSave{}, fmt.Errorf("docker save: %w", parseErr)
			}
			if save.Done {
				return save, nil
			}
			if time.Now().After(nextLog) {
				logf("rank %d docker save: %s", rank, save.Detail)
				nextLog = time.Now().Add(30 * time.Second)
			}
		}
		select {
		case <-ctx.Done():
			return hostedArchiveSave{}, ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return hostedArchiveSave{}, fmt.Errorf("docker save did not finish within %s", hostedArchiveSaveTimeout)
}

// hostedImportRunnerImageFromArchive gives both udocker nodes the runner image
// without a registry pull: each VM saves the image from its Docker store and
// serves the tar on its private address, the control plane previews and
// declares the two addresses, and each node downloads its own VM's archive
// directly and loads it. It returns once both node image rows are present,
// with the harness files removed, and logs the save, download and load times
// to compare with the pull this replaces.
func hostedImportRunnerImageFromArchive(t *testing.T, harness *hostedControlPlane, image string) hostedImportedImage {
	t.Helper()
	if len(harness.Nodes) != len(harness.VMs) {
		t.Fatalf("image import needs one node per VM, got %d nodes on %d VMs", len(harness.Nodes), len(harness.VMs))
	}
	type result struct {
		save hostedArchiveSave
		err  error
	}
	saves := make([]result, len(harness.VMs))
	var wg sync.WaitGroup
	for rank := range harness.VMs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			save, err := hostedSaveArchive(t.Context(), t.Logf, harness, rank, image)
			if err == nil {
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
				defer cancel()
				var out string
				out, err = runCloudCommand(ctx, harness.VMs[rank].Instance, hostedArchiveServeCommand(hostedArchiveDir(harness.RemoteRoot), harness.VMs[rank].Instance.PrivateIP, hostedArchivePort))
				if err != nil {
					err = fmt.Errorf("serve the archive: %w (%s)", err, strings.TrimSpace(out))
				}
			}
			saves[rank] = result{save, err}
		}()
	}
	wg.Wait()
	urls := make([]string, len(harness.VMs))
	for rank, saved := range saves {
		if saved.err != nil {
			t.Fatalf("rank %d: save %s as an archive: %v", rank, image, saved.err)
		}
		urls[rank] = hostedArchiveURL(harness.VMs[rank].Instance.PrivateIP)
		t.Logf("rank %d docker save %s: %.1f GiB in %ds, served at %s", rank, image, float64(saved.save.SizeBytes)/(1<<30), saved.save.Seconds, urls[rank])
	}

	// What an operator does in the console: paste each address and read what
	// the archive carries, then record the addresses on the image.
	for rank, archive := range urls {
		// The first API call after the VMs wrote the archives can outlast the
		// client's timeout; each attempt's duration is logged so a slow answer
		// and one that never comes are told apart.
		var preview dto.ImageArchivePreview
		var err error
		for attempt := 1; attempt <= 4; attempt++ {
			started := time.Now()
			preview, err = hostedPreviewArchive(t.Context(), harness.API, archive)
			if err == nil {
				if attempt > 1 {
					t.Logf("rank %d preview answered on attempt %d in %s", rank, attempt, time.Since(started).Round(time.Millisecond))
				}
				break
			}
			t.Logf("rank %d preview attempt %d failed after %s: %v", rank, attempt, time.Since(started).Round(time.Millisecond), err)
			if hostedAPIErrorCode(err) != "" {
				break
			}
		}
		if err != nil {
			t.Fatalf("rank %d preview %s: %v", rank, archive, err)
		}
		warning, err := hostedRequireArchiveCarries(preview, image)
		if err != nil {
			t.Fatalf("rank %d: %v", rank, err)
		}
		if warning != "" {
			t.Logf("rank %d: %s", rank, warning)
		} else {
			t.Logf("rank %d preview: %s carries %v, %d bytes", rank, preview.URL, preview.RepoTags, preview.SizeBytes)
		}
		if preview.SizeBytes != saves[rank].save.SizeBytes {
			t.Fatalf("rank %d: control plane sees %d bytes at %s, the VM saved %d", rank, preview.SizeBytes, archive, saves[rank].save.SizeBytes)
		}
	}
	declared, err := hostedDeclareArchiveImage(t.Context(), harness.API, image, urls)
	if err != nil {
		t.Fatalf("declare the runner image archive: %v", err)
	}
	imported := hostedImportedImage{SeriesID: declared.SeriesID, ImageID: declared.ID, Ref: declared.ImageRef, URLs: urls}

	// Each node downloads from its own VM, concurrently.
	type imported1 struct {
		progress hostedImageNodeProgress
		row      dto.ImageNode
		err      error
	}
	rows := make([]imported1, len(harness.Nodes))
	for rank, node := range harness.Nodes {
		if _, err := hostedFetchArchive(t.Context(), harness.API, imported.ImageID, node.UUID, urls[rank]); err != nil {
			t.Fatalf("rank %d: import the archive: %v", rank, err)
		}
	}
	for rank, node := range harness.Nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			progress, row, err := hostedWaitImageNodePresent(t.Context(), t.Logf, harness.API, rank, imported.SeriesID, imported.ImageID, node.UUID, hostedArchiveImportTimeout)
			rows[rank] = imported1{progress, row, err}
		}()
	}
	wg.Wait()
	for rank, done := range rows {
		if done.err != nil {
			t.Fatalf("rank %d: %v\n%s", rank, done.err, harness.Diagnostics(t.Context()))
		}
		if done.row.Source != "url" {
			t.Fatalf("rank %d image row source=%q, want url (the node must have downloaded the archive, not pulled)", rank, done.row.Source)
		}
		load := "under the poll interval"
		if done.progress.Load() > 0 {
			load = done.progress.Load().Round(time.Second).String()
		}
		t.Logf("rank %d image import timing: save=%ds download=%s load=%s import_total=%s (archive %.1f GiB, node message %q)", rank,
			saves[rank].save.Seconds, done.progress.Download().Round(time.Second), load, done.progress.Total().Round(time.Second),
			float64(done.row.SizeBytes)/(1<<30), done.row.Message)
	}

	// The tars are 50 GiB each: free the disk the deployment's container
	// unpack needs, and stop serving.
	harness.cleanupArchives(t.Context())
	return imported
}

// hostedRequireImportedImageStillPresent checks, after the deployment ran,
// that every node still holds the image from the archive and that no registry
// pull replaced it.
func hostedRequireImportedImageStillPresent(t *testing.T, harness *hostedControlPlane, imported hostedImportedImage) {
	t.Helper()
	for rank, node := range harness.Nodes {
		row, err := hostedImageNodeRow(t.Context(), harness.API, imported.SeriesID, imported.ImageID, node.UUID)
		if err != nil {
			t.Fatalf("rank %d image row after the deployment: %v", rank, err)
		}
		if row.State != "present" || row.Source != "url" {
			t.Fatalf("rank %d image row after the deployment is state=%q source=%q (%q), want present from url", rank, row.State, row.Source, row.Message)
		}
	}
}

// cleanupArchives stops every archive server and deletes the tars. It is safe
// to call repeatedly and for VMs that never held one.
func (h *hostedControlPlane) cleanupArchives(ctx context.Context) {
	if h == nil {
		return
	}
	h.archiveDirs.Range(func(key, value any) bool {
		rank, dir := key.(int), value.(string)
		if rank < 0 || rank >= len(h.VMs) || h.VMs[rank] == nil || h.VMs[rank].Instance == nil {
			return true
		}
		cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		_, _ = runCloudCommand(cleanCtx, h.VMs[rank].Instance, hostedArchiveCleanupCommand(dir))
		cancel()
		h.archiveDirs.Delete(key)
		return true
	})
}

// hostedPrivateAddresses lists the VMs' private addresses, which the control
// plane must be allowed to read archives from.
func hostedPrivateAddresses(vms []*cloudTestEnv) ([]string, error) {
	out := make([]string, 0, len(vms))
	for rank, vm := range vms {
		ip := net.ParseIP(vm.Instance.PrivateIP)
		if ip == nil || !ip.IsPrivate() {
			return nil, fmt.Errorf("rank %d has no private address (%q)", rank, vm.Instance.PrivateIP)
		}
		out = append(out, ip.String())
	}
	return out, nil
}

// hostedServerConfigYAML is the server.yaml the hosted control plane starts
// with when it must read archives from the VMs' private addresses (the
// documented outbound_allow_private_hosts exception).
func hostedServerConfigYAML(allowPrivateHosts []string) (string, error) {
	var b strings.Builder
	b.WriteString("outbound_allow_private_hosts:\n")
	for _, host := range allowPrivateHosts {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsPrivate() {
			return "", fmt.Errorf("outbound_allow_private_hosts entry %q is not a private IP", host)
		}
		b.WriteString("  - " + strconv.Quote(ip.String()) + "\n")
	}
	return b.String(), nil
}
