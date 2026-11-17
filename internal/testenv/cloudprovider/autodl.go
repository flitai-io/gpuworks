package cloudprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
	autodldriver "gpuworks.ai/gpuworks/internal/server/provider/autodl"
)

const (
	autodlRentTimeout      = 8 * time.Minute
	autodlEndpointTimeout  = 5 * time.Minute
	autodlDestroyTimeout   = 3 * time.Minute
	autodlDefaultMaxPrice  = 10.0 // CNY/hour; snapshot payg_price is milli-CNY/hour.
	autodlDefaultImageUUID = "base-image-l2t43iu6uk"
	autodlDefaultGPUModel  = "RTX 4080S"
)

// autodlAutoGPUModels are the specs an unpinned Rent tries, cheapest first;
// the hourly price cap still applies to whichever one is created. PRO6000
// (pro6000-p) and H800 (h800) close the walk: they are the dearest Pro specs
// (about 7 and 10 CNY/h, at the cap), so they are tried only when every
// cheaper one is sold out; the price check after the create still releases
// one that comes in above the cap.
var autodlAutoGPUModels = []string{autodlDefaultGPUModel, "RTX 4090D", "RTX 4090", "RTX 3090", "RTX 5090", "PRO6000", "H800"}

var autodlLedgerMu sync.Mutex

// isAutoDLOutOfStock reports the create refusal AutoDL answers with when a
// spec has no free machine ("当前算力规格暂无库存, 请修改配置或稍等再试").
func isAutoDLOutOfStock(err error) bool {
	return errors.Is(err, autodldriver.ErrOutOfStock)
}

type autodlLifecycleDriver interface {
	ListInstances(context.Context, serverprovider.Credentials) ([]serverprovider.Instance, error)
	CreateInstance(context.Context, serverprovider.Credentials, serverprovider.CreateInstanceRequest) (serverprovider.Instance, error)
	DeleteInstance(context.Context, serverprovider.Credentials, string) error
	Snapshot(context.Context, serverprovider.Credentials, string) (autodldriver.Snapshot, error)
}

// AutoDL rents Pro container instances for L4 acceptance tests.
type AutoDL struct {
	driver   autodlLifecycleDriver
	creds    serverprovider.Credentials
	runID    string
	image    string
	maxPrice float64
	// stockWait is the budget for retrying after every spec is out of stock;
	// zero (the default of newAutoDL) fails on the first walk.
	stockWait StockWait
}

func NewAutoDL(token, runID string) (*AutoDL, error) {
	image := strings.TrimSpace(os.Getenv("AUTODL_IMAGE_UUID"))
	if image == "" {
		image = autodlDefaultImageUUID
	}
	maxPrice := autodlDefaultMaxPrice
	if raw := strings.TrimSpace(os.Getenv("AUTODL_MAX_HOURLY_PRICE")); raw != "" {
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || value <= 0 {
			return nil, fmt.Errorf("autodl: invalid AUTODL_MAX_HOURLY_PRICE %q", raw)
		}
		maxPrice = value
	}
	wait, err := StockWaitFromEnv("AUTODL_STOCK_WAIT", DefaultStockWait)
	if err != nil {
		return nil, fmt.Errorf("autodl: %w", err)
	}
	p, err := newAutoDL(autodldriver.New(), serverprovider.Credentials{APIKey: strings.TrimSpace(token)}, runID, image, maxPrice)
	if err != nil {
		return nil, err
	}
	p.stockWait = wait
	return p, nil
}

