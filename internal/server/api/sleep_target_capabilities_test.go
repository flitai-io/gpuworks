package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func setSleepFixtureGPUVendor(t *testing.T, f sleepFixture, gpuID uint, vendor string) {
	t.Helper()
	if err := f.st.DB().Model(&store.GPU{}).Where("id = ?", gpuID).Update("vendor", vendor).Error; err != nil {
		t.Fatal(err)
	}
}

func TestSleepTargetPolicyUsesSelectedGPUOnMixedNode(t *testing.T) {
	f := newSleepFixture(t)
	gpu := store.GPU{Index: 1, UUID: "gpu-1", NodeID: f.node.ID, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 24 << 30}
	if err := f.st.DB().Create(&gpu).Error; err != nil {
		t.Fatal(err)
	}
	setSleepFixtureGPUVendor(t, f, f.node.GPUs[0].ID, "amd")
	dep := f.deployment(t, &store.Deployment{Name: "mixed", ServedName: "mixed", Engine: "vllm", Sleepable: true})
	inst := f.instance(t, dep, "mixed-nv", 0)
	if err := f.st.DB().Model(&store.Instance{}).Where("id = ?", inst.ID).Update("GPUIndexes", "[1]").Error; err != nil {
		t.Fatal(err)
	}
	status, body := f.call(t, f.h.sleepDeployment, "/api/v1/deployments/x/sleep", dep.ID)
	if status != http.StatusOK || body["status"] != "sleeping" {
		t.Fatalf("sleep on selected NVIDIA GPU of mixed node = %d %v", status, body)
	}
	if got := sleepIntent(t, f.st, inst.UUID); got != 1 {
		t.Fatalf("selected NVIDIA instance sleep intent = %d, want 1", got)
	}
}

