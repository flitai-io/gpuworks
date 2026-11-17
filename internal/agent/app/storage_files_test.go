package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/storagemount"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestStorageFilesRootFollowsTheStorageState(t *testing.T) {
	if _, err := (&NodeRuntime{}).storageFilesRoot(1); err == nil {
		t.Fatal("an agent without storages resolved a root")
	}
	r := newStorageTestRuntime(t)
	local := t.TempDir()
	shared := t.TempDir()
	cache := filepath.Join(t.TempDir(), "cache")
	r.fs.mountNFS(t, shared)
	r.applyStorages([]proto.StorageTask{
		{ID: 1, Name: "local", Type: proto.StorageLocal, Path: local},
		{ID: 3, Name: "nfs-cached", Type: proto.StorageNFSCache, Path: shared, Existing: true, CachePath: cache},
	})
	if root, err := r.storageFilesRoot(1); err != nil || root.Ready {
		t.Fatalf("before the first check: %+v %v", root, err)
	}
	r.checkStorages(context.Background(), true)
	root, err := r.storageFilesRoot(1)
	if err != nil || root.Dir != local || !root.Ready || !root.Writable {
		t.Fatalf("local root = %+v, %v", root, err)
	}
	// nfs_cache browses the authoritative NFS root, never the node cache.
	root, err = r.storageFilesRoot(3)
	if err != nil || root.Dir != shared || !root.Ready {
		t.Fatalf("nfs_cache root = %+v, %v", root, err)
	}
	if _, err := r.storageFilesRoot(9); err == nil {
		t.Fatal("an unknown storage resolved")
	}
	// A storage that is ready but read-only here.
	r.storage.mu.Lock()
	st := r.storage.statuses[1]
	st.Writable = false
	r.storage.statuses[1] = st
	r.storage.mu.Unlock()
	if root, _ := r.storageFilesRoot(1); !root.Ready || root.Writable {
		t.Fatalf("read-only root = %+v", root)
	}
	_, err = r.OnRPC(context.Background(), proto.RPCStorageFilesMkdir, json.RawMessage(`{"storage_id":1,"path":"files/x"}`))
	if storageFilesCode(err) != proto.StorageFilesErrNotWritable {
		t.Fatalf("mkdir on a read-only storage: %v", err)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestStorageFilesRPCsAndStreamsThroughTheNodeRuntime(t *testing.T) {
	r := newStorageTestRuntime(t)
	local := t.TempDir()
	r.applyStorages([]proto.StorageTask{{ID: 1, Name: "local", Type: proto.StorageLocal, Path: local}})
	r.checkStorages(context.Background(), true)
	ctx := context.Background()
	call := func(method, params string) (string, error) {
		res, err := r.OnRPC(ctx, method, json.RawMessage(params))
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(res)
		return string(b), nil
	}
	if got, err := call(proto.RPCStorageFilesMkdir, `{"storage_id":1,"path":"files/d"}`); err != nil || got != `{"path":"files/d"}` {
		t.Fatalf("mkdir = %s, %v", got, err)
	}
	// Upload through the stream entry point.
	conn := &rwBuffer{in: strings.NewReader("hello")}
	hdr := proto.StreamHeader{Kind: proto.StreamStorageUpload, StorageID: 1, Rel: "files/d/a.txt",
		Staging: proto.StorageUploadStagingRel("cp", "u1"), Size: 5}
	if err := r.ReceiveStorageUpload(ctx, hdr, conn); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(conn.out.String(), `"state":"done"`) {
		t.Fatalf("upload answer = %q", conn.out.String())
	}
	got, err := call(proto.RPCStorageFilesList, `{"storage_id":1,"path":"files/d"}`)
	if err != nil || !strings.Contains(got, `"name":"a.txt","dir":false,"size":5`) {
		t.Fatalf("list = %s, %v", got, err)
	}
	if got, err := call(proto.RPCStorageFilesRename, `{"storage_id":1,"from":"files/d/a.txt","to":"files/b.txt"}`); err != nil || got != `{"path":"files/b.txt"}` {
		t.Fatalf("rename = %s, %v", got, err)
	}
	var out bytes.Buffer
	if err := r.SendStorageDownload(ctx, proto.StreamHeader{Kind: proto.StreamStorageDownload, StorageID: 1, Rel: "files/b.txt"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != `{"dir":false,"size":5,"name":"b.txt"}`+"\nhello" {
		t.Fatalf("download = %q", out.String())
	}
	if got, err := call(proto.RPCStorageFilesDelete, `{"storage_id":1,"path":"files/b.txt"}`); err != nil || got != `{"path":"files/b.txt"}` {
		t.Fatalf("delete = %s, %v", got, err)
	}
	// Refusals cross the RPC boundary with their code.
	_, err = call(proto.RPCStorageFilesDelete, `{"storage_id":1,"path":"files/b.txt"}`)
	if storageFilesCode(err) != proto.StorageFilesErrNotFound {
		t.Fatalf("second delete: %v", err)
	}
	_, err = call(proto.RPCStorageFilesMkdir, `{"storage_id":1,"path":"models/x"}`)
	if storageFilesCode(err) != proto.StorageFilesErrRegionReadonly {
		t.Fatalf("mkdir outside the regions: %v", err)
	}
	for _, method := range []string{proto.RPCStorageFilesList, proto.RPCStorageFilesMkdir, proto.RPCStorageFilesRename, proto.RPCStorageFilesDelete} {
		_, err := call(method, `{"storage_id":"one"}`)
		if err == nil || !strings.HasPrefix(err.Error(), "storage_files:failed: decode request") {
			t.Fatalf("%s with a bad request: %v", method, err)
		}
	}
	// The guard is the storage's: a stuck list in every slot refuses the next.
	release := make(chan struct{})
	for i := 0; i < storageFilesSlots; i++ {
		_ = r.storage.manager.DoAny(1, "files.list", storageFilesSlots, time.Millisecond, func() error { <-release; return nil })
	}
	_, err = call(proto.RPCStorageFilesList, `{"storage_id":1,"path":"files"}`)
	close(release)
	if !strings.Contains(errString(err), "did not return in time") {
		t.Fatalf("list with every slot stuck: %v", err)
	}
}

// rwBuffer is an upload stream: the body to read and the answer written.
type rwBuffer struct {
	in  io.Reader
	out bytes.Buffer
}

func (b *rwBuffer) Read(p []byte) (int, error)  { return b.in.Read(p) }
func (b *rwBuffer) Write(p []byte) (int, error) { return b.out.Write(p) }

func TestIncomingStagingIsSweptAtStartAndWithTheTrash(t *testing.T) {
	r := newStorageTestRuntime(t)
	local := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	stale := func(name string) string {
		p := filepath.Join(local, ".gpuworks", "incoming", "cp", name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		return p
	}
	first := stale("upload-1")
	r.applyStorages([]proto.StorageTask{{ID: 1, Name: "local", Type: proto.StorageLocal, Path: local}})
	r.checkStorages(context.Background(), true)
	waitGone(t, first)
	// Once per process: a later check does not sweep again ...
	second := stale("upload-2")
	r.checkStorages(context.Background(), true)
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Lstat(second); err != nil {
		t.Fatalf("a second check swept again: %v", err)
	}
	// ... the heartbeat's trash sweep does.
	r.writeHeartbeat(1, local, proto.StorageHeartbeat{ControlPlaneID: "cp", IntervalSeconds: 30})
	if _, err := os.Lstat(second); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the heartbeat sweep kept stale staging: %v", err)
	}
	// A storage the node cannot write is not swept.
	ro := newStorageTestRuntime(t)
	ro.applyStorages([]proto.StorageTask{{ID: 1, Name: "local", Type: proto.StorageLocal, Path: local}})
	ro.storage.mu.Lock() // as a check would leave it, without the check's own sweep
	ro.storage.statuses[1] = storagemount.Status{ID: 1, State: storagemount.StateReady, Writable: false}
	ro.storage.mu.Unlock()
	third := stale("upload-3")
	ro.sweepIncomingOnce()
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Lstat(third); err != nil {
		t.Fatalf("a read-only storage was swept: %v", err)
	}
}

func waitGone(t *testing.T, p string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was never swept", p)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// storageFilesCode is the storage-files code an RPC refusal carries.
func storageFilesCode(err error) string {
	var refusal *proto.StorageFilesError
	if !errors.As(err, &refusal) {
		return ""
	}
	return refusal.Code
}
