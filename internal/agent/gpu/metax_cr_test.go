package gpu

import "testing"

func TestMetaXSummaryRequiresCompleteMemoryRows(t *testing.T) {
	cards := parseMetaXSummary("GPU#0 MXC550 0000:0f:00.0\n    Memory\n        vram total : 67108864 KB\nGPU#1 MXC550 0000:10:00.0\n    Memory\n        vram total : not-a-number KB\nGPU#2 MXC550 0000:11:00.0\n    Memory\n        vram total : 4096 KB trailing\nGPU#3 MXC550 0000:12:00.0\n    Memory\n        vram total : 0 KB")
	if got := cards["0000:0f:00.0"].memoryTotalBytes; got != 67108864<<10 {
		t.Fatalf("valid summary memory = %d", got)
	}
	for _, bdf := range []string{"0000:10:00.0", "0000:11:00.0", "0000:12:00.0"} {
		if _, ok := cards[bdf]; ok {
			t.Errorf("malformed/unknown summary row for %s was accepted", bdf)
		}
	}
}
