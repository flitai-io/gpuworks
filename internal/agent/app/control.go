package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/agent/hostsim"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/hostproc"
	"gpuworks.ai/gpuworks/internal/commandlog"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

// Bind attaches the current connection's sender.
func (n *NodeRuntime) Bind(send conn.SendFunc) {
	n.sendMu.Lock()
	n.observationConnectionEpoch++
	n.send = send
	pending := append([]proto.BenchmarkStatus(nil), n.benchPending...)
	n.benchPending = nil
	n.sendMu.Unlock()
	for i, st := range pending {
		if err := send(proto.TStatusBenchmark, st); err != nil {
			n.sendMu.Lock()
			n.benchPending = append(pending[i:], n.benchPending...)
			n.sendMu.Unlock()
			return
		}
	}
}

// Unbind detaches on disconnect.
func (n *NodeRuntime) Unbind() {
	n.sendMu.Lock()
	n.observationConnectionEpoch++
	n.send = nil
	n.sendMu.Unlock()
}

func (n *NodeRuntime) captureObservationConnection() uint64 {
	n.sendMu.Lock()
	defer n.sendMu.Unlock()
	if n.send == nil {
		return 0
	}
	return n.observationConnectionEpoch
}

func (n *NodeRuntime) reportRuntimeObservation(epoch uint64, st proto.InstanceStatus) bool {
	n.sendMu.Lock()
	if epoch == 0 || epoch != n.observationConnectionEpoch || n.send == nil {
		n.sendMu.Unlock()
		return false
	}
	send := n.send
	n.sendMu.Unlock()
	if err := send(proto.TStatusInstance, st); err != nil {
		n.log.Warn("emit runtime observation failed", "err", err)
		return false
	}
	return true
}

func (n *NodeRuntime) emit(t string, d any) {
	n.sendMu.Lock()
	send := n.send
	n.sendMu.Unlock()
	if send == nil {
		n.log.Warn("emit dropped: no bound connection", "t", t)
		return // offline; server will re-sync on reconnect
	}
	if err := send(t, d); err != nil {
		n.log.Warn("emit failed", "t", t, "err", err)
	}
}

// reportInstance is the only path an instance status takes to the server, so it
// is where the one-report-per-second bound is enforced. A state or phase change
// and a terminal state go out immediately; everything between them is progress.
func (n *NodeRuntime) reportInstance(st proto.InstanceStatus) {
	if st.RuntimeObservation != nil && st.State == "" {
		// Runtime observations must enter through reportRuntimeObservation with
		// an epoch captured before inspect. Keeping a current-sender fallback
		// here would allow a future caller to move old facts onto a new session.
		n.log.Warn("runtime observation dropped without connection epoch", "uuid", st.UUID)
		return
	}
	terminal := st.State == proto.InstFailed || st.State == proto.InstStopped
	if st.State == proto.InstFailed {
		n.recheckCardsAfterStartFailure(st.UUID)
	}
	n.progress.report("instance\x00"+st.UUID, st.State+"\x00"+st.Phase, terminal, func() {
		n.emit(proto.TStatusInstance, st)
	})
}

// modelFileProgressKey is the progress gate key of one model file task.
func modelFileProgressKey(key string) string { return "modelfile\x00" + key }

// reportModelFile is the model-weights equivalent. A download reports four
// times a second and a peer sync every few megabytes; neither is a milestone.
func (n *NodeRuntime) reportModelFile(st proto.ModelFileStatus) {
	n.reportModelFileContext(context.Background(), st)
}

func (n *NodeRuntime) reportModelFileContext(ctx context.Context, st proto.ModelFileStatus) {
	terminal := proto.ModelFileStateReady(st.State) ||
		st.State == proto.MFFailed || st.State == proto.MFAbsent || st.State == proto.MFInsufficientDisk
	milestone := st.State
	if st.Preparation != nil {
		milestone += "\x00" + st.Preparation.Attempt + "\x00" + st.Preparation.Phase + fmt.Sprint(st.Preparation.Stalled)
	}
	epoch := n.captureObservationConnection()
	n.progress.report(modelFileProgressKey(st.Key), milestone, terminal, func() {
		if ctx.Err() != nil || (st.Acquisition == proto.ModelAcquireCopy && epoch != n.captureObservationConnection()) {
			return
		}
		n.emit(proto.TStatusModelFile, st)
	})
}

