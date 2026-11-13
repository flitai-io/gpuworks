package deploy

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Manual shell text is refused as unknown, while recognizable JSON argv
// identifies the unsupported sleep feature. Both stored forms stay gated.
func TestManualCommandSleepRequestIsGatedInBothStoredForms(t *testing.T) {
	engine, err := SelectEngine("vllm")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, display string
		gated         bool
	}{
		{name: "shell block", display: "vllm serve /model --enable-sleep-mode", gated: true},
		{name: "json argv array", display: `["vllm","serve","/model","--enable-sleep-mode"]`, gated: true},
		{name: "json argv array, equals form", display: `["vllm","serve","/model","--enable-sleep-mode=true"]`, gated: true},
		// An explicit false is not a request, and neither is the flag as data.
		{name: "explicitly disabled", display: `["vllm","serve","/model","--enable-sleep-mode=false"]`},
		{name: "flag inside a model name", display: `["vllm","serve","/models/--enable-sleep-mode"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := ResolveLaunchInput{Engine: engine, Params: Params{
				Vendor: "kunlunxin", ModelPath: "/model", AssignedGPUs: 1, TensorParallel: 1,
			}}
			in.ContainerOverrides.CommandMode = string(CommandManual)
			in.ContainerOverrides.CmdDisplay = tc.display
			_, err := ResolveLaunch(in)
			var product *messages.ProductError
			wantCode := messages.ErrorAcceleratorFeatureUnsupported
			if tc.name == "shell block" {
				wantCode = messages.ErrorLaunchFeaturesUnknown
			}
			gated := errors.As(err, &product) && product.Code() == wantCode
			if gated != tc.gated {
				t.Fatalf("gated = %v, want %v (err = %v)", gated, tc.gated, err)
			}
			// A vendor that provides sleep keeps the same command.
			in.Params.Vendor = "nvidia"
			if _, err := ResolveLaunch(in); err != nil {
				t.Fatalf("the gate reached a vendor that provides sleep: %v", err)
			}
		})
	}
}
