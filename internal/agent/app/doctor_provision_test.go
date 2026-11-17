package app

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// provisioningDriver is a udocker-like driver whose setup state is a fixture;
// reading it counts, because the doctor must not start the setup.
type provisioningDriver struct {
	imagePullTestDriver
	state runtime.ProvisionState
	reads int
}

func (*provisioningDriver) Name() string { return "udocker" }

func (d *provisioningDriver) ProvisionState() runtime.ProvisionState {
	d.reads++
	return d.state
}

func doctorWith(t *testing.T, driver runtime.Driver) proto.DoctorReport {
	t.Helper()
	withNoTools(t)
	return buildDoctorReport(context.Background(), doctorFixedNonNVIDIAProber(), nil, driver)
}

// The field failure: ownership of the runtime directory could not be handed
// over, and every deployment failed with it. The node page now says so.
func TestDoctorReportsAFailedRuntimeSetupWithItsCauseAndRecheck(t *testing.T) {
	failedAt := time.Date(2026, 10, 10, 8, 30, 5, 0, time.UTC)
	driver := &provisioningDriver{state: runtime.ProvisionState{
		LastError:   "/workspace/gpuworks/udocker does not allow ':' in file names, which container images need (for example /var/lib/dpkg/info/<package>:amd64.list); reinstall the agent with an install directory on a local filesystem",
		LastErrorAt: failedAt,
		RetryAt:     failedAt.Add(30 * time.Second),
		Probe:       "set up the runveil runtime: run user, /workspace/gpuworks/udocker and /workspace/gpuworks/udocker-state, patched PRoot",
		Recheck:     "findmnt -T /workspace/gpuworks/udocker; ls -ld /workspace/gpuworks/udocker /workspace/gpuworks/udocker-state",
	}}
	r := doctorWith(t, driver)
	found := findingsByCode(r, proto.DoctorRuntimeProvisionFailed)
	if len(found) != 1 {
		t.Fatalf("findings = %+v", r.Findings)
	}
	f := found[0]
	wantEvidence := []proto.DoctorField{
		{Name: "error", Value: driver.state.LastError},
		{Name: "failed_at", Value: "2026-10-10T08:30:05Z"},
		{Name: "retry_at", Value: "2026-10-10T08:30:35Z"},
	}
	if f.Status != proto.CheckFail || f.Probe != driver.state.Probe || f.Recheck != driver.state.Recheck || !reflect.DeepEqual(f.Evidence, wantEvidence) {
		t.Fatalf("finding = %+v", f)
	}
	if got := factsByCode(r, proto.DoctorFactRuntimeIdentity); len(got) != 0 {
		t.Fatalf("identity stated for a runtime that is not set up: %q", got)
	}
	if driver.reads != 1 {
		t.Fatalf("state read %d times", driver.reads)
	}
	for _, want := range []string{"[fail] RUNTIME_PROVISION_FAILED", "does not allow ':' in file names", "recheck   findmnt -T /workspace/gpuworks/udocker", "the selected runtime could not be set up"} {
		if !strings.Contains(r.Report, want) {
			t.Errorf("report text lacks %q:\n%s", want, r.Report)
		}
	}
}

func TestDoctorStatesWhoWorkloadsRunAs(t *testing.T) {
	for name, tc := range map[string]struct {
		state runtime.ProvisionState
		want  string
	}{
		"dedicated user": {
			state: runtime.ProvisionState{Provisioned: true, RunUser: "udocker", RunUID: 1001, RunGID: 1001},
			want:  "dedicated user udocker (uid 1001)",
		},
		"fallback to the agent": {
			state: runtime.ProvisionState{Provisioned: true, RunUID: 0, FallbackReason: "/workspace/gpuworks/udocker refuses to change file ownership: lchown: operation not permitted"},
			want:  "the agent's own user (uid 0): the dedicated runtime user is not used because /workspace/gpuworks/udocker refuses to change file ownership: lchown: operation not permitted",
		},
		"non-root agent": {
			state: runtime.ProvisionState{Provisioned: true, RunUID: 1000},
			want:  "the agent's own user (uid 1000)",
		},
		"not set up yet": {
			state: runtime.ProvisionState{},
			want:  "udocker is set up on first use",
		},
		"not set up yet, named as operators know it": {
			state: runtime.ProvisionState{Runtime: "runveil"},
			want:  "runveil is set up on first use",
		},
	} {
		t.Run(name, func(t *testing.T) {
			driver := &provisioningDriver{state: tc.state}
			r := doctorWith(t, driver)
			if got := factsByCode(r, proto.DoctorFactRuntimeIdentity); !reflect.DeepEqual(got, []string{tc.want}) {
				t.Fatalf("identity facts = %q, want %q", got, tc.want)
			}
			if got := findingsByCode(r, proto.DoctorRuntimeProvisionFailed); len(got) != 0 {
				t.Fatalf("findings = %+v", got)
			}
		})
	}
}

// The identity is also stated typed, so the console words it in the reader's
// language instead of showing the agent's English sentence.
func TestDoctorStatesTheRunIdentityTyped(t *testing.T) {
	for name, tc := range map[string]struct {
		state runtime.ProvisionState
		want  proto.DoctorRunIdentity
	}{
		"dedicated user": {
			state: runtime.ProvisionState{Runtime: "runveil", Provisioned: true, RunUser: "udocker", RunUID: 1001},
			want:  proto.DoctorRunIdentity{Runtime: "runveil", User: "udocker", UID: 1001},
		},
		"fallback": {
			state: runtime.ProvisionState{Runtime: "runveil", Provisioned: true, FallbackReason: "/workspace/udocker refuses to change file ownership: lchown: operation not permitted"},
			want:  proto.DoctorRunIdentity{Runtime: "runveil", Fallback: "/workspace/udocker refuses to change file ownership: lchown: operation not permitted"},
		},
		"not set up yet": {
			state: runtime.ProvisionState{Runtime: "runveil"},
			want:  proto.DoctorRunIdentity{Runtime: "runveil", Pending: true},
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := doctorWith(t, &provisioningDriver{state: tc.state})
			var got []proto.DoctorRunIdentity
			for _, f := range r.Facts {
				if f.Code == proto.DoctorFactRuntimeIdentity && f.Identity != nil {
					got = append(got, *f.Identity)
				}
			}
			if !reflect.DeepEqual(got, []proto.DoctorRunIdentity{tc.want}) {
				t.Fatalf("identity = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A runtime that does not report its setup (docker) adds neither.
func TestDoctorSaysNothingAboutTheSetupOfARuntimeThatHasNone(t *testing.T) {
	r := doctorWith(t, &doctorDockerDriver{})
	if len(factsByCode(r, proto.DoctorFactRuntimeIdentity)) != 0 || len(findingsByCode(r, proto.DoctorRuntimeProvisionFailed)) != 0 {
		t.Fatalf("report = %+v", r)
	}
	r = doctorWith(t, nil)
	if len(factsByCode(r, proto.DoctorFactRuntimeIdentity)) != 0 {
		t.Fatalf("report without a driver = %+v", r.Facts)
	}
}
