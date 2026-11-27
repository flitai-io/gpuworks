package gpu

import (
	"errors"
	"strings"
	"testing"
)

func TestMetaXSmokeVerdictTreatsUnattributedFailuresAsInconclusive(t *testing.T) {
	for _, marker := range []string{
		"MACA_RUNTIME_SMOKE_UNAVAILABLE: mcInit=7",
		"MACA_RUNTIME_SMOKE_DIE_FAILED: mcSetDevice rc=7",
		"MACA_RUNTIME_SMOKE_DIE_FAILED: mcMalloc rc=7",
		"MACA_RUNTIME_SMOKE_DIE_FAILED: mcFree rc=7",
	} {
		t.Run(strings.TrimSpace(marker), func(t *testing.T) {
			err := metaXSmokeVerdict([]byte(marker+"\n"), errors.New("exit status 1"))
			if HostRuntimeUnavailable(err) == "" {
				t.Fatalf("verdict should be host-inconclusive, got %v", err)
			}
			if !strings.Contains(err.Error(), strings.TrimPrefix(marker, strings.Split(marker, ":")[0]+":")) {
				t.Fatalf("verdict lost MACA failure detail: %v", err)
			}
		})
	}
}

func TestMetaXSmokeVerdictRequiresExactSuccessMarker(t *testing.T) {
	for _, out := range []string{"", "ordinary output", "MACA_RUNTIME_SMOKE_OK-ish\n"} {
		err := metaXSmokeVerdict([]byte(out), nil)
		if HostRuntimeUnavailable(err) == "" {
			t.Fatalf("output %q should not pass without exact success marker; got %v", out, err)
		}
	}
	if err := metaXSmokeVerdict([]byte("MACA_RUNTIME_SMOKE_OK\n"), nil); err != nil {
		t.Fatalf("explicit success marker: %v", err)
	}
	if err := metaXSmokeVerdict([]byte("MACA_RUNTIME_SMOKE_OK\n"), errors.New("process failed")); HostRuntimeUnavailable(err) == "" {
		t.Fatalf("success marker with failed process passed: %v", err)
	}
}
