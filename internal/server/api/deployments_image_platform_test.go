package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDeploymentRejectsKnownImagePlatformBeforeRegisteringModel(t *testing.T) {
	for _, tc := range []struct{ name, arch, framework string }{
		{"architecture", "arm64", "cuda"},
		{"execution OS", "amd64", "metal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const image = "catalog/platform:v1"
			rig := newCompatTemplateRig(t, "docker", func(spec *store.InferenceSpecRow) { spec.Image = image })
			node, err := rig.st.NodeByUUID(rig.nodeID)
			if err != nil {
				t.Fatal(err)
			}
			if err := rig.st.DB().Model(node).Updates(map[string]any{"arch": "amd64", "os": "linux"}).Error; err != nil {
				t.Fatal(err)
			}
			_, err = rig.st.UpsertImage(&store.Image{Engine: "vllm", Framework: tc.framework, Arch: tc.arch, ImageRef: image, Source: "custom", Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			body := rig.body(map[string]any{"node_uuid": rig.nodeID})
			requireCreateRefusalWithoutDeployment(t, rig.h, rig.st, body, messages.WarningImageOverrideIncompatible)
			requireNoRowsWritten(t, rig.st)
			rec := httptest.NewRecorder()
			rig.h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
			var evaluated dto.EvaluateResponse
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &evaluated) != nil {
				t.Fatalf("evaluate: %d %s", rec.Code, rec.Body.String())
			}
			if evaluated.OK || !hasWarningCode(evaluated.Warnings, messages.WarningImageOverrideIncompatible) {
				t.Fatalf("incompatible image preview: %+v", evaluated)
			}
		})
	}
}
