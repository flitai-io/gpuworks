package app

import (
	"context"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestDoctorUCXNetlinkHangReportsAnAffectedHostWithLiteralFixes(t *testing.T) {
	old := doctorUCXNetlink
	t.Cleanup(func() { doctorUCXNetlink = old })

	doctorUCXNetlink = func(context.Context) (*proto.UCXNetlinkHang, bool) {
		return &proto.UCXNetlinkHang{Kernel: "5.15.0-203-generic"}, true
	}
	var r proto.DoctorReport
	doctorUCXNetlinkHang(context.Background(), &r)
	if len(r.Findings) != 1 {
		t.Fatalf("findings = %+v", r.Findings)
	}
	f := r.Findings[0]
	fields := map[string]string{}
	for _, e := range f.Evidence {
		fields[e.Name] = e.Value
	}
	if f.Code != proto.DoctorUCXNetlinkHang || f.Level != proto.DoctorLevelWarning || fields["kernel"] != "5.15.0-203-generic" ||
		fields["fix"] != "sudo sysctl -w net.ipv6.conf.all.disable_ipv6=1 net.ipv6.conf.default.disable_ipv6=1" ||
		!strings.Contains(fields["fix_persist"], "/etc/sysctl.d/90-gpuworks-ucx.conf") {
		t.Fatalf("finding = %+v", f)
	}
	r.Report = renderDoctorText(r)
	if !strings.Contains(r.Report, "ucx_netlink_hang") || !strings.Contains(r.Report, "disable_ipv6=1") {
		t.Fatalf("text = %s", r.Report)
	}

	for name, answer := range map[string]func(context.Context) (*proto.UCXNetlinkHang, bool){
		"not affected": func(context.Context) (*proto.UCXNetlinkHang, bool) { return nil, true },
		"unknown":      func(context.Context) (*proto.UCXNetlinkHang, bool) { return nil, false },
	} {
		doctorUCXNetlink = answer
		var clean proto.DoctorReport
		doctorUCXNetlinkHang(context.Background(), &clean)
		if len(clean.Findings) != 0 {
			t.Errorf("%s: findings = %+v", name, clean.Findings)
		}
	}
}
