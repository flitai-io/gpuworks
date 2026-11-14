//go:build cloudgpu && (compshare || aliyun)

package l4

import (
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

const manualCommandMarker = "gpuworks-manual-command"

// hostedCreateWithManualDraft deploys body again as a manual command: it asks
// the evaluation for the draft a multi-rank deployment's command editor starts
// from, adds one line of its own (echoLine, which may reference platform
// variables) and creates the deployment with that script. The line is what
// proves each rank ran the shared script with its own values: the engine
// command alone would look the same as an engine-mode launch.
func hostedCreateWithManualDraft(t *testing.T, harness *hostedControlPlane, body map[string]any, servedName, echoLine string, expected int, timeout time.Duration) *hostedDeployment {
	t.Helper()
	var evaluated struct {
		ManualCommandDraft string `json:"manual_command_draft"`
	}
	if err := harness.API.PostJSON(t.Context(), "/api/v1/evaluate", body, &evaluated); err != nil {
		t.Fatalf("evaluate for the manual command draft: %v", err)
	}
	rest, ok := strings.CutPrefix(evaluated.ManualCommandDraft, "set -eu\n")
	if !ok {
		t.Fatalf("manual command draft = %q, want a script starting with set -eu", evaluated.ManualCommandDraft)
	}
	script := fmt.Sprintf("set -eu\necho \"%s %s\"\n%s", manualCommandMarker, echoLine, rest)
	manual := maps.Clone(body)
	overrides := map[string]any{}
	if stated, ok := body["container_overrides"].(map[string]any); ok {
		overrides = maps.Clone(stated)
	}
	overrides["command_mode"], overrides["cmd_display"] = "manual", script
	manual["container_overrides"], manual["served_name"] = overrides, servedName
	return createHostedDeployment(t, harness, manual, expected, timeout)
}

// assertManualCommandRan checks one instance of a manual multi-rank
// deployment: the script the platform resolved for it carries this rank's
// values, and the container really printed the line, read from its own log on
// its own VM.
func assertManualCommandRan(t *testing.T, vm *cloudTestEnv, instance dto.Instance, wantLine string, wantInScript ...string) {
	t.Helper()
	script := strings.Join(instance.Argv, "\n")
	want := manualCommandMarker + " " + wantLine
	for _, text := range append([]string{want}, wantInScript...) {
		if !strings.Contains(script, text) {
			t.Fatalf("instance %s resolved script lacks %q:\n%s", instance.UUID, text, script)
		}
	}
	if strings.Contains(script, "${GPUWORKS_") {
		t.Fatalf("instance %s resolved script still holds a platform variable:\n%s", instance.UUID, script)
	}
	command := fmt.Sprintf(`set -eu
docker_cmd=docker; docker info >/dev/null 2>&1 || docker_cmd='sudo -n docker'
$docker_cmd logs %s 2>&1 | grep -F -m1 %s`, shellQuoteForTest(instance.ContainerID), shellQuoteForTest(want))
	out, err := runCloudCommand(t.Context(), vm.Instance, command)
	if err != nil || !strings.Contains(out, want) {
		t.Fatalf("instance %s container log lacks %q: %v\n%s", instance.UUID, want, err, out)
	}
}
