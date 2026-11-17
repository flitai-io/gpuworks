package engine

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// residueProber is a two-card NVIDIA-shaped host: Metrics reports the full
// normalized metric set per card, as the real prober does, and the compute
// process listing carries the card addresses.
type residueProber struct {
	gpu.FixedProber
	used       map[int]float64 // index → used bytes; a missing index has no row
	metricsErr error
	procs      []gpu.ComputeProcess
	procsErr   error
	reads      int
}

func newResidueProber(used map[int]float64) *residueProber {
	return &residueProber{
		FixedProber: gpu.FixedProber{VendorName: "nvidia", GPUs: []proto.GPUInfo{
			{Index: 0, UUID: "GPU-aa", Name: "A100", PCIeBDF: "0000:3f:00.0"},
			{Index: 1, UUID: "GPU-bb", Name: "A100", PCIeBDF: "0000:40:00.0"},
		}},
		used: used,
	}
}

func (p *residueProber) Metrics(context.Context) ([]gpu.Metric, error) {
	p.reads++
	if p.metricsErr != nil {
		return nil, p.metricsErr
	}
	var out []gpu.Metric
	for index, used := range p.used {
		out = append(out, gpu.Metric{Index: index, Vendor: "nvidia", Values: map[string]float64{
			"gpuworks_gpu_utilization":        0,
			"gpuworks_gpu_memory_total_bytes": 80 << 30,
			"gpuworks_gpu_memory_used_bytes":  used,
		}})
	}
	return out, nil
}

func (p *residueProber) ComputeProcesses(context.Context) ([]gpu.ComputeProcess, error) {
	return p.procs, p.procsErr
}

func newResidueEngine(t *testing.T, prober gpu.Prober) (*Engine, chan proto.InstanceStatus) {
	t.Helper()
	reported := make(chan proto.InstanceStatus, 16)
	e := NewWithProber(&headlessTestDriver{}, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir(), prober)
	e.residueSettle = 0
	return e, reported
}

func residueSpec(h *sleepHTTPServer, t *testing.T, uuid string, indexes ...int) proto.InstanceDesiredState {
	return proto.InstanceDesiredState{
		UUID: uuid, DesiredState: "running", Sleepable: true, GPUIndexes: indexes,
		APIPort: h.port(t), Ports: []int{h.port(t)},
	}
}

func TestSleepReportsMeasuredResidueWhenAloneOnItsCard(t *testing.T) {
	h := newSleepHTTPServer(t)
	prober := newResidueProber(map[int]float64{0: 1.5 * (1 << 30), 1: 70 << 30})
	prober.procs = []gpu.ComputeProcess{{PID: 41, Name: "VLLM::Worker", PCIeBDF: "00000000:3F:00.0", MemoryBytes: 1 << 30}}
	e, reported := newResidueEngine(t, prober)
	spec := residueSpec(h, t, "res-1", 0)
	e.desired[spec.UUID] = spec

	spec.DesiredSleep = 1
	e.reconcileSleep(context.Background(), spec)
	st := <-reported
	if st.SleepLevel != 1 || st.SleepResidueBytes != int64(1.5*(1<<30)) {
		t.Fatalf("report = level %d residue %d, want level 1 residue %d", st.SleepLevel, st.SleepResidueBytes, int64(1.5*(1<<30)))
	}
}

func TestSleepMultiGPUResidueIsTheLargestCard(t *testing.T) {
	h := newSleepHTTPServer(t)
	prober := newResidueProber(map[int]float64{0: 2 << 30, 1: 3 << 30})
	e, reported := newResidueEngine(t, prober)
	spec := residueSpec(h, t, "res-2", 0, 1)
	e.desired[spec.UUID] = spec

	spec.DesiredSleep = 2
	e.reconcileSleep(context.Background(), spec)
	if st := <-reported; st.SleepResidueBytes != 3<<30 {
		t.Fatalf("residue = %d, want the larger card's %d", st.SleepResidueBytes, int64(3<<30))
	}
}

func TestSleepResidueIsUnmeasuredWhenAnotherInstanceSharesACard(t *testing.T) {
	cases := map[string]proto.InstanceDesiredState{
		"same index": {UUID: "other", DesiredState: "running", GPUIndexes: []int{1, 2}},
		"same ident": {UUID: "other", DesiredState: "running", GPUIndexes: []int{7}, GPUIdents: []string{"pci:0000:3f:00.0"}},
	}
	for name, other := range cases {
		t.Run(name, func(t *testing.T) {
			h := newSleepHTTPServer(t)
			prober := newResidueProber(map[int]float64{0: 2 << 30, 1: 2 << 30})
			e, reported := newResidueEngine(t, prober)
			spec := residueSpec(h, t, "res-3", 0, 1)
			spec.GPUIdents = []string{"pci:0000:3f:00.0", "pci:0000:40:00.0"}
			e.desired[spec.UUID] = spec
			e.desired[other.UUID] = other

			spec.DesiredSleep = 1
			e.reconcileSleep(context.Background(), spec)
			st := <-reported
			if st.SleepLevel != 1 || st.SleepResidueBytes != 0 {
				t.Fatalf("report = level %d residue %d, want level 1 and unmeasured", st.SleepLevel, st.SleepResidueBytes)
			}
			if prober.reads != 0 {
				t.Fatalf("metrics read %d times for a shared card", prober.reads)
			}
		})
	}
}

