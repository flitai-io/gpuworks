package accelerator

// Shared sources of the NVIDIA card specs.
var (
	// nvidiaNVLinkPage states fourth-generation NVLink, NVIDIA Hopper's, at
	// 900 GB/s per GPU and NVLink bandwidth as bidirectional.
	nvidiaNVLinkPage = SpecSource{
		URL:       "https://www.nvidia.com/en-us/data-center/nvlink/",
		Title:     "NVIDIA NVLink and NVLink Switch",
		Version:   "web page, NVLink generations table",
		Retrieved: "2026-11-28",
	}
	// nvidiaH100Whitepaper prints dense and sparse rates side by side
	// (Table 1, Table 3), and states PCIe Gen 5 as 128 GB/s total, 64 GB/s
	// in each direction.
	nvidiaH100Whitepaper = SpecSource{
		URL:       "https://resources.nvidia.com/en-us-hopper-architecture/nvidia-h100-tensor-c",
		Title:     "NVIDIA H100 Tensor Core GPU Architecture",
		Version:   "V1.04, Table 1 and Table 3",
		Retrieved: "2026-11-28",
	}
)

// nvidiaCardSpecs are the NVIDIA models whose figures the platform shows.
//
// Models the platform has seen but cannot list yet, for want of a public
// NVIDIA document: L20, H20, H20-3e and H800 (China-market parts NVIDIA
// publishes no datasheet for). A100 80GB PCIe has a datasheet but no name
// read off real hardware.
var nvidiaCardSpecs = []CardSpec{
	{
		// The SXM5 part: Table 3 of the whitepaper gives HBM3 to the SXM5
		// board and HBM2e to the PCIe one, and the node the name was read
		// on had NVSwitch devices.
		Name: "H100 80GB HBM3",
		Peaks: []Peak{
			{Precision: PrecisionBF16, Stated: 989.4},
			{Precision: PrecisionFP16, Stated: 989.4},
			{Precision: PrecisionFP8, Stated: 1978.9},
			{Precision: PrecisionINT8, Stated: 1978.9},
			{Precision: PrecisionTF32, Stated: 494.7},
			{Precision: PrecisionFP32, Stated: 66.9},
		},
		MemoryBandwidthGBps: 3350,
		Interconnects: []Interconnect{
			{Kind: InterconnectNVLink, Generation: 4, GBps: 900, Bidirectional: true},
			{Kind: InterconnectPCIe, Generation: 5, GBps: 128, Bidirectional: true},
		},
		Sources: []SpecSource{
			nvidiaH100Whitepaper.covering(FactCompute, FactInterconnectDirection),
			{
				URL:       "https://www.nvidia.com/en-us/data-center/h100/",
				Title:     "NVIDIA H100 GPU",
				Version:   "web page, Product Specifications table, H100 SXM column",
				Retrieved: "2026-11-28",
				Covers:    []SpecFact{FactMemoryBandwidth, FactInterconnect},
			},
			nvidiaNVLinkPage.covering(FactInterconnect, FactInterconnectDirection),
		},
		NameSeen: "GPU listing of a Slurm job on a Together AI node with eight H100 and four NVSwitch devices, 2026-11-13: all eight cards read NVIDIA H100 80GB HBM3",
	},
	{
		// The SXM part: the node the name was read on ran its cards at a
		// 700 W limit, which the H200 page gives to SXM (NVL: up to 600 W).
		Name: "H200",
		Peaks: []Peak{
			{Precision: PrecisionBF16, Stated: 1979, Sparse: true},
			{Precision: PrecisionFP16, Stated: 1979, Sparse: true},
			{Precision: PrecisionFP8, Stated: 3958, Sparse: true},
			{Precision: PrecisionINT8, Stated: 3958, Sparse: true},
			{Precision: PrecisionTF32, Stated: 989, Sparse: true},
			{Precision: PrecisionFP32, Stated: 67},
		},
		MemoryBandwidthGBps: 4800,
		Interconnects: []Interconnect{
			{Kind: InterconnectNVLink, Generation: 4, GBps: 900, Bidirectional: true},
			{Kind: InterconnectPCIe, Generation: 5, GBps: 128, Bidirectional: true},
		},
		Sources: []SpecSource{
			{
				URL:       "https://www.nvidia.com/en-us/data-center/h200/",
				Title:     "NVIDIA H200 GPU",
				Version:   "web page, Specifications table, H200 SXM column (marked preliminary; Tensor Core rows with sparsity)",
				Retrieved: "2026-11-28",
				Covers:    []SpecFact{FactCompute, FactMemoryBandwidth, FactInterconnect},
			},
			nvidiaNVLinkPage.covering(FactInterconnect, FactInterconnectDirection),
			// For PCIe Gen 5 x16 only: 64 GB/s in each direction.
			nvidiaH100Whitepaper.covering(FactInterconnectDirection),
		},
		NameSeen: "nvidia-smi -L on a cloud node with eight H200 at a 700 W power limit, 2026-09-22: NVIDIA H200, 143771 MiB each",
	},
}
