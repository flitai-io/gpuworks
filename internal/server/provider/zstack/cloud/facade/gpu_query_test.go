package facade_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/facade"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
)

const nativePath = "/v1/gpu-device/gpu-devices"
const legacyPath = "/v1/pci-device/pci-devices"

func testClient(t *testing.T, handler http.HandlerFunc) *apiclient.APIClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("query attempted a write: %s", r.Method)
			http.Error(w, "unexpected write", 405)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := apiclient.New(provider.Credentials{Endpoint: server.URL, PublicKey: "test-ak", PrivateKey: "test-sk"}, apiclient.WithReadRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func missingRoute(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprint(w, "no api mapping to "+r.URL.Path)
}

func TestGPUProtocolsReturnEquivalentPhysicalDevices(t *testing.T) {
	want := inventory.GPUDevice{HostUUID: "host", GPU: inventory.CloudGPU{
		ExternalID: "gpu", Model: "NVIDIA Test", PCIAddress: "0000:17:00.0", State: "Enabled", Status: "Active", Allocation: "Unallocated",
	}}
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			var nativeCalls, pciCalls atomic.Int32
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case nativePath:
					nativeCalls.Add(1)
					if legacy {
						missingRoute(w, r)
						return
					}
					fmt.Fprint(w, `{"inventories":[{"uuid":"gpu","hostUuid":"host","gpuType":"NVIDIA Test","pciDeviceAddress":"0000:17:00.0","state":"Enabled","status":"Active","allocateStatus":"Unallocated"}]}`)
				case legacyPath:
					pciCalls.Add(1)
					fmt.Fprint(w, `{"inventories":[{"uuid":"gpu","hostUuid":"host","name":"NVIDIA Test","type":"GPU_3D_Controller","pciDeviceAddress":"0000:17:00.0","state":"Enabled","status":"Active","virtStatus":"UNVIRTUALIZABLE"}]}`)
				default:
					http.NotFound(w, r)
				}
			})
			query := facade.NewGPUQuery(client)
			list, err := query.List(t.Context())
			if err != nil || !reflect.DeepEqual(list, []inventory.GPUDevice{want}) {
				t.Fatalf("list=%+v err=%v", list, err)
			}
			got, err := query.Get(t.Context(), "gpu")
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("GPU=%+v err=%v", got, err)
			}
			if legacy && (nativeCalls.Load() != 1 || pciCalls.Load() != 2) {
				t.Fatal("legacy capability was not retained for this query object")
			}
			if !legacy && (nativeCalls.Load() != 2 || pciCalls.Load() != 0) {
				t.Fatal("current protocol added unnecessary requests")
			}
		})
	}
}

func TestLegacyAllocationDoesNotConfuseDisabledWithVMOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, state, status, virt, vm, parent, kind, want string
		wantError                                         bool
	}{
		{name: "free", state: "Enabled", status: "Active", virt: "UNVIRTUALIZABLE", kind: "GPU_3D_Controller", want: "Unallocated"},
		{name: "processing accelerator", state: "Enabled", status: "Active", virt: "UNVIRTUALIZABLE", kind: "GPU_Processing_Accelerators", want: "Unallocated"},
		{name: "coprocessor", state: "Enabled", status: "Active", virt: "UNVIRTUALIZABLE", kind: "GPU_Co_Processor", want: "Unallocated"},
		{name: "disabled by GPUWorks", state: "Disabled", status: "System", virt: "UNVIRTUALIZABLE", kind: "GPU_Video_Controller", want: "Unallocated"},
		{name: "VM attached", state: "Enabled", status: "Attached", virt: "UNVIRTUALIZABLE", kind: "GPU_3D_Controller", want: "Allocated"},
		{name: "VM reserved", state: "Enabled", status: "Reserved", virt: "UNVIRTUALIZABLE", kind: "GPU_3D_Controller", want: "Allocated"},
		{name: "VM reference", state: "Enabled", status: "Active", virt: "UNVIRTUALIZABLE", vm: "vm", kind: "GPU_3D_Controller", want: "Allocated"},
		{name: "MDEV eligible", state: "Enabled", status: "System", virt: "VFIO_MDEV_VIRTUALIZABLE", kind: "GPU_3D_Controller", want: "Unallocated"},
		{name: "SRIOV eligible", state: "Enabled", status: "System", virt: "SRIOV_VIRTUALIZABLE", kind: "GPU_3D_Controller", want: "Unallocated"},
		{name: "unknown status", state: "Enabled", status: "Future", virt: "UNVIRTUALIZABLE", kind: "GPU_3D_Controller"},
		{name: "unknown virtualization", state: "Enabled", status: "System", kind: "GPU_3D_Controller"},
		{name: "virtualized", state: "Enabled", status: "System", virt: "VFIO_MDEV_VIRTUALIZED", kind: "GPU_3D_Controller"},
		{name: "virtual child", state: "Enabled", status: "Active", virt: "UNVIRTUALIZABLE", parent: "parent", kind: "GPU_3D_Controller"},
		{name: "audio function", state: "Enabled", status: "System", virt: "UNVIRTUALIZABLE", kind: "GPU_Audio_Controller", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == nativePath {
					missingRoute(w, r)
					return
				}
				if r.URL.Path != legacyPath || r.URL.Query().Get("q") != "uuid=gpu" || len(r.URL.Query()) != 1 {
					t.Errorf("unexpected query %s", r.URL)
					http.NotFound(w, r)
					return
				}
				fmt.Fprintf(w, `{"inventories":[{"uuid":"gpu","hostUuid":"host","state":%q,"status":%q,"virtStatus":%q,"vmInstanceUuid":%q,"parentUuid":%q,"type":%q}]}`, tc.state, tc.status, tc.virt, tc.vm, tc.parent, tc.kind)
			})
			gpu, err := facade.NewGPUQuery(client).Get(t.Context(), "gpu")
			if (err != nil) != tc.wantError {
				t.Fatalf("GPU=%+v err=%v", gpu, err)
			}
			if !tc.wantError && (gpu.GPU.Allocation != tc.want || gpu.GPU.State != tc.state || gpu.GPU.VMUUID != tc.vm) {
				t.Fatalf("incorrect normalized state: %+v", gpu.GPU)
			}
		})
	}
}

