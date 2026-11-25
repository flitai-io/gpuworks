package accelerator

// kunlunVendor is Kunlunxin. PCI identity measured 2026-10-28: P800
// 2057:3686 class 0x0780 bound to kunlun, eight cards on each of two hosts
// (the node11 and node12 captures in internal/agent/gpu/testdata).
var kunlunVendor = vendorSpec{
	id:           VendorKunlunxin,
	brand:        "Kunlunxin",
	frameworks:   []string{"xre"},
	named:        either(contains("kunlunxin", "kunlun"), exactly("xre")),
	precedence:   10,
	namePrefixes: []string{"kunlunxin"},
	domestic:     true,

	// xpu-smi is not a detectCommand: Intel uses that name too.
	tool:           "xpu-smi -q -x",
	controlDevices: []string{"/dev/xpuctrl"},
	pciID:          0x2057,
	pci:            &pciVendor{classes: []uint32{pciClassOtherCommunication}, drivers: []string{"kunlun"}},
	arch:           kunlunArch,

	runtimes:        []string{RuntimeDocker, RuntimeNerdctl, RuntimeKube},
	hostOS:          "linux",
	hostArch:        "amd64",
	explicitDevices: true,
	// Engine sleep and PD are not offered on the P800.
	supports:            []Feature{FeatureMultiNode, FeatureKVDiskTier, FeaturePipelineParallel, FeatureDataParallel, FeatureExternalKVCache},
	collectiveInterface: true,
	ownCatalogImage:     true,

	cardChecks:  true,
	healthGuide: kunlunHealthGuide,
}