func TestSleepResidueIgnoresInstancesOnOtherCards(t *testing.T) {
	h := newSleepHTTPServer(t)
	prober := newResidueProber(map[int]float64{0: 2 << 30, 1: 9 << 30})
	e, reported := newResidueEngine(t, prober)
	spec := residueSpec(h, t, "res-4", 0)
	e.desired[spec.UUID] = spec
	e.desired["neighbour"] = proto.InstanceDesiredState{UUID: "neighbour", DesiredState: "running", GPUIndexes: []int{1}}

	spec.DesiredSleep = 1
	e.reconcileSleep(context.Background(), spec)
	if st := <-reported; st.SleepResidueBytes != 2<<30 {
		t.Fatalf("residue = %d, want the instance's own card %d", st.SleepResidueBytes, int64(2<<30))
	}
}

func TestSleepResidueFollowsTheCardToItsCurrentIndex(t *testing.T) {
	h := newSleepHTTPServer(t)
	// The spec was placed on index 0, but the card now answers as index 1.
	prober := newResidueProber(map[int]float64{0: 8 << 30, 1: 2 << 30})
	e, reported := newResidueEngine(t, prober)
	spec := residueSpec(h, t, "res-5", 0)
	spec.GPUIdents = []string{"pci:0000:40:00.0"}
	e.desired[spec.UUID] = spec

	spec.DesiredSleep = 1
	e.reconcileSleep(context.Background(), spec)
	if st := <-reported; st.SleepResidueBytes != 2<<30 {
		t.Fatalf("residue = %d, want the bound card's %d, not the old index's", st.SleepResidueBytes, int64(2<<30))
	}
}

func TestSleepResidueIsUnmeasuredWhenAForeignProcessIsOnTheCard(t *testing.T) {
	h := newSleepHTTPServer(t)
	prober := newResidueProber(map[int]float64{0: 20 << 30})
	prober.procs = []gpu.ComputeProcess{
		{PID: 41, Name: "VLLM::Worker", PCIeBDF: "0000:3f:00.0"},
		{PID: 99, Name: "python", PCIeBDF: "0000:3f:00.0"},
	}
	e, reported := newResidueEngine(t, prober)
	spec := residueSpec(h, t, "res-6", 0)
	e.desired[spec.UUID] = spec

	spec.DesiredSleep = 1
	e.reconcileSleep(context.Background(), spec)
	st := <-reported
	if st.SleepLevel != 1 || st.SleepResidueBytes != 0 {
		t.Fatalf("report = level %d residue %d, want level 1 and unmeasured", st.SleepLevel, st.SleepResidueBytes)
	}
}

func TestSleepResidueIsUnmeasuredWhenProcessesCannotBeListed(t *testing.T) {
	h := newSleepHTTPServer(t)
	prober := newResidueProber(map[int]float64{0: 2 << 30})
	prober.procsErr = errors.New("nvidia-smi: timed out")
	e, reported := newResidueEngine(t, prober)
	spec := residueSpec(h, t, "res-7", 0)
	e.desired[spec.UUID] = spec

	spec.DesiredSleep = 1
	e.reconcileSleep(context.Background(), spec)
	if st := <-reported; st.SleepLevel != 1 || st.SleepResidueBytes != 0 {
		t.Fatalf("report = level %d residue %d, want level 1 and unmeasured", st.SleepLevel, st.SleepResidueBytes)
	}
}

func TestSleepStillSucceedsWhenMetricsFailOrLackTheCard(t *testing.T) {
	cases := map[string]*residueProber{
		"metrics error":    {FixedProber: newResidueProber(nil).FixedProber, metricsErr: errors.New("nvidia-smi: driver not loaded")},
		"card has no data": newResidueProber(map[int]float64{1: 4 << 30}),
		"no metrics":       newResidueProber(nil),
	}
	for name, prober := range cases {
		t.Run(name, func(t *testing.T) {
			h := newSleepHTTPServer(t)
			e, reported := newResidueEngine(t, prober)
			spec := residueSpec(h, t, "res-8", 0)
			e.desired[spec.UUID] = spec

			spec.DesiredSleep = 1
			e.reconcileSleep(context.Background(), spec)
			st := <-reported
			if st.State != proto.InstRunning || st.SleepLevel != 1 || st.Phase != "" || st.SleepResidueBytes != 0 {
				t.Fatalf("report = %+v, want a successful sleep with no residue", st)
			}
			if got := e.processOfLocked(spec.UUID).sleepLevel; got != 1 {
				t.Fatalf("level = %d, want 1", got)
			}
		})
	}
}