func newAutoDL(driver autodlLifecycleDriver, creds serverprovider.Credentials, runID, image string, maxPrice float64) (*AutoDL, error) {
	if strings.TrimSpace(creds.APIKey) == "" {
		return nil, fmt.Errorf("AutoDL token is required")
	}
	if strings.TrimSpace(runID) == "" {
		var err error
		runID, err = NewRunID()
		if err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(image) == "" {
		return nil, fmt.Errorf("AutoDL image UUID is required")
	}
	if maxPrice <= 0 {
		return nil, fmt.Errorf("AutoDL maximum hourly price must be positive")
	}
	return &AutoDL{driver: driver, creds: creds, runID: runID, image: strings.TrimSpace(image), maxPrice: maxPrice}, nil
}

func (p *AutoDL) Name() string { return "autodl" }

func (p *AutoDL) ManagementCredentials() serverprovider.Credentials { return p.creds }

// errAutoDLNoSpecInStock marks the aggregated refusal after every candidate
// spec reported 暂无库存. Only this error is waited out and retried: nothing was
// created, so no instance exists while waiting.
var errAutoDLNoSpecInStock = errors.New("autodl: no GPU spec could be rented")

// StockWait is how long Rent keeps retrying after every candidate spec is out
// of stock (see AUTODL_STOCK_WAIT); zero means it fails on the first walk.
func (p *AutoDL) StockWait() time.Duration { return p.stockWait.Max }

type autodlCreated struct {
	instance serverprovider.Instance
	gpuModel string
}

func (p *AutoDL) Rent(ctx context.Context, spec GPUSpec) (*Instance, error) {
	requested := strings.TrimSpace(spec.GPUModel)
	candidates := []string{requested}
	if requested == "" || strings.EqualFold(requested, "auto") {
		// No card was asked for: AutoDL's stock of any one spec comes and goes
		// during the day, so walk the specs this suite can use when a create
		// is refused for stock. A card the caller named is never swapped.
		candidates = autodlAutoGPUModels
	}
	gpuCount := spec.GPUCount
	if gpuCount <= 0 {
		gpuCount = 1
	}
	extraDisk := spec.DiskGB
	if extraDisk < 0 {
		extraDisk = 0
	}
	wait := p.stockWait
	if len(candidates) < 2 {
		wait.Max = 0 // a named card is neither swapped nor waited for
	}
	// The stock wait runs on the caller's context; the rent budget below only
	// starts once a create succeeded.
	won, err := RetryOnStockOut(ctx, wait, "autodl", func(err error) bool { return errors.Is(err, errAutoDLNoSpecInStock) },
		func(ctx context.Context) (autodlCreated, error) {
			walkCtx, cancel := withMaxTimeout(ctx, autodlRentTimeout)
			defer cancel()
			return p.createFirstInStock(walkCtx, spec, candidates, gpuCount, extraDisk)
		})
	if err != nil {
		return nil, err
	}
	created, gpuModel := won.instance, won.gpuModel
	ctx, cancel := withMaxTimeout(ctx, autodlRentTimeout)
	defer cancel()
	if strings.TrimSpace(created.ExternalID) == "" {
		return nil, fmt.Errorf("autodl: create returned an empty instance id")
	}
	if err := addAutoDLLedger(created.ExternalID); err != nil {
		_ = p.driver.DeleteInstance(context.Background(), p.creds, created.ExternalID)
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), autodlDestroyTimeout)
			defer cleanupCancel()
			_ = p.Destroy(cleanupCtx, created.ExternalID)
		}
	}()
	endpointCtx, endpointCancel := withMaxTimeout(ctx, autodlEndpointTimeout)
	defer endpointCancel()
	var snapshot autodldriver.Snapshot
	var lastErr error
	for {
		snapshot, lastErr = p.driver.Snapshot(endpointCtx, p.creds, created.ExternalID)
		if lastErr == nil && snapshot.Instance.PublicIP != "" && snapshot.Instance.SSHPort > 0 && snapshot.RootPassword != "" {
			break
		}
		select {
		case <-endpointCtx.Done():
			return nil, fmt.Errorf("wait for AutoDL SSH endpoint: %w (instance=%s last_error=%v)", endpointCtx.Err(), created.ExternalID, lastErr)
		case <-time.After(5 * time.Second):
		}
	}
	if snapshot.Instance.PricePerHr <= 0 {
		return nil, fmt.Errorf("autodl: instance %s did not report a positive hourly price", created.ExternalID)
	}
	if snapshot.Instance.PricePerHr > p.maxPrice {
		return nil, fmt.Errorf("autodl: instance hourly price %.3f CNY exceeds configured maximum %.3f CNY", snapshot.Instance.PricePerHr, p.maxPrice)
	}
	failed = false
	return &Instance{
		InstanceID: created.ExternalID, GPUModel: firstNonEmpty(snapshot.Instance.GPUModel, gpuModel),
		GPUCount: firstPositive(snapshot.Instance.GPUCount, gpuCount), Kind: "container", Region: snapshot.Instance.Region,
		PublicIP: snapshot.Instance.PublicIP, SSHPort: snapshot.Instance.SSHPort,
		Username: firstNonEmpty(snapshot.Instance.SSHUser, "root"), Password: snapshot.RootPassword,
	}, nil
}

