package gpu

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestVendorCensusBlockedAMDComputeReadPreservesOtherVendors(t *testing.T) {
	root := t.TempDir()
	writeBusCard(t, root, "0000:43:00.0", "0x1002", "0x1586", "0x030000", "amdgpu")
	writeBusCard(t, root, "0000:44:00.0", "0x10de", "0x2204", "0x030000", "nvidia")
	writeBusCard(t, root, "0000:45:00.0", "0x19e5", "0xd802", "0x120000", "devdrv_device_driver")
	writeAMDCensusCompute(t, root)
	started, release := make(chan struct{}), make(chan struct{})
	var startedOnce, releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var calls atomic.Int32
	census := &VendorCensus{HostRoot: root, amdReadFile: func(path string) ([]byte, error) {
		if strings.Contains(path, "/kfd/topology/") {
			calls.Add(1)
			startedOnce.Do(func() { close(started) })
			<-release
		}
		return os.ReadFile(path)
	}}
	done := make(chan []proto.AcceleratorPresence, 1)
	go func() { done <- census.Vendors() }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("KFD read did not start")
	}
	var vendors []proto.AcceleratorPresence
	select {
	case vendors = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("blocked KFD read froze the whole vendor census")
	}
	if len(vendors) != 2 || vendors[0].Vendor != "ascend" || vendors[1].Vendor != "nvidia" {
		t.Fatalf("unrelated vendor inventory lost: %+v", vendors)
	}
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		candidates := census.amdComputeCandidates(ctx)
		cancel()
		if len(candidates) != 0 {
			t.Fatalf("unverified AMD candidates: %v", candidates)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("spawned %d blocked KFD readers", calls.Load())
	}
	releaseOnce.Do(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	candidates := census.amdComputeCandidates(ctx)
	if !candidates["0000:43:00.0"] {
		t.Fatalf("AMD discovery did not recover after real read exited: %v", candidates)
	}
	vendors = census.Vendors()
	if len(vendors) != 3 || vendors[0].Vendor != "amd" || vendors[1].Vendor != "ascend" || vendors[2].Vendor != "nvidia" {
		t.Fatalf("recovered census: %+v", vendors)
	}
}

func TestVendorCensusWithoutAMDDoesNotReadKFD(t *testing.T) {
	root := t.TempDir()
	writeBusCard(t, root, "0000:44:00.0", "0x10de", "0x2204", "0x030000", "nvidia")
	writeAMDCensusCompute(t, root)
	var calls atomic.Int32
	census := &VendorCensus{HostRoot: root, amdReadFile: func(path string) ([]byte, error) { calls.Add(1); return os.ReadFile(path) }}
	vendors := census.Vendors()
	if len(vendors) != 1 || vendors[0].Vendor != "nvidia" || calls.Load() != 0 {
		t.Fatalf("non-AMD census performed KFD I/O: vendors=%v reads=%d", vendors, calls.Load())
	}
}

func TestVendorCensusAMDDefaultHostRootUsesAbsoluteSysfs(t *testing.T) {
	properties := "/sys/class/kfd/kfd/topology/nodes/1/properties"
	device := "/sys/bus/pci/devices/0000:43:00.0"
	reads := map[string]string{
		properties:         "vendor_id 4098\ndevice_id 5510\ndomain 0\nlocation_id 17152\ndrm_render_minor 135\ngfx_target_version 110501\n",
		device + "/device": "0x1586",
	}
	read := func(path string) ([]byte, error) {
		value, ok := reads[path]
		if !ok {
			t.Errorf("unexpected sysfs read %q", path)
			return nil, os.ErrNotExist
		}
		return []byte(value), nil
	}
	glob := func(pattern string) ([]string, error) {
		switch pattern {
		case "/sys/class/kfd/kfd/topology/nodes/*/properties":
			return []string{properties}, nil
		case "/sys/bus/pci/devices/*":
			return []string{device}, nil
		default:
			t.Errorf("default host root used non-host glob %q", pattern)
			return nil, os.ErrNotExist
		}
	}
	candidates := amdComputePCIFunctions(context.Background(), "", read, glob)
	if !candidates["0000:43:00.0"] {
		t.Fatalf("host compute card disappeared with empty HostRoot: %v", candidates)
	}
}
