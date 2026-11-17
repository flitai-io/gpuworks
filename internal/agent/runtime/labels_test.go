package runtime

import (
	"slices"
	"testing"
)

func TestLabelArgsAreSortedAndSkipEmpty(t *testing.T) {
	got := LabelArgs(map[string]string{
		"gpuworks.server": "https://gw.example:8800",
		"gpuworks.node":   "node-a",
		"gpuworks.empty":  "",
		"":                "no-key",
	})
	want := []string{"--label", "gpuworks.node=node-a", "--label", "gpuworks.server=https://gw.example:8800"}
	if !slices.Equal(got, want) {
		t.Fatalf("LabelArgs = %q, want %q", got, want)
	}
	if got := LabelArgs(nil); len(got) != 0 {
		t.Fatalf("LabelArgs(nil) = %q, want none", got)
	}
}
