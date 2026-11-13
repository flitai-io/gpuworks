package accelerator

import (
	"strings"
	"testing"
)

func TestClassifyKubeResource(t *testing.T) {
	for _, c := range []struct {
		resource string
		kind     KubeResourceKind
		vendor   string
	}{
		{"nvidia.com/gpu", KubeResourceCard, VendorNVIDIA},
		{"nvidia.com/gpu.shared", KubeResourceShared, VendorNVIDIA},
		{"nvidia.com/mig-1g.10gb", KubeResourceShared, VendorNVIDIA},
		{"nvidia.com/gpumem", KubeResourceShared, VendorNVIDIA},
		{"nvidia.com/gpucores", KubeResourceShared, VendorNVIDIA},
		{"huawei.com/Ascend910", KubeResourceCard, VendorAscend},
		{"huawei.com/Ascend310P", KubeResourceCard, VendorAscend},
		{"huawei.com/npu", KubeResourceCard, VendorAscend},
		{"huawei.com/Ascend910-2c", KubeResourceShared, VendorAscend},
		{"huawei.com/Ascend310P-4c.3cpu", KubeResourceShared, VendorAscend},
		{"huawei.com/npu.sliced", KubeResourceShared, VendorAscend},
		{"hygon.com/dcu", KubeResourceCard, VendorHygon},
		{"hygon.com/dcunum", KubeResourceShared, VendorHygon},
		{"amd.com/gpu", KubeResourceCard, VendorAMD},
		{"amd.com/gpu.partitioned.cpx", KubeResourceShared, VendorAMD},
		{"kunlunxin.com/xpu", KubeResourceCard, VendorKunlunxin},
		{"baidu.com/xpu", KubeResourceCard, VendorKunlunxin},
		{"kunlunxin.com/xpu.shared", KubeResourceShared, VendorKunlunxin},
		{"baidu.com/xpu.shared", KubeResourceShared, VendorKunlunxin},
		{"kunlunxin.com/other", KubeResourceOther, ""},
		{"baidu.com/other", KubeResourceOther, ""},
		{"aliyun.com/gpu-mem", KubeResourceShared, VendorNVIDIA},
		{"aliyun.com/gpu-count", KubeResourceShared, VendorNVIDIA},
		{"aliyun.com/gpu-core.percentage", KubeResourceShared, VendorNVIDIA},
		{"aliyun.com/other", KubeResourceOther, ""},
		{"alibabacloud.com/ppu", KubeResourceCard, VendorPPU},
		{"alibabacloud.com/ppu.partitioned.mig-1g", KubeResourceShared, VendorPPU},
		{"cpu", KubeResourceOther, ""},
		{"example.com/gpu", KubeResourceOther, ""},
		{"nvidia.com/other", KubeResourceOther, ""},
	} {
		kind, vendor := ClassifyKubeResource(c.resource)
		if kind != c.kind || vendor != c.vendor {
			t.Errorf("ClassifyKubeResource(%q) = %v %q, want %v %q", c.resource, kind, vendor, c.kind, c.vendor)
		}
	}
}

func TestKubeVendors(t *testing.T) {
	if got := strings.Join(KubeVendors(), ","); got != "amd,ascend,hygon,kunlunxin,nvidia,ppu" {
		t.Fatalf("KubeVendors = %s", got)
	}
	for v, want := range map[string]bool{"NVIDIA": true, "ascend": true, "amd": true, "kunlunxin": true, "apple": false, "": false} {
		if RunsOnKube(v) != want {
			t.Fatalf("RunsOnKube(%q) = %v", v, !want)
		}
	}
}
