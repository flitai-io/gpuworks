// Package hardwarecatalog provides the static accelerator choices used by
// hypothetical deployment planning. Runtime compatibility never consults this
// package; it evaluates a spec's inline GPU predicate against detected GPUs.
package hardwarecatalog

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

const (
	CategoryDataCenter  = "data-center"
	CategoryWorkstation = "workstation-consumer"
	CategoryDomestic    = "domestic"
)

type GPU struct {
	Vendor       string  `json:"vendor"`
	Name         string  `json:"name"`
	CC           float64 `json:"cc,omitempty"`
	Category     string  `json:"category"`
	Architecture string  `json:"architecture"`
	ArchID       string  `json:"arch_id,omitempty"`
}

// List returns a copy so callers cannot mutate the process-wide catalog.
func List() []GPU { return append([]GPU(nil), catalog...) }

func ArchitectureForCC(cc float64) string {
	switch {
	case cc >= 10:
		return "Blackwell"
	case cc >= 9:
		return "Hopper"
	case cc >= 8.9:
		return "Ada"
	case cc >= 8:
		return "Ampere"
	case cc >= 7.5:
		return "Turing"
	case cc >= 7:
		return "Volta"
	case cc >= 6:
		return "Pascal"
	default:
		return "Legacy"
	}
}

func nvidia(category string, cc float64, names ...string) []GPU {
	result := make([]GPU, 0, len(names))
	for _, name := range names {
		result = append(result, GPU{Vendor: accelerator.VendorNVIDIA, Name: name, CC: cc, Category: category, Architecture: ArchitectureForCC(cc)})
	}
	return result
}

func domestic(vendor string, names ...string) []GPU {
	result := make([]GPU, 0, len(names))
	for _, name := range names {
		result = append(result, GPU{Vendor: vendor, Name: name, Category: CategoryDomestic, Architecture: strings.ToUpper(vendor)})
	}
	return result
}

// apple lists Apple silicon SoCs. They are workstation hardware whose GPU
// shares the host memory, so the generation is the architecture and the tier
// (Pro, Max, Ultra) only changes core count and memory.
func apple(generation string, tiers ...string) []GPU {
	result := make([]GPU, 0, len(tiers))
	for _, tier := range tiers {
		name := strings.TrimSpace("Apple " + generation + " " + tier)
		result = append(result, GPU{Vendor: accelerator.VendorApple, Name: name, Category: CategoryWorkstation, Architecture: generation})
	}
	return result
}

