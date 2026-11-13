package hostsim

import "gpuworks.ai/gpuworks/internal/proto"

type rule int

const (
	refuse       rule = iota // the zero value: anything unclassified is refused
	allow                    // acts only on the node's simulated runtime or its own records
	allowInDir               // names a path, which must lie in the node's directory
	allowEngines             // a tunnel, to the node's own engines only
)

// rpcRules classifies every RPC (TestEveryRPCIsClassified). What showing the
// product needs is allowed: registering a model already on the node, dGPU
// probing, image bookkeeping in the simulated runtime. Diagnosis, probes and
// network tests, storage, model and image transfer, runtime changes and
// upgrade act on the real host and are refused.
var rpcRules = map[string]rule{
	// This reads only the existing connection, without probing the real host.
	proto.RPCNodeConnectionDiagnostics: allow,
	proto.RPCFSList:                    allowInDir,
	proto.RPCFSReadFile:                allowInDir,
	proto.RPCFSStat:                    allowInDir,
	proto.RPCRegisterPath:              allowInDir,
	proto.RPCPartitionReprobe:          allow,
	proto.RPCModelCopyStop:             allow,
	proto.RPCModelDownloadStop:         allow, // stops a task by key/attempt; no host filesystem or network access
	// Eviction stays under the storage roots, which on a simulated node are
	// its models directory alone (ConfineState drops the rest).
	proto.RPCModelEvict:      allow,
	proto.RPCCacheEvict:      allow,
	proto.RPCAcceleratorPing: allow,
	// Answered from the agent's memory; a simulated node has no runtime
	// check, so a recheck says the host cannot be asked.
	proto.RPCNodeCardCheckDetail: allow,
	proto.RPCNodeCardRecheck:     allow,
	proto.RPCBenchCancel:         allow,
	proto.RPCImagePull:           allow, // answered without a registry (PullsImages)
	proto.RPCImageCancel:         allow,
	proto.RPCImageRemove:         allow,
	proto.RPCImageInspect:        allow,
	proto.RPCImageExists:         allow,
	// A debug session writes the agent's own log records under its state
	// directory, nothing of the host.
	proto.RPCDebugStart: allow,
	proto.RPCDebugStop:  allow,

	proto.RPCNodeDoctor:          refuse,
	proto.RPCNodeSetRuntime:      refuse,
	proto.RPCNodeSetAccelerator:  refuse,
	proto.RPCNetProbe:            refuse,
	proto.RPCNetBenchStart:       refuse,
	proto.RPCNetBenchStop:        refuse,
	proto.RPCRDMABenchServe:      refuse,
	proto.RPCRDMABenchRun:        refuse,
	proto.RPCPeerRelayWaitClosed: refuse,
	proto.RPCModelSyncListen:     refuse,
	proto.RPCModelSyncSend:       refuse,
	proto.RPCModelSyncStop:       refuse,
	proto.RPCModelSyncProbe:      refuse,
	proto.RPCImageSyncListen:     refuse,
	proto.RPCImageSyncSend:       refuse,
	proto.RPCImageSyncStop:       refuse,
	proto.RPCImageSyncProbe:      refuse,
	proto.RPCImageLoadURL:        refuse,
	proto.RPCImageLoadPath:       refuse,
	proto.RPCRuntimeRestart:      refuse,
	proto.RPCBenchProbe:          refuse,
	proto.RPCAgentUpgrade:        refuse,
	proto.RPCStorageFilesList:    refuse,
	proto.RPCStorageFilesMkdir:   refuse,
	proto.RPCStorageFilesRename:  refuse,
	proto.RPCStorageFilesDelete:  refuse,
	proto.RPCStorageProbe:        refuse,
	proto.RPCStorageScan:         refuse,
	proto.RPCStoragePeers:        refuse,
	proto.RPCStorageRemove:       refuse,
	proto.RPCSourceProbe:         refuse,
	// A simulated node is never a Kubernetes node: these reach the
	// cluster's API server.
	proto.RPCKubeNodeExists:    refuse,
	proto.RPCKubeNodesSelected: refuse,
}

// streamRules classifies every stream kind (TestEveryStreamIsClassified): a
// simulated node serves its engines, their logs and its own files; a shell,
// other ports, images, model transfer, storage and its binary are refused.
var streamRules = map[string]rule{
	proto.StreamCtrl:          allow,
	proto.StreamMetrics:       allow,
	proto.StreamLogtail:       allow,
	proto.StreamTunnel:        allowEngines,
	proto.StreamLargeFileRead: allowInDir,
	// The agent's own log and debug session files, nothing of the host.
	proto.StreamDiagnosticsRead: allow,

	proto.StreamPeerRelaySend:    refuse,
	proto.StreamPeerRelayReceive: refuse,
	proto.StreamModelRead:        refuse,
	proto.StreamModelWrite:       refuse,
	proto.StreamImageRead:        refuse,
	proto.StreamImageWrite:       refuse,
	proto.StreamTerminal:         refuse,
	proto.StreamAgentUpgrade:     refuse,
	proto.StreamStorageUpload:    refuse,
	proto.StreamStorageDownload:  refuse,
}

// refusedCaps are the capabilities of what a simulated node refuses.
var refusedCaps = map[string]bool{
	proto.CapPeerRelayV1:             true,
	proto.CapWebterm:                 true,
	proto.CapWebtermContainer:        true,
	proto.CapWebtermContainerUdocker: true,
	proto.CapUpgradePush:             true,
	proto.CapKubeBinaryUpgrade:       true,
	proto.CapStorageFiles:            true,
	proto.CapBench:                   true,
	proto.CapBenchWarmup:             true,
	proto.CapBenchArrival:            true,
	proto.CapBenchInteractivity:      true,
	proto.CapBenchEvidence:           true,
	proto.CapBenchRequestTimeout:     true,
}
