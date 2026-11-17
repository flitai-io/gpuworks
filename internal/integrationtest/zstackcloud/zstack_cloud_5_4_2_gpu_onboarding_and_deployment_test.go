package zstackcloud_test

import (
	"net/http"
	"testing"
)

func TestZStackCloud5_4_2GPUOnboardingAndDeploymentRestart(t *testing.T) {
	fixture := newGPUDeploymentTest(t, cloudGPUAPIOptions{allocationField: true, hostCPU: true})
	providerPath := addSyncInstall(t, fixture.env, fixture.cloud.url, "host-a")
	nodeUUID := waitForInstalledHost(t, fixture.env, providerPath, "host-a")
	fixture.assertInstalledHost(t, nodeUUID, 2)

	path := fixture.createDeployment(t, nodeUUID)
	first := fixture.waitRunning(t, path, "")
	firstLease := fixture.assertReserved(t, first, 1, "原有备注\n被gpuworks-127.0.0.1使用中")
	fixture.cloud.appendDescription("\n运行期间补充")

	waitForRetry, allowRemoval := fixture.blockContainerRemoval(t)
	requestJSON(t, fixture.env, "POST", path+"/stop", `{}`, http.StatusOK)
	waitForRetry()
	fixture.assertReserved(t, first, 1, "原有备注\n被gpuworks-127.0.0.1使用中\n运行期间补充")
	allowRemoval()
	fixture.waitReleased(t, path, firstLease, 1, "原有备注\n运行期间补充")

	requestJSON(t, fixture.env, "POST", path+"/start", `{}`, http.StatusOK)
	second := fixture.waitRunning(t, path, first)
	secondLease := fixture.assertReserved(t, second, 2, "原有备注\n运行期间补充\n被gpuworks-127.0.0.1使用中")
	if secondLease == firstLease {
		t.Fatal("restarted instance reused a released Cloud lease")
	}
	requestJSON(t, fixture.env, "POST", path+"/stop", `{}`, http.StatusOK)
	fixture.waitReleased(t, path, secondLease, 2, "原有备注\n运行期间补充")

	state := fixture.cloud.state()
	if state.nativeLists == 0 || state.nativeTargets == 0 || state.pciLists != 0 || state.pciTargets != 0 || state.missingRoute != 0 {
		t.Fatalf("Cloud 5.4.2 must use GPU discovery and queries: %+v", state)
	}
	if state.globalCPUReads != 0 {
		t.Fatalf("Cloud 5.4.2 must read the host CPU reservation baseline: %+v", state)
	}
}