// reportImageNode carries catalog image transfers, whose pull output and
// archive download progress are as verbose as an instance's.
func (n *NodeRuntime) reportImageNode(st proto.ImageNodeStatus) {
	terminal := st.State == "present" || st.State == "failed"
	n.progress.report("image\x00"+st.ImageRef, st.State, terminal, func() {
		n.emit(proto.TStatusImageNode, st)
	})
}

// lockImageOperation serializes the node's image work, and reports that it is
// waiting when another operation already holds the slot.
//
// A queued transfer that says nothing is indistinguishable from a stalled one:
// both show up as a progress bar that never moves. Saying which one it is is
// the difference between "wait, the node is busy" and "this is broken".
func (n *NodeRuntime) lockImageOperation(imageRef, state string) {
	if n.imagePullMu.TryLock() {
		return
	}
	n.reportImageNode(proto.ImageNodeStatus{
		ImageRef: imageRef, State: state,
		Message: "waiting for another image operation on this node to finish",
	})
	n.imagePullMu.Lock()
}

func (n *NodeRuntime) reportBenchmark(st proto.BenchmarkStatus) {
	n.sendMu.Lock()
	send := n.send
	if send == nil {
		n.queueBenchmarkStatusLocked(st)
		n.sendMu.Unlock()
		return
	}
	n.sendMu.Unlock()
	if err := send(proto.TStatusBenchmark, st); err != nil {
		n.sendMu.Lock()
		n.queueBenchmarkStatusLocked(st)
		n.sendMu.Unlock()
	}
}

func (n *NodeRuntime) queueBenchmarkStatusLocked(st proto.BenchmarkStatus) {
	const maxPending = 1024
	if len(n.benchPending) >= maxPending {
		copy(n.benchPending, n.benchPending[len(n.benchPending)-maxPending+1:])
		n.benchPending = n.benchPending[:maxPending-1]
	}
	n.benchPending = append(n.benchPending, st)
}

// OnStateSync applies a full desired state (server replayed on connect).
func (n *NodeRuntime) OnStateSync(ctx context.Context, ds proto.DesiredState) {
	n.refuseModelFiles(n.host.ConfineState(&ds))
	n.log.Info("desired state sync", "instances", len(ds.Instances), "model_files", len(ds.ModelFiles))
	if strings.TrimSpace(ds.RuntimePreference) != "" {
		if err := n.applyRuntimePreference(ctx, ds.RuntimePreference, true); err != nil {
			n.log.Warn("runtime preference could not be applied", "preference", ds.RuntimePreference, "err", err)
		}
	}
	n.setNetwork(ds.Network)
	n.setRegistryAuth(ds.RegistryAuth)
	n.applyRuntimeProxy()
	if ds.ModelsRoot != "" {
		n.setModelsRoot(ds.ModelsRoot)
	}
	n.applyStorages(ds.Storages)
	n.imageRefsMu.Lock()
	n.imageRefs = append([]string(nil), ds.ImageRefs...)
	n.imageRefsMu.Unlock()
	n.imageSeen.forget(ds.ImageRefs)
	if n.engine != nil {
		n.engine.SetDesiredFull(ctx, ds.Instances)
		// The orphan sweep is opt-in (server orphan_cleanup setting). Its
		// name-prefix ownership is not cluster-scoped: with the sweep on by
		// default, any second gpuworks agent sharing this container runtime —
		// a test agent, a leftover binary — would delete this node's
		// production instances on its first state sync.
		if !ds.OrphanCleanup {
			n.log.Debug("orphan container cleanup disabled by server setting")
		} else if err := n.engine.CleanupOrphans(ctx, n.listContainers); err != nil {
			// Leftover containers keep their disk and, when still running, their
			// GPU, so a failed sweep has to be visible on the agent side; there is
			// no per-instance report for a container no deployment refers to.
			n.log.Warn("orphan container cleanup failed; leaked containers may remain", "driver", n.RuntimeDriverName(), "err", err)
		}
	}
	if n.bench != nil {
		n.bench.SetDesiredFull(ctx, ds.Benchmarks)
	}
	// After the desired instances are known: a differing accelerator
	// choice is applied only when none of them runs.
	n.applyAcceleratorPreference(ds.AcceleratorPreference)
	// A full state lists every model file the node should have; a task parked
	// for its storage that is not among them was withdrawn.
	n.withdrawUnlistedDownloads(ds.ModelFiles)
	n.pruneWaiting(ds.ModelFiles)
	for _, mf := range ds.ModelFiles {
		n.reconcileModelFile(ctx, mf)
	}
}

