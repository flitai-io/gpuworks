package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The launch-configuration conflict code is written out in api/errors.go
// because that file is the inventory clients and the locale check read, and
// produced by deploy.Materialize. Nothing but this test keeps the two spellings
// from drifting apart.
func TestLaunchConfigConflictCodeMatchesItsProducer(t *testing.T) {
	if ErrorLaunchConfigConflict != deploy.ErrorLaunchConfigConflict {
		t.Fatalf("api code %q, deploy code %q", ErrorLaunchConfigConflict, deploy.ErrorLaunchConfigConflict)
	}
	if ErrorLaunchConfigUnsetConflict != deploy.ErrorLaunchConfigUnsetConflict {
		t.Fatalf("api code %q, deploy code %q", ErrorLaunchConfigUnsetConflict, deploy.ErrorLaunchConfigUnsetConflict)
	}
	if ErrorQuickParamJSON != deploy.ErrorQuickParamJSON {
		t.Fatalf("api code %q, deploy code %q", ErrorQuickParamJSON, deploy.ErrorQuickParamJSON)
	}
	if ErrorQuickParamPath != deploy.ErrorQuickParamPath {
		t.Fatalf("api code %q, deploy code %q", ErrorQuickParamPath, deploy.ErrorQuickParamPath)
	}
}

func TestDockerArgCodesMatchTheirProducer(t *testing.T) {
	for api, producer := range map[string]string{
		ErrorDockerArgPlatformManaged:    deploy.ErrorDockerArgPlatformManaged,
		ErrorDockerArgUseShmField:        deploy.ErrorDockerArgUseShmField,
		ErrorDockerArgUseEnvField:        deploy.ErrorDockerArgUseEnvField,
		ErrorDockerArgUseMountField:      deploy.ErrorDockerArgUseMountField,
		ErrorDockerArgUseEntrypointField: deploy.ErrorDockerArgUseEntrypointField,
		ErrorShmSizeWithHostIPC:          deploy.ErrorShmSizeWithHostIPC,
		ErrorExtraArgsJSONInvalid:        deploy.ErrorExtraArgsJSONInvalid,
	} {
		if api != producer {
			t.Fatalf("api code %q, deploy code %q", api, producer)
		}
	}
}

func TestAscendSleepCodesMatchTheirProducer(t *testing.T) {
	for api, producer := range map[string]string{
		ErrorAscendSleepAdditionalConfigInvalid: deploy.ErrorAscendSleepAdditionalConfigInvalid,
		ErrorAscendSleepWeightNZConflict:        deploy.ErrorAscendSleepWeightNZConflict,
		ErrorAscendSleepExpandableSegments:      deploy.ErrorAscendSleepExpandableSegments,
	} {
		if api != producer {
			t.Fatalf("api code %q, deploy code %q", api, producer)
		}
	}
}

func TestDataParallelCodesMatchTheirProducer(t *testing.T) {
	for api, producer := range map[string]string{
		ErrorDataParallelGPUCount:         deploy.ErrorDataParallelGPUCount,
		ErrorDataParallelAttentionDivisor: deploy.ErrorDataParallelAttentionDivisor,
	} {
		if api != producer {
			t.Fatalf("api code %q, deploy code %q", api, producer)
		}
	}
}

