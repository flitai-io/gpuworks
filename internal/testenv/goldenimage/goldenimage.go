// Package goldenimage holds what every cloud provider's golden-image builder
// shares: the model fixture the L4 cases register (listed from ModelScope and
// downloaded on the source VM with checksums) and the checks that a booted VM
// carries it. Provider-specific parts (renting the source VM, creating the
// image) stay in the builders under test/l4/cmd.
package goldenimage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

const (
	// DefaultModelRepo is the model the L4 deployment cases serve.
	DefaultModelRepo = "Qwen/Qwen3-0.6B"
	DefaultModelRev  = "master"
	// DefaultModelPath is where the image carries it; cases register this path
	// instead of downloading (see GPUWORKS_PRELOADED_MODEL_PATH).
	DefaultModelPath = "/opt/gpuworks/test-fixtures/models/qwen3-0.6b"
	// DefaultDockerImage is the vLLM runner the L4 cases deploy on cards of
	// compute capability 8.0 and later (A10, L20, ...).
	DefaultDockerImage = "gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4"
	// TuringDockerImage is the vLLM runner for Turing cards (T4): its CUDA 12.9
	// build still carries sm_75 kernels, and it runs on later cards as well, so
	// cases that use one image on every card take this one.
	TuringDockerImage = "gpuworks/gpuworks-runner:cuda12.9-vllm0.31.0-r2"
)

// File is one remote model file with the size ModelScope reports for it.
type File struct {
	Path string `json:"path"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
}

// ModelFiles lists the files of a ModelScope model and refuses a repository
// whose total size is not that of a small model: a renamed or replaced repo
// must not silently bake a different multi-gigabyte model into an image.
func ModelFiles(ctx context.Context, repo, rev string) ([]File, error) {
	listed, err := modelrepo.ListFiles(ctx, modelrepo.SourceMS, repo, rev, "", nil)
	if err != nil {
		return nil, fmt.Errorf("list ModelScope files: %w", err)
	}
	var total int64
	files := make([]File, 0, len(listed))
	for _, file := range listed {
		if strings.TrimSpace(file.Path) == "" || file.Size < 0 {
			return nil, fmt.Errorf("invalid ModelScope file metadata: %+v", file)
		}
		total += file.Size
		files = append(files, File{
			Path: file.Path, Size: file.Size,
			URL: modelrepo.ResolveURL(modelrepo.SourceMS, repo, file.Revision, file.Path, ""),
		})
	}
	if total < 100<<20 || total > 3<<30 {
		return nil, fmt.Errorf("refusing unexpected model size %d bytes", total)
	}
	return files, nil
}

// ShellQuote single-quotes a value for POSIX sh.
func ShellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

// PrepareModelCommand is the shell command that downloads files into
// modelPath on the source VM, writes SHA256SUMS and replaces the target
// atomically. It requires dockerImage to be present already and never pulls.
// sudo is the privilege prefix ("sudo -n " for a login user, "" for root).
func PrepareModelCommand(modelPath, dockerImage string, files []File, sudo string) string {
	manifest, _ := json.Marshal(files)
	script := `import base64, hashlib, json, os, pathlib, shutil, sys, time, urllib.request
target = pathlib.Path(sys.argv[1])
files = json.loads(base64.b64decode(sys.argv[2]))
staging = target.with_name(target.name + ".partial")
if staging.exists(): shutil.rmtree(staging)
staging.mkdir(parents=True, exist_ok=True)
for item in files:
    rel = pathlib.PurePosixPath(item["path"])
    if rel.is_absolute() or ".." in rel.parts: raise RuntimeError("unsafe model path: " + item["path"])
    dest = staging.joinpath(*rel.parts)
    dest.parent.mkdir(parents=True, exist_ok=True)
    last = None
    for attempt in range(5):
        try:
            req = urllib.request.Request(item["url"], headers={"User-Agent": "gpuworks-golden-image/1"})
            with urllib.request.urlopen(req, timeout=120) as src, open(str(dest) + ".part", "wb") as out:
                shutil.copyfileobj(src, out, 8 * 1024 * 1024)
            os.replace(str(dest) + ".part", dest)
            if item["size"] and dest.stat().st_size != item["size"]:
                raise RuntimeError("size mismatch for %s: %d != %d" % (rel, dest.stat().st_size, item["size"]))
            last = None
            break
        except Exception as exc:
            last = exc
            try: os.unlink(str(dest) + ".part")
            except FileNotFoundError: pass
            time.sleep(2 ** attempt)
    if last is not None: raise last
if not (staging / "config.json").is_file(): raise RuntimeError("model config is missing")
if not any(p.stat().st_size > 0 for p in staging.glob("*.safetensors")): raise RuntimeError("model weights are missing")
with open(staging / "SHA256SUMS", "w", encoding="utf-8") as sums:
    for path in sorted(p for p in staging.rglob("*") if p.is_file() and p.name != "SHA256SUMS"):
        digest = hashlib.sha256()
        with open(path, "rb") as stream:
            for block in iter(lambda: stream.read(8 * 1024 * 1024), b""): digest.update(block)
        sums.write(digest.hexdigest() + "  " + path.relative_to(staging).as_posix() + "\n")
if target.exists(): shutil.rmtree(target)
os.replace(staging, target)
`
	encodedScript := base64.StdEncoding.EncodeToString([]byte(script))
	encodedManifest := base64.StdEncoding.EncodeToString(manifest)
	return "set -eu; " + sudo + "docker image inspect " + ShellQuote(dockerImage) + " >/dev/null; " +
		"printf %s " + ShellQuote(encodedScript) + " | base64 -d >/tmp/gpuworks-prepare-model.py; " +
		sudo + "python3 /tmp/gpuworks-prepare-model.py " + ShellQuote(modelPath) + " " + ShellQuote(encodedManifest) + "; " +
		"rm -f /tmp/gpuworks-prepare-model.py; " + sudo + "sync"
}

// ValidationCommand checks a VM carries the model (with matching checksums)
// and the Docker image.
func ValidationCommand(modelPath, dockerImage, sudo string) string {
	checks := "set -eu; test -s " + ShellQuote(strings.TrimRight(modelPath, "/")+"/config.json") + "; " +
		"find " + ShellQuote(modelPath) + " -maxdepth 1 -name '*.safetensors' -type f -size +0c | grep -q .; " +
		"cd " + ShellQuote(modelPath) + " && sha256sum -c SHA256SUMS >/dev/null; " +
		"docker image inspect " + ShellQuote(dockerImage) + " >/dev/null"
	return sudo + "sh -c " + ShellQuote(checks)
}
