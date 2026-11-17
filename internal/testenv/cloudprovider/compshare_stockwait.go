package cloudprovider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	compsharedriver "gpuworks.ai/gpuworks/internal/server/provider/compshare"
)

// ErrCompshareOutOfStock marks a walk over the candidate card types in which
// every create attempt was refused for lack of supply, so no instance exists
// and the whole walk is worth repeating later.
var ErrCompshareOutOfStock = errors.New("compshare: every candidate create was refused for lack of supply")

// compshareSupplyRetCodes are the create refusals that mean "CompShare has no
// such machine right now or here", as opposed to a credential, balance or
// request error. Each one leaves nothing rented; so does a placement that
// found no zone with room (compsharedriver.ErrCapacityUnavailable).
var compshareSupplyRetCodes = []compsharedriver.RetCode{
	226604, // out of resources
	8433,   // transient service blip while placing
	230,    // machine type not configured for the zone
	226641, // zone does not support the image
	226603, // GPU type not supported by the image
}

// IsCompshareSupplyRefusal reports whether err is a CompShare create refusal
// for lack of supply, the only kind a stock wait retries.
func IsCompshareSupplyRefusal(err error) bool {
	if errors.Is(err, compsharedriver.ErrCapacityUnavailable) {
		return true
	}
	for _, code := range compshareSupplyRetCodes {
		if errors.Is(err, code) {
			return true
		}
	}
	return false
}

// CompshareWalk describes one pass over the candidate card types.
type CompshareWalk struct {
	Models []string
	// Skip returns a non-empty reason for a model that is not tried at all
	// (too little VRAM); it is recorded but is not an attempt. May be nil.
	Skip func(model string) string
	// Rent creates an instance of one model.
	Rent func(ctx context.Context, model string) (*Instance, error)
	// TryNext reports whether a Rent error only rules this model out. Any other
	// error ends the walk, and the wait, immediately.
	TryNext func(error) bool
	// OnTryNext is called for each model ruled out (logging). May be nil.
	OnTryNext func(model string, err error)
}

// Once walks the candidates and returns the first instance rented. When none
// could be, it returns the reason per model. If at least one create was tried
// and every one was a supply refusal, the error wraps ErrCompshareOutOfStock;
// a mixed outcome (some other try-next reason such as an SSH endpoint that
// never appeared) is not a stock-out and returns a nil error.
func (w CompshareWalk) Once(ctx context.Context) (*Instance, []string, error) {
	var checked []string
	attempts, supply := 0, 0
	for _, model := range w.Models {
		if w.Skip != nil {
			if reason := w.Skip(model); reason != "" {
				checked = append(checked, reason)
				continue
			}
		}
		attempts++
		instance, err := w.Rent(ctx, model)
		if err == nil {
			return instance, checked, nil
		}
		if !w.TryNext(err) {
			return nil, checked, err
		}
		if IsCompshareSupplyRefusal(err) {
			supply++
		}
		if w.OnTryNext != nil {
			w.OnTryNext(model, err)
		}
		checked = append(checked, model+": "+err.Error())
	}
	if attempts > 0 && supply == attempts {
		return nil, checked, fmt.Errorf("%w (%s)", ErrCompshareOutOfStock, strings.Join(checked, "; "))
	}
	return nil, checked, nil
}

// RentWithStockWait repeats the whole walk while it ends in a full stock-out,
// every wait.Interval until wait.Max is spent (see RetryOnStockOut). Nothing is
// rented while waiting. On exhaustion it returns the last walk's reasons with
// the aggregated stock-out error; any other error is returned at once.
func (w CompshareWalk) RentWithStockWait(ctx context.Context, wait StockWait) (*Instance, []string, error) {
	type result struct {
		instance *Instance
		checked  []string
	}
	got, err := RetryOnStockOut(ctx, wait, "compshare container",
		func(err error) bool { return errors.Is(err, ErrCompshareOutOfStock) },
		func(ctx context.Context) (result, error) {
			instance, checked, err := w.Once(ctx)
			return result{instance, checked}, err
		})
	return got.instance, got.checked, err
}
