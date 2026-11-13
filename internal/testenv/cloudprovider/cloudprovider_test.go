package cloudprovider

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
	vastdriver "gpuworks.ai/gpuworks/internal/server/provider/vast"
)

type fakeProviderDriver struct {
	created      serverprovider.CreateInstanceRequest
	createdGPUs  []string
	result       serverprovider.Instance
	results      []serverprovider.Instance
	listed       []serverprovider.Instance
	deleted      []string
	deleteErrs   []error
	keys         []string
	replacedKeys [][]string
	err          error
	createErrors map[string]error
	replaceErr   error
	detailed     serverprovider.Instance
	detailErr    error
	detailCalls  int
}

type fakeVastDriver struct {
	*fakeProviderDriver
	offers      []vastdriver.Offer
	search      vastdriver.SearchOffersRequest
	searchError error
}

func (d *fakeVastDriver) SearchOffers(_ context.Context, _ serverprovider.Credentials, req vastdriver.SearchOffersRequest) ([]vastdriver.Offer, error) {
	d.search = req
	return append([]vastdriver.Offer(nil), d.offers...), d.searchError
}

func (d *fakeProviderDriver) GetInstance(_ context.Context, _ serverprovider.Credentials, _ string) (serverprovider.Instance, error) {
	d.detailCalls++
	if d.detailErr != nil {
		return serverprovider.Instance{}, d.detailErr
	}
	return d.detailed, nil
}

func (d *fakeProviderDriver) CreateInstance(_ context.Context, _ serverprovider.Credentials, req serverprovider.CreateInstanceRequest) (serverprovider.Instance, error) {
	d.created = req
	d.createdGPUs = append(d.createdGPUs, req.GPUModel)
	if err := d.createErrors[req.GPUModel]; err != nil {
		return serverprovider.Instance{}, err
	}
	if len(d.results) > 0 {
		result := d.results[0]
		d.results = d.results[1:]
		return result, d.err
	}
	return d.result, d.err
}
func (d *fakeProviderDriver) ListInstances(context.Context, serverprovider.Credentials) ([]serverprovider.Instance, error) {
	return d.listed, d.err
}
func (d *fakeProviderDriver) DeleteInstance(_ context.Context, _ serverprovider.Credentials, id string) error {
	d.deleted = append(d.deleted, id)
	if len(d.deleteErrs) > 0 {
		err := d.deleteErrs[0]
		d.deleteErrs = d.deleteErrs[1:]
		return err
	}
	return d.err
}
func (d *fakeProviderDriver) ListSSHKeys(context.Context, serverprovider.Credentials) ([]string, error) {
	return append([]string(nil), d.keys...), d.err
}
func (d *fakeProviderDriver) ReplaceSSHKeys(_ context.Context, _ serverprovider.Credentials, keys []string) error {
	d.replacedKeys = append(d.replacedKeys, append([]string(nil), keys...))
	if d.replaceErr != nil {
		return d.replaceErr
	}
	d.keys = append([]string(nil), keys...)
	return d.err
}
func (d *fakeProviderDriver) SSHAccess(_ context.Context, _ serverprovider.Credentials, _ string) (string, int, string, string, error) {
	return d.result.PublicIP, d.result.SSHPort, d.result.SSHUser, d.created.Extra["password"], d.err
}

