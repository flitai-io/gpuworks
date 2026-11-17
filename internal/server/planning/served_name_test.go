package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
)

func TestTranslatesServedNameOnlyOnHostAgentsWithoutTheCapability(t *testing.T) {
	for _, tc := range []struct {
		driver string
		caps   []string
		want   bool
	}{
		{proto.RuntimeDriverHost, nil, true},
		{proto.RuntimeDriverHost, []string{proto.CapImagePullCandidates}, true},
		{proto.RuntimeDriverHost, []string{proto.CapLaunchNameVerbatim}, false},
		{"docker", nil, false},
		{"", nil, false},
	} {
		if got := TranslatesServedName(tc.driver, tc.caps); got != tc.want {
			t.Errorf("TranslatesServedName(%q, %v) = %v, want %v", tc.driver, tc.caps, got, tc.want)
		}
	}
}

func TestExpectedServedNameDropsANameTheAgentWouldTranslate(t *testing.T) {
	launch := func(name string) deploy.ResolvedLaunch {
		return deploy.ResolvedLaunch{
			Entrypoint: []string{"vllm", "serve", "/model"},
			Argv:       []string{"--served-model-name", name},
			ModelMount: "/model",
			Mounts:     []proto.MountSpec{{Source: "/m/draft", Target: "/draft/"}},
		}
	}
	for _, tc := range []struct {
		name     string
		implicit bool
		want     string
	}{
		{"/model", false, "/model"},
		{"/model", true, ""},
		{"/model/sub", true, ""},
		{"/draft", true, ""},
		{"/modelx", true, "/modelx"},
		{"qwen", true, "qwen"},
	} {
		if got := ExpectedServedName(launch(tc.name), tc.implicit); got != tc.want {
			t.Errorf("ExpectedServedName(%q, implicit=%v) = %q, want %q", tc.name, tc.implicit, got, tc.want)
		}
	}
	if got := ExpectedServedName(deploy.ResolvedLaunch{Entrypoint: []string{"vllm", "serve", "/model"}, ModelMount: "/model"}, true); got != "" {
		t.Errorf("a launch with no name expects %q", got)
	}
}
