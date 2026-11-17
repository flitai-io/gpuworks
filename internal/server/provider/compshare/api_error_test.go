package compshare

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// Answers as the CompShare API sends them; the classification happens once,
// in call(), and survives wrapping.
func TestCallClassifiesTheAPIAnswer(t *testing.T) {
	for _, tc := range []struct {
		name              string
		status            int
		body              string
		transient, absent bool
		text              string
	}{
		{"create busy", 200, `{"RetCode":226645,"Message":"资源不足，请稍后重试"}`, true, false, "compshare: API RetCode 226645: 资源不足，请稍后重试"},
		{"service error", 200, `{"RetCode":8433,"Message":"Service error, please retry or contact technical support"}`, true, false, ""},
		{"gateway 502", 502, `<html>502 Bad Gateway</html>`, true, false, "compshare: HTTP 502: <html>502 Bad Gateway</html>"},
		{"gateway 429 service unavailable", 429, `Service Unavailable`, true, false, ""},
		{"deleted (en)", 200, `{"RetCode":190,"Message":"instance does not exist"}`, false, true, ""},
		{"deleted (zh)", 200, `{"RetCode":8039,"Message":"主机已删除"}`, false, true, ""},
		{"bad params", 200, `{"RetCode":230,"Message":"Params [Zone] not available"}`, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			d := New()
			d.setBaseURL(srv.URL)
			_, err := d.call(t.Context(), provider.Credentials{PublicKey: "pub", PrivateKey: "priv"}, map[string]string{"Action": listAction})
			wrapped := fmt.Errorf("create: %w", err)
			if isCreateTransientError(wrapped) != tc.transient || isNotFoundError(wrapped) != tc.absent {
				t.Fatalf("%v: transient=%v notFound=%v", err, isCreateTransientError(wrapped), isNotFoundError(wrapped))
			}
			if tc.text != "" && err.Error() != tc.text {
				t.Fatalf("text = %q, want %q", err, tc.text)
			}
		})
	}
}

func TestGetInstanceReportsAMissingInstanceAsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "UHostSet": []any{}})
	}))
	defer srv.Close()
	d := New()
	d.setBaseURL(srv.URL)
	_, err := d.GetInstance(t.Context(), provider.Credentials{PublicKey: "pub", PrivateKey: "priv"}, "uhost-gone")
	if !errors.Is(err, errInstanceNotFound) || !isNotFoundError(err) {
		t.Fatalf("err = %v", err)
	}
	if isNotFoundError(errors.New("compshare: request failed: dial tcp: lookup api: not found")) {
		t.Fatal("a transport error is not the API saying the instance is gone")
	}
}

func TestRetCodeMatchesAnywhereInAPlacementWalk(t *testing.T) {
	walk := placementErrors{
		fmt.Errorf("compshare: check resource capacity in cn-sh2/cn-sh2-01: %w", &APIError{Status: 200, RetCode: 226603, Message: "gpu type is not support current image"}),
		fmt.Errorf("%w in cn-bj2/cn-bj2-03: gpu=4090 x1", ErrCapacityUnavailable),
	}
	if !errors.Is(walk, RetCode(226603)) || !errors.Is(walk, ErrCapacityUnavailable) || errors.Is(walk, RetCode(226641)) {
		t.Fatalf("classification lost in %v", walk)
	}
	if errors.Is(errors.New("compshare: API RetCode 226603: x"), RetCode(226603)) {
		t.Fatal("text alone must not match")
	}
}