func TestSleepTargetPolicyRefusesRestrictedVendorSleepButAllowsWake(t *testing.T) {
	for _, vendor := range []string{"apple", "kunlunxin"} {
		t.Run(vendor, func(t *testing.T) {
			f := newSleepFixture(t)
			setSleepFixtureGPUVendor(t, f, f.node.GPUs[0].ID, vendor)
			dep := f.deployment(t, &store.Deployment{
				Name: "legacy-" + vendor, ServedName: "legacy-" + vendor, Engine: "vllm",
				Sleepable: true, DesiredSleep: 1,
			})
			inst := f.instance(t, dep, "legacy-"+vendor+"-1", 1)
			before := inst.DesiredStateJSON

			status, body := f.call(t, f.h.sleepDeployment, "/api/v1/deployments/x/sleep", dep.ID)
			if status != http.StatusBadRequest || body["code"] != messages.ErrorAcceleratorFeatureUnsupported {
				t.Fatalf("new sleep order for restricted-vendor deployment = %d %v", status, body)
			}
			gotDep, err := f.st.GetDeployment(dep.ID)
			if err != nil {
				t.Fatal(err)
			}
			gotInst, err := f.st.InstanceByUUID(inst.UUID)
			if err != nil {
				t.Fatal(err)
			}
			if gotDep.DesiredSleep != 1 || gotInst.DesiredStateJSON != before {
				t.Fatalf("refused sleep changed state: desired_sleep=%d desired_state=%s", gotDep.DesiredSleep, gotInst.DesiredStateJSON)
			}

			status, body = f.call(t, f.h.wakeDeployment, "/api/v1/deployments/x/wake", dep.ID)
			if status != http.StatusOK || body["status"] != "waking" {
				t.Fatalf("wake existing restricted-vendor sleeper = %d %v", status, body)
			}
			gotDep, err = f.st.GetDeployment(dep.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gotDep.DesiredSleep != 0 || sleepIntent(t, f.st, inst.UUID) != 0 {
				t.Fatalf("restricted-vendor wake did not clear sleep intent: deployment=%d instance=%d", gotDep.DesiredSleep, sleepIntent(t, f.st, inst.UUID))
			}
		})
	}
}

func TestInstanceSleepTargetPolicyRejectsRestrictedVendorWithoutChangingIntentAndAllowsWake(t *testing.T) {
	f := newSleepFixture(t)
	setSleepFixtureGPUVendor(t, f, f.node.GPUs[0].ID, "apple")
	dep := f.deployment(t, &store.Deployment{
		Name: "legacy-amd-instance", ServedName: "legacy-amd-instance", Engine: "vllm",
		Sleepable: true, DesiredSleep: 1,
	})
	inst := f.instance(t, dep, "legacy-amd-instance-1", 1)
	reportAsleep(t, f.st, inst.UUID, 1)
	before, err := f.st.InstanceByUUID(inst.UUID)
	if err != nil {
		t.Fatal(err)
	}

	rec := instanceRequest(f.h.sleepInstance, inst.UUID, "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"`+messages.ErrorAcceleratorFeatureUnsupported+`"`) {
		t.Fatalf("new instance sleep order for restricted-vendor deployment = %d %s", rec.Code, rec.Body.String())
	}
	after, err := f.st.InstanceByUUID(inst.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if after.DesiredStateJSON != before.DesiredStateJSON || sleepIntent(t, f.st, inst.UUID) != 1 {
		t.Fatalf("refused instance sleep changed intent: before=%s after=%s", before.DesiredStateJSON, after.DesiredStateJSON)
	}
	var beforeDesired proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(before.DesiredStateJSON), &beforeDesired); err != nil {
		t.Fatal(err)
	}
	if beforeDesired.DesiredSleep != 1 {
		t.Fatalf("fixture desired sleep = %d, want 1", beforeDesired.DesiredSleep)
	}

	rec = instanceRequest(f.h.wakeInstance, inst.UUID, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"waking"`) {
		t.Fatalf("wake existing restricted-vendor instance sleeper = %d %s", rec.Code, rec.Body.String())
	}
	if sleepIntent(t, f.st, inst.UUID) != 0 {
		t.Fatalf("restricted-vendor instance wake left intent at %d", sleepIntent(t, f.st, inst.UUID))
	}
}

func TestSleepTargetUnresolvedSelectionFailsClosedOnlyWhenNodeHasRestrictedVendor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		vendor  string
		indexes string
		want    int
	}{
		{name: "restricted empty selection", vendor: "apple", indexes: "[]", want: http.StatusInternalServerError},
		{name: "restricted stale index", vendor: "apple", indexes: "[99]", want: http.StatusInternalServerError},
		{name: "NVIDIA empty legacy selection", vendor: "nvidia", indexes: "[]", want: http.StatusOK},
		{name: "NVIDIA stale legacy index", vendor: "nvidia", indexes: "[99]", want: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSleepFixture(t)
			setSleepFixtureGPUVendor(t, f, f.node.GPUs[0].ID, tc.vendor)
			dep := f.deployment(t, &store.Deployment{Name: "unresolved", ServedName: "unresolved", Engine: "vllm", Sleepable: true})
			inst := f.instance(t, dep, "unresolved-1", 0)
			if err := f.st.DB().Model(&store.Instance{}).Where("id = ?", inst.ID).Update("GPUIndexes", tc.indexes).Error; err != nil {
				t.Fatal(err)
			}
			before, err := f.st.InstanceByUUID(inst.UUID)
			if err != nil {
				t.Fatal(err)
			}
			status, body := f.call(t, f.h.sleepDeployment, "/api/v1/deployments/x/sleep", dep.ID)
			if status != tc.want {
				t.Fatalf("sleep with %s = %d %v, want %d", tc.indexes, status, body, tc.want)
			}
			after, err := f.st.InstanceByUUID(inst.UUID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.vendor == "apple" {
				if after.DesiredStateJSON != before.DesiredStateJSON {
					t.Fatalf("unresolved restricted-vendor target changed desired state: before=%s after=%s", before.DesiredStateJSON, after.DesiredStateJSON)
				}
			} else if after.DesiredStateJSON == before.DesiredStateJSON {
				t.Fatal("pure NVIDIA legacy target did not retain the pre-existing sleep behavior")
			}
		})
	}
}
