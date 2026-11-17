package probe

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

type fakeDriver struct {
	exitCode     int
	logs         string
	imageWait    time.Duration
	imageMissing bool
	imageErr     error
	runErr       error
	statusErr    error
	logsErr      error
	loadErr      error
	removeErr    error
	removeErrAt  int
	runs         []runtime.WorkloadSpec
	removed      []string
}

type archRestrictedBackend struct{ accelerator.Backend }

func (archRestrictedBackend) Matches(vendor, arch string) bool {
	return vendor == accelerator.VendorHygon && arch == "Z100"
}

func TestProbeBackendMatchUsesPhysicalGPUArchitecture(t *testing.T) {
	backend := archRestrictedBackend{}
	if !probeBackendMatchesGPU(backend, accelerator.VendorHygon, proto.GPUInfo{Vendor: accelerator.VendorHygon, ArchID: "Z100"}) {
		t.Fatal("supported GPU was not selected for behavior probing")
	}
	if probeBackendMatchesGPU(backend, accelerator.VendorHygon, proto.GPUInfo{Vendor: accelerator.VendorHygon, ArchID: "K100"}) {
		t.Fatal("unsupported GPU was selected for behavior probing")
	}
	if probeBackendMatchesGPU(backend, accelerator.VendorHygon, proto.GPUInfo{Vendor: accelerator.VendorNVIDIA, ArchID: "Z100"}) {
		t.Fatal("GPU of another vendor was selected for behavior probing")
	}
	// A card with no device index cannot be given to a probe container,
	// and one lent to a virtual machine is not this node's to probe.
	if probeBackendMatchesGPU(backend, accelerator.VendorHygon, proto.GPUInfo{Index: -1, Vendor: accelerator.VendorHygon, ArchID: "Z100"}) {
		t.Fatal("a card without a device index was selected for behavior probing")
	}
	if probeBackendMatchesGPU(backend, accelerator.VendorHygon, proto.GPUInfo{Vendor: accelerator.VendorHygon, ArchID: "Z100", Ownership: accelerator.CardPassedThrough}) {
		t.Fatal("a card passed through to a VM was selected for behavior probing")
	}
}

func (d *fakeDriver) Name() string                         { return "docker" }
func (d *fakeDriver) Detect(context.Context) (bool, error) { return true, nil }
func (d *fakeDriver) ImageExists(ctx context.Context, _ string) (bool, error) {
	if d.imageErr != nil {
		return false, d.imageErr
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-time.After(d.imageWait):
		return !d.imageMissing, nil
	}
}
func (d *fakeDriver) PullImage(context.Context, string, func(string)) error { return nil }
func (d *fakeDriver) Prepare(context.Context, runtime.WorkloadSpec) error   { return nil }
func (d *fakeDriver) Run(_ context.Context, spec runtime.WorkloadSpec) (string, error) {
	d.runs = append(d.runs, spec)
	if d.runErr != nil {
		return "", d.runErr
	}
	return spec.Name, nil
}
func (d *fakeDriver) Stop(context.Context, string) error { return nil }
func (d *fakeDriver) ListContainers(context.Context) ([]string, error) {
	return nil, nil
}
func (d *fakeDriver) RunningContainers(context.Context) ([]string, error) {
	return nil, nil
}
func (d *fakeDriver) Remove(_ context.Context, id string) error {
	d.removed = append(d.removed, id)
	if d.removeErr != nil && len(d.removed) == d.removeErrAt {
		return d.removeErr
	}
	return nil
}
func (d *fakeDriver) Status(context.Context, string) (runtime.Status, error) {
	if d.statusErr != nil {
		return runtime.Status{}, d.statusErr
	}
	return runtime.Status{Exists: true, ExitCode: d.exitCode}, nil
}
func (d *fakeDriver) Logs(context.Context, string, bool, int) (io.ReadCloser, error) {
	if d.logsErr != nil {
		return nil, d.logsErr
	}
	return io.NopCloser(strings.NewReader(d.logs)), nil
}
func (d *fakeDriver) Exec(context.Context, string, []string) ([]byte, error) { return nil, nil }
func (d *fakeDriver) SaveImage(context.Context, string, io.Writer) error     { return nil }
func (d *fakeDriver) LoadImage(context.Context, string, io.Reader) error     { return d.loadErr }

type fakeBuilder struct {
	built        []string
	cleaned      []string
	buildErr     error
	cleanupErr   error
	cleanupErrAt int
	notes        []string
}

