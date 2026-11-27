package deployment

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// patchedImageWarning reports the warning for a spec that patches its own
// runtime run on another image. Its pre-start script or assets are written
// for the engine sources of its own image; another image may be refused by
// the patch inside the container (the Voxtral realtime patch refusing vLLM
// 0.34 sources) and fail the start. The catalog records that such a step
// exists but not what it does, so the platform states the risk instead of
// deciding: a version-agnostic script gets the same warning.
func patchedImageWarning(requestedImage string, spec *store.InferenceSpecRow, selection templateSelection) *messages.Warning {
	if spec == nil {
		return nil
	}
	if requestedImage == "" || imagecatalog.SameImageRef(requestedImage, spec.Image) {
		return nil
	}
	if len(selection.Assets) == 0 && strings.TrimSpace(selection.PreStartScript) == "" {
		return nil
	}
	warning := messages.NewWarning(messages.WarningTemplateImagePinnedByRuntimePatch,
		map[string]any{"image": spec.Image, "requested": requestedImage})
	return &warning
}
