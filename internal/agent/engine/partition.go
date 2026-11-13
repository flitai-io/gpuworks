package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	stdruntime "runtime"
	"sort"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/dgpucache"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// partitionStateRoot is the per-node state directory holding runtime asset
// bundles and per-instance partition state for filesystem-backed backends.
const partitionStateRoot = "dgpu-runtime"

// partitionInjector is the agent-side executor of one partition backend. It
// applies the backend's container injection, reports the host-side assets
// execution would use, and removes per-instance host state on teardown. Each
// backend owns one file in this package; the engine dispatches by backend ID
// and never branches on backend identity.
type partitionInjector interface {
	// apply mutates ws in place for the instance. ok=false with a reason
	// fails the instance rather than silently running unlimited.
	apply(e *Engine, ctx context.Context, ws *agentruntime.WorkloadSpec, spec proto.InstanceDesiredState) (bool, string)
	// assetReports returns the asset bundles execution would use, or nil for
	// backends whose assets are embedded in the agent binary.
	assetReports(e *Engine) []proto.AssetReport
	// cleanupInstance removes per-instance host state (state dirs, ledgers).
	cleanupInstance(e *Engine, uuid string) error
	// applyLimit holds a running instance to the memory limit its spec
	// names, in place, reporting whether anything changed. A backend that
	// cannot (accelerator.LiveMemoryLimit is false for it) returns
	// errLiveLimitUnsupported; its instance takes a new limit at restart.
	applyLimit(e *Engine, spec proto.InstanceDesiredState) (bool, error)
}

// errLiveLimitUnsupported: the backend reads its limit only when the
// container starts.
var errLiveLimitUnsupported = errors.New("the partition backend takes a new memory limit only at restart")

// reconcilePartitionLimit holds a running partitioned instance to the memory
// limit its desired state names. The server rewrites the limit of a running
// instance in place (PUT /deployments/{id}/dgpu); its container keeps
// running, so the new limit reaches it here, after the instance is adopted
// or has started. Failing to apply it is logged, never fatal: the instance
// keeps serving under the limit it has.
func (e *Engine) reconcilePartitionLimit(spec proto.InstanceDesiredState) {
	if spec.VRAMLimitBytes <= 0 || len(spec.GPUIndexes) == 0 {
		return
	}
	backendID := spec.PartitionBackend
	if backendID == "" {
		backendID = accelerator.BackendNVIDIALogical
	}
	injector, ok := partitionInjectors[backendID]
	if !ok {
		return
	}
	changed, err := injector.applyLimit(e, spec)
	switch {
	case err == nil && changed:
		e.log.Info("dGPU memory limit applied to the running instance", "uuid", spec.UUID, "vram_limit_bytes", spec.VRAMLimitBytes)
	case err == nil, errors.Is(err, errLiveLimitUnsupported), errors.Is(err, os.ErrNotExist), errors.Is(err, dgpucache.ErrCacheUninitialized):
		// Nothing to hold, or nothing yet: a cache HAMi-core has not set up is
		// set up from the spec's own environment.
	default:
		e.log.Warn("dGPU memory limit not applied to the running instance; it applies at restart", "uuid", spec.UUID, "vram_limit_bytes", spec.VRAMLimitBytes, "err", err)
	}
}

// partitionInjectors registers the agent-side executor for every partition
// backend. Adding a backend means adding its implementation file plus one
// entry here; TestPartitionInjectorsCoverRegistry keeps the map honest.
var partitionInjectors = map[string]partitionInjector{
	accelerator.BackendNVIDIALogical: nvidiaPartitionInjector{},
	accelerator.BackendAscendLogical: ascendPartitionInjector{},
	accelerator.BackendPPULogical:    ppuPartitionInjector{},
	accelerator.BackendHygonLogical:  hygonPartitionInjector{},
	accelerator.BackendAMDLogical:    amdPartitionInjector{},
}

func (e *Engine) applyPartition(ctx context.Context, ws *agentruntime.WorkloadSpec, spec proto.InstanceDesiredState) (bool, string) {
	if spec.VRAMLimitBytes <= 0 {
		return true, ""
	}
	backendID := strings.TrimSpace(spec.PartitionBackend)
	if backendID == "" { // desired state from an older server
		backendID = accelerator.BackendNVIDIALogical
	}
	if _, ok := accelerator.Lookup(backendID); !ok {
		return false, "unsupported GPU partition backend: " + backendID
	}
	injector, ok := partitionInjectors[backendID]
	if !ok {
		return false, "partition backend has no agent executor: " + backendID
	}
	return injector.apply(e, ctx, ws, spec)
}