func (b *fakeBuilder) BuildPartitionProbeWorkload(_ context.Context, backend, image string, gpuIndex int, quota int64, id string) (runtime.WorkloadSpec, error) {
	b.built = append(b.built, backend)
	if b.buildErr != nil {
		return runtime.WorkloadSpec{}, b.buildErr
	}
	return runtime.WorkloadSpec{Name: "probe-" + backend, Image: image, GPUIndexes: []int{gpuIndex}, Env: map[string]string{"quota": "set"}, PreparationNotes: append([]string(nil), b.notes...)}, nil
}
func (b *fakeBuilder) CleanupPartitionProbe(id string) error {
	b.cleaned = append(b.cleaned, id)
	if b.cleanupErrAt == 0 || len(b.cleaned) == b.cleanupErrAt {
		return b.cleanupErr
	}
	return nil
}

func ascendFacts() ([]proto.AssetReport, []proto.GPUInfo) {
	return []proto.AssetReport{{Backend: accelerator.BackendAscendLogical, Root: "/assets", Files: []string{"libvruntime.so"}}},
		[]proto.GPUInfo{{Index: 4, Vendor: "ascend", Healthy: true}}
}

func TestRunnerOnlyProbesSupportedHygonGPU(t *testing.T) {
	driver, builder := &fakeDriver{logs: "backend=hygon quota_enforced=true"}, &fakeBuilder{}
	runner := New(driver, builder, "hygon")
	assets := []proto.AssetReport{{Backend: accelerator.BackendHygonLogical, Files: []string{"libamdhip64.so", "libhydmi.so", "libhydmi_mig.so", "librocm_smi64.so", "gpuworks-hygon-vdev-helper"}}}
	gpus := []proto.GPUInfo{
		{Index: 0, Vendor: "hygon", ArchID: "Z100"},
		{Index: 1, Vendor: "hygon", ArchID: "unknown"},
	}
	got := runner.Results(context.Background(), assets, gpus)
	if len(got) != 1 || got[0].GPUIndex == nil || *got[0].GPUIndex != 0 || got[0].Status != proto.PartitionProbePassed || len(driver.runs) != 1 {
		t.Fatalf("Hygon probe results = %+v, runs = %+v", got, driver.runs)
	}
}

func TestRunnerPassedUsesRealBuilderAndCaches(t *testing.T) {
	driver, builder := &fakeDriver{logs: "quota=4096MiB"}, &fakeBuilder{}
	runner := New(driver, builder, "ascend")
	runner.now = func() time.Time { return time.Unix(123, 0) }
	assets, gpus := ascendFacts()
	got := runner.Results(context.Background(), assets, gpus)
	again := runner.Results(context.Background(), assets, gpus)
	if len(got) != 1 || got[0].GPUIndex == nil || *got[0].GPUIndex != 4 || got[0].Status != proto.PartitionProbePassed || got[0].Detail != "quota=4096MiB" || !got[0].CheckedAt.Equal(time.Unix(123, 0)) {
		t.Fatalf("results = %+v", got)
	}
	if len(again) != 1 || len(builder.built) != 1 || len(driver.runs) != 1 || len(builder.cleaned) != 2 {
		t.Fatalf("cache/build/run/cleanup = %+v/%v/%d/%d", again, builder.built, len(driver.runs), len(builder.cleaned))
	}
	if driver.runs[0].GPUIndexes[0] != 4 {
		t.Fatalf("probe GPU indexes = %v", driver.runs[0].GPUIndexes)
	}
}

func TestRunnerCachedNeverRunsAndKeepsTheLastSnapshot(t *testing.T) {
	driver, builder := &fakeDriver{}, &fakeBuilder{}
	runner := New(driver, builder, "ascend")
	if got := runner.Cached(); got == nil || len(got) != 0 || len(driver.runs) != 0 {
		t.Fatalf("cached before any run = %#v, runs=%d; want empty non-nil and no run", got, len(driver.runs))
	}
	assets, gpus := ascendFacts()
	want := runner.Results(context.Background(), assets, gpus)
	got := runner.Cached()
	if len(got) != 1 || got[0].Status != want[0].Status || *got[0].GPUIndex != *want[0].GPUIndex || len(driver.runs) != 1 {
		t.Fatalf("cached = %+v (runs=%d), want %+v from one run", got, len(driver.runs), want)
	}
	got[0].Status = proto.PartitionProbeFailed
	if runner.Cached()[0].Status != proto.PartitionProbePassed {
		t.Fatal("Cached must return a copy")
	}
}

func TestRunnerReportsEachPhysicalGPUSeparately(t *testing.T) {
	driver, builder := &fakeDriver{}, &fakeBuilder{}
	runner := New(driver, builder, "ascend")
	assets, _ := ascendFacts()
	results := runner.Results(context.Background(), assets, []proto.GPUInfo{
		{Index: 0, Vendor: "ascend", Healthy: true},
		{Index: 3, Vendor: "ascend", Healthy: false},
		{Index: 6, Vendor: "ascend", Healthy: true},
	})
	if len(results) != 3 || results[0].GPUIndex == nil || *results[0].GPUIndex != 0 || results[1].GPUIndex == nil || *results[1].GPUIndex != 3 || results[2].GPUIndex == nil || *results[2].GPUIndex != 6 {
		t.Fatalf("GPU identities = %+v", results)
	}
	if results[0].Status != proto.PartitionProbePassed || results[1].Status != proto.PartitionProbePassed || results[2].Status != proto.PartitionProbePassed || len(driver.runs) != 3 {
		t.Fatalf("per-GPU results/runs = %+v/%+v", results, driver.runs)
	}
}

