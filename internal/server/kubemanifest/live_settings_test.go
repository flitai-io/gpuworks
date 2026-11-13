package kubemanifest

import (
	"strings"
	"testing"
)

// TestRenderLeavesLiveSettingsOut: the idle-return grace period, the NVIDIA
// RuntimeClass and the engine port range are handed to running agents by the
// server, so the manifest carries none of them and an edit of one leaves it
// as it was (no apply, no rolling restart).
func TestRenderLeavesLiveSettingsOut(t *testing.T) {
	out, err := Render(sample())
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"idle-return", "--engine-ports", "GPUWORKS_KUBE_RUNTIME_CLASS", "GPUWORKS_ENGINE_PORTS"} {
		if strings.Contains(out, banned) {
			t.Errorf("manifest mentions %q", banned)
		}
	}
	for _, d := range docs(t, out) {
		if d["kind"] != "DaemonSet" {
			continue
		}
		meta := dig(t, d, "spec", "template", "metadata").(map[string]any)
		if _, ok := meta["annotations"]; ok {
			t.Errorf("pod template annotations = %v, want none", meta["annotations"])
		}
	}
}

func TestValidRuntimeClass(t *testing.T) {
	for class, want := range map[string]bool{"": true, "nvidia": true, "nvidia-legacy": true, "No": false, "a_b": false, strings.Repeat("a", 64): false} {
		if got := ValidRuntimeClass(class); got != want {
			t.Errorf("ValidRuntimeClass(%q) = %v, want %v", class, got, want)
		}
	}
}
