package cloudprovider

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
	compsharedriver "gpuworks.ai/gpuworks/internal/server/provider/compshare"
)

// podDriver behaves like the CompShare driver for a container: it looks for
// the container zones itself unless the caller pinned both region and zone,
// refuses an image that is not in the container catalog the way the real API
// does (RetCode 226641), and returns the instance with no SSH endpoint, which
// the listing reveals only once the container's forwarded SSH port exists.
type podDriver struct {
	containerImage string
	podZone        serverprovider.Zone
	created        []serverprovider.CreateInstanceRequest
	listCalls      int
	endpointAfter  int // listings that still show no endpoint
}

func newPodDriver(endpointAfter int) *podDriver {
	return &podDriver{containerImage: "pod-image-1", podZone: serverprovider.Zone{Region: "cn-wlcb", Zone: "cn-wlcb-pod", IsPod: true}, endpointAfter: endpointAfter}
}

func (d *podDriver) CreateInstance(_ context.Context, _ serverprovider.Credentials, req serverprovider.CreateInstanceRequest) (serverprovider.Instance, error) {
	d.created = append(d.created, req)
	wantPod := req.Extra["instance_kind"] == "container"
	region, zone := req.Extra["region"], req.Extra["zone"]
	if region == "" || zone == "" {
		// Zone discovery: the pod zones for a container, the VM zones else.
		if wantPod {
			region, zone = d.podZone.Region, d.podZone.Zone
		} else {
			region, zone = "cn-wlcb", "cn-wlcb-01"
		}
	}
	unsupported := fmt.Errorf("compshare: create in %s/%s: %w", region, zone, &compsharedriver.APIError{Status: 200, RetCode: 226641, Message: "The current availability zone does not support this image."})
	if wantPod != (zone == d.podZone.Zone) {
		return serverprovider.Instance{}, unsupported
	}
	if wantPod && req.Image != d.containerImage {
		return serverprovider.Instance{}, unsupported
	}
	kind, port := "vm", 22
	if wantPod {
		kind, port = "container", 0
	}
	return serverprovider.Instance{ExternalID: "uhost-pod-1", GPUModel: req.GPUModel, GPUCount: req.GPUCount, Kind: kind, Region: region, Zone: zone, SSHPort: port, SSHUser: "root"}, nil
}

func (d *podDriver) ListInstances(context.Context, serverprovider.Credentials) ([]serverprovider.Instance, error) {
	d.listCalls++
	inst := serverprovider.Instance{ExternalID: "uhost-pod-1", GPUModel: "4090", GPUCount: 1, Kind: "container", Region: d.podZone.Region, Zone: d.podZone.Zone, SSHUser: "root"}
	if d.listCalls > d.endpointAfter {
		inst.PublicIP, inst.SSHPort = "pod-gw.podtcp.compshare.cn", 30123
	}
	return []serverprovider.Instance{inst}, nil
}

func (d *podDriver) DeleteInstance(context.Context, serverprovider.Credentials, string) error {
	return nil
}

func (d *podDriver) SSHAccess(context.Context, serverprovider.Credentials, string) (string, int, string, string, error) {
	return "pod-gw.podtcp.compshare.cn", 30123, "root", "generated-pw", nil
}

func newPodProvider(t *testing.T, driver compshareDriver, containerImage string) *Compshare {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	old := compshareEndpointPoll
	compshareEndpointPoll = time.Millisecond
	t.Cleanup(func() { compshareEndpointPoll = old })
	p, err := newCompshare(driver, serverprovider.Credentials{PublicKey: "pub", PrivateKey: "priv"}, "vm-golden-image", nil, "run-pod")
	if err != nil {
		t.Fatal(err)
	}
	return p.WithContainerImage(containerImage)
}

