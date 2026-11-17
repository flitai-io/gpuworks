package cloudprovider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// giteeStockServer fakes the compute API: creates are refused with Gitee's
// "no available host" answer until `refusals` are used up, in either of the
// two shapes the API has been seen to use, then succeed. A non-empty
// `failure` makes the create fail with an unrelated error instead.
type giteeStockServer struct {
	refusals int
	httpErr  bool // refuse with HTTP 400 instead of 200 + success=false
	failure  string
	creates  int
}

func (s *giteeStockServer) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/compute/specs":
			_ = json.NewEncoder(w).Encode([]giteeSpec{{Zone: "gitee", GPUModel: "Ascend 910B2C", GPUVRAMGB: 64}})
		case r.Method == http.MethodPost && r.URL.Path == "/compute/instances":
			s.creates++
			switch {
			case s.failure != "":
				http.Error(w, s.failure, http.StatusUnauthorized)
			case s.creates <= s.refusals && s.httpErr:
				http.Error(w, `{"error":"没有找到符合条件的可用主机"}`, http.StatusBadRequest)
			case s.creates <= s.refusals:
				_ = json.NewEncoder(w).Encode([]giteeCreateResult{{Success: false, Error: "没有找到符合条件的可用主机"}})
			default:
				_ = json.NewEncoder(w).Encode([]giteeCreateResult{{ID: "stock-id", Success: true}})
			}
		case r.Method == http.MethodGet && r.URL.Path == "/compute/instances":
			row := giteeInstance{ID: "stock-id", GPUModel: "Ascend 910B2C", GPUCount: 1, Status: "running"}
			row.SSH.Host, row.SSH.Port, row.SSH.User = "127.0.0.1", 22, "root"
			_ = json.NewEncoder(w).Encode([]giteeInstance{row})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
}