func TestGPUQueryDoesNotFallbackOnFailuresOrInventMissingAllocation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantError bool
	}{
		{"proxy 404", 404, "404 page not found", true},
		{"forbidden", 403, "forbidden", true},
		{"gateway timeout", 504, "gateway timeout", true},
		{"malformed inventory", 200, `{}`, true},
		{"invalid JSON", 200, `broken`, true},
		{"empty inventory", 200, `{"inventories":[]}`, true},
		{"duplicate inventory", 200, `{"inventories":[{"uuid":"gpu"},{"uuid":"gpu"}]}`, true},
		{"missing allocation", 200, `{"inventories":[{"uuid":"gpu","hostUuid":"host","state":"Enabled"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pciCalls atomic.Int32
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == legacyPath {
					pciCalls.Add(1)
					fmt.Fprint(w, `{"inventories":[]}`)
					return
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			gpu, err := facade.NewGPUQuery(client).Get(t.Context(), "gpu")
			if (err != nil) != tc.wantError || pciCalls.Load() != 0 {
				t.Fatalf("gpu=%+v err=%v PCI calls=%d", gpu, err, pciCalls.Load())
			}
			if !tc.wantError && gpu.GPU.Allocation != "" {
				t.Fatal("missing allocation became available")
			}
		})
	}
}

func TestLegacyGPUsAreFilteredAfterCompletePagination(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == nativePath {
					missingRoute(w, r)
					return
				}
				start := r.URL.Query().Get("start")
				if r.URL.Path != legacyPath || r.URL.Query().Get("limit") != "100" || r.URL.Query().Get("sort") != "uuid" {
					t.Errorf("bad page %s", r.URL)
					http.NotFound(w, r)
					return
				}
				if start == "0" {
					items := make([]map[string]string, 100)
					for i := range items {
						items[i] = map[string]string{"uuid": strconv.Itoa(i), "type": "Ethernet_Controller"}
					}
					json.NewEncoder(w).Encode(map[string]any{"inventories": items})
					return
				}
				if start != "100" {
					t.Errorf("unexpected pagination %s", start)
				}
				if fail {
					http.Error(w, "page rejected", 400)
					return
				}
				fmt.Fprint(w, `{"inventories":[{"uuid":"gpu","hostUuid":"host","type":"GPU_3D_Controller","state":"Enabled","status":"Active","virtStatus":"UNVIRTUALIZABLE"}]}`)
			})
			gpus, err := facade.NewGPUQuery(client).List(t.Context())
			if fail {
				if err == nil || gpus != nil {
					t.Fatalf("partial result=%+v err=%v", gpus, err)
				}
				return
			}
			if err != nil || len(gpus) != 1 || gpus[0].GPU.ExternalID != "gpu" {
				t.Fatalf("GPUs=%+v err=%v", gpus, err)
			}
		})
	}
}

func TestGPUProtocolsAreIsolatedAcrossConcurrentEnvironments(t *testing.T) {
	var legacyReads, modernPCIReads atomic.Int32
	legacy := facade.NewGPUQuery(testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == nativePath {
			missingRoute(w, r)
			return
		}
		legacyReads.Add(1)
		fmt.Fprint(w, `{"inventories":[]}`)
	}))
	modern := facade.NewGPUQuery(testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == legacyPath {
			modernPCIReads.Add(1)
		}
		fmt.Fprint(w, `{"inventories":[]}`)
	}))
	var wg sync.WaitGroup
	for range 10 {
		for _, query := range []*facade.GPUQueries{legacy, modern} {
			wg.Go(func() {
				if _, err := query.List(t.Context()); err != nil {
					t.Error(err)
				}
			})
		}
	}
	wg.Wait()
	if legacyReads.Load() != 10 || modernPCIReads.Load() != 0 {
		t.Fatal("protocol selection leaked across environments")
	}
}