func TestGenerateSSHKey(t *testing.T) {
	privateKey, publicKey, err := GenerateSSHKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ssh.ParsePrivateKey(privateKey); err != nil {
		t.Fatalf("parse private key: %v", err)
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey(publicKey); err != nil {
		t.Fatalf("parse authorized key: %v", err)
	}
}

func TestRunpodRentMarksInstanceAndReturnsSSHKey(t *testing.T) {
	driver := &fakeProviderDriver{result: serverprovider.Instance{
		ExternalID: "pod-1", Kind: "container", Region: "EU-RO-1",
		PublicIP: "203.0.113.4", SSHPort: 2244, SSHUser: "root",
	}}
	p, err := newRunpod(driver, serverprovider.Credentials{APIKey: "key"}, "run-123")
	if err != nil {
		t.Fatal(err)
	}
	instance, err := p.Rent(t.Context(), GPUSpec{GPUModel: "RTX4090", GPUCount: 1, DiskGB: 80})
	if err != nil {
		t.Fatal(err)
	}
	if instance.InstanceID != "pod-1" || instance.Kind != "container" || instance.Region != "EU-RO-1" ||
		instance.PublicIP != "203.0.113.4" || instance.SSHPort != 2244 || len(instance.PrivateKey) == 0 {
		t.Fatalf("instance = %+v", instance)
	}
	if driver.created.Name != TestNamePrefix+"run-123" || driver.created.GPUModel != "NVIDIA GeForce RTX 4090" {
		t.Fatalf("create request = %+v", driver.created)
	}
	if driver.created.Extra["env.GPUWORKS_TEST"] != "true" || driver.created.Extra["env.GPUWORKS_TEST_RUN_ID"] != "run-123" {
		t.Fatalf("create labels = %#v", driver.created.Extra)
	}
	if len(driver.replacedKeys) < 1 || !strings.HasPrefix(driver.replacedKeys[0][0], "ssh-ed25519 ") {
		t.Fatalf("SSH keys were not registered: %#v", driver.replacedKeys)
	}
	if err := p.Destroy(t.Context(), instance.InstanceID); err != nil {
		t.Fatal(err)
	}
	if len(driver.replacedKeys) < 2 || len(driver.replacedKeys[len(driver.replacedKeys)-1]) != 0 {
		t.Fatalf("SSH keys were not restored: %#v", driver.replacedKeys)
	}
}

func TestRunpodAutoPrefersFirstAllowedGPU(t *testing.T) {
	driver := &fakeProviderDriver{result: serverprovider.Instance{ExternalID: "pod-2000-ada", PublicIP: "203.0.113.40", SSHPort: 2244, SSHUser: "root"}}
	p, err := newRunpod(driver, serverprovider.Credentials{APIKey: "key"}, "run-auto")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Rent(t.Context(), GPUSpec{GPUModel: "auto"}); err != nil {
		t.Fatal(err)
	}
	if driver.created.GPUModel != runpodGPUAllowlist[0] {
		t.Fatalf("auto GPU request=%q, want %s", driver.created.GPUModel, runpodGPUAllowlist[0])
	}
}

func TestNormalizeRunpodInventoryGPUModels(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"PRO 6000 MIG 24GB", "NVIDIA RTX PRO 6000 Blackwell Server Edition MIG 1g.24gb"},
		{"NVIDIA RTX PRO 6000 Blackwell Server Edition MIG 1g.24gb", "NVIDIA RTX PRO 6000 Blackwell Server Edition MIG 1g.24gb"},
		{"RTX PRO 4000", "NVIDIA RTX PRO 4000 Blackwell"},
		{"RTX A5000", "NVIDIA RTX A5000"},
		{"RTX A6000", "NVIDIA RTX A6000"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := NormalizeGPUModel(tt.input); got != tt.want {
				t.Fatalf("NormalizeGPUModel(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestKnownGPUVRAMIncludesCompShareModels(t *testing.T) {
	for _, tt := range []struct {
		model string
		vram  int
	}{
		{model: "4090", vram: 24576},
		{model: "4090_48G", vram: 49152},
		{model: "5090", vram: 32768},
		{model: "3080Ti", vram: 12288},
		{model: "3090", vram: 24576},
		{model: "A100", vram: 40960},
		{model: "2080Ti", vram: 11264},
		{model: "V100S", vram: 32768},
		{model: "P40", vram: 24576},
		{model: "A800", vram: 81920},
		{model: "H20", vram: 98304},
	} {
		got, ok := gpuVRAMMB(tt.model)
		if !ok || got != tt.vram {
			t.Errorf("gpuVRAMMB(%q) = %d, %t; want %d, true", tt.model, got, ok, tt.vram)
		}
	}
}

func TestRunpodAutoFallsBackOnlyThroughAllowedGPUs(t *testing.T) {
	want := append([]string(nil), runpodGPUAllowlist...)
	driver := &fakeProviderDriver{
		result:       serverprovider.Instance{ExternalID: "pod-a6000", PublicIP: "203.0.113.49", SSHPort: 2244, SSHUser: "root"},
		createErrors: map[string]error{},
	}
	for _, model := range want[:len(want)-1] {
		driver.createErrors[model] = errors.New("no inventory")
	}
	p, err := newRunpod(driver, serverprovider.Credentials{APIKey: "key"}, "run-fallback")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Rent(t.Context(), GPUSpec{GPUModel: "auto"}); err != nil {
		t.Fatal(err)
	}
	if len(driver.createdGPUs) != len(want) {
		t.Fatalf("GPU requests = %v, want %v", driver.createdGPUs, want)
	}
	for i := range want {
		if driver.createdGPUs[i] != want[i] {
			t.Fatalf("GPU requests = %v, want %v", driver.createdGPUs, want)
		}
	}
}

func TestRunpodRentRefreshesDetailEndpoint(t *testing.T) {
	driver := &fakeProviderDriver{result: serverprovider.Instance{ExternalID: "pod-detail"}, detailed: serverprovider.Instance{ExternalID: "pod-detail", PublicIP: "203.0.113.44", SSHPort: 2244, SSHUser: "root", Power: serverprovider.PowerRunning}}
	p, err := newRunpod(driver, serverprovider.Credentials{APIKey: "key"}, "run-detail")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUNPOD_ENDPOINT_TIMEOUT", "1s")
	instance, err := p.Rent(t.Context(), GPUSpec{GPUModel: "RTX4090"})
	if err != nil {
		t.Fatal(err)
	}
	if instance.PublicIP != "203.0.113.44" || instance.SSHPort != 2244 {
		t.Fatalf("instance = %+v", instance)
	}
	if driver.detailCalls != 1 {
		t.Fatalf("detail calls = %d, want 1", driver.detailCalls)
	}
}

func TestRunpodRentFallsBackToListWhenDetailUnavailable(t *testing.T) {
	driver := &fakeProviderDriver{result: serverprovider.Instance{ExternalID: "pod-list"}, detailErr: errors.New("detail unavailable"), listed: []serverprovider.Instance{{ExternalID: "pod-list", PublicIP: "203.0.113.45", SSHPort: 2245, SSHUser: "root", Power: serverprovider.PowerRunning}}}
	p, err := newRunpod(driver, serverprovider.Credentials{APIKey: "key"}, "run-list")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUNPOD_ENDPOINT_TIMEOUT", "1s")
	instance, err := p.Rent(t.Context(), GPUSpec{GPUModel: "RTX4090"})
	if err != nil {
		t.Fatal(err)
	}
	if instance.PublicIP != "203.0.113.45" || instance.SSHPort != 2245 {
		t.Fatalf("instance = %+v", instance)
	}
	if driver.detailCalls != 1 {
		t.Fatalf("detail calls = %d, want 1", driver.detailCalls)
	}
}

func TestRunpodManualIsNonOwning(t *testing.T) {
	privateKey, _, err := GenerateSSHKey()
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewRunpodManual("manual-pod", "198.51.100.8", 2222, "root", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := p.Rent(t.Context(), GPUSpec{GPUModel: "auto"})
	if err != nil || instance.InstanceID != "manual-pod" {
		t.Fatalf("manual Rent=%+v err=%v", instance, err)
	}
	if err := p.Destroy(t.Context(), instance.InstanceID); err != nil {
		t.Fatal(err)
	}
	if leaked, err := p.ListLeaked(t.Context()); err != nil || len(leaked) != 0 {
		t.Fatalf("manual leaked=%v err=%v", leaked, err)
	}
}

func TestRunpodListLeakedAndDestroy(t *testing.T) {
	driver := &fakeProviderDriver{listed: []serverprovider.Instance{{ExternalID: "keep", Name: "production"}, {ExternalID: "leak-1", Name: TestNamePrefix + "old"}}}
	p, err := newRunpod(driver, serverprovider.Credentials{APIKey: "key"}, "run")
	if err != nil {
		t.Fatal(err)
	}
	leaked, err := p.ListLeaked(t.Context())
	if err != nil || len(leaked) != 1 || leaked[0] != "leak-1" {
		t.Fatalf("leaked=%v err=%v", leaked, err)
	}
	if err := p.Destroy(t.Context(), leaked[0]); err != nil {
		t.Fatal(err)
	}
	if len(driver.deleted) != 1 || driver.deleted[0] != "leak-1" {
		t.Fatalf("deleted = %v", driver.deleted)
	}
}

func TestRunpodListLeakedForRunIDIsIsolated(t *testing.T) {
	driver := &fakeProviderDriver{listed: []serverprovider.Instance{
		{ExternalID: "ours", Name: TestNamePrefix + "gitlab-1-20"},
		{ExternalID: "other-agent", Name: TestNamePrefix + "gitlab-1-21"},
		{ExternalID: "similar", Name: TestNamePrefix + "gitlab-1-20-suffix"},
		{ExternalID: "keep", Name: "production"},
	}}
	p, err := newRunpod(driver, serverprovider.Credentials{APIKey: "key"}, "gitlab-1-20")
	if err != nil {
		t.Fatal(err)
	}
	leaked, err := p.ListLeakedForRunID(t.Context(), "gitlab-1-20")
	if err != nil || len(leaked) != 1 || leaked[0] != "ours" {
		t.Fatalf("scoped leaked=%v err=%v", leaked, err)
	}
	if _, err := p.ListLeakedForRunID(t.Context(), ""); err == nil {
		t.Fatal("empty cleanup run ID was accepted")
	}
}

func TestCompshareListLeakedForRunIDIsIsolated(t *testing.T) {
	driver := &fakeProviderDriver{listed: scopedCleanupInventory()}
	p, err := newCompshare(driver, serverprovider.Credentials{PublicKey: "public", PrivateKey: "private"}, "", nil, "gitlab-1-20")
	if err != nil {
		t.Fatal(err)
	}
	assertScopedCleanup(t, p)
}

func TestRunpodRentCleansUpCreateFailureAfterAllocation(t *testing.T) {
	driver := &fakeProviderDriver{result: serverprovider.Instance{ExternalID: "pod-1"}, err: errors.New("create failed")}
	p, _ := newRunpod(driver, serverprovider.Credentials{APIKey: "key"}, "run")
	if _, err := p.Rent(t.Context(), GPUSpec{GPUModel: "RTX4090"}); err == nil {
		t.Fatal("Rent succeeded unexpectedly")
	}
	if len(driver.deleted) != 0 {
		t.Fatalf("delete called for a failed create: %v", driver.deleted)
	}
}

func TestRunpodRentValidatesMinimumVRAM(t *testing.T) {
	driver := &fakeProviderDriver{result: serverprovider.Instance{ExternalID: "pod-1", PublicIP: "203.0.113.4", SSHPort: 22}}
	p, _ := newRunpod(driver, serverprovider.Credentials{APIKey: "key"}, "run")
	if _, err := p.Rent(t.Context(), GPUSpec{GPUModel: "RTX4090", MinVRAMMB: 32768}); err == nil {
		t.Fatal("Rent accepted a GPU below MinVRAMMB")
	}
	if len(driver.replacedKeys) != 0 {
		t.Fatalf("SSH keys changed before VRAM validation: %#v", driver.replacedKeys)
	}
}

func TestRunpodDestroyKeepsKeyStateWhenRestoreFails(t *testing.T) {
	driver := &fakeProviderDriver{result: serverprovider.Instance{ExternalID: "pod-1", PublicIP: "203.0.113.4", SSHPort: 22}}
	p, _ := newRunpod(driver, serverprovider.Credentials{APIKey: "key"}, "run")
	instance, err := p.Rent(t.Context(), GPUSpec{GPUModel: "RTX4090"})
	if err != nil {
		t.Fatal(err)
	}
	driver.replaceErr = errors.New("key API unavailable")
	if err := p.Destroy(t.Context(), instance.InstanceID); err == nil {
		t.Fatal("Destroy succeeded despite SSH key restore failure")
	}
	driver.replaceErr = nil
	if err := p.Destroy(t.Context(), instance.InstanceID); err != nil {
		t.Fatalf("Destroy retry failed: %v", err)
	}
	if len(driver.replacedKeys) < 3 || len(driver.replacedKeys[len(driver.replacedKeys)-1]) != 0 {
		t.Fatalf("SSH key was not restored on retry: %#v", driver.replacedKeys)
	}
}

func TestRunpodDestroyPreservesExternallyAddedSSHKey(t *testing.T) {
	driver := &fakeProviderDriver{keys: []string{"ssh-ed25519 original"}, result: serverprovider.Instance{ExternalID: "pod-1", PublicIP: "203.0.113.4", SSHPort: 22}}
	p, _ := newRunpod(driver, serverprovider.Credentials{APIKey: "key"}, "run")
	instance, err := p.Rent(t.Context(), GPUSpec{GPUModel: "RTX4090"})
	if err != nil {
		t.Fatal(err)
	}
	driver.keys = append(driver.keys, "ssh-ed25519 externally-added")
	if err := p.Destroy(t.Context(), instance.InstanceID); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"ssh-ed25519 original": true, "ssh-ed25519 externally-added": true}
	if len(driver.keys) != len(want) {
		t.Fatalf("keys=%v", driver.keys)
	}
	for _, key := range driver.keys {
		if !want[key] {
			t.Fatalf("unexpected key %q in %v", key, driver.keys)
		}
	}
}

func TestCompshareRentMarksInstanceAndReturnsPassword(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	driver := &fakeProviderDriver{result: serverprovider.Instance{ExternalID: "uhost-1", GPUModel: "4090", GPUCount: 1, Kind: "vm", PublicIP: "203.0.113.9", PrivateIP: "10.0.0.9", SSHPort: 60022, SSHUser: "root"}}
	p, err := newCompshare(driver, serverprovider.Credentials{PublicKey: "public", PrivateKey: "private"}, "compshareImage-test", nil, "run-123")
	if err != nil {
		t.Fatal(err)
	}
	instance, err := p.Rent(t.Context(), GPUSpec{GPUModel: "RTX4090", GPUCount: 1, MinVRAMMB: 24576, DiskGB: 120})
	if err != nil {
		t.Fatal(err)
	}
	if instance.InstanceID != "uhost-1" || instance.SSHPort != 60022 || instance.Password == "" || len(instance.PrivateKey) != 0 {
		t.Fatalf("instance=%+v", instance)
	}
	if instance.GPUModel != "4090" || instance.GPUCount != 1 || instance.Kind != "vm" || instance.PrivateIP != "10.0.0.9" {
		t.Fatalf("instance metadata=%+v", instance)
	}
	if driver.created.Name != TestNamePrefix+"run-123" || driver.created.Image != "compshareImage-test" || driver.created.Extra["disk_gb"] != "120" || driver.created.Extra["password"] != instance.Password {
		t.Fatalf("create request=%+v", driver.created)
	}
	if err := p.Destroy(t.Context(), instance.InstanceID); err != nil {
		t.Fatal(err)
	}
	if len(driver.deleted) != 1 || driver.deleted[0] != "uhost-1" {
		t.Fatalf("deleted=%v", driver.deleted)
	}
	if _, err := os.Stat(compshareLedgerPath()); !os.IsNotExist(err) {
		t.Fatalf("ledger still exists after destroy: %v", err)
	}
}

func TestCompshareCleanupDoesNotRequireImage(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	driver := &fakeProviderDriver{listed: []serverprovider.Instance{{ExternalID: "leak-1", Name: TestNamePrefix + "old"}}}
	p, err := newCompshare(driver, serverprovider.Credentials{PublicKey: "public", PrivateKey: "private"}, "", nil, "cleanup")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Rent(t.Context(), GPUSpec{GPUModel: "RTX4090"}); err == nil {
		t.Fatal("Rent accepted missing image id")
	}
	leaked, err := p.ListLeaked(t.Context())
	if err != nil || len(leaked) != 1 || leaked[0] != "leak-1" {
		t.Fatalf("leaked=%v err=%v", leaked, err)
	}
}

func TestCompshareDestroyRetriesTransientAPIError(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	driver := &fakeProviderDriver{deleteErrs: []error{errors.New("compshare: request failed: EOF")}}
	p, err := newCompshare(driver, serverprovider.Credentials{PublicKey: "public", PrivateKey: "private"}, "", nil, "cleanup")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Destroy(t.Context(), "uhost-transient"); err != nil {
		t.Fatal(err)
	}
	if len(driver.deleted) != 2 {
		t.Fatalf("delete attempts=%v, want two attempts", driver.deleted)
	}
}

func TestCompshareLedgerAddsUnknownLeakedInstance(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	if err := addCompshareLedger("ledger-leak"); err != nil {
		t.Fatal(err)
	}
	driver := &fakeProviderDriver{}
	p, _ := newCompshare(driver, serverprovider.Credentials{PublicKey: "public", PrivateKey: "private"}, "", nil, "cleanup")
	leaked, err := p.ListLeaked(t.Context())
	if err != nil || len(leaked) != 1 || leaked[0] != "ledger-leak" {
		t.Fatalf("leaked=%v err=%v", leaked, err)
	}
	if err := p.Destroy(t.Context(), "ledger-leak"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(compshareLedgerPath()); !os.IsNotExist(err) {
		t.Fatalf("ledger still exists after cleanup: %v", err)
	}
}

func scopedCleanupInventory() []serverprovider.Instance {
	return []serverprovider.Instance{
		{ExternalID: "ours", Name: TestNamePrefix + "gitlab-1-20"},
		{ExternalID: "other-agent", Name: TestNamePrefix + "gitlab-1-21"},
		{ExternalID: "similar", Name: TestNamePrefix + "gitlab-1-20-suffix"},
		{ExternalID: "keep", Name: "production"},
	}
}

func assertScopedCleanup(t *testing.T, lister ScopedLeakLister) {
	t.Helper()
	leaked, err := lister.ListLeakedForRunID(t.Context(), "gitlab-1-20")
	if err != nil || len(leaked) != 1 || leaked[0] != "ours" {
		t.Fatalf("scoped leaked=%v err=%v", leaked, err)
	}
	if _, err := lister.ListLeakedForRunID(t.Context(), ""); err == nil {
		t.Fatal("empty cleanup run ID was accepted")
	}
}