func newGiteeStockProvider(t *testing.T, s *giteeStockServer) *Gitee {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	server := httptest.NewServer(s.handler(t))
	t.Cleanup(server.Close)
	provider, err := newGitee("token", "gitee", "image", "run-stock", server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func TestIsGiteeNoHostRecognisesBothRefusalShapes(t *testing.T) {
	for _, httpErr := range []bool{false, true} {
		provider := newGiteeStockProvider(t, &giteeStockServer{refusals: 1, httpErr: httpErr})
		_, err := provider.Rent(t.Context(), GPUSpec{GPUModel: "Ascend 910B2C", GPUCount: 1})
		if !IsGiteeNoHost(err) {
			t.Fatalf("httpErr=%v: refusal not classified as no-host: %v", httpErr, err)
		}
	}
	for _, err := range []error{nil, errors.New("gitee: GPU model \"x\" is not available in zone \"gitee\""), errors.New("POST /compute/instances status=401 body=unauthorized"),
		// Only the create's own classification counts, not text that says so.
		errors.New("gitee create compute instance: 没有找到符合条件的可用主机")} {
		if IsGiteeNoHost(err) {
			t.Fatalf("%v wrongly classified as no-host", err)
		}
	}
}

func TestGiteeRentWaitsOutNoHostThenSucceeds(t *testing.T) {
	stock := &giteeStockServer{refusals: 2}
	provider := newGiteeStockProvider(t, stock)
	clock := &fakeStockClock{now: time.Unix(0, 0)}
	instance, err := RetryOnStockOut(t.Context(), clock.wait(30*time.Minute, 5*time.Minute), "gitee Ascend 910B2C", IsGiteeNoHost,
		func(ctx context.Context) (*Instance, error) {
			return provider.Rent(ctx, GPUSpec{GPUModel: "Ascend 910B2C", GPUCount: 1})
		})
	if err != nil || instance.InstanceID != "stock-id" {
		t.Fatalf("instance=%+v err=%v", instance, err)
	}
	if stock.creates != 3 || len(clock.sleeps) != 2 || len(clock.logs) != 2 {
		t.Fatalf("creates=%d sleeps=%v logs=%q", stock.creates, clock.sleeps, clock.logs)
	}
	// Refused creates never reached the ledger: only the one real instance is owned.
	if owned, err := giteeLedgerContains("run-stock", "stock-id"); err != nil || !owned {
		t.Fatalf("ledger owned=%v err=%v", owned, err)
	}
}

func TestGiteeRentGivesUpAtDeadlineAndSkipsNonStockErrors(t *testing.T) {
	stock := &giteeStockServer{refusals: 1 << 20}
	provider := newGiteeStockProvider(t, stock)
	clock := &fakeStockClock{now: time.Unix(0, 0)}
	rent := func(ctx context.Context) (*Instance, error) {
		return provider.Rent(ctx, GPUSpec{GPUModel: "Ascend 910B2C", GPUCount: 1})
	}
	_, err := RetryOnStockOut(t.Context(), clock.wait(10*time.Minute, 5*time.Minute), "gitee", IsGiteeNoHost, rent)
	if !IsGiteeNoHost(err) || !strings.Contains(err.Error(), "3 rounds") || stock.creates != 3 || len(clock.sleeps) != 2 {
		t.Fatalf("err=%v creates=%d sleeps=%v", err, stock.creates, clock.sleeps)
	}
	if leaked, err := provider.ListLeakedForRunID(t.Context(), "run-stock"); err != nil || len(leaked) != 0 {
		t.Fatalf("a waited-out stock-out must leave no instance: leaked=%v err=%v", leaked, err)
	}

	failing := &giteeStockServer{failure: "unauthorized"}
	provider = newGiteeStockProvider(t, failing)
	clock = &fakeStockClock{now: time.Unix(0, 0)}
	_, err = RetryOnStockOut(t.Context(), clock.wait(30*time.Minute, 5*time.Minute), "gitee", IsGiteeNoHost, func(ctx context.Context) (*Instance, error) {
		return provider.Rent(ctx, GPUSpec{GPUModel: "Ascend 910B2C", GPUCount: 1})
	})
	if err == nil || IsGiteeNoHost(err) || failing.creates != 1 || len(clock.sleeps) != 0 {
		t.Fatalf("non-stock error was retried: err=%v creates=%d sleeps=%v", err, failing.creates, clock.sleeps)
	}
}

func TestRetryOnStockOutDisabledAndCancelled(t *testing.T) {
	stock := errors.New("sold out")
	isStock := func(err error) bool { return errors.Is(err, stock) }
	calls := 0
	attempt := func(context.Context) (int, error) { calls++; return calls, stock }
	clock := &fakeStockClock{now: time.Unix(0, 0)}
	if _, err := RetryOnStockOut(t.Context(), clock.wait(0, time.Minute), "x", isStock, attempt); err != stock || calls != 1 {
		t.Fatalf("Max=0 must return the first stock-out unchanged: err=%v calls=%d", err, calls)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls = 0
	clock = &fakeStockClock{now: time.Unix(0, 0)}
	if _, err := RetryOnStockOut(ctx, clock.wait(time.Hour, time.Minute), "x", isStock, attempt); !errors.Is(err, context.Canceled) || !errors.Is(err, stock) || calls != 1 {
		t.Fatalf("cancelled wait: err=%v calls=%d", err, calls)
	}
}

func TestStockWaitFromEnv(t *testing.T) {
	t.Setenv("GITEE_STOCK_WAIT_TEST", "")
	if w, err := StockWaitFromEnv("GITEE_STOCK_WAIT_TEST", DefaultStockWait); err != nil || w.Max != 30*time.Minute || w.Interval != 5*time.Minute {
		t.Fatalf("default: %+v err=%v", w, err)
	}
	t.Setenv("GITEE_STOCK_WAIT_TEST", "90s")
	if w, err := StockWaitFromEnv("GITEE_STOCK_WAIT_TEST", DefaultStockWait); err != nil || w.Max != 90*time.Second {
		t.Fatalf("override: %+v err=%v", w, err)
	}
	for _, bad := range []string{"soon", "-5m", "30"} {
		t.Setenv("GITEE_STOCK_WAIT_TEST", bad)
		if _, err := StockWaitFromEnv("GITEE_STOCK_WAIT_TEST", DefaultStockWait); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
