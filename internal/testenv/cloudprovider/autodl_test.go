package cloudprovider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
	autodldriver "gpuworks.ai/gpuworks/internal/server/provider/autodl"
)

type fakeAutoDLDriver struct {
	created     serverprovider.CreateInstanceRequest
	create      serverprovider.Instance
	snapshot    autodldriver.Snapshot
	listed      []serverprovider.Instance
	deleted     []string
	deleteErr   error
	snapshotErr error
	// createErrs refuses a create by GPU model; tried records every attempt.
	createErrs map[string]error
	tried      []string
}

func (d *fakeAutoDLDriver) ListInstances(context.Context, serverprovider.Credentials) ([]serverprovider.Instance, error) {
	return d.listed, nil
}

func (d *fakeAutoDLDriver) CreateInstance(_ context.Context, _ serverprovider.Credentials, req serverprovider.CreateInstanceRequest) (serverprovider.Instance, error) {
	d.created = req
	d.tried = append(d.tried, req.GPUModel)
	if err := d.createErrs[req.GPUModel]; err != nil {
		return serverprovider.Instance{}, err
	}
	return d.create, nil
}

func (d *fakeAutoDLDriver) DeleteInstance(_ context.Context, _ serverprovider.Credentials, id string) error {
	d.deleted = append(d.deleted, id)
	return d.deleteErr
}

func (d *fakeAutoDLDriver) Snapshot(context.Context, serverprovider.Credentials, string) (autodldriver.Snapshot, error) {
	return d.snapshot, d.snapshotErr
}

func TestAutoDLRentReturnsSnapshotSSHAndCleansUp(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	driver := &fakeAutoDLDriver{
		create: serverprovider.Instance{ExternalID: "pro-test"},
		snapshot: autodldriver.Snapshot{Instance: serverprovider.Instance{
			ExternalID: "pro-test", GPUModel: "NVIDIA RTX 4080S", GPUCount: 1, PricePerHr: 1.97,
			Region: "westDC3", PublicIP: "connect.example", SSHPort: 34222, SSHUser: "root",
		}, RootPassword: "password"},
	}
	p, err := newAutoDL(driver, serverprovider.Credentials{APIKey: "token"}, "run", "base-image", 5)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := p.Rent(t.Context(), GPUSpec{GPUModel: "auto", GPUCount: 1, MinVRAMMB: 12000, DiskGB: 20})
	if err != nil {
		t.Fatal(err)
	}
	if instance.InstanceID != "pro-test" || instance.PublicIP != "connect.example" || instance.SSHPort != 34222 || instance.Password != "password" || instance.Kind != "container" {
		t.Fatalf("instance = %+v", instance)
	}
	if driver.created.Name != TestNamePrefix+"run" || driver.created.GPUModel != autodlDefaultGPUModel || driver.created.Image != "base-image" || driver.created.Extra["expand_system_disk_by_gb"] != "20" {
		t.Fatalf("create request = %+v", driver.created)
	}
	if err := p.Destroy(t.Context(), instance.InstanceID); err != nil {
		t.Fatal(err)
	}
	if len(driver.deleted) != 1 || driver.deleted[0] != "pro-test" {
		t.Fatalf("deleted = %v", driver.deleted)
	}
	if _, err := os.Stat(autodlLedgerPath()); !os.IsNotExist(err) {
		t.Fatalf("ledger still exists: %v", err)
	}
}

func TestAutoDLRentDestroysInstanceAbovePriceLimit(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	driver := &fakeAutoDLDriver{
		create: serverprovider.Instance{ExternalID: "pro-expensive"},
		snapshot: autodldriver.Snapshot{Instance: serverprovider.Instance{
			ExternalID: "pro-expensive", PricePerHr: 5.01, PublicIP: "connect.example", SSHPort: 34222,
		}, RootPassword: "password"},
	}
	p, _ := newAutoDL(driver, serverprovider.Credentials{APIKey: "token"}, "run", "base-image", 5)
	if _, err := p.Rent(t.Context(), GPUSpec{GPUModel: "RTX 4080S"}); err == nil || !strings.Contains(err.Error(), "exceeds configured maximum") {
		t.Fatalf("Rent error = %v", err)
	}
	if len(driver.deleted) != 1 || driver.deleted[0] != "pro-expensive" {
		t.Fatalf("cleanup = %v", driver.deleted)
	}
}

func TestAutoDLListLeakedIncludesLedger(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	if err := addAutoDLLedger("ledger-only"); err != nil {
		t.Fatal(err)
	}
	driver := &fakeAutoDLDriver{listed: []serverprovider.Instance{
		{ExternalID: "keep", Name: "production"}, {ExternalID: "cloud-leak", Name: TestNamePrefix + "old"},
	}}
	p, _ := newAutoDL(driver, serverprovider.Credentials{APIKey: "token"}, "run", "base-image", 5)
	leaked, err := p.ListLeaked(t.Context())
	if err != nil || len(leaked) != 2 || leaked[0] != "cloud-leak" || leaked[1] != "ledger-only" {
		t.Fatalf("leaked=%v err=%v", leaked, err)
	}
}

func TestAutoDLListLeakedForRunIDIsIsolated(t *testing.T) {
	driver := &fakeAutoDLDriver{listed: scopedCleanupInventory()}
	p, err := newAutoDL(driver, serverprovider.Credentials{APIKey: "token"}, "gitlab-1-20", "base-image", 5)
	if err != nil {
		t.Fatal(err)
	}
	assertScopedCleanup(t, p)
}

