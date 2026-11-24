package estimate

import (
	"strings"
	"testing"
)

func TestDtypeSupportKeepsNVIDIACompatibility(t *testing.T) {
	if ok, reason := DtypeSupport("Turing", "bfloat16", "", 7.5); ok || !strings.Contains(reason, "bfloat16") {
		t.Fatalf("Turing bf16 ok=%v reason=%q", ok, reason)
	}
	if ok, reason := DtypeSupport("Ada", "bfloat16", "fp8", 8.9); !ok || reason != "" {
		t.Fatalf("Ada bf16/fp8 ok=%v reason=%q", ok, reason)
	}
}

func TestDtypeSupportUsesExplicitNVIDIAQuantMatrix(t *testing.T) {
	// Ampere serves FP8 weights through Marlin W8A16; Turing has no such path.
	if ok, reason := DtypeSupportForGPU("nvidia", "sm_86", "Ampere", "bfloat16", "fp8", 8.6); !ok || reason != "" {
		t.Fatalf("Ampere fp8 quant ok=%v reason=%q", ok, reason)
	}
	if ok, reason := DtypeSupportForGPU("nvidia", "sm_80", "Ampere", "float8", "", 8.0); !ok || reason != "" {
		t.Fatalf("Ampere fp8 weights ok=%v reason=%q", ok, reason)
	}
	if ok, reason := DtypeSupportForGPU("nvidia", "sm_75", "Turing", "float16", "fp8", 7.5); ok || !strings.Contains(reason, "fp8") {
		t.Fatalf("Turing fp8 quant ok=%v reason=%q", ok, reason)
	}
	if ok, reason := DtypeSupportForGPU("nvidia", "sm_86", "Ampere", "bfloat16", "nvfp4", 8.6); ok || !strings.Contains(reason, "nvfp4") {
		t.Fatalf("Ampere nvfp4 quant ok=%v reason=%q", ok, reason)
	}
	if ok, reason := DtypeSupportForGPU("nvidia", "sm_100", "Blackwell", "float16", "nvfp4", 10.0); !ok || reason != "" {
		t.Fatalf("Blackwell nvfp4 ok=%v reason=%q", ok, reason)
	}
}

func TestDtypeSupportForAscendAndUnknownVendor(t *testing.T) {
	if ok, reason := DtypeSupportForGPU("ascend", "910B4", "", "bfloat16", "", 0); !ok || reason != "" {
		t.Fatalf("Ascend 910B4 bf16 ok=%v reason=%q", ok, reason)
	}
	if ok, reason := DtypeSupportForGPU("ascend", "910B4", "", "float8", "", 0); ok || !strings.Contains(reason, "fp8") {
		t.Fatalf("Ascend 910B4 fp8 ok=%v reason=%q", ok, reason)
	}
	if ok, reason := DtypeSupportForGPU("future-vendor", "x1", "", "float8", "unknown-quant", 0); !ok || reason != "" {
		t.Fatalf("unknown vendor should be permissive, ok=%v reason=%q", ok, reason)
	}
}

func TestDtypeSupportForPPUConservativeFP8(t *testing.T) {
	if ok, reason := DtypeSupportForGPU("ppu", "ZW810E", "", "float8", "", 0); ok || !strings.Contains(reason, "fp8") {
		t.Fatalf("PPU fp8 ok=%v reason=%q", ok, reason)
	}
	if ok, reason := DtypeSupportForGPU("ppu", "ZW810E", "", "bfloat16", "", 0); !ok || reason != "" {
		t.Fatalf("PPU bf16 ok=%v reason=%q", ok, reason)
	}
}
