package app

import (
	"context"
	"encoding/json"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

func (s runtimeImageRPC) startCopy(ctx context.Context, _ string, raw json.RawMessage) (any, error) {
	var p proto.RuntimeImageCopyRequest
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.TransferID == "" || p.Namespace == "" || p.Namespace == p.TargetNamespace || !runtime.ValidLocalImageRef(p.Ref) || !runtime.ValidImageDigest(p.Identity) {
		return nil, localImageError(proto.RPCImageRuntimeCopy, p.RuntimeImageSelection, runtime.ErrLocalImageChanged)
	}
	d, ok := s.driver.(runtime.LocalImageStore)
	if !ok {
		return nil, localImageError(proto.RPCImageRuntimeCopy, p.RuntimeImageSelection, runtime.ErrLocalImageUnsupported)
	}
	if !s.lock.TryLock() {
		return nil, &proto.RPCError{Code: proto.ErrorLocalImageBusy, Message: "another node image operation is in progress"}
	}
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), proto.ImageOperationTimeout)
	unregister := s.ops.begin(p.Ref, p.TransferID, cancel)
	go func() {
		defer s.lock.Unlock()
		defer unregister()
		defer cancel()
		job := &runtime.LocalImageCopy{StateDir: s.stateDir}
		done := make(chan struct{})
		heartbeatDone := make(chan struct{})
		go func() {
			defer close(heartbeatDone)
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				s.report(proto.ImageNodeStatus{ImageRef: p.Ref, TransferID: p.TransferID, Source: proto.RuntimeImageCopySource, State: "syncing", TransferredBytes: job.Bytes.Load()})
				select {
				case <-done:
					return
				case <-ticker.C:
				}
			}
		}()
		image, err := d.ResolveLocalImage(work, p.RuntimeImageSelection, job)
		close(done)
		<-heartbeatDone
		status := proto.ImageNodeStatus{ImageRef: p.Ref, TransferID: p.TransferID, Source: proto.RuntimeImageCopySource, State: "present", ImageID: image.ID, TransferredBytes: job.Bytes.Load()}
		if err != nil || image.ID == "" {
			if err == nil {
				err = runtime.ErrLocalImageChanged
			}
			status.ImageID, status.State, status.Message = "", "failed", localImageError(proto.RPCImageRuntimeCopy, p.RuntimeImageSelection, err).Code
		}
		s.report(status)
	}()
	return proto.ImageLoadPathResult{State: "syncing", CanonicalRef: p.Ref}, nil
}