// BuildPartitionProbeWorkload constructs a short-lived workload through the
// exact same device- and partition-injection functions used by deployment.
// The probe package owns execution; keeping construction here prevents a
// metadata-only probe from drifting away from the real contract.
func (e *Engine) BuildPartitionProbeWorkload(ctx context.Context, backend, image string, gpuIndex int, quotaBytes int64, probeID string) (agentruntime.WorkloadSpec, error) {
	spec := proto.InstanceDesiredState{
		UUID: probeID, Image: image, GPUIndexes: []int{gpuIndex},
		VRAMLimitBytes: quotaBytes, SMLimitPct: 25, PartitionBackend: backend,
	}
	if backend == accelerator.BackendHygonLogical {
		// The deployment API currently exposes Hygon memory slicing only.
		// Exercise its actual shared-compute path in the behavior probe.
		spec.SMLimitPct = 0
	}
	ws := toWorkloadSpec(spec, "gpuworks-partition-probe-"+sanitizeID(backend)+"-"+strconv.Itoa(gpuIndex))
	if err := e.applyDeviceInjection(&ws, spec.GPUIndexes); err != nil {
		return agentruntime.WorkloadSpec{}, err
	}
	if ok, reason := e.applyPartition(ctx, &ws, spec); !ok {
		return agentruntime.WorkloadSpec{}, fmt.Errorf("%s", reason)
	}
	if provider, ok := e.prober.(interface {
		PartitionProbeLibraries(context.Context) ([]gpu.ProbeLibrary, error)
	}); ok {
		libraries, err := provider.PartitionProbeLibraries(ctx)
		if err != nil {
			return agentruntime.WorkloadSpec{}, fmt.Errorf("prepare partition probe runtime: %w", err)
		}
		const probeLibraryDir = "/opt/gpuworks/probe-libs"
		for _, library := range libraries {
			name := filepath.Base(strings.TrimSpace(library.Name))
			if name == "." || name == "" || name != strings.TrimSpace(library.Name) || strings.TrimSpace(library.Source) == "" {
				return agentruntime.WorkloadSpec{}, fmt.Errorf("invalid partition probe library: %+v", library)
			}
			ws.Mounts = append(ws.Mounts, agentruntime.Mount{Source: library.Source, Target: filepath.Join(probeLibraryDir, name), ReadOnly: true})
		}
		if len(libraries) > 0 {
			if existing := strings.TrimSpace(ws.Env[accelerator.EnvLDLibraryPath]); existing != "" {
				ws.Env[accelerator.EnvLDLibraryPath] = probeLibraryDir + ":" + existing
			} else {
				ws.Env[accelerator.EnvLDLibraryPath] = probeLibraryDir
			}
		}
	}
	ws.Env["GPUWORKS_PARTITION_PROBE_BACKEND"] = backend
	ws.Env["GPUWORKS_PARTITION_PROBE_DEVICE"] = strconv.Itoa(gpuIndex)
	if backend == accelerator.BackendPPULogical {
		ws.Env["GPUWORKS_PARTITION_PROBE_UUID"] = ws.Env[accelerator.PPUEnvDeviceUUIDs]
	}
	ws.Env["GPUWORKS_PARTITION_PROBE_QUOTA_MIB"] = strconv.FormatInt((quotaBytes+(1<<20)-1)/(1<<20), 10)
	return ws, nil
}

// CleanupPartitionProbe removes the temporary ledger/config directory after
// the probe container has been deleted, releasing its virtual NPU id.
func (e *Engine) CleanupPartitionProbe(probeID string) error {
	return e.cleanupPartitionState(probeID)
}

func (e *Engine) cleanupPartitionState(uuid string) error {
	var cleanupErr error
	for _, injector := range partitionInjectors {
		cleanupErr = errors.Join(cleanupErr, injector.cleanupInstance(e, uuid))
	}
	return cleanupErr
}

// PartitionAssetReports returns exactly the directory and filenames the
// execution path would use. Readiness remains a server-side conclusion.
func (e *Engine) PartitionAssetReports(ctx context.Context) []proto.AssetReport {
	reports := []proto.AssetReport{}
	for _, backend := range accelerator.All() {
		if e.prober == nil || accelerator.NormalizeVendor(backend.Metadata().Vendor) != accelerator.NormalizeVendor(e.prober.Vendor()) {
			continue
		}
		injector, ok := partitionInjectors[backend.ID()]
		if !ok {
			continue
		}
		reports = append(reports, injector.assetReports(e)...)
	}
	return reports
}

type partitionAsset struct {
	Root  string
	Files map[string]string
}

