// Command ascend-nerdctl-verify exercises the real production Ascend
// nerdctl device-injection code path end to end: gpu.AscendProber's
// driver-aware RuntimeInjectionForDriver builds the Env/DockerArgs/Mounts,
// and the real nerdctl runtime.Driver (Run/Exec/Remove) starts the
// container. It must run locally on the target host — nerdctl is a CLI, not
// remote-addressable — so the L4 test builds this binary for linux/arm64 from
// the revision under test and streams it over SSH, rather than replaying shell
// strings the way the docker-based L4 tests do. See
// docs/dev/ledgers/engineering-log.md "Ascend device injection for the nerdctl
// runtime driver".
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/nerdctl"
	"gpuworks.ai/gpuworks/internal/assets"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("VERIFY_OK")
}

func run() error {
	image := strings.TrimSpace(os.Getenv("ASCEND_NERDCTL_VERIFY_IMAGE"))
	indexesRaw := strings.TrimSpace(os.Getenv("ASCEND_NERDCTL_VERIFY_INDEXES"))
	if indexesRaw == "" {
		return fmt.Errorf("ASCEND_NERDCTL_VERIFY_INDEXES is required")
	}
	var indexes []int
	for _, s := range strings.Split(indexesRaw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return fmt.Errorf("invalid device index %q: %w", s, err)
		}
		indexes = append(indexes, n)
	}
	if len(indexes) != 1 {
		return fmt.Errorf("exactly one physical device index is required, got %v", indexes)
	}

	driver := nerdctl.New()
	detectCtx, detectCancel := context.WithTimeout(context.Background(), 10*time.Second)
	ok, err := driver.Detect(detectCtx)
	detectCancel()
	if err != nil {
		return fmt.Errorf("detect nerdctl: %w", err)
	}
	if !ok {
		return fmt.Errorf("nerdctl not detected on this host")
	}

	switch strings.TrimSpace(os.Getenv("ASCEND_NERDCTL_VERIFY_MODE")) {
	case "", "device":
		if image == "" {
			return fmt.Errorf("ASCEND_NERDCTL_VERIFY_IMAGE is required in device mode")
		}
		return verifyDeviceInjection(driver, image, indexes)
	case "partition":
		return verifyPartitionCapabilityProbe(driver, indexes[0])
	default:
		return fmt.Errorf("unsupported ASCEND_NERDCTL_VERIFY_MODE")
	}
}

