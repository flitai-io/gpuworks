package main

import (
	"context"
	"embed"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
)

// The golden image carries both builds of its NVIDIA driver's kernel
// modules and picks one at boot (nvidia/select-kmod.sh). The open modules
// alone cannot drive GPUs without a GSP (V100S, P40); the proprietary ones
// alone cannot drive Blackwell (RTX 5090). The version must equal the
// userspace driver already in the source image.
const (
	nvidiaDriverVersion = "570.153.02"
	nvidiaDriverSHA256  = "148886e4f69576fa8fa67140e6e5dd6e51f90b2ec74a65f1a7a7334dfa5de1b6"
	nvidiaKmodStore     = "/opt/gpuworks/nvidia-kmod/" + nvidiaDriverVersion
	// nvidiaToolkitVersion is the nvidia-container-toolkit the cn-wlcb golden
	// image carries; images without one get this version.
	nvidiaToolkitVersion = "1.20.0-1"
)

//go:embed nvidia/select-kmod.sh nvidia/gpuworks-nvidia-kmod.service nvidia/install-dual-kmod.sh
var nvidiaFiles embed.FS

func nvidiaFile(name string) string {
	data, err := nvidiaFiles.ReadFile("nvidia/" + name)
	if err != nil {
		panic(err) // embedded at build time
	}
	return strings.NewReplacer("@VERSION@", nvidiaDriverVersion, "@SHA256@", nvidiaDriverSHA256, "@STORE@", nvidiaKmodStore, "@TOOLKIT@", nvidiaToolkitVersion).Replace(string(data))
}

// remoteDualKmodCommand stages the selector and its unit, then builds and
// installs both module flavours on the source VM.
func remoteDualKmodCommand() string {
	stage := func(content, path string) string {
		return "printf %s " + shellQuote(base64.StdEncoding.EncodeToString([]byte(content))) + " | base64 -d >" + path + "; "
	}
	return "set -eu; " +
		stage(nvidiaFile("select-kmod.sh"), "/tmp/gpuworks-nvidia-kmod.select") +
		stage(nvidiaFile("gpuworks-nvidia-kmod.service"), "/tmp/gpuworks-nvidia-kmod.service") +
		stage(nvidiaFile("install-dual-kmod.sh"), "/tmp/gpuworks-install-dual-kmod.sh") +
		"sudo -n sh /tmp/gpuworks-install-dual-kmod.sh; rm -f /tmp/gpuworks-install-dual-kmod.sh"
}

// remoteKmodValidationCommand checks a VM booted from the image: the
// selector ran and chose a flavour that initialised the GPU, and containers
// see the GPU through the toolkit. It prints the chosen flavour.
func remoteKmodValidationCommand(dockerImage string) string {
	checks := "set -eu; systemctl is-enabled gpuworks-nvidia-kmod.service >/dev/null; " +
		"test -s " + shellQuote(nvidiaKmodStore+"/open/nvidia.ko") + "; test -s " + shellQuote(nvidiaKmodStore+"/proprietary/nvidia.ko") + "; " +
		"flavour=$(cat /run/gpuworks-nvidia-kmod); case \"$flavour\" in open|proprietary) ;; *) echo \"selector state: $flavour\" >&2; journalctl -u gpuworks-nvidia-kmod --no-pager | tail -20 >&2; exit 1;; esac; " +
		"nvidia-smi -L; " +
		// The runner's CUDA runtime is newer than the driver; minor version
		// compatibility must carry it, which only a real CUDA call proves.
		"docker run --rm --gpus all --entrypoint python3 " + shellQuote(dockerImage) + " -c \"import torch; torch.ones(1).cuda(); print(torch.cuda.get_device_name(0))\"; " +
		"echo \"KMOD_FLAVOUR=$flavour\""
	return "sudo -n sh -c " + shellQuote(checks)
}