func TestRunnerReprobesWithReplacementRuntime(t *testing.T) {
	first, second := &fakeDriver{}, &fakeDriver{}
	runner := New(first, &fakeBuilder{}, "ascend")
	assets, gpus := ascendFacts()
	if got := runner.Results(context.Background(), assets, gpus); len(got) != 1 || got[0].Status != proto.PartitionProbePassed {
		t.Fatalf("first runtime result = %+v", got)
	}
	runner.SetDriver(second)
	if got := runner.Results(context.Background(), assets, gpus); len(got) != 1 || got[0].Status != proto.PartitionProbePassed || len(first.runs) != 1 || len(second.runs) != 1 {
		t.Fatalf("runtime switch reused stale probe: first=%d second=%d result=%+v", len(first.runs), len(second.runs), got)
	}
}

func TestRunnerFailurePreservesStderrTail(t *testing.T) {
	detail := strings.Repeat("prefix", 1000) + " dynamic linker GLIBC_2.33 not found"
	driver, builder := &fakeDriver{exitCode: 127, logs: detail}, &fakeBuilder{}
	runner := New(driver, builder, "ascend")
	assets, gpus := ascendFacts()
	got := runner.Results(context.Background(), assets, gpus)
	if len(got) != 1 || got[0].Status != proto.PartitionProbeFailed || !strings.Contains(got[0].Detail, "GLIBC_2.33 not found") || len(got[0].Detail) > maxProbeDetail {
		t.Fatalf("results = %+v", got)
	}
}

func TestRunnerPreservesPreparationNotesInProbeDetail(t *testing.T) {
	driver := &fakeDriver{logs: "quota=4096MiB"}
	builder := &fakeBuilder{notes: []string{"Ascend device-share 已自动开启：物理卡 4"}}
	runner := New(driver, builder, "ascend")
	assets, gpus := ascendFacts()
	got := runner.Results(context.Background(), assets, gpus)
	if len(got) != 1 || got[0].Status != proto.PartitionProbePassed || !strings.Contains(got[0].Detail, "device-share 已自动开启") || !strings.Contains(got[0].Detail, "quota=4096MiB") {
		t.Fatalf("results = %+v", got)
	}
}

func TestRunnerSeparatesColdImagePreparationFromProbeBudget(t *testing.T) {
	driver, builder := &fakeDriver{imageWait: 20 * time.Millisecond}, &fakeBuilder{}
	runner := New(driver, builder, "ascend")
	runner.prepareTimeout = time.Second
	runner.timeout = 5 * time.Millisecond
	assets, gpus := ascendFacts()
	got := runner.Results(context.Background(), assets, gpus)
	if len(got) != 1 || got[0].Status != proto.PartitionProbePassed {
		t.Fatalf("cold image preparation consumed behavioral probe budget: %+v", got)
	}
}

func TestRunnerCleansPartialStateWhenWorkloadBuildFails(t *testing.T) {
	driver, builder := &fakeDriver{}, &fakeBuilder{buildErr: errors.New("probe dependency missing")}
	runner := New(driver, builder, "ascend")
	assets, gpus := ascendFacts()
	got := runner.Results(context.Background(), assets, gpus)
	if len(got) != 1 || got[0].Status != proto.PartitionProbeFailed || !strings.Contains(got[0].Detail, "probe dependency missing") || len(builder.cleaned) != 2 {
		t.Fatalf("results/cleanup = %+v/%v", got, builder.cleaned)
	}
}

func TestRunnerDoesNotReleasePartitionStateBeforeProbeContainerIsRemoved(t *testing.T) {
	assets, gpus := ascendFacts()
	driver := &fakeDriver{removeErr: errors.New("container still exists"), removeErrAt: 3}
	builder := &fakeBuilder{}
	got := New(driver, builder, "ascend").Results(context.Background(), assets, gpus)
	if len(got) != 1 || got[0].Status != proto.PartitionProbeFailed || !strings.Contains(got[0].Detail, "container still exists") || len(builder.cleaned) != 1 {
		t.Fatalf("unsafe probe cleanup: result=%+v cleaned=%v", got, builder.cleaned)
	}
}

