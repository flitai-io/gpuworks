package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func requireNoRowsWritten(t *testing.T, st *store.Store) {
	t.Helper()
	models, err := st.ListModels()
	if err != nil || len(models) != 0 {
		t.Fatalf("refused create persisted models: %+v, %v", models, err)
	}
}

// The refusals that no choice of the user can avoid still stop the create,
// even though the request names no image: the template supplies the
// compatibility image and the pinned node supplies the runtime.
func TestCreateRefusesUnrunnableCompatLaunchesFromTheTemplateImage(t *testing.T) {
	v2 := `{"VLLM_USE_V2_MODEL_RUNNER":"1"}`
	for name, test := range map[string]struct {
		mutate func(*store.InferenceSpecRow)
		code   string
	}{
		"V1 with argv preparation": {
			mutate: func(spec *store.InferenceSpecRow) { spec.PreStartArgv = `["prepare-tool"]` },
			code:   messages.ErrorAscendV1CompatPreStart,
		},
		"V2 with argv preparation": {
			mutate: func(spec *store.InferenceSpecRow) { spec.PreStartArgv, spec.Env = `["prepare-tool"]`, v2 },
			code:   messages.ErrorAscendCompatPreStartArgv,
		},
		"V1 with script preparation": {
			mutate: func(spec *store.InferenceSpecRow) { spec.PreStartScript = "echo prepare" },
			code:   messages.ErrorAscendV1CompatPreStart,
		},
		"merged script above the agent limit": {
			mutate: func(spec *store.InferenceSpecRow) {
				spec.Env, spec.PreStartScript = v2, "# "+strings.Repeat("x", 64<<10)+"\n"
			},
			code: messages.ErrorPreStartScriptTooLarge,
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newCompatTemplateRig(t, "udocker", test.mutate)
			body := rig.body(map[string]any{"node_uuid": rig.nodeID})
			requireCreateRefusalWithoutDeployment(t, rig.h, rig.st, body, test.code)
			requireNoRowsWritten(t, rig.st)
		})
	}
	t.Run("the same template on Docker is created", func(t *testing.T) {
		rig := newCompatTemplateRig(t, "docker", func(spec *store.InferenceSpecRow) { spec.PreStartArgv = `["prepare-tool"]` })
		requireCreateSucceeds(t, rig.h, rig.st, rig.body(map[string]any{"node_uuid": rig.nodeID}))
	})
}

func TestCreateWireStillReportsTheKeptRefusalAsProductError(t *testing.T) {
	rig := newCompatTemplateRig(t, "udocker", func(spec *store.InferenceSpecRow) { spec.PreStartArgv = `["prepare-tool"]` })
	rec := httptest.NewRecorder()
	rig.h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(rig.body(map[string]any{"node_uuid": rig.nodeID}))))
	var response struct {
		Code   string         `json:"code"`
		Error  string         `json:"error"`
		Params map[string]any `json:"params"`
	}
	if rec.Code != http.StatusBadRequest || json.Unmarshal(rec.Body.Bytes(), &response) != nil {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	if response.Code != messages.ErrorAscendV1CompatPreStart || response.Params["field"] != "pre_start_argv" ||
		!strings.Contains(response.Error, "pre_start_argv") {
		t.Fatalf("wire error = %+v", response)
	}
}