// OnStateUpdate applies an incremental update.
func (n *NodeRuntime) OnStateUpdate(ctx context.Context, su proto.StateUpdate) bool {
	n.refuseModelFiles(n.host.ConfineUpdate(&su))
	imageRefsChanged := false
	if su.Network != nil {
		n.setNetwork(*su.Network)
		n.applyRuntimeProxy()
	}
	if su.ModelsRoot != "" {
		n.setModelsRoot(su.ModelsRoot)
	}
	if su.ImageRefs != nil {
		n.imageRefsMu.Lock()
		if !sameImageRefSet(n.imageRefs, su.ImageRefs) {
			n.imageRefs = append([]string(nil), su.ImageRefs...)
			imageRefsChanged = true
		}
		n.imageRefsMu.Unlock()
		n.imageSeen.forget(su.ImageRefs)
	}
	if n.engine != nil {
		n.engine.ApplyUpdate(ctx, su.Instances, su.RemovedInstances)
		n.engine.UpdateObservations(su.Observations)
	}
	if n.bench != nil {
		n.bench.ApplyUpdate(ctx, su.Benchmarks, su.RemovedBenchmarks)
	}
	for _, mf := range su.ModelFiles {
		n.reconcileModelFile(ctx, mf)
	}
	return imageRefsChanged
}

// StatusSnapshot is consumed opportunistically by conn.reportStatus. Keeping
// it outside conn preserves the lightweight connection package and lets older
// test handlers continue to implement the original interface unchanged.
func (n *NodeRuntime) StatusSnapshot(ctx context.Context, refs []string) ([]proto.ImagePresence, []proto.PathUsage) {
	var images []proto.ImagePresence
	if len(refs) == 0 {
		n.imageRefsMu.RLock()
		refs = append([]string(nil), n.imageRefs...)
		n.imageRefsMu.RUnlock()
	}
	if d := n.runtimeDriver(); d != nil {
		for _, ref := range refs {
			var present bool
			var imageID string
			var size *int64
			var err error
			if reporter, ok := d.(runtime.ImagePresenceReporter); ok {
				present, imageID, size, err = reporter.ImagePresence(ctx, ref)
			} else {
				present, err = d.ImageExists(ctx, ref)
			}
			if err != nil {
				n.log.Warn("image presence inspection failed", "image", ref, "runtime", d.Name(), "err", err)
				continue
			}
			n.imageSeen.observe(ctx, n.log, d, ref, present)
			images = append(images, proto.ImagePresence{ImageRef: ref, Present: present, ImageID: imageID, RuntimeSizeBytes: size})
		}
	}
	return images, n.pathUsage()
}

// pathUsage measures the paths the node depends on: the models root, the
// container data roots (on a Mac the environment packages instead) and the
// storages.
func (n *NodeRuntime) pathUsage() []proto.PathUsage {
	models := n.ModelsRoot()
	if models == "" {
		models = "/models"
	}
	paths := []proto.PathUsage{statAgentOwnedPath("models", models)}
	if goruntime.GOOS == "darwin" {
		// A Mac runs engines from environment packages; it has neither
		// runveil's root nor a Docker data root.
		paths = append(paths, statAgentOwnedPath(hostproc.DriverName, hostproc.EnvsDir(n.stateDir)))
	} else {
		paths = append(paths, statAgentOwnedPath("udocker", filepath.Join(n.stateDir, "udocker")))
		paths = append(paths, statManagedPath("docker", "/var/lib/docker"))
	}
	return append(paths, n.storagePathUsage()...)
}

