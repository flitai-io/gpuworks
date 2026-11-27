package gpu

import "testing"

func TestMetaXInventoryRejectsAmbiguousIdentity(t *testing.T) {
	for name, listing := range map[string]string{
		"missing uuid":   "GPU#0 MXC550 0000:0f:00.0 Available\n",
		"duplicate uuid": "GPU#0 MXC550 0000:0f:00.0 Available (UUID: GPU-a)\nGPU#1 MXC550 0000:34:00.0 Available (UUID: GPU-a)\n",
		"truncated row":  "GPU#0 MXC550 0000:0f:00.0 Available (UUID: GPU-a)\nGPU#1 MXC550\n",
	} {
		t.Run(name, func(t *testing.T) {
			if cards, ok := validatedMetaXList(listing); ok {
				t.Fatalf("accepted incomplete inventory: %+v", cards)
			}
		})
	}
}
