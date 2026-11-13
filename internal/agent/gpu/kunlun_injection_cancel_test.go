package gpu

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Inventory is not the last operation in an injection: cancellation can arrive
// while the selected device identities are being checked against sysfs.
func TestKunlunInjectionCanceledDuringDeviceValidation(t *testing.T) {
	p, _ := kunlunFixture(t)
	if _, err := p.Probe(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	inspect := p.stat
	p.stat = func(path string) (os.FileInfo, error) {
		info, err := inspect(path)
		if path == "/dev/xpuctrl" {
			cancel()
		}
		return info, err
	}
	injection := p.runtimeInjectionForDriver(ctx, []int{0, 1}, "docker")
	if !strings.Contains(injection.UnsupportedReason, "context canceled") {
		t.Fatalf("canceled device validation accepted: %+v", injection)
	}
	if len(injection.DockerArgs) != 0 || len(injection.Env) != 0 || len(injection.Mounts) != 0 {
		t.Fatalf("canceled validation exposed partial resources: %+v", injection)
	}
}
