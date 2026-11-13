package deploy

import (
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestKunlunSleepCapabilityAndMergedLaunch(t *testing.T) {
	for _, vendor := range []string{"kunlunxin", "nvidia", ""} {
		profile, err := ProfileForTarget("vllm", vendor)
		if err != nil || profile.Sleep.Supported != (vendor != "kunlunxin") {
			t.Fatalf("profile(%s) = %+v, %v", vendor, profile.Sleep, err)
		}
	}
	for _, vendor := range []string{"kunlunxin", "nvidia"} {
		profile, err := ProfileForTarget("sglang", vendor)
		if err != nil || (len(profile.PDModes) == 0) != (vendor == "kunlunxin") {
			t.Fatalf("PD profile(%s) = %+v, %v", vendor, profile.PDModes, err)
		}
	}
	engine, _ := SelectEngine("vllm")
	for _, tc := range []struct {
		name   string
		change func(*ResolveLaunchInput)
	}{
		{"declaration", func(in *ResolveLaunchInput) { in.Params.Sleepable = true }},
		{"user argv", func(in *ResolveLaunchInput) { in.Params.ExtraArgs = []string{"--enable-sleep-mode"} }},
		{"template argv", func(in *ResolveLaunchInput) { in.Params.TemplateArgs = []string{"--enable-sleep-mode=true"} }},
		{"adjacent sleep flags", func(in *ResolveLaunchInput) {
			in.Params.ExtraArgs = []string{"--enable-sleep-mode=false", "--enable-sleep-mode"}
		}},
		{"adjacent manual sleep flags", func(in *ResolveLaunchInput) {
			in.ContainerOverrides = ContainerOverrides{CommandMode: "manual", CmdDisplay: "python -m vllm.entrypoints.openai.api_server --enable-sleep-mode=0 --enable-sleep-mode=true --port 30000"}
		}},
		{"manual command", func(in *ResolveLaunchInput) {
			in.ContainerOverrides = ContainerOverrides{CommandMode: "manual", CmdDisplay: "python -m vllm.entrypoints.openai.api_server --enable-sleep-mode --port 30000"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := ResolveLaunchInput{Engine: engine, Params: Params{Vendor: "kunlunxin", AssignedGPUs: 1, TensorParallel: 1, ModelPath: "/model"}}
			if _, err := ResolveLaunch(in); err != nil {
				t.Fatal(err)
			}
			tc.change(&in)
			_, err := ResolveLaunch(in)
			var productErr *messages.ProductError
			wantCode := messages.ErrorAcceleratorFeatureUnsupported
			if strings.Contains(tc.name, "manual") {
				wantCode = messages.ErrorLaunchFeaturesUnknown
			}
			if !errors.As(err, &productErr) || productErr.Code() != wantCode {
				t.Fatalf("unsupported sleep must return a stable product error: %v", err)
			}
		})
	}
}

func TestKunlunTensorParallelLaunch(t *testing.T) {
	engine, _ := SelectEngine("sglang")
	for _, tc := range []struct {
		name                   string
		cards, tp, nodes, rank int
		want                   string
	}{
		{"single node two cards", 2, 2, 1, 0, "--tp-size 2"},
		{"two nodes leader", 2, 4, 2, 0, "--nnodes 2 --node-rank 0 --dist-init-addr 192.0.2.10:29500"},
		{"two nodes follower", 2, 4, 2, 1, "--nnodes 2 --node-rank 1 --dist-init-addr 192.0.2.10:29500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Params{Vendor: "kunlunxin", AssignedGPUs: tc.cards, TensorParallel: tc.tp, NNodes: tc.nodes, NodeRank: tc.rank, Headless: tc.rank > 0, MasterAddr: "192.0.2.10", MasterPort: 29500, ModelPath: "/model"}
			launch, err := ResolveLaunch(ResolveLaunchInput{Engine: engine, Params: p})
			if err != nil {
				t.Fatal(err)
			}
			command := strings.Join(append(launch.Entrypoint, launch.Argv...), " ")
			if !strings.Contains(command, tc.want) {
				t.Fatalf("launch=%s, want %s", command, tc.want)
			}
		})
	}
}

func TestKunlunRejectsPrefillDecodeDisaggregation(t *testing.T) {
	engine, _ := SelectEngine("sglang")
	for _, change := range []func(*Params){
		func(p *Params) { p.PD = &PDLaunch{} },
	} {
		p := Params{Vendor: "kunlunxin", AssignedGPUs: 2, TensorParallel: 2, ModelPath: "/model"}
		change(&p)
		if _, err := ResolveLaunch(ResolveLaunchInput{Engine: engine, Params: p}); err == nil {
			t.Fatal("unsupported parallel mode accepted")
		}
	}
}

func TestKunlunDevModeAndDisabledSleepAreNotSleepRequests(t *testing.T) {
	engine, _ := SelectEngine("vllm")
	for _, args := range [][]string{nil, {"--enable-sleep-mode=false"}, {"--enable-sleep-mode=0"}, {"--enable-sleep-mode=false", "--enable-sleep-mode=0"}, {"--enable-sleep-mode-custom"}} {
		in := ResolveLaunchInput{Engine: engine, Params: Params{Vendor: "kunlunxin", AssignedGPUs: 1, TensorParallel: 1, ModelPath: "/model", ExtraArgs: args, TemplateEnv: map[string]string{"VLLM_SERVER_DEV_MODE": "1"}}}
		if _, err := ResolveLaunch(in); err != nil {
			t.Fatal(err)
		}
	}
}