// ObservedPartitionAssets scans the same state layout used by partition
// injection without requiring a container driver. Asset presence is a host
// fact and must still be reported when docker/udocker is unavailable; the
// server combines that fact with runtime availability to derive readiness.
func ObservedPartitionAssets(ctx context.Context, stateDir string, prober gpu.Prober) []proto.AssetReport {
	if prober == nil {
		return []proto.AssetReport{}
	}
	scanner := NewWithProber(nil, nil, nil, stateDir, prober)
	return scanner.PartitionAssetReports(ctx)
}

// scanPartitionAssets walks the configured asset roots for one backend's
// runtime-asset bundles and returns the best complete candidate. aliasDir is
// the backend's directory name inside a root ("ascend", "ppu"); acceptDir
// optionally narrows which directories may supply a complete bundle.
func (e *Engine) scanPartitionAssets(backend accelerator.Backend, aliasDir string, acceptDir func(root, aliasDir, dir string) bool) (partitionAsset, error) {
	roots := []string{}
	if configured := strings.TrimSpace(os.Getenv("GPUWORKS_PARTITION_ASSETS")); configured != "" {
		roots = append(roots, configured)
	}
	roots = append(roots, filepath.Join(e.stateDir, partitionStateRoot), filepath.Join(e.stateDir, "partition-assets"), filepath.Join("/opt/gpuworks", partitionStateRoot))
	archHints := []string{stdruntime.GOARCH}
	if stdruntime.GOARCH == "amd64" {
		archHints = append(archHints, "linux-x86_64", "linux-amd64")
	} else if stdruntime.GOARCH == "arm64" {
		archHints = append(archHints, "linux-arm64")
	}
	type candidate struct {
		asset partitionAsset
		rank  int
	}
	var candidates []candidate
	for rank, root := range roots {
		byDir := map[string]partitionAsset{}
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil || entry == nil || entry.IsDir() || !hasArchHint(path, archHints) || !strings.Contains(filepath.ToSlash(path), "/"+aliasDir+"/") {
				return nil //nolint:nilerr // Asset discovery skips unreadable candidates and keeps searching other roots.
			}
			dir := filepath.Dir(path)
			if acceptDir != nil && !acceptDir(root, aliasDir, dir) {
				return nil
			}
			found := byDir[dir]
			found.Root = dir
			if found.Files == nil {
				found.Files = map[string]string{}
			}
			found.Files[entry.Name()] = path
			byDir[dir] = found
			return nil
		})
		for _, found := range byDir {
			if missingAssetNames(backend.RequiredAssets(), found.Files) == nil {
				candidates = append(candidates, candidate{asset: found, rank: rank})
			}
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].rank != candidates[j].rank {
			return candidates[i].rank < candidates[j].rank
		}
		return candidates[i].asset.Root < candidates[j].asset.Root
	})
	if len(candidates) == 0 {
		return partitionAsset{}, fmt.Errorf("required %s asset files not found for %s", strings.Join(backend.RequiredAssets(), ", "), stdruntime.GOARCH)
	}
	return candidates[0].asset, nil
}

// scanAssetReports discovers one backend's complete asset bundle and converts
// it into the wire report; an incomplete or missing bundle yields nil.
func scanAssetReports(e *Engine, backendID, aliasDir string, acceptDir func(root, aliasDir, dir string) bool) []proto.AssetReport {
	backend, ok := accelerator.Lookup(backendID)
	if !ok {
		return nil
	}
	asset, err := e.scanPartitionAssets(backend, aliasDir, acceptDir)
	if err != nil {
		return nil
	}
	return []proto.AssetReport{assetReport(backend.ID(), asset)}
}

// assetReport converts a discovered bundle into the wire report; file order
// is sorted so repeated reports compare equal.
func assetReport(backendID string, asset partitionAsset) proto.AssetReport {
	files := make([]string, 0, len(asset.Files))
	for file := range asset.Files {
		files = append(files, file)
	}
	sort.Strings(files)
	return proto.AssetReport{Backend: backendID, Root: asset.Root, Files: files}
}

func missingAssetNames(required []string, files map[string]string) []string {
	var missing []string
	for _, name := range required {
		if files[name] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return missing
}

func hasArchHint(path string, hints []string) bool {
	path = filepath.ToSlash(path)
	for _, hint := range hints {
		if strings.Contains(path, hint) {
			return true
		}
	}
	return false
}

func writePrivateFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func sanitizeID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	var out strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			out.WriteRune(r)
		} else {
			out.WriteByte('_')
		}
	}
	return out.String()
}
