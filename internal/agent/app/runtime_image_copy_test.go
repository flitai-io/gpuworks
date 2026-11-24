package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
)

type runtimeCopyFixture struct {
	*recording.Driver
	targetID  string
	installed runtime.ImageInspection
	started   chan struct{}
	release   chan struct{}
}

func (f *runtimeCopyFixture) ListLocalImages(_ context.Context, ns string) (proto.RuntimeImageList, error) {
	return proto.RuntimeImageList{Namespace: ns, TargetNamespace: "k8s.io", Items: []proto.RuntimeImage{{Ref: "runner:v1", Identity: f.installed.ID}}}, nil
}
func (f *runtimeCopyFixture) NeedsLocalImageCopy(_ context.Context, ref, id, stateDir string) (bool, error) {
	if f.targetID != "" && f.targetID != id {
		return false, runtime.ErrLocalImageConflict
	}
	return f.targetID == "", nil
}
func (f *runtimeCopyFixture) ResolveLocalImage(ctx context.Context, p proto.RuntimeImageSelection, job *runtime.LocalImageCopy) (runtime.ImageInspection, error) {
	if p.Identity != f.installed.ID {
		return runtime.ImageInspection{}, runtime.ErrLocalImageChanged
	}
	if job != nil && f.started != nil {
		job.Bytes.Add(1024)
		close(f.started)
		select {
		case <-ctx.Done():
			return runtime.ImageInspection{}, ctx.Err()
		case <-f.release:
		}
	}
	return f.installed, nil
}

func TestRuntimeImageRPCSelection(t *testing.T) {
	id := "sha256:" + strings.Repeat("a", 64)
	f := &runtimeCopyFixture{Driver: recording.New(""), installed: runtime.ImageInspection{ID: id, Architecture: "amd64", Labels: map[string]string{"io.gpuworks.engine": "vllm"}}}
	n, _ := localImportNode(t, f)
	raw, _ := json.Marshal(proto.RuntimeImageSelection{Namespace: "default", TargetNamespace: "k8s.io", Ref: "runner:v1", Identity: id})
	v, err := n.OnRPC(t.Context(), proto.RPCImageRuntimeList, raw)
	if err != nil {
		t.Fatal(err)
	}
	list := v.(proto.RuntimeImageList)
	if list.Namespace != "default" || len(list.Items) != 1 || list.Items[0].Identity != id {
		t.Fatalf("list: %+v", list)
	}
	for _, method := range []string{proto.RPCImageRuntimeInspect, proto.RPCImageRuntimeImport, proto.RPCImageRuntimePrepare} {
		params := raw
		if method == proto.RPCImageRuntimeImport {
			if _, err := n.OnRPC(t.Context(), method, raw); err == nil {
				t.Fatal("untracked cross-namespace import was accepted")
			}
			params, _ = json.Marshal(proto.RuntimeImageSelection{Runtime: "nerdctl", Namespace: "k8s.io", TargetNamespace: "k8s.io", Ref: "runner:v1", Identity: id})
		}
		v, err = n.OnRPC(t.Context(), method, params)
		if err != nil {
			t.Fatal(err)
		}
		got := v.(proto.RuntimeImageDetails)
		if got.ImageID != id || got.Metadata.Engine != "vllm" || got.CopyRequired != (method == proto.RPCImageRuntimePrepare) {
			t.Fatalf("%s: %+v", method, got)
		}
	}
	f.targetID = id
	v, err = n.OnRPC(t.Context(), proto.RPCImageRuntimePrepare, raw)
	if err != nil || v.(proto.RuntimeImageDetails).CopyRequired {
		t.Fatal("identical destination should be reused", v, err)
	}
	f.targetID = "different"
	_, err = n.OnRPC(t.Context(), proto.RPCImageRuntimePrepare, raw)
	var rpc *proto.RPCError
	if !errors.As(err, &rpc) || rpc.Code != proto.ErrorLocalImageConflict {
		t.Fatal("destination conflict", err)
	}
}

func TestRuntimeImageCopyLifecycle(t *testing.T) {
	for _, finish := range []string{"success", "cancel"} {
		t.Run(finish, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				id := "sha256:" + strings.Repeat("b", 64)
				f := &runtimeCopyFixture{Driver: recording.New(""), installed: runtime.ImageInspection{ID: id}, started: make(chan struct{}), release: make(chan struct{})}
				n, statuses := localImportNode(t, f)
				p := proto.RuntimeImageCopyRequest{TransferID: "unique", RuntimeImageSelection: proto.RuntimeImageSelection{Namespace: "default", TargetNamespace: "k8s.io", Ref: "runner:v1", Identity: id}}
				raw, _ := json.Marshal(p)
				request, disconnect := context.WithCancel(t.Context())
				if _, err := n.OnRPC(request, proto.RPCImageRuntimeCopy, raw); err != nil {
					t.Fatal(err)
				}
				disconnect()
				<-f.started
				time.Sleep(31 * time.Second)
				synctest.Wait()
				var last proto.ImageNodeStatus
				for len(statuses) > 0 {
					last = <-statuses
				}
				if last.State != "syncing" || last.TransferID != p.TransferID || last.TransferredBytes != 1024 {
					t.Fatalf("progress after browser disconnect: %+v", last)
				}
				if finish == "success" {
					close(f.release)
				} else {
					raw, _ := json.Marshal(proto.ImageCancelParams{TransferID: p.TransferID, CanonicalRef: p.Ref})
					if _, err := n.OnRPC(t.Context(), proto.RPCImageCancel, raw); err != nil {
						t.Fatal(err)
					}
				}
				synctest.Wait()
				if len(statuses) != 1 {
					t.Fatalf("expected one terminal status, got %d", len(statuses))
				}
				last = <-statuses
				if last.TransferID != p.TransferID || last.Source != proto.RuntimeImageCopySource || (finish == "success" && (last.State != "present" || last.ImageID != id)) || (finish == "cancel" && (last.State != "failed" || last.ImageID != "")) {
					t.Fatalf("terminal: %+v", last)
				}
				if n.imageOps.cancel(p.Ref, p.TransferID) || !n.imagePullMu.TryLock() {
					t.Fatal("finished transfer still active")
				}
				n.imagePullMu.Unlock()
			})
		})
	}
}
