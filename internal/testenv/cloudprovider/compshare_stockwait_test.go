package cloudprovider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	compsharedriver "gpuworks.ai/gpuworks/internal/server/provider/compshare"
)

// scriptedWalk rents from a per-round script: rounds[i][model] is the error
// of that model in round i+1 (absent: the create succeeds). Rounds past the
// end repeat the last one. A round starts at the first model.
type scriptedWalk struct {
	rounds []map[string]error
	calls  []string // "round:model"
	round  int
}

func (s *scriptedWalk) walk() CompshareWalk {
	models := []string{"4090", "4090_48G", "5090"}
	return CompshareWalk{
		Models: models,
		Rent: func(_ context.Context, model string) (*Instance, error) {
			if model == models[0] {
				s.round++
			}
			script := s.rounds[min(s.round, len(s.rounds))-1]
			s.calls = append(s.calls, fmt.Sprintf("%d:%s", s.round, model))
			if err := script[model]; err != nil {
				return nil, err
			}
			return &Instance{InstanceID: "pod-" + model, GPUModel: model}, nil
		},
		TryNext: func(err error) bool {
			return IsCompshareSupplyRefusal(err) || strings.Contains(err.Error(), "wait for compshare ssh endpoint")
		},
	}
}

var (
	errNoRoom  = fmt.Errorf("%w in cn-bj2/cn-bj2-03: gpu=4090 x1", compsharedriver.ErrCapacityUnavailable)
	errOOR     = fmt.Errorf("compshare: create in cn-sh2/cn-sh2-01: %w", &compsharedriver.APIError{Status: 200, RetCode: 226604, Message: "out of resources"})
	errBlip    = &compsharedriver.APIError{Status: 200, RetCode: 8433, Message: "service busy"}
	errNotZone = &compsharedriver.APIError{Status: 200, RetCode: 230, Message: "machine type not configured for zone"}
	errNoImage = &compsharedriver.APIError{Status: 200, RetCode: 226641, Message: "The current availability zone does not support this image."}
)

func allOut() map[string]error {
	return map[string]error{"4090": errNoRoom, "4090_48G": errOOR, "5090": errNotZone}
}

func TestCompshareSupplyRefusalClassification(t *testing.T) {
	for _, err := range []error{errNoRoom, errOOR, errBlip, errNotZone, errNoImage,
		&compsharedriver.APIError{Status: 200, RetCode: 226603, Message: "gpu type is not support current image"}} {
		if !IsCompshareSupplyRefusal(err) {
			t.Errorf("%v should be a supply refusal", err)
		}
	}
	for _, err := range []error{nil,
		&compsharedriver.APIError{Status: 200, RetCode: 100, Message: "invalid signature"},
		&compsharedriver.APIError{Status: 200, RetCode: 230001, Message: "another code that merely starts with 230"},
		&compsharedriver.APIError{Status: 200, RetCode: 8001, Message: "insufficient balance"},
		// Text alone is not a refusal any more: only the driver's typed answer is.
		errors.New("compshare: API RetCode 226604: out of resources"),
		errors.New("wait for compshare ssh endpoint: timeout")} {
		if IsCompshareSupplyRefusal(err) {
			t.Errorf("%v must not be a supply refusal", err)
		}
	}
}

func TestCompshareWalkRetriesAfterFullStockOut(t *testing.T) {
	s := &scriptedWalk{rounds: []map[string]error{allOut(), allOut(), {"4090": errNoRoom}}}
	clock := &fakeStockClock{now: time.Unix(0, 0)}
	instance, checked, err := s.walk().RentWithStockWait(t.Context(), clock.wait(30*time.Minute, 5*time.Minute))
	if err != nil || instance == nil || instance.InstanceID != "pod-4090_48G" {
		t.Fatalf("instance=%+v checked=%v err=%v", instance, checked, err)
	}
	if len(clock.sleeps) != 2 || clock.sleeps[0] != 5*time.Minute || clock.sleeps[1] != 5*time.Minute {
		t.Fatalf("sleeps=%v, want two 5m pauses", clock.sleeps)
	}
	// Each refused round walks every type in order before the wait.
	want := []string{"1:4090", "1:4090_48G", "1:5090", "2:4090", "2:4090_48G", "2:5090", "3:4090", "3:4090_48G"}
	if strings.Join(s.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls=%v want %v", s.calls, want)
	}
}

