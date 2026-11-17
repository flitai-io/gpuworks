package proto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The wire shape of a runtime failure is a contract between agent and server
// versions and the persisted row: exact keys, omitted zero values.
func TestRuntimeFailureWireJSON(t *testing.T) {
	exit, device := 137, 1
	f := RuntimeFailure{
		Code: ErrorContainerExited, Phase: PhaseHealthCheck, ExitCode: &exit, OOMKilled: true,
		Causes: []FailureCause{
			{Code: "CUDA_OOM", Evidence: []string{"torch.OutOfMemoryError: CUDA out of memory"}},
			{Code: "DGPU_MEMORY_LIMIT_EXCEEDED", Device: &device, NeededBytes: 9, LimitBytes: 8},
		},
		Evidence: []string{"torch.OutOfMemoryError: CUDA out of memory"},
	}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"code":"CONTAINER_EXITED","phase":"health_check","exit_code":137,"oom_killed":true,` +
		`"causes":[{"code":"CUDA_OOM","evidence":["torch.OutOfMemoryError: CUDA out of memory"]},` +
		`{"code":"DGPU_MEMORY_LIMIT_EXCEEDED","device":1,"needed_bytes":9,"limit_bytes":8}],` +
		`"evidence":["torch.OutOfMemoryError: CUDA out of memory"]}`
	if string(raw) != want {
		t.Fatalf("wire = %s\nwant  %s", raw, want)
	}
	var back RuntimeFailure
	if err := json.Unmarshal(raw, &back); err != nil || !reflect.DeepEqual(back, f) {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
	// A zero exit code is a fact, not an absence.
	zero := 0
	raw, _ = json.Marshal(RuntimeFailure{Code: ErrorContainerExited, ExitCode: &zero})
	if string(raw) != `{"code":"CONTAINER_EXITED","exit_code":0}` {
		t.Fatalf("zero exit = %s", raw)
	}
	// The accelerator shape an older agent sends still reads.
	var old RuntimeFailure
	if err := json.Unmarshal([]byte(`{"code":"ACCELERATOR_RUNTIME_UNSUPPORTED","vendor":"kunlunxin","os":"linux","arch":"arm64","runtime":"docker","required_os":"linux","required_arch":"amd64","required_runtime":"docker"}`), &old); err != nil ||
		old.Code != ErrorAcceleratorRuntimeUnsupported || old.Vendor != "kunlunxin" || old.RequiredArch != "amd64" {
		t.Fatalf("old shape = %+v, %v", old, err)
	}
}

// Bounded keeps the newest evidence lines, at most MaxFailureCauses causes,
// and lines of at most MaxFailureEvidenceChars bytes cut on a rune boundary.
func TestRuntimeFailureBounded(t *testing.T) {
	long := strings.Repeat("显", 200)
	f := RuntimeFailure{
		Code:     ErrorContainerExited,
		Detail:   long,
		Evidence: []string{"1", "2", "3", "4", long},
	}
	for i := 0; i < MaxFailureCauses+2; i++ {
		f.Causes = append(f.Causes, FailureCause{Code: "C", Evidence: []string{"a", "b", "c", "d"}})
	}
	b := f.Bounded()
	if len(b.Causes) != MaxFailureCauses || !reflect.DeepEqual(b.Causes[0].Evidence, []string{"b", "c", "d"}) {
		t.Fatalf("causes = %+v", b.Causes)
	}
	if len(b.Evidence) != MaxFailureEvidence || b.Evidence[0] != "3" {
		t.Fatalf("evidence = %q", b.Evidence)
	}
	for _, s := range []string{b.Detail, b.Evidence[2]} {
		if len(s) > MaxFailureEvidenceChars || !strings.HasSuffix(s, "…") || !strings.HasPrefix(s, "显") {
			t.Fatalf("clipped = %q (%d bytes)", s, len(s))
		}
		if strings.ContainsRune(s, '�') {
			t.Fatalf("clip split a rune: %q", s)
		}
	}
	if len(f.Causes[0].Evidence) != 4 {
		t.Fatal("Bounded changed its receiver's causes")
	}
}

// An older agent's lower-case codes are read in the one spelling.
func TestRuntimeFailureNormalized(t *testing.T) {
	f := RuntimeFailure{Code: "container_exited", Causes: []FailureCause{{Code: "cuda_oom"}}}.Normalized()
	if f.Code != ErrorContainerExited || f.Causes[0].Code != "CUDA_OOM" {
		t.Fatalf("normalized = %+v", f)
	}
}
