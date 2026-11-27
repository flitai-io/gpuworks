package gpu

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

const regressionMetaXHeader = "deviceId,bdfId,utilization.vram.used [KB],utilization.vram.total [KB],utilization.GPU [%],temperature.hotspot [C]\n"

func TestIdentifyMetaXMetricsRejectsConflictingRowsAndKeepsHealthyNeighbor(t *testing.T) {
	listing := parseMetaXList("GPU#0 MXC550 0000:0f:00.0 Available (UUID: GPU-a)\nGPU#1 MXC550 0000:34:00.0 Available (UUID: GPU-b)\n")
	csv := regressionMetaXHeader +
		"GPU#0,0000:0f:00.0,10,100,20,40\n" +
		"GPU#0,0000:0f:00.0,11,100,20,40\n" +
		"GPU#1,0000:34:00.0,0,0,0,0\n"
	got := identifyMetaXMetrics(csv, listing)
	if len(got) != 1 || got[0].Index != 1 || got[0].UUID != "GPU-b" {
		t.Fatalf("metrics = %+v, want only healthy neighbor GPU-b", got)
	}
}

func TestIdentifyMetaXMetricsRejectsAmbiguousBDFIdentity(t *testing.T) {
	listing := parseMetaXList("GPU#0 MXC550 0000:0f:00.0 Available (UUID: GPU-a)\nGPU#1 MXC550 0000:34:00.0 Available (UUID: GPU-b)\n")
	listing[1].PCIeBDF = listing[0].PCIeBDF
	csv := regressionMetaXHeader +
		"GPU#0,0000:0f:00.0,10,100,20,40\n" +
		"GPU#1,0000:34:00.0,10,100,20,40\n"
	if got := identifyMetaXMetrics(csv, listing); len(got) != 0 {
		t.Fatalf("duplicate inventory BDF attributed samples: %+v", got)
	}
}

func TestIdentifyMetaXMetricsRejectsInvalidValuesAndMissingColumns(t *testing.T) {
	listing := parseMetaXList("GPU#0 MXC550 0000:0f:00.0 Available (UUID: GPU-a)\nGPU#1 MXC550 0000:34:00.0 Available (UUID: GPU-b)\n")
	for _, value := range []string{"NaN", "+Inf", "-Inf", "-1"} {
		csv := regressionMetaXHeader +
			"GPU#0,0000:0f:00.0," + value + ",100,20,40\n" +
			"GPU#1,0000:34:00.0,10,100,20,40\n"
		got := identifyMetaXMetrics(csv, listing)
		if len(got) != 1 || got[0].UUID != "GPU-b" {
			t.Errorf("value %q yielded metrics %+v, want only healthy neighbor", value, got)
		}
	}
	missing := "deviceId,bdfId,utilization.vram.used [KB],utilization.vram.total [KB],utilization.GPU [%]\n" +
		"GPU#0,0000:0f:00.0,10,100,20\nGPU#1,0000:34:00.0,10,100,20\n"
	if got := identifyMetaXMetrics(missing, listing); len(got) != 2 || len(got[0].Values) != 3 {
		t.Fatalf("known columns lost from partial sample: %+v", got)
	} else if _, ok := got[0].Values["gpuworks_gpu_temperature_celsius"]; ok {
		t.Fatalf("missing temperature was fabricated: %+v", got[0])
	}
}

func TestIdentifyMetaXMetricsAcceptsZeroAndNegativeTemperature(t *testing.T) {
	listing := []proto.GPUInfo{{Index: 0, UUID: "GPU-a", PCIeBDF: "0000:0f:00.0"}}
	csv := regressionMetaXHeader + "GPU#0,0000:0f:00.0,0,0,0,-10\n"
	got := identifyMetaXMetrics(csv, listing)
	if len(got) != 1 || got[0].Values["gpuworks_gpu_temperature_celsius"] != -10 {
		t.Fatalf("legitimate boundary sample rejected: %+v", got)
	}
}

func TestIdentifyMetaXMetricsRejectsTruncatedDuplicate(t *testing.T) {
	listing := []proto.GPUInfo{{Index: 0, UUID: "GPU-a", PCIeBDF: "0000:0f:00.0"}}
	valid := "GPU#0,0000:0f:00.0,10,100,20,40\n"
	truncated := "GPU#0,0000:0f:00.0,11\n"
	for _, rows := range []string{valid + truncated, truncated + valid} {
		if got := identifyMetaXMetrics(regressionMetaXHeader+rows, listing); len(got) != 0 {
			t.Fatalf("truncated duplicate accepted: %+v", got)
		}
	}
}
