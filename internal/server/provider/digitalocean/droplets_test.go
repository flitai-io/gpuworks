package digitalocean

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func gpuSize() map[string]any { return sizeJSON("gpu-h100x1-80gb", 4.41, "nvidia_h100", 1, 80) }
func cpuSize() map[string]any { return sizeJSON("s-2vcpu-4gb", 0.03571, "", 0, 0) }
func adaSize() map[string]any {
	return sizeJSON("gpu-4000adax1-20gb", 0.76, "nvidia_rtx4000_ada", 1, 20)
}
func mi300Size() map[string]any { return sizeJSON("gpu-mi300x8-1536gb", 20.72, "amd_mi300x", 8, 1536) }

func TestListInstancesKeepsGPUDropletsOnly(t *testing.T) {
	d, f := newFake(t)
	f.droplets = []map[string]any{
		dropletJSON(401, "trainer", "active", gpuSize(), "nyc2", "203.0.113.10", "10.0.0.5"),
		dropletJSON(402, "web", "active", cpuSize(), "nyc3", "203.0.113.11", "10.0.0.6"),
		dropletJSON(403, "ada", "off", adaSize(), "tor1", "203.0.113.12", ""),
		dropletJSON(404, "booting", "new", mi300Size(), "atl1", "", ""),
	}
	got, err := d.ListInstances(t.Context(), testCreds)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("listed %d instances, want the 3 GPU droplets: %+v", len(got), got)
	}
	a := got[0]
	want := provider.Instance{
		ExternalID: "401", Name: "trainer", GPUModel: "NVIDIA H100", GPUCount: 1, Power: provider.PowerRunning, PricePerHr: 4.41,
		Region: "nyc2", PublicIP: "203.0.113.10", PrivateIP: "10.0.0.5", SSHPort: 22, SSHUser: "root", Kind: "vm",
		SSHConnections: []provider.SSHConnection{{ID: "direct", Type: "direct", Label: provider.SSHConnectionLabel("direct"), Host: "203.0.113.10", Port: 22, User: "root", InstallSupported: true}},
	}
	if fmt.Sprintf("%+v", a) != fmt.Sprintf("%+v", want) {
		t.Errorf("instance =\n%+v\nwant\n%+v", a, want)
	}
	if got[1].ExternalID != "403" || got[1].GPUModel != "NVIDIA RTX 4000 Ada" || got[1].Power != provider.PowerStopped || got[1].PrivateIP != "" {
		t.Errorf("ada droplet = %+v", got[1])
	}
	// A droplet still being created has no address and so no SSH path.
	if got[2].ExternalID != "404" || got[2].Power != provider.PowerUnknown || got[2].GPUCount != 8 || got[2].GPUModel != "AMD MI300X" ||
		got[2].PublicIP != "" || len(got[2].SSHConnections) != 0 {
		t.Errorf("new droplet = %+v", got[2])
	}
}

func TestListInstancesFollowsEveryPage(t *testing.T) {
	d, f := newFake(t)
	for i := 0; i < 450; i++ {
		size := gpuSize()
		if i%3 == 0 {
			size = cpuSize()
		}
		f.droplets = append(f.droplets, dropletJSON(int64(1000+i), "d"+strconv.Itoa(i), "active", size, "nyc2", "203.0.113.1", ""))
	}
	got, err := d.ListInstances(t.Context(), testCreds)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 300 {
		t.Fatalf("listed %d GPU droplets, want 300", len(got))
	}
	var pages []string
	for _, r := range f.calls(http.MethodGet, "/v2/droplets") {
		pages = append(pages, r.Query.Get("page"))
	}
	if !slices.Equal(pages, []string{"1", "2", "3"}) {
		t.Errorf("pages requested = %v", pages)
	}
}

// The sync removes rows the listing does not show, so a listing that stops
// early must fail.
func TestListInstancesFailsRatherThanReturnPartial(t *testing.T) {
	t.Run("a failing page", func(t *testing.T) {
		d, f := newFake(t)
		for i := 0; i < 250; i++ {
			f.droplets = append(f.droplets, dropletJSON(int64(1000+i), "d", "active", gpuSize(), "nyc2", "203.0.113.1", ""))
		}
		// Page 1 is served, page 2 fails with a non-retryable error.
		f.failPage("2", http.StatusForbidden)
		got, err := d.ListInstances(t.Context(), testCreds)
		if err == nil || got != nil {
			t.Fatalf("got %d instances, err %v; want an error and no instances", len(got), err)
		}
	})
	t.Run("fewer items than meta.total", func(t *testing.T) {
		d, f := newFake(t)
		for i := 0; i < 10; i++ {
			f.droplets = append(f.droplets, dropletJSON(int64(1000+i), "d", "active", gpuSize(), "nyc2", "203.0.113.1", ""))
		}
		f.metaTotal = 25
		if _, err := d.ListInstances(t.Context(), testCreds); err == nil {
			t.Fatal("a listing of 10 items against meta.total 25 was accepted")
		}
	})
	t.Run("a next page that is empty", func(t *testing.T) {
		d, f := newFake(t)
		f.emptyNext = true
		if _, err := d.ListInstances(t.Context(), testCreds); err == nil {
			t.Fatal("an empty page that names a next page was accepted")
		}
	})
}

