package kube

import (
	"maps"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/deviceplugin"
)

func TestPlanDevicesSplitsSharedAndOwnNodes(t *testing.T) {
	// Ascend, two cards: each its davinciN, both the three control nodes.
	common := []string{"/dev/davinci_manager", "/dev/devmm_svm", "/dev/hisi_hdc"}
	plan, err := planDevices(map[int][]string{6: append([]string{"/dev/davinci6"}, common...), 5: append([]string{"/dev/davinci5"}, common...)})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.resources) != 3 || plan.resources[0].Name != "gpuworks.ai/accelerator-5" || plan.resources[1].Name != "gpuworks.ai/accelerator-6" || plan.resources[2].Name != "gpuworks.ai/accelerator-shared" {
		t.Fatalf("resources = %+v", plan.resources)
	}
	if got := plan.resources[0].Devices; len(got) != 1 || got[0] != (deviceplugin.DeviceSpec{ContainerPath: "/dev/davinci5", HostPath: "/dev/davinci5", Permissions: "rwm"}) {
		t.Fatalf("card 5 = %+v", got)
	}
	if len(plan.resources[2].Devices) != 3 || plan.resources[2].Units != 64 || plan.resources[0].Units != 32 {
		t.Fatalf("shared = %+v", plan.resources[2])
	}
	reqs, uncovered, err := plan.requests([]string{"/dev/davinci6", "/dev/davinci_manager", "/dev/devmm_svm", "/dev/hisi_hdc"}, []int{6})
	if err != nil || uncovered || !maps.Equal(reqs, map[string]string{"gpuworks.ai/accelerator-6": "1", "gpuworks.ai/accelerator-shared": "1"}) {
		t.Fatalf("requests = %v %v %v", reqs, uncovered, err)
	}
	if _, uncovered, _ := plan.requests([]string{"/dev/davinci5", "/dev/infiniband"}, []int{5}); !uncovered {
		t.Fatal("an InfiniBand node counted as the plugin's")
	}
	if reqs, _, _ := plan.requests(nil, []int{5}); reqs != nil {
		t.Fatalf("no devices = %v", reqs)
	}
}

// TestPlanDevicesOneCard: with one card every node is shared; the card's own
// resource still accounts for the pod on it.
func TestPlanDevicesOneCard(t *testing.T) {
	plan, err := planDevices(map[int][]string{0: {"/dev/alixpu:/dev/alixpu:rwm", "/dev/alixpu_ctl:/dev/alixpu_ctl:rwm", "/dev/alixpu_ppu0:/dev/alixpu_ppu0:rwm"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.resources) != 2 || len(plan.resources[0].Devices) != 0 || len(plan.resources[1].Devices) != 3 {
		t.Fatalf("plan = %+v", plan.resources)
	}
	reqs, uncovered, err := plan.requests([]string{"/dev/alixpu:/dev/alixpu:rwm", "/dev/alixpu_ppu0:/dev/alixpu_ppu0:rwm"}, []int{0})
	if err != nil || uncovered || !maps.Equal(reqs, map[string]string{"gpuworks.ai/accelerator-0": "1", "gpuworks.ai/accelerator-shared": "1"}) {
		t.Fatalf("requests = %v %v %v", reqs, uncovered, err)
	}
}

func TestPlanDevicesRefusesPartlySharedNodes(t *testing.T) {
	// A node two of three cards claim is neither one card's nor everyone's.
	_, err := planDevices(map[int][]string{0: {"/dev/a", "/dev/x"}, 1: {"/dev/b", "/dev/x"}, 2: {"/dev/c"}})
	if err == nil || !strings.Contains(err.Error(), "/dev/x") {
		t.Fatalf("planDevices = %v", err)
	}
	if _, err := planDevices(map[int][]string{0: {"etc/passwd"}}); err == nil {
		t.Fatal("a non-device path was planned")
	}
	if plan, err := planDevices(nil); err != nil || len(plan.resources) != 0 {
		t.Fatalf("no cards = %+v, %v", plan, err)
	}
}

func TestParseDevice(t *testing.T) {
	for in, want := range map[string]deviceplugin.DeviceSpec{
		"/dev/davinci5":                         {ContainerPath: "/dev/davinci5", HostPath: "/dev/davinci5", Permissions: "rwm"},
		"/dev/alixpu:/dev/alixpu:rw":            {ContainerPath: "/dev/alixpu", HostPath: "/dev/alixpu", Permissions: "rw"},
		"/dev/dri/renderD129:/dev/dri/renderD0": {ContainerPath: "/dev/dri/renderD0", HostPath: "/dev/dri/renderD129", Permissions: "rwm"},
	} {
		if got, err := parseDevice(in); err != nil || got != want {
			t.Errorf("parseDevice(%q) = %+v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"davinci5", "/etc/passwd", "/dev/x:/dev/y:rwx", "/dev/x:/dev/y:", "/dev/a:/dev/b:r:w"} {
		if _, err := parseDevice(bad); err == nil {
			t.Errorf("parseDevice(%q) accepted", bad)
		}
	}
}