func TestCompshareRentContainerUsesContainerImageAndLetsTheDriverPlaceIt(t *testing.T) {
	driver := newPodDriver(2)
	p := newPodProvider(t, driver, " pod-image-1 ")
	// A VM placement chosen earlier for the same card must not leak into the
	// container's request: the VM zone is not a pod zone.
	p.placements[placementKey("4090", 1)] = serverprovider.Zone{Region: "cn-wlcb", Zone: "cn-wlcb-01"}
	instance, err := p.Rent(t.Context(), GPUSpec{GPUModel: "4090", GPUCount: 1, DiskGB: 100, Container: true})
	if err != nil {
		t.Fatal(err)
	}
	req := driver.created[0]
	if req.Image != "pod-image-1" || req.Extra["instance_kind"] != "container" || req.Extra["region"] != "" || req.Extra["zone"] != "" {
		t.Fatalf("container create request = %+v", req)
	}
	if req.Name != TestNamePrefix+"run-pod" {
		t.Fatalf("container is not marked for run-id cleanup: name=%q", req.Name)
	}
	if instance.Kind != "container" || instance.PublicIP != "pod-gw.podtcp.compshare.cn" || instance.SSHPort != 30123 || instance.Password != "generated-pw" || instance.Username != "root" {
		t.Fatalf("instance = %+v", instance)
	}
	if driver.listCalls <= driver.endpointAfter {
		t.Fatalf("Rent returned after %d listings, before the SSH endpoint appeared (after %d)", driver.listCalls, driver.endpointAfter)
	}
	// The ledger records it for cleanup, and destroying clears it.
	ledger, err := readCompshareLedger()
	if err != nil || len(ledger) != 1 || ledger[0] != "uhost-pod-1" {
		t.Fatalf("ledger = %v err=%v", ledger, err)
	}
	if err := p.Destroy(t.Context(), instance.InstanceID); err != nil {
		t.Fatal(err)
	}
	if ledger, _ := readCompshareLedger(); len(ledger) != 0 {
		t.Fatalf("ledger after destroy = %v", ledger)
	}
}

func TestCompshareRentContainerNeedsAContainerImage(t *testing.T) {
	driver := newPodDriver(0)
	p := newPodProvider(t, driver, "")
	_, err := p.Rent(t.Context(), GPUSpec{GPUModel: "4090", GPUCount: 1, Container: true})
	if err == nil || !strings.Contains(err.Error(), "container image id is required") || len(driver.created) != 0 {
		t.Fatalf("err=%v creates=%d", err, len(driver.created))
	}
}

// The container image must never be used for a VM, nor the VM golden image
// for a container: each Rent sends its own product's image.
func TestCompshareRentVMIgnoresContainerImage(t *testing.T) {
	driver := newPodDriver(0)
	p := newPodProvider(t, driver, "pod-image-1")
	p.placements[placementKey("4090", 1)] = serverprovider.Zone{Region: "cn-wlcb", Zone: "cn-wlcb-01"}
	instance, err := p.Rent(t.Context(), GPUSpec{GPUModel: "4090", GPUCount: 1, DiskGB: 100})
	if err != nil {
		t.Fatal(err)
	}
	req := driver.created[0]
	if req.Image != "vm-golden-image" || req.Extra["instance_kind"] != "vm" || req.Extra["zone"] != "cn-wlcb-01" {
		t.Fatalf("vm create request = %+v", req)
	}
	_ = p.Destroy(context.Background(), instance.InstanceID)
}

// A container image the pod zone does not carry fails the rent with the
// provider's own error, and is not retried in another (VM) zone.
func TestCompshareRentContainerWithUnknownImageFails(t *testing.T) {
	driver := newPodDriver(0)
	p := newPodProvider(t, driver, "some-other-image")
	p.placements[placementKey("4090", 1)] = serverprovider.Zone{Region: "cn-wlcb", Zone: "cn-wlcb-01"}
	_, err := p.Rent(t.Context(), GPUSpec{GPUModel: "4090", GPUCount: 1, Container: true})
	if err == nil || !IsImageUnsupportedInZone(err) {
		t.Fatalf("err = %v", err)
	}
	if len(driver.created) != 1 {
		t.Fatalf("creates = %d, want one (no retry in another zone for a container)", len(driver.created))
	}
}