func TestDOKSWorkerDropletsCarryTheirCluster(t *testing.T) {
	d, f := newFake(t)
	f.droplets = []map[string]any{
		dropletJSON(501, "pool-gpu-abc", "active", gpuSize(), "nyc2", "203.0.113.20", "10.0.0.7", "k8s", "k8s:worker", "k8s:bd5f5959-5e1e-4205-a714-a914373942af"),
		dropletJSON(502, "standalone", "active", gpuSize(), "nyc2", "203.0.113.21", "", "gpuworks"),
		dropletJSON(503, "odd", "active", gpuSize(), "nyc2", "203.0.113.22", "", "k8s:worker"),
	}
	got, err := d.ListInstances(t.Context(), testCreds)
	if err != nil {
		t.Fatal(err)
	}
	clusters := []string{got[0].Cluster, got[1].Cluster, got[2].Cluster}
	if !slices.Equal(clusters, []string{"bd5f5959-5e1e-4205-a714-a914373942af", "", ""}) {
		t.Errorf("clusters = %q", clusters)
	}
}

// The listing gives the size slug even without the embedded size object.
func TestListInstancesReadsAGPUSizeFromItsSlug(t *testing.T) {
	d, f := newFake(t)
	dr := dropletJSON(601, "x", "active", sizeJSON("gpu-h100x8-640gb", 35.28, "nvidia_h100", 8, 640), "nyc2", "203.0.113.30", "")
	delete(dr, "size")
	f.droplets = []map[string]any{dr}
	got, err := d.ListInstances(t.Context(), testCreds)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].GPUModel != "NVIDIA H100" || got[0].GPUCount != 8 || got[0].PricePerHr != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestPowerStateMapping(t *testing.T) {
	d, f := newFake(t)
	cases := map[string]provider.PowerState{
		"active": provider.PowerRunning, "off": provider.PowerStopped, "archive": provider.PowerStopped, "new": provider.PowerUnknown,
	}
	id := int64(700)
	for status, want := range cases {
		id++
		f.droplets = append(f.droplets, dropletJSON(id, "d", status, gpuSize(), "nyc2", "203.0.113.40", ""))
		got, err := d.PowerState(t.Context(), testCreds, strconv.FormatInt(id, 10))
		if err != nil || got != want {
			t.Errorf("status %q: PowerState = %q, %v; want %q", status, got, err, want)
		}
	}
	if _, err := d.PowerState(t.Context(), testCreds, "999"); err == nil {
		t.Error("PowerState of an unknown droplet succeeded")
	}
}

func TestPowerOnAndOffSendTheActionAndChangeTheDroplet(t *testing.T) {
	d, f := newFake(t)
	f.droplets = []map[string]any{dropletJSON(801, "d", "active", gpuSize(), "nyc2", "203.0.113.50", "")}
	if err := d.PowerOff(t.Context(), testCreds, "801"); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.PowerState(t.Context(), testCreds, "801"); st != provider.PowerStopped {
		t.Errorf("after power_off: %q", st)
	}
	if err := d.PowerOn(t.Context(), testCreds, "801"); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.PowerState(t.Context(), testCreds, "801"); st != provider.PowerRunning {
		t.Errorf("after power_on: %q", st)
	}
	var types []any
	for _, r := range f.calls(http.MethodPost, "/v2/droplets/801/actions") {
		if len(r.Body) != 1 {
			t.Errorf("action body = %v, want only the type", r.Body)
		}
		types = append(types, r.Body["type"])
	}
	if fmt.Sprint(types) != "[power_off power_on]" {
		t.Errorf("action types = %v", types)
	}
	if err := d.PowerOn(t.Context(), testCreds, "802"); err == nil {
		t.Error("power_on of an unknown droplet succeeded")
	}
}

func TestDeleteInstance(t *testing.T) {
	d, f := newFake(t)
	f.droplets = []map[string]any{
		dropletJSON(901, "a", "active", gpuSize(), "nyc2", "203.0.113.60", ""),
		dropletJSON(902, "b", "active", gpuSize(), "nyc2", "203.0.113.61", ""),
	}
	if err := d.DeleteInstance(t.Context(), testCreds, "901"); err != nil {
		t.Fatal(err)
	}
	if len(f.droplets) != 1 || f.droplets[0]["id"].(int64) != 902 {
		t.Errorf("droplets left = %v", f.droplets)
	}
	// Already gone: deleted.
	if err := d.DeleteInstance(t.Context(), testCreds, "901"); err != nil {
		t.Errorf("deleting a droplet that is gone: %v", err)
	}
	if n := len(f.calls(http.MethodDelete, "/v2/droplets/901")); n != 2 {
		t.Errorf("DELETE sent %d times, want 2", n)
	}
}

func TestDropletIDsAreNumbers(t *testing.T) {
	d, f := newFake(t)
	for _, id := range []string{"", "abc", "12/../account", "-5", "0"} {
		if err := d.DeleteInstance(t.Context(), testCreds, id); err == nil {
			t.Errorf("DeleteInstance(%q) succeeded", id)
		}
		if err := d.PowerOn(t.Context(), testCreds, id); err == nil {
			t.Errorf("PowerOn(%q) succeeded", id)
		}
		if _, err := d.GetInstance(t.Context(), testCreds, id); err == nil {
			t.Errorf("GetInstance(%q) succeeded", id)
		}
	}
	if len(f.requests) != 0 {
		t.Errorf("requests were sent for invalid ids: %v", f.requests)
	}
}

func TestGetInstanceAndResize(t *testing.T) {
	d, f := newFake(t)
	f.droplets = []map[string]any{dropletJSON(1101, "d", "active", gpuSize(), "nyc2", "203.0.113.70", "10.0.0.9")}
	inst, err := d.GetInstance(t.Context(), testCreds, "1101")
	if err != nil || inst.ExternalID != "1101" || inst.PublicIP != "203.0.113.70" || inst.GPUCount != 1 {
		t.Errorf("GetInstance = %+v, %v", inst, err)
	}
	if err := d.ResizeInstance(t.Context(), testCreds, "1101", provider.ResizeInstanceRequest{}); !errors.Is(err, provider.ErrUnsupported) {
		t.Errorf("ResizeInstance = %v, want ErrUnsupported", err)
	}
}
