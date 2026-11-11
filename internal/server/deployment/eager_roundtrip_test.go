package deployment

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// A feature card submits the evaluated display values again after another
// edit. An explicit false must survive that projection, or a template's true
// returns even though the first evaluation correctly removed its flag.
func TestEagerOffSurvivesEvaluateFeedback(t *testing.T) {
	selection := templateSelection{ExtraArgs: []string{"--enforce-eager"}}
	quick := map[string]any{"enforce_eager": false}
	for round := range 2 {
		req := &dto.DeployRequest{QuickParams: quick}
		launch, err := materializeDeployRequest("vllm", req, selection, nil, "0.25.1")
		if err != nil {
			t.Fatal(err)
		}
		if launch.Quick.EnforceEager == nil || *launch.Quick.EnforceEager {
			t.Fatalf("round %d restored eager mode", round)
		}
		quick = launchConfigMap(launch.Quick)
		value, present := quick["enforce_eager"]
		if !present || value != false {
			t.Fatalf("round %d lost explicit Off: present=%v value=%v", round, present, value)
		}
	}
}

func TestUnstatedEagerDoesNotBecomeAnExplicitOff(t *testing.T) {
	for _, engine := range []string{"vllm", "sglang"} {
		t.Run(engine, func(t *testing.T) {
			launch, err := materializeDeployRequest(engine, &dto.DeployRequest{}, templateSelection{}, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, present := launchConfigMap(launch.Quick)["enforce_eager"]; present {
				t.Fatal("implicit eager default became an explicit choice in feedback")
			}
		})
	}
}
