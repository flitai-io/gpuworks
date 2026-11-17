package cloudprovider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
	autodldriver "gpuworks.ai/gpuworks/internal/server/provider/autodl"
)

// fakeStockClock drives StockWait without real sleeping: Sleep advances the
// clock and records the pause, so rounds, intervals and the deadline are exact.
type fakeStockClock struct {
	now    time.Time
	sleeps []time.Duration
	logs   []string
	// onSleep runs before the clock moves, to change stock or cancel ctx.
	onSleep func(round int)
}

func (c *fakeStockClock) wait(limit, interval time.Duration) StockWait {
	return StockWait{
		Max: limit, Interval: interval,
		Now: func() time.Time { return c.now },
		Sleep: func(ctx context.Context, d time.Duration) error {
			c.sleeps = append(c.sleeps, d)
			if c.onSleep != nil {
				c.onSleep(len(c.sleeps))
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			c.now = c.now.Add(d)
			return nil
		},
		Logf: func(format string, args ...any) { c.logs = append(c.logs, fmt.Sprintf(format, args...)) },
	}
}

var autodlStockErr = fmt.Errorf("api error code=InternalError msg=当前算力规格暂无库存, 请修改配置或稍等再试: %w", autodldriver.ErrOutOfStock)

func allAutoDLSpecsOutOfStock() map[string]error {
	errs := map[string]error{}
	for _, m := range autodlAutoGPUModels {
		errs[m] = autodlStockErr
	}
	return errs
}

func TestAutoDLRentWaitsOutStockOutAndRetriesEverySpec(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	driver := &fakeAutoDLDriver{
		createErrs: allAutoDLSpecsOutOfStock(),
		create:     serverprovider.Instance{ExternalID: "pro-restocked"},
		snapshot: autodldriver.Snapshot{Instance: serverprovider.Instance{
			ExternalID: "pro-restocked", GPUModel: "NVIDIA RTX 4090", GPUCount: 1, PricePerHr: 2.5,
			Region: "westDC3", PublicIP: "connect.example", SSHPort: 34222, SSHUser: "root",
		}, RootPassword: "password"},
	}
	clock := &fakeStockClock{now: time.Unix(0, 0)}
	// Stock returns for RTX 4090 after the second wait.
	clock.onSleep = func(round int) {
		if round == 2 {
			delete(driver.createErrs, "RTX 4090")
		}
	}
	p, _ := newAutoDL(driver, serverprovider.Credentials{APIKey: "token"}, "run", "base-image", 5)
	p.stockWait = clock.wait(30*time.Minute, 5*time.Minute)
	instance, err := p.Rent(t.Context(), GPUSpec{GPUModel: "auto", GPUCount: 1, MinVRAMMB: 12000})
	if err != nil {
		t.Fatal(err)
	}
	if instance.InstanceID != "pro-restocked" || driver.created.GPUModel != "RTX 4090" {
		t.Fatalf("instance=%+v create=%+v", instance, driver.created)
	}
	// Three walks over every spec in order; the last one stops at the restocked spec.
	walk := strings.Join(autodlAutoGPUModels, ",")
	if got, want := strings.Join(driver.tried, ","), walk+","+walk+","+strings.Join(autodlAutoGPUModels[:3], ","); got != want {
		t.Fatalf("tried = %s, want %s", got, want)
	}
	if fmt.Sprint(clock.sleeps) != fmt.Sprint([]time.Duration{5 * time.Minute, 5 * time.Minute}) {
		t.Fatalf("sleeps = %v", clock.sleeps)
	}
	if len(clock.logs) != 2 || !strings.Contains(clock.logs[0], "round 1") || !strings.Contains(clock.logs[1], "round 2") {
		t.Fatalf("each waiting round must be logged: %q", clock.logs)
	}
	if len(driver.deleted) != 0 {
		t.Fatalf("nothing existed while waiting, yet deleted %v", driver.deleted)
	}
}

func TestAutoDLRentStopsAtStockWaitDeadlineWithAggregatedError(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	driver := &fakeAutoDLDriver{createErrs: allAutoDLSpecsOutOfStock()}
	clock := &fakeStockClock{now: time.Unix(0, 0)}
	p, _ := newAutoDL(driver, serverprovider.Credentials{APIKey: "token"}, "run", "base-image", 5)
	// 12m budget with a 5m interval: attempts at 0, 5, 10 and 12 (the last pause is clipped).
	p.stockWait = clock.wait(12*time.Minute, 5*time.Minute)
	_, err := p.Rent(t.Context(), GPUSpec{GPUModel: "auto"})
	if err == nil || !errors.Is(err, errAutoDLNoSpecInStock) || !strings.Contains(err.Error(), "RTX 5090") || !strings.Contains(err.Error(), "4 rounds") {
		t.Fatalf("err = %v", err)
	}
	if want := []time.Duration{5 * time.Minute, 5 * time.Minute, 2 * time.Minute}; fmt.Sprint(clock.sleeps) != fmt.Sprint(want) {
		t.Fatalf("sleeps = %v, want %v", clock.sleeps, want)
	}
	if len(driver.tried) != 4*len(autodlAutoGPUModels) || len(driver.deleted) != 0 {
		t.Fatalf("tried=%d deleted=%v", len(driver.tried), driver.deleted)
	}
}

func TestAutoDLRentDoesNotWaitOnNonStockErrors(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	denied := errors.New("api error code=InternalError msg=账户余额不足")
	driver := &fakeAutoDLDriver{createErrs: map[string]error{autodlDefaultGPUModel: autodlStockErr, "RTX 4090D": denied}}
	clock := &fakeStockClock{now: time.Unix(0, 0)}
	p, _ := newAutoDL(driver, serverprovider.Credentials{APIKey: "token"}, "run", "base-image", 5)
	p.stockWait = clock.wait(30*time.Minute, 5*time.Minute)
	if _, err := p.Rent(t.Context(), GPUSpec{GPUModel: "auto"}); !errors.Is(err, denied) || len(clock.sleeps) != 0 || len(driver.tried) != 2 {
		t.Fatalf("err=%v sleeps=%v tried=%v", err, clock.sleeps, driver.tried)
	}
	// A VRAM floor that excludes every spec is a caller error, not a stock-out.
	driver = &fakeAutoDLDriver{createErrs: map[string]error{}}
	p, _ = newAutoDL(driver, serverprovider.Credentials{APIKey: "token"}, "run", "base-image", 5)
	p.stockWait = clock.wait(30*time.Minute, 5*time.Minute)
	if _, err := p.Rent(t.Context(), GPUSpec{GPUModel: "auto", MinVRAMMB: 1 << 30}); err == nil || errors.Is(err, errAutoDLNoSpecInStock) || len(clock.sleeps) != 0 || len(driver.tried) != 0 {
		t.Fatalf("VRAM floor: err=%v sleeps=%v tried=%v", err, clock.sleeps, driver.tried)
	}
	// A named card is neither swapped nor waited for.
	driver = &fakeAutoDLDriver{createErrs: map[string]error{autodlDefaultGPUModel: autodlStockErr}}
	p, _ = newAutoDL(driver, serverprovider.Credentials{APIKey: "token"}, "run", "base-image", 5)
	p.stockWait = clock.wait(30*time.Minute, 5*time.Minute)
	if _, err := p.Rent(t.Context(), GPUSpec{GPUModel: autodlDefaultGPUModel}); !errors.Is(err, autodlStockErr) || len(clock.sleeps) != 0 || len(driver.tried) != 1 {
		t.Fatalf("named card: err=%v sleeps=%v tried=%v", err, clock.sleeps, driver.tried)
	}
}

func TestAutoDLRentStockWaitRespectsContextCancellation(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	driver := &fakeAutoDLDriver{createErrs: allAutoDLSpecsOutOfStock()}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	clock := &fakeStockClock{now: time.Unix(0, 0)}
	clock.onSleep = func(int) { cancel() }
	p, _ := newAutoDL(driver, serverprovider.Credentials{APIKey: "token"}, "run", "base-image", 5)
	p.stockWait = clock.wait(30*time.Minute, 5*time.Minute)
	_, err := p.Rent(ctx, GPUSpec{GPUModel: "auto"})
	if !errors.Is(err, context.Canceled) || len(clock.sleeps) != 1 || len(driver.tried) != len(autodlAutoGPUModels) {
		t.Fatalf("err=%v sleeps=%v tried=%v", err, clock.sleeps, driver.tried)
	}
}

func TestAutoDLStockWaitComesFromEnvironment(t *testing.T) {
	t.Setenv("AUTODL_STOCK_WAIT", "")
	t.Setenv("AUTODL_MAX_HOURLY_PRICE", "")
	p, err := NewAutoDL("token", "run")
	if err == nil && p.maxPrice != 10 {
		t.Fatalf("default hourly price cap = %v CNY, want 10", p.maxPrice)
	}
	if err != nil || p.StockWait() != DefaultStockWait || p.stockWait.Interval != DefaultStockPollInterval {
		t.Fatalf("default: wait=%v err=%v", p.StockWait(), err)
	}
	t.Setenv("AUTODL_STOCK_WAIT", "45m")
	if p, err = NewAutoDL("token", "run"); err != nil || p.StockWait() != 45*time.Minute {
		t.Fatalf("override: err=%v", err)
	}
	t.Setenv("AUTODL_STOCK_WAIT", "0")
	if p, err = NewAutoDL("token", "run"); err != nil || p.StockWait() != 0 {
		t.Fatalf("disabled: err=%v", err)
	}
	t.Setenv("AUTODL_STOCK_WAIT", "soon")
	if _, err = NewAutoDL("token", "run"); err == nil || !strings.Contains(err.Error(), "AUTODL_STOCK_WAIT") {
		t.Fatalf("invalid value accepted: %v", err)
	}
	var _ StockWaiter = p
}
