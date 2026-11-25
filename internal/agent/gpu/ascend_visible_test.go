package gpu

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

func TestAscendRootlessLogicalDeviceSelection(t *testing.T) {
	const header = "NPU ID  Chip ID  Chip Logic ID  Chip Name\n"
	for _, tc := range []struct {
		name, info, mapping, want, fail string
		indexes                         []int
		queryErr                        error
	}{
		{name: "sparse bare host", info: ascendInfo26Sample, mapping: header + "2 0 2 Ascend 910B4\n2 1 - Mcu\n4 0 4 Ascend 910B4", indexes: []int{4, 2}, want: "2,4"},
		{name: "VM compacts physical IDs", info: ascendInfo26Sample, mapping: header + "2 0 0 Ascend 910B4\n4 0 1 Ascend 910B4", indexes: []int{4}, want: "1"},
		{name: "multi-chip physical IDs", info: readNPUFixture(t, "npu-smi-info-910c-sparse.txt"), mapping: header + "7 0 6 Ascend 910C\n7 1 7 Ascend 910C", indexes: []int{15, 14}, want: "6,7"},
		// GWKS-418: physical dies 2, 3, 4 and 6 carry logical IDs 3, 4, 0 and
		// 1. The ACL runtime refused "3,4,0,1" (EE2002: "Expected value:
		// configured in ascending order") and every multi-card start failed.
		{name: "logical IDs out of physical order", info: readNPUFixture(t, "npu-smi-info-910c-five-cards.txt"), mapping: header + "1 0 3 Ascend 910C\n1 1 4 Ascend 910C\n2 0 0 Ascend 910C\n3 0 1 Ascend 910C", indexes: []int{2, 3, 4, 6}, want: "0,1,3,4"},
		{name: "query error", info: ascendInfo26Sample, indexes: []int{2}, queryErr: errors.New("driver failed"), fail: "driver failed"},
		{name: "empty response", info: ascendInfo26Sample, indexes: []int{2}, fail: "header"},
		{name: "partial mapping", info: ascendInfo26Sample, mapping: header + "2 0 2 Ascend 910B4", indexes: []int{2, 4}, fail: "no logical device mapping"},
		{name: "absent physical device", info: ascendInfo26Sample, mapping: header, indexes: []int{7}, fail: "absent"},
		{name: "conflicting mapping", info: ascendInfo26Sample, mapping: header + "2 0 2 Ascend 910B4\n2 0 3 Ascend 910B4", indexes: []int{2}, fail: "conflicting"},
		{name: "invalid logical ID", info: ascendInfo26Sample, mapping: header + "2 0 -1 Ascend 910B4", indexes: []int{2}, fail: "no logical"},
		{name: "duplicate logical ID", info: ascendInfo26Sample, mapping: header + "2 0 0 Ascend 910B4\n4 0 0 Ascend 910B4", indexes: []int{2, 4}, fail: "multiple selected"},
		{name: "inventory failure", indexes: []int{2}, fail: "physical devices"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &AscendProber{runner: func(ctx context.Context, args ...string) (string, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("mapping query must have a deadline")
				}
				switch strings.Join(args, " ") {
				case "info":
					if tc.info == "" {
						return "", errors.New("inventory unavailable")
					}
					return tc.info, nil
				case "info -m":
					return tc.mapping, tc.queryErr
				default:
					t.Fatalf("unexpected CLI args: %v", args)
					return "", nil
				}
			}}
			got := p.RuntimeInjectionForDriver(tc.indexes, runtime.TargetNamed("udocker"))
			if tc.fail != "" {
				if !strings.Contains(got.UnsupportedReason, tc.fail) || len(got.Env) != 0 {
					t.Fatalf("failure must not fall back to card 0: %+v", got)
				}
				return
			}
			if got.UnsupportedReason != "" || got.Env[accelerator.AscendEnvRTVisibleDevices] != tc.want || got.Env[accelerator.AscendEnvNPUVisible] != tc.want {
				t.Fatalf("injection=%+v want logical=%s", got, tc.want)
			}
			original := p.RuntimeInjection(tc.indexes)
			if got.Env[accelerator.AscendEnvVisibleDevices] != original.Env[accelerator.AscendEnvVisibleDevices] {
				t.Fatal("physical selection changed")
			}
		})
	}
}