func verifyDeviceInjection(driver *nerdctl.Driver, image string, indexes []int) error {
	// The real production function under test: this is what
	// engine.applyDeviceInjection dispatches to for an Ascend node whose
	// driver is nerdctl.
	prober := &gpu.AscendProber{}
	inj := prober.RuntimeInjectionForDriver(indexes, "nerdctl")
	if inj.UnsupportedReason != "" {
		return fmt.Errorf("unsupported: %s", inj.UnsupportedReason)
	}

	name := "gpuworks-nerdctl-verify-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	ws := runtime.WorkloadSpec{
		Name:        name,
		Image:       image,
		Entrypoint:  []string{"sleep"},
		Cmd:         []string{"300"},
		Env:         inj.Env,
		HostNetwork: true,
		DockerArgs:  inj.DockerArgs,
	}
	for _, m := range inj.Mounts {
		ws.Mounts = append(ws.Mounts, runtime.Mount{Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}

	runCtx, runCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer runCancel()
	id, err := driver.Run(runCtx, ws)
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}
	defer func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer rmCancel()
		_ = driver.Remove(rmCtx, id)
	}()

	for attempt := 0; attempt < 20; attempt++ {
		statusCtx, statusCancel := context.WithTimeout(context.Background(), 10*time.Second)
		status, statusErr := driver.Status(statusCtx, id)
		statusCancel()
		if statusErr == nil && status.Exists && status.Running {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	execCtx, execCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer execCancel()
	out, err := driver.Exec(execCtx, id, []string{"python3", "-c",
		"import torch, torch_npu; print('AVAILABLE', torch.npu.is_available()); print('COUNT', torch.npu.device_count())"})
	fmt.Println(string(out))
	if err != nil {
		return fmt.Errorf("exec probe: %w", err)
	}
	if !strings.Contains(string(out), "AVAILABLE True") {
		return fmt.Errorf("torch.npu.is_available() was not True")
	}
	wantCount := fmt.Sprintf("COUNT %d", len(indexes))
	if !strings.Contains(string(out), wantCount) {
		return fmt.Errorf("torch.npu.device_count() did not report %d", len(indexes))
	}
	return nil
}

const partitionQuotaMiB = int64(4096)

func verifyPartitionCapabilityProbe(driver *nerdctl.Driver, index int) error {
	keepOnFailure := os.Getenv("GPUWORKS_KEEP_ON_FAILURE") == "1"
	succeeded := false
	stateDir, err := os.MkdirTemp("/tmp", "gpuworks-nerdctl-partition-")
	if err != nil {
		return fmt.Errorf("create partition state: %w", err)
	}
	defer func() {
		if succeeded || !keepOnFailure {
			_ = os.RemoveAll(stateDir)
		} else {
			fmt.Println("PRESERVED_PARTITION_STATE", stateDir)
		}
	}()

	installed, err := assets.InstallDomesticRuntimeAssets(stateDir, accelerator.VendorAscend)
	if err != nil {
		return fmt.Errorf("install embedded Ascend partition assets: %w", err)
	}
	for _, item := range installed {
		fmt.Printf("ASSET backend=%s asset=%s arch=%s\n", item.Backend, item.AssetID, item.PlatformArch)
	}

	prober := &gpu.AscendProber{}
	partitionEngine := engine.NewWithProber(driver, nil, slog.Default(), stateDir, prober)
	probeID := "nerdctl-capability-probe-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	defer func() {
		if succeeded || !keepOnFailure {
			if err := partitionEngine.CleanupPartitionProbe(probeID); err != nil {
				slog.Warn("partition probe cleanup failed", "probe", probeID, "err", err)
			}
		}
	}()

	prepareCtx, prepareCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	present, err := driver.ImageExists(prepareCtx, assets.PartitionProbeImage)
	if err == nil && !present {
		var archive io.ReadCloser
		archive, err = assets.OpenPartitionProbeImage()
		if err == nil {
			err = driver.LoadImage(prepareCtx, assets.PartitionProbeImage, archive)
			_ = archive.Close()
		}
	}
	prepareCancel()
	if err != nil {
		return fmt.Errorf("prepare embedded partition probe image: %w", err)
	}

	buildCtx, buildCancel := context.WithTimeout(context.Background(), 45*time.Second)
	ws, err := partitionEngine.BuildPartitionProbeWorkload(
		buildCtx,
		accelerator.BackendAscendLogical,
		assets.PartitionProbeImage,
		index,
		partitionQuotaMiB<<20,
		probeID,
	)
	buildCancel()
	if err != nil {
		return fmt.Errorf("build production partition workload: %w", err)
	}
	configPath := filepath.Join(stateDir, "dgpu-runtime", "instances", probeID, "npu_info.config")
	config, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read production partition config: %w", err)
	}
	fmt.Printf("CONFIG\n%s", config)
	if !strings.Contains(string(config), "physical-npu-id="+strconv.Itoa(index)+"\n") ||
		!strings.Contains(string(config), "memory-quota="+strconv.FormatInt(partitionQuotaMiB, 10)+"\n") {
		return fmt.Errorf("unexpected production partition config: %s", config)
	}
	staleCtx, staleCancel := context.WithTimeout(context.Background(), 15*time.Second)
	_ = driver.Remove(staleCtx, ws.Name)
	staleCancel()

	runCtx, runCancel := context.WithTimeout(context.Background(), 90*time.Second)
	id, err := driver.Run(runCtx, ws)
	runCancel()
	if err != nil {
		return fmt.Errorf("run partition allocation: %w", err)
	}
	defer func() {
		if succeeded || !keepOnFailure {
			rmCtx, rmCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer rmCancel()
			_ = driver.Remove(rmCtx, id)
		} else {
			fmt.Println("PRESERVED_PARTITION_CONTAINER", id)
		}
	}()

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer waitCancel()
	var status runtime.Status
	for {
		status, err = driver.Status(waitCtx, id)
		if err != nil {
			return fmt.Errorf("inspect partition allocation: %w", err)
		}
		if status.Exists && !status.Running {
			break
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("partition allocation timed out")
		case <-time.After(250 * time.Millisecond):
		}
	}

	logsCtx, logsCancel := context.WithTimeout(context.Background(), 15*time.Second)
	rc, logsErr := driver.Logs(logsCtx, id, false, 200)
	var logs []byte
	if logsErr == nil {
		logs, _ = io.ReadAll(io.LimitReader(rc, 64*1024))
		_ = rc.Close()
	}
	logsCancel()
	fmt.Print(string(logs))
	if status.ExitCode != 0 {
		return fmt.Errorf("partition allocation exited %d: %s", status.ExitCode, strings.TrimSpace(string(logs)))
	}
	for _, want := range []string{
		"backend=ascend",
		"total_kb=4194304",
		"quota_mib=4096",
	} {
		if !strings.Contains(string(logs), want) {
			return fmt.Errorf("partition allocation output missing %q: %s", want, strings.TrimSpace(string(logs)))
		}
	}
	for _, note := range ws.PreparationNotes {
		fmt.Println("PREPARATION", note)
	}
	fmt.Println("PARTITION_VERIFY_OK")
	succeeded = true
	return nil
}
