package gpu

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStructuredAndCombinedVendorOutputRemainSeparate(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "smi")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nprintf stdout\nprintf '<warning&>' >&2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, query := range []func() (string, error){
		func() (string, error) { return QueryVendorCLI(t.Context(), exe) },
		func() (string, error) { return RunVendorCLI(t.Context(), nil, exe) },
	} {
		got, err := query()
		if err != nil || !strings.Contains(got, "stdout") || !strings.Contains(got, "<warning&>") {
			t.Fatalf("legacy output changed: %q %v", got, err)
		}
	}
	for _, query := range []func() (string, error){
		func() (string, error) { return QueryVendorCLIStdout(t.Context(), exe) },
		func() (string, error) { return RunVendorCLIStdout(t.Context(), nil, exe) },
	} {
		got, err := query()
		if err != nil || got != "stdout" {
			t.Fatalf("structured output contaminated: %q %v", got, err)
		}
	}
}

func TestStructuredStderrBufferRetainsOnlyBoundedTail(t *testing.T) {
	var b limitedDiagnostic
	for _, s := range []string{strings.Repeat("a", 900), "b", strings.Repeat("c", 399)} {
		n, err := b.Write([]byte(s))
		if n != len(s) || err != nil {
			t.Fatal(n, err)
		}
	}
	if got := b.String(); got != "b"+strings.Repeat("c", 399) {
		t.Fatalf("wrong stderr tail length=%d", len(got))
	}
}
