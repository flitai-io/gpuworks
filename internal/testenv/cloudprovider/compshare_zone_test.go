package cloudprovider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
	compsharedriver "gpuworks.ai/gpuworks/internal/server/provider/compshare"
)

// zoneImageDriver behaves like CompShare across two zones: the capacity API
// reports stock everywhere without looking at the image, and only a create
// in a zone lacking the image fails with RetCode 226641, worded the way the
// real driver wraps it.
type zoneImageDriver struct {
	zones     []serverprovider.Zone
	noImage   map[string]bool // region/zone where create fails
	creates   []string        // region/zone of every create, in order
	instances []serverprovider.Instance
}

func (d *zoneImageDriver) SupportedZones(context.Context, serverprovider.Credentials) ([]serverprovider.Zone, error) {
	return d.zones, nil
}

func (d *zoneImageDriver) CheckResourceCapacity(_ context.Context, _ serverprovider.Credentials, req compsharedriver.ResourceCapacityRequest) ([]compsharedriver.ResourceCapacitySpec, error) {
	if req.Region == "" || req.Zone == "" {
		return nil, errors.New("capacity query without a zone")
	}
	return []compsharedriver.ResourceCapacitySpec{{GPU: 2, CPU: 32, MemoryGB: 128, ResourceEnough: true}}, nil
}

func (d *zoneImageDriver) CreateInstance(_ context.Context, _ serverprovider.Credentials, req serverprovider.CreateInstanceRequest) (serverprovider.Instance, error) {
	where := req.Extra["region"] + "/" + req.Extra["zone"]
	d.creates = append(d.creates, where)
	if d.noImage[where] {
		return serverprovider.Instance{}, fmt.Errorf("compshare: create in %s: %w", where, &compsharedriver.APIError{Status: 200, RetCode: 226641, Message: "The current availability zone does not support this image."})
	}
	inst := serverprovider.Instance{ExternalID: "uhost-" + req.Extra["zone"], GPUModel: req.GPUModel, GPUCount: req.GPUCount, Kind: "vm", Region: req.Extra["region"], PublicIP: "203.0.113.7", SSHPort: 22, SSHUser: "root"}
	d.instances = append(d.instances, inst)
	return inst, nil
}

func (d *zoneImageDriver) ListInstances(context.Context, serverprovider.Credentials) ([]serverprovider.Instance, error) {
	return d.instances, nil
}

func (d *zoneImageDriver) DeleteInstance(context.Context, serverprovider.Credentials, string) error {
	return nil
}

func (d *zoneImageDriver) SSHAccess(context.Context, serverprovider.Credentials, string) (string, int, string, string, error) {
	return "203.0.113.7", 22, "root", "pw", nil
}

func twoZones() []serverprovider.Zone {
	return []serverprovider.Zone{{Region: "cn-sh2", Zone: "cn-sh2-02"}, {Region: "cn-wlcb", Zone: "cn-wlcb-01"}}
}

func TestCompshareRentMovesToAZoneThatHasTheImage(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	driver := &zoneImageDriver{zones: twoZones(), noImage: map[string]bool{"cn-sh2/cn-sh2-02": true}}
	p, err := newCompshare(driver, serverprovider.Credentials{PublicKey: "pub", PrivateKey: "priv"}, "image-1", nil, "run-zone")
	if err != nil {
		t.Fatal(err)
	}
	if _, zone, err := p.SelectPlacement(t.Context(), "4090", 2, 120); err != nil || zone.Zone != "cn-sh2-02" {
		t.Fatalf("first placement = %+v err=%v", zone, err)
	}
	instance, err := p.Rent(t.Context(), GPUSpec{GPUModel: "4090", GPUCount: 2, DiskGB: 120})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(driver.creates, ",") != "cn-sh2/cn-sh2-02,cn-wlcb/cn-wlcb-01" || instance.InstanceID != "uhost-cn-wlcb-01" {
		t.Fatalf("creates=%v instance=%+v", driver.creates, instance)
	}
	// The rejected zone stays excluded for the rest of the run.
	if _, zone, err := p.SelectPlacement(t.Context(), "4090", 2, 120); err != nil || zone.Zone != "cn-wlcb-01" {
		t.Fatalf("placement after rejection = %+v err=%v", zone, err)
	}
	_ = p.Destroy(context.Background(), instance.InstanceID)
}

func TestCompshareRentNamesEveryZoneWithoutTheImage(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	driver := &zoneImageDriver{zones: twoZones(), noImage: map[string]bool{"cn-sh2/cn-sh2-02": true, "cn-wlcb/cn-wlcb-01": true}}
	p, err := newCompshare(driver, serverprovider.Credentials{PublicKey: "pub", PrivateKey: "priv"}, "image-1", nil, "run-zone")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.SelectPlacement(t.Context(), "4090", 2, 120); err != nil {
		t.Fatal(err)
	}
	_, err = p.Rent(t.Context(), GPUSpec{GPUModel: "4090", GPUCount: 2, DiskGB: 120})
	if err == nil || !strings.Contains(err.Error(), "cn-sh2/cn-sh2-02, cn-wlcb/cn-wlcb-01") || !strings.Contains(err.Error(), "226641") {
		t.Fatalf("err = %v", err)
	}
	if len(driver.creates) != 2 {
		t.Fatalf("creates = %v", driver.creates)
	}
}

// Other create errors are not retried in another zone.
func TestCompshareRentDoesNotRetryOtherErrors(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	driver := &zoneImageDriver{zones: twoZones()}
	failing := &failingCreateDriver{zoneImageDriver: driver, err: errors.New("compshare: API RetCode 8040: insufficient balance")}
	p, err := newCompshare(failing, serverprovider.Credentials{PublicKey: "pub", PrivateKey: "priv"}, "image-1", nil, "run-zone")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.SelectPlacement(t.Context(), "4090", 2, 120); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Rent(t.Context(), GPUSpec{GPUModel: "4090", GPUCount: 2, DiskGB: 120}); err == nil || !strings.Contains(err.Error(), "8040") || failing.calls != 1 {
		t.Fatalf("err=%v calls=%d", err, failing.calls)
	}
}

type failingCreateDriver struct {
	*zoneImageDriver
	err   error
	calls int
}

func (d *failingCreateDriver) CreateInstance(context.Context, serverprovider.Credentials, serverprovider.CreateInstanceRequest) (serverprovider.Instance, error) {
	d.calls++
	return serverprovider.Instance{}, d.err
}

func TestCompshareErrorKindsAndDisplayNames(t *testing.T) {
	if !IsImageUnsupportedInZone(&compsharedriver.APIError{Status: 200, RetCode: 226641, Message: "The current availability zone does not support this image."}) || IsImageUnsupportedInZone(nil) {
		t.Fatal("226641")
	}
	if !IsGPUTypeUnsupportedByImage(fmt.Errorf("compshare: check resource capacity: %w", &compsharedriver.APIError{Status: 200, RetCode: 226603, Message: "gpu type is not support current image"})) || IsGPUTypeUnsupportedByImage(&compsharedriver.APIError{Status: 200, RetCode: 8433, Message: "service busy"}) {
		t.Fatal("226603")
	}
	for gpuType, want := range map[string]string{"4090_48G": "4090", "4090": "4090", "3080Ti": "3080Ti", " A800 ": "A800"} {
		if got := CompshareGPUDisplayName(gpuType); got != want {
			t.Fatalf("%q -> %q, want %q", gpuType, got, want)
		}
	}
}