// createFirstInStock walks the candidate specs once and creates the first one
// AutoDL has stock for. It creates nothing when it returns an error.
func (p *AutoDL) createFirstInStock(ctx context.Context, spec GPUSpec, candidates []string, gpuCount, extraDisk int) (autodlCreated, error) {
	var outOfStock []string // candidates passed over, with why
	sawStockOut := false
	for _, candidate := range candidates {
		if spec.MinVRAMMB > 0 {
			known, ok := gpuVRAMMB(candidate)
			var reason error
			switch {
			case !ok:
				reason = fmt.Errorf("autodl: cannot verify MinVRAMMB=%d for GPU %q", spec.MinVRAMMB, candidate)
			case known < spec.MinVRAMMB:
				reason = fmt.Errorf("autodl: GPU %q has %d MiB VRAM, below requested %d MiB", candidate, known, spec.MinVRAMMB)
			}
			if reason != nil {
				if len(candidates) > 1 {
					outOfStock = append(outOfStock, reason.Error())
					continue
				}
				return autodlCreated{}, reason
			}
		}
		instance, err := p.driver.CreateInstance(ctx, p.creds, serverprovider.CreateInstanceRequest{
			Name: TestNamePrefix + p.runID, GPUModel: candidate, GPUCount: gpuCount, Image: p.image,
			Extra: map[string]string{
				"expand_system_disk_by_gb": strconv.Itoa(extraDisk),
				"cuda_v_from":              "113",
				"start_command":            "sleep 1",
			},
		})
		if err != nil {
			if len(candidates) > 1 && isAutoDLOutOfStock(err) {
				sawStockOut = true
				outOfStock = append(outOfStock, candidate+": "+err.Error())
				continue
			}
			if len(outOfStock) > 0 {
				return autodlCreated{}, fmt.Errorf("%w (out of stock before: %s)", err, strings.Join(outOfStock, "; "))
			}
			return autodlCreated{}, err
		}
		return autodlCreated{instance: instance, gpuModel: candidate}, nil
	}
	if sawStockOut {
		return autodlCreated{}, fmt.Errorf("%w: %s", errAutoDLNoSpecInStock, strings.Join(outOfStock, "; "))
	}
	return autodlCreated{}, fmt.Errorf("autodl: no GPU spec could be rented: %s", strings.Join(outOfStock, "; "))
}

func (p *AutoDL) Destroy(ctx context.Context, instanceID string) error {
	ctx, cancel := withMaxTimeout(ctx, autodlDestroyTimeout)
	defer cancel()
	if err := p.driver.DeleteInstance(ctx, p.creds, instanceID); err != nil {
		return err
	}
	return removeAutoDLLedger(instanceID)
}

func (p *AutoDL) ListLeaked(ctx context.Context) ([]string, error) {
	instances, err := p.driver.ListInstances(ctx, p.creds)
	if err != nil {
		return nil, err
	}
	var leaked []string
	seen := map[string]struct{}{}
	for _, instance := range instances {
		if strings.HasPrefix(instance.Name, TestNamePrefix) {
			leaked = append(leaked, instance.ExternalID)
			seen[instance.ExternalID] = struct{}{}
		}
	}
	ledger, err := readAutoDLLedger()
	if err != nil {
		return nil, err
	}
	for _, id := range ledger {
		if _, ok := seen[id]; !ok {
			leaked = append(leaked, id)
		}
	}
	return leaked, nil
}

func (p *AutoDL) ListLeakedForRunID(ctx context.Context, runID string) ([]string, error) {
	instances, err := p.driver.ListInstances(ctx, p.creds)
	if err != nil {
		return nil, err
	}
	return leakedInstanceIDsForRun(p.Name(), runID, instances)
}

func autodlLedgerPath() string {
	return filepath.Join(os.TempDir(), "gpuworks-test-autodl-instances.json")
}

func readAutoDLLedger() ([]string, error) {
	autodlLedgerMu.Lock()
	defer autodlLedgerMu.Unlock()
	return readAutoDLLedgerLocked()
}

func readAutoDLLedgerLocked() ([]string, error) {
	data, err := os.ReadFile(autodlLedgerPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read AutoDL test ledger: %w", err)
	}
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil {
		return nil, fmt.Errorf("decode AutoDL test ledger: %w", err)
	}
	return ids, nil
}

func addAutoDLLedger(id string) error {
	autodlLedgerMu.Lock()
	defer autodlLedgerMu.Unlock()
	ids, err := readAutoDLLedgerLocked()
	if err != nil {
		return err
	}
	for _, existing := range ids {
		if existing == id {
			return nil
		}
	}
	return writeAutoDLLedgerLocked(append(ids, id))
}

func removeAutoDLLedger(id string) error {
	autodlLedgerMu.Lock()
	defer autodlLedgerMu.Unlock()
	ids, err := readAutoDLLedgerLocked()
	if err != nil {
		return err
	}
	filtered := ids[:0]
	for _, existing := range ids {
		if existing != id {
			filtered = append(filtered, existing)
		}
	}
	return writeAutoDLLedgerLocked(filtered)
}

func writeAutoDLLedgerLocked(ids []string) error {
	if len(ids) == 0 {
		if err := os.Remove(autodlLedgerPath()); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove AutoDL test ledger: %w", err)
		}
		return nil
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	if err := os.WriteFile(autodlLedgerPath(), data, 0o600); err != nil {
		return fmt.Errorf("write AutoDL test ledger: %w", err)
	}
	return nil
}
