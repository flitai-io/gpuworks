package kubemanifest

import (
	"os"
	"strings"
	"testing"
)

// The release pipeline tags the image with build/agent-image/REVISION; the
// manifest has to name a tag that exists.
func TestAgentImageRevisionIsTheImageBuildsRevision(t *testing.T) {
	raw, err := os.ReadFile("../../../build/agent-image/REVISION")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(raw)); got != AgentImageRevision {
		t.Fatalf("build/agent-image/REVISION = %q, AgentImageRevision = %q", got, AgentImageRevision)
	}
}

func TestAgentImageNamesEachRegistryAtTheRevision(t *testing.T) {
	for source, want := range map[string]string{
		"":          "gpuworks/gpuworks-agent:" + AgentImageRevision,
		"dockerhub": "gpuworks/gpuworks-agent:" + AgentImageRevision,
		"swr":       "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-agent:" + AgentImageRevision,
		"quay":      "quay.io/gpuworks/gpuworks-agent:" + AgentImageRevision,
	} {
		if got := AgentImage(source); got != want {
			t.Errorf("AgentImage(%q) = %q, want %q", source, got, want)
		}
		if !ValidAgentImageSource(source) {
			t.Errorf("ValidAgentImageSource(%q) = false", source)
		}
	}
	if ValidAgentImageSource("ghcr") {
		t.Error("ValidAgentImageSource accepts an unknown registry")
	}
	if len(AgentImageSources) != 3 || AgentImageSources[0] != AgentImageDockerHub {
		t.Errorf("AgentImageSources = %v, want Docker Hub first of three", AgentImageSources)
	}
}
