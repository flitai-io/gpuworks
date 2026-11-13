package udocker

import (
	"fmt"
	"os"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// vastA100Report is an abridged `nvidia-smi -q -x` from a container whose
// /dev nodes are numbered from the host driver: 4 GPUs, no /dev/nvidia0.
const vastA100Report = `<?xml version="1.0" ?>
<!DOCTYPE nvidia_smi_log SYSTEM "nvsmi_device_v13.dtd">
<nvidia_smi_log>
	<timestamp>Fri Sep 11 19:24:11 2026</timestamp>
	<gpu id="00000000:54:00.0">
		<product_name>NVIDIA A100-SXM4-40GB</product_name>
		<uuid>GPU-37cc94d5-e63d-40ee-aa50-fa9c6a909872</uuid>
		<minor_number>1</minor_number>
	</gpu>
	<gpu id="00000000:8D:00.0">
		<product_name>NVIDIA A100-SXM4-40GB</product_name>
		<uuid>GPU-13c8a698-5e4c-9096-6a6a-010d76815bb6</uuid>
		<minor_number>6</minor_number>
	</gpu>
	<gpu id="00000000:92:00.0">
		<product_name>NVIDIA A100-SXM4-40GB</product_name>
		<uuid>GPU-365d69aa-a2be-240c-772a-aba59515c765</uuid>
		<minor_number>7</minor_number>
	</gpu>
	<gpu id="00000000:DA:00.0">
		<product_name>NVIDIA A100-SXM4-40GB</product_name>
		<uuid>GPU-441962b4-b92e-6c02-512e-79cf8c633321</uuid>
		<minor_number>5</minor_number>
	</gpu>
</nvidia_smi_log>
`

// hostReport is the ordinary case: minor number equals CUDA index.
const hostReport = `<?xml version="1.0" ?>
<nvidia_smi_log>
	<gpu id="00000000:01:00.0"><minor_number>0</minor_number></gpu>
	<gpu id="00000000:02:00.0"><minor_number>1</minor_number></gpu>
</nvidia_smi_log>
`

func TestParseNVIDIAMinorNumbersDocumentOrder(t *testing.T) {
	cases := []struct {
		name string
		xml  string
		want []int
	}{
		{"injected container: minors differ from CUDA index", vastA100Report, []int{1, 6, 7, 5}},
		{"ordinary host: minor equals index", hostReport, []int{0, 1}},
		{"no gpus", `<nvidia_smi_log></nvidia_smi_log>`, nil},
		{"malformed xml", `<nvidia_smi_log><gpu>`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseNVIDIAMinorNumbers([]byte(tc.xml))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("parseNVIDIAMinorNumbers() = %v, want %v", got, tc.want)
			}
		})
	}
}

// An unparsable minor must discard the whole mapping: a partial list would
// silently shift every later index onto the wrong device node.
func TestParseNVIDIAMinorNumbersRejectsPartialMapping(t *testing.T) {
	const partial = `<nvidia_smi_log>
	<gpu><minor_number>1</minor_number></gpu>
	<gpu><minor_number>N/A</minor_number></gpu>
</nvidia_smi_log>`
	if got := parseNVIDIAMinorNumbers([]byte(partial)); got != nil {
		t.Fatalf("want nil for an unparsable minor, got %v", got)
	}
}

// resetNVIDIANodeCache points the cache at query and restores it afterwards.
func resetNVIDIANodeCache(t *testing.T, query func() ([]byte, error)) {
	t.Helper()
	origQuery := runNVIDIASMIQueryXML
	reset := func() {
		nvidiaNodesMu.Lock()
		nvidiaNodes, nvidiaNodesResolved = nil, false
		nvidiaNodesMu.Unlock()
	}
	reset()
	runNVIDIASMIQueryXML = query
	t.Cleanup(func() { runNVIDIASMIQueryXML = origQuery; reset() })
}

func TestCachedNVIDIADeviceNodeMapResolvesOnce(t *testing.T) {
	calls := 0
	resetNVIDIANodeCache(t, func() ([]byte, error) {
		calls++
		return []byte(vastA100Report), nil
	})
	want := map[int]string{0: "/dev/nvidia1", 1: "/dev/nvidia6", 2: "/dev/nvidia7", 3: "/dev/nvidia5"}
	for i := 0; i < 2; i++ {
		got := cachedNVIDIADeviceNodeMap()
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("index %d = %q, want %q (full: %v)", k, got[k], v, got)
			}
		}
	}
	if calls != 1 {
		t.Fatalf("nvidia-smi queries = %d, want 1", calls)
	}
}

// A failed query (nvidia-smi timing out on a busy host) is not cached: the
// next workload build asks again and gets the driver's real node names.
func TestCachedNVIDIADeviceNodeMapRetriesAfterQueryFailure(t *testing.T) {
	calls := 0
	resetNVIDIANodeCache(t, func() ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("nvidia-smi: no answer within 10s: %w", gpu.ErrCLIUnresponsive)
		}
		return []byte(vastA100Report), nil
	})
	if got := cachedNVIDIADeviceNodeMap(); got != nil {
		t.Fatalf("want nil on query failure, got %v", got)
	}
	if got := cachedNVIDIADeviceNodeMap(); got[0] != "/dev/nvidia1" || calls != 2 {
		t.Fatalf("retry = %v after %d queries, want the driver's map from a second query", got, calls)
	}
}