// OnRPC handles short-action RPCs.
func (n *NodeRuntime) OnRPC(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case proto.RPCDebugStart:
		return n.rpcDebugStart(params)
	case proto.RPCDebugStop:
		return n.rpcDebugStop(params)
	case proto.RPCNodeDoctor:
		if !n.doctorRunning.CompareAndSwap(false, true) {
			return nil, errors.New("a diagnosis is already running on this node; wait for it to finish")
		}
		defer n.doctorRunning.Store(false)
		ctx, cancel := context.WithTimeout(ctx, doctorBudget)
		defer cancel()
		ctx = withDoctorCardsInUse(ctx, n.cardsInUse)
		ctx = withDoctorForeignCards(ctx, n.occupancy.foreign)
		ctx = withDoctorRuntimeCheck(ctx, n.cardRuntimeCheck)
		ctx = withDoctorAttempts(ctx, n.cardSmoke.Begin)
		ctx = withDoctorOutcomes(ctx, n.cardSmoke.Record)
		ctx = withDoctorHeldFindings(ctx, n.applyHeldHardwareFindings)
		return buildDoctorReportForNode(ctx, n.prober, n.doctorPathUsage(), n.stateDir, n.runtimeDriver()), nil
	case proto.RPCNodeCardCheckDetail:
		var req proto.CardCheckDetailRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, fmt.Errorf("decode card check request: %w", err)
		}
		if _, ok := accelerator.CardCheckFor(req.Check); !ok || req.Ident == "" {
			return nil, fmt.Errorf("unknown card check %q on card %q", req.Check, req.Ident)
		}
		return n.checkDetail(req.Ident, req.Check), nil
	case proto.RPCNodeCardRecheck:
		var req proto.CardRecheckRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, fmt.Errorf("decode card recheck request: %w", err)
		}
		ctx, cancel := context.WithTimeout(ctx, doctorBudget)
		defer cancel()
		return n.recheckCard(ctx, req.Ident)
	case proto.RPCNodeSetAccelerator:
		return n.handleSetAcceleratorRPC(params)
	case proto.RPCNodeSetRuntime:
		var req struct {
			Runtime string `json:"runtime"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, fmt.Errorf("decode runtime request: %w", err)
		}
		if err := n.ApplyRuntimePreference(ctx, req.Runtime); err != nil {
			return nil, err
		}
		return map[string]string{"runtime": n.RuntimeDriverName()}, nil
	case proto.RPCPartitionReprobe:
		if n.partitionProbe == nil {
			return []proto.PartitionProbeResult{}, nil
		}
		gpus, err := n.prober.Probe(ctx)
		if err != nil {
			return nil, err
		}
		reports := n.PartitionAssets(ctx)
		// Remember the explicitly refreshed snapshot so the next periodic SysInfo
		// report does not immediately execute the same probe a second time.
		_ = n.partitionProbeNeedsRefresh(reports, gpus)
		return n.partitionProbe.Refresh(ctx, reports, gpus), nil
	case proto.RPCKubeNodeExists, proto.RPCKubeNodesSelected:
		return n.kubeRPC(ctx, method, params)
	case proto.RPCFSList:
		return n.rpcFSList(params)
	case proto.RPCFSReadFile:
		return n.rpcFSReadFile(params)
	case proto.RPCFSStat:
		return n.rpcFSStat(params)
	case proto.RPCRegisterPath:
		return n.rpcRegisterPath(ctx, params)
	case proto.RPCSourceProbe:
		return n.rpcSourceProbe(ctx, params)
	case proto.RPCModelDownloadStop:
		return n.stopModelTransfer(ctx, params, false)
	case proto.RPCModelCopyStop:
		return n.stopModelCopy(ctx, params)
	case proto.RPCModelEvict:
		return n.rpcModelEvict(ctx, params)
	case proto.RPCCacheEvict:
		return n.rpcCacheEvict(ctx, params)
	case proto.RPCStorageProbe:
		return n.rpcStorageProbe(ctx, params)
	case proto.RPCStorageScan:
		return n.rpcStorageScan(ctx, params)
	case proto.RPCStoragePeers:
		return n.rpcStoragePeers(params)
	case proto.RPCStorageRemove:
		return n.rpcStorageRemove(params)
	case proto.RPCStorageFilesList:
		return n.rpcStorageFilesList(ctx, params)
	case proto.RPCStorageFilesMkdir:
		return n.rpcStorageFilesMkdir(ctx, params)
	case proto.RPCStorageFilesRename:
		return n.rpcStorageFilesRename(ctx, params)
	case proto.RPCStorageFilesDelete:
		return n.rpcStorageFilesDelete(ctx, params)
	case proto.RPCNetProbe:
		return n.rpcNetProbe(ctx, params)
	case proto.RPCNetBenchStart:
		return n.rpcNetBenchStart(params)
	case proto.RPCNetBenchStop:
		return n.rpcNetBenchStop(params)
	case proto.RPCRDMABenchServe:
		return n.rpcRDMABenchServe(params)
	case proto.RPCRDMABenchRun:
		return n.rpcRDMABenchRun(ctx, params)
	case proto.RPCAcceleratorPing:
		return n.rpcAcceleratorPing(ctx, params)
	case proto.RPCBenchProbe:
		var p struct {
			URL    string `json:"url"`
			APIKey string `json:"api_key,omitempty"`
		}
		if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.URL) == "" {
			return nil, errors.New("bench probe requires url")
		}
		baseURL, _ := loadgen.NormalizeBaseURL(p.URL)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
		if err != nil {
			return nil, err
		}
		if p.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+p.APIKey)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return nil, errors.New("models probe returned " + resp.Status)
		}
		var body any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, err
		}
		return body, nil
	case proto.RPCBenchCancel:
		var p proto.BenchmarkCancelParams
		if err := json.Unmarshal(params, &p); err != nil || p.ID == 0 {
			return nil, errors.New("bench cancel requires id")
		}
		if n.bench == nil {
			return nil, errors.New("benchmark engine unavailable")
		}
		_, err := n.bench.Cancel(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		return proto.BenchmarkCancelResult{ID: p.ID, State: "canceled", Running: false}, nil
	case proto.RPCPeerRelayWaitClosed:
		return n.rpcPeerRelayWaitClosed(ctx, params)
	case proto.RPCModelSyncListen:
		return n.rpcModelSyncListen(params)
	case proto.RPCModelSyncSend:
		return n.rpcModelSyncSend(ctx, params)
	case proto.RPCModelSyncStop:
		return n.rpcModelSyncStop(params)
	case proto.RPCModelSyncProbe:
		return n.rpcModelSyncProbe(ctx, params)
	case proto.RPCImageSyncListen:
		return n.rpcImageSyncListen(params)
	case proto.RPCImageSyncSend:
		return n.rpcImageSyncSend(ctx, params)
	case proto.RPCImageSyncProbe:
		return n.rpcImageSyncProbe(ctx, params)
	case proto.RPCImageSyncStop:
		return n.rpcImageSyncStop(ctx, params)
	case proto.RPCImagePull:
		return n.rpcImagePull(ctx, params)
	case proto.RPCRuntimeRestart:
		return n.rpcRuntimeRestart(ctx)
	case proto.RPCImageLoadURL:
		return n.rpcImageLoadURL(ctx, params)
	case proto.RPCImageLoadPath:
		return n.rpcImageLoadPath(ctx, params)
	case proto.RPCImageCancel:
		return n.rpcImageCancel(ctx, params)
	case proto.RPCImageRemove:
		return n.rpcImageRemove(ctx, params)
	case proto.RPCImageInspect:
		return n.rpcImageInspect(ctx, params)
	case proto.RPCImageExists:
		return n.rpcImageExists(ctx, params)
	default:
		return nil, errUnknownRPC(method)
	}
}

func (n *NodeRuntime) rpcImageInspect(ctx context.Context, raw json.RawMessage) (any, error) {
	var p proto.ImageInspectParams
	if err := json.Unmarshal(raw, &p); err != nil || strings.TrimSpace(p.ImageRef) == "" {
		return nil, errors.New("image inspect requires image_ref")
	}
	d := n.runtimeDriver()
	inspector, ok := d.(runtime.ImageInspector)
	if !ok {
		return nil, errors.New("runtime does not support image inspection")
	}
	inspected, err := inspector.InspectImage(ctx, strings.TrimSpace(p.ImageRef))
	if err != nil {
		return nil, err
	}
	return proto.ImageInspectResult{ImageRef: strings.TrimSpace(p.ImageRef), ImageID: inspected.ID, Entrypoint: inspected.Entrypoint, Cmd: inspected.Cmd, Env: commandlog.RedactEnv(inspected.Env), Volumes: inspected.Volumes}, nil
}

// StreamLogs streams an instance's container logs into w (logtail data plane).

// refuseModelFiles fails the model downloads a simulated node refuses
// (hostsim.Host.ConfineState); a real node refuses none.
func (n *NodeRuntime) refuseModelFiles(tasks []proto.ModelFileTask) {
	for _, mf := range tasks {
		n.failModelFile(mf, hostsim.ErrRefused)
	}
}