var catalog = func() []GPU {
	var result []GPU
	result = append(result, nvidia(CategoryDataCenter, 12.0,
		"NVIDIA RTX PRO 6000 Blackwell Server Edition", "NVIDIA RTX PRO 4500 Blackwell Server Edition")...)
	result = append(result, nvidia(CategoryDataCenter, 10.3, "NVIDIA GB300", "NVIDIA B300")...)
	result = append(result, nvidia(CategoryDataCenter, 10.0, "NVIDIA GB200", "NVIDIA B200")...)
	result = append(result, nvidia(CategoryDataCenter, 9.0, "NVIDIA GH200", "NVIDIA H200", "NVIDIA H100")...)
	result = append(result, nvidia(CategoryDataCenter, 8.9, "NVIDIA L4", "NVIDIA L40", "NVIDIA L40S")...)
	result = append(result, nvidia(CategoryDataCenter, 8.6, "NVIDIA A40", "NVIDIA A10", "NVIDIA A16", "NVIDIA A2")...)
	result = append(result, nvidia(CategoryDataCenter, 8.0, "NVIDIA A100", "NVIDIA A30")...)
	result = append(result, nvidia(CategoryDataCenter, 7.5, "NVIDIA T4")...)
	result = append(result, nvidia(CategoryDataCenter, 7.0, "NVIDIA V100")...)

	result = append(result, nvidia(CategoryWorkstation, 12.1, "NVIDIA GB10 (DGX Spark)")...)
	result = append(result, nvidia(CategoryWorkstation, 12.0,
		"NVIDIA RTX PRO 6000 Blackwell Workstation Edition", "NVIDIA RTX PRO 6000 Blackwell Max-Q Workstation Edition",
		"NVIDIA RTX PRO 5000 Blackwell", "NVIDIA RTX PRO 4500 Blackwell", "NVIDIA RTX PRO 4000 Blackwell",
		"NVIDIA RTX PRO 4000 Blackwell SFF Edition", "NVIDIA RTX PRO 2000 Blackwell",
		"GeForce RTX 5090", "GeForce RTX 5080", "GeForce RTX 5070 Ti", "GeForce RTX 5070",
		"GeForce RTX 5060 Ti", "GeForce RTX 5060", "GeForce RTX 5050")...)
	result = append(result, nvidia(CategoryWorkstation, 8.9,
		"NVIDIA RTX 6000 Ada", "NVIDIA RTX 5000 Ada", "NVIDIA RTX 4500 Ada", "NVIDIA RTX 4000 Ada",
		"NVIDIA RTX 4000 SFF Ada", "NVIDIA RTX 2000 Ada", "GeForce RTX 4090", "GeForce RTX 4080",
		"GeForce RTX 4070 Ti", "GeForce RTX 4070", "GeForce RTX 4060 Ti", "GeForce RTX 4060", "GeForce RTX 4050")...)
	result = append(result, nvidia(CategoryWorkstation, 8.6,
		"NVIDIA RTX A6000", "NVIDIA RTX A5000", "NVIDIA RTX A4000", "NVIDIA RTX A3000", "NVIDIA RTX A2000",
		"GeForce RTX 3090 Ti", "GeForce RTX 3090", "GeForce RTX 3080 Ti", "GeForce RTX 3080",
		"GeForce RTX 3070 Ti", "GeForce RTX 3070", "GeForce RTX 3060 Ti", "GeForce RTX 3060",
		"GeForce RTX 3050 Ti", "GeForce RTX 3050")...)
	result = append(result, nvidia(CategoryWorkstation, 7.5,
		"QUADRO RTX 8000", "QUADRO RTX 6000", "QUADRO RTX 5000", "QUADRO RTX 4000", "QUADRO RTX 3000",
		"QUADRO T2000", "NVIDIA T1200", "NVIDIA T1000", "NVIDIA T600", "NVIDIA T500", "NVIDIA T400",
		"GeForce GTX 1650 Ti", "NVIDIA TITAN RTX", "GeForce RTX 2080 Ti", "GeForce RTX 2080",
		"GeForce RTX 2070", "GeForce RTX 2060")...)
	// Representative AMD choices for hypothetical planning, not a runtime
	// support or GPU-memory-capacity claim. Device eligibility uses probe facts.
	result = append(result,
		GPU{Vendor: accelerator.VendorAMD, Name: "AMD Instinct MI300X", Category: CategoryDataCenter, Architecture: "CDNA 3", ArchID: "gfx942"},
		GPU{Vendor: accelerator.VendorAMD, Name: "AMD Radeon 8060S (AMD395)", Category: CategoryWorkstation, Architecture: "RDNA 3.5", ArchID: "gfx1151"},
	)
	result = append(result, apple("M5", "", "Pro", "Max")...)
	result = append(result, apple("M4", "", "Pro", "Max")...)
	result = append(result, apple("M3", "", "Pro", "Max", "Ultra")...)
	result = append(result, apple("M2", "", "Pro", "Max", "Ultra")...)
	result = append(result, apple("M1", "", "Pro", "Max", "Ultra")...)

	result = append(result, domestic(accelerator.VendorAscend, "Ascend 310P", "Ascend 910B", "Ascend 910C", "Ascend 950PR", "Ascend 950DT")...)
	result = append(result, domestic(accelerator.VendorHygon, "Hygon K100", "Hygon K500", "Hygon BW1000", "Hygon BW1100")...)
	result = append(result, domestic(accelerator.VendorPPU, "PPU ZW810E", "PPU M890")...)
	result = append(result, GPU{Vendor: accelerator.VendorKunlunxin, Name: "Kunlunxin P800 PCIe", Category: CategoryDomestic, Architecture: "KL3"})
	return result
}()
