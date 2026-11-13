package deployment

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const fieldQuickParamsUnset = "quick_params_unset"

const fieldExtraArgs = "extra_args"

const FieldContainerMounts = "container_mount_rows"

const FieldContainerDocker = "container_docker_args"

const FieldContainerShm = "container_shm_gib"

// evaluateFieldErrors collects the request errors an evaluation can attribute
// to a form field. Evaluation answers a preview with ok=false and errors[]
// where create refuses the request: the deploy form keeps the draft and shows each error
// beside its control, so no validation failure costs the user their input.
// Rules are not invented here - every error comes from a validation that
// already existed on the create path; this type only carries it with a Field.
type evaluateFieldErrors struct {
	items []dto.FieldError
}

// add maps one error onto a field. A ProductError keeps its stable code and
// interpolation params; anything else becomes FIELD_INVALID with the detail as
// a param, so a rule that has not been typed yet still reaches the form
// instead of a bare status code. The same rule can fire at two layers (the
// request-level mount check and the assembled launch's own validation); the
// first report carries the better field attribution and wins.
func (c *evaluateFieldErrors) add(field string, err error) bool {
	if err == nil {
		return false
	}
	var productErr *messages.ProductError
	var item dto.FieldError
	if errors.As(err, &productErr) {
		item = dto.FieldError{Field: launchErrorField(field, productErr), Code: productErr.Code(), Params: productErr.Params()}
	} else {
		item = dto.FieldError{Field: launchErrorFieldFor(field, err), Code: messages.ErrorFieldInvalid, Params: map[string]any{"detail": err.Error()}}
	}
	for _, existing := range c.items {
		if existing.Code == item.Code && sameErrorParams(existing.Params, item.Params) {
			return true
		}
	}
	c.items = append(c.items, item)
	return true
}

func sameErrorParams(left, right map[string]any) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		other, ok := right[key]
		if !ok || fmt.Sprint(value) != fmt.Sprint(other) {
			return false
		}
	}
	return true
}

func (c *evaluateFieldErrors) empty() bool { return len(c.items) == 0 }

// launchErrorField answers the form field a launch ProductError belongs to.
// The launch conflict family names the dictionary key it judged, and that key
// is the form field; codes from other domains keep the caller's field.
func launchErrorField(fallback string, productErr *messages.ProductError) string {
	switch productErr.Code() {
	case deploy.ErrorLaunchConfigConflict, deploy.ErrorLaunchConfigUnsetConflict,
		deploy.ErrorQuickParamJSON, deploy.ErrorQuickParamPath,
		deploy.ErrorDataParallelGPUCount, deploy.ErrorDataParallelAttentionDivisor:
		if key, ok := productErr.Params()["key"].(string); ok && key != "" {
			return key
		}
	case deploy.ErrorDockerArgPlatformManaged, deploy.ErrorDockerArgUseShmField, deploy.ErrorDockerArgUseEnvField,
		deploy.ErrorDockerArgUseMountField, deploy.ErrorDockerArgUseEntrypointField:
		return FieldContainerDocker
	case deploy.ErrorShmSizeWithHostIPC, messages.ErrorShmBelowKVCPUTier:
		return FieldContainerShm
	case deploy.ErrorAscendSleepWeightNZConflict:
		return fieldExtraArgs
	}
	return fallback
}

// launchErrorFieldFor answers the form field a plain launch-materialization
// error belongs to, classified by the message shape it has always had.
func launchErrorFieldFor(fallback string, err error) string {
	message := err.Error()
	switch {
	case strings.HasPrefix(message, "invalid quick_params_unset"):
		return fieldQuickParamsUnset
	case strings.HasPrefix(message, "argument text"), strings.HasPrefix(message, "parse argument JSON"):
		return fieldExtraArgs
	case strings.HasPrefix(message, "invalid quick_params: "):
		// Every rule behind that prefix starts its sentence with the key it
		// judges; the exceptions are typed as ProductErrors with a key param.
		key, _, _ := strings.Cut(strings.TrimPrefix(message, "invalid quick_params: "), " ")
		if key != "" {
			return key
		}
	}
	return fallback
}

