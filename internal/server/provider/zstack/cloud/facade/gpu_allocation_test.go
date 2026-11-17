package facade_test

import (
	"fmt"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/facade"
)

func TestGPUAllocationUsesPCIStateOnlyWhenAllocationFieldIsAbsent(t *testing.T) {
	for _, tc := range []struct {
		name, fields, want string
		wantError          bool
		wantCount          int
	}{
		{"free physical GPU", `"status":"Active","virtStatus":"UNVIRTUALIZABLE"`, "Unallocated", false, 1},
		{"system physical GPU", `"status":"System","virtStatus":"SRIOV_VIRTUALIZABLE"`, "Unallocated", false, 1},
		{"VM reserved", `"status":"Reserved","virtStatus":"UNVIRTUALIZABLE"`, "Allocated", false, 1},
		{"VM attached", `"status":"Attached","virtStatus":"UNVIRTUALIZABLE"`, "Allocated", false, 1},
		{"VM reference", `"status":"Active","virtStatus":"UNVIRTUALIZABLE","vmInstanceUuid":"vm"`, "Allocated", false, 1},
		{"missing PCI state", `"status":"Active"`, "", false, 1},
		{"unknown PCI state", `"status":"Future","virtStatus":"UNVIRTUALIZABLE"`, "", false, 1},
		{"virtual child", `"status":"Active","virtStatus":"UNVIRTUALIZABLE","parentUuid":"parent"`, "", false, 0},
		{"virtualized physical GPU", `"status":"Active","virtStatus":"VFIO_MDEV_VIRTUALIZED"`, "", false, 1},
		{"explicit allocation wins", `"status":"Active","virtStatus":"UNVIRTUALIZABLE","allocateStatus":"Allocated"`, "Allocated", false, 1},
		{"current allocation remains available", `"allocateStatus":"Unallocated"`, "Unallocated", false, 1},
		{"empty allocation stays unknown", `"status":"Active","virtStatus":"UNVIRTUALIZABLE","allocateStatus":""`, "", false, 1},
		{"null allocation stays unknown", `"status":"Active","virtStatus":"UNVIRTUALIZABLE","allocateStatus":null`, "", false, 1},
		{"future allocation is preserved", `"status":"Active","virtStatus":"UNVIRTUALIZABLE","allocateStatus":"Future"`, "Future", false, 1},
		{"invalid allocation is rejected", `"status":"Active","virtStatus":"UNVIRTUALIZABLE","allocateStatus":42`, "", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != nativePath {
					t.Errorf("GPU inventory already includes PCI fields; unexpected query: %s", r.URL)
					http.NotFound(w, r)
					return
				}
				fmt.Fprintf(w, `{"inventories":[{"uuid":"gpu","hostUuid":"host","type":"GPU_3D_Controller","state":"Enabled",%s}]}`, tc.fields)
			})
			query := facade.NewGPUQuery(client)
			list, listErr := query.List(t.Context())
			gpu, err := query.Get(t.Context(), "gpu")
			if (err != nil) != tc.wantError || (listErr != nil) != tc.wantError {
				t.Fatalf("list error=%v get error=%v", listErr, err)
			}
			if !tc.wantError {
				if len(list) != tc.wantCount || gpu.GPU.Allocation != tc.want {
					t.Fatalf("list=%+v gpu=%+v, want count %d and allocation %q", list, gpu, tc.wantCount, tc.want)
				}
				for _, listed := range list {
					if listed.GPU.Allocation != tc.want {
						t.Fatalf("listed GPU=%+v, want allocation %q", listed, tc.want)
					}
				}
			}
		})
	}
}

func TestIntermediateGPUInventoryPreservesNamesAndAllComputeTypes(t *testing.T) {
	for _, kind := range []string{"GPU_Video_Controller", "GPU_3D_Controller", "GPU_Processing_Accelerators", "GPU_Co_Processor", "GPU_Audio_Controller", "Future"} {
		t.Run(kind, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != nativePath {
					t.Errorf("unexpected PCI query: %s", r.URL)
				}
				fmt.Fprintf(w, `{"inventories":[{"uuid":"gpu","hostUuid":"host","name":"  Cloud device name  ","type":%q,"state":"Enabled","status":"Active","virtStatus":"UNVIRTUALIZABLE"}]}`, kind)
			})
			gpu, err := facade.NewGPUQuery(client).Get(t.Context(), "gpu")
			want := "Unallocated"
			if kind == "GPU_Audio_Controller" || kind == "Future" {
				want = ""
			}
			if err != nil || gpu.GPU.Allocation != want || gpu.GPU.Model != "Cloud device name" {
				t.Fatalf("gpu=%+v err=%v", gpu, err)
			}
		})
	}
}

func TestMixedGPUAllocationResponsesDoNotChangeEnvironmentProtocol(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != nativePath {
			t.Errorf("unexpected protocol change: %s", r.URL)
		}
		fmt.Fprint(w, `{"inventories":[
		{"uuid":"legacy","hostUuid":"host","type":"GPU_3D_Controller","state":"Enabled","status":"Active","virtStatus":"UNVIRTUALIZABLE"},
		{"uuid":"occupied","hostUuid":"host","type":"GPU_3D_Controller","state":"Enabled","status":"Active","virtStatus":"UNVIRTUALIZABLE","allocateStatus":"Allocated"},
		{"uuid":"unknown","hostUuid":"host","type":"GPU_3D_Controller","state":"Enabled","status":"Active","virtStatus":"UNVIRTUALIZABLE","allocateStatus":null}
		]}`)
	})
	query := facade.NewGPUQuery(client)
	for range 2 {
		gpus, err := query.List(t.Context())
		if err != nil || len(gpus) != 3 {
			t.Fatalf("GPUs=%+v err=%v", gpus, err)
		}
		for i, want := range []string{"Unallocated", "Allocated", ""} {
			if gpus[i].GPU.Allocation != want {
				t.Fatalf("GPU=%+v want=%s", gpus[i], want)
			}
		}
	}
}