func TestCompshareWalkDeadlineAggregatesStockOut(t *testing.T) {
	s := &scriptedWalk{rounds: []map[string]error{allOut()}}
	clock := &fakeStockClock{now: time.Unix(0, 0)}
	instance, checked, err := s.walk().RentWithStockWait(t.Context(), clock.wait(30*time.Minute, 5*time.Minute))
	if instance != nil {
		t.Fatalf("instance=%+v, want none", instance)
	}
	if !errors.Is(err, ErrCompshareOutOfStock) {
		t.Fatalf("err=%v, want ErrCompshareOutOfStock", err)
	}
	for _, want := range []string{"requested capacity unavailable", "RetCode 226604", "RetCode 230", "still out of stock after waiting 30m0s over 7 rounds"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if len(clock.sleeps) != 6 {
		t.Fatalf("sleeps=%v, want 6 pauses of 5m in a 30m budget", clock.sleeps)
	}
	if len(checked) != 3 {
		t.Fatalf("checked=%v, want the last walk's three reasons", checked)
	}
}

func TestCompshareWalkWaitDisabledReturnsFirstStockOut(t *testing.T) {
	s := &scriptedWalk{rounds: []map[string]error{allOut()}}
	clock := &fakeStockClock{now: time.Unix(0, 0)}
	_, _, err := s.walk().RentWithStockWait(t.Context(), clock.wait(0, 5*time.Minute))
	if !errors.Is(err, ErrCompshareOutOfStock) || len(clock.sleeps) != 0 || s.round != 1 {
		t.Fatalf("err=%v sleeps=%v rounds=%d, want one walk and no wait", err, clock.sleeps, s.round)
	}
}

func TestCompshareWalkDoesNotRetryNonStockErrors(t *testing.T) {
	boom := errors.New("compshare: API RetCode 100: invalid signature")
	cases := map[string]map[string]error{
		// A fatal error after a supply refusal still ends the walk at once.
		"fatal after refusal": {"4090": errNoRoom, "4090_48G": boom},
		"fatal first":         {"4090": boom},
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			s := &scriptedWalk{rounds: []map[string]error{script}}
			clock := &fakeStockClock{now: time.Unix(0, 0)}
			_, _, err := s.walk().RentWithStockWait(t.Context(), clock.wait(30*time.Minute, 5*time.Minute))
			if !errors.Is(err, boom) || errors.Is(err, ErrCompshareOutOfStock) {
				t.Fatalf("err=%v, want the original error", err)
			}
			if len(clock.sleeps) != 0 || s.round != 1 {
				t.Fatalf("sleeps=%v rounds=%d, want no retry", clock.sleeps, s.round)
			}
		})
	}
}

func TestCompshareWalkMixedRefusalsAreNotAStockOut(t *testing.T) {
	// One type was ruled out for a reason that waiting does not fix (its SSH
	// endpoint never appeared): the walk ends without retrying.
	sshGone := errors.New("wait for compshare ssh endpoint: deadline exceeded")
	s := &scriptedWalk{rounds: []map[string]error{{"4090": errNoRoom, "4090_48G": sshGone, "5090": errNotZone}}}
	clock := &fakeStockClock{now: time.Unix(0, 0)}
	instance, checked, err := s.walk().RentWithStockWait(t.Context(), clock.wait(30*time.Minute, 5*time.Minute))
	if err != nil || instance != nil || len(checked) != 3 {
		t.Fatalf("instance=%+v checked=%v err=%v, want nil instance, three reasons, nil error", instance, checked, err)
	}
	if len(clock.sleeps) != 0 || s.round != 1 {
		t.Fatalf("sleeps=%v rounds=%d, want no retry", clock.sleeps, s.round)
	}
}

func TestCompshareWalkSkippedModelsAreNotAttempts(t *testing.T) {
	w := CompshareWalk{
		Models:  []string{"a", "b"},
		Skip:    func(string) string { return "too small" },
		Rent:    func(context.Context, string) (*Instance, error) { t.Fatal("rent called"); return nil, nil },
		TryNext: func(error) bool { return true },
	}
	instance, checked, err := w.Once(t.Context())
	if instance != nil || err != nil || len(checked) != 2 {
		t.Fatalf("instance=%+v checked=%v err=%v, want skipped models to be no stock-out", instance, checked, err)
	}
}

func TestCompshareWalkContextCancelStopsWait(t *testing.T) {
	s := &scriptedWalk{rounds: []map[string]error{allOut()}}
	ctx, cancel := context.WithCancel(t.Context())
	clock := &fakeStockClock{now: time.Unix(0, 0), onSleep: func(round int) {
		if round == 2 {
			cancel()
		}
	}}
	instance, _, err := s.walk().RentWithStockWait(ctx, clock.wait(30*time.Minute, 5*time.Minute))
	if instance != nil || !errors.Is(err, context.Canceled) || !errors.Is(err, ErrCompshareOutOfStock) {
		t.Fatalf("instance=%+v err=%v, want the stock-out wrapped with context.Canceled", instance, err)
	}
	if s.round != 2 {
		t.Fatalf("rounds=%d, want the wait to stop at the cancelled pause", s.round)
	}
}

func TestCompshareStockWaitEnv(t *testing.T) {
	t.Setenv("COMPSHARE_STOCK_WAIT", "")
	if w, err := StockWaitFromEnv("COMPSHARE_STOCK_WAIT", DefaultStockWait); err != nil || w.Max != 30*time.Minute || w.Interval != 5*time.Minute {
		t.Fatalf("default wait=%+v err=%v", w, err)
	}
	t.Setenv("COMPSHARE_STOCK_WAIT", "0")
	if w, err := StockWaitFromEnv("COMPSHARE_STOCK_WAIT", DefaultStockWait); err != nil || w.Max != 0 {
		t.Fatalf("disabled wait=%+v err=%v", w, err)
	}
	t.Setenv("COMPSHARE_STOCK_WAIT", "soon")
	if _, err := StockWaitFromEnv("COMPSHARE_STOCK_WAIT", DefaultStockWait); err == nil {
		t.Fatal("invalid duration must be rejected")
	}
}