func TestAutoDLDestroyKeepsLedgerOnFailure(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	if err := addAutoDLLedger("pro-failed"); err != nil {
		t.Fatal(err)
	}
	driver := &fakeAutoDLDriver{deleteErr: errors.New("release failed")}
	p, _ := newAutoDL(driver, serverprovider.Credentials{APIKey: "token"}, "run", "base-image", 5)
	if err := p.Destroy(t.Context(), "pro-failed"); err == nil {
		t.Fatal("Destroy succeeded unexpectedly")
	}
	ledger, err := readAutoDLLedger()
	if err != nil || len(ledger) != 1 || ledger[0] != "pro-failed" {
		t.Fatalf("ledger=%v err=%v", ledger, err)
	}
}

func TestAutoDLRentFallsBackWhenSpecIsOutOfStock(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	driver := &fakeAutoDLDriver{
		createErrs: map[string]error{
			autodlDefaultGPUModel: fmt.Errorf("api error code=InternalError msg=当前算力规格暂无库存, 请修改配置或稍等再试: %w", autodldriver.ErrOutOfStock),
			"RTX 4090D":           fmt.Errorf("api error code=InternalError msg=当前算力规格暂无库存, 请修改配置或稍等再试: %w", autodldriver.ErrOutOfStock),
		},
		create: serverprovider.Instance{ExternalID: "pro-fallback"},
		snapshot: autodldriver.Snapshot{Instance: serverprovider.Instance{
			ExternalID: "pro-fallback", GPUModel: "NVIDIA RTX 4090", GPUCount: 1, PricePerHr: 2.5,
			Region: "westDC3", PublicIP: "connect.example", SSHPort: 34222, SSHUser: "root",
		}, RootPassword: "password"},
	}
	p, _ := newAutoDL(driver, serverprovider.Credentials{APIKey: "token"}, "run", "base-image", 5)
	instance, err := p.Rent(t.Context(), GPUSpec{GPUModel: "auto", GPUCount: 1, MinVRAMMB: 12000})
	if err != nil {
		t.Fatal(err)
	}
	if instance.InstanceID != "pro-fallback" || driver.created.GPUModel != "RTX 4090" {
		t.Fatalf("instance=%+v create request=%+v", instance, driver.created)
	}
	if got := strings.Join(driver.tried, ","); got != autodlDefaultGPUModel+",RTX 4090D,RTX 4090" {
		t.Fatalf("tried = %s", got)
	}
}

func TestAutoDLRentNeverSwapsANamedCardOrMasksOtherErrors(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	stock := fmt.Errorf("api error code=InternalError msg=当前算力规格暂无库存, 请修改配置或稍等再试: %w", autodldriver.ErrOutOfStock)
	driver := &fakeAutoDLDriver{createErrs: map[string]error{autodlDefaultGPUModel: stock}}
	p, _ := newAutoDL(driver, serverprovider.Credentials{APIKey: "token"}, "run", "base-image", 5)
	if _, err := p.Rent(t.Context(), GPUSpec{GPUModel: autodlDefaultGPUModel}); !errors.Is(err, stock) || len(driver.tried) != 1 {
		t.Fatalf("named card: err=%v tried=%v", err, driver.tried)
	}
	// A refusal that is not about stock stops the walk at once.
	denied := errors.New("api error code=InternalError msg=账户余额不足")
	driver = &fakeAutoDLDriver{createErrs: map[string]error{autodlDefaultGPUModel: stock, "RTX 4090D": denied}}
	p, _ = newAutoDL(driver, serverprovider.Credentials{APIKey: "token"}, "run", "base-image", 5)
	if _, err := p.Rent(t.Context(), GPUSpec{GPUModel: "auto"}); !errors.Is(err, denied) || !strings.Contains(err.Error(), autodlDefaultGPUModel) || len(driver.tried) != 2 {
		t.Fatalf("other error: err=%v tried=%v", err, driver.tried)
	}
	// Every spec out of stock reports all of them.
	driver = &fakeAutoDLDriver{createErrs: map[string]error{}}
	for _, m := range autodlAutoGPUModels {
		driver.createErrs[m] = stock
	}
	p, _ = newAutoDL(driver, serverprovider.Credentials{APIKey: "token"}, "run", "base-image", 5)
	if _, err := p.Rent(t.Context(), GPUSpec{GPUModel: "auto"}); err == nil || len(driver.tried) != len(autodlAutoGPUModels) || !strings.Contains(err.Error(), "RTX 5090") {
		t.Fatalf("all out of stock: err=%v tried=%v", err, driver.tried)
	}
	// A VRAM floor skips smaller specs instead of failing the walk.
	driver = &fakeAutoDLDriver{createErrs: map[string]error{autodlDefaultGPUModel: stock}}
	p, _ = newAutoDL(driver, serverprovider.Credentials{APIKey: "token"}, "run", "base-image", 5)
	_, _ = p.Rent(t.Context(), GPUSpec{GPUModel: "auto", MinVRAMMB: 30000})
	if got := strings.Join(driver.tried, ","); got != autodlDefaultGPUModel+",RTX 5090" {
		t.Fatalf("VRAM floor tried = %s", got)
	}
}

// A candidate whose VRAM is unknown is skipped as "cannot verify" whenever a
// case sets MinVRAMMB, so every spec the walk tries must have an entry.
func TestAutoDLAutoGPUModelsHaveKnownVRAM(t *testing.T) {
	for _, model := range autodlAutoGPUModels {
		if mb, ok := gpuVRAMMB(model); !ok || mb <= 0 {
			t.Errorf("gpuVRAMMB(%q) = %d, %v; want a known size", model, mb, ok)
		}
	}
}