func TestWakeNeverCarriesAResidue(t *testing.T) {
	h := newSleepHTTPServer(t)
	prober := newResidueProber(map[int]float64{0: 2 << 30})
	e, reported := newResidueEngine(t, prober)
	spec := residueSpec(h, t, "res-9", 0)
	e.desired[spec.UUID] = spec
	e.setSleepLevelLocked(spec.UUID, 1)

	spec.DesiredSleep = 0
	e.reconcileSleep(context.Background(), spec)
	st := <-reported
	if st.SleepLevel != 0 || st.SleepResidueBytes != 0 {
		t.Fatalf("wake report = level %d residue %d", st.SleepLevel, st.SleepResidueBytes)
	}
	if prober.reads != 0 {
		t.Fatalf("a wake read the card %d times", prober.reads)
	}
}

func TestFailedSleepNeverCarriesAResidue(t *testing.T) {
	h := newSleepHTTPServer(t)
	h.status["/sleep"] = 500
	prober := newResidueProber(map[int]float64{0: 2 << 30})
	e, reported := newResidueEngine(t, prober)
	spec := residueSpec(h, t, "res-10", 0)
	e.desired[spec.UUID] = spec

	spec.DesiredSleep = 1
	e.reconcileSleep(context.Background(), spec)
	st := <-reported
	if st.Phase != proto.PhaseSleep || st.SleepLevel != 0 || st.SleepResidueBytes != 0 {
		t.Fatalf("failed sleep report = %+v", st)
	}
}

func TestSleepThatTookEffectDespiteATransportFailureCarriesAResidue(t *testing.T) {
	h := newSleepHTTPServer(t)
	h.drop["/sleep"] = true
	h.probeBodies = []string{`{"is_sleeping":true}`}
	prober := newResidueProber(map[int]float64{0: 2 << 30})
	e, reported := newResidueEngine(t, prober)
	spec := residueSpec(h, t, "res-11", 0)
	e.desired[spec.UUID] = spec

	spec.DesiredSleep = 1
	e.reconcileSleep(context.Background(), spec)
	st := <-reported
	if st.SleepLevel != 1 || st.Phase != "" || st.SleepResidueBytes != 2<<30 {
		t.Fatalf("report = %+v, want a successful level 1 with the measured residue", st)
	}
}

func TestSleepResidueWaitsForTheSettleTimeAndStopsOnCancel(t *testing.T) {
	h := newSleepHTTPServer(t)
	prober := newResidueProber(map[int]float64{0: 2 << 30})
	e, _ := newResidueEngine(t, prober)
	e.residueSettle = time.Hour
	spec := residueSpec(h, t, "res-12", 0)
	e.desired[spec.UUID] = spec

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int64, 1)
	go func() { done <- e.measureSleepResidue(ctx, spec) }()
	select {
	case got := <-done:
		t.Fatalf("measured %d without waiting for the card to settle", got)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case got := <-done:
		if got != 0 || prober.reads != 0 {
			t.Fatalf("cancelled measure = %d with %d reads, want 0 and none", got, prober.reads)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("measure ignored cancellation")
	}
}

// The process a sleeper keeps on its card is learned when its residue is
// measured; the card is then one whose foreign processes the node can
// report. A wake forgets it, and a card with an awake instance of ours is
// not offered.
func TestASleepersOwnProcessIsKnownUntilItWakes(t *testing.T) {
	h := newSleepHTTPServer(t)
	prober := newResidueProber(map[int]float64{0: 600 << 20})
	prober.procs = []gpu.ComputeProcess{{PID: 41, Name: "VLLM::Worker", PCIeBDF: "00000000:3F:00.0"}}
	e, reported := newResidueEngine(t, prober)
	spec := residueSpec(h, t, "res-pid", 0)
	spec.GPUIdents = []string{"pci:0000:3f:00.0"}
	e.desired[spec.UUID] = spec

	if got := e.SleepingCardPIDs(); len(got) != 0 {
		t.Fatalf("an awake instance's card = %v, want none", got)
	}
	spec.DesiredSleep = 1
	e.reconcileSleep(context.Background(), spec)
	<-reported
	got := e.SleepingCardPIDs()
	if len(got) != 1 || !got["pci:0000:3f:00.0"][41] {
		t.Fatalf("sleeping card pids = %v, want pid 41 on pci:0000:3f:00.0", got)
	}

	// Another instance of ours, awake, on the same card: its processes are
	// not known, so the card is not offered.
	e.desired["awake"] = proto.InstanceDesiredState{UUID: "awake", DesiredState: "running", GPUIndexes: []int{0}, GPUIdents: []string{"pci:0000:3f:00.0"}}
	if got := e.SleepingCardPIDs(); len(got) != 0 {
		t.Fatalf("a card with an awake instance of ours = %v, want none", got)
	}
	delete(e.desired, "awake")

	spec.DesiredSleep = 0
	e.reconcileSleep(context.Background(), spec)
	<-reported
	if got := e.SleepingCardPIDs(); len(got) != 0 {
		t.Fatalf("after the wake = %v, want none", got)
	}
}