func TestRunnerReportsPartitionCleanupFailure(t *testing.T) {
	assets, gpus := ascendFacts()
	builder := &fakeBuilder{cleanupErr: errors.New("vdev file busy"), cleanupErrAt: 2}
	runner := New(&fakeDriver{}, builder, "ascend")
	got := runner.Results(context.Background(), assets, gpus)
	if len(got) != 1 || got[0].Status != proto.PartitionProbeFailed || !strings.Contains(got[0].Detail, "vdev file busy") {
		t.Fatalf("cleanup failure marked probe ready: %+v", got)
	}
	got = runner.Refresh(context.Background(), assets, gpus)
	if len(got) != 1 || got[0].Status != proto.PartitionProbePassed || len(builder.cleaned) != 4 {
		t.Fatalf("cleanup was not retried before the next probe: result=%+v cleanup=%v", got, builder.cleaned)
	}
	for _, id := range builder.cleaned {
		if id != "probe-ascend-dgpu-4" {
			t.Fatalf("unstable probe cleanup key %q", id)
		}
	}
}

func TestRunnerWithoutContainerRuntimeReportsSkipped(t *testing.T) {
	runner := New(nil, nil, "ppu")
	assets := []proto.AssetReport{{Backend: accelerator.BackendPPULogical, Root: "/assets", Files: []string{"hggc_quota.so", "hgml_dlsym_hook.so"}}}
	gpus := []proto.GPUInfo{{Index: 9, Vendor: "ppu", Healthy: true}}
	got := runner.Results(context.Background(), assets, gpus)
	if len(got) != 1 || got[0].Status != proto.PartitionProbeSkipped || !strings.Contains(got[0].Detail, "no container runtime") {
		t.Fatalf("results = %+v", got)
	}
}

func TestRunnerWithRuntimeThatCannotLoadProbeReportsFailed(t *testing.T) {
	driver, builder := &fakeDriver{}, &fakeBuilder{}
	// Embedding the base Driver interface intentionally hides the optional
	// ImageArchiver methods implemented by fakeDriver.
	nonArchiver := struct{ runtime.Driver }{Driver: driver}
	runner := New(nonArchiver, builder, "ppu")
	assets := []proto.AssetReport{{Backend: accelerator.BackendPPULogical, Root: "/assets", Files: []string{"hggc_quota.so", "hgml_dlsym_hook.so"}}}
	gpus := []proto.GPUInfo{{Index: 9, Vendor: "ppu", Healthy: true}}
	got := runner.Results(context.Background(), assets, gpus)
	if len(got) != 1 || got[0].Status != proto.PartitionProbeFailed || !strings.Contains(got[0].Detail, "cannot load") || len(builder.built) != 0 {
		t.Fatalf("results/builds = %+v/%v", got, builder.built)
	}
}

func TestRunnerPropagatesRuntimeFailuresAndCleansUp(t *testing.T) {
	assets, gpus := ascendFacts()
	cases := []struct {
		name   string
		driver *fakeDriver
		want   string
	}{
		{"run", &fakeDriver{runErr: errors.New("runtime unavailable")}, "runtime unavailable"},
		{"status", &fakeDriver{statusErr: errors.New("inspect failed")}, "inspect partition probe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			builder := &fakeBuilder{}
			runner := New(tc.driver, builder, "ascend")
			got := runner.Results(context.Background(), assets, gpus)
			if len(got) != 1 || got[0].Status != proto.PartitionProbeFailed || !strings.Contains(got[0].Detail, tc.want) {
				t.Fatalf("results=%+v want=%q", got, tc.want)
			}
			if len(builder.cleaned) != 2 {
				t.Fatalf("cleanup calls=%v", builder.cleaned)
			}
		})
	}
}

func TestRunnerFailsWhenEmbeddedProbeImageCannotLoad(t *testing.T) {
	assets, gpus := ascendFacts()
	driver := &fakeDriver{imageMissing: true, loadErr: errors.New("load failed")}
	runner := New(driver, &fakeBuilder{}, "ascend")
	got := runner.Results(context.Background(), assets, gpus)
	if len(got) != 1 || got[0].Status != proto.PartitionProbeFailed || !strings.Contains(got[0].Detail, "load partition probe image") {
		t.Fatalf("results=%+v", got)
	}
}

type namedDriver struct {
	fakeDriver
	name string
}

func (d *namedDriver) Name() string { return d.name }

// On Kubernetes the probe is a pod whose sandbox alone takes 10 to 20
// seconds to start, so its budget is longer than the docker one.
func TestProbeBudgetAllowsForAPodOnKubernetes(t *testing.T) {
	r := New(&namedDriver{name: "kube"}, nil, "ppu")
	if got := r.probeTimeout(); got != kubeProbeTimeout {
		t.Fatalf("kube budget = %s, want %s", got, kubeProbeTimeout)
	}
	r = New(&namedDriver{name: "docker"}, nil, "ppu")
	if got := r.probeTimeout(); got != 15*time.Second {
		t.Fatalf("docker budget = %s, want 15s", got)
	}
}