// A report without minor numbers is an answer, kept like any other.
func TestCachedNVIDIADeviceNodeMapKeepsAnEmptyAnswer(t *testing.T) {
	calls := 0
	resetNVIDIANodeCache(t, func() ([]byte, error) {
		calls++
		return []byte("<nvidia_smi_log></nvidia_smi_log>"), nil
	})
	first, second := cachedNVIDIADeviceNodeMap(), cachedNVIDIADeviceNodeMap()
	if first != nil || second != nil || calls != 1 {
		t.Fatalf("empty report queried %d times, want 1 and a nil map", calls)
	}
}

// stubNVIDIADevices installs a fixed index->node map and filesystem view, and
// restores both when the test ends.
func stubNVIDIADevices(t *testing.T, nodes map[int]string, existing ...string) {
	t.Helper()
	origMap, origStat := nvidiaDeviceNodeMap, osStat
	nvidiaDeviceNodeMap = func() map[int]string { return nodes }
	osStat = func(name string) (os.FileInfo, error) {
		if slices.Contains(existing, name) {
			return nil, nil
		}
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() { nvidiaDeviceNodeMap, osStat = origMap, origStat })
}

func mountSources(mounts []runtime.Mount) []string {
	srcs := make([]string, 0, len(mounts))
	for _, m := range mounts {
		srcs = append(srcs, m.Source)
	}
	return srcs
}

// The regression this file exists for: CUDA index 0 must bind the node the
// driver named for it, not the nonexistent /dev/nvidia0.
func TestAcceleratorMountsUsesDriverDeviceNode(t *testing.T) {
	stubNVIDIADevices(t,
		map[int]string{0: "/dev/nvidia1", 1: "/dev/nvidia6", 2: "/dev/nvidia7", 3: "/dev/nvidia5"},
		"/dev/nvidia1", "/dev/nvidia6", "/dev/nvidia7", "/dev/nvidia5",
		"/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools", "/dev/nvidia-modeset")

	mounts := acceleratorMounts(runtime.WorkloadSpec{
		Env: map[string]string{"CUDA_VISIBLE_DEVICES": "0"},
	})
	srcs := mountSources(mounts)
	if !slices.Contains(srcs, "/dev/nvidia1") {
		t.Fatalf("CUDA index 0 must bind /dev/nvidia1, got %v", srcs)
	}
	if slices.Contains(srcs, "/dev/nvidia0") {
		t.Fatalf("mounted a device node that does not exist: %v", srcs)
	}
}

// With no resolvable node and no index-named node, nothing is mounted: a
// missing bind degrades the container, a nonexistent bind source fails it.
func TestAcceleratorMountsSkipsMissingDeviceNode(t *testing.T) {
	stubNVIDIADevices(t, nil) // no driver report, and no /dev/nvidia* on disk

	mounts := acceleratorMounts(runtime.WorkloadSpec{
		Env: map[string]string{"CUDA_VISIBLE_DEVICES": "0"},
	})
	for _, src := range mountSources(mounts) {
		if src == "/dev/nvidia0" {
			t.Fatalf("mounted nonexistent /dev/nvidia0: %v", mountSources(mounts))
		}
	}
}

// On an ordinary host the historical spelling is already correct and must not
// regress when the driver report is unavailable.
func TestAcceleratorMountsFallsBackToIndexNode(t *testing.T) {
	stubNVIDIADevices(t, nil, "/dev/nvidia0", "/dev/nvidia1")

	mounts := acceleratorMounts(runtime.WorkloadSpec{
		Env: map[string]string{"CUDA_VISIBLE_DEVICES": "0,1"},
	})
	srcs := mountSources(mounts)
	for _, want := range []string{"/dev/nvidia0", "/dev/nvidia1"} {
		if !slices.Contains(srcs, want) {
			t.Fatalf("expected fallback to bind %s, got %v", want, srcs)
		}
	}
}

// The resolved node is only used when it exists; a stale driver report must not
// reintroduce a nonexistent bind source.
func TestAddNVIDIADeviceNodePrefersExistingResolvedNode(t *testing.T) {
	stubNVIDIADevices(t, map[int]string{0: "/dev/nvidia1"}, "/dev/nvidia0")

	var got []string
	addNVIDIADeviceNode(map[int]string{0: "/dev/nvidia1"}, "0", func(p string) { got = append(got, p) })
	if !slices.Equal(got, []string{"/dev/nvidia0"}) {
		t.Fatalf("resolved node missing on disk -> want fallback /dev/nvidia0, got %v", got)
	}
}

func TestAddNVIDIADeviceNodeNonNumericIndex(t *testing.T) {
	stubNVIDIADevices(t, map[int]string{0: "/dev/nvidia1"}, "/dev/nvidiaGPU-abc")

	var got []string
	addNVIDIADeviceNode(map[int]string{}, "GPU-abc", func(p string) { got = append(got, p) })
	if !slices.Equal(got, []string{"/dev/nvidiaGPU-abc"}) {
		t.Fatalf("want pass-through for a non-numeric selector, got %v", got)
	}
}
