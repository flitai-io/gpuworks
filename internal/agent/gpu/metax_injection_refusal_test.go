package gpu

import (
	"context"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

func TestMetaXInjectionRefusesWithoutDeviceSideEffects(t *testing.T) {
	p := &MetaXProber{run: func(context.Context, ...string) (string, error) { t.Fatal("injection invoked mx-smi"); return "", nil }}
	for _, indexes := range [][]int{{0}, {1, 3}} {
		got := p.RuntimeInjection(indexes)
		if got.UnsupportedReason == "" || !reflect.DeepEqual(got.Grant, runtime.DeviceGrant{}) || len(got.Mounts) != 0 || len(got.Env) != 0 {
			t.Fatalf("selected %v: %+v", indexes, got)
		}
	}
	got := p.RuntimeInjection(nil)
	if got.UnsupportedReason != "" || !reflect.DeepEqual(got.Grant, runtime.DeviceGrant{}) || len(got.Mounts) != 0 || len(got.Env) != 0 {
		t.Fatalf("empty: %+v", got)
	}
}
