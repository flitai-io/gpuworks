package accelerator

import (
	"regexp"
	"sort"
	"strings"
)

// kubeVendors are the vendors whose cards the agent runs on a Kubernetes
// node. It needs no vendor's mounts or runtime for any of them: it reads
// the host's files through the host's root and runs the vendor's CLIs in
// the host's mount namespace. A node with no card of these stands by.
var kubeVendors = map[string]bool{VendorNVIDIA: true, VendorAscend: true, VendorHygon: true, VendorPPU: true, VendorAMD: true, VendorKunlunxin: true}

// RunsOnKube reports whether the agent runs the vendor's cards on
// Kubernetes.
func RunsOnKube(vendor string) bool { return kubeVendors[NormalizeVendor(vendor)] }

// KubeVendors lists the vendors that run on Kubernetes, sorted.
func KubeVendors() []string {
	out := make([]string, 0, len(kubeVendors))
	for v := range kubeVendors {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// KubeResourceKind says what a Kubernetes extended resource is to the
// platform.
type KubeResourceKind int

const (
	// KubeResourceOther is not an accelerator resource.
	KubeResourceOther KubeResourceKind = iota
	// KubeResourceCard is one whole card per unit.
	KubeResourceCard
	// KubeResourceShared hands out parts of cards (time-slicing, MIG, HAMi,
	// vNPU, ACK GPU sharing): a unit is not a card the platform could own.
	KubeResourceShared
)

var ascendSlice = regexp.MustCompile(`^Ascend[0-9A-Za-z]*-\d+c`)

// ClassifyKubeResource classifies an extended resource a device plugin
// advertises. The vendors' own plugins name whole cards nvidia.com/gpu,
// huawei.com/Ascend910 (or Ascend310P, npu…), hygon.com/dcu, amd.com/gpu and
// alibabacloud.com/ppu, and Kunlun's kunlunxin.com/xpu (baidu.com/xpu on
// Baidu CCE); the names below them are partitions or shares of a
// card, and so is any ".shared", ".sliced" or ".partitioned" variant.
func ClassifyKubeResource(resource string) (KubeResourceKind, string) {
	domain, name, ok := strings.Cut(resource, "/")
	if !ok {
		return KubeResourceOther, ""
	}
	vendor := ""
	switch domain {
	case "nvidia.com":
		vendor = VendorNVIDIA
	case "huawei.com":
		vendor = VendorAscend
	case "hygon.com":
		vendor = VendorHygon
	case "amd.com":
		vendor = VendorAMD
	case "alibabacloud.com":
		vendor = VendorPPU
	case "kunlunxin.com", "baidu.com":
		vendor = VendorKunlunxin
	case "aliyun.com":
		// ACK's GPU sharing counts GiB of card memory, a card count and a
		// compute share, never cards.
		switch name {
		case "gpu-mem", "gpu-count", "gpu-core.percentage":
			return KubeResourceShared, VendorNVIDIA
		}
		return KubeResourceOther, ""
	default:
		return KubeResourceOther, ""
	}
	for _, part := range []string{".shared", ".sliced", ".partitioned"} {
		if strings.Contains(name, part) {
			return KubeResourceShared, vendor
		}
	}
	switch {
	case vendor == VendorNVIDIA && name == "gpu",
		vendor == VendorAMD && name == "gpu",
		vendor == VendorHygon && name == "dcu",
		vendor == VendorPPU && name == "ppu",
		vendor == VendorKunlunxin && name == "xpu",
		vendor == VendorAscend && (name == "npu" || (strings.HasPrefix(name, "Ascend") && !ascendSlice.MatchString(name))):
		return KubeResourceCard, vendor
	case vendor == VendorNVIDIA && (strings.HasPrefix(name, "mig-") || strings.HasPrefix(name, "gpumem") || strings.HasPrefix(name, "gpucores")),
		vendor == VendorHygon && strings.HasPrefix(name, "dcu"),
		vendor == VendorAscend && ascendSlice.MatchString(name):
		return KubeResourceShared, vendor
	}
	return KubeResourceOther, ""
}