// validateMountRowRequest checks the extra mounts a request states against the
// same rules the assembled launch holds them to, plus the ones the form used
// to own: a host path must be an absolute non-root directory, the deployment's
// own mount targets are reserved, and a row mounts exactly one source. Rows
// the form left half-empty are not sent, so only complete rows are judged here.
func validateMountRowRequest(req *dto.DeployRequest, reservedTargets []string) error {
	rows, ok := requestExtraMounts(req)
	if !ok {
		return productError(messages.ErrorFieldInvalid, "invalid extra_mounts rows", map[string]any{"detail": "extra_mounts must be a list of mount rows"})
	}
	if len(rows) == 0 {
		return nil
	}
	targets := append([]string(nil), reservedTargets...)
	for _, row := range rows {
		source := strings.TrimSpace(row.HostPath)
		if source == "" {
			source = strings.TrimSpace(row.Volume)
		}
		target := strings.TrimSpace(row.ContainerPath)
		if source == "" && target == "" {
			continue
		}
		if row.ModelID != 0 && (strings.TrimSpace(row.HostPath) != "" || strings.TrimSpace(row.Volume) != "") {
			return productError(messages.ErrorFieldInvalid, "a mount row names a model and a host path or volume", map[string]any{"detail": "a mount row mounts one source: a host path, a volume or a model"})
		}
		if !absoluteNonRoot(target) {
			return productError(messages.ErrorMountContainerPath, "a mount's container path must be an absolute non-root path", map[string]any{"target": row.ContainerPath})
		}
		if row.HostPath != "" && !absoluteNonRoot(row.HostPath) {
			return productError(messages.ErrorMountHostPath, "a mount's host path must be an absolute non-root path", map[string]any{"source": row.HostPath})
		}
		if containsExact(targets, target) {
			return productError(messages.ErrorMountTargetDuplicate, "two mounts target "+target+" inside the container", map[string]any{"target": target})
		}
		for _, existing := range targets {
			if mountPathContains(existing, target) || mountPathContains(target, existing) {
				return productError(messages.ErrorMountTargetNested, "nested mount targets overlap", map[string]any{"parent": existing, "child": target})
			}
		}
		targets = append(targets, target)
	}
	return nil
}

// ValidateMountModels refuses an extra-mount row naming a model the store
// does not have. A row only names it by id, so a model deleted after the form
// listed it would otherwise surface as a launch that waits for it forever.
func (s *Service) ValidateMountModels(req *dto.DeployRequest) error {
	rows, ok := requestExtraMounts(req)
	if !ok {
		return nil
	}
	for _, id := range (deploy.ContainerOverrides{ExtraMounts: rows}).ModelMountIDs() {
		if _, err := s.d.Store.GetModel(id); errors.Is(err, store.ErrNotFound) {
			return productError(messages.ErrorMountModelNotFound, fmt.Sprintf("mounted model %d does not exist", id), map[string]any{"model_id": id})
		} else if err != nil {
			return err
		}
	}
	return nil
}

// requestExtraMounts decodes the extra_mounts override rows. A row shape the
// wire cannot decode is itself a field error, reported as such.
func requestExtraMounts(req *dto.DeployRequest) ([]deploy.ContainerMount, bool) {
	raw, stated := req.ContainerOverrides["extra_mounts"]
	if !stated || raw == nil {
		return nil, true
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	var rows []deploy.ContainerMount
	if err := json.Unmarshal(encoded, &rows); err != nil {
		return nil, false
	}
	return rows, true
}

// mountRowTargets lists the container targets the deployment itself owns: the
// catalog components' mount paths, an explicit model mount override, or the
// default mount.
func mountRowTargets(components []deploy.ComponentBinding, modelMount string) []string {
	var out []string
	for _, component := range components {
		if target := strings.TrimSpace(component.MountPath); target != "" {
			out = appendIfAbsent(out, target)
		}
	}
	if len(out) > 0 {
		return out
	}
	if target := strings.TrimSpace(modelMount); target != "" {
		return []string{target}
	}
	return []string{deploy.DefaultModelMount}
}

func requestModelMount(req *dto.DeployRequest) string {
	if raw, ok := req.ContainerOverrides["model_mount_path"]; ok {
		if text, ok := raw.(string); ok {
			return text
		}
	}
	return ""
}

func appendIfAbsent(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func containsExact(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func mountPathContains(parent, child string) bool {
	return parent != "/" && strings.HasPrefix(child, path.Clean(strings.TrimSpace(parent))+"/")
}

// absoluteNonRoot reports whether value is an absolute path that survives
// cleaning unchanged and is not the root - the container-path rule the launch
// assembler applies (deploy.validateResolvedMounts) spelled for the form.
func absoluteNonRoot(value string) bool {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, "/") || trimmed == "/" {
		return false
	}
	return path.Clean(trimmed) == trimmed
}
