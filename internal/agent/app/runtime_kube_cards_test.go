package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// kubeCardsProber is a vendor prober with per-card injection results. It
// refuses what it was told to refuse, like a real injector verifying the
// card's device nodes, and answers the driver it is asked for.
type kubeCardsProber struct {
	cards    []proto.GPUInfo
	probeErr error
	refuse   map[int]string
	drivers  []string
}

func (p *kubeCardsProber) Vendor() string                       { return "amd" }
func (p *kubeCardsProber) Available() bool                      { return true }
func (p *kubeCardsProber) DriverVersion(context.Context) string { return "" }
func (p *kubeCardsProber) Topology(context.Context) string      { return "" }
func (p *kubeCardsProber) Metrics(context.Context) ([]gpu.Metric, error) {
	return nil, nil
}
func (p *kubeCardsProber) Probe(context.Context) ([]proto.GPUInfo, error) {
	return p.cards, p.probeErr
}
func (p *kubeCardsProber) RuntimeInjection(indexes []int) gpu.RuntimeInjection {
	return p.RuntimeInjectionForDriver(indexes, "")
}
func (p *kubeCardsProber) RuntimeInjectionForDriver(indexes []int, driver string) gpu.RuntimeInjection {
	p.drivers = append(p.drivers, driver)
	if reason := p.refuse[indexes[0]]; reason != "" {
		return gpu.RuntimeInjection{UnsupportedReason: reason}
	}
	node := "/dev/dri/renderD" + string(rune('0'+indexes[0]))
	return gpu.RuntimeInjection{DockerArgs: []string{"--device", "/dev/kfd:/dev/kfd:rwm", "--device", node + ":" + node + ":rwm"}}
}

func twoAMDCards() []proto.GPUInfo {
	return []proto.GPUInfo{
		{Index: 1, UUID: "amd-1234", PCIeBDF: "0000:43:00.0"},
		{Index: 2, UUID: "amd-5678", PCIeBDF: "0000:44:00.0"},
		{Index: -1, UUID: "amd-ignored", PCIeBDF: "0000:45:00.0"},
	}
}

func TestKubeCardIdentities(t *testing.T) {
	p := &kubeCardsProber{cards: append(twoAMDCards(), proto.GPUInfo{Index: 3})}
	uuids, err := cardUUIDs(p)(t.Context())
	if err != nil || !reflect.DeepEqual(uuids, map[int]string{1: "amd-1234", 2: "amd-5678"}) {
		t.Fatalf("cardUUIDs = %v, %v", uuids, err)
	}
	bdfs, err := cardBDFs(p)(t.Context())
	if err != nil || !reflect.DeepEqual(bdfs, map[int]string{1: "0000:43:00.0", 2: "0000:44:00.0"}) {
		t.Fatalf("cardBDFs = %v, %v", bdfs, err)
	}
	p.probeErr = errors.New("sysfs unreadable")
	if _, err := cardBDFs(p)(t.Context()); err == nil {
		t.Fatal("a failed probe gave addresses")
	}
	if got, err := cardBDFs(nil)(t.Context()); err != nil || len(got) != 0 {
		t.Fatalf("cardBDFs(nil) = %v, %v", got, err)
	}
}

// A card whose injection fails is not offered; the others are, as the
// kube runtime's driver name asks for.
func TestCardDevicesSkipsACardWhoseInjectionFails(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	p := &kubeCardsProber{cards: twoAMDCards(), refuse: map[int]string{1: "AMD: /dev/dri/renderD1 unavailable: no such file or directory"}}
	got, err := cardDevices(p, log)(t.Context())
	if err != nil {
		t.Fatalf("one bad card failed the whole node: %v", err)
	}
	want := map[int][]string{2: {"/dev/kfd:/dev/kfd:rwm", "/dev/dri/renderD2:/dev/dri/renderD2:rwm"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cardDevices = %v, want %v", got, want)
	}
	if !strings.Contains(logs.String(), "card=1") || !strings.Contains(logs.String(), "renderD1 unavailable") {
		t.Fatalf("the skipped card is not logged with its reason: %s", logs.String())
	}
	for _, d := range p.drivers {
		if d != "kube" {
			t.Fatalf("injection asked for driver %q", d)
		}
	}
}

func TestCardDevicesFailsWhenTheNodeCannotBeProbed(t *testing.T) {
	p := &kubeCardsProber{cards: twoAMDCards(), probeErr: errors.New("sysfs unreadable")}
	if _, err := cardDevices(p, slog.New(slog.DiscardHandler))(t.Context()); err == nil || !strings.Contains(err.Error(), "sysfs unreadable") {
		t.Fatalf("cardDevices = %v", err)
	}
}

// Every card refused: nothing is offered, and that is not an error (the
// node's cards are used up or broken, the agent still runs).
func TestCardDevicesOffersNothingWhenEveryCardIsRefused(t *testing.T) {
	p := &kubeCardsProber{cards: twoAMDCards(), refuse: map[int]string{1: "bad", 2: "bad"}}
	got, err := cardDevices(p, slog.New(slog.DiscardHandler))(t.Context())
	if err != nil || len(got) != 0 {
		t.Fatalf("cardDevices = %v, %v", got, err)
	}
}