// An out-of-range utilization is a bad request, answered at the write boundary
// with the bound it violated. Reaching the scheduler with it would produce
// `invalid_request`, which describes a server defect rather than the field the
// caller has to change.
//
// The floor is one step, not a tuning recommendation, so the values refused here
// are only the ones the parameter cannot mean: zero, a share below the smallest
// the control can express, and a negative. A frugal-but-valid share such as 0.3
// is accepted, which the case below the creation checks.
func TestRejectsOutOfRangeUtilizationAtTheWriteBoundary(t *testing.T) {
	st, err := store.Open("file:launch_config_util_range?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/qwen", Revision: "main", Name: "org/qwen", WeightBytes: 4 << 30})
	if err != nil {
		t.Fatal(err)
	}
	launchConfigNode(t, st, "util-range-node")
	handler := &apiHandlers{d: Deps{Store: st}}

	for name, util := range map[string]string{"explicit zero": "0", "below the minimum": "0.005", "negative": "-0.5"} {
		t.Run(name, func(t *testing.T) {
			body := fmt.Sprintf(`{"name":"util-%s","model_id":%d,"node_uuid":"util-range-node","engine":"vllm","image":"vllm/vllm-openai:latest","quick_params":{"gpu_memory_utilization":%s}}`,
				strings.ReplaceAll(name, " ", "-"), model.ID, util)
			for endpoint, call := range map[string]func(http.ResponseWriter, *http.Request){"evaluate": handler.evaluate, "create": handler.createDeployment} {
				rec := httptest.NewRecorder()
				call(rec, httptest.NewRequest(http.MethodPost, "/api/v1/"+endpoint, strings.NewReader(body)))
				if endpoint == "evaluate" {
					// Evaluation keeps the draft: the bound violation is a
					// field error on a 200/ok=false response.
					if rec.Code != http.StatusOK {
						t.Fatalf("%s=%d, want 200: %s", endpoint, rec.Code, rec.Body.String())
					}
					var evaluated struct {
						OK     bool             `json:"ok"`
						Errors []dto.FieldError `json:"errors"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &evaluated); err != nil {
						t.Fatal(err)
					}
					if !evaluated.OK && len(evaluated.Errors) == 1 && evaluated.Errors[0].Field == "gpu_memory_utilization" && violatedBoundNamed(fmt.Sprint(evaluated.Errors[0].Params["detail"]), util) {
						continue
					}
					t.Fatalf("%s must report the field error beside the control: %s", endpoint, rec.Body.String())
				}
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("%s=%d, want 400: %s", endpoint, rec.Code, rec.Body.String())
				}
				if text := rec.Body.String(); !strings.Contains(text, "gpu_memory_utilization") || !violatedBoundNamed(text, util) {
					t.Fatalf("%s error must name the field and its minimum: %s", endpoint, text)
				}
			}
		})
	}

	// Shares inside the range create, both a generous one and one far below what
	// the form used to allow: the refusals above only mean something because a
	// frugal share is not among them.
	for name, util := range map[string]string{"generous": "0.92", "frugal": "0.3", "the floor itself": "0.01"} {
		t.Run("accepts a "+name+" share", func(t *testing.T) {
			ok := httptest.NewRecorder()
			handler.createDeployment(ok, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(
				fmt.Sprintf(`{"name":"util-ok-%[1]s","served_name":"util-ok-%[1]s","model_id":%[2]d,"node_uuid":"util-range-node","engine":"vllm","image":"vllm/vllm-openai:latest","quick_params":{"gpu_memory_utilization":%[3]s}}`,
					strings.ReplaceAll(name, " ", "-"), model.ID, util))))
			// The floor is accepted as a share; a 4 GiB model then cannot fit
			// in 1% of a card, which the create refuses for capacity - never
			// for the share's bound.
			if name == "the floor itself" {
				if text := ok.Body.String(); ok.Code != http.StatusBadRequest || !strings.Contains(text, ErrorDeploymentNeverFits) || strings.Contains(text, "gpu_memory_utilization must") {
					t.Fatalf("create=%d: %s", ok.Code, text)
				}
				return
			}
			if ok.Code != http.StatusCreated {
				t.Fatalf("create=%d: %s", ok.Code, ok.Body.String())
			}
		})
	}
}

// violatedBoundNamed reports whether an error text names the bound the value
// broke: the floor for a share below it, and negativeness for a negative, which
// is refused for being unrepresentable rather than for missing the floor.
func violatedBoundNamed(text, util string) bool {
	if strings.HasPrefix(util, "-") {
		return strings.Contains(text, "must not be negative")
	}
	return strings.Contains(text, "0.01")
}
