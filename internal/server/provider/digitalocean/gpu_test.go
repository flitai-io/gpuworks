package digitalocean

import "testing"

func TestReadableModel(t *testing.T) {
	for id, want := range map[string]string{
		"nvidia_rtx4000_ada": "NVIDIA RTX 4000 Ada",
		"nvidia_rtx6000_ada": "NVIDIA RTX 6000 Ada",
		"nvidia_l40s":        "NVIDIA L40S",
		"nvidia_h100":        "NVIDIA H100",
		"nvidia_h200":        "NVIDIA H200",
		"nvidia_b300":        "NVIDIA B300",
		"amd_mi300x":         "AMD MI300X",
		"amd_mi355x":         "AMD MI355X",
		"":                   "",
	} {
		if got := readableModel(id); got != want {
			t.Errorf("readableModel(%q) = %q, want %q", id, got, want)
		}
	}
}

// The slugs are the account's /v2/sizes and /v2/kubernetes/options (2026-10-22).
func TestParseSizeSlug(t *testing.T) {
	for slug, want := range map[string]sizeSlug{
		"gpu-4000adax1-20gb":       {"RTX 4000 Ada", 1, "nvidia"},
		"gpu-6000adax1-48gb":       {"RTX 6000 Ada", 1, "nvidia"},
		"gpu-l40sx1-48gb":          {"L40S", 1, "nvidia"},
		"gpu-h100x1-80gb":          {"H100", 1, "nvidia"},
		"gpu-h100x8-640gb":         {"H100", 8, "nvidia"},
		"gpu-h200x8-1128gb":        {"H200", 8, "nvidia"},
		"gpu-b300x1-288gb-lc-spot": {"B300", 1, "nvidia"},
		"gpu-b300x8-2304gb-spot":   {"B300", 8, "nvidia"},
		"gpu-mi300x1-192gb":        {"MI300X", 1, "amd"},
		"gpu-mi300x8-1536gb":       {"MI300X", 8, "amd"},
		"gpu-mi350x1-288gb-spot":   {"MI350X", 1, "amd"},
		"gpu-5090x2-64gb":          {"5090", 2, "nvidia"}, // generic fallback
		"gpu-8000adax16-960gb":     {"RTX 8000 Ada", 16, "nvidia"},
		"GPU-H100X8-640GB":         {"H100", 8, "nvidia"},
	} {
		got, ok := parseSizeSlug(slug)
		if !ok || got != want {
			t.Errorf("parseSizeSlug(%q) = %+v, %v; want %+v", slug, got, ok, want)
		}
	}
	for _, slug := range []string{"", "s-2vcpu-4gb", "gpu-", "gpu-h100-80gb", "gpu-x1-80gb", "gpu-h100x0-80gb", "c-8-intel"} {
		if got, ok := parseSizeSlug(slug); ok {
			t.Errorf("parseSizeSlug(%q) = %+v, want no result", slug, got)
		}
	}
}

func TestDefaultImage(t *testing.T) {
	for slug, want := range map[string]string{
		"gpu-h100x1-80gb": "gpu-h100x1-base", "gpu-l40sx1-48gb": "gpu-h100x1-base",
		"gpu-h100x8-640gb": "gpu-h100x8-base", "gpu-mi300x8-1536gb": "gpu-amd-base", "gpu-mi325x1-256gb": "gpu-amd-base",
		"s-2vcpu-4gb": "",
	} {
		if got := defaultImage(slug); got != want {
			t.Errorf("defaultImage(%q) = %q, want %q", slug, got, want)
		}
	}
}
