package gpu

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// Captured from mx-smi 3.100.0 on an idle MetaX node; timestamp redacted.
// The exact capture date is unavailable; it is not the 2026-11-20 occupied sample.
func TestMetaXCapturedEmptyProcessTable(t *testing.T) {
	raw, err := os.ReadFile("testdata/mx-smi-process-empty.txt")
	if err != nil {
		t.Fatal(err)
	}
	processes, err := parseMetaXProcesses(string(raw))
	if err != nil || len(processes) != 0 {
		t.Fatalf("empty process table: %+v, %v", processes, err)
	}
	mixed := strings.Replace(string(raw), "|  no process found", "|  0 123 example 8 |\n|  no process found", 1)
	if _, err := parseMetaXProcesses(mixed); err == nil {
		t.Fatal("mixed empty and occupied table accepted")
	}
}

func TestMetaXProcessTableRejectsPrematureClose(t *testing.T) {
	// Derived from the existing empty and occupied fixtures; these altered
	// outputs model truncation or corruption, not captured device output.
	empty := metaxFixture(t, "mx-smi-process-empty.txt")
	occupied := metaxFixture(t, "mx-smi-process.txt")
	border := "+---------------------------------------------------------------------------------+"
	for name, out := range map[string]string{
		"empty then occupied":     strings.Replace(empty, border+"\n\nEnd of Log", border+"\n|  3 879033 sgl_diffusion:: 31276 |\n"+border+"\n\nEnd of Log", 1),
		"occupied then empty":     strings.Replace(occupied, border+"\n\nEnd of Log", border+"\n|  no process found |\n"+border+"\n\nEnd of Log", 1),
		"occupied then malformed": strings.Replace(occupied, border+"\n\nEnd of Log", border+"\n| broken |\n"+border+"\n\nEnd of Log", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseMetaXProcesses(out); err == nil {
				t.Fatal("accepted rows after process table closed")
			}
			p := &MetaXProber{run: func(_ context.Context, args ...string) (string, error) {
				if args[0] == "-L" {
					return metaxFixture(t, "mx-smi-list.txt"), nil
				}
				return out, nil
			}}
			if procs, err := p.ComputeProcesses(t.Context()); err == nil || len(procs) != 0 {
				t.Fatalf("premature close reported occupancy: %+v, %v", procs, err)
			}
		})
	}
}

func TestMetaXProcessTableRejectsIncompleteAndInvalidRows(t *testing.T) {
	base := "| Process: |\n| GPU PID Process Name GPU Memory |\n%s\n+---+\nEnd of Log\n"
	cases := []struct{ name, row string }{{"missing body", ""}, {"negative index", "| -1 123 worker 8 |"}, {"zero pid", "| 0 0 worker 8 |"}, {"negative pid", "| 0 -2 worker 8 |"}, {"negative memory", "| 0 123 worker -8 |"}, {"overflow memory", "| 0 123 worker 8796093022208 |"}, {"truncated row", "| 0 123 worker"}, {"missing empty marker", "|"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseMetaXProcesses(fmt.Sprintf(base, tc.row)); err == nil {
				t.Fatal("invalid process table accepted")
			}
		})
	}
}
