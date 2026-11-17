package autodl

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// AutoDL answers HTTP 200 with an envelope; a missing instance is only named
// in its code/msg. The classification happens in call() and survives the
// driver's own wrapping.
func TestStatusOfAMissingInstanceIsNotFound(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		missing bool
	}{
		{"envelope", 200, `{"code":"InstanceNotFound","msg":"instance not found","data":null}`, true},
		{"http 404", 404, `404 page not found`, true},
		{"other api error", 200, `{"code":"InvalidParam","msg":"instance_uuid is required","data":null}`, false},
		{"server error", 500, `internal error`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newMock(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := d.PowerState(t.Context(), provider.Credentials{APIKey: "k"}, "ins-1")
			if err == nil || errors.Is(fmt.Errorf("x: %w", err), errAPINotFound) != tc.missing {
				t.Fatalf("err = %v, missing = %v", err, tc.missing)
			}
		})
	}
	if errors.Is(errors.New("autodl: instance not found"), errAPINotFound) {
		t.Fatal("text alone must not classify")
	}
}

func TestDeleteOfAMissingInstanceSucceeds(t *testing.T) {
	d := newMock(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"InstanceNotFound","msg":"instance not found","data":null}`))
	})
	if err := d.DeleteInstance(t.Context(), provider.Credentials{APIKey: "k"}, "ins-gone"); err != nil {
		t.Fatalf("delete of a missing instance = %v", err)
	}
}

// The create refusal exactly as AutoDL answers a sold-out spec.
func TestCreateRefusalForAnEmptySpecIsOutOfStock(t *testing.T) {
	d := newMock(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"InternalError","msg":"当前算力规格暂无库存, 请修改配置或稍等再试","data":null}`))
	})
	_, err := d.PowerState(t.Context(), provider.Credentials{APIKey: "k"}, "ins-1")
	if !errors.Is(fmt.Errorf("rent: %w", err), ErrOutOfStock) || errors.Is(err, errAPINotFound) {
		t.Fatalf("err = %v", err)
	}
	if err.Error() != "autodl: status instance \"ins-1\": api error code=InternalError msg=当前算力规格暂无库存, 请修改配置或稍等再试" {
		t.Fatalf("text = %q", err)
	}
}