// gpuCheck is the outcome of booting the image on one GPU type.
type gpuCheck struct {
	GPU     string
	Outcome string // ok-open | ok-proprietary | refused | no-stock | failed
	Detail  string
}

// validateOnGPUs boots imageID on each GPU type that has stock and checks
// that the right module flavour loaded. CompShare refusing the image for a
// type (RetCode 226603) and stock-outs are reported, not treated as failures:
// the point is to learn which types the image serves.
func validateOnGPUs(ctx context.Context, accessKey, secretKey, imageID, runID, modelPath, dockerImage string, diskGB int, gpus []string) []gpuCheck {
	var results []gpuCheck
	for _, gpu := range gpus {
		gpu = strings.TrimSpace(gpu)
		if gpu == "" {
			continue
		}
		results = append(results, validateOnGPU(ctx, accessKey, secretKey, imageID, runID, modelPath, dockerImage, diskGB, gpu))
		last := results[len(results)-1]
		fmt.Printf("run_id=%s phase=validate-gpu gpu=%s outcome=%s detail=%q\n", runID, last.GPU, last.Outcome, last.Detail)
	}
	return results
}

// parseGPU splits a -validate-gpus entry, "4090" or "2080Ti*4" (some
// zones only stock wide shapes, e.g. cn-sh2-02 2080Ti x4).
func parseGPU(entry string) (string, int) {
	model, count, ok := strings.Cut(strings.TrimSpace(entry), "*")
	n, err := strconv.Atoi(strings.TrimSpace(count))
	if !ok || err != nil || n < 1 {
		return strings.TrimSpace(model), 1
	}
	return strings.TrimSpace(model), n
}

func validateOnGPU(ctx context.Context, accessKey, secretKey, imageID, runID, modelPath, dockerImage string, diskGB int, entry string) gpuCheck {
	result := gpuCheck{GPU: entry}
	gpu, count := parseGPU(entry)
	provider, err := cloudprovider.NewCompshare(accessKey, secretKey, imageID, runID)
	if err != nil {
		result.Outcome, result.Detail = "failed", err.Error()
		return result
	}
	if _, _, err := provider.SelectPlacement(ctx, gpu, count, diskGB); err != nil {
		switch {
		case strings.Contains(err.Error(), "RetCode 226603"):
			result.Outcome = "refused"
		default:
			result.Outcome = "no-stock"
		}
		result.Detail = err.Error()
		return result
	}
	gpuCtx, cancel := context.WithTimeout(ctx, 25*time.Minute)
	defer cancel()
	instance, err := provider.Rent(gpuCtx, cloudprovider.GPUSpec{GPUModel: gpu, GPUCount: count, DiskGB: diskGB})
	if err != nil {
		result.Outcome, result.Detail = "no-stock", err.Error()
		if cloudprovider.IsImageUnsupportedInZone(err) {
			result.Outcome = "refused"
		}
		return result
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cleanupCancel()
		if err := provider.Destroy(cleanupCtx, instance.InstanceID); err != nil {
			result.Detail += "; destroy: " + err.Error()
		}
	}()
	if err := waitSSH(gpuCtx, instance); err != nil {
		result.Outcome, result.Detail = "failed", "SSH: "+err.Error()
		return result
	}
	if _, err := runSSH(gpuCtx, instance, remoteValidationCommand(modelPath, dockerImage)); err != nil {
		result.Outcome, result.Detail = "failed", "fixtures: "+err.Error()
		return result
	}
	out, err := runSSH(gpuCtx, instance, remoteKmodValidationCommand(dockerImage))
	if err != nil {
		result.Outcome, result.Detail = "failed", strings.TrimSpace(out+" "+err.Error())
		return result
	}
	flavour := ""
	for _, line := range strings.Split(out, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "KMOD_FLAVOUR="); ok {
			flavour = value
		}
	}
	result.Outcome, result.Detail = "ok-"+flavour, firstLine(out)
	return result
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}
